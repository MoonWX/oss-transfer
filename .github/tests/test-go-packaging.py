#!/usr/bin/env python3
"""Reject ambiguous or misidentified release inputs before publishing assets."""
import hashlib
import importlib.util
import io
import json
import os
import shutil
from pathlib import Path
import subprocess
import tarfile
import tempfile
import textwrap
import unittest
from unittest import mock
import warnings
import zipfile

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("release_assets", ROOT / "deploy/go/release-assets.py")
ASSETS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ASSETS)
PACKAGE = ASSETS.PACKAGE
PUBLISH_SPEC = importlib.util.spec_from_file_location("publish_oss", ROOT / "deploy/go/publish-oss.py")
PUBLISH = importlib.util.module_from_spec(PUBLISH_SPEC)
PUBLISH_SPEC.loader.exec_module(PUBLISH)
DOCKER_SPEC = importlib.util.spec_from_file_location("docker_context", ROOT / "deploy/go/docker-context.py")
DOCKER = importlib.util.module_from_spec(DOCKER_SPEC)
DOCKER_SPEC.loader.exec_module(DOCKER)


class Packaging(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root / "packages"
        self.source.mkdir()
        self.output = self.root / "release"
        self.version = "4.1.0-test"
        self.commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
        for platform in PACKAGE.PLATFORMS:
            self.archive(*platform)

    def archive(self, os_name, arch, omit_version=False, duplicate_member=False,
                duplicate_hash=False, altered_version=False, payload_suffix=b""):
        name = f"oss-transfer-{os_name}-{arch}"
        extension = ".exe" if os_name == "windows" else ""
        payloads = {"oss-transfer-" + role + extension: (role + os_name + arch).encode() + payload_suffix
                    for role in ("master", "agent", "cli")}
        payloads["VERSION"] = (f"Version: {self.version}\nFullCommit: {self.commit}\n"
                               f"Platform: {os_name}/{arch}\n").encode()
        checksums = [hashlib.sha256(data).hexdigest() + "  " + key + "\n"
                     for key, data in payloads.items() if key != "VERSION" or not omit_version]
        if duplicate_hash:
            checksums.append(checksums[0])
        payloads["SHA256SUMS"] = "".join(checksums).encode()
        if altered_version:
            payloads["VERSION"] = payloads["VERSION"].replace(b"4.1.0-test", b"4.1.1-test")
        entries = list(payloads.items())
        if duplicate_member:
            entries.append(entries[0])
        path = self.source / (name + (".zip" if os_name == "windows" else ".tar.gz"))
        if os_name == "windows":
            with warnings.catch_warnings(), zipfile.ZipFile(path, "w") as archive:
                warnings.simplefilter("ignore", UserWarning)
                for key, data in entries:
                    archive.writestr(name + "/" + key, data)
        else:
            with tarfile.open(path, "w:gz") as archive:
                for key, data in entries:
                    item = tarfile.TarInfo(name + "/" + key)
                    item.size = len(data)
                    archive.addfile(item, io.BytesIO(data))
        Path(str(path) + ".sha256").write_text(ASSETS.sha256(path) + "  " + path.name + "\n")

    def assemble(self, version=None):
        ASSETS.assemble(self.source, self.output, version or self.version, self.commit)

    def rejected(self):
        with self.assertRaises(ValueError):
            self.assemble()
        self.assertFalse(self.output.exists())
        self.assertEqual({path.name for path in self.root.iterdir()}, {"packages"})

    def test_release_uses_identical_payloads_and_committed_installers(self):
        self.assemble()
        names = {PACKAGE.TARGETS.archive_name(*platform) for platform in PACKAGE.PLATFORMS}
        names.update({'install.sh', 'native-install.sh', 'Install.ps1', 'manifest.tsv', 'release.json', 'SHA256SUMS'})
        self.assertEqual({p.name for p in self.output.iterdir()}, names)
        manifest = [line.split('\t') for line in (self.output / 'manifest.tsv').read_text().splitlines()]
        archive_rows = [row for row in manifest if row[0] == 'archive']
        self.assertEqual(len(archive_rows), len(PACKAGE.PLATFORMS))
        for os_name, arch in PACKAGE.PLATFORMS:
            name = PACKAGE.TARGETS.archive_name(os_name, arch)
            path = self.output / name
            self.assertEqual(path.read_bytes(), (self.source / name).read_bytes())
            self.assertIn(['archive', os_name, arch, name, ASSETS.sha256(path), str(path.stat().st_size)], archive_rows)
            contents = PACKAGE.members(path)
            extension = ".exe" if os_name == "windows" else ""
            for role in ("master", "agent", "cli"):
                payload = contents[f'oss-transfer-{os_name}-{arch}/oss-transfer-{role}{extension}']
                self.assertEqual(payload, (role + os_name + arch).encode())
                self.assertIn(['file', os_name, arch, role, f'oss-transfer-{role}-{os_name}-{arch}{extension}',
                               hashlib.sha256(payload).hexdigest(), str(len(payload))], manifest)
        for name, source_name in PACKAGE.INSTALLERS.items():
            expected = subprocess.check_output(["git", "show", self.commit + ":" + source_name], cwd=ROOT)
            self.assertEqual((self.output / name).read_bytes(), expected)
        for line in (self.output / "SHA256SUMS").read_text().splitlines():
            digest, name = line.split("  ", 1)
            self.assertEqual(ASSETS.sha256(self.output / name), digest)

    def test_metadata_must_be_checksummed(self):
        self.archive("linux", "amd64", omit_version=True)
        self.rejected()

    def test_installer_package_reads_one_archive_snapshot(self):
        name = PACKAGE.TARGETS.archive_name('linux', 'amd64')
        original = (self.source / name).read_bytes()
        members = PACKAGE.members

        def replace_source_after_copy(path):
            self.assertNotEqual(path.parent, self.source)
            contents = members(path)
            if path.name == name:
                self.archive('linux', 'amd64', payload_suffix=b'-concurrent-build')
            return contents

        with mock.patch.object(PACKAGE, 'members', side_effect=replace_source_after_copy):
            PACKAGE.package(self.source, self.output, ROOT)
        self.assertEqual((self.output / name).read_bytes(), original)
        self.assertNotEqual((self.source / name).read_bytes(), original)
        rows = [line.split('\t') for line in (self.output / 'manifest.tsv').read_text().splitlines()]
        self.assertIn(['archive', 'linux', 'amd64', name, hashlib.sha256(original).hexdigest(), str(len(original))], rows)
        self.assertIn(['file', 'linux', 'amd64', 'master', 'oss-transfer-master-linux-amd64',
                       hashlib.sha256(b'masterlinuxamd64').hexdigest(), str(len(b'masterlinuxamd64'))], rows)

    def test_modified_metadata_is_rejected_even_with_valid_outer_hash(self):
        self.archive("linux", "amd64", altered_version=True)
        self.rejected()

    def test_duplicate_archive_members_are_rejected(self):
        for os_name in ("linux", "windows"):
            with self.subTest(os=os_name):
                self.archive(os_name, "amd64", duplicate_member=True)
                self.rejected()
                self.archive(os_name, "amd64")

    def test_duplicate_hash_entries_are_rejected(self):
        self.archive("linux", "arm64", duplicate_hash=True)
        self.rejected()

    def test_outer_hash_mismatch_is_rejected(self):
        archive = self.source / "oss-transfer-linux-amd64.tar.gz"
        with archive.open("ab") as stream:
            stream.write(b"changed")
        self.rejected()

    def test_tag_version_must_match_archive(self):
        with self.assertRaises(ValueError):
            self.assemble(version="4.1.1")
        self.assertFalse(self.output.exists())

    def test_docker_context_uses_private_archive_snapshot(self):
        archive = self.source / "oss-transfer-linux-amd64.tar.gz"
        original_hash = ASSETS.sha256(archive)
        package = DOCKER.PACKAGE.package

        def rebuild_after_verification(source, destination, scripts, require_all, unpack_binaries):
            self.assertNotEqual(source, self.source)
            self.archive("linux", "amd64", payload_suffix=b"-concurrent-rebuild")
            return package(source, destination, scripts, require_all, unpack_binaries)

        with mock.patch.object(DOCKER.PACKAGE, "package", side_effect=rebuild_after_verification):
            DOCKER.create(self.source, self.output)
        self.assertNotEqual(ASSETS.sha256(archive), original_hash)
        provenance = json.loads((self.output / "provenance.json").read_text())
        self.assertEqual({row["platform"] for row in provenance["binaries"]}, {"linux/amd64", "linux/arm64"})
        self.assertEqual(len(provenance["binaries"]), 6)
        recorded = next(row["sha256"] for row in provenance["archives"] if row["name"] == archive.name)
        self.assertEqual(recorded, original_hash)
        self.assertEqual((self.output / "bin/linux/amd64/oss-transfer-master").read_bytes(), b"masterlinuxamd64")
        for row in provenance["binaries"]:
            path = self.output / "bin" / row["platform"] / ("oss-transfer-" + row["program"])
            self.assertEqual(ASSETS.sha256(path), row["sha256"])
        self.assertEqual({path.name for path in self.root.iterdir()}, {"packages", "release"})

    def test_dockerfile_comes_from_package_commit_not_working_tree(self):
        repository = self.root / "repository"
        repository.mkdir()
        committed = b"FROM scratch\n# committed release Dockerfile\n"
        (repository / "Dockerfile").write_bytes(committed)
        env = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1")
        subprocess.run(["git", "init", "-q", str(repository)], check=True, env=env)
        subprocess.run(["git", "-C", str(repository), "add", "Dockerfile"], check=True, env=env)
        subprocess.run(["git", "-C", str(repository), "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                        "-c", "commit.gpgsign=false", "commit", "-qm", "Release Dockerfile"], check=True, env=env)
        self.commit = subprocess.check_output(["git", "-C", str(repository), "rev-parse", "HEAD"], text=True, env=env).strip()
        (repository / "Dockerfile").write_text("FROM unrelated-working-tree-image\n")
        for source_name in PACKAGE.INSTALLERS.values():
            path = repository / source_name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes((ROOT / source_name).read_bytes())
        for platform in PACKAGE.PLATFORMS:
            self.archive(*platform)
        with mock.patch.object(DOCKER, "ROOT", repository):
            DOCKER.create(self.source, self.output)
        self.assertEqual((self.output / "Dockerfile").read_bytes(), committed)
        self.assertEqual(json.loads((self.output / "provenance.json").read_text())["commit"], self.commit)

    def test_docker_context_rejects_bad_archive_without_partial_output(self):
        archive = self.source / "oss-transfer-linux-amd64.tar.gz"
        with archive.open("ab") as stream:
            stream.write(b"changed")
        with self.assertRaisesRegex(ValueError, "archive checksum mismatch"):
            DOCKER.create(self.source, self.output)
        self.assertFalse(self.output.exists())
        self.assertEqual({path.name for path in self.root.iterdir()}, {"packages"})

    def test_docker_context_rejects_uncommitted_source_without_partial_output(self):
        self.commit += "-dirty"
        for platform in PACKAGE.PLATFORMS:
            self.archive(*platform)
        with self.assertRaisesRegex(ValueError, "clean full commit SHA"):
            DOCKER.create(self.source, self.output)
        self.assertFalse(self.output.exists())
        self.assertEqual({path.name for path in self.root.iterdir()}, {"packages"})

    def github_publication(self, existing=False):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        step = workflow.split("      - name: Publish GitHub Release\n", 1)[1]
        script = textwrap.dedent(step.split("        run: |\n", 1)[1])
        platform_dir = self.root / "deploy/go"
        platform_dir.mkdir(parents=True, exist_ok=True)
        for name in ("platforms.py", "platforms.json"):
            shutil.copyfile(ROOT / "deploy/go" / name, platform_dir / name)
        bin_dir = self.root / "bin"
        bin_dir.mkdir(exist_ok=True)
        calls = self.root / "github-calls.jsonl"
        calls.unlink(missing_ok=True)
        gh = bin_dir / "gh"
        gh.write_text(r"""#!/usr/bin/env python3
import json, os, sys
with open(os.environ['GH_TEST_CALLS'], 'a') as stream:
    stream.write(json.dumps(sys.argv[1:]) + '\n')
if sys.argv[1:3] == ['release', 'view']:
    sys.exit(0 if os.environ['GH_TEST_EXISTS'] == '1' else 1)
""")
        gh.chmod(0o755)
        environment = dict(os.environ, PATH=str(bin_dir) + os.pathsep + os.environ["PATH"],
                           GH_TEST_CALLS=str(calls), GH_TEST_EXISTS="1" if existing else "0",
                           RELEASE_TAG="v" + self.version)
        result = subprocess.run(["bash", "-euo", "pipefail", "-c", script], cwd=self.root,
                                env=environment, text=True, capture_output=True)
        commands = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
        return result, commands

    def test_github_release_uploads_only_supported_platform_archives(self):
        self.assemble()
        shutil.copytree(self.output, self.root / "output")
        expected = {"output/" + PACKAGE.TARGETS.archive_name(*platform) for platform in PACKAGE.PLATFORMS}
        for existing in (False, True):
            with self.subTest(existing_release=existing):
                result, commands = self.github_publication(existing)
                self.assertEqual(result.returncode, 0, result.stderr)
                uploads = [command for command in commands if command[:2] in (["release", "create"], ["release", "upload"])]
                self.assertEqual(len(uploads), 1)
                assets = [value for value in uploads[0] if value.startswith("output/")]
                self.assertEqual(len(assets), len(expected))
                self.assertEqual(set(assets), expected)
                self.assertEqual(uploads[0][1], "upload" if existing else "create")

    def test_github_release_rejects_missing_archive_before_publishing(self):
        self.assemble()
        shutil.copytree(self.output, self.root / "output")
        (self.root / "output/oss-transfer-windows-arm64.zip").unlink()
        result, commands = self.github_publication()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Missing release archive", result.stderr)
        self.assertEqual(commands, [])

    def test_publish_refuses_unlisted_or_changed_payloads(self):
        self.assemble()
        PUBLISH.verify(self.output, "v" + self.version, self.commit)
        extra = self.output / "unlisted"
        extra.write_text("unexpected")
        with self.assertRaises(ValueError):
            PUBLISH.verify(self.output, "v" + self.version, self.commit)
        extra.unlink()
        (self.output / "install.sh").write_text("changed")
        with self.assertRaises(ValueError):
            PUBLISH.verify(self.output, "v" + self.version, self.commit)

    def uploaded(self, fail_at=None):
        keys = []
        def upload(command, **options):
            if 'list-objects-v2' in command:
                prefix = command[command.index('--prefix') + 1]
                if prefix.endswith('/latest/'):
                    names = PUBLISH.LATEST
                else:
                    self.assertEqual(prefix, 'osstransfer/v' + self.version + '/')
                    names = [path.name for path in self.output.iterdir()]
                return subprocess.CompletedProcess(command, 0, stdout=json.dumps({
                    'IsTruncated': False, 'Contents': [{'Key': prefix + name} for name in names]}), stderr='')
            keys.append(command[command.index("--key") + 1])
            if "put-object" in command:
                path = Path(command[command.index("--body") + 1])
                self.assertTrue(path.is_file())
            else:
                self.assertIn("delete-object", command)
                self.assertIn(keys[-1], ["osstransfer/latest/"+name for name in PUBLISH.RETIRED_LATEST])
            if fail_at == len(keys):
                raise subprocess.CalledProcessError(1, ["aws"])
        with mock.patch.dict(os.environ, AWS_ACCESS_KEY_ID="test", AWS_SECRET_ACCESS_KEY="test"), mock.patch.object(PUBLISH.subprocess, "run", side_effect=upload):
            if fail_at:
                with self.assertRaises(subprocess.CalledProcessError):
                    PUBLISH.publish(self.output, "v" + self.version, self.commit)
            else:
                PUBLISH.publish(self.output, "v" + self.version, self.commit)
        return keys

    def stable(self):
        self.version = "4.1.0"
        for platform in PACKAGE.PLATFORMS:
            self.archive(*platform)
        self.assemble()

    def test_prerelease_never_updates_latest(self):
        self.assemble()
        keys = self.uploaded()
        self.assertTrue(keys)
        self.assertTrue(all(key.startswith("osstransfer/v4.1.0-test/") for key in keys))

    def test_latest_advances_only_after_all_versioned_uploads(self):
        self.stable()
        keys = self.uploaded()
        versioned = ["osstransfer/v4.1.0/" + path.name for path in sorted(self.output.iterdir())]
        latest = ["osstransfer/latest/" + name for name in PUBLISH.LATEST]
        self.assertEqual(keys, versioned + latest + ["osstransfer/latest/"+name for name in PUBLISH.RETIRED_LATEST])

    def test_failed_payload_upload_preserves_latest(self):
        self.stable()
        keys = self.uploaded(fail_at=3)
        self.assertEqual(len(keys), 3)
        self.assertFalse(any("/latest/" in key for key in keys))


if __name__ == "__main__":
    unittest.main()
