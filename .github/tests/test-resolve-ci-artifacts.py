#!/usr/bin/env python3
"""Offline regressions for release-to-CI artifact selection."""

import copy
import importlib.util
import io
from pathlib import Path
import tempfile
import unittest
from unittest import mock
import urllib.error


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "resolve-ci-artifacts.py"
SPEC = importlib.util.spec_from_file_location("resolve_ci_artifacts", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
SHA = "a" * 40
REPO = "MoonWX/oss-transfer"
RUN = {
    "id": 123, "event": "push", "head_branch": "main", "head_sha": SHA,
    "path": ".github/workflows/ci.yml", "head_repository": {"full_name": REPO},
    "status": "completed", "conclusion": "success", "run_attempt": 1,
}


def artifact(name, artifact_id):
    return {"name": name, "id": artifact_id, "size_in_bytes": 1234,
            "expired": False, "workflow_run": {
                "id": 123, "head_sha": SHA, "head_branch": "main"}}


ARTIFACTS = [artifact("release-package", 501), artifact("release-image", 502)]


class Clock:
    def __init__(self):
        self.now = 0
        self.sleeps = []

    def __call__(self):
        return self.now

    def sleep(self, delay):
        self.sleeps.append(delay)
        self.now += delay


class FakeAPI:
    def __init__(self, *, listings=None, runs=None, artifacts=None):
        self.listings = copy.deepcopy(listings if listings is not None else [[RUN]])
        self.runs = copy.deepcopy(runs if runs is not None else [RUN])
        self.artifact_list = copy.deepcopy(ARTIFACTS if artifacts is None else artifacts)
        self.calls = []

    @staticmethod
    def consume(items):
        return copy.deepcopy(items.pop(0) if len(items) > 1 else items[0])

    def get(self, path, params=None, *, deadline):
        self.calls.append((path, params, deadline))
        if path.endswith("/workflows/ci.yml/runs"):
            return {"workflow_runs": self.consume(self.listings)}
        if path.endswith("/artifacts"):
            return {"artifacts": copy.deepcopy(self.artifact_list)}
        if path.endswith("/runs/123"):
            return self.consume(self.runs)
        raise AssertionError("Unexpected API request: " + path)


class ResolveTests(unittest.TestCase):
    def resolve(self, api=None, **kwargs):
        self.clock = Clock()
        self.logs = []
        self.api = api or FakeAPI()
        self.resolver = MODULE.ArtifactResolver(
            self.api, REPO, SHA, clock=self.clock, sleep=self.clock.sleep,
            log=self.logs.append, wait_seconds=kwargs.pop("wait_seconds", 45),
            poll_seconds=15, **kwargs)
        return self.resolver.resolve()

    def test_success_pins_artifact_ids(self):
        self.assertEqual(self.resolve(), {
            "ci_run_id": 123, "package_artifact_id": 501, "image_artifact_id": 502})
        params = self.api.calls[0][1]
        self.assertEqual(params["head_sha"], SHA)
        self.assertEqual(params["event"], "push")
        self.assertEqual(params["branch"], "main")
        self.assertEqual(self.clock.sleeps, [])

    def test_main_and_tag_race_waits_for_creation_and_completion(self):
        queued = dict(RUN, status="queued", conclusion=None)
        running = dict(RUN, status="in_progress", conclusion=None)
        self.resolve(FakeAPI(listings=[[], [queued]], runs=[queued, running, RUN]),
                     wait_seconds=60)
        self.assertEqual(self.clock.sleeps, [15, 15, 15])
        self.assertEqual(len(self.logs), 4)

    def test_unchanged_status_is_not_repeated(self):
        running = dict(RUN, status="in_progress", conclusion=None)
        with self.assertRaisesRegex(MODULE.ResolutionError, "Timed out"):
            self.resolve(FakeAPI(runs=[running]))
        self.assertEqual(len(self.logs), 1)

    def test_failed_or_cancelled_ci_is_not_publishable(self):
        for conclusion in ("failure", "cancelled", "timed_out", "skipped", "neutral", None):
            with self.subTest(conclusion=conclusion):
                with self.assertRaisesRegex(MODULE.ResolutionError, "CI finished"):
                    self.resolve(FakeAPI(runs=[dict(RUN, conclusion=conclusion)]))
                self.assertFalse(any(path.endswith("/artifacts")
                                     for path, _, _ in self.api.calls))

    def test_untrusted_candidates_never_publish(self):
        cases = [
            {"event": "pull_request"}, {"head_branch": "feature"},
            {"head_repository": {"full_name": "someone/fork"}},
            {"head_repository": None}, {"path": ".github/workflows/release.yml"},
            {"head_sha": "b" * 40}, {"id": "123"},
        ]
        for change in cases:
            with self.subTest(change=change):
                with self.assertRaisesRegex(MODULE.ResolutionError, "Timed out"):
                    self.resolve(FakeAPI(listings=[[dict(RUN, **change)]]))
                self.assertFalse(any(path.endswith("/artifacts")
                                     for path, _, _ in self.api.calls))

    def test_trusted_candidate_is_chosen_over_untrusted_newer_run(self):
        untrusted = dict(RUN, id=999, event="pull_request")
        self.assertEqual(self.resolve(FakeAPI(listings=[[untrusted, RUN]]))["ci_run_id"], 123)

    def test_latest_trusted_run_failure_does_not_fall_back_to_older_success(self):
        older = dict(RUN, id=122)
        failed = dict(RUN, conclusion="failure")
        with self.assertRaisesRegex(MODULE.ResolutionError, "CI finished with failure"):
            self.resolve(FakeAPI(listings=[[older, failed]], runs=[failed]))

    def test_workflow_run_pagination(self):
        untrusted = [dict(RUN, id=value + 1000, event="pull_request") for value in range(100)]
        self.resolve(FakeAPI(listings=[untrusted, [RUN]]))
        pages = [params["page"] for path, params, _ in self.api.calls
                 if path.endswith("/workflows/ci.yml/runs")]
        self.assertEqual(pages, [1, 2])

    def test_run_identity_is_revalidated(self):
        with self.assertRaisesRegex(MODULE.ResolutionError, "not trusted"):
            self.resolve(FakeAPI(runs=[dict(RUN, head_branch="feature")]))

    def test_missing_artifacts_fail_without_fallback(self):
        with self.assertRaisesRegex(MODULE.ResolutionError, "missing: release-image"):
            self.resolve(FakeAPI(artifacts=ARTIFACTS[:1]))

    def test_expired_empty_invalid_artifacts_fail(self):
        for change in ({"expired": True}, {"size_in_bytes": 0}, {"id": "501"}):
            with self.subTest(change=change):
                artifacts = copy.deepcopy(ARTIFACTS)
                artifacts[0].update(change)
                with self.assertRaisesRegex(MODULE.ResolutionError, "expired, empty or invalid"):
                    self.resolve(FakeAPI(artifacts=artifacts))

    def test_artifact_cannot_claim_another_run_or_commit(self):
        for change in ({"id": 789}, {"head_sha": "b" * 40}, {"head_branch": "other"}):
            with self.subTest(change=change):
                artifacts = copy.deepcopy(ARTIFACTS)
                artifacts[0]["workflow_run"].update(change)
                with self.assertRaisesRegex(MODULE.ResolutionError, "different run or commit"):
                    self.resolve(FakeAPI(artifacts=artifacts))

    def test_duplicate_artifact_names_fail(self):
        with self.assertRaisesRegex(MODULE.ResolutionError, "duplicate"):
            self.resolve(FakeAPI(artifacts=ARTIFACTS + [ARTIFACTS[0]]))

    def test_partial_rerun_accepts_same_run_artifacts_from_earlier_attempt(self):
        artifacts = copy.deepcopy(ARTIFACTS)
        artifacts[0]["created_at"] = "2026-10-09T01:00:00Z"
        artifacts[1]["created_at"] = "2026-10-09T02:00:00Z"
        self.assertEqual(self.resolve(FakeAPI(
            runs=[dict(RUN, run_attempt=2)], artifacts=artifacts))["package_artifact_id"], 501)

    def test_rerun_starting_during_resolution_is_awaited(self):
        running = dict(RUN, status="in_progress", conclusion=None, run_attempt=2)
        completed = dict(RUN, run_attempt=2)
        self.resolve(FakeAPI(runs=[RUN, running, completed, completed]))
        self.assertEqual(self.clock.sleeps, [15])

    def test_same_sha_has_no_ci_timeout(self):
        with self.assertRaisesRegex(MODULE.ResolutionError, "Run CI for this commit"):
            self.resolve(FakeAPI(listings=[[]]))
        self.assertEqual(self.clock(), 45)
        self.assertEqual(len(self.logs), 1)

    def test_invalid_environment_is_rejected(self):
        for repo, sha in (("../repo", SHA), ("MoonWX/oss-transfer/extra", SHA),
                          (REPO, "abc"), (REPO, "a" * 39 + "\n")):
            with self.subTest(repo=repo, sha=sha):
                with self.assertRaises(MODULE.ResolutionError):
                    MODULE.ArtifactResolver(FakeAPI(), repo, sha)

    def test_outputs_are_written_only_after_success(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "output"
            with mock.patch.object(MODULE.ArtifactResolver, "resolve", return_value={
                "ci_run_id": 123, "package_artifact_id": 501, "image_artifact_id": 502,
            }):
                self.assertEqual(MODULE.main({"GH_TOKEN": "test-secret",
                    "GITHUB_REPOSITORY": REPO, "GITHUB_SHA": SHA,
                    "GITHUB_OUTPUT": str(output)}), 0)
            self.assertEqual(output.read_text(),
                "ci_run_id=123\npackage_artifact_id=501\nimage_artifact_id=502\n")

    def test_failed_resolution_does_not_write_outputs_or_log_token(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "output"
            stderr = io.StringIO()
            with mock.patch.object(MODULE.ArtifactResolver, "resolve",
                                   side_effect=MODULE.ResolutionError("CI failed")), \
                    mock.patch.object(MODULE.sys, "stderr", stderr):
                self.assertEqual(MODULE.main({"GH_TOKEN": "test-secret",
                    "GITHUB_REPOSITORY": REPO, "GITHUB_SHA": SHA,
                    "GITHUB_OUTPUT": str(output)}), 1)
            self.assertFalse(output.exists())
            self.assertIn("CI failed", stderr.getvalue())
            self.assertNotIn("test-secret", stderr.getvalue())


class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.logs = []

    def client(self, opener):
        return MODULE.GitHubClient("not-for-logs", clock=self.clock,
                                  sleep=self.clock.sleep, opener=opener, log=self.logs.append)

    def error(self, code, headers=None):
        return urllib.error.HTTPError("https://api.github.com/example", code,
                                      "test error", headers or {}, None)

    def test_transient_failure_recovers_with_bounded_request_timeout(self):
        opener = mock.Mock(side_effect=[self.error(503), io.BytesIO(b'{"ok": true}')])
        result = self.client(opener).get("/repos/a/b/actions/runs", deadline=100)
        self.assertEqual(result, {"ok": True})
        self.assertEqual(opener.call_count, 2)
        self.assertEqual(opener.call_args.kwargs["timeout"], 20)
        self.assertNotIn("not-for-logs", " ".join(self.logs))

    def test_exhausted_retries_fail(self):
        opener = mock.Mock(side_effect=self.error(503))
        with self.assertRaisesRegex(MODULE.ResolutionError, "after 4 attempts"):
            self.client(opener).get("/repos/a/b/actions/runs", deadline=100)
        self.assertEqual(opener.call_count, 4)

    def test_permission_failure_is_not_retried(self):
        for code in (401, 403, 404):
            opener = mock.Mock(side_effect=self.error(code))
            with self.assertRaisesRegex(MODULE.ResolutionError, "read access"):
                self.client(opener).get("/repos/a/b/actions/runs", deadline=100)
            self.assertEqual(opener.call_count, 1)

    def test_rate_limit_retry_after_is_capped_by_deadline(self):
        opener = mock.Mock(side_effect=self.error(429, {"Retry-After": "9999"}))
        with self.assertRaisesRegex(MODULE.ResolutionError, "Timed out"):
            self.client(opener).get("/repos/a/b/actions/runs", deadline=5)
        self.assertEqual(self.clock.sleeps, [5])
        self.assertEqual(opener.call_count, 1)

    def test_network_timeout_is_retried(self):
        opener = mock.Mock(side_effect=[TimeoutError(), io.BytesIO(b'{}')])
        self.assertEqual(self.client(opener).get("/repos/a/b/actions/runs", deadline=100), {})

    def test_invalid_json_is_rejected(self):
        opener = mock.Mock(return_value=io.BytesIO(b'not JSON'))
        with self.assertRaisesRegex(MODULE.ResolutionError, "invalid JSON"):
            self.client(opener).get("/repos/a/b/actions/runs", deadline=100)


if __name__ == "__main__":
    unittest.main()
