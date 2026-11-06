#!/usr/bin/env python3
"""Reject ambiguous or misidentified release inputs before publishing assets."""
import base64
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
import threading
import unittest
from unittest import mock
import warnings
import zipfile

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("release_assets", ROOT / "deploy/native/release-assets.py")
ASSETS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(ASSETS)
PACKAGE = ASSETS.PACKAGE
PUBLISH_SPEC = importlib.util.spec_from_file_location("publish_oss", ROOT / "deploy/native/publish-oss.py")
PUBLISH = importlib.util.module_from_spec(PUBLISH_SPEC)
PUBLISH_SPEC.loader.exec_module(PUBLISH)
DOCKER_SPEC = importlib.util.spec_from_file_location("docker_context", ROOT / "deploy/native/docker-context.py")
DOCKER = importlib.util.module_from_spec(DOCKER_SPEC)
DOCKER_SPEC.loader.exec_module(DOCKER)


class InstallerHistory(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.repository = self.root / "repository"
        self.environment = dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM="1")
        self.repository.mkdir()
        self.git("init", "-q")
        self.legacy = {
            "install.sh": b"root installer from old commit\n",
            "deploy/go/install.sh": b"native installer from old commit\n",
            "deploy/go/Install.ps1": b"Windows installer from old commit\n",
        }
        for name, content in self.legacy.items():
            path = self.repository / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)
        self.old_commit = self.commit("Old installer layout")
        # Keep the old paths too so the preference for current paths is tested.
        self.current = {name.replace("deploy/go/", "deploy/native/"): b"new " + content
                        for name, content in self.legacy.items()}
        for name, content in self.current.items():
            path = self.repository / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(content)
        self.new_commit = self.commit("New installer layout")
        for name in self.current:
            (self.repository / name).write_bytes(b"uncommitted installer must not be used\n")

    def git(self, *args):
        return subprocess.check_output(["git", "-C", str(self.repository), *args],
                                       env=self.environment, stderr=subprocess.PIPE)

    def commit(self, message):
        self.git("add", ".")
        self.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
                 "-c", "commit.gpgsign=false", "commit", "-qm", message)
        return self.git("rev-parse", "HEAD").decode().strip()

    def test_installer_sources_follow_archive_commit_across_directory_rename(self):
        for commit, expected in ((self.old_commit, self.legacy), (self.new_commit, self.current)):
            with self.subTest(commit=commit):
                destination = self.root / commit
                module_path = self.repository / "deploy/native/release-assets.py"
                with mock.patch.object(ASSETS, "__file__", str(module_path)):
                    ASSETS.release_sources(destination, commit)
                for name, content in expected.items():
                    self.assertEqual((destination / name.replace("deploy/go/", "deploy/native/")).read_bytes(), content)

    def test_missing_commit_never_uses_working_tree_installers(self):
        module_path = self.repository / "deploy/native/release-assets.py"
        with mock.patch.object(ASSETS, "__file__", str(module_path)):
            with self.assertRaises(subprocess.CalledProcessError):
                ASSETS.release_sources(self.root / "invalid", "0" * 40)


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
        committed_paths = subprocess.check_output(
            ["git", "ls-tree", "-r", "--name-only", self.commit], cwd=ROOT, text=True).splitlines()
        for name, source_name in PACKAGE.INSTALLERS.items():
            if source_name not in committed_paths:
                source_name = source_name.replace("deploy/native/", "deploy/go/", 1)
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

    def github_publication(self, existing=False, step="Stage GitHub Release draft", view_error="release not found"):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        block = workflow.split("      - name: " + step + "\n", 1)[1].split("        run: |\n", 1)[1]
        lines = []
        for line in block.splitlines(keepends=True):
            if line.strip() and not line.startswith("          "):
                break
            lines.append(line)
        script = textwrap.dedent("".join(lines))
        platform_dir = self.root / "deploy/native"
        platform_dir.mkdir(parents=True, exist_ok=True)
        for name in ("platforms.py", "platforms.json"):
            shutil.copyfile(ROOT / "deploy/native" / name, platform_dir / name)
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
    if os.environ['GH_TEST_EXISTS'] == '1':
        sys.exit(0)
    sys.exit(os.environ['GH_TEST_VIEW_ERROR'])
""")
        gh.chmod(0o755)
        environment = dict(os.environ, PATH=str(bin_dir) + os.pathsep + os.environ["PATH"],
                           GH_TEST_CALLS=str(calls), GH_TEST_EXISTS="1" if existing else "0", GH_TEST_VIEW_ERROR=view_error,
                           RELEASE_TAG="v" + self.version)
        result = subprocess.run(["bash", "-euo", "pipefail", "-c", script], cwd=self.root,
                                env=environment, text=True, capture_output=True)
        commands = [json.loads(line) for line in calls.read_text().splitlines()] if calls.exists() else []
        return result, commands

    def test_github_release_uploads_a_complete_offline_bundle(self):
        self.assemble()
        shutil.copytree(self.output, self.root / "output")
        expected = {"output/" + PACKAGE.TARGETS.archive_name(*platform) for platform in PACKAGE.PLATFORMS}
        expected |= {"output/" + name for name in ("manifest.tsv", "SHA256SUMS", "install.sh", "native-install.sh", "Install.ps1")}
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
                if not existing:
                    self.assertIn("--draft", uploads[0])
                    self.assertIn("--verify-tag", uploads[0])
                self.assertFalse(any("--draft=false" in command for command in commands))

    def test_github_draft_is_not_duplicated_when_lookup_fails(self):
        self.assemble()
        shutil.copytree(self.output, self.root / "output")
        result, commands = self.github_publication(view_error="HTTP 502: Bad Gateway")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("HTTP 502", result.stderr)
        self.assertEqual([command[:2] for command in commands], [["release", "view"]])

    def test_github_release_is_published_only_by_the_final_job(self):
        workflow = (ROOT / ".github/workflows/release.yml").read_text()
        self.assertIn("    needs: [oss, docker_hub, github_draft]\n", workflow.split("\n  release:\n", 1)[1])
        self.assertIn("    needs: artifacts\n", workflow.split("\n  github_draft:\n", 1)[1].split("\n  release:\n", 1)[0])
        result, commands = self.github_publication(step="Publish GitHub Release")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(commands, [["release", "edit", "v" + self.version, "--draft=false", "--prerelease", "--latest=false"]])

    def test_github_release_rejects_missing_archive_before_publishing(self):
        self.assemble()
        shutil.copytree(self.output, self.root / "output")
        (self.root / "output/oss-transfer-windows-arm64.zip").unlink()
        result, commands = self.github_publication()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Missing release file", result.stderr)
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

    def uploaded(self, fail_at=None, remote=None, etag=None, raises=None):
        # Fake OSS bucket: puts and deletes change what later listings return.
        keys = []
        remote = dict(remote or {})
        lock = threading.Lock()
        def upload(command, **options):
            self.assertEqual((options["env"]["AWS_RETRY_MODE"], options["env"]["AWS_MAX_ATTEMPTS"]), ("standard", "5"))
            if 'list-objects-v2' in command:
                prefix = command[command.index('--prefix') + 1]
                with lock:
                    rows = [{'Key': key, 'Size': len(data), 'ETag': etag(data) if etag else '"%s"' % hashlib.md5(data).hexdigest()}
                            for key, data in sorted(remote.items()) if key.startswith(prefix)]
                return subprocess.CompletedProcess(command, 0, stdout=json.dumps({'IsTruncated': False, 'Contents': rows}), stderr='')
            key = command[command.index("--key") + 1]
            with lock:
                keys.append(key)
                failed = fail_at == len(keys)
            if failed:
                raise subprocess.CalledProcessError(1, ["aws"])
            if "put-object" in command:
                path = Path(command[command.index("--body") + 1])
                self.assertTrue(path.is_file())
                data = path.read_bytes()
                self.assertEqual(command[command.index("--content-md5") + 1], base64.b64encode(hashlib.md5(data).digest()).decode())
                with lock:
                    remote[key] = data
            else:
                self.assertIn("delete-object", command)
                name = key.removeprefix("oss-transfer/latest/")
                self.assertTrue(name in PUBLISH.RETIRED_LATEST or PUBLISH.STALE_ARCHIVE.fullmatch(name), key)
                with lock:
                    self.assertIn(key, remote)
                    del remote[key]
        with mock.patch.dict(os.environ, AWS_ACCESS_KEY_ID="test", AWS_SECRET_ACCESS_KEY="test"), mock.patch.object(PUBLISH.subprocess, "run", side_effect=upload):
            if fail_at or raises:
                with self.assertRaises(raises or subprocess.CalledProcessError):
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
        self.assertTrue(all(key.startswith("oss-transfer/v4.1.0-test/") for key in keys))

    def test_latest_mirrors_every_release_file_after_versioned_uploads(self):
        self.stable()
        keys = self.uploaded()
        names = sorted(path.name for path in self.output.iterdir())
        versioned = ["oss-transfer/v4.1.0/" + name for name in names]
        self.assertEqual(sorted(keys[:len(versioned)]), versioned)
        latest = keys[len(versioned):]
        # Every archive, installer and SHA256SUMS lands before the pointers;
        # manifest.tsv then release.json are the final writes.
        self.assertEqual(sorted(latest), ["oss-transfer/latest/" + name for name in names])
        self.assertEqual(latest[-2:], ["oss-transfer/latest/manifest.tsv", "oss-transfer/latest/release.json"])
        for name in ("native-install.sh", "SHA256SUMS", "install.sh", "Install.ps1", PACKAGE.TARGETS.archive_name("linux", "amd64")):
            self.assertIn("oss-transfer/latest/" + name, latest[:-2])

    def test_only_retired_files_and_stale_archives_are_deleted_from_latest(self):
        self.stable()
        stale = "oss-transfer/latest/oss-transfer-linux-mips64.tar.gz"
        keys = self.uploaded(remote={"oss-transfer/latest/VERSION": b"4.0.0\n", "oss-transfer/latest/install.sh": b"old", stale: b"old"})
        self.assertEqual(sorted(keys[-2:]), sorted(["oss-transfer/latest/VERSION", stale]))
        self.assertEqual(keys[-3], "oss-transfer/latest/release.json")

    def test_unknown_latest_file_fails_verification_without_deletion(self):
        self.stable()
        keys = self.uploaded(remote={"oss-transfer/latest/notes.txt": b"manual"}, raises=ValueError)
        self.assertEqual(keys[-1], "oss-transfer/latest/release.json")

    def test_remote_content_must_match_local_md5_when_etag_is_plain(self):
        self.stable()
        keys = self.uploaded(etag=lambda data: '"%s"' % hashlib.md5(data + b"x").hexdigest(), raises=ValueError)
        # The version directory is checked before latest/ can advance.
        self.assertTrue(keys)
        self.assertFalse(any("/latest/" in key for key in keys))
        # Multipart-style ETags are not MD5s; key and size still verify.
        self.assertTrue(self.uploaded(etag=lambda data: '"%s-2"' % hashlib.md5(data).hexdigest()))

    def test_failed_payload_upload_preserves_latest(self):
        self.stable()
        keys = self.uploaded(fail_at=3)
        self.assertGreaterEqual(len(keys), 3)
        self.assertFalse(any("/latest/" in key for key in keys))


if __name__ == "__main__":
    unittest.main()
