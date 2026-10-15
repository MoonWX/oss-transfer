#!/usr/bin/env python3
"""Guard external-fixture isolation, authentication failures and owned-PID cleanup."""

import importlib.util
import json
import os
from pathlib import Path
import tempfile
import types
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / ".github/acceptance" / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


CLUSTER = load("external_s3_cluster", "cluster.py")
HOST = load("host_s3_test", "host-s3.py")


class HostS3Tests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.path = self.directory / "config.json"
        self.config = {"format": 1, "endpoint": "http://127.0.0.1:54321", "accessKey": "private-access",
                       "secretKey": "private-secret", "provenance": {
                           "mode": "host-native", "release": "4.48", "platform": "linux/amd64", "sha256": "1" * 64,
                           "archive": "linux_amd64.tar.gz", "archiveSHA256": HOST.PREPARE.WEED["linux_amd64.tar.gz"],
                           "version": "SeaweedFS 4.48", "secretKey": "must-not-enter-evidence"}}
        HOST.private_json(self.path, self.config)
        self.environment = mock.patch.dict(os.environ, OSS_TRANSFER_QA_S3_CONFIG=str(self.path))
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def test_config_is_private_and_public_provenance_is_allowlisted(self):
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)
        actual = CLUSTER.external_s3_fixture()
        self.assertNotIn("secretKey", actual["provenance"])
        self.assertNotIn("must-not-enter-evidence", json.dumps(actual["provenance"]))
        self.assertEqual(actual["secretKey"], self.config["secretKey"])

    def test_invalid_or_public_config_fails_instead_of_selecting_local_fixture(self):
        self.path.chmod(0o644)
        with self.assertRaisesRegex(AssertionError, "must be private"):
            CLUSTER.external_s3_fixture()
        self.path.chmod(0o600)
        for change in ({"format": 2}, {"accessKey": ""}, {"endpoint": "http://key:secret@localhost:9000"}):
            HOST.private_json(self.path, dict(self.config, **change))
            with self.assertRaises(AssertionError):
                CLUSTER.external_s3_fixture()

    def harness(self):
        result = object.__new__(CLUSTER.Harness)
        result.external_s3 = CLUSTER.external_s3_fixture()
        result.spawn = mock.Mock(side_effect=AssertionError("must not spawn a local S3 fallback"))
        return result

    def test_authentication_failure_is_fatal_and_sanitized(self):
        h = self.harness()
        s3 = mock.Mock()
        s3.list_buckets.side_effect = ValueError("private-secret")
        with mock.patch.object(CLUSTER.boto3, "client", return_value=s3), \
                self.assertRaisesRegex(RuntimeError, "authentication/connectivity") as raised:
            h.start_s3()
        self.assertNotIn("private-secret", str(raised.exception))
        h.spawn.assert_not_called()
        s3.create_bucket.assert_not_called()

    def test_shared_host_fixture_keeps_independent_bucket_names(self):
        first, second = self.harness(), self.harness()
        with mock.patch.object(CLUSTER.boto3, "client", return_value=mock.Mock()):
            first.start_s3()
            second.start_s3()
        self.assertTrue(set(first.buckets.values()).isdisjoint(second.buckets.values()))
        self.assertEqual(first.storage["endpoint"], self.config["endpoint"])
        self.assertEqual(second.storage["secretKey"], self.config["secretKey"])

    def test_stop_refuses_reused_pid_and_scrubs_private_log(self):
        state = {"pid": 123, "processIdentity": "fixture process", "accessKey": "private-access",
                 "secretKey": "private-secret"}
        HOST.private_json(self.directory / "process.json", state)
        log = self.directory / "seaweedfs.log"
        log.write_text("private-access private-secret")
        with mock.patch.object(HOST, "identity", return_value="another process"), \
                mock.patch.object(HOST.os, "killpg") as kill, \
                self.assertRaisesRegex(RuntimeError, "different process"):
            HOST.stop(self.directory)
        kill.assert_not_called()
        self.assertEqual(log.read_text(), "[redacted] [redacted]")

    def test_process_identity_treats_zombie_as_exited(self):
        with mock.patch.object(HOST.subprocess, "run", return_value=types.SimpleNamespace(
                returncode=0, stdout="Z Thu Oct 15 19:00:00 2026 <defunct>\n")):
            self.assertEqual(HOST.identity(123), "")

    def test_stop_waits_through_a_transient_exit_marker_without_signalling_it(self):
        state = {"pid": 123, "processIdentity": "fixture process", "accessKey": "private-access",
                 "secretKey": "private-secret"}
        HOST.private_json(self.directory / "process.json", state)
        with mock.patch.object(HOST, "identity", side_effect=["fixture process", "<exiting>", "", ""]), \
                mock.patch.object(HOST.os, "killpg") as kill, mock.patch.object(HOST.time, "sleep"):
            HOST.stop(self.directory)
        kill.assert_called_once_with(123, HOST.signal.SIGTERM)
        self.assertFalse((self.directory / "process.json").exists())

    def test_stop_accepts_process_exit_between_ownership_check_and_signal(self):
        state = {"pid": 123, "processIdentity": "fixture process", "accessKey": "private-access",
                 "secretKey": "private-secret"}
        HOST.private_json(self.directory / "process.json", state)
        with mock.patch.object(HOST, "identity", side_effect=["fixture process", ""]), \
                mock.patch.object(HOST.os, "killpg", side_effect=ProcessLookupError):
            HOST.stop(self.directory)
        self.assertFalse((self.directory / "process.json").exists())

    def test_stop_verifies_exit_after_kill_before_removing_private_state(self):
        for exited, kill_race in ((False, False), (True, False), (True, True)):
            with self.subTest(exited=exited, kill_race=kill_race):
                state = {"pid": 123, "processIdentity": "fixture process", "accessKey": "private-access",
                         "secretKey": "private-secret"}
                HOST.private_json(self.directory / "process.json", state)
                HOST.private_json(self.directory / "config.json", self.config)
                with mock.patch.object(HOST, "identity", return_value="fixture process"), \
                        mock.patch.object(HOST, "wait_gone", side_effect=[False, exited]) as wait, \
                        mock.patch.object(HOST.os, "killpg", side_effect=[None, ProcessLookupError] if kill_race else None) as kill:
                    if exited:
                        HOST.stop(self.directory)
                    else:
                        with self.assertRaisesRegex(HOST.FixtureError, "did not exit after SIGKILL"):
                            HOST.stop(self.directory)
                self.assertEqual(kill.call_args_list, [mock.call(123, HOST.signal.SIGTERM), mock.call(123, HOST.signal.SIGKILL)])
                self.assertEqual(wait.call_args_list, [mock.call(123, 15), mock.call(123, 5)])
                self.assertEqual((self.directory / "process.json").exists(), not exited)
                self.assertEqual((self.directory / "config.json").exists(), not exited)


if __name__ == "__main__":
    unittest.main()
