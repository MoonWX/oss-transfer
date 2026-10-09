#!/usr/bin/env python3
"""Offline publication checks: real Git/tar/checksums, recording AWS stub."""
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest


SOURCE_SCRIPT = Path(__file__).resolve().parents[1] / "publish-oss.sh"
AWS_STUB = r'''#!/usr/bin/env python3
import base64, hashlib, json, os, pathlib, sys
args = sys.argv[1:]
def option(name):
    return args[args.index(name) + 1]
assert args[:4] == ['--endpoint-url', 'https://s3.cn-north-1.jdcloud-oss.com', '--region', 'cn-north-1']
assert args[4:6] == ['s3api', 'put-object']
assert option('--bucket') == 'moonwx'
assert '--acl' not in args and '--no-verify-ssl' not in args
assert '--debug' not in args
body = pathlib.Path(option('--body')).read_bytes()
assert option('--content-md5') == base64.b64encode(hashlib.md5(body).digest()).decode()
assert os.environ['AWS_PROFILE'] == 'default'
assert os.environ['AWS_DEFAULT_PROFILE'] == 'default'
assert os.environ['AWS_REQUEST_CHECKSUM_CALCULATION'] == 'when_required'
assert os.environ['AWS_RESPONSE_CHECKSUM_VALIDATION'] == 'when_required'
assert os.environ['AWS_EC2_METADATA_DISABLED'] == 'true'
assert os.environ['AWS_CLI_AUTO_PROMPT'] == 'off'
assert os.environ['AWS_PAGER'] == ''
assert 'AWS_SESSION_TOKEN' not in os.environ
assert 'AWS_SECURITY_TOKEN' not in os.environ
config_path = pathlib.Path(os.environ['AWS_CONFIG_FILE'])
config = config_path.read_text()
assert 'signature_version = s3v4' in config
assert 'addressing_style = virtual' in config
assert 'payload_signing_enabled = true' in config
assert 'region = cn-north-1' in config
assert os.environ['AWS_ACCESS_KEY_ID'] not in config
assert os.environ['AWS_SECRET_ACCESS_KEY'] not in config
assert not pathlib.Path(os.environ['AWS_SHARED_CREDENTIALS_FILE']).exists()
log = pathlib.Path(os.environ['AWS_STUB_LOG'])
calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
record = {'key': option('--key'), 'body': base64.b64encode(body).decode(),
          'content_type': option('--content-type'), 'cache_control': option('--cache-control'),
          'config_path': str(config_path)}
with log.open('a') as output:
    output.write(json.dumps(record) + '\n')
if len(calls) == 0 and os.environ.get('AWS_STUB_MUTATE_ARTIFACT'):
    pathlib.Path(os.environ['AWS_STUB_MUTATE_ARTIFACT']).write_bytes(b'concurrent build replaced archive')
if os.environ.get('AWS_STUB_FAIL_CALL') == str(len(calls) + 1):
    sys.exit(23)
remote = pathlib.Path(os.environ['AWS_STUB_REMOTE']) / record['key']
remote.parent.mkdir(parents=True, exist_ok=True)
remote.write_bytes(body)
'''


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="oss publish tests ")
        self.root = Path(self.temp.name)
        self.repo = self.root / "source with spaces"
        (self.repo / "deploy").mkdir(parents=True)
        shutil.copy2(SOURCE_SCRIPT, self.repo / "deploy/publish-oss.sh")
        self.output = self.repo / "release output"
        self.output.mkdir()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        stub = self.bin / "aws"
        stub.write_text(AWS_STUB)
        stub.chmod(0o755)
        self.git("init", "-q")
        self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "fixture")
        self.commit = self.git("rev-parse", "HEAD").strip()
        self.git("tag", "v4.0.0")
        self.log = self.root / "aws-calls.jsonl"
        self.remote = self.root / "remote"
        self.env = os.environ.copy()
        # The test never reads or uses credentials supplied by the caller.
        self.env.update(PATH=f"{self.bin}:{self.env['PATH']}",
                        GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull,
                        AWS_ACCESS_KEY_ID="fixture-access-key", AWS_SECRET_ACCESS_KEY="fixture-secret-key",
                        AWS_SESSION_TOKEN="unrelated-aws-session", AWS_SECURITY_TOKEN="unrelated-token",
                        AWS_PROFILE="unrelated-profile", AWS_CONFIG_FILE="/nonexistent/caller-config",
                        AWS_STUB_LOG=str(self.log), AWS_STUB_REMOTE=str(self.remote))
        self.package()

    def tearDown(self):
        self.temp.cleanup()

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.repo), *args], text=True,
                                       stderr=subprocess.DEVNULL,
                                       env=dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull))

    def package(self, version="4.0.0", commit=None, extra_version="", member="oss-transfer/VERSION"):
        self.version_data = (f"Branch: fixture\nCommit: {commit or self.commit[:7]}\n"
                             f"Version: {version}\nBuildTime: fixture\n{extra_version}").encode()
        with tarfile.open(self.output / "oss-transfer.tar.gz", "w:gz") as archive:
            info = tarfile.TarInfo(member)
            info.size = len(self.version_data)
            archive.addfile(info, io.BytesIO(self.version_data))
        self.checksum()

    def checksum(self):
        digest = hashlib.sha256((self.output / "oss-transfer.tar.gz").read_bytes()).hexdigest()
        (self.output / "oss-transfer.tar.gz.sha256").write_text(f"{digest}  oss-transfer.tar.gz\n")

    def publish(self, tag="v4.0.0", *args, env=None):
        return subprocess.run(["bash", str(self.repo / "deploy/publish-oss.sh"), tag,
                               str(self.output), *args], env=env or self.env,
                              text=True, capture_output=True)

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def assert_rejected(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.calls(), [])

    def assert_uploaded(self, call, name):
        expected = self.version_data if name == "VERSION" else (self.output / name).read_bytes()
        self.assertEqual(base64.b64decode(call["body"]), expected)
        self.assertEqual(call["cache_control"], "no-cache")
        content_type = "application/gzip" if name.endswith(".tar.gz") else "text/plain; charset=utf-8"
        self.assertEqual(call["content_type"], content_type)
        self.assertFalse(Path(call["config_path"]).exists(), "temporary configuration not cleaned up")

    def test_stable_uploads_version_then_latest(self):
        result = self.publish()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        names = ["oss-transfer.tar.gz", "oss-transfer.tar.gz.sha256", "VERSION"]
        expected = [f"osstransfer/{prefix}/{name}" for prefix in ("v4.0.0", "latest") for name in names]
        self.assertEqual([c["key"] for c in self.calls()], expected)
        for call, name in zip(self.calls(), names * 2):
            self.assert_uploaded(call, name)
        self.assertNotIn("fixture-access-key", result.stdout + result.stderr)
        self.assertNotIn("fixture-secret-key", result.stdout + result.stderr)

    def test_prerelease_never_updates_latest(self):
        self.git("tag", "v4.0.0-rc.1")
        self.package(version="4.0.0-rc.1")
        latest = self.remote / "osstransfer/latest/VERSION"
        latest.parent.mkdir(parents=True)
        latest.write_bytes(b"previous stable")
        result = self.publish("v4.0.0-rc.1")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.calls()), 3)
        self.assertTrue(all(c["key"].startswith("osstransfer/v4.0.0-rc.1/") for c in self.calls()))
        self.assertEqual(latest.read_bytes(), b"previous stable")

    def test_uploads_use_validated_snapshot(self):
        original_archive = (self.output / "oss-transfer.tar.gz").read_bytes()
        env = dict(self.env, AWS_STUB_MUTATE_ARTIFACT=str(self.output / "oss-transfer.tar.gz"))
        result = self.publish(env=env)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(base64.b64decode(self.calls()[0]["body"]), original_archive)
        self.assertEqual(base64.b64decode(self.calls()[3]["body"]), original_archive)
        self.assertNotEqual((self.output / "oss-transfer.tar.gz").read_bytes(), original_archive)

    def test_annotated_tag_and_full_commit(self):
        self.git("tag", "-d", "v4.0.0")
        self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "tag", "-a", "v4.0.0", "-m", "fixture release")
        self.package(commit=self.commit)
        result = self.publish()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.calls()), 6)

    def test_upload_failure_stops_at_every_object(self):
        for failure in range(1, 7):
            with self.subTest(failure=failure):
                self.log.unlink(missing_ok=True)
                env = dict(self.env, AWS_STUB_FAIL_CALL=str(failure))
                result = self.publish(env=env)
                self.assertEqual(result.returncode, 23, result.stdout + result.stderr)
                self.assertEqual(len(self.calls()), failure)
                self.assertNotIn("OSS publish completed", result.stdout)
                if failure <= 3:
                    self.assertFalse(any("/latest/" in c["key"] for c in self.calls()))
                if failure < 6:
                    self.assertFalse(any(c["key"] == "osstransfer/latest/VERSION" for c in self.calls()))
                self.assertFalse(Path(self.calls()[0]["config_path"]).exists())

    def test_invalid_tags_and_extra_arguments(self):
        for tag in ["4.0.0", "v04.0.0", "v4.0", "v4.0.0-rc..1", "v4.0.0-01", "v4.0.0/../../latest"]:
            with self.subTest(tag=tag):
                self.assert_rejected(self.publish(tag))
        self.assert_rejected(self.publish("v4.0.0", "false"))

    def test_tag_must_exist_and_point_to_head(self):
        self.assert_rejected(self.publish("v4.0.1"))
        self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "another commit")
        self.assert_rejected(self.publish())

    def test_invalid_package_identity(self):
        for kwargs in [{"version": "4.0.1"}, {"commit": "0000000"}, {"commit": "unknown"},
                       {"extra_version": "Version: 4.0.0\n"}, {"member": "other/VERSION"}]:
            with self.subTest(kwargs=kwargs):
                self.package(**kwargs)
                self.assert_rejected(self.publish())

    def test_corrupt_archive_and_checksum(self):
        archive = self.output / "oss-transfer.tar.gz"
        archive.write_bytes(archive.read_bytes() + b"corrupt")
        self.assert_rejected(self.publish())
        self.package()
        (self.output / "oss-transfer.tar.gz.sha256").write_text("0" * 64 + "  ../other.tar.gz\n")
        self.assert_rejected(self.publish())
        archive.write_bytes(b"not a tar archive")
        self.checksum()
        self.assert_rejected(self.publish())

    def test_missing_artifacts_and_credentials(self):
        env = dict(self.env, AWS_ACCESS_KEY_ID="")
        self.assert_rejected(self.publish(env=env))
        for name in ["oss-transfer.tar.gz.sha256", "oss-transfer.tar.gz"]:
            with self.subTest(name=name):
                self.package()
                (self.output / name).unlink()
                self.assert_rejected(self.publish())

    def test_default_artifact_directory(self):
        shutil.move(self.output, self.repo / "output")
        result = subprocess.run(["bash", str(self.repo / "deploy/publish-oss.sh"), "v4.0.0"],
                                env=self.env, text=True, capture_output=True)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(len(self.calls()), 6)


if __name__ == "__main__":
    unittest.main(verbosity=2)
