#!/usr/bin/env python3
"""Keep published OSS downloads limited to the verified release files."""
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / "deploy/native" / filename)
    value = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(value)
    return value


PUBLISH = module("oss_layout_publish", "publish-oss.py")
# latest/ mirrors the complete release.
LATEST = tuple(sorted(PUBLISH.release_names()))
VERSION = "4.1.5"
COMMIT = "a" * 40


def sha(data):
    return hashlib.sha256(data).hexdigest()


def payloads():
    files = {name: name.encode() for name in ("install.sh", "native-install.sh", "Install.ps1", "oss-transfer-run.sh")}
    rows = ["format\toss-transfer-go-v1", "version\t" + VERSION, "commit\t" + COMMIT]
    for operating_system, architecture in PUBLISH.PLATFORMS.PLATFORMS:
        name = PUBLISH.PLATFORMS.archive_name(operating_system, architecture)
        data = (operating_system + architecture).encode()
        files[name] = data
        rows.append("\t".join(("archive", operating_system, architecture, name, sha(data), str(len(data)))))
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

    def test_publisher_accepts_only_archive_payloads_and_required_metadata(self):
        names = self.verify_local(payloads())
        self.assertEqual(set(names), PUBLISH.release_names())
        self.assertEqual(len(names), len(PUBLISH.PLATFORMS.PLATFORMS) + 7)

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
        rows = [{"Key": "oss-transfer/v" + VERSION + "/" + name} for name in sorted(names)]
        self.assertEqual(PUBLISH.verify_listing({"IsTruncated": False, "Contents": rows}, "v" + VERSION, names), len(names))
        changes = {
            "missing": rows[:-1],
            "extra": rows + [{"Key": "oss-transfer/v" + VERSION + "/stale-binary"}],
            "out-of-prefix": rows[:-1] + [{"Key": "oss-transfer/v4.1.4/" + sorted(names)[-1]}],
            "duplicate": rows + [rows[0]],
        }
        for problem, altered in changes.items():
            with self.subTest(problem=problem), self.assertRaises(ValueError):
                PUBLISH.verify_listing({"IsTruncated": False, "Contents": altered}, "v" + VERSION, names)
        with self.assertRaisesRegex(ValueError, "paginated"):
            PUBLISH.verify_listing({"IsTruncated": True, "Contents": rows, "NextContinuationToken": "next"}, "v" + VERSION, names)

    def test_remote_listing_checks_content_against_local_files(self):
        local = {name: (hashlib.md5(name.encode()).hexdigest(), len(name)) for name in LATEST}
        def page(etag=lambda md5: '"' + md5 + '"', size=lambda value: value):
            return {"IsTruncated": False, "Contents": [
                {"Key": "oss-transfer/latest/" + name, "ETag": etag(md5), "Size": size(length)}
                for name, (md5, length) in local.items()]}
        self.assertEqual(PUBLISH.verify_listing(page(), "latest", LATEST, local), len(LATEST))
        self.assertEqual(PUBLISH.verify_listing(page(lambda md5: md5.upper()), "latest", LATEST, local), len(LATEST))
        # Multipart or missing ETags are not MD5s, so key and size decide.
        for etag in (lambda md5: '"' + md5 + '-3"', lambda md5: None):
            self.assertEqual(PUBLISH.verify_listing(page(etag), "latest", LATEST, local), len(LATEST))
        with self.assertRaisesRegex(ValueError, "ETag differs"):
            PUBLISH.verify_listing(page(lambda md5: '"' + "0" * 32 + '"'), "latest", LATEST, local)
        for size in (lambda value: value + 1, lambda value: None, lambda value: True):
            with self.subTest(size=size(1)), self.assertRaisesRegex(ValueError, "size differs"):
                PUBLISH.verify_listing(page(size=size), "latest", LATEST, local)

    def test_latest_listing_rejects_retired_metadata(self):
        rows = [{"Key": "oss-transfer/latest/" + name} for name in LATEST]
        self.assertEqual(PUBLISH.verify_listing({"IsTruncated": False, "Contents": rows}, "latest", LATEST), len(LATEST))
        rows.append({"Key": "oss-transfer/latest/VERSION"})
        with self.assertRaisesRegex(ValueError, "unexpected files"):
            PUBLISH.verify_listing({"IsTruncated": False, "Contents": rows}, "latest", LATEST)


if __name__ == "__main__":
    unittest.main()
