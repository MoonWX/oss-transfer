#!/usr/bin/env python3
"""Check GitHub metadata parsing and archive verification before installation."""
import hashlib
import io
import json
import os
import shutil
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT / "deploy/native/download-source.sh"
ARCHIVE = "oss-transfer-linux-amd64.tar.gz"
PREFIX = "oss-transfer-linux-amd64/"
TAG = "v4.1.5"
COMMIT = "a" * 40
API = "https://api.github.com/repos/MoonWX/oss-transfer/releases/latest"
ASSET_URL = f"https://github.com/MoonWX/oss-transfer/releases/download/{TAG}/{ARCHIVE}"
INSTALLER_URL = f"https://raw.githubusercontent.com/MoonWX/oss-transfer/{COMMIT}/deploy/native/install.sh"


class DownloadSourceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="download-source-test-")
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.package = self.root / "package"
        self.package.mkdir()
        self.archive = self.root / ARCHIVE
        self.metadata = self.root / "release.json"
        self.installer = self.root / "install.sh"
        self.installer.write_text("#!/bin/bash\nexit 99\n")
        self.log = self.root / "requests.log"
        self.make_archive()
        self.release = self.make_release()
        self.metadata.write_text(json.dumps(self.release))
        shim = self.root / "bin"
        shim.mkdir()
        (shim / "curl").write_text('#!' + sys.executable + '\n' + """
import json, os, pathlib, shutil, sys
args=sys.argv[1:]
url=next(arg for arg in args if arg.startswith('https://'))
with open(os.environ['REQUEST_LOG'],'a') as log: log.write(url+'\\n')
mapping=json.loads(os.environ['DOWNLOAD_MAP'])
if url not in mapping: sys.exit(22)
source=pathlib.Path(mapping[url])
if source.stat().st_size > int(args[args.index('--max-filesize')+1]): sys.exit(63)
shutil.copyfile(source,args[args.index('-o')+1])
""")
        (shim / "curl").chmod(0o755)
        self.env = os.environ.copy()
        self.env["PATH"] = str(shim) + os.pathsep + self.env["PATH"]
        self.env["REQUEST_LOG"] = str(self.log)
        self.env["DOWNLOAD_MAP"] = json.dumps({
            API: str(self.metadata),
            f"https://api.github.com/repos/MoonWX/oss-transfer/releases/tags/{TAG}": str(self.metadata),
            ASSET_URL: str(self.archive), INSTALLER_URL: str(self.installer),
            INSTALLER_URL.replace("/deploy/native/install.sh", "/install.sh"): str(self.installer),
            INSTALLER_URL.replace("/install.sh", "/Install.ps1"): str(self.installer),
        })

    def make_archive(self, modification=None):
        version = (f"Version: {TAG[1:]}\nCommit: {COMMIT[:7]}\nFullCommit: {COMMIT}\n"
                   "BuildTime: 2026-10-15T00:00:00Z\nPlatform: linux/amd64\n").encode()
        payloads = {"VERSION": version}
        for role in ("master", "agent", "cli"):
            payloads[f"oss-transfer-{role}"] = f"#!/bin/sh\necho NEVER_EXECUTE_{role}\n".encode()
        payloads["SHA256SUMS"] = "".join(
            hashlib.sha256(value).hexdigest() + "  " + key + "\n"
            for key, value in payloads.items()
        ).encode()
        entries = [(PREFIX + name, value, None) for name, value in payloads.items()]
        if modification:
            entries = modification(entries)
        with tarfile.open(self.archive, "w:gz") as archive:
            for name, payload, kind in entries:
                item = tarfile.TarInfo(name)
                item.mode = 0o755
                if kind in ("symlink", "hardlink"):
                    item.type = tarfile.SYMTYPE if kind == "symlink" else tarfile.LNKTYPE
                    item.linkname = "/etc/passwd"
                    archive.addfile(item)
                else:
                    item.size = len(payload)
                    archive.addfile(item, io.BytesIO(payload))

    def make_release(self):
        return {"tag_name": TAG, "draft": False, "assets": [{
            "name": ARCHIVE, "digest": "sha256:" + hashlib.sha256(self.archive.read_bytes()).hexdigest(),
            "size": self.archive.stat().st_size, "browser_download_url": ASSET_URL,
        }]}

    def metadata_result(self, text):
        self.metadata.write_text(text)
        return subprocess.run(
            ["bash", "-c", 'source "$1"; transfer_github_metadata "$2" "$3"', "test", str(HELPER), str(self.metadata), ARCHIVE],
            capture_output=True, text=True,
        )

    def prepare(self, version="latest"):
        self.metadata.write_text(json.dumps(self.release))
        return subprocess.run(
            ["bash", "-c", 'set -euo pipefail; source "$1"; transfer_github_prepare "$2" linux amd64 "$3" || exit $?',
             "test", str(HELPER), version, str(self.package)],
            env=self.env, capture_output=True, text=True,
        )

    def assert_refused_before_installer(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(list(self.package.iterdir()), [])
        self.assertNotIn(INSTALLER_URL, self.log.read_text() if self.log.exists() else "")
        self.assertNotIn("NEVER_EXECUTE", result.stdout)

    def test_parser_accepts_reordered_compact_and_escaped_keys(self):
        reordered = {"body": {"assets": [self.release["assets"][0]], "tag_name": "v9.9.9"},
                     "assets": self.release["assets"], "draft": False, "tag_name": TAG}
        for text in (json.dumps(reordered), json.dumps(reordered, indent=3),
                     json.dumps(reordered).replace('"tag_name"', '"tag\\u005fname"')):
            with self.subTest(text=text[:50]):
                result = self.metadata_result(text)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.split("\t")[0], TAG)
                self.assertEqual(result.stdout.rstrip().split("\t")[-1], ASSET_URL)

    def test_parser_rejects_ambiguous_or_malformed_json(self):
        original = json.dumps(self.release)
        bad_inputs = [
            original + "{}", original[:-1], original.replace('"draft": false', '"draft": false, "draft": true'),
            original.replace('"tag_name":', '"tag_name": "v8.0.0", "tag\\u005fname":'),
            original.replace('"size": ', '"size": 0'),
            original.replace('"draft": false', '"draft": "false"'),
            original.replace('"sha256:', '"sha512:'),
            original.replace("https://github.com/MoonWX", "https://evil.example/MoonWX"),
            original.replace('"name":', '"name": "different", "name":'),
            original.replace('"assets":', '"body":'),
            original.replace('"assets": [', '"assets": [{"name":"irrelevant"},').replace('"digest": ', '"digest": null, "digest": '),
            original.replace('"tag_name": "v4.1.5"', '"tag_name": "v4.1.5\\n"'),
            original.replace('"tag_name": "v4.1.5"', '"tag_name": "v4.1.5\\q"'),
            original.replace('"assets": [', '"assets": [')[:-2] + ',]}',
        ]
        duplicate_asset = dict(self.release)
        duplicate_asset["assets"] = self.release["assets"] * 2
        bad_inputs.append(json.dumps(duplicate_asset))
        for text in bad_inputs:
            with self.subTest(text=text[:100]):
                result = self.metadata_result(text)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertEqual(result.stdout, "")

    def test_prepare_produces_verified_package_and_commit_pinned_installer(self):
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual({p.name for p in self.package.iterdir()}, {ARCHIVE, "manifest.tsv", "native-install.sh", "install.sh", "Install.ps1"})
        manifest = (self.package / "manifest.tsv").read_text()
        self.assertIn(f"version\t{TAG[1:]}\ncommit\t{COMMIT}\n", manifest)
        self.assertEqual(sum(line.startswith("file\t") for line in manifest.splitlines()), 3)
        self.assertIn("archive\tlinux\tamd64\t" + ARCHIVE, manifest)
        self.assertEqual((self.package / ARCHIVE).read_bytes(), self.archive.read_bytes())
        self.assertEqual(self.log.read_text().splitlines(), [API, ASSET_URL, INSTALLER_URL,
                         INSTALLER_URL.replace("/deploy/native/install.sh", "/install.sh"), INSTALLER_URL.replace("/install.sh", "/Install.ps1")])
        self.assertNotIn("NEVER_EXECUTE", result.stdout)

    def test_old_release_uses_legacy_installer_path_at_same_verified_commit(self):
        mapping = json.loads(self.env["DOWNLOAD_MAP"])
        legacy_url = INSTALLER_URL.replace("/deploy/native/", "/deploy/go/")
        mapping[legacy_url] = mapping.pop(INSTALLER_URL)
        self.env["DOWNLOAD_MAP"] = json.dumps(mapping)
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.log.read_text().splitlines(), [API, ASSET_URL, INSTALLER_URL, legacy_url,
                         INSTALLER_URL.replace("/deploy/native/install.sh", "/install.sh"), INSTALLER_URL.replace("/install.sh", "/Install.ps1")])
        self.assertEqual((self.package / "native-install.sh").read_bytes(), self.installer.read_bytes())

    def test_missing_pinned_installers_fail_without_using_a_moving_branch(self):
        mapping = json.loads(self.env["DOWNLOAD_MAP"])
        del mapping[INSTALLER_URL]
        self.env["DOWNLOAD_MAP"] = json.dumps(mapping)
        result = self.prepare()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(list(self.package.iterdir()), [])
        self.assertIn("Cannot fetch the versioned native installer", result.stderr)
        self.assertEqual(self.log.read_text().splitlines(), [API, ASSET_URL, INSTALLER_URL,
                         INSTALLER_URL.replace("/deploy/native/", "/deploy/go/")])

    def test_fixed_version_uses_tag_endpoint(self):
        result = self.prepare(TAG)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("/releases/tags/" + TAG, self.log.read_text().splitlines()[0])

    def test_wrong_api_digest_or_size_is_refused_before_extraction(self):
        for field, value in (("digest", "sha256:" + "0" * 64), ("size", self.archive.stat().st_size + 1),
                             ("size", self.archive.stat().st_size - 1)):
            with self.subTest(field=field, value=value):
                self.release = self.make_release()
                self.release["assets"][0][field] = value
                self.assert_refused_before_installer(self.prepare())

    def test_archive_duplicate_link_missing_and_wrong_identity_are_refused(self):
        cases = {
            "duplicate": lambda entries: entries + [entries[0]],
            "symlink": lambda entries: [(name, body, "symlink" if name.endswith("/VERSION") else kind) for name, body, kind in entries],
            "hardlink": lambda entries: [(name, body, "hardlink" if name.endswith("/oss-transfer-agent") else kind) for name, body, kind in entries],
            "missing": lambda entries: [item for item in entries if not item[0].endswith("/oss-transfer-cli")],
            "identity": lambda entries: [(name, body.replace(b"Platform: linux/amd64", b"Platform: linux/arm64") if name.endswith("/VERSION") else body, kind) for name, body, kind in entries],
            "inner_hash": lambda entries: [(name, body + b"corrupt" if name.endswith("/oss-transfer-master") else body, kind) for name, body, kind in entries],
            "metadata_duplicate": lambda entries: [(name, body + b"Version: 4.1.5\n" if name.endswith("/VERSION") else body, kind) for name, body, kind in entries],
        }
        for label, modify in cases.items():
            with self.subTest(label=label):
                self.make_archive(modify)
                self.release = self.make_release()
                self.assert_refused_before_installer(self.prepare())

    def test_unselected_paths_cannot_write_outside_staging(self):
        target = self.root / "escaped"
        self.make_archive(lambda entries: entries + [(str(target), b"must not be extracted", None),
                                                      ("../../escaped", b"must not be extracted", None)])
        self.release = self.make_release()
        result = self.prepare()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(target.exists())

    def public_installer(self, method):
        for name, body in {
            'uname': 'case "$1" in -s) echo Linux;; -m) echo x86_64;; esac',
            'id': 'echo 0',
            'docker': 'exit 1',
        }.items():
            path = self.root / 'bin' / name
            path.write_text('#!/bin/sh\n' + body + '\n')
            path.chmod(0o755)
        self.installer.write_text('#!/bin/bash\nprintf "PINNED_INSTALLER\\n"\nprintf "%s\\n" "$@"\nexit 99\n')
        self.metadata.write_text(json.dumps(self.release))
        return subprocess.run(
            ['bash', str(ROOT/'install.sh'), '--role', 'master', '--method', method,
             '--source', 'github', '--version', TAG, '--yes', '--no-deps',
             '--install-dir', str(self.root.resolve()/'service'), '--data-dir', str(self.root.resolve()/'data')],
            env=dict(self.env, TMPDIR=str(self.root.resolve())), text=True, capture_output=True,
        )

    def test_public_native_installer_executes_only_pinned_script_with_verified_package(self):
        result = self.public_installer('native')
        self.assertEqual(result.returncode, 99, result.stderr)
        self.assertIn('PINNED_INSTALLER\n', result.stdout)
        self.assertIn('--package-dir\n', result.stdout)
        self.assertNotIn('--release-url', result.stdout)
        self.assertNotIn('NEVER_EXECUTE', result.stdout)
        self.assertFalse(list(self.root.glob('oss-transfer-setup.*')))

    def test_public_native_installer_rejects_corrupt_package_before_running_script(self):
        self.release['assets'][0]['digest'] = 'sha256:' + '0'*64
        result = self.public_installer('native')
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('PINNED_INSTALLER', result.stdout)
        self.assertNotIn(INSTALLER_URL, self.log.read_text())

    def local_prepare(self, source, *fallbacks):
        return subprocess.run(
            ["bash", "-c", 'set -euo pipefail; source "$1"; shift; transfer_local_prepare "$@" || exit $?',
             "test", str(HELPER), str(source), "linux", "amd64", str(self.package), *fallbacks],
            env=self.env, capture_output=True, text=True,
        )

    def test_local_archive_without_manifest_is_verified_offline(self):
        source = self.root / "copied"
        source.mkdir()
        shutil.copyfile(self.archive, source / ARCHIVE)
        (source / "install.sh").write_text("public installer\n")
        fallback = self.root / "embedded-native.sh"
        fallback.write_text("embedded native installer\n")
        result = self.local_prepare(source, "native-install.sh", str(fallback))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.log.exists(), "offline preparation made a request")
        self.assertEqual({p.name for p in self.package.iterdir()}, {ARCHIVE, "manifest.tsv", "native-install.sh", "install.sh"})
        manifest = (self.package / "manifest.tsv").read_text()
        self.assertIn(f"version\t{TAG[1:]}\ncommit\t{COMMIT}\n", manifest)
        self.assertEqual(sum(line.startswith("file\t") for line in manifest.splitlines()), 3)
        digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        self.assertIn(f"archive\tlinux\tamd64\t{ARCHIVE}\t{digest}\t{self.archive.stat().st_size}\n", manifest)
        self.assertIn("asset\tnative-install.sh\t" + hashlib.sha256(b"embedded native installer\n").hexdigest(), manifest)

    def test_local_archive_with_wrong_inner_checksum_is_refused(self):
        self.make_archive(lambda entries: [(name, body + b"corrupt" if name.endswith("/oss-transfer-master") else body, kind)
                                           for name, body, kind in entries])
        source = self.root / "copied"
        source.mkdir()
        shutil.copyfile(self.archive, source / ARCHIVE)
        result = self.local_prepare(source)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum mismatch", result.stderr)
        self.assertEqual(list(self.package.iterdir()), [])

    def test_missing_manifest_and_archive_lists_required_files(self):
        source = self.root / "empty"
        source.mkdir()
        for language, phrase in (("en", "Copy the complete release"), ("zh", "下载完整发布文件")):
            result = subprocess.run(
                ["bash", "-c", 'source "$1"; shift; transfer_local_prepare "$@"', "test", str(HELPER),
                 str(source), "linux", "amd64", str(self.package)],
                env=dict(self.env, OSS_TRANSFER_INSTALL_LANG=language), capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn(phrase, result.stderr)
            for name in ("manifest.tsv", ARCHIVE, "native-install.sh"):
                self.assertIn(name, result.stderr)

    def test_install_script_embeds_the_current_native_installer(self):
        source = (ROOT / "install.sh").read_text()
        block = source.split("# BEGIN embedded native installer\n", 1)[1].split("# END embedded native installer\n", 1)[0]
        result = subprocess.run(["bash", "-c", block + "\ntransfer_embedded_native_installer"], capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, (ROOT / "deploy/native/install.sh").read_bytes())

    def offline_installer(self, bundle, *arguments, cwd=None):
        self.public_installer_shims()
        return subprocess.run(
            ['bash', str(bundle / 'install.sh'), '--role', 'master', '--yes', '--lang', 'en',
             '--install-dir', str(self.root.resolve()/'service'), '--data-dir', str(self.root.resolve()/'data'), *arguments],
            env=dict(self.env, TMPDIR=str(self.root.resolve())), text=True, capture_output=True, cwd=cwd or self.root,
        )

    def public_installer_shims(self):
        for name, body in {
            'uname': 'case "$1" in -s) echo Linux;; -m) echo x86_64;; esac',
            'id': 'echo 0',
            'docker': 'exit 1',
        }.items():
            path = self.root / 'bin' / name
            path.write_text('#!/bin/sh\n' + body + '\n')
            path.chmod(0o755)

    def test_copied_archive_and_installers_install_offline_without_manifest(self):
        bundle = self.root / 'bundle'
        bundle.mkdir()
        shutil.copyfile(ROOT / 'install.sh', bundle / 'install.sh')
        shutil.copyfile(self.archive, bundle / ARCHIVE)
        (bundle / 'native-install.sh').write_text('#!/bin/bash\nprintf "LOCAL_NATIVE\\n"\nprintf "%s\\n" "$@"\nexit 99\n')
        result = self.offline_installer(bundle)
        self.assertEqual(result.returncode, 99, result.stderr)
        self.assertIn('Using local release files (offline): ' + str(bundle.resolve()), result.stdout)
        self.assertIn('No manifest.tsv found', result.stdout)
        self.assertIn('LOCAL_NATIVE\n', result.stdout)
        self.assertIn('--package-dir\n', result.stdout)
        self.assertNotIn('--release-url', result.stdout)
        self.assertNotIn('Download source', result.stdout)
        self.assertFalse(self.log.exists(), 'offline installation made a network request')
        self.assertEqual({p.name for p in bundle.iterdir()}, {'install.sh', ARCHIVE, 'native-install.sh'})

    def test_offline_bundle_in_current_directory_rejects_other_version(self):
        bundle = self.root / 'bundle'
        bundle.mkdir()
        shutil.copyfile(self.archive, bundle / ARCHIVE)
        tools = self.root / 'tools'
        tools.mkdir()
        shutil.copyfile(ROOT / 'install.sh', tools / 'install.sh')
        self.public_installer_shims()
        result = subprocess.run(['bash', str(tools / 'install.sh'), '--role', 'master', '--yes', '--lang', 'zh', '--version', '9.9.9'],
                                env=dict(self.env, TMPDIR=str(self.root.resolve())), text=True, capture_output=True, cwd=bundle)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('使用本地安装包目录（离线）: ' + str(bundle.resolve()), result.stdout)
        self.assertIn('本地安装包版本为 4.1.5，不是 9.9.9', result.stderr)
        self.assertFalse(self.log.exists(), 'offline installation made a network request')

    def test_public_compose_installer_accepts_internally_prepared_github_package(self):
        result = self.public_installer('compose')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Install Docker Engine and the Compose plugin first', result.stderr)
        self.assertNotIn('Native configuration/package options', result.stderr)
        self.assertNotIn('PINNED_INSTALLER', result.stdout)
        self.assertFalse((self.root/'service').exists())


if __name__ == "__main__":
    unittest.main()
