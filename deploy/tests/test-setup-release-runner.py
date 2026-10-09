#!/usr/bin/env python3
"""Offline Runner preparation regressions; no packages, host services or network."""
import hashlib
import io
import json
import os
from pathlib import Path
import shlex
import subprocess
import tarfile
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "setup-release-runner.sh"
REPO = "https://github.com/MoonWX/oss-transfer"


class RunnerSetupTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="oss-runner-setup-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.runner = self.root / "runner"
        self.cache = self.root / "cache"
        self.work = self.root / "work"
        self.work.mkdir()
        self.binfmt = self.root / "qemu-aarch64"
        self.binfmt.write_text("enabled\nflags: POCF\n")
        self.script = self.root / "setup-release-runner.sh"
        self.script.write_text(SCRIPT.read_text().replace("/proc/sys/fs/binfmt_misc/qemu-aarch64", str(self.binfmt)))
        self.requests = self.root / "requests"
        self.archive = self.root / "fixture.tar.gz"
        with tarfile.open(self.archive, "w:gz") as archive:
            for name in ("config.sh", "bin/Runner.Listener", "bin/installdependencies.sh"):
                content = b"#!/bin/bash\nexit 0\n"
                entry = tarfile.TarInfo(name)
                entry.mode = 0o755
                entry.size = len(content)
                archive.addfile(entry, io.BytesIO(content))
        self.digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        self.metadata = self.root / "release.json"
        self.asset_name = "actions-runner-linux-x64-9.8.7.tar.gz"
        self.asset_url = "https://github.com/actions/runner/releases/download/v9.8.7/" + self.asset_name
        self.release = {
            "tag_name": "v9.8.7", "draft": False, "prerelease": False,
            "assets": [{"name": self.asset_name, "browser_download_url": self.asset_url,
                        "digest": "sha256:" + self.digest}],
        }
        self.write_metadata()
        self.env = dict(os.environ)
        for key in list(self.env):
            if key in ("BASH_ENV", "RUNNER_TOKEN", "RUNNER_LABEL", "RUNNER_NAME") or key.startswith("ACTIONS_RUNNER_INPUT_"):
                self.env.pop(key)

    def write_metadata(self):
        self.metadata.write_text(json.dumps(self.release))

    def run_shell(self, body, stdin=None, **env):
        prefix = f"""
source {shlex.quote(str(self.script))}
runner_dir={shlex.quote(str(self.runner))}
runner_cache={shlex.quote(str(self.cache))}
runner_home={shlex.quote(str(self.root / 'home'))}
work_dir={shlex.quote(str(self.work))}
download() {{
  printf '%s\\n' "$1" >> {shlex.quote(str(self.requests))}
  if [[ "$1" == https://api.github.com/repos/actions/runner/releases/latest ]]; then
    cp {shlex.quote(str(self.metadata))} "$2"
  else cp {shlex.quote(str(self.archive))} "$2"; fi
}}
# Never change actual ownership. GNU stat is mocked for macOS portability.
chown() {{ :; }}
stat() {{ printf '0\\n'; }}
# Host may have the BSD sha256sum shim; validate actual archive bytes offline.
sha256sum() {{ python3 -c 'import hashlib, sys; expected, path = sys.stdin.readline().strip().split("  ", 1); sys.exit(0 if hashlib.sha256(open(path, "rb").read()).hexdigest() == expected else 1)'; }}
"""
        return subprocess.run(["bash", "-c", prefix + body], env={**self.env, **env},
                              input=stdin, text=True, capture_output=True, timeout=10)

    def assert_failure(self, result, message):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(message, result.stdout + result.stderr)

    def test_official_asset_requires_a_digest_and_exact_source(self):
        result = self.run_shell(f"runner_asset {shlex.quote(str(self.metadata))}")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "\t".join((self.asset_name, self.asset_url, self.digest)))
        for change in ({"digest": None}, {"browser_download_url": "https://example.com/runner.tar.gz"}):
            self.release["assets"][0].update(change)
            self.write_metadata()
            self.assert_failure(self.run_shell(f"runner_asset {shlex.quote(str(self.metadata))}"), "SHA256 is unavailable")

    def test_ambiguous_or_prerelease_runner_is_rejected(self):
        self.release["assets"].append(dict(self.release["assets"][0]))
        self.write_metadata()
        self.assert_failure(self.run_shell(f"runner_asset {shlex.quote(str(self.metadata))}"), "ambiguous")
        self.release["prerelease"] = True
        self.write_metadata()
        self.assert_failure(self.run_shell(f"runner_asset {shlex.quote(str(self.metadata))}"), "Invalid official")

    def test_verified_archive_prepares_without_a_generated_service_or_token(self):
        result = self.run_shell("prepare_runner")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.runner / ".oss-transfer-prepared").read_text().strip(), self.digest)
        self.assertTrue((self.runner / "config.sh").exists())
        self.assertFalse((self.runner / "svc.sh").exists())
        self.assertFalse((self.runner / ".runner").exists())
        self.assertEqual(self.requests.read_text().splitlines(), [
            "https://api.github.com/repos/actions/runner/releases/latest", self.asset_url])

    def test_bad_archive_checksum_fails_before_extraction(self):
        self.release["assets"][0]["digest"] = "sha256:" + "0" * 64
        self.write_metadata()
        self.assert_failure(self.run_shell("prepare_runner"), "SHA256 verification failed")
        self.assertFalse(self.runner.exists())
        self.assertFalse((self.cache / self.asset_name).exists())

    def test_prepared_repeat_verifies_cached_archive_and_skips_asset_download(self):
        self.assertEqual(self.run_shell("prepare_runner").returncode, 0)
        self.requests.unlink()
        result = self.run_shell("prepare_runner")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.requests.read_text().splitlines(), ["https://api.github.com/repos/actions/runner/releases/latest"])

    def test_unknown_nonempty_installation_is_never_overwritten(self):
        self.runner.mkdir()
        keep = self.runner / "existing-data"
        keep.write_text("preserve")
        self.assert_failure(self.run_shell("prepare_runner"), "inspect it before retrying")
        self.assertEqual(keep.read_text(), "preserve")

    def configured(self, repo=REPO, label="jdcloud-publish"):
        self.runner.mkdir(exist_ok=True)
        (self.runner / ".runner").write_text(json.dumps({"gitHubUrl": repo}))
        (self.runner / ".oss-transfer-runner-label").write_text(label + "\n")

    def test_other_repository_registration_is_never_replaced(self):
        self.configured("https://github.com/another/repository")
        self.assert_failure(self.run_shell("validate_existing"), "another repository")
        self.assertFalse(self.requests.exists())

    def test_same_repository_is_reused_without_consuming_a_token(self):
        self.configured()
        result = self.run_shell("validate_existing", RUNNER_TOKEN="offline-synthetic-token")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Reusing", result.stdout)
        self.assertNotIn("offline-synthetic-token", result.stdout + result.stderr)

    def test_conflicting_existing_label_is_explicit(self):
        self.configured(label="another-label")
        self.assert_failure(self.run_shell("validate_existing"), "label differs")

    def config_fixture(self):
        self.runner.mkdir()
        capture = self.root / "config.json"
        path = self.runner / "config.sh"
        path.write_text("#!/usr/bin/env python3\n" + f"""
import json, os, sys
token = os.environ.get('ACTIONS_RUNNER_INPUT_TOKEN', '')
json.dump({{'argv': sys.argv[1:], 'has_token': bool(token),
           'parent_token': 'RUNNER_TOKEN' in os.environ,
           'parent_pat': 'GH_TOKEN' in os.environ or 'GITHUB_TOKEN' in os.environ,
           'in_args': any(token in arg for arg in sys.argv) if token else False}}, open({str(capture)!r}, 'w'))
open('.env', 'w').write('')
""")
        path.chmod(0o755)
        return capture

    def test_token_uses_supported_config_environment_and_never_argv_or_service_env(self):
        capture = self.config_fixture()
        # Mimic runuser after verifying its account argument; use real env -i/bash.
        body = """
runuser() { [[ "$1" == -u && "$2" == oss-release && "$3" == -- ]]; shift 3; "$@"; }
configure_runner
"""
        result = self.run_shell(body, RUNNER_TOKEN="offline-registration-token", GH_TOKEN="offline-pat")
        self.assertEqual(result.returncode, 0, result.stderr)
        record = json.loads(capture.read_text())
        self.assertTrue(record["has_token"])
        self.assertFalse(record["in_args"])
        self.assertFalse(record["parent_token"])
        self.assertFalse(record["parent_pat"])
        self.assertEqual(record["argv"][0:3], ["--unattended", "--url", REPO])
        self.assertNotIn("--replace", record["argv"])
        self.assertNotIn("--disableupdate", record["argv"])
        self.assertNotIn("offline-registration-token", result.stdout + result.stderr)
        self.assertNotIn("offline-registration-token", (self.runner / ".env").read_text())

    def test_stdin_token_is_supported_and_invalid_tokens_fail_before_config(self):
        capture = self.config_fixture()
        body = 'runuser() { shift 3; "$@"; }; configure_runner'
        result = self.run_shell(body, stdin="offline-stdin-token\n")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(capture.read_text())["has_token"])
        capture.unlink()
        self.assert_failure(self.run_shell(body, stdin="bad token\n"), "short-lived")
        self.assertFalse(capture.exists())

    def test_personal_access_token_is_never_accepted_for_registration(self):
        self.runner.mkdir()
        result = self.run_shell("configure_runner", RUNNER_TOKEN="github_pat_offline_wrong_token")
        self.assert_failure(result, "not a GitHub personal access token")

    def test_wrong_aws_signing_key_is_rejected_before_signature_verification(self):
        result = self.run_shell("""
gpg() { [[ "$*" == *show-only* ]] || { printf 'UNEXPECTED VERIFY'; return 1; }; printf 'fpr:::::::::WRONG:\n'; }
verify_aws key signature archive
""")
        self.assert_failure(result, "fingerprint does not match")
        self.assertNotIn("UNEXPECTED", result.stdout)

    def test_aws_bad_signature_is_fail_closed(self):
        result = self.run_shell("""
gpg() {
  if [[ "$*" == *show-only* ]]; then printf 'fpr:::::::::FB5DB77FD5C118B80511ADA8A6310ACC4672475C:\n';
  elif [[ "$*" == *--verify* ]]; then return 1; fi
}
verify_aws key signature archive
""")
        self.assert_failure(result, "signature verification failed")

    def service_fixture(self):
        self.runner.mkdir()
        (self.runner / ".service").write_text("actions.runner.MoonWX-oss-transfer.offline.service\n")

    def test_service_must_have_the_dedicated_account(self):
        self.service_fixture()
        result = self.run_shell("""
systemctl() { [[ "$*" == *'-p User'* ]] && printf 'root\n' || printf '%s/runsvc.sh\n' "$runner_dir"; }
start_service
""")
        self.assert_failure(result, "service account or executable")

    def test_running_and_listening_service_is_reused_without_restart(self):
        self.service_fixture()
        result = self.run_shell("""
systemctl() {
  if [[ "$*" == *'-p User'* ]]; then printf 'oss-release\n';
  elif [[ "$*" == *'-p ExecStart'* ]]; then printf '%s/runsvc.sh\n' "$runner_dir";
  elif [[ "$*" == *'-p MainPID'* ]]; then printf '1234\n'; fi
}
journalctl() { printf 'Listening for Jobs\n'; }
start_service
""")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("already active", result.stdout)
        self.assertIn("Confirm Online", result.stdout)
        self.assertNotIn("active and listening", result.stdout)

    def test_new_service_requires_a_fresh_listener_event_after_its_start(self):
        self.service_fixture()
        svc = self.runner / "svc.sh"
        svc.write_text("#!/bin/bash\nexit 0\n")
        svc.chmod(0o755)
        journal_args = self.root / "journal-args"
        result = self.run_shell(f"""
active_checks=0
systemctl() {{
  if [[ "$*" == *'-p User'* ]]; then printf 'oss-release\\n';
  elif [[ "$*" == *'-p ExecStart'* ]]; then printf '%s/runsvc.sh\\n' "$runner_dir";
  elif [[ "$*" == *'-p MainPID'* ]]; then printf '1234\\n';
  elif [[ "$1" == is-active ]]; then ((active_checks+=1)); ((active_checks>1)); fi
}}
journalctl() {{ printf '%s\\n' "$@" > {shlex.quote(str(journal_args))}; printf 'Listening for Jobs\\n'; }}
start_service
""")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("active and listening", result.stdout)
        self.assertIn("--since\n", journal_args.read_text())

    def test_service_without_listener_is_bounded_and_fails(self):
        self.service_fixture()
        svc = self.runner / "svc.sh"
        svc.write_text("#!/bin/bash\nexit 0\n")
        svc.chmod(0o755)
        result = self.run_shell("""
active_checks=0
systemctl() {
  if [[ "$*" == *'-p User'* ]]; then printf 'oss-release\n';
  elif [[ "$*" == *'-p ExecStart'* ]]; then printf '%s/runsvc.sh\n' "$runner_dir";
  elif [[ "$*" == *'-p MainPID'* ]]; then printf '1234\n';
  elif [[ "$1" == is-active ]]; then ((active_checks+=1)); ((active_checks>1)); fi
}
journalctl() { printf 'Not connected\n'; }
sleep() { :; }
start_service
""")
        self.assert_failure(result, "within 90 seconds")

    def preflight_body(self, free_kib):
        self.runner.mkdir()
        return f"""
command() {{ if [[ "$1" == -v ]]; then return 0; else builtin command "$@"; fi; }}
aws() {{ printf 'aws-cli/2.99.1 offline\\n'; }}
runner_command() {{ [[ "$*" == 'timeout 15 skopeo --version' ]]; }}
df() {{ printf 'Filesystem 1024-blocks Used Available Capacity Mounted\\nfixture 30000000 0 {free_kib} 0%% /\\n'; }}
preflight
"""

    def test_transfer_workspace_needs_five_gib_free(self):
        self.assert_failure(self.run_shell(self.preflight_body(5000000)), "5 GiB free disk")

    def test_preflight_accepts_storage_without_docker_or_emulation(self):
        self.binfmt.unlink()
        result = self.run_shell(self.preflight_body(6000000))
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_transfer_only_preparation_has_no_build_toolchain(self):
        script = SCRIPT.read_text()
        self.assertNotIn('prepare_docker', script)
        self.assertNotIn('usermod -aG docker', script)
        self.assertNotIn('qemu', script)
        packages = next(line for line in script.splitlines() if line.strip().startswith('apt_install ca-certificates'))
        self.assertIn('skopeo', packages)
        for package in ('maven', 'qemu-user-static', 'binfmt-support', 'docker-ce', 'nodejs', 'golang'):
            self.assertNotIn(package, packages)


if __name__ == "__main__":
    unittest.main()
