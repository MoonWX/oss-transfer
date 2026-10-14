#!/usr/bin/env python3
"""Black-box Go Master + two native Agent acceptance against local SeaweedFS.

Requires the pinned acceptance requirements and a verified native weed binary.
All processes, buckets, credentials and directories belong to this run. This is
not a JDCloud compatibility or Java/Go performance-equivalence test.
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
import platform
import re
import secrets
import subprocess
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import Request, urlopen
import zipfile

import boto3
from botocore.config import Config


HELPERS = importlib.util.spec_from_file_location("store_qa", Path(__file__).with_name("go-store-qa.py"))
store_qa = importlib.util.module_from_spec(HELPERS)
HELPERS.loader.exec_module(store_qa)
MIB = 1024 * 1024
TERMINAL = {"COMPLETED", "STOPPED", "FAILED", "PARTIAL_FAILED"}


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(MIB), b""):
            digest.update(chunk)
    return digest.hexdigest()


def eventually(check, description, timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.15)
    raise AssertionError("Timed out: " + description)


def create_fixture(directory, rate=False):
    directory.mkdir()
    manifest = {}
    for index in range(256 if rate else 260):
        size = 128 * 1024 if rate else (17 * MIB if index in (64, 192) else 4096)
        if not rate and index == 0:
            size = 0
        suffix = " unicode 空格.bin" if index == 2 else ".bin"
        key = f"{index:04d}" + suffix
        path = directory / key
        block = hashlib.sha256(("go-cluster:" + str(index)).encode()).digest() * 2048
        remaining = size
        with path.open("wb") as stream:
            while remaining:
                piece = block[:min(remaining, len(block))]
                stream.write(piece)
                remaining -= len(piece)
        manifest[key] = {"size": size, "sha256": sha256(path)}
    return manifest


def existing_manifest(directory):
    require(directory.is_dir(), "Shared fixture directory does not exist")
    manifest = {}
    for path in sorted(directory.rglob("*")):
        require(not path.is_symlink(), "Shared acceptance fixture must not contain symlinks")
        if path.is_file():
            manifest[path.relative_to(directory).as_posix()] = {"size": path.stat().st_size, "sha256": sha256(path)}
    require(len(manifest) > 128, "Shared fixture must cross the 128-object child boundary")
    return manifest


def local_bytes(directory):
    """Count temporary and published bytes once, including hard-link publication."""
    size, identities = 0, set()
    if directory.exists():
        for root, _, files in os.walk(directory):
            for name in files:
                try:
                    info = (Path(root) / name).stat()
                except FileNotFoundError:
                    continue  # A temporary file may have just been renamed.
                identity = (info.st_dev, info.st_ino)
                if identity not in identities:
                    identities.add(identity)
                    size += info.st_size
    return size


class GrowthSampler:
    def __init__(self, directories):
        self.directories = directories
        self.samples = []
        self.stop_event = threading.Event()
        self.thread = threading.Thread(target=self.run, daemon=True)

    def run(self):
        while not self.stop_event.is_set():
            self.samples.append([time.monotonic(), *[local_bytes(path) for path in self.directories]])
            self.stop_event.wait(0.2)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *_):
        self.stop_event.set()
        self.thread.join(timeout=10)
        require(not self.thread.is_alive(), "Destination sampler did not stop")

    def evaluate(self, rate, expected_bytes):
        require(len(self.samples) >= 30, "Bandwidth test produced too few physical-byte samples")
        results = []
        for column, total in enumerate(expected_bytes, 1):
            windows = []
            for first in range(0, len(self.samples), 5):
                start = self.samples[first]
                if not 0 < start[column] < total * 0.65:
                    continue
                end = next((row for row in self.samples[first + 1:] if row[0] - start[0] >= 5), None)
                if end is None or end[column] >= total:
                    continue
                elapsed, amount = end[0] - start[0], end[column] - start[column]
                # Covers a read/write chunk already reserved at the boundary,
                # sampling jitter and the two Agents' initial token allowance.
                allowance = MIB
                require(amount <= rate * elapsed + allowance,
                        f"Task {column} exceeded its cluster bandwidth: {amount} bytes/{elapsed:.2f}s")
                windows.append({"seconds": elapsed, "bytes": amount, "bytesPerSecond": amount / elapsed})
            require(len(windows) >= 3, f"Task {column}: fewer than three useful 5-second bandwidth windows")
            require(max(row["bytesPerSecond"] for row in windows) > rate * 0.45,
                    "Bandwidth upper-bound check was vacuous: transfer made too little progress")
            results.append({"limitBytesPerSecond": rate, "boundaryAllowanceBytes": MIB, "windows": windows})
        concurrent = [row for row in self.samples if all(0 < row[i + 1] < value for i, value in enumerate(expected_bytes))]
        require(len(concurrent) >= 25 and concurrent[-1][0] - concurrent[0][0] >= 5,
                "Two parent tasks were not concurrently writing payload for at least 5 seconds")
        return results


class Harness:
    def __init__(self, args):
        self.args = args
        self.root = args.output.resolve()
        self.root.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.children, self.streams, self.secrets = [], [], []
        self.environment = dict(os.environ, PATH=str(self.root / "no-external-programs"))
        self.token, self.origin = "", ""
        self.nodes = ["cluster-qa-1", "cluster-qa-2"]
        self.agent_processes = []
        self.serial = 0
        self.evidence = {"status": "RUNNING", "platform": platform.platform(), "checks": [],
                         "parameters": {"scenario": args.scenario, "singleUploadAgent": args.single_upload,
                                        "sharedFixture": str(args.fixture.resolve()) if args.fixture else None},
                         "limitations": ["SeaweedFS is not JDCloud OSS compatibility evidence",
                                         "Two Agents share one host and directory; this does not prove a multi-host shared filesystem",
                                         "No Java/Go performance-equivalence claim"],
                         "binaries": {"master": store_qa.binary_provenance(args.master, "--version"),
                                      "agent": store_qa.binary_provenance(args.agent, "--version"),
                                      "seaweedfs": store_qa.binary_provenance(args.weed, "version")}}
        self.save()

    def scrub(self, text):
        for value in self.secrets:
            text = text.replace(value, "[redacted]")
        return re.sub(r"Setup token: \S+", "Setup token: [redacted]", text)

    def save(self):
        path = self.root / "evidence.json"
        path.write_text(self.scrub(json.dumps(self.evidence, indent=2)) + "\n", encoding="utf-8")

    def record(self, name, **data):
        self.evidence["checks"].append(dict(name=name, **data))
        self.save()
        print("PASS " + name, flush=True)

    def spawn(self, name, binary, arguments, environment=None):
        self.serial += 1
        log = self.root / f"{self.serial:02d}-{name}.log"
        stream = log.open("wb")
        os.chmod(log, 0o600)
        child = subprocess.Popen([str(binary.resolve()), *arguments], cwd=self.root,
                                 env=environment or self.environment, stdout=stream, stderr=subprocess.STDOUT)
        self.children.append(child)
        self.streams.append(stream)
        return child, log

    def api(self, path, body=None, method=None, key=None):
        headers = {"Content-Type": "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        if key:
            headers["Idempotency-Key"] = key
        request = Request(self.origin + path, data=None if body is None else json.dumps(body).encode(),
                          headers=headers, method=method or ("GET" if body is None else "POST"))
        try:
            response = urlopen(request, timeout=40)
        except HTTPError as error:
            response = error
        with response:
            code = response.status
            payload = json.load(response)
        require(code == 200, self.scrub(f"HTTP {code} {path}: {payload.get('message', '')}"))
        data = payload.get("data", payload)
        require(payload.get("success") is not False and data.get("success") is not False,
                self.scrub(f"API failure {path}: {data}"))
        require(not data.get("partial") and not data.get("failedCount") and not data.get("failed"),
                self.scrub(f"Incomplete API result {path}: {data}"))
        return data

    def start_s3(self):
        access, secret = "cluster-qa-" + secrets.token_hex(8), secrets.token_urlsafe(32)
        self.secrets.extend([access, secret])
        ports = store_qa.reserve_ports(8)
        flags = ["master.port", "master.port.grpc", "volume.port", "volume.port.grpc",
                 "filer.port", "filer.port.grpc", "s3.port", "s3.port.grpc"]
        directory = self.root / "seaweed-data"
        directory.mkdir()
        arguments = ["mini", "-dir=" + str(directory), "-ip=127.0.0.1", "-ip.bind=127.0.0.1",
                     "-admin.ui=false", "-webdav=false", "-s3.port.iceberg=0", "-s3.port.lance=0"]
        arguments += [f"-{flag}={port}" for flag, port in zip(flags, ports)]
        process, _ = self.spawn("seaweedfs", self.args.weed, arguments,
                                dict(self.environment, AWS_ACCESS_KEY_ID=access, AWS_SECRET_ACCESS_KEY=secret))
        endpoint = "http://127.0.0.1:" + str(ports[6])
        self.s3 = boto3.client("s3", endpoint_url=endpoint, region_name="us-east-1",
                              aws_access_key_id=access, aws_secret_access_key=secret,
                              config=Config(signature_version="s3v4", s3={"addressing_style": "path"},
                                            connect_timeout=2, read_timeout=30, retries={"max_attempts": 1},
                                            request_checksum_calculation="when_required",
                                            response_checksum_validation="when_required"))

        def ready():
            require(process.poll() is None, "SeaweedFS exited before readiness")
            try:
                self.s3.list_buckets()
                return True
            except Exception:
                return False

        eventually(ready, "authenticated SeaweedFS readiness")
        suffix = secrets.token_hex(4)
        self.buckets = {name: "go-cluster-" + name + "-" + suffix for name in ("source", "destination", "rate")}
        for bucket in self.buckets.values():
            self.s3.create_bucket(Bucket=bucket)
        self.storage = {"alias": "cluster-qa", "endpoint": endpoint, "accessKey": access, "secretKey": secret,
                        "region": "us-east-1", "pathStyleEnable": True, "useV2SignerType": False, "validate": True}

    def multipart_crash_recovery(self):
        environment = dict(os.environ,
                           OSS_TRANSFER_STORE_QA_ENDPOINT=self.storage["endpoint"],
                           OSS_TRANSFER_STORE_QA_ACCESS_KEY=self.storage["accessKey"],
                           OSS_TRANSFER_STORE_QA_SECRET_KEY=self.storage["secretKey"])
        result = subprocess.run(["go", "test", "./internal/agentservice", "-run", "^TestRealS3CrashRecovery$", "-count=1", "-v", "-timeout=180s"],
                                cwd=Path(__file__).resolve().parents[2], env=environment,
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=240, encoding="utf-8")
        (self.root / "multipart-crash.log").write_text(self.scrub(result.stdout), encoding="utf-8")
        require(result.returncode == 0 and "--- PASS: TestRealS3CrashRecovery" in result.stdout,
                "Real multipart crash recovery did not pass; inspect multipart-crash.log")
        self.record("multipart SIGKILL recovery removes owned upload and spool; preserves unrelated upload and prior completed object")

    def start_master(self, first=False):
        address = "127.0.0.1:0" if first else self.origin.removeprefix("http://")
        self.master, log = self.spawn("master", self.args.master,
                                      ["--listen", address, "--data-dir", str(self.root / "master-data")])

        def ready():
            require(self.master.poll() is None, "Master exited before startup")
            return re.search(r"Master listening on (127\.0\.0\.1:\d+)", log.read_text(errors="replace", encoding="utf-8"))

        found = eventually(ready, "Master readiness")
        if first:
            self.origin = "http://" + found[1]
            token = eventually(lambda: re.search(r"Setup token: ([A-Za-z0-9_-]+)", log.read_text(encoding="utf-8")), "setup token")[1]
            credentials = {"username": "cluster-qa", "password": secrets.token_urlsafe(24)}
            self.secrets.extend([token, credentials["password"]])
            self.api("/api/setup/complete", dict(credentials, setupToken=token, publicUrl=self.origin))
            self.token = self.api("/api/auth/login", credentials)["token"]
            self.secrets.append(self.token)
        else:
            require("Setup token:" not in log.read_text(encoding="utf-8"), "Restart incorrectly reset initial setup")
            require(self.api("/api/auth/current")["user"]["username"] == "cluster-qa", "Restart lost account/session")

    def start_agent(self, index, first=False):
        arguments = ["--data-dir", str(self.root / f"agent-{index}-data")]
        environment = dict(self.environment)
        if first:
            invite = self.api("/api/master/agents/invitations", {"address": self.nodes[index], "label": self.nodes[index]})
            environment["OSS_TRANSFER_JOIN_TOKEN"] = invite["inviteToken"]
            self.secrets.append(invite["inviteToken"])
            arguments += ["--master-url", self.origin]
        child, _ = self.spawn("agent-" + str(index), self.args.agent, arguments, environment)
        if first:
            self.agent_processes.append(child)
        else:
            self.agent_processes[index] = child

    def online(self):
        nodes = self.api("/api/master/nodes")["nodes"]
        return nodes if len(nodes) == 2 and all(node["online"] for node in nodes) else None

    def node_settings(self):
        node = self.nodes[0]
        self.api("/api/master/rate-limit/put/" + node, {"qps": 350})
        self.api("/api/master/rate-limit/bandwidth/" + node, {"qps": 16_000_000})
        root = self.root / "declared local directory"
        root.mkdir()
        self.api("/api/master/nodes/" + node, {"label": "QA source", "advertisedLocalRoots": [str(root)]}, method="PUT")
        history = self.api("/api/master/rate-limit/history?limit=50")["adjustments"]
        events = [event for event in history if event["workerIp"] == node]
        require(len(events) == 2 and all(event["manual"] for event in events), "Node setting history omitted the two manual changes")
        self.agent_processes[0].kill()
        self.agent_processes[0].wait(timeout=10)
        self.start_agent(0)
        workers = self.api("/api/master/worker-status")["workers"]
        require(set(workers) == set(self.nodes) and all(row["success"] for row in workers.values()), "Worker status omitted a registered node")
        limits = workers[node]["rateLimit"]
        require(limits["put"]["limitQps"] == 350 and limits["bandwidth"]["limitBps"] == 16_000_000
                and workers[self.nodes[1]]["rateLimit"]["put"]["limitQps"] == 1400,
                "Node restart lost a saved limit or a single-node edit affected its peer")
        declared = self.api("/api/master/local-nodes")["nodes"]
        require(any(row["executionNodeIp"] == node and row["reachable"]
                    and row["advertisedLocalRoots"] == [str(root)] for row in declared), "Declared local root did not survive restart")
        detail = self.api("/api/master/rate-limit/adjustment?" + urlencode({"workerIp": node, "eventId": events[0]["id"]}))
        require(detail["historyComplete"] and detail["processRestarted"], "Manual change detail was lost across native Agent restart")
        self.api("/api/master/rate-limit/put/" + node, {"qps": 1400})
        self.api("/api/master/rate-limit/bandwidth/" + node, {"qps": 2_000_000_000})
        limits = self.api("/api/master/worker-status")["workers"][node]["rateLimit"]
        require(limits["put"]["limitSource"] == "CONFIG" and limits["bandwidth"]["limitSource"] == "CONFIG",
                "Returning saved settings to the configured targets did not show CONFIG")
        self.record("native Agent restart preserves manual QPS, node bandwidth, roots and adjustment history", worker=node)

    def task(self, task_id):
        tasks = self.api("/api/master/status")["tasks"]
        ids = [int(task["taskIndex"]) for task in tasks]
        require(len(ids) == len(set(ids)), "Master returned duplicate logical tasks")
        return next((task for task in tasks if int(task["taskIndex"]) == task_id), None)

    def create(self, path, body, key):
        result = self.api(path, [body], key=key)
        ids = result.get("parentTaskIds", result.get("taskIds", []))
        require(len(ids) == 1 and result.get("rejectedCount", 0) == 0, f"Expected one accepted task: {result}")
        return int(ids[0])

    def wait_task(self, task_id, timeout=180, wanted="COMPLETED"):
        def done():
            current = self.task(task_id)
            require(current is not None, "Created task disappeared")
            if current["status"] in TERMINAL:
                require(current["status"] == wanted, f"Task {task_id} ended as {current['status']}: {current.get('errorMessage')}")
                return current
            return None
        return eventually(done, f"task {task_id} {wanted}", timeout)

    def children_for(self, task_id):
        rows, offset = [], 0
        while True:
            data = self.api(f"/api/master/tasks/{task_id}/children?offset={offset}&limit=2")
            rows.extend(data["items"])
            if len(rows) == data["totalCount"]:
                return rows
            require(data["items"] and len(rows) < data["totalCount"], "Child pagination lost or duplicated a page")
            offset += len(data["items"])

    def verify_task(self, task_id, manifest, both=True):
        task = self.wait_task(task_id)
        require(task.get("scanComplete") is True, "Task completed before source scan finished")
        require(task["listObjectCount"] == len(manifest), "Source count was lost or double counted")
        require(task["listObjectSize"] == sum(row["size"] for row in manifest.values()), "Source bytes differ from fixture")
        require(task["putObjectCount"] + task["skippedObjectCount"] == len(manifest) and task["failedObjectCount"] == 0,
                "Parent results do not account for each source object exactly once")
        children = self.children_for(task_id)
        require([child["childTaskId"] for child in children] == [f"{task_id}-{index + 1}" for index in range(len(children))],
                "Child IDs are not unique per-parent consecutive numbers")
        for field in ("listObjectCount", "listObjectSize", "putObjectCount", "putObjectSize", "skippedObjectCount", "failedObjectCount"):
            require(sum(child[field] for child in children) == task[field], "Parent/child aggregation mismatch: " + field)
        completed_by_node = {}
        for child in children:
            node = child["executionNodeIp"]
            completed_by_node[node] = completed_by_node.get(node, 0) + child["putObjectSize"]
        if both:
            # The built-in fixture places a large object in both child batches.
            # An external fixture may have one indivisible large object, so do
            # not invent an equal-byte-distribution requirement for that input.
            minimum = 1 if self.args.fixture else MIB
            require(all(completed_by_node.get(node, 0) >= minimum for node in self.nodes),
                    "Both Agents must transfer real payload: " + str(completed_by_node))
        return {"taskId": task_id, "objects": len(manifest), "bytes": task["listObjectSize"],
                "children": len(children), "bytesByNode": completed_by_node}

    def upload_body(self, directory, bucket, prefix, bandwidth=None, multi=True):
        body = {"direction": "LOCAL_TO_OSS", "sourceEndpoint": {"type": "LOCAL", "nodeBindings": [
            {"executionNodeIp": node, "localPath": str(directory)} for node in (self.nodes if multi else self.nodes[:1])]},
            "destinationEndpoint": {"type": "OSS", "s3Alias": "cluster-qa", "bucket": bucket, "prefix": prefix},
            "distributionMode": "balanced", "conflictPolicy": "OVERWRITE"}
        if bandwidth:
            body["taskBandwidthLimitMbps"] = bandwidth
        return body

    def download_body(self, bucket, prefix, directory, bandwidth=None):
        body = {"direction": "OSS_TO_LOCAL", "sourceEndpoint": {"type": "OSS", "s3Alias": "cluster-qa", "bucket": bucket, "prefix": prefix},
                "destinationEndpoint": {"type": "LOCAL", "nodeBindings": [
                    {"executionNodeIp": node, "localPath": str(directory)} for node in self.nodes]},
                "distributionMode": "balanced", "conflictPolicy": "OVERWRITE"}
        if bandwidth:
            body["taskBandwidthLimitMbps"] = bandwidth
        return body

    def list_objects(self, bucket, prefix):
        result = {}
        for page in self.s3.get_paginator("list_objects_v2").paginate(Bucket=bucket, Prefix=prefix):
            for row in page.get("Contents", []):
                require(row["Key"] not in result, "S3 listing returned a duplicate key")
                result[row["Key"]] = row["Size"]
        return result

    def verify_s3(self, bucket, prefix, manifest):
        expected = {prefix + key: row["size"] for key, row in manifest.items()}
        require(self.list_objects(bucket, prefix) == expected, "S3 target keys/sizes differ from source manifest")

        def check(item):
            key, row = item
            digest = hashlib.sha256()
            response = self.s3.get_object(Bucket=bucket, Key=prefix + key)
            with response["Body"] as stream:
                for chunk in iter(lambda: stream.read(MIB), b""):
                    digest.update(chunk)
            require(digest.hexdigest() == row["sha256"], "SHA-256 mismatch: " + prefix + key)

        with ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(check, manifest.items()))

    def verify_local(self, directory, prefix, manifest):
        target = directory / prefix
        expected = set(manifest)
        actual = {str(path.relative_to(target)).replace(os.sep, "/") for path in target.rglob("*")
                  if path.is_file() and not path.name.startswith(".oss-transfer-")}
        require(actual == expected, "Local target file set differs from source manifest")
        for key, row in manifest.items():
            path = target / key
            require(path.stat().st_size == row["size"] and sha256(path) == row["sha256"], "Local SHA-256 mismatch: " + key)
        return [str(path.relative_to(directory)) for path in directory.rglob(".oss-transfer-*.tmp")]

    def task_report(self, task_id, expected_status=200):
        request = Request(self.origin + f"/api/master/task-reports/{task_id}",
                          headers={"Authorization": "Bearer " + self.token})
        try:
            response = urlopen(request, timeout=40)
        except HTTPError as error:
            response = error
        with response:
            payload = response.read(32 * MIB + 1)
            require(response.status == expected_status,
                    f"Task report returned HTTP {response.status}, expected {expected_status}")
            require(len(payload) <= 32 * MIB, "Fixture report exceeds its bounded acceptance size")
            if expected_status == 200:
                require(response.headers.get_content_type() == "application/zip", "Report has wrong content type")
                (self.root / f"task-{task_id}-report.zip").write_bytes(payload)
                return payload
            require(json.loads(payload).get("success") is False, "Running-task report did not report failure")

    def verify_report(self, task_id, manifest, direction, source_prefix, target_prefix,
                      source_directory=None, target_directory=None):
        with zipfile.ZipFile(io.BytesIO(self.task_report(task_id))) as report:
            require(set(report.namelist()) == {"task-summary.csv", "file-results.csv", "exception-details.csv", "report-metadata.json"},
                    "Task report is missing a documented CSV or metadata entry")
            metadata = json.loads(report.read("report-metadata.json"))
            def rows(name):
                # Parse CSV before reversing the documented spreadsheet escaping.
                parsed = list(csv.DictReader(io.StringIO(report.read(name).decode("utf-8-sig"))))
                return [{key: (value[1:] if len(value) > 1 and value[0] == "'" and value[1] in "'=+-@\t\r\n" else value)
                         for key, value in row.items()} for row in parsed]
            files, summary, exceptions = rows("file-results.csv"), rows("task-summary.csv"), rows("exception-details.csv")
        total = sum(row["size"] for row in manifest.values())
        require(metadata.get("complete") is True and metadata.get("countsReconciled") is True
                and metadata.get("stableSnapshot") is True and metadata.get("contentVerified") is False,
                "Completed task report is incomplete or falsely claims independent checksum verification")
        require(metadata["parentTaskId"] == task_id and metadata["fileRows"] == len(manifest)
                and metadata["resultCounts"]["SUCCESS"] == len(manifest)
                and metadata["resultBytes"]["SUCCESS"] == total, "Report metadata differs from verified files")
        require(len(files) == len(manifest) and not exceptions, "Report has duplicate/missing file rows or unexplained exceptions")
        indexed = {row["logicalKey"]: row for row in files}
        require(len(indexed) == len(files) and set(indexed) == {source_prefix + key for key in manifest},
                "Report source inventory differs from the real fixture")
        children = {row["childTaskId"]: row for row in self.children_for(task_id)}
        counts = {child_id: 0 for child_id in children}
        for key, expected in manifest.items():
            row = indexed[source_prefix + key]
            require("taskId" not in row and "shardId" not in row, "Report leaked obsolete task/shard IDs")
            require(row["parentTaskId"] == str(task_id) and row["direction"] == direction
                    and row["childTaskId"] in children and row["node"] in self.nodes
                    and row["result"] == "SUCCESS" and int(row["size"]) == expected["size"],
                    "Report row identity/result/size differs from the verified task")
            counts[row["childTaskId"]] += 1
            if source_directory is None:
                require(row["sourceKey"] == source_prefix + key and not row["sourcePath"], "Report source key is incorrect")
            else:
                require(not row["sourceKey"] and Path(row["sourcePath"]) == source_directory / key, "Report source path is incorrect")
            if target_directory is None:
                require(row["destinationKey"] == target_prefix + key and not row["destinationPath"], "Report target key mapping is incorrect")
            else:
                require(not row["destinationKey"] and Path(row["destinationPath"]) == target_directory / (target_prefix + key),
                        "Report target path is incorrect")
        require(all(counts[child_id] == child["putObjectCount"] for child_id, child in children.items()),
                "Report assigned file rows to the wrong child task")
        parents = [row for row in summary if row["rowType"] == "TASK"]
        require(len(parents) == 1 and parents[0]["parentTaskId"] == str(task_id)
                and int(parents[0]["successCount"]) == len(manifest) and int(parents[0]["successBytes"]) == total,
                "Report task summary differs from actual transfer")
        self.record(direction + " report matches verified file inventory and target mapping", taskId=task_id,
                    fileRows=len(files), bytes=total, children=len(children), archive=f"task-{task_id}-report.zip")

    def core(self):
        fixture = self.args.fixture.resolve() if self.args.fixture else self.root / "fixture"
        manifest = existing_manifest(fixture) if self.args.fixture else create_fixture(fixture)
        (self.root / "fixture-manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        body = self.upload_body(fixture, self.buckets["source"], "uploaded/", multi=not self.args.single_upload)
        started = time.monotonic()
        upload = self.create("/api/master/local-transfers", body, "core-upload")
        require(self.create("/api/master/local-transfers", body, "core-upload") == upload, "Repeated create made another parent task")
        data = self.verify_task(upload, manifest, both=not self.args.single_upload)
        data["seconds"] = time.monotonic() - started
        self.verify_s3(self.buckets["source"], "uploaded/", manifest)
        self.record("LOCAL_TO_OSS integrity and idempotent creation", **data)
        self.verify_report(upload, manifest, "LOCAL_TO_OSS", "", "uploaded/", source_directory=fixture)
        started = time.monotonic()
        copy = self.create("/api/master/add-buckets", {"sourceS3Alias": "cluster-qa", "sourceBucket": self.buckets["source"],
                           "destS3Alias": "cluster-qa", "destBucket": self.buckets["destination"], "prefix": "uploaded/",
                           "eligibleWorkerNodes": self.nodes, "distributionMode": "balanced", "conflictPolicy": "OVERWRITE",
                           "keyMapping": {"mode": "REPLACE_PREFIX", "sourcePrefix": "uploaded/", "targetPrefix": "copied/"}}, "core-copy")
        data = self.verify_task(copy, manifest)
        data["seconds"] = time.monotonic() - started
        self.verify_s3(self.buckets["destination"], "copied/", manifest)
        self.record("OSS_TO_OSS two-Agent transfer and mapped target keys", **data)
        self.verify_report(copy, manifest, "OSS_TO_OSS", "uploaded/", "copied/")
        directory = self.root / "download"
        started = time.monotonic()
        download = self.create("/api/master/local-transfers", self.download_body(self.buckets["destination"], "copied/", directory), "core-download")
        data = self.verify_task(download, manifest)
        data["seconds"] = time.monotonic() - started
        require(not self.verify_local(directory, "copied", manifest), "Successful transfer left temporary files")
        self.record("OSS_TO_LOCAL integrity and parent/child aggregation", **data)
        self.verify_report(download, manifest, "OSS_TO_LOCAL", "copied/", "copied/", target_directory=directory)
        self.metadata_and_page_apis()

    def metadata_and_page_apis(self):
        payload = b"metadata acceptance"
        for bucket, key in ((self.buckets["source"], "metadata/source"), (self.buckets["destination"], "metadata/target")):
            self.s3.put_object(Bucket=bucket, Key=key, Body=payload, ContentType="application/x-go-qa",
                               CacheControl="max-age=123", Metadata={"qa-owner": "native-cluster"})
        query = {"sourceBucket": "cluster-qa/" + self.buckets["source"], "sourceKey": "metadata/source",
                 "destBucket": "cluster-qa/" + self.buckets["destination"], "destKey": "metadata/target"}
        result = self.api("/api/master/object-metadata?" + urlencode(query))
        for field in ("sourceMetadata", "destMetadata"):
            metadata = result[field]
            require(metadata["exists"] is True and metadata["queryStatus"] == "FOUND"
                    and metadata["contentLength"] == len(payload) and metadata["contentType"] == "application/x-go-qa"
                    and metadata["cacheControl"] == "max-age=123" and metadata["userMetadata"]["qa-owner"] == "native-cluster",
                    "Object metadata API dropped actual S3 headers or returned an invented success")
        require(result["verification"]["sizeMatch"] is True, "Metadata size comparison did not use returned object sizes")
        query["destKey"] = "metadata/absent"
        result = self.api("/api/master/object-metadata?" + urlencode(query))
        require(result["destMetadata"]["queryStatus"] == "NOT_FOUND" and result["verification"]["sizeMatch"] is None,
                "Missing target was presented as comparable metadata")
        reads = ["/api/auth/current", "/api/worker/node-info", "/api/master/nodes", "/api/master/local-nodes",
                 "/api/master/status", "/api/master/deleted-tasks", "/api/master/worker-status",
                 "/api/master/rate-limit/history?limit=50", "/api/master/s3-config",
                 "/api/master/s3-config/cluster-qa/buckets", "/api/master/local-transfers/failures?limit=10"]
        for path in reads:
            self.api(path)
        for kind in ("failed", "fallback"):
            self.api("/api/master/" + kind, {"limit": 10})
        self.record("page read APIs and Agent-backed S3 metadata comparison", getEndpoints=reads,
                    exceptionQueries=["failed", "fallback"], missingTarget="NOT_FOUND")

    def rate_fixture(self):
        directory = self.root / "rate-fixture"
        manifest = create_fixture(directory, rate=True)
        def put(item):
            key, _ = item
            with (directory / key).open("rb") as body:
                self.s3.put_object(Bucket=self.buckets["rate"], Key="rate/" + key, Body=body)
        with ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(put, manifest.items()))
        return directory, manifest

    def bandwidth(self, manifest):
        destinations = [self.root / f"limited-{index}" for index in (1, 2)]
        started = time.monotonic()
        with GrowthSampler(destinations) as sampler:
            tasks = [self.create("/api/master/local-transfers", self.download_body(self.buckets["rate"], "rate/", directory, 8),
                                 "bandwidth-" + str(index)) for index, directory in enumerate(destinations)]
            self.task_report(tasks[0], expected_status=409)
            for task_id in tasks:
                self.wait_task(task_id, timeout=120)
        total = sum(row["size"] for row in manifest.values())
        result = sampler.evaluate(1_000_000, [total, total])
        (self.root / "bandwidth-samples.json").write_text(json.dumps(sampler.samples) + "\n", encoding="utf-8")
        data = []
        for task_id, directory in zip(tasks, destinations):
            data.append(self.verify_task(task_id, manifest))
            self.verify_local(directory, "rate", manifest)
        self.record("two concurrent 8 Mbps parents share each limit across two Agents", tasks=data,
                    seconds=time.monotonic() - started, physicalByteWindows=result, runningReportHTTPStatus=409)

    def kill_restart(self, task_id):
        identities = {node["address"]: node["id"] for node in self.api("/api/master/nodes")["nodes"]}
        self.master.kill()
        self.master.wait(timeout=10)
        self.agent_processes[0].kill()
        self.agent_processes[0].wait(timeout=10)
        self.start_master()
        self.start_agent(0)
        nodes = eventually(self.online, "two Agents reconnect after process termination")
        require({node["address"]: node["id"] for node in nodes} == identities, "Restart changed enrollment identity")
        require(self.task(task_id) is not None, "Restart lost the durable parent task")

    def recovery(self, directory, manifest):
        body = self.upload_body(directory, self.buckets["source"], "resume/", 8, multi=not self.args.single_upload)
        task_id = self.create("/api/master/local-transfers", body, "stop-resume")
        eventually(lambda: len(self.list_objects(self.buckets["source"], "resume/")) >= 4,
                   "payload before stop", timeout=30)
        started = time.monotonic()
        self.api("/api/master/stop", {"taskIds": [task_id]})
        stopped = self.wait_task(task_id, wanted="STOPPED", timeout=10)
        elapsed = time.monotonic() - started
        require(elapsed < 8, f"Stop took {elapsed:.2f}s")
        before = self.list_objects(self.buckets["source"], "resume/")
        time.sleep(2)
        require(before == self.list_objects(self.buckets["source"], "resume/"), "Payload kept publishing after stop ACK")
        self.kill_restart(task_id)
        require(self.task(task_id)["status"] == "STOPPED", "Restart resumed a deliberately stopped task")
        time.sleep(2)
        require(before == self.list_objects(self.buckets["source"], "resume/"), "Restart bypassed the durable stop fence")
        require(self.create("/api/master/local-transfers", body, "stop-resume") == task_id, "Restart lost create idempotency")
        self.api("/api/master/resume", {"taskIds": [task_id]})
        data = self.verify_task(task_id, manifest, both=not self.args.single_upload)
        self.verify_s3(self.buckets["source"], "resume/", manifest)
        self.record("stop ACK fences writes; Master/Agent restart retains stop and original parent", **data,
                    stopSeconds=elapsed, objectsCommittedAtStop=len(before), creditedBeforeResume=stopped["putObjectCount"])

        target = self.root / "crash-download"
        body = self.download_body(self.buckets["rate"], "rate/", target, 8)
        task_id = self.create("/api/master/local-transfers", body, "running-crash")
        eventually(lambda: local_bytes(target) >= 2 * MIB, "active payload before forced crash", timeout=30)
        def incomplete_payloads():
            files = []
            for path in target.rglob(".oss-transfer-*.tmp"):
                if not re.fullmatch(r"\.oss-transfer-[0-9a-f]{64}-[0-9a-f]{32}\.tmp", path.name):
                    continue
                try:
                    if path.stat().st_size > 0:
                        files.append(str(path.relative_to(target)))
                except FileNotFoundError:
                    pass  # A writer may have published this file.
            return files
        incomplete = eventually(incomplete_payloads, "durably tracked incomplete local files before forced crash", timeout=10)
        require(self.task(task_id)["status"] not in TERMINAL, "Crash test fixture finished before injection")
        self.kill_restart(task_id)
        require(self.create("/api/master/local-transfers", body, "running-crash") == task_id, "Crash retry made another task")
        data = self.verify_task(task_id, manifest)
        orphans = self.verify_local(target, "rate", manifest)
        if orphans:
            self.evidence["orphanTemporaryFileDetails"] = [
                {"path": name, "bytes": (target / name).stat().st_size,
                 "modifiedNs": (target / name).stat().st_mtime_ns} for name in orphans
            ]
        require(not orphans, "Restart left incomplete temporary files: " + ", ".join(orphans))
        self.record("SIGKILL during transfer recovers original work without duplicate progress", **data,
                    temporaryFilesBeforeCrash=incomplete, orphanTemporaryFiles=orphans)

    def close(self):
        for child in reversed(self.children):
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=10)
        for stream in self.streams:
            stream.close()
        for path in self.root.glob("*.log"):
            path.write_text(self.scrub(path.read_text(errors="replace", encoding="utf-8")), encoding="utf-8")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("master", "agent", "weed", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--scenario", choices=("core", "all"), default="all")
    parser.add_argument("--fixture", type=Path, help="Read an existing Java/Go shared fixture; never modify it")
    parser.add_argument("--multipart-recovery", action="store_true", help="Run the real S3 crash-recovery integration test using the current source")
    parser.add_argument("--single-upload", action="store_true", help="Use one Agent for upload until shared-root validation is ready")
    args = parser.parse_args()
    h = Harness(args)
    try:
        h.start_s3()
        if args.multipart_recovery:
            h.multipart_crash_recovery()
        h.start_master(first=True)
        for index in range(2):
            h.start_agent(index, first=True)
        eventually(h.online, "two native Agents online")
        result = h.api("/api/master/s3-config", h.storage)
        require(result["successCount"] == 2, "Storage configuration was not applied by both Agents")
        h.record("native Master and two Agents run with an empty executable PATH; storage validated through Agent")
        h.node_settings()
        h.core()
        if args.scenario == "all":
            directory, manifest = h.rate_fixture()
            h.bandwidth(manifest)
            h.recovery(directory, manifest)
        h.evidence["status"] = "PASSED"
        h.save()
        print("Evidence: " + str(h.root), flush=True)
    except BaseException as error:
        h.evidence["status"] = "FAILED"
        h.evidence["error"] = h.scrub(str(error))
        h.save()
        raise
    finally:
        h.close()


if __name__ == "__main__":
    main()
