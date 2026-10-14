#!/usr/bin/env python3
"""Resolve immutable artifacts from a successful main CI run for one commit."""

import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


WORKFLOW = "ci.yml"
ARTIFACT_NAMES = {
    "go-release-assets": "package_artifact_id",
    "go-release-image": "image_artifact_id",
}
WAIT_SECONDS = 65 * 60
POLL_SECONDS = 15


class ResolutionError(Exception):
    pass


class GitHubClient:
    def __init__(self, token, *, clock=time.monotonic, sleep=time.sleep,
                 opener=urllib.request.urlopen, log=print):
        self.token = token
        self.clock = clock
        self.sleep = sleep
        self.opener = opener
        self.log = log

    def get(self, path, params=None, *, deadline):
        url = "https://api.github.com" + path
        if params:
            url += "?" + urllib.parse.urlencode(params)
        request = urllib.request.Request(url, headers={
            "Accept": "application/vnd.github+json",
            "Authorization": "Bearer " + self.token,
            "X-GitHub-Api-Version": "2022-11-28",
            "User-Agent": "oss-transfer-release",
        })
        for attempt in range(4):
            remaining = deadline - self.clock()
            if remaining <= 0:
                raise ResolutionError("Timed out waiting for CI artifacts.")
            delay = 2 ** (attempt + 1)
            try:
                with self.opener(request, timeout=min(20, remaining)) as response:
                    payload = json.load(response)
                if not isinstance(payload, dict):
                    raise ResolutionError("GitHub API returned an invalid response.")
                return payload
            except urllib.error.HTTPError as error:
                rate_limited = error.code == 403 and (
                    error.headers.get("X-RateLimit-Remaining") == "0"
                    or error.headers.get("Retry-After") is not None
                )
                if error.code not in (429, 500, 502, 503, 504) and not rate_limited:
                    raise ResolutionError(
                        "GitHub API returned HTTP {}. Check Actions read access."
                        .format(error.code)
                    ) from None
                retry_after = error.headers.get("Retry-After", "")
                if retry_after.isdigit():
                    delay = min(30, max(delay, int(retry_after)))
                description = "HTTP {}".format(error.code)
            except (urllib.error.URLError, TimeoutError, OSError):
                description = "a connection error"
            except (ValueError, UnicodeError):
                raise ResolutionError("GitHub API returned invalid JSON.") from None
            if attempt == 3:
                raise ResolutionError(
                    "GitHub API remained unavailable after 4 attempts ({})."
                    .format(description)
                )
            self.log("GitHub API returned {}; retrying ({}/3).".format(
                description, attempt + 1))
            self.sleep(min(delay, max(0, deadline - self.clock())))


class ArtifactResolver:
    def __init__(self, client, repository, commit, *, wait_seconds=WAIT_SECONDS,
                 poll_seconds=POLL_SECONDS, clock=time.monotonic,
                 sleep=time.sleep, log=print):
        self.workflow, self.artifact_names = WORKFLOW, ARTIFACT_NAMES
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ResolutionError("GITHUB_REPOSITORY must be owner/repository.")
        if any(part in (".", "..") for part in repository.split("/")):
            raise ResolutionError("GITHUB_REPOSITORY must be owner/repository.")
        if not re.fullmatch(r"[0-9a-f]{40}", commit):
            raise ResolutionError("GITHUB_SHA must be the full release commit SHA.")
        self.client = client
        self.repository = repository
        self.commit = commit
        self.prefix = "/repos/{}/actions".format(repository)
        self.clock = clock
        self.sleep = sleep
        self.log = log
        self.deadline = clock() + wait_seconds
        self.poll_seconds = poll_seconds
        self.last_state = None

    def get(self, path, params=None):
        return self.client.get(path, params, deadline=self.deadline)

    def trusted(self, run):
        head_repository = run.get("head_repository") or {}
        if not isinstance(head_repository, dict):
            return False
        repository_name = head_repository.get("full_name", "")
        return (
            run.get("event") == "push"
            and run.get("head_branch") == "main"
            and run.get("head_sha") == self.commit
            and run.get("path") == ".github/workflows/" + self.workflow
            and isinstance(repository_name, str)
            and repository_name.casefold() == self.repository.casefold()
            and type(run.get("id")) is int and run["id"] > 0
        )

    def discover(self):
        candidates = []
        # GitHub limits filtered workflow-run searches to 1,000 results.
        for page in range(1, 11):
            payload = self.get(self.prefix + "/workflows/" + self.workflow + "/runs", {
                "event": "push", "branch": "main", "head_sha": self.commit,
                "exclude_pull_requests": "true", "per_page": 100, "page": page,
            })
            runs = payload.get("workflow_runs")
            if not isinstance(runs, list):
                raise ResolutionError("GitHub API did not return workflow runs.")
            candidates.extend(run for run in runs
                              if isinstance(run, dict) and self.trusted(run))
            if len(runs) < 100:
                break
        return max(candidates, key=lambda run: run["id"]) if candidates else None

    def run_url(self, run_id):
        return "https://github.com/{}/actions/runs/{}".format(
            self.repository, run_id)

    def report(self, state, message):
        if state != self.last_state:
            self.log(message)
            self.last_state = state

    def wait(self):
        remaining = self.deadline - self.clock()
        if remaining <= 0:
            raise ResolutionError(
                "Timed out waiting for successful main CI for {}. "
                "Run CI for this commit, then rerun Release.".format(self.commit))
        self.sleep(min(self.poll_seconds, remaining))

    def validate_run(self, run, run_id):
        if not self.trusted(run) or run["id"] != run_id:
            raise ResolutionError("CI run identity changed or is not trusted.")
        status = run.get("status")
        if status == "completed":
            if run.get("conclusion") != "success":
                raise ResolutionError(
                    "CI finished with {}: {}. Fix or rerun this CI run, "
                    "then rerun Release.".format(
                        run.get("conclusion", "no conclusion"), self.run_url(run_id)))
            return True
        if status not in ("queued", "in_progress", "requested", "waiting", "pending"):
            raise ResolutionError("CI returned an unexpected status: {}.".format(status))
        return False

    def artifacts(self, run_id):
        found = {}
        for page in range(1, 101):
            payload = self.get(self.prefix + "/runs/{}/artifacts".format(run_id),
                               {"per_page": 100, "page": page})
            artifacts = payload.get("artifacts")
            if not isinstance(artifacts, list):
                raise ResolutionError("GitHub API did not return CI artifacts.")
            for artifact in artifacts:
                name = artifact.get("name") if isinstance(artifact, dict) else None
                if name not in self.artifact_names:
                    continue
                if name in found:
                    raise ResolutionError("CI has duplicate artifacts named {}.".format(name))
                workflow_run = artifact.get("workflow_run")
                if workflow_run is not None and (
                    not isinstance(workflow_run, dict)
                    or workflow_run.get("id") != run_id
                    or workflow_run.get("head_sha", self.commit) != self.commit
                    or workflow_run.get("head_branch", "main") != "main"
                ):
                    raise ResolutionError("CI artifact belongs to a different run or commit.")
                if (artifact.get("expired") is not False
                        or type(artifact.get("size_in_bytes")) is not int
                        or artifact["size_in_bytes"] <= 0
                        or type(artifact.get("id")) is not int or artifact["id"] <= 0):
                    raise ResolutionError(
                        "CI artifact {} is expired, empty or invalid. Rerun CI at {}, "
                        "then rerun Release.".format(name, self.run_url(run_id)))
                found[name] = artifact["id"]
            if len(artifacts) < 100:
                break
        else:
            raise ResolutionError("Too many CI artifacts to resolve safely.")
        missing = sorted(set(self.artifact_names) - set(found))
        if missing:
            raise ResolutionError(
                "CI artifacts are missing: {}. Rerun CI at {}, then rerun Release."
                .format(", ".join(missing), self.run_url(run_id)))
        return {self.artifact_names[name]: artifact_id for name, artifact_id in found.items()}

    def resolve(self):
        run_id = None
        while self.clock() < self.deadline:
            if run_id is None:
                run = self.discover()
                if run is None:
                    self.report("not-created", "Waiting for main CI for {} to start."
                                .format(self.commit))
                    self.wait()
                    continue
                run_id = run["id"]
            run = self.get(self.prefix + "/runs/{}".format(run_id))
            if self.validate_run(run, run_id):
                artifacts = self.artifacts(run_id)
                # A rerun can start while we resolve names. Wait for it before
                # publishing; afterwards consumers use the immutable artifact IDs.
                current = self.get(self.prefix + "/runs/{}".format(run_id))
                if (self.validate_run(current, run_id)
                        and current.get("run_attempt") == run.get("run_attempt")):
                    self.log("Using successful main CI: {}".format(self.run_url(run_id)))
                    return {"ci_run_id": run_id, **artifacts}
                run = current
            self.report((run_id, run.get("status"), run.get("run_attempt")),
                        "Waiting for CI ({}): {}".format(
                            run.get("status"), self.run_url(run_id)))
            self.wait()
        self.wait()


def main(environ=None):
    environ = os.environ if environ is None else environ
    try:
        token = environ.get("GH_TOKEN", "")
        output_path = environ.get("GITHUB_OUTPUT", "")
        if not token or not output_path:
            raise ResolutionError("GH_TOKEN and GITHUB_OUTPUT are required.")
        resolver = ArtifactResolver(GitHubClient(token),
                                    environ.get("GITHUB_REPOSITORY", ""),
                                    environ.get("GITHUB_SHA", ""))
        result = resolver.resolve()
        with open(output_path, "a", encoding="utf-8") as output:
            for name, value in result.items():
                output.write("{}={}\n".format(name, value))
        return 0
    except (ResolutionError, OSError) as error:
        print("CI artifact resolution failed: {}".format(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
