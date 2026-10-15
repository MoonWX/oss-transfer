#!/usr/bin/env python3
"""Keep OSS downloads compact without breaking verification of older releases."""
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / "deploy/go" / filename)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


PUBLISH = module("oss_layout_publish", "publish-oss.py")
WITHDRAW = module("oss_layout_withdraw", "withdraw-release.py")
VERSION = "4.1.5"
COMMIT = "a" * 40


def sha(data):
    return hashlib.sha256(data).hexdigest()


def payloads(legacy=False):
    files = {name: name.encode() for name in ("install.sh", "native-install.sh", "Install.ps1")}
    rows = ["format\toss-transfer-go-v1", "version\t" + VERSION, "commit\t" + COMMIT]
    for operating_system, architecture in PUBLISH.PLATFORMS.PLATFORMS:
        name = PUBLISH.PLATFORMS.archive_name(operating_system, architecture)
        data = (operating_system + architecture).encode()
        files[name] = data
        if not legacy:
            rows.append("\t".join(("archive", operating_system, architecture, name, sha(data), str(len(data)))))
    if legacy:
        files["VERSION"] = f"Version: {VERSION}\nFullCommit: {COMMIT}\n".encode()
        files["docker-compose.yml"] = b"services: {}\n"
        files["oss-transfer-master-linux-amd64"] = b"legacy native executable"
    files["manifest.tsv"] = ("\n".join(rows) + "\n").encode()
    return files


def complete(files):
    files = dict(files)
    release = {"format": "oss-transfer-release-v1", "version": VERSION, "tag": "v" + VERSION,
               "commit": COMMIT, "assets": [{"name": name, "sha256": sha(data), "size": len(data)}
                                            for name, data in sorted(files.items())]}
    files["release.json"] = (json.dumps(release) + "\n").encode()
    files["SHA256SUMS"] = "".join(sha(data) + "  " + name + "\n" for name, data in sorted(files.items())).encode()
    return files


class OSSLayout(unittest.TestCase):
    def verify_local(self, files):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            for name, data in complete(files).items():
                (directory / name).write_bytes(data)
            return PUBLISH.verify(directory, "v" + VERSION, COMMIT)

    def verify_public(self, files):
        files = complete(files)
        release = json.loads(files["release.json"])
        base = WITHDRAW.OSS + "/v" + VERSION + "/"
        latest = set(WITHDRAW.LATEST)
        if "VERSION" in files:
            latest.update(WITHDRAW.LEGACY_LATEST)

        def request(url, method="GET", **kwargs):
            prefix, name = url.rsplit("/", 1)
            self.assertIn(prefix + "/", (base, WITHDRAW.OSS + "/latest/"))
            if prefix.endswith("/latest"):
                self.assertIn(name, latest)
            data = files[name]
            return 200, {"Content-Length": str(len(data))}, b"" if method == "HEAD" else data

        with mock.patch.object(WITHDRAW, "read_json", return_value=(release, {}, files["release.json"])), \
                mock.patch.object(WITHDRAW, "request", side_effect=request):
            return WITHDRAW.verify_oss(VERSION, COMMIT)

    def test_publisher_accepts_only_archive_payloads_and_required_metadata(self):
        names = self.verify_local(payloads())
        self.assertEqual(set(names), PUBLISH.release_names())
        self.assertEqual(len(names), len(PUBLISH.PLATFORMS.PLATFORMS) + 6)

    def test_publisher_rejects_redundant_assets_even_with_valid_checksums(self):
        for extra in ("oss-transfer-master-linux-amd64", "oss-transfer-linux-amd64.tar.gz.sha256",
                      "VERSION", "docker-compose.yml"):
            with self.subTest(extra=extra):
                files = payloads()
                files[extra] = b"redundant but correctly checksummed"
                with self.assertRaisesRegex(ValueError, "only supported platform archives"):
                    self.verify_local(files)

    def test_publisher_rejects_missing_platform_even_with_valid_checksums(self):
        files = payloads()
        del files[PUBLISH.PLATFORMS.archive_name("freebsd", "arm64")]
        with self.assertRaisesRegex(ValueError, "only supported platform archives"):
            self.verify_local(files)

    def test_remote_listing_requires_the_exact_release_files(self):
        names = PUBLISH.release_names()
        rows = [{"Key": "osstransfer/v" + VERSION + "/" + name} for name in sorted(names)]
        self.assertEqual(PUBLISH.verify_listing({"IsTruncated": False, "Contents": rows}, "v" + VERSION, names), len(names))
        changes = {
            "missing": rows[:-1],
            "extra": rows + [{"Key": "osstransfer/v" + VERSION + "/stale-binary"}],
            "out-of-prefix": rows[:-1] + [{"Key": "osstransfer/v4.1.4/" + sorted(names)[-1]}],
            "duplicate": rows + [rows[0]],
        }
        for problem, altered in changes.items():
            with self.subTest(problem=problem), self.assertRaises(ValueError):
                PUBLISH.verify_listing({"IsTruncated": False, "Contents": altered}, "v" + VERSION, names)
        with self.assertRaisesRegex(ValueError, "paginated"):
            PUBLISH.verify_listing({"IsTruncated": True, "Contents": rows, "NextContinuationToken": "next"}, "v" + VERSION, names)

    def test_latest_listing_rejects_retired_metadata(self):
        rows = [{"Key": "osstransfer/latest/" + name} for name in PUBLISH.LATEST]
        self.assertEqual(PUBLISH.verify_listing({"IsTruncated": False, "Contents": rows}, "latest", PUBLISH.LATEST), 4)
        rows.append({"Key": "osstransfer/latest/VERSION"})
        with self.assertRaisesRegex(ValueError, "unexpected files"):
            PUBLISH.verify_listing({"IsTruncated": False, "Contents": rows}, "latest", PUBLISH.LATEST)

    def test_withdrawal_verifies_current_layout_without_old_latest_files(self):
        hashes = self.verify_public(payloads())
        self.assertEqual(set(hashes), set(WITHDRAW.LATEST))

    def test_withdrawal_still_verifies_legacy_layout(self):
        hashes = self.verify_public(payloads(legacy=True))
        self.assertEqual(set(hashes), set(WITHDRAW.LATEST + WITHDRAW.LEGACY_LATEST))

    def test_withdrawal_checks_archive_manifest_against_public_assets(self):
        files = payloads()
        rows = files["manifest.tsv"].decode().splitlines()
        fields = rows[3].split("\t")
        fields[4] = "0" * 64
        rows[3] = "\t".join(fields)
        files["manifest.tsv"] = ("\n".join(rows) + "\n").encode()
        with self.assertRaisesRegex(RuntimeError, "archive manifest differs"):
            self.verify_public(files)

    def test_withdrawal_rejects_partial_legacy_metadata(self):
        files = payloads(legacy=True)
        del files["docker-compose.yml"]
        with self.assertRaisesRegex(RuntimeError, "legacy metadata is incomplete"):
            self.verify_public(files)


if __name__ == "__main__":
    unittest.main()
