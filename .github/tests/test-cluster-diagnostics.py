#!/usr/bin/env python3
"""Offline checks that failed acceptance retains bounded, sanitized evidence."""

import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("cluster_diagnostics", ROOT / ".github/acceptance/cluster.py")
CLUSTER = importlib.util.module_from_spec(SPEC)
# These tests never use S3. Keep the diagnostic failure paths runnable without
# installing the acceptance client's dependencies or starting any service.
with mock.patch.dict(sys.modules, {"boto3": types.ModuleType("boto3"),
                                  "botocore": types.ModuleType("botocore"),
                                  "botocore.config": types.SimpleNamespace(Config=object)}):
    SPEC.loader.exec_module(CLUSTER)


class DiagnosticsTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.harness = object.__new__(CLUSTER.Harness)
        self.harness.root = Path(self.temporary.name)
        self.harness.evidence = {}
        self.harness.secrets = ["known-secret"]

    def test_startup_credentials_redacted_before_the_harness_can_read_them(self):
        output = self.harness.scrub(
            "Initial administrator: admin\nInitial password: unseen_random-password\n"
            "Open the console to complete setup. Setup token: unseen_legacy-token\n")
        self.assertIn("Initial administrator: admin", output)
        self.assertIn("Initial password: [redacted]", output)
        self.assertIn("Setup token: [redacted]", output)
        self.assertNotIn("unseen_", output)

    def test_timeout_retains_last_poll_and_only_bounded_safe_fields(self):
        failure = AssertionError("original task completion timeout")
        last = {"taskIndex": 4, "status": "RUNNING", "putObjectCount": 12,
                "executionNodeIp": "node-known-secret", "errorCode": "x" * 400,
                "accessKey": "unknown-access", "secretKey": "unknown-secret",
                "sourceEndpoint": {"password": "nested-secret"},
                "errorMessage": "unexpected credential in upstream error"}
        self.harness.task = mock.Mock(side_effect=[dict(last, putObjectCount=1), last])
        self.harness.api = mock.Mock(return_value={"items": [dict(last, childTaskId=f"4-{i}") for i in range(30)],
                                                   "totalCount": 30})

        def timeout(check, description, seconds):
            self.assertIsNone(check())
            self.assertIsNone(check())
            raise failure

        with mock.patch.object(CLUSTER, "eventually", side_effect=timeout), self.assertRaises(AssertionError) as raised:
            self.harness.wait_task(4, timeout=120)
        self.assertIs(raised.exception, failure)
        self.harness.api.assert_called_once_with("/api/master/tasks/4/children?offset=0&limit=20",
                                                 timeout=3, max_bytes=256 * 1024)
        encoded = (self.harness.root / "evidence.json").read_text()
        diagnostic = json.loads(encoded)["taskWaitFailure"]
        self.assertEqual(diagnostic["task"]["putObjectCount"], 12)
        self.assertEqual(diagnostic["timeoutSeconds"], 120)
        self.assertEqual(diagnostic["task"]["executionNodeIp"], "node-[redacted]")
        self.assertEqual(len(diagnostic["task"]["errorCode"]), 256)
        self.assertEqual(len(diagnostic["children"]), 20)
        self.assertTrue(diagnostic["childrenTruncated"])
        for excluded in ("known-secret", "unknown-access", "unknown-secret", "nested-secret", "upstream error"):
            self.assertNotIn(excluded, encoded)

    def test_diagnostic_api_or_disk_failure_does_not_replace_original_error(self):
        for disk_failure in (False, True):
            with self.subTest(disk_failure=disk_failure):
                failure = OSError("original polling failure")
                self.harness.task = mock.Mock(side_effect=failure)
                self.harness.api = mock.Mock(side_effect=TimeoutError("known-secret"))
                save = CLUSTER.Harness.save.__get__(self.harness)
                self.harness.save = mock.Mock(side_effect=OSError("disk unavailable")) if disk_failure else save
                with self.assertRaises(OSError) as raised:
                    self.harness.wait_task(4)
                self.assertIs(raised.exception, failure)
                diagnostic = self.harness.evidence["taskWaitFailure"]
                self.assertIsNone(diagnostic["task"])
                self.assertEqual(diagnostic["childrenErrorType"], "TimeoutError")
                self.assertNotIn("known-secret", json.dumps(diagnostic))

    def sampler(self):
        sampler = mock.MagicMock()
        sampler.samples = [[1.0, 0, 0], [2.0, 128, 64]]
        sampler.__enter__.return_value = sampler
        sampler.__exit__.return_value = False
        self.harness.buckets = {"rate": "fixture-bucket"}
        self.harness.download_body = mock.Mock(return_value={})
        self.harness.create = mock.Mock(side_effect=[4, 5])
        self.harness.task_report = mock.Mock()
        return sampler

    def test_bandwidth_timeout_still_writes_physical_samples(self):
        sampler = self.sampler()
        failure = AssertionError("original bandwidth wait timeout")
        self.harness.wait_task = mock.Mock(side_effect=failure)
        with mock.patch.object(CLUSTER, "GrowthSampler", return_value=sampler), self.assertRaises(AssertionError) as raised:
            self.harness.bandwidth({})
        self.assertIs(raised.exception, failure)
        self.harness.wait_task.assert_called_once_with(4, timeout=120)
        self.assertEqual(json.loads((self.harness.root / "bandwidth-samples.json").read_text()), sampler.samples)
        sampler.evaluate.assert_not_called()

    def test_sample_write_failure_preserves_existing_failure(self):
        sampler = self.sampler()
        failure = AssertionError("original bandwidth wait timeout")
        self.harness.wait_task = mock.Mock(side_effect=failure)
        with mock.patch.object(CLUSTER, "GrowthSampler", return_value=sampler), \
                mock.patch.object(Path, "write_text", side_effect=OSError("disk unavailable")), \
                self.assertRaises(AssertionError) as raised:
            self.harness.bandwidth({})
        self.assertIs(raised.exception, failure)

    def test_successful_wait_does_not_collect_failure_diagnostics(self):
        completed = {"taskIndex": 4, "status": "COMPLETED"}
        self.harness.task = mock.Mock(return_value=completed)
        self.harness.api = mock.Mock()
        self.assertEqual(self.harness.wait_task(4), completed)
        self.harness.api.assert_not_called()
        self.assertEqual(self.harness.evidence, {})

    def test_two_mbps_sampling_rejects_independent_two_mbps_node_budgets(self):
        total, rate = 8 * CLUSTER.MIB, 250_000
        sampler = CLUSTER.GrowthSampler([])
        normal = [[index / 5, min(total, index * rate // 5), min(total, index * rate // 5)]
                  for index in range(180)]
        sampler.samples = normal
        result = sampler.evaluate(rate, [total, total])
        self.assertTrue(all(len(task["windows"]) >= 3 for task in result))
        # Two Agents independently spending the full 2 Mbps parent budget
        # produce 2.5 MB in five seconds. Also reject 30% overspeed so a slow
        # emulated CPU cannot hide a missing limiter inside boundary allowance.
        for factor in (1.3, 2):
            with self.subTest(factor=factor):
                sampler.samples = [[stamp, min(total, int(first * factor)), min(total, int(second * factor))]
                                   for stamp, first, second in normal]
                with self.assertRaisesRegex(AssertionError, "exceeded its cluster bandwidth"):
                    sampler.evaluate(rate, [total, total])


if __name__ == "__main__":
    unittest.main()
