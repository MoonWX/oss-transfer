#!/usr/bin/env python3
"""Check the Linux Compose Agent join: failures clean up and the command can be rerun."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("download_source_tests", Path(__file__).with_name("test-download-source.py"))
DOWNLOAD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DOWNLOAD)

# A Docker CLI stand-in. Compose state lives in files so reruns see earlier runs.
DOCKER = r'''#!/bin/bash
printf '%s\n' "$*" >> "$DOCKER_LOG"
state=$DOCKER_STATE
case "$1" in
  info|pull) exit 0;;
  image) printf '%s\n' "$EXPECTED_COMMIT"; exit 0;;
  inspect) echo running; exit 0;;
  ps) [[ -f "$state/project" ]] && echo existing-container; exit 0;;
  compose)
    [[ "$2" == version ]] && exit 0
    case "$*" in
      *' config --quiet'*) exit 0;;
      *' run --rm '*)
        touch "$state/network"
        case "$ENROLL" in
          ok) [[ "$OSS_TRANSFER_JOIN_TOKEN" == join-token ]] || exit 9; echo enrolled >> "$AGENT_DATA/agent.db"; echo 'Agent enrolled: agent-1'; exit 0;;
          unavailable) echo pending >> "$AGENT_DATA/agent.db"; echo 'enroll Agent: HTTP 503' >&2; exit 1;;
          rejected) echo pending >> "$AGENT_DATA/agent.db"; echo 'enroll Agent: HTTP 403' >&2; exit 1;;
          denied) echo 'open /data/agent.db: permission denied' >&2; exit 1;;
          refused) echo 'enroll Agent: dial tcp 127.0.0.1:1685: connect: connection refused' >&2; exit 1;;
        esac;;
      *' up -d'*) [[ "$UP" == fail ]] && { echo 'port is already allocated' >&2; exit 1; }; touch "$state/project"; exit 0;;
      *' ps -q '*) echo agent-container; exit 0;;
      *' down '*) rm -f "$state/network" "$state/project"; exit 0;;
    esac;;
esac
exit 0
'''


class ComposeJoin(unittest.TestCase):
    def setUp(self):
        self.base = DOWNLOAD.DownloadSourceTests("test_fixed_version_uses_tag_endpoint")
        self.base.setUp()
        self.addCleanup(self.base.doCleanups)
        self.root = self.base.root.resolve()
        self.base.metadata.write_text(json.dumps(self.base.release))
        self.state = self.root / "docker-state"
        self.state.mkdir()
        self.bin = self.root / "bin"
        for name, body in {
            "uname": '#!/bin/sh\ncase "$1" in -s) echo "${FAKE_OS:-Linux}";; -m) echo x86_64;; esac\n',
            "id": '#!/bin/sh\necho "${FAKE_UID:-0}"\n',
            "chown": "#!/bin/sh\nexit 0\n",
            "sudo": '#!/bin/sh\necho sudo >> "$DOCKER_LOG"\nexit 97\n',
            "docker": DOCKER,
        }.items():
            (self.bin / name).write_text(body)
            (self.bin / name).chmod(0o755)
        self.log = self.root / "docker.log"
        self.service = self.root / "service"
        self.data = self.root / "data"

    def join(self, enroll="ok", up="ok", args=(), **env):
        environment = dict(self.base.env, TMPDIR=str(self.root), DOCKER_LOG=str(self.log), DOCKER_STATE=str(self.state),
                           EXPECTED_COMMIT=DOWNLOAD.COMMIT, ENROLL=enroll, UP=up, AGENT_DATA=str(self.data), **env)
        return subprocess.run(
            ["bash", str(ROOT / "install.sh"), "--role", "agent", "--method", "compose", "--source", "github",
             "--version", DOWNLOAD.TAG, "--yes", "--no-deps", "--master-url", "http://10.0.0.1:1685", "--token-stdin",
             "--install-dir", str(self.service), "--data-dir", str(self.data), *args],
            input="join-token\n", env=environment, text=True, capture_output=True,
        )

    def calls(self):
        return self.log.read_text() if self.log.exists() else ""

    def test_failed_enrollment_cleans_up_and_the_same_command_succeeds_again(self):
        result = self.join(enroll="denied")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("permission denied", result.stderr)
        self.assertIn("UID 65532", result.stderr)
        self.assertIn("Removed the containers, network and files created by this attempt", result.stderr)
        self.assertIn(" down --remove-orphans", self.calls())
        self.assertFalse(self.service.exists())
        self.assertFalse(self.data.exists())
        self.assertFalse((self.state / "network").exists())

        result = self.join(enroll="ok")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Agent enrolled: agent-1", result.stdout)
        self.assertNotIn("Existing files", result.stderr)
        # Uninstall reads the project and data directory back from this file.
        compose = (self.service / "docker-compose.yml").read_text()
        self.assertIn("# oss-transfer-project: oss-transfer-agent\n", compose)
        self.assertIn(f"      - '{self.data}:/data'\n", compose)

    def test_unreachable_master_explains_container_localhost(self):
        result = self.join(enroll="refused")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("cannot reach the Master at http://10.0.0.1:1685", result.stderr)
        self.assertFalse(self.service.exists())

    def test_messages_follow_the_console_language(self):
        result = self.join(enroll="denied", OSS_TRANSFER_INSTALL_LANG="zh")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("下载源", result.stdout)
        self.assertIn("已清理本次创建的容器、网络和文件", result.stderr)
        self.assertIn("Agent 加入失败", result.stderr)

    def test_enrolled_agent_keeps_its_identity_when_the_container_cannot_start(self):
        self.data.mkdir()
        result = self.join(up="fail")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("The Agent is enrolled, but its container did not start", result.stderr)
        self.assertNotIn("Removed the containers", result.stderr)
        self.assertTrue((self.service / "docker-compose.yml").is_file())
        self.assertTrue(self.data.is_dir())
        self.assertNotIn(" down ", self.calls())

    def test_leftovers_from_an_unfinished_attempt_are_explained(self):
        self.service.mkdir()
        (self.service / "docker-compose.yml").write_text("services: {}\n")
        result = self.join()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("contains files from an earlier attempt that did not finish", result.stderr)
        self.assertIn(f"rm -rf {self.service} {self.data}", result.stderr)
        self.assertNotIn("upgrad", result.stderr)
        self.assertTrue((self.service / "docker-compose.yml").is_file(), "existing files must not be removed")

        (self.state / "project").write_text("")
        result = self.join()
        self.assertIn("An installation already exists", result.stderr)
        compose = f"docker compose -p oss-transfer-agent -f {self.service}/docker-compose.yml"
        self.assertIn(f"{compose} pull && {compose} up -d", result.stderr)

    def test_unconfirmed_join_keeps_the_identity_and_the_same_command_finishes_it(self):
        result = self.join(enroll="unavailable")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("HTTP 503", result.stderr)
        self.assertIn(f"Its identity is kept in {self.data}", result.stderr)
        self.assertNotIn("Removed the containers", result.stderr)
        self.assertNotIn("rm -rf", result.stderr)
        self.assertTrue((self.data / "agent.db").is_file())
        self.assertTrue((self.service / "docker-compose.yml").is_file())

        result = self.join(enroll="ok")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Existing agent data found", result.stdout)
        self.assertEqual((self.data / "agent.db").read_text(), "pending\nenrolled\n")

    def test_rejected_join_rolls_back(self):
        result = self.join(enroll="rejected")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("invitation is invalid", result.stderr)
        self.assertIn("Removed the containers, network and files created by this attempt", result.stderr)
        self.assertFalse(self.data.exists())
        self.assertFalse(self.service.exists())

    def test_kept_data_is_reused_and_never_offered_for_deletion(self):
        # What --uninstall --keep-data or docker compose down leaves behind.
        self.data.mkdir()
        (self.data / "agent.db").write_text("kept\n")
        result = self.join(enroll="ok")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"Existing agent data found in {self.data}", result.stdout)
        self.assertEqual((self.data / "agent.db").read_text(), "kept\nenrolled\n")

        # Failing later must not remove reused data either.
        (self.state / "project").unlink()
        (self.service / "docker-compose.yml").unlink()
        result = self.join(enroll="rejected")
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue((self.data / "agent.db").is_file())

        # Other files beside reused data are never answered with rm -rf.
        (self.service / "notes.txt").write_text("operator notes")
        result = self.join(enroll="ok")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("contains other files", result.stderr)
        self.assertNotIn("rm -rf", result.stderr)
        self.assertTrue((self.data / "agent.db").is_file())

    def test_data_in_use_or_of_the_other_role_is_not_touched(self):
        self.data.mkdir()
        (self.data / "master.db").write_text("master")
        result = self.join(enroll="ok")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("holds master data, not agent data", result.stderr)
        self.assertNotIn("rm -rf", result.stderr)
        (self.data / "master.db").unlink()
        (self.data / "agent.db").write_text("kept")
        (self.state / "project").write_text("")
        result = self.join(enroll="ok")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("is in use by container(s): existing-container", result.stderr)
        self.assertNotIn("rm -rf", result.stderr)
        self.assertEqual((self.data / "agent.db").read_text(), "kept")

    def test_force_is_ignored_for_compose(self):
        result = self.join(args=("--force",))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("Native configuration/package options", result.stderr)
        self.assertIn("--force applies to native installations only", result.stdout)

    def test_upgrade_names_the_installation_project_and_file(self):
        # The project recorded in the Compose file, not the directory name,
        # must be used: a bare docker compose up -d would start a second
        # container on the same data.
        self.service.mkdir()
        (self.service / "docker-compose.yml").write_text("# oss-transfer-project: edge-agent\nservices: {}\n")
        result = self.join(args=("--upgrade",))
        self.assertNotEqual(result.returncode, 0)
        compose = f"docker compose -p edge-agent -f {self.service}/docker-compose.yml"
        self.assertIn(f"{compose} pull", result.stderr)
        self.assertIn(f"{compose} up -d", result.stderr)

    def test_docker_desktop_is_pointed_to_the_docker_command_before_sudo(self):
        result = self.join(FAKE_OS="Darwin", FAKE_UID="501")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Docker Compose installation needs Linux", result.stderr)
        self.assertIn("Nodes > Add node > Docker", result.stderr)
        self.assertNotIn("sudo", self.calls())


if __name__ == "__main__":
    unittest.main()
