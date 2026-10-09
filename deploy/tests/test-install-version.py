#!/usr/bin/env python3
"""Run installer version selection without touching services or installing files."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


INSTALLER = Path(__file__).resolve().parents[1] / "install.sh"


class InstallerVersionTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="oss-install-version-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.source = self.root / "source"
        self.deploy = self.source / "deploy"
        self.deploy.mkdir(parents=True)
        shutil.copy2(INSTALLER, self.deploy / "install.sh")
        (self.deploy / "preflight-node.sh").write_text(
            "#!/bin/bash\nprintf 'x86_64\\t/usr/bin/java\\t8\\n'\n"
        )
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.log = self.root / "requests.jsonl"
        self.env = dict(os.environ)
        for key in list(self.env):
            if key.startswith("OSS_TRANSFER_") or key == "BASH_ENV":
                self.env.pop(key)
        self.env.update(PATH=f"{self.bin}:{self.env['PATH']}", TMPDIR=str(self.root),
                        INSTALL_TEST_LOG=str(self.log))
        self.stub("uname", "printf 'Linux\\n'")
        self.stub("id", "printf '0\\n'")
        for name in ("systemctl", "ss", "timeout", "sha256sum"):
            self.stub(name, "exit 92")
        curl = self.bin / "curl"
        curl.write_text("""#!/usr/bin/env python3
import json, os, sys
with open(os.environ['INSTALL_TEST_LOG'], 'a') as output:
    output.write(json.dumps(sys.argv[1:]) + '\\n')
# Stop before download/extraction/systemd writes. No real network requests.
sys.exit(71)
""")
        curl.chmod(0o755)

    def stub(self, name, body):
        path = self.bin / name
        path.write_text("#!/bin/bash\n" + body + "\n")
        path.chmod(0o755)

    def run_installer(self, *args):
        return subprocess.run([
            "bash", str(self.deploy / "install.sh"), "--role", "master", "--no-start",
            "--install-dir", str(self.root / "install"),
            "--config-dir", str(self.root / "config"),
            "--data-dir", str(self.root / "data"),
            "--cli-path", str(self.root / "cli"), *args,
        ], env=self.env, text=True, capture_output=True, timeout=10)

    def assert_download(self, expected, *args):
        result = self.run_installer(*args)
        self.assertEqual(result.returncode, 71, result.stdout + result.stderr)
        requests = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.assertEqual(len(requests), 1)
        self.assertIn(expected, requests[0])
        self.assertFalse((self.root / "install").exists())
        self.assertFalse((self.root / "config").exists())

    def test_source_defaults_to_latest_stable(self):
        self.assert_download("https://github.com/MoonWX/oss-transfer/releases/latest/download/oss-transfer.tar.gz")

    def test_old_metadata_does_not_pin_a_download(self):
        (self.source / "VERSION").write_text("Commit: fixture\nVersion: 4.7.2\n")
        self.assert_download("https://github.com/MoonWX/oss-transfer/releases/latest/download/oss-transfer.tar.gz")

    def test_explicit_version_overrides_package_metadata(self):
        (self.source / "VERSION").write_text("Version: 4.7.2\n")
        self.assert_download("https://github.com/MoonWX/oss-transfer/releases/download/v9.1.0/oss-transfer.tar.gz",
                             "--version", "v9.1.0")

    def test_explicit_latest_overrides_package_metadata(self):
        (self.source / "VERSION").write_text("Version: 4.7.2\n")
        self.assert_download("https://github.com/MoonWX/oss-transfer/releases/latest/download/oss-transfer.tar.gz",
                             "--version", "latest")

    def test_invalid_explicit_version_is_rejected_before_download(self):
        result = self.run_installer("--version", "../../other")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("invalid release version", result.stderr)
        self.assertFalse(self.log.exists())

    def test_explicit_local_package_uses_local_files(self):
        package = self.root / "selected-package"
        package.mkdir()
        (package / "VERSION").write_text("Version: 4.7.2\n")
        result = self.run_installer("--package-dir", str(package))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("package is missing oss-transfer.jar", result.stderr)
        self.assertNotIn("invalid package VERSION", result.stderr)
        self.assertFalse(self.log.exists())

    def test_packaged_installer_uses_local_files_without_download(self):
        (self.source / "oss-transfer.jar").write_text("fixture jar")
        result = self.run_installer()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("package is missing oss-transfer-cli-amd64", result.stderr)
        self.assertFalse(self.log.exists())


if __name__ == "__main__":
    unittest.main(verbosity=2)
