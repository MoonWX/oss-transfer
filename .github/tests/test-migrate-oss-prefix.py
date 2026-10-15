#!/usr/bin/env python3
"""Exercise migration scope, interrupted copies and real AWS command boundaries."""
import contextlib
from copy import deepcopy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("migrate_oss_prefix", ROOT / "deploy/native/migrate-oss-prefix.py")
MIGRATION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MIGRATION)
POLICY = {"Owner": {"ID": "owner"}, "Grants": [
    {"Grantee": {"Type": "CanonicalUser", "ID": "owner"}, "Permission": "FULL_CONTROL"},
    {"Grantee": {"Type": "Group", "URI": "http://acs.amazonaws.com/groups/global/AllUsers"}, "Permission": "READ"},
]}


class MemoryOSS:
    def __init__(self):
        names = (*MIGRATION.SAMPLES, "v4.1.7/release.json", "v4.1.7/archive.tar.gz")
        self.data = {MIGRATION.SOURCE + name: name.encode() for name in names}
        self.heads = {key: {"ContentLength": len(data), "ETag": hashlib.md5(data).hexdigest(),
                            "ContentType": "application/octet-stream", "Metadata": {"test": "preserved"}}
                      for key, data in self.data.items()}
        self.policies = {key: deepcopy(POLICY) for key in self.data}
        self.copies = []
        self.acl_writes = []
        self.copy_error = False
        self.multipart = False

    def listing(self, prefix):
        return {key: MIGRATION.identity(head) for key, head in self.heads.items() if key.startswith(prefix)}

    def head(self, key):
        return deepcopy(self.heads[key])

    def acl(self, key):
        return deepcopy(self.policies[key])

    def copy(self, source, target, head):
        if self.copy_error:
            raise RuntimeError("copy rejected")
        self.copies.append((source, target))
        self.data[target] = self.data[source]
        self.heads[target] = deepcopy(head)
        if self.multipart:
            self.heads[target]["ETag"] = "copied-with-different-etag"
        self.policies[target] = {"Owner": {"ID": "owner"}, "Grants": POLICY["Grants"][:1]}

    def set_acl(self, key, policy):
        self.acl_writes.append(key)
        self.policies[key] = deepcopy(policy)

    def public_sha256(self, key):
        return hashlib.sha256(self.data[key]).hexdigest()


class MigrationTests(unittest.TestCase):
    def run_migration(self, store, apply=True):
        with contextlib.redirect_stdout(io.StringIO()):
            return MIGRATION.migrate(store, apply)

    def test_exact_prefix_only_and_encoded_copy_source(self):
        store = MIGRATION.OSS({})
        for key in ("other/file", "osstransfer-copy/file", "oss-transfer/file", "osstransfer", ""):
            with self.subTest(key=key), self.assertRaises(ValueError):
                MIGRATION.destination(key)
        self.assertEqual(MIGRATION.destination("osstransfer/v4/a b#c"), "oss-transfer/v4/a b#c")
        with mock.patch.object(store, "call") as call:
            store.copy("osstransfer/v4/a b#c", "oss-transfer/v4/a b#c", {"ETag": '"source-etag"'})
            arguments = call.call_args.args
            self.assertIn("moonwx/osstransfer/v4/a%20b%23c", arguments)
            self.assertIn("--copy-source-if-match", arguments)
            self.assertIn("--metadata-directive", arguments)
            self.assertIn("COPY", arguments)
            with self.assertRaises(ValueError):
                store.copy("osstransfer/v4/file", "oss-transfer/v5/file", {"ETag": "etag"})
            with self.assertRaises(ValueError):
                store.set_acl("other/file", POLICY)

    def test_paginated_listing_rejects_bad_scope_and_repeated_tokens(self):
        store = MIGRATION.OSS({})
        first = {"Contents": [{"Key": "osstransfer/v4/a", "Size": 1, "ETag": "a"}],
                 "IsTruncated": True, "NextContinuationToken": "next"}
        last = {"Contents": [{"Key": "osstransfer/v4/b", "Size": 2, "ETag": "b"}], "IsTruncated": False}
        with mock.patch.object(store, "call", side_effect=[first, last]) as call:
            self.assertEqual(len(store.listing(MIGRATION.SOURCE)), 2)
            self.assertIn("--continuation-token", call.call_args.args)
        for second in (first, {"Contents": [{"Key": "other/file", "Size": 1, "ETag": "a"}], "IsTruncated": False},
                       {"IsTruncated": True, "NextContinuationToken": "next"}):
            with mock.patch.object(store, "call", side_effect=[first, second]), self.assertRaises(ValueError):
                store.listing(MIGRATION.SOURCE)

    def test_listing_ignores_only_empty_root_marker_and_still_rejects_duplicates(self):
        store = MIGRATION.OSS({})
        for prefix in (MIGRATION.SOURCE, MIGRATION.TARGET):
            marker = {"Key": prefix, "Size": 0, "ETag": "empty"}
            archive = {"Key": prefix + "v4/archive.tar.gz", "Size": 10, "ETag": "archive"}
            page = {"Contents": [marker, archive], "IsTruncated": False}
            with self.subTest(prefix=prefix), mock.patch.object(store, "call", return_value=page):
                self.assertEqual(store.listing(prefix), {archive["Key"]: (10, "archive")})
            for invalid in ([dict(marker, Size=1), archive], [marker, marker, archive]):
                with mock.patch.object(store, "call", return_value={"Contents": invalid, "IsTruncated": False}), \
                        self.assertRaisesRegex(ValueError, prefix):
                    store.listing(prefix)

    def test_copy_preserves_source_and_acl_then_latest_commits_last(self):
        store = MemoryOSS()
        original = deepcopy(store.data)
        report = self.run_migration(store)
        self.assertEqual(report["objects"], len(original))
        self.assertEqual(report["copied"], len(original))
        self.assertEqual(store.copies[-1][1], "oss-transfer/latest/release.json")
        latest_index = next(i for i, (_, key) in enumerate(store.copies) if "/latest/" in key)
        self.assertTrue(all("/latest/" in target for _, target in store.copies[latest_index:]))
        for source, data in original.items():
            target = MIGRATION.destination(source)
            self.assertEqual(store.data[source], data)
            self.assertEqual(store.data[target], data)
            self.assertEqual(store.heads[source], store.heads[target])
            self.assertEqual(store.policies[source], store.policies[target])
        copies = len(store.copies)
        self.assertEqual(self.run_migration(store)["copied"], 0)
        self.assertEqual(len(store.copies), copies)

    def test_dry_run_never_writes_and_identical_partial_copy_repairs_acl(self):
        store = MemoryOSS()
        self.assertEqual(self.run_migration(store, False)["wouldCopy"], len(store.data))
        self.assertEqual(store.copies + store.acl_writes, [])
        source = "osstransfer/v4.1.7/archive.tar.gz"
        target = MIGRATION.destination(source)
        store.copy(source, target, store.head(source))
        self.run_migration(store)
        self.assertEqual(store.policies[target], POLICY)
        self.assertEqual(sum(key == target for _, key in store.copies), 1)

    def test_conflicting_destination_is_not_overwritten(self):
        for field in ("ContentLength", "Metadata", "ETag"):
            with self.subTest(field=field):
                store = MemoryOSS()
                source = "osstransfer/latest/release.json"
                target = MIGRATION.destination(source)
                store.copy(source, target, store.head(source))
                store.data[target] = b"different bytes"
                store.heads[target][field] = 999 if field == "ContentLength" else {"bad": "value"} if field == "Metadata" else "bad-etag"
                with self.assertRaises(ValueError):
                    self.run_migration(store)
                self.assertEqual(sum(key == target for _, key in store.copies), 1)

    def test_multipart_etag_change_uses_byte_hashes_and_copy_failure_stops_latest(self):
        store = MemoryOSS()
        store.multipart = True
        with mock.patch.object(store, "public_sha256", wraps=store.public_sha256) as digest:
            self.run_migration(store)
            self.assertGreater(digest.call_count, 2 * len(MIGRATION.SAMPLES))
        store = MemoryOSS()
        store.copy_error = True
        with self.assertRaisesRegex(RuntimeError, "copy rejected"):
            self.run_migration(store)
        self.assertEqual(store.copies, [])

    def test_public_download_failure_and_source_changes_are_errors(self):
        store = MemoryOSS()
        with mock.patch.object(store, "public_sha256", side_effect=OSError("403")), self.assertRaises(OSError):
            self.run_migration(store)
        store = MemoryOSS()
        listing = store.listing
        count = 0

        def mutate(prefix):
            nonlocal count
            count += 1
            rows = listing(prefix)
            if count > 2 and prefix == MIGRATION.SOURCE:
                rows.pop(next(iter(rows)))
            return rows

        with mock.patch.object(store, "listing", side_effect=mutate), self.assertRaisesRegex(ValueError, "changed during"):
            self.run_migration(store)

    def test_failed_aws_command_is_not_a_successful_copy(self):
        result = subprocess.CompletedProcess([], 1, stdout=b"", stderr=b"permission denied")
        with mock.patch.object(subprocess, "run", return_value=result), self.assertRaisesRegex(RuntimeError, "copy-object failed"):
            MIGRATION.OSS({}).copy("osstransfer/a", "oss-transfer/a", {"ETag": "etag"})


if __name__ == "__main__":
    unittest.main()
