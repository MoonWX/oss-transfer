#!/usr/bin/env python3
"""A successful Docker check must also leave no owned runtime resources."""
import importlib.util
import json
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("docker_acceptance", Path(__file__).resolve().parents[1] / "acceptance/docker.py")
DOCKER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DOCKER)


class DockerCleanupTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.acceptance = DOCKER.Acceptance.__new__(DOCKER.Acceptance)
        self.acceptance.root = Path(self.temporary.name)
        self.acceptance.prefix = "oss-qa-owned"
        self.acceptance.containers = ["oss-qa-owned-master", "oss-qa-owned-agent"]
        self.acceptance.volumes = ["oss-qa-owned-data"]
        self.acceptance.network = "oss-qa-owned-network"
        self.acceptance.secrets = ["private-test-token"]
        self.acceptance.evidence = {"checks": ["transfer passed"]}
        self.resources = {
            "containers": set(self.acceptance.containers) | {"unrelated-container"},
            "volumes": set(self.acceptance.volumes) | {"unrelated-volume"},
            "networks": {self.acceptance.network, "unrelated-network"},
        }
        self.commands = []
        self.retained = None
        self.failed_removal = None
        self.failed_query = None
        self.acceptance.docker = self.docker
        logger = patch.object(DOCKER.subprocess, "run", return_value=SimpleNamespace(
            stdout="private-test-token\n", stderr=""))
        logger.start()
        self.addCleanup(logger.stop)

    def docker(self, *command, check=True):
        self.assertTrue(check, "Cleanup must not suppress Docker command failures")
        self.commands.append(command)
        if command[0] == "rm":
            kind, name = "containers", command[-1]
        elif command[:2] in (("volume", "rm"), ("network", "rm")):
            kind, name = command[0] + "s", command[-1]
        else:
            kind = "containers" if command[0] == "ps" else command[0] + "s"
            if self.failed_query == kind:
                raise RuntimeError("Docker daemon query failed")
            return "\n".join(sorted(self.resources[kind]))
        if name == self.failed_removal:
            raise RuntimeError("Docker removal rejected private-test-token")
        if name != self.retained:
            self.resources[kind].remove(name)
        return name

    def proof(self):
        return json.loads((self.acceptance.root / "evidence.json").read_text())

    def test_removes_and_verifies_only_owned_resources(self):
        self.acceptance.close()
        self.assertTrue(self.proof()["testResourcesRemoved"])
        self.assertEqual(self.resources, {
            "containers": {"unrelated-container"}, "volumes": {"unrelated-volume"},
            "networks": {"unrelated-network"},
        })
        self.assertEqual((self.acceptance.root / "master.log").read_text(), "[redacted]\n")

    def test_successful_removal_command_does_not_hide_surviving_container(self):
        self.retained = self.acceptance.containers[0]
        with self.assertRaisesRegex(AssertionError, "Test containers remain"):
            self.acceptance.close()
        self.assertFalse(self.proof()["testResourcesRemoved"])
        self.assertIn(("network", "rm", self.acceptance.network), self.commands)

    def test_unavailable_daemon_cannot_be_mistaken_for_empty_resource_list(self):
        self.failed_query = "containers"
        with self.assertRaisesRegex(AssertionError, "Docker daemon query failed"):
            self.acceptance.close()
        self.assertFalse(self.proof()["testResourcesRemoved"])

    def test_removal_failure_keeps_original_transfer_error_and_attempts_all_cleanup(self):
        self.failed_removal = self.acceptance.containers[1]
        self.acceptance.evidence["failure"] = "original transfer failure"
        with self.assertRaisesRegex(AssertionError, "Docker removal rejected") as raised:
            try:
                raise RuntimeError("original transfer failure")
            finally:
                self.acceptance.close()
        self.assertEqual(str(raised.exception.__context__), "original transfer failure")
        proof = self.proof()
        self.assertEqual(proof["failure"], "original transfer failure")
        self.assertFalse(proof["testResourcesRemoved"])
        self.assertNotIn("private-test-token", json.dumps(proof))
        self.assertIn(("rm", "-f", self.acceptance.containers[0]), self.commands)
        self.assertIn(("volume", "rm", self.acceptance.volumes[0]), self.commands)
        self.assertIn(("network", "rm", self.acceptance.network), self.commands)


if __name__ == "__main__":
    unittest.main()
