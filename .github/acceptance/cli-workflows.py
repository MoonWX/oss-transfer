#!/usr/bin/env python3
"""Exercise CLI workflows against isolated Master, two Agents and SeaweedFS.

All application operations use the actual CLI executable. boto3 only seeds and
independently verifies fixture objects; it never substitutes for a CLI action.
This is a single-host acceptance run, not JDCloud or multi-host filesystem proof.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import csv
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time
import zipfile

SPEC = importlib.util.spec_from_file_location("cluster_cli_fixture", Path(__file__).with_name("cluster.py"))
cluster = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cluster)
require = cluster.require
MIB = 1024 * 1024


def data(value):
    return value.get("data", value)


def digest(payload):
    return hashlib.sha256(payload).hexdigest()


class Workflows(cluster.Harness):
    def __init__(self, args):
        # Never inherit a real deployment, user session or external S3 fixture.
        require(not os.environ.get("OSS_TRANSFER_QA_S3_CONFIG"), "Unset OSS_TRANSFER_QA_S3_CONFIG; this run owns its S3 fixture")
        args.scenario, args.single_upload, args.fixture = "cli-workflows", False, None
        super().__init__(args)
        self.environment = {key: value for key, value in os.environ.items() if not key.startswith("OSS_TRANSFER_")}
        self.environment.update(OSS_TRANSFER_CLI_CONFIG_DIR=str(self.root / "cli-profile"),
                                OSS_TRANSFER_CONFIG_FILE=str(self.root / "absent-config"),
                                OSS_TRANSFER_LANG="en", QA_PASSWORD=secrets.token_urlsafe(24))
        self.secrets.append(self.environment["QA_PASSWORD"])
        self.nodes = ["cli-workflow-1", "cli-workflow-2"]
        self.invites, self.commands = [], []
        self.expected_exits = set()
        self.evidence["binaries"]["cli"] = cluster.store_qa.binary_provenance(args.cli, "--version")
        self.evidence["limitations"] = [
            "Local SeaweedFS fixture; JDCloud behavior requires the separate cloud run",
            "Two Agents share one host; shared-path checks do not prove cross-host mount consistency",
            "Unmanaged processes exercise update rejection, not a system service upgrade or rollback",
            "No throughput or IOPS performance claim"]

    def cli(self, *arguments, expected=0, input_text=None, timeout=45):
        """No --host: every invocation must use the persisted CLI configuration."""
        require("--host" not in arguments, "Black-box workflows must use configured default Master")
        started = time.monotonic()
        result = subprocess.run([str(self.args.cli.resolve()), "--json", *map(str, arguments)], cwd=self.root,
                                env=self.environment, input=input_text, text=True, encoding="utf-8",
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        allowed = {expected} if isinstance(expected, int) else set(expected)
        self.commands.append({"command": list(map(str, arguments)), "exitCode": result.returncode,
                              "seconds": round(time.monotonic() - started, 3)})
        require(result.returncode in allowed,
                self.scrub(f"CLI {' '.join(map(str, arguments))}: exit {result.returncode}, expected {sorted(allowed)}; {result.stderr}; {result.stdout[:1500] if arguments[:2] != ('agent', 'invite') else '[invitation omitted]'}"))
        try:
            value = json.loads(result.stdout)
        except json.JSONDecodeError:
            raise AssertionError("CLI returned non-JSON for " + " ".join(map(str, arguments[:2]))) from None
        if arguments[:2] != ("agent", "invite"):
            require(not any(secret in result.stdout + result.stderr for secret in self.secrets), "CLI disclosed an input credential")
        return value

    def wait(self, check, description, timeout=90):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            for process in self.children:
                require(process.pid in self.expected_exits or process.poll() is None,
                        "An owned service exited while waiting for " + description)
            result = check()
            if result:
                return result
            time.sleep(0.2)
        raise AssertionError("Timed out: " + description)

    def start_cluster(self):
        self.start_s3()
        self.master, log = self.spawn("master", self.args.master,
                                      ["--listen", "127.0.0.1:0", "--data-dir", str(self.root / "master-data")])
        found = self.wait(lambda: re.search(r"Master listening on (127\.0\.0\.1:\d+)", log.read_text(errors="replace")), "Master listen address")
        self.origin = "http://" + found[1]
        self.check_connection_file(self.root / "master-data", "master")
        initial = self.wait(lambda: re.search(r"Initial password: ([A-Za-z0-9_-]+)", log.read_text(errors="replace")), "initial password")[1]
        self.secrets.append(initial)
        self.environment["QA_INITIAL_PASSWORD"] = initial
        self.cli("config", "set", "master", self.origin)
        configured = self.cli("config", "get", "master")
        require(self.origin in json.dumps(configured), "Saved default Master differs from owned process")
        require(self.cli("setup", "status")["configured"] is True, "Setup status incorrect")
        self.cli("agent", "list", expected=1)
        login = self.cli("login", "--password-stdin", input_text=initial + "\n")
        require(login["user"]["mustChangePassword"] is True, "Initial password did not require replacement")
        self.cli("agent", "list", expected=1)
        changed = self.cli("change-password", "--old-password-env", "QA_INITIAL_PASSWORD", "--new-password-env", "QA_PASSWORD")
        require(changed["user"]["mustChangePassword"] is False, "Password change did not unlock account")
        require(self.cli("whoami")["user"]["username"] == "admin", "CLI session was not persisted")
        for index, node in enumerate(self.nodes):
            invitation = data(self.cli("agent", "invite", "--address", node, "--label", f"CLI Agent {index + 1}"))
            self.secrets.append(invitation["inviteToken"])
            self.invites.append(invitation)
            pending = data(self.cli("agent", "invite-status", invitation["id"]))
            require(pending.get("status") == "pending", "Unused invitation already joined")
            child, _ = self.spawn("agent-" + str(index), self.args.agent,
                                  ["--master-url", self.origin, "--data-dir", str(self.root / f"agent-{index}-data")],
                                  dict(self.environment, OSS_TRANSFER_JOIN_TOKEN=invitation["inviteToken"]))
            self.agent_processes.append(child)
            self.wait(lambda: self.cli("agent", "show", node, expected=(0, 1, 3)).get("status", {}).get("success"), "Agent online " + node)
            joined = data(self.cli("agent", "invite-status", invitation["id"]))
            require(joined.get("status") == "joined", "Joined invitation did not report joined status")
            self.check_connection_file(self.root / f"agent-{index}-data", "agent")
        self.record("persisted default Master, first-login password change, sessions and invitation lifecycle", agents=2)

    def check_connection_file(self, directory, role):
        path = directory / "cli-cluster.json"
        self.wait(path.is_file, role + " CLI discovery file")
        require(json.loads(path.read_text()) == {"masterUrl": self.origin, "role": role},
                role + " CLI discovery file has the wrong bound address or unexpected fields")

    def nodes_and_storage(self):
        local = self.root / "data with spaces"
        local.mkdir()
        for index, node in enumerate(self.nodes):
            self.cli("agent", "update", node, "--label", f"Worker {index + 1}", "--child-concurrency", "8", "--object-concurrency", "4", "--local-root", local)
            shown = self.cli("agent", "show", node)
            require(shown["node"]["label"] == f"Worker {index + 1}", "Node label edit lost")
            require(shown["status"]["executionStatus"].get("maxConcurrentChildren") == 8 and shown["status"]["executionStatus"].get("objectsPerChild") == 4, "Node concurrency edit lost")
            self.cli("node", "details", node)
        self.cli("agent", "remove", self.nodes[0], expected=1)
        self.cli("limit", "set", "bandwidth", "12", "--unit", "Mbps", "--worker", self.nodes[0])
        self.cli("limit", "set", "put", "33", "--worker", self.nodes[0])
        self.cli("limit", "set", "list", "22", "--worker", self.nodes[0])
        self.cli("limit", "set", "head", "44", "--worker", self.nodes[0])
        status = self.cli("agent", "show", self.nodes[0])["status"]["rateLimit"]
        require(status["bandwidth"]["limitBps"] == 12_000_000 and
                all(status[metric]["limitQps"] == limit for metric, limit in (("list", 22), ("head", 44), ("put", 33))),
                "Node four-field limits did not take effect or a later edit erased an earlier limit")
        other = self.cli("agent", "show", self.nodes[1])["status"]["rateLimit"]
        require(other["bandwidth"]["limitBps"] != 12_000_000 and
                all(other[metric]["limitQps"] != limit for metric, limit in (("list", 22), ("head", 44), ("put", 33))),
                "Single-node limit changed its peer")
        history = data(self.cli("limit", "history", "--limit", "50"))["adjustments"]
        events = [row for row in history if row.get("workerIp") == self.nodes[0] and row.get("manual")]
        require(len(events) >= 4, "Node adjustments absent from history")
        self.cli("limit", "adjustment", "--worker", self.nodes[0], "--event-id", events[0]["id"])
        for metric in ("bandwidth", "list", "head", "put"):
            self.cli("limit", "set", metric, "unlimited", "--worker", self.nodes[0])
        self.environment.update(QA_AK=self.storage["accessKey"], QA_SK=self.storage["secretKey"])
        for alias in ("fixture", "temporary"):
            self.cli("storage", "add", alias, "--endpoint", self.storage["endpoint"], "--region", "us-east-1",
                     "--access-key-env", "QA_AK", "--secret-key-env", "QA_SK", "--path-style", "true", "--v2-signer", "false")
        self.cli("storage", "update", "temporary", "--region", "us-east-1")
        self.cli("storage", "test", "fixture")
        buckets = self.cli("storage", "buckets", "fixture")
        require(all(bucket in json.dumps(buckets) for bucket in self.buckets.values()), "CLI buckets omitted fixture buckets")
        self.cli("storage", "remove", "temporary", expected=1)
        self.cli("storage", "remove", "temporary", "--yes")
        saved = self.cli("storage", "list")
        require('"temporary"' not in json.dumps(saved) and '"fixture"' in json.dumps(saved), "Storage delete/list differs")
        self.record("node concurrency, node limits/history, storage CRUD, bucket listing and credential redaction")

    def tasks(self, *filters):
        result = data(self.cli("task", "list", "--all", "--limit", "2", *filters))
        return result["tasks"]

    def task(self, task_id):
        return self.cli("task", "show", task_id)["task"]

    def create(self, *arguments):
        result = data(self.cli("task", "create", *arguments))
        ids = result.get("parentTaskIds", result.get("taskIds", []))
        require(len(ids) == 1 and result.get("rejectedCount", 0) == 0, "Creation did not accept exactly one parent task")
        return int(ids[0])

    def completed(self, task_id, wanted="COMPLETED", timeout=150):
        last = {}
        def done():
            nonlocal last
            last = self.task(task_id)
            if last["status"] in cluster.TERMINAL:
                require(last["status"] == wanted, f"Task {task_id} ended as {last['status']}, wanted {wanted}")
                return last
            return None
        try:
            return self.wait(done, f"task {task_id}: {wanted}", timeout)
        finally:
            self.evidence["lastTask"] = {key: value for key, value in last.items() if key in (
                "taskIndex", "status", "listObjectCount", "putObjectCount", "skippedObjectCount", "failedObjectCount")}

    def seed(self, prefix, payloads):
        for key, payload in payloads.items():
            self.s3.put_object(Bucket=self.buckets["source"], Key=prefix + key, Body=payload, ContentType="application/octet-stream")

    def verify_objects(self, bucket, prefix, payloads):
        actual = set()
        for page in self.s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=prefix):
            actual.update(row["Key"] for row in page.get("Contents", []))
        require(actual == {prefix + key for key in payloads}, "Destination S3 key set differs from fixture")
        for key, expected in payloads.items():
            body = self.s3.get_object(Bucket=bucket, Key=prefix + key)["Body"]
            try:
                require(digest(body.read()) == digest(expected), "Destination object checksum differs: " + key)
            finally:
                body.close()

    def verify_files(self, target, prefix, payloads):
        for key, expected in payloads.items():
            path = target / (prefix + key)
            require(path.is_file() and cluster.sha256(path) == digest(expected), "Downloaded file checksum differs: " + key)

    def validate_counts(self, task_id, payloads):
        result = self.completed(task_id)
        require(result["listObjectCount"] == len(payloads), "Scanned object count differs from fixture")
        require(result["putObjectCount"] == len(payloads) and result["failedObjectCount"] == 0, "Completed task counts differ from verified objects")
        require(result["putObjectSize"] == sum(map(len, payloads.values())), "Completed task bytes differ from verified objects")
        return result

    def transfers(self):
        self.payloads = {f"nested/{index:04d}.bin": hashlib.sha256(str(index).encode()).digest() * 1024 for index in range(260)}
        self.payloads["large.bin"] = bytes(range(256)) * (16 * MIB // 256)
        self.seed("core/", self.payloads)
        source, destination = ("fixture/" + self.buckets[name] for name in ("source", "destination"))
        upload = self.root / "upload source"
        upload.mkdir()
        for key, payload in self.payloads.items():
            path = upload / key
            path.parent.mkdir(exist_ok=True, parents=True)
            path.write_bytes(payload)
        download = self.root / "download target"
        download.mkdir()
        # Different empty directories are not shared data, even on one host.
        different = self.root / "unshared target"
        different.mkdir()
        invalid = ("oss-to-local", source, "--binding", f"{self.nodes[0]}={download}", "--binding", f"{self.nodes[1]}={different}", "--prefix", "core/")
        before = {row["taskIndex"] for row in self.tasks()}
        self.cli("task", "create", *invalid, "--validate-only", expected=1)
        self.cli("task", "create", *invalid, expected=1)
        require({row["taskIndex"] for row in self.tasks()} == before, "Failed shared precheck created a task")
        self.upload_id = self.create("local-to-oss", destination, "--binding", f"{self.nodes[0]}={upload}", "--binding", f"{self.nodes[1]}={upload}", "--target-prefix", "uploaded/", "--balanced")
        self.validate_counts(self.upload_id, self.payloads)
        self.verify_objects(self.buckets["destination"], "uploaded/", self.payloads)
        self.download_id = self.create("oss-to-local", source, "--binding", f"{self.nodes[0]}={download}", "--binding", f"{self.nodes[1]}={download}", "--prefix", "core/", "--balanced")
        self.validate_counts(self.download_id, self.payloads)
        self.verify_files(download, "core/", self.payloads)
        self.oss_id = self.create("oss-to-oss", source, destination, "--prefix", "core/", "--node", self.nodes[0], "--node", self.nodes[1], "--balanced", "--mapping", "add-prefix", "--target-prefix", "copied/")
        self.validate_counts(self.oss_id, self.payloads)
        self.verify_objects(self.buckets["destination"], "copied/core/", self.payloads)
        self.record("shared-path precheck blocks creation; all three transfer directions independently SHA-256 verified",
                    objectsPerDirection=len(self.payloads), bytesPerDirection=sum(map(len, self.payloads.values())))

    def imports(self):
        payloads = {f"{number:02d}.bin": bytes([number]) * 4096 for number in range(4)}
        self.seed("imports/", payloads)
        source, destination = self.buckets["source"], self.buckets["destination"]
        base = {"sourceS3Alias": "fixture", "sourceBucket": source,
                "destS3Alias": "fixture", "destBucket": destination,
                "prefix": "imports/", "overwrite": False,
                "eligibleWorkerNodes": self.nodes, "distributionMode": "balanced"}
        def fixture(name, rows):
            path = self.root / name
            path.write_text(json.dumps(rows))
            return path
        def accepted(result, count):
            require(result.get("submissionStatus") == "accepted" and result.get("requestedCount") == count,
                    "Import did not confirm the entire submitted batch")
            detail = data(result)
            ids = detail.get("parentTaskIds", detail.get("taskIds", []))
            require(len(ids) == count and len(set(ids)) == count, "Import did not return every distinct parent task")
            return list(map(int, ids))

        rows = [dict(base, keyMapping={"mode": "REPLACE_PREFIX", "sourcePrefix": "imports/",
                                      "targetPrefix": f"import-json-{index}/"}) for index in range(2)]
        path = fixture("bucket-import.json", rows)
        request_id = "cli-import-" + secrets.token_hex(12)
        ids = accepted(self.cli("task", "import", "buckets", path, "--request-id", request_id), 2)
        repeated = accepted(self.cli("task", "import", "buckets", path, "--request-id", request_id), 2)
        require(repeated == ids, "Repeated import request created different parent tasks")
        for index, task_id in enumerate(ids):
            self.validate_counts(task_id, payloads)
            self.verify_objects(destination, f"import-json-{index}/", payloads)

        # A malformed later row must not submit the valid first row.
        before = {row["taskIndex"] for row in self.tasks()}
        invalid = fixture("invalid-import.json", [rows[0], dict(rows[1], destBucket="")])
        self.cli("task", "import", "buckets", invalid, expected=1)
        require({row["taskIndex"] for row in self.tasks()} == before, "Invalid import partially created tasks")

        csv_source, csv_destination = "cli-import-source", "cli-import-destination"
        for bucket in (csv_source, csv_destination):
            self.s3.create_bucket(Bucket=bucket)
        for key, payload in payloads.items():
            self.s3.put_object(Bucket=csv_source, Key=key, Body=payload)
        csv_path = self.root / "bucket-import.csv"
        csv_path.write_text(f"fixture/{csv_source},fixture/{csv_destination},,false,balanced\n")
        csv_id = accepted(self.cli("task", "import", "buckets", csv_path), 1)[0]
        self.validate_counts(csv_id, payloads)
        self.verify_objects(csv_destination, "", payloads)

        # Explicit range import keeps the exclusive start and inclusive end.
        ranged = fixture("range-import.json", [dict(base, workerNode=self.nodes[0], eligibleWorkerNodes=[self.nodes[0]],
                          startKey="imports/00.bin", endKey="imports/02.bin",
                          keyMapping={"mode": "ADD_PREFIX", "targetPrefix": "import-range/"})])
        range_id = accepted(self.cli("task", "import", "tasks", ranged), 1)[0]
        expected = {key: value for key, value in payloads.items() if "00.bin" < key <= "02.bin"}
        self.validate_counts(range_id, expected)
        self.verify_objects(destination, "import-range/imports/", expected)

        upload, download = self.root / "import upload", self.root / "import download"
        upload.mkdir()
        download.mkdir()
        for key, payload in payloads.items():
            (upload / key).write_bytes(payload)
        local = fixture("local-import.json", [
            {"direction": "OSS_TO_LOCAL", "conflictPolicy": "SKIP", "distributionMode": "affinity",
             "sourceEndpoint": {"type": "OSS", "s3Alias": "fixture", "bucket": source, "prefix": "imports/"},
             "destinationEndpoint": {"type": "LOCAL", "nodeBindings": [
                 {"executionNodeIp": self.nodes[0], "localPath": str(download)}]}},
            {"direction": "LOCAL_TO_OSS", "conflictPolicy": "SKIP", "distributionMode": "affinity",
             "sourceEndpoint": {"type": "LOCAL", "nodeBindings": [
                 {"executionNodeIp": self.nodes[1], "localPath": str(upload)}]},
             "destinationEndpoint": {"type": "OSS", "s3Alias": "fixture", "bucket": destination,
                                     "prefix": "import-local/"}}])
        before = {row["taskIndex"] for row in self.tasks()}
        checked = self.cli("task", "import", "local", local, "--validate-only")
        require(checked.get("submissionStatus") == "validated" and data(checked).get("valid"),
                "Local batch precheck did not validate both directions")
        require({row["taskIndex"] for row in self.tasks()} == before, "Validate-only imported real tasks")
        local_ids = accepted(self.cli("task", "import", "local", local), 2)
        for task_id in local_ids:
            self.validate_counts(task_id, payloads)
        self.verify_files(download, "imports/", payloads)
        self.verify_objects(destination, "import-local/", payloads)
        self.record("JSON/CSV bucket imports, exact range import, both local directions, request deduplication and invalid-batch atomicity",
                    taskIds=ids + [csv_id, range_id] + local_ids)

    def child_pages(self, task_id, active=False):
        rows, cursor, seen = [], None, set()
        while True:
            args = ["task", "children", task_id, "--limit", "2", "--order", "priority"]
            if active:
                args.append("--active")
            if cursor:
                args.extend(["--cursor", cursor])
            page = self.cli(*args)
            batch = page["children"]
            rows.extend(batch)
            next_cursor = page["subtaskPage"].get("nextCursor")
            if not page["subtaskPage"].get("hasMore"):
                break
            require(next_cursor and next_cursor not in seen, "Child cursor did not advance")
            seen.add(next_cursor)
            cursor = next_cursor
        require(len({row["childTaskId"] for row in rows}) == len(rows), "Child paging duplicated an ID")
        return rows

    def progress_and_stop(self):
        blob = bytes(range(251)) * (32 * MIB // 251)
        payloads = {f"{index:04d}.bin": b"small-content" for index in range(130)}
        payloads["zzz-large.bin"] = blob
        self.seed("progress/", payloads)
        target = self.root / "progress target"
        target.mkdir()
        task_id = self.create("oss-to-local", "fixture/" + self.buckets["source"], "--binding", f"{self.nodes[0]}={target}", "--prefix", "progress/", "--bandwidth-mbps", "8")
        def in_flight():
            snapshot = data(self.cli("task", "progress", task_id))
            return next((row for row in snapshot.get("items", []) if row["sourceKey"].endswith("zzz-large.bin") and 0 < row["transferredBytes"] < row["size"]), None)
        progress = self.wait(in_flight, "nonzero file progress before object completion", 45)
        require(progress["size"] == len(blob) and progress["childTaskId"].startswith(str(task_id) + "-"), "File progress lost size or child identity")
        active = self.cli("task", "children", task_id, "--active", "--order", "priority")
        require(active["children"] and all(row["status"] not in {"COMPLETED", "FAILED", "PARTIAL_FAILED", "STOPPED"} for row in active["children"]), "Active children include terminal rows")
        def mixed_priority():
            rows = self.cli("task", "children", task_id, "--order", "priority")["children"]
            states = [row["status"] for row in rows]
            return rows if "COMPLETED" in states and "RUNNING" in states else None
        ranked = self.wait(mixed_priority, "running child alongside completed children")
        require(ranked[0]["status"] == "RUNNING", "Priority child listing places completed work before running work")
        child = progress["childTaskId"]
        scoped = data(self.cli("task", "progress", task_id, "--child", child))
        require(all(row["childTaskId"] == child for row in scoped["items"]), "Child progress includes another child")
        self.cli("task", "limits", task_id, "--list-qps", "100", "--head-qps", "120", "--put-qps", "130", "--bandwidth-mbps", "8")
        limits = data(self.cli("task", "limits", task_id))
        require(limits["limitMbps"] == 8 and limits["requestLimits"]["listQps"] == 100 and
                limits["requestLimits"]["headQps"] == 120 and limits["requestLimits"]["putQps"] == 130,
                "Task four-field limits did not take effect")
        self.cli("task", "limits", task_id, "--head-qps", "140")
        patched = data(self.cli("task", "limits", task_id))
        require(patched["limitMbps"] == 8 and patched["requestLimits"]["listQps"] == 100 and
                patched["requestLimits"]["headQps"] == 140 and patched["requestLimits"]["putQps"] == 130,
                "One-field task limit patch erased unrelated limits")
        self.cli("task", "stop", task_id)
        stopped = self.completed(task_id, "STOPPED")
        require(stopped["putObjectCount"] < len(payloads), "Stop test finished before cancellation")
        self.cli("task", "resume", task_id)
        self.cli("task", "bandwidth", task_id, "unlimited")
        self.validate_counts(task_id, payloads)
        self.verify_files(target, "progress/", payloads)
        all_children = self.child_pages(self.upload_id)
        require(len(all_children) >= 2, "Fixture did not exercise multi-page child loading")
        ids = {row["childTaskId"] for row in all_children}
        require(ids == {f"{self.upload_id}-{number}" for number in range(1, len(ids) + 1)}, "Child numbering is not per-parent consecutive")
        require(self.child_pages(self.upload_id, active=True) == [], "Completed task has active children")
        exact = self.cli("task", "show", self.upload_id, "--child", f"{self.upload_id}-1")
        require(exact["child"]["childTaskId"] == f"{self.upload_id}-1", "Child lookup returned the wrong item")
        self.record("live per-file bytes, scoped progress, active children, priority cursor, task limits and stop/resume", taskId=task_id)

    def interrupted_child_recovery(self):
        # Two real interrupted transfers compete for one execution slot after
        # restart. No record or status is manufactured in the database.
        payloads = {"object.bin": b"recovery" * (4 * MIB // 8)}
        self.seed("recover-child/", payloads)
        self.cli("agent", "update", self.nodes[0], "--child-concurrency", "2")
        jobs = []
        for index in range(2):
            target = self.root / f"recover-child-{index}"
            target.mkdir()
            task_id = self.create("oss-to-local", "fixture/" + self.buckets["source"],
                                  "--binding", f"{self.nodes[0]}={target}", "--prefix", "recover-child/",
                                  "--bandwidth-mbps", "0.2")
            jobs.append((task_id, target))
        def both_transferring():
            rows = [row for task_id, _ in jobs for row in self.cli("task", "children", task_id)["children"]]
            return rows if len(rows) == 2 and all(row["status"] == "RUNNING" for row in rows) else None
        before = self.wait(both_transferring, "both child jobs running before process loss")
        generations = {task_id: self.task(task_id)["generation"] for task_id, _ in jobs}
        self.cli("agent", "update", self.nodes[0], "--child-concurrency", "1")
        old = self.agent_processes[0]
        self.expected_exits.add(old.pid)
        old.kill()
        old.wait(timeout=10)
        restarted, _ = self.spawn("agent-0-restarted", self.args.agent,
                                  ["--data-dir", str(self.root / "agent-0-data")])
        self.agent_processes[0] = restarted
        self.wait(lambda: self.cli("agent", "show", self.nodes[0], expected=(0, 1, 3)).get("status", {}).get("success"),
                  "restarted Agent reconnecting")
        self.check_connection_file(self.root / "agent-0-data", "agent")
        def capacity_bound():
            rows = [self.cli("task", "show", task_id, "--child", f"{task_id}-1")["child"] for task_id, _ in jobs]
            if sorted(row["status"] for row in rows) == ["RECOVERABLE", "RUNNING"]:
                return rows
            return None
        recovered = self.wait(capacity_bound, "interrupted child waiting behind occupied recovery capacity", 45)
        waiting = next(row for row in recovered if row["status"] == "RECOVERABLE")
        child_id = waiting["childTaskId"]
        for _ in range(2):
            result = data(self.cli("task", "recover-child", child_id))
            require(result["outcome"] == "WAITING_CAPACITY" and result["child"]["status"] == "RECOVERABLE",
                    "Manual child recovery bypassed the configured execution limit")
            require(result["child"]["attempt"] == waiting["attempt"], "Capacity rejection started a duplicate attempt")
        self.cli("agent", "update", self.nodes[0], "--child-concurrency", "2")
        result = data(self.cli("task", "recover-child", child_id))
        require(result["outcome"] in {"STARTED", "ALREADY_RUNNING"} and result["child"]["status"] == "RUNNING",
                "Available capacity did not recover the interrupted child")
        require(result["child"]["attempt"] == waiting["attempt"] + 1, "Recovery did not start exactly one new attempt")
        again = data(self.cli("task", "recover-child", child_id))
        require(again["outcome"] == "ALREADY_RUNNING" and again["child"]["attempt"] == result["child"]["attempt"],
                "Repeated recovery started another running attempt")
        for (task_id, target), original in zip(jobs, before):
            require(self.task(task_id)["generation"] == generations[task_id], "Child recovery changed the parent generation")
            self.cli("task", "bandwidth", task_id, "unlimited")
            self.validate_counts(task_id, payloads)
            self.verify_files(target, "recover-child/", payloads)
            children = self.child_pages(task_id)
            require(len(children) == 1 and children[0]["childTaskId"] == original["childTaskId"],
                    "Recovery created a replacement child instead of preserving its identity")
        self.cli("task", "recover-child", child_id, expected=1)
        self.cli("agent", "update", self.nodes[0], "--child-concurrency", "8")
        self.record("real Agent interruption, capacity-bound manual child recovery, duplicate recovery and preserved identity/content",
                    taskIds=[task_id for task_id, _ in jobs], admittedOutcome=result["outcome"])

    def retry_and_clear(self):
        payloads = {"blocked.bin": b"r" * (4 * MIB), "ok.bin": b"ok" * 2048}
        self.seed("retry/", payloads)
        failures = []
        for index in range(3):
            target = self.root / f"retry-{index}"
            (target / "retry" / "blocked.bin").mkdir(parents=True)
            task_id = self.create("oss-to-local", "fixture/" + self.buckets["source"], "--binding", f"{self.nodes[index % 2]}={target}", "--prefix", "retry/", "--bandwidth-mbps", "1")
            current = self.completed(task_id, "PARTIAL_FAILED")
            require(current["putObjectCount"] == 1 and current["failedObjectCount"] == 1, "Failure fixture did not produce one success and one failure")
            rows = self.cli("exceptions", "list", "local", "--task-id", task_id)
            require(rows["total"] == 1 and rows["clearableCount"] == 0 and rows["items"][0]["retryStatus"] == "NOT_RETRIED", "Unresolved failure history incorrect")
            cleared = data(self.cli("exceptions", "clear", "local", "--task-id", task_id, "--yes"))
            require(cleared.get("clearedCount") == 0, "Unresolved exception was cleared")
            require(self.cli("exceptions", "list", "local", "--task-id", task_id)["total"] == 1, "Unresolved exception disappeared")
            failures.append((task_id, target, current["generation"]))
        for _, target, _ in failures:
            (target / "retry" / "blocked.bin").rmdir()
        # Race real independent CLI processes. At most one generation per task
        # may be admitted; a second command can reject or report ignored work.
        with ThreadPoolExecutor(max_workers=2) as workers:
            futures = [workers.submit(self.cli, "task", "retry", "--all", "--yes", expected=(0, 1, 3)) for _ in range(2)]
            results = [future.result() for future in futures]
        require(sum(int(data(result).get("accepted", 0)) for result in results) == len(failures), "Duplicate retry submissions were accepted")
        for task_id, _, generation in failures:
            current = self.task(task_id)
            require(current["generation"] == generation + 1, "Retry race incremented a task generation more than once")
            self.cli("task", "retry", task_id, expected=1)
            self.cli("task", "bandwidth", task_id, "unlimited")
        for task_id, target, _ in failures:
            self.validate_counts(task_id, payloads)
            self.verify_files(target, "retry/", payloads)
            rows = self.cli("exceptions", "list", "local", "--task-id", task_id)
            require(rows["total"] == 1 and rows["clearableCount"] == 1, "Resolved exception count incorrect")
            require(rows["items"][0]["retryStatus"] == "RECOVERED" and rows["items"][0]["retryCount"] == 1, "Retry result was not retained")
        first, second = failures[0][0], failures[1][0]
        removed = data(self.cli("exceptions", "clear", "local", "--task-id", first, "--worker", self.nodes[0], "--direction", "OSS_TO_LOCAL", "--yes"))
        require(removed["clearedCount"] == 1, "Filtered resolved cleanup returned wrong count")
        require(self.cli("exceptions", "list", "local", "--task-id", first)["total"] == 0, "Cleaned exception remained")
        require(self.cli("exceptions", "list", "local", "--task-id", second)["total"] == 1, "Scoped cleanup erased another task")
        require(self.task(first)["status"] == "COMPLETED", "Exception cleanup removed its task")
        self.record("unresolved records protected, bulk retry races deduplicated, recovery recorded and resolved cleanup scoped", retryTasks=[row[0] for row in failures])

    def reports_paging_and_recycle(self):
        listed = data(self.cli("task", "list", "--all", "--limit", "2"))
        ids = [row["taskIndex"] for row in listed["tasks"]]
        require(len(ids) >= 7 and len(ids) == len(set(ids)) and listed["pages"] >= 4, "Task --all did not traverse multiple pages")
        first = data(self.cli("task", "list", "--limit", "2"))
        second = data(self.cli("task", "list", "--limit", "2", "--before-id", first["nextBeforeId"]))
        require(not {row["taskIndex"] for row in first["tasks"]} & {row["taskIndex"] for row in second["tasks"]}, "Manual task pagination overlaps")
        report = self.root / "upload-report.zip"
        self.cli("task", "report", self.upload_id, "--output", report)
        with zipfile.ZipFile(report) as archive:
            require(set(archive.namelist()) == {"task-summary.csv", "file-results.csv", "exception-details.csv", "report-metadata.json"}, "ZIP report entries differ")
            metadata = json.loads(archive.read("report-metadata.json"))
            files = list(csv.DictReader(io.StringIO(archive.read("file-results.csv").decode("utf-8-sig"))))
            require(metadata["parentTaskId"] == self.upload_id and metadata["complete"] and metadata["countsReconciled"], "Report is incomplete")
            require(len(files) == len(self.payloads) and all(row["result"] == "SUCCESS" for row in files), "Report rows differ from successful fixture")
        task_id = self.oss_id
        self.cli("task", "delete", task_id)
        def in_trash():
            rows = data(self.cli("task", "trash", "--task-id", task_id))["recycledTasks"]
            return rows if rows else None
        self.wait(in_trash, "deleted task in recycle bin")
        require(task_id not in {row["taskIndex"] for row in self.tasks()}, "Deleted task remains active")
        self.cli("task", "restore", task_id)
        self.wait(lambda: task_id in {row["taskIndex"] for row in self.tasks()}, "restored task in active list")
        self.cli("task", "delete", task_id)
        self.wait(in_trash, "task in recycle bin before purge")
        self.cli("task", "purge", task_id, expected=1)
        self.cli("task", "purge", task_id, "--yes")
        def purged():
            rows = data(self.cli("task", "cleanup-status", task_id))["tasks"]
            require(len(rows) == 1 and rows[0]["taskId"] == task_id, "Cleanup status lost selected task")
            return rows[0] if rows[0].get("purged") else None
        self.wait(purged, "background purge completion")
        require(not data(self.cli("task", "trash", "--task-id", task_id))["recycledTasks"], "Purged task remains in recycle bin")
        self.verify_objects(self.buckets["destination"], "copied/core/", self.payloads)
        self.record("task --all and manual pagination, reconciled ZIP report, delete/restore/background purge retain transferred data")

    def comparison(self):
        for bucket in (self.buckets["source"], self.buckets["destination"]):
            for key in ("same", "different"):
                body = b"12345678" if key == "same" or bucket == self.buckets["source"] else b"87654321"
                self.s3.put_object(Bucket=bucket, Key="compare/" + key, Body=body, ContentType="application/octet-stream")
        for mode in ("metadata", "content"):
            started = data(self.cli("object", "compare", "--source-bucket", "fixture/" + self.buckets["source"],
                                    "--dest-bucket", "fixture/" + self.buckets["destination"], "--scope", "prefix",
                                    "--source-prefix", "compare/", "--dest-prefix", "compare/", "--mode", mode))
            operation = started["id"]
            def completed():
                row = data(self.cli("object", "status", operation, "--limit", "100"))
                require(row["status"] not in {"FAILED", "INTERRUPTED"}, "Comparison did not complete")
                return row if row["status"] == "COMPLETED" else None
            result = self.wait(completed, mode + " prefix comparison")
            require(result["compared"] == 2 and result["errors"] == 0, "Comparison omitted fixture objects")
            require(result["matched"] == (2 if mode == "metadata" else 1), "Metadata/content comparison semantics differ")
            require(result["different"] == (0 if mode == "metadata" else 1), "Same-size content mismatch was not distinguished")
            if mode == "content":
                different = next(row for row in result["items"] if row["status"] == "DIFFERENT")
                require(different["sourceSha256"] == digest(b"12345678") and different["destinationSha256"] == digest(b"87654321"), "Content comparison hashes differ from fixture")
            self.cli("object", "delete", operation)
            self.cli("object", "status", operation, expected=1)
        self.record("prefix metadata comparison distinguishes same-size streaming SHA-256 content mismatch")

    def maintenance_and_logout(self):
        status = data(self.cli("update", "status"))
        require(status["master"]["info"]["supported"] is False, "Raw Master process falsely supports service updates")
        check = data(self.cli("update", "check", expected=(0, 1), timeout=60))
        if check.get("success") is False:
            require(bool(check.get("error") or check.get("message")), "Version check failure lacks an error")
            self.evidence["versionCheck"] = {"status": "FAILED", "reason": "Remote release query unavailable; CLI reported failure"}
        else:
            require(re.fullmatch(r"v?\d+\.\d+\.\d+", check.get("version", "")), "Version check did not return a release version")
            self.evidence["versionCheck"] = {"status": "SUCCESS", "version": check["version"]}
        rejected = self.cli("update", "master", "--version", "4.1.22", "--yes", expected=1)
        require(rejected.get("success") is not True and "version" not in rejected, "Update --version was swallowed by global version flag")
        self.cli("update", "master", "--version", "4.1.22", expected=1)
        self.cli("update", "agents", "--yes", expected=1)
        backups = data(self.cli("backup", "list"))
        require(backups["master"]["supported"] is False, "Raw Master falsely supports service backups")
        self.cli("backup", "prune", "master", "--yes", expected=1)
        self.cli("backup", "prune", "agent", "--worker", self.nodes[0], "--yes", expected=1)
        self.cli("agent", "remove", self.nodes[1], "--yes")
        require(self.nodes[1] not in {row["address"] for row in data(self.cli("agent", "list"))["nodes"]}, "Removed idle Agent remains registered")
        self.cli("logout")
        self.cli("whoami", expected=1)
        self.cli("login", "--password-env", "QA_PASSWORD")
        require(self.cli("whoami")["user"]["username"] == "admin", "Login after logout failed")
        self.record("update target/version parsing, unmanaged update/backup rejection, node removal and session logout/login")

    def run(self):
        for phase in (self.start_cluster, self.nodes_and_storage, self.transfers, self.imports, self.progress_and_stop,
                      self.interrupted_child_recovery,
                      self.retry_and_clear, self.reports_paging_and_recycle, self.comparison, self.maintenance_and_logout):
            self.evidence["phase"] = phase.__name__
            self.save()
            phase()
        self.evidence["status"] = "PASSED"

    def close(self):
        try:
            super().close()
        finally:
            self.evidence["commands"] = self.commands
            self.save()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("master", "agent", "cli", "weed", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    for name in ("master", "agent", "cli", "weed"):
        require(getattr(args, name).is_file(), "Missing executable: " + name)
    harness = Workflows(args)
    try:
        harness.run()
    except Exception as error:
        harness.evidence.update(status="FAILED", error=harness.scrub(str(error)), errorType=type(error).__name__)
        print("FAIL " + harness.scrub(str(error)), flush=True)
        raise
    finally:
        harness.close()
    print("PASS evidence: " + str(harness.root / "evidence.json"), flush=True)


if __name__ == "__main__":
    main()
