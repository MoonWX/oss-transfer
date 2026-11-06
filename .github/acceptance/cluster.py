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
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import secrets
import stat
import subprocess
import sys
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode, urlsplit
from urllib.request import Request, urlopen
import zipfile

import boto3
from botocore.config import Config

# botocore sends "Expect: 100-continue" even with an empty body. SeaweedFS
# answers such a PUT without a 100 response, which leaves that pooled
# connection broken: the next PUT with a body on it waits out the 30-second
# read timeout before a retry. Fixtures with empty objects lost about 30
# seconds per upload. Drop the header for empty bodies only.
os.environ.setdefault("BOTO_EXPERIMENTAL__NO_EMPTY_CONTINUE", "true")


HELPERS = importlib.util.spec_from_file_location("store_qa", Path(__file__).with_name("store-qa.py"))
store_qa = importlib.util.module_from_spec(HELPERS)
HELPERS.loader.exec_module(store_qa)
MIB = 1024 * 1024
TERMINAL = {"COMPLETED", "STOPPED", "FAILED", "PARTIAL_FAILED"}
SCENARIOS = ("core", "transfer", "recovery", "nodes", "dynamic_share", "all")
# Scenarios that measure bandwidth or inject stops and crashes; with a gate
# they wait for the other acceptance groups on this runner.
MEASURED = ("transfer", "recovery", "nodes", "all")


def bandwidth_allowance(rate):
    """Bytes a task may write beyond rate × elapsed in one sampled window.

    Each Agent keeps up to 100 ms of its share as earned credit, at least
    64 KiB, and holds at most one reserved read of 64 KiB or 100 ms of it,
    so all Agents together may pass the limit by 100 ms of it plus their
    floors and reads. The 256 KiB covers those and sampling jitter for two
    Agents; a slower CPU must not hide an unenforced cap.
    """
    return int(rate * 0.1) + 256 * 1024


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def external_s3_fixture():
    name = os.environ.get("OSS_TRANSFER_QA_S3_CONFIG")
    if not name:
        return None
    path = Path(name)
    require(path.is_file(), "External S3 fixture configuration is missing")
    require(os.name == "nt" or stat.S_IMODE(path.stat().st_mode) & 0o077 == 0,
            "External S3 fixture configuration must be private (mode 0600)")
    config = json.loads(path.read_text(encoding="utf-8"))
    require(config.get("format") == 1, "Unsupported external S3 fixture configuration")
    require(all(isinstance(config.get(key), str) and config[key] for key in ("endpoint", "accessKey", "secretKey")),
            "External S3 fixture endpoint or credentials are missing")
    endpoint = urlsplit(config["endpoint"])
    require(endpoint.scheme == "http" and endpoint.hostname and endpoint.port
            and not endpoint.username and not endpoint.password and endpoint.path in ("", "/")
            and not endpoint.query and not endpoint.fragment, "Invalid external S3 fixture endpoint")
    provenance = config.get("provenance", {})
    require(provenance.get("mode") == "host-native" and provenance.get("release") == "4.48"
            and all(isinstance(provenance.get(key), str) and provenance[key]
                    for key in ("platform", "sha256", "archive", "archiveSHA256", "version")),
            "External S3 fixture provenance is incomplete")
    # Do not copy arbitrary configuration fields into public acceptance evidence.
    config["provenance"] = {key: provenance[key] for key in
                            ("mode", "release", "platform", "sha256", "archive", "archiveSHA256", "version")}
    return config


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(MIB), b""):
            digest.update(chunk)
    return digest.hexdigest()


def multipart_test_platform(agent, test_binary):
    """Check the test executable against the released Agent, not the host CPU."""
    spec = importlib.util.spec_from_file_location("multipart_package_check", Path(__file__).with_name("prepare.py"))
    package_check = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(package_check)
    matches = []
    for operating_system, architecture in package_check.TARGETS.PLATFORMS:
        try:
            package_check.verify_binary(agent, operating_system, architecture)
        except ValueError:
            continue
        matches.append((operating_system, architecture))
    require(len(matches) == 1, "Cannot identify the packaged Agent platform for multipart recovery")
    operating_system, architecture = matches[0]
    package_check.verify_binary(test_binary, operating_system, architecture)
    return operating_system + "/" + architecture


def eventually(check, description, timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.15)
    raise AssertionError("Timed out: " + description)


def create_fixture(directory, rate=False, bandwidth=False):
    directory.mkdir()
    manifest = {}
    for index in range(129 if bandwidth else 256 if rate else 260):
        if bandwidth:
            # Cross the real 128-object dispatch boundary with payload in both
            # batches, without making the bandwidth check a small-file benchmark.
            size = 4 * MIB if index in (0, 128) else 0
        else:
            size = 128 * 1024 if rate else (17 * MIB if index in (64, 192) else 4096)
        if not bandwidth and not rate and index == 0:
            size = 0
        suffix = " unicode 空格.bin" if index == 2 else ".bin"
        key = f"{index:04d}" + suffix
        path = directory / key
        block = hashlib.sha256(("cluster:" + str(index)).encode()).digest() * 2048
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


def published_files(directory, manifest):
    """Manifest keys already published at their full size."""
    done = []
    for key, row in manifest.items():
        try:
            if (directory / key).stat().st_size == row["size"]:
                done.append(key)
        except FileNotFoundError:
            pass
    return done


def require_unfinished(finished, published, total, injection, rate):
    """A recovery check whose transfer ended first proves nothing; fail it loudly."""
    require(not finished and published < total,
            f"{injection} test transfer finished before injection ({published}/{total} objects at {rate} Mbps); "
            "lower this platform's RECOVERY_BANDWIDTH_MBPS in run-platform.py")


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

    def __exit__(self, error_type, *_):
        self.stop_event.set()
        self.thread.join(timeout=10)
        if error_type is None:
            require(not self.thread.is_alive(), "Destination sampler did not stop")

    def evaluate(self, rate, expected_bytes):
        require(len(self.samples) >= 30, "Bandwidth test produced too few physical-byte samples")
        # Each parent has two nonempty objects, one reader per Agent. At the
        # fixture's 2 Mbps cap, each Agent can retain 64 KiB of earned credit
        # plus one <=12.5 KB reserved read (see bandwidth_allowance).
        allowance = bandwidth_allowance(rate)
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
                require(amount <= rate * elapsed + allowance,
                        f"Task {column} exceeded its cluster bandwidth: {amount} bytes/{elapsed:.2f}s")
                windows.append({"seconds": elapsed, "bytes": amount, "bytesPerSecond": amount / elapsed})
            require(len(windows) >= 3, f"Task {column}: fewer than three useful 5-second bandwidth windows")
            require(max(row["bytesPerSecond"] for row in windows) > rate * 0.45,
                    "Bandwidth upper-bound check was vacuous: transfer made too little progress")
            results.append({"limitBytesPerSecond": rate, "boundaryAllowanceBytes": allowance, "windows": windows})
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
        self.s3_process = None
        self.external_s3 = external_s3_fixture()
        if self.external_s3:
            self.secrets.extend([self.external_s3["accessKey"], self.external_s3["secretKey"]])
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
                                      "seaweedfs": self.external_s3["provenance"] if self.external_s3
                                      else store_qa.binary_provenance(args.weed, "version")}}
        self.save()

    def scrub(self, text):
        for value in self.secrets:
            text = text.replace(value, "[redacted]")
        return re.sub(r'((?:Initial password|Setup token|初始密码|初始化密钥|\\u521d\\u59cb\\u5bc6\\u7801|\\u521d\\u59cb\\u5316\\u5bc6\\u94a5)[ \t]*(?:[:：]|\\uff1a))[ \t]*[^\s"\\]+', r"\1 [redacted]", text)

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

    def api(self, path, body=None, method=None, key=None, *, timeout=40, max_bytes=None, allow_partial=False):
        headers = {"Content-Type": "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        if key:
            headers["Idempotency-Key"] = key
        request = Request(self.origin + path, data=None if body is None else json.dumps(body).encode(),
                          headers=headers, method=method or ("GET" if body is None else "POST"))
        try:
            response = urlopen(request, timeout=timeout)
        except HTTPError as error:
            response = error
        with response:
            code = response.status
            if max_bytes is None:
                payload = json.load(response)
            else:
                encoded = response.read(max_bytes + 1)
                require(len(encoded) <= max_bytes, "Diagnostic response exceeded its size limit")
                payload = json.loads(encoded)
        require(code == 200, self.scrub(f"HTTP {code} {path}: {payload.get('message', '')}"))
        data = payload.get("data", payload)
        require(payload.get("success") is not False and data.get("success") is not False,
                self.scrub(f"API failure {path}: {data}"))
        require((allow_partial or not data.get("partial")) and not data.get("failedCount") and not data.get("failed"),
                self.scrub(f"Incomplete API result {path}: {data}"))
        return data

    def start_s3(self):
        process = None
        if self.external_s3:
            access, secret, endpoint = (self.external_s3[key] for key in ("accessKey", "secretKey", "endpoint"))
        else:
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
            self.s3_process = process
            endpoint = "http://127.0.0.1:" + str(ports[6])
        self.s3 = boto3.client("s3", endpoint_url=endpoint, region_name="us-east-1",
                              aws_access_key_id=access, aws_secret_access_key=secret,
                              config=Config(signature_version="s3v4", s3={"addressing_style": "path"},
                                            connect_timeout=2, read_timeout=30, retries={"max_attempts": 1},
                                            request_checksum_calculation="when_required",
                                            response_checksum_validation="when_required"))

        def ready():
            require(process is None or process.poll() is None, "SeaweedFS exited before readiness")
            try:
                self.s3.list_buckets()
                return True
            except Exception:
                return False

        if self.external_s3:
            # The host has already checked readiness. Authentication/connectivity
            # failures here are fatal; never silently launch a different fixture.
            try:
                self.s3.list_buckets()
            except Exception:
                raise RuntimeError("External S3 fixture authentication/connectivity check failed") from None
        else:
            eventually(ready, "authenticated SeaweedFS readiness")
        suffix = secrets.token_hex(4)
        self.buckets = {name: "cluster-" + name + "-" + suffix for name in ("source", "destination", "rate")}
        for bucket in self.buckets.values():
            self.s3.create_bucket(Bucket=bucket)
        self.storage = {"alias": "cluster-qa", "endpoint": endpoint, "accessKey": access, "secretKey": secret,
                        "region": "us-east-1", "pathStyleEnable": True, "useV2SignerType": False, "validate": True}

    def multipart_crash_recovery(self):
        environment = dict(os.environ,
                           OSS_TRANSFER_STORE_QA_ENDPOINT=self.storage["endpoint"],
                           OSS_TRANSFER_STORE_QA_ACCESS_KEY=self.storage["accessKey"],
                           OSS_TRANSFER_STORE_QA_SECRET_KEY=self.storage["secretKey"])
        environment.pop("OSS_TRANSFER_STORE_QA_HOST_FIXTURE", None)
        if self.external_s3:
            environment["OSS_TRANSFER_STORE_QA_HOST_FIXTURE"] = "1"
        test_binary = self.args.multipart_test_binary
        if test_binary:
            test_binary = test_binary.resolve(strict=True)
            target_platform = multipart_test_platform(self.args.agent, test_binary)
            self.evidence["multipartRecovery"] = {
                "mode": "precompiled-test", "path": str(test_binary), "sha256": sha256(test_binary),
                "platform": target_platform, "platformSource": "executable header matched against the packaged Agent",
                "scope": "internal/agentservice test executable; separate from the released Agent binary",
            }
            command = [str(test_binary), "-test.run=^TestRealS3CrashRecovery$", "-test.count=1", "-test.v", "-test.timeout=180s"]
        else:
            self.evidence["multipartRecovery"] = {"mode": "go-test", "package": "./internal/agentservice"}
            command = ["go", "test", "./internal/agentservice", "-run", "^TestRealS3CrashRecovery$", "-count=1", "-v", "-timeout=180s"]
        self.save()
        result = subprocess.run(command,
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
            initial_password = eventually(lambda: re.search(r"Initial password: ([A-Za-z0-9_-]+)", log.read_text(encoding="utf-8")), "initial password")[1]
            credentials = {"username": "admin", "password": secrets.token_urlsafe(24)}
            self.secrets.extend([initial_password, credentials["password"]])
            login = self.api("/api/auth/login", dict(credentials, password=initial_password))
            require(login["user"]["mustChangePassword"], "Initial password did not require replacement")
            self.token = login["token"]
            self.secrets.append(self.token)
            changed = self.api("/api/auth/change-password", {"oldPassword": initial_password, "newPassword": credentials["password"]})
            require(not changed["user"]["mustChangePassword"], "Password change did not unlock the account")
        else:
            require("Initial password:" not in log.read_text(encoding="utf-8"), "Restart incorrectly regenerated the administrator password")
            require(self.api("/api/auth/current")["user"]["username"] == "admin", "Restart lost account/session")

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

    def restarted_agent_status(self, index, previous_process):
        node = self.nodes[index]
        require(bool(previous_process), "Agent settings history omitted its process ID")

        def ready():
            require(self.master.poll() is None, "Master exited during Agent restart")
            require(self.agent_processes[index].poll() is None, "Agent exited during restart")
            status = self.api("/api/master/worker-status", allow_partial=True)
            workers = status["workers"]
            require(set(workers) == set(self.nodes), "Worker status omitted a registered node")
            if status.get("partial"):
                # Only this deliberate restart may temporarily miss a command.
                # HTTP errors, peer failures and Agent business errors stay fatal.
                require(status.get("complete") is False and status.get("failedWorkers") == [node]
                        and workers[node].get("success") is False
                        and workers[node].get("message") == "context deadline exceeded"
                        and all(row.get("success") is True for name, row in workers.items() if name != node),
                        self.scrub(f"Unexpected response during Agent restart: {status}"))
                return None
            require(status.get("complete") is True and all(row.get("success") is True for row in workers.values()),
                    "Worker status did not confirm both Agents")
            process = workers[node]["rateLimit"]["autoAdjust"]["processId"]
            require(bool(process) and process != previous_process, "Worker status did not come from the restarted Agent")
            return workers

        return eventually(ready, "restarted Agent command channel")

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
        workers = self.restarted_agent_status(0, events[0]["processId"])
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
        started, last_observed, last_task = time.monotonic(), None, None
        def done():
            nonlocal last_observed, last_task
            current = self.task(task_id)
            last_observed, last_task = time.monotonic(), current
            require(current is not None, "Created task disappeared")
            if current["status"] in TERMINAL:
                require(current["status"] == wanted, f"Task {task_id} ended as {current['status']}: {current.get('errorMessage')}")
                return current
            return None
        try:
            return eventually(done, f"task {task_id} {wanted}", timeout)
        except Exception as error:
            # Preserve the last successful poll instead of making another full
            # status query after a timeout. Never log endpoint configuration,
            # request headers or arbitrary error messages from task responses.
            fields = ("taskIndex", "parentTaskId", "childTaskId", "status", "direction",
                      "executionNodeIp", "attempt", "scanComplete", "childCount",
                      "listObjectCount", "listObjectSize", "putObjectCount", "putObjectSize",
                      "skippedObjectCount", "skippedObjectSize", "failedObjectCount", "failedObjectSize",
                      "currentSpeedBytesPerSecond", "averageSpeedBytesPerSecond",
                      "createTime", "startTime", "endTime", "errorCode", "creationStatus")
            def summary(row):
                if not isinstance(row, dict):
                    return None
                return {key: self.scrub(value)[:256] if isinstance(value, str) else value
                        for key in fields if key in row
                        for value in (row[key],) if value is None or type(value) in (str, int, float, bool)}
            try:
                diagnostic = {"taskId": task_id, "wanted": wanted, "timeoutSeconds": timeout,
                              "elapsedSeconds": round(time.monotonic() - started, 3),
                              "errorType": type(error).__name__, "task": summary(last_task),
                              "lastObservationSeconds": None if last_observed is None else round(last_observed - started, 3)}
                try:
                    # One small page, no retries; diagnostic collection must
                    # not turn a failed test into another long-running check.
                    data = self.api(f"/api/master/tasks/{task_id}/children?offset=0&limit=20",
                                    timeout=3, max_bytes=256 * 1024)
                    diagnostic["children"] = [summary(row) for row in data["items"][:20]]
                    total = data.get("totalCount")
                    diagnostic["childTotalCount"] = total if type(total) is int else None
                    diagnostic["childrenTruncated"] = type(total) is int and total > len(diagnostic["children"])
                except Exception as diagnostic_error:
                    diagnostic["childrenErrorType"] = type(diagnostic_error).__name__
                self.evidence["taskWaitFailure"] = diagnostic
                self.save()
            except Exception:
                pass  # Diagnostic failures must not replace the original failure.
            raise

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
        # Multi-Agent preflight writes and reads a probe in the existing shared directory.
        directory.mkdir(parents=True, exist_ok=True)
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
        registered = {node["address"]: node for node in self.api("/api/master/nodes")["nodes"]}
        require(set(registered) == set(self.nodes), "Report verification lost a registered Agent")
        labels = {}
        for address, node in registered.items():
            name, display_address = node["label"].strip(), node["displayAddress"]
            require(name and display_address, "Registered Agent is missing its name or display address")
            ipaddress.ip_address(display_address)
            labels[address] = name if name == display_address else f"{name} ({display_address})"
            require(node["displayLabel"] == labels[address], "Registered Agent display differs from its name and address")
        for child in children.values():
            owner = child["executionNodeIp"]
            require(owner in registered and child["executionNodeLabel"] == labels[owner],
                    "Child display differs from its assigned registered Agent")
            assignment = child["executionNodes"]
            require(len(assignment) == 1 and assignment[0]["id"] == registered[owner]["id"]
                    and assignment[0]["address"] == owner,
                    "Child task lost its assigned Agent enrollment identity")
        counts = {child_id: 0 for child_id in children}
        for key, expected in manifest.items():
            row = indexed[source_prefix + key]
            require("taskId" not in row and "shardId" not in row, "Report leaked obsolete task/shard IDs")
            require(row["parentTaskId"] == str(task_id) and row["direction"] == direction
                    and row["childTaskId"] in children
                    and row["result"] == "SUCCESS" and int(row["size"]) == expected["size"],
                    "Report row identity/result/size differs from the verified task")
            owner = children[row["childTaskId"]]["executionNodeIp"]
            require(row["nodeId"] == owner and row["node"] == labels[owner],
                    "Report row Agent identity or display differs from its actual child task owner")
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
        child_summaries = [row for row in summary if row["rowType"] == "CHILD_TASK"]
        require(len(child_summaries) == len(children)
                and {row["childTaskId"] for row in child_summaries} == set(children),
                "Report child summary has missing or duplicate rows")
        for row in child_summaries:
            child = children[row["childTaskId"]]
            owner = child["executionNodeIp"]
            require(row["parentTaskId"] == str(task_id) and row["direction"] == direction
                    and row["nodeId"] == owner and row["node"] == labels[owner]
                    and int(row["successCount"]) == child["putObjectCount"]
                    and int(row["successBytes"]) == child["putObjectSize"],
                    "Report child summary identity or totals differ from its actual child task")
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

    def rate_fixture(self, bandwidth=False):
        directory = self.root / ("bandwidth-fixture" if bandwidth else "rate-fixture")
        manifest = create_fixture(directory, rate=not bandwidth, bandwidth=bandwidth)
        prefix = "bandwidth/" if bandwidth else "rate/"
        def put(item):
            key, _ = item
            with (directory / key).open("rb") as body:
                self.s3.put_object(Bucket=self.buckets["rate"], Key=prefix + key, Body=body)
        with ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(put, manifest.items()))
        return directory, manifest

    def bandwidth(self, manifest):
        destinations = [self.root / f"limited-{index}" for index in (1, 2)]
        started = time.monotonic()
        sampler = GrowthSampler(destinations)
        try:
            with sampler:
                tasks = [self.create("/api/master/local-transfers", self.download_body(self.buckets["rate"], "bandwidth/", directory, 2),
                                     "bandwidth-" + str(index)) for index, directory in enumerate(destinations)]
                self.task_report(tasks[0], expected_status=409)
                for task_id in tasks:
                    self.wait_task(task_id, timeout=120)
        finally:
            preserving_error = sys.exc_info()[0] is not None
            try:
                (self.root / "bandwidth-samples.json").write_text(json.dumps(sampler.samples) + "\n", encoding="utf-8")
            except Exception:
                if not preserving_error:
                    raise
        total = sum(row["size"] for row in manifest.values())
        result = sampler.evaluate(250_000, [total, total])
        data = []
        for task_id, directory in zip(tasks, destinations):
            data.append(self.verify_task(task_id, manifest))
            self.verify_local(directory, "bandwidth", manifest)
            self.verify_report(task_id, manifest, "OSS_TO_LOCAL", "bandwidth/", "bandwidth/", target_directory=directory)
        self.record("two concurrent 2 Mbps parents share each limit across two Agents", tasks=data,
                    seconds=time.monotonic() - started, physicalByteWindows=result, runningReportHTTPStatus=409)

    def kill_restart(self, task_id, killed=None):
        identities = {node["address"]: node["id"] for node in self.api("/api/master/nodes")["nodes"]}
        self.master.kill()
        self.master.wait(timeout=10)
        self.agent_processes[0].kill()
        self.agent_processes[0].wait(timeout=10)
        if killed:
            killed()
        self.start_master()
        self.start_agent(0)
        nodes = eventually(self.online, "two Agents reconnect after process termination")
        require({node["address"]: node["id"] for node in nodes} == identities, "Restart changed enrollment identity")
        require(self.task(task_id) is not None, "Restart lost the durable parent task")

    def recovery(self, directory, manifest):
        rate = self.args.recovery_bandwidth_mbps
        body = self.upload_body(directory, self.buckets["source"], "resume/", rate, multi=not self.args.single_upload)
        task_id = self.create("/api/master/local-transfers", body, "stop-resume")
        eventually(lambda: len(self.list_objects(self.buckets["source"], "resume/")) >= 4,
                   "payload before stop", timeout=30)
        require_unfinished(self.task(task_id)["status"] in TERMINAL, 0, 1, "Stop", rate)
        started = time.monotonic()
        self.api("/api/master/stop", {"taskIds": [task_id]})
        try:
            stopped = self.wait_task(task_id, wanted="STOPPED", timeout=10)
        except AssertionError:
            require_unfinished(self.task(task_id)["status"] == "COMPLETED", 0, 1, "Stop", rate)
            raise
        elapsed = time.monotonic() - started
        require(elapsed < 8, f"Stop took {elapsed:.2f}s")
        before = self.list_objects(self.buckets["source"], "resume/")
        require_unfinished(False, len(before), len(manifest), "Stop", rate)
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
                    stopSeconds=elapsed, objectsCommittedAtStop=len(before), creditedBeforeResume=stopped["putObjectCount"],
                    taskBandwidthLimitMbps=rate)

        target = self.root / "crash-download"
        body = self.download_body(self.buckets["rate"], "rate/", target, rate)
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
        require_unfinished(self.task(task_id)["status"] in TERMINAL, 0, 1, "Crash", rate)
        # Inspect the disk between the kill and the restart: a fully published
        # target means the crash landed after the transfer, not during it.
        self.kill_restart(task_id, killed=lambda: require_unfinished(
            False, len(published_files(target / "rate", manifest)), len(manifest), "Crash", rate))
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
                    temporaryFilesBeforeCrash=incomplete, orphanTemporaryFiles=orphans, taskBandwidthLimitMbps=rate)

    def membership_fixture(self):
        """Many small children so a serialized Agent keeps queued work to hand off."""
        directory = self.root / "membership-fixture"
        directory.mkdir()
        manifest = {}
        for index in range(1024):
            key = f"{index:04d}.bin"
            block = hashlib.sha256(("membership:" + str(index)).encode()).digest()
            (directory / key).write_bytes(block * 1024)  # 32 KiB
            manifest[key] = {"size": 32 * 1024, "sha256": sha256(directory / key)}

        def put(key):
            with (directory / key).open("rb") as body:
                self.s3.put_object(Bucket=self.buckets["rate"], Key="members/" + key, Body=body)
        with ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(put, manifest))
        return manifest

    def nodes_view(self, task_id):
        return self.api(f"/api/master/tasks/{task_id}/nodes")

    def node_state(self, task_id, address):
        view = self.nodes_view(task_id)
        row = next((node for node in view["nodes"] if node["address"] == address), None)
        removed = any(node["address"] == address for node in view["removedNodes"])
        return view, (row["state"] if row else "REMOVED" if removed else None)

    def copy_body(self, prefix, nodes, rate):
        return {"sourceS3Alias": "cluster-qa", "sourceBucket": self.buckets["rate"], "destS3Alias": "cluster-qa",
                "destBucket": self.buckets["destination"], "prefix": "members/", "eligibleWorkerNodes": nodes,
                "distributionMode": "balanced", "conflictPolicy": "OVERWRITE", "taskBandwidthLimitMbps": rate,
                "keyMapping": {"mode": "REPLACE_PREFIX", "sourcePrefix": "members/", "targetPrefix": prefix}}

    def planned_task_errors(self, task_id, errors):
        """Sample the task while a requested node change is in progress.

        Every stage of a planned change (joining, draining, fencing, moving the
        scan) is progress in the node view. Any task error seen meanwhile is
        recorded; only a scrubbed, bounded excerpt is kept.
        """
        task = self.task(task_id)
        message = (task or {}).get("errorMessage") or ""
        if message and len(errors) < 5:
            errors.append(self.scrub(message)[:300])
        return task

    def require_no_planned_errors(self, errors, change):
        require(not errors, f"A planned node change ({change}) was reported as a task error: {errors}")

    def require_shares_within(self, view, rate):
        total = int(rate * 125000)
        shares = [node["share"]["bandwidthBytesPerSecond"] for node in view["nodes"] if node.get("share")]
        require(shares and all(value > 0 for value in shares) and sum(shares) <= total,
                f"Per-node bandwidth shares exceed the task limit: {shares} > {total}")
        return shares

    def dynamic_share_fixture(self):
        """One large object in the first dispatch batch, one small in the second."""
        directory = self.root / "dynamic-fixture"
        directory.mkdir()
        manifest = {}
        for index in range(129):
            # The scanner cuts 128-object batches: the large object's child goes
            # to one Agent, the small object's child to the other, which then
            # has nothing left while the large object is still transferring.
            size = 24 * MIB if index == 0 else 256 * 1024 if index == 128 else 0
            key = f"{index:04d}.bin"
            block = hashlib.sha256(("dynamic:" + str(index)).encode()).digest() * 2048
            with (directory / key).open("wb") as stream:
                remaining = size
                while remaining:
                    piece = block[:min(remaining, len(block))]
                    stream.write(piece)
                    remaining -= len(piece)
            manifest[key] = {"size": size, "sha256": sha256(directory / key)}

        def put(key):
            with (directory / key).open("rb") as body:
                self.s3.put_object(Bucket=self.buckets["rate"], Key="dynamic/" + key, Body=body)
        with ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(put, manifest))
        return manifest

    def dynamic_share(self, manifest=None):
        """An idle Agent releases its share of the task limit to the busy one.

        Every node-view sample must keep the shares within the limit. Once one
        Agent has finished its only child, the other must hold at least
        T * (1 - 1/64) - 1 bytes per second. Physical bytes written stay
        within the task limit (the bandwidth scenario's upper-bound check);
        throughput is recorded, never asserted.
        """
        manifest = manifest or self.dynamic_share_fixture()
        rate = self.args.recovery_bandwidth_mbps
        limit = int(rate * 125000)
        destination = self.root / "dynamic-share"
        total = sum(row["size"] for row in manifest.values())
        started = time.monotonic()
        sampler = GrowthSampler([destination])
        views = []
        try:
            with sampler:
                task_id = self.create("/api/master/local-transfers",
                                      self.download_body(self.buckets["rate"], "dynamic/", destination, rate), "dynamic-share")

                def released():
                    view = self.nodes_view(task_id)
                    shares = self.require_shares_within(view, rate)
                    nodes = [node for node in view["nodes"] if node.get("share")]
                    views.append({"seconds": round(time.monotonic() - started, 2), "allocation": view.get("allocation"),
                                  "nodes": [{"address": node["address"], "demand": node.get("demand"),
                                             "adjusting": node.get("adjusting"), "pendingChildren": node.get("pendingChildren"),
                                             "bandwidthBytesPerSecond": node["share"]["bandwidthBytesPerSecond"]} for node in nodes]})
                    busy = [node for node in nodes if node.get("demand") == "busy"]
                    idle = [node for node in nodes if node.get("demand") == "idle"]
                    if (view.get("allocation") == "dynamic" and len(nodes) == 2 and len(busy) == 1 and len(idle) == 1
                            and not idle[0].get("adjusting")
                            and busy[0]["share"]["bandwidthBytesPerSecond"] >= limit * (1 - 1 / 64) - 1):
                        return view, shares
                    return None

                view, shares = eventually(released, "the idle Agent's share moves to the busy Agent", timeout=90)
                status = self.task(task_id)["status"]
                require(status not in TERMINAL,
                        f"Dynamic share transfer finished before the share moved at {rate} Mbps; lower RECOVERY_BANDWIDTH_MBPS")
                busy = next(node for node in view["nodes"] if node.get("demand") == "busy")

                def finished():
                    self.require_shares_within(self.nodes_view(task_id), rate)
                    row = self.task(task_id)
                    return row if row and row["status"] in TERMINAL else None
                eventually(finished, "dynamic share transfer completes", timeout=180)
        finally:
            preserving_error = sys.exc_info()[0] is not None
            try:
                (self.root / "dynamic-share-samples.json").write_text(
                    json.dumps({"bytes": sampler.samples, "views": views}) + "\n", encoding="utf-8")
            except Exception:
                if not preserving_error:
                    raise
        allowance = bandwidth_allowance(limit)
        windows = []
        samples = sampler.samples
        for first in range(0, len(samples), 5):
            start = samples[first]
            if not 0 < start[1] < total * 0.65:
                continue
            end = next((row for row in samples[first + 1:] if row[0] - start[0] >= 5), None)
            if end is None or end[1] >= total:
                continue
            elapsed, amount = end[0] - start[0], end[1] - start[1]
            require(amount <= limit * elapsed + allowance,
                    f"Dynamic share task exceeded its bandwidth limit: {amount} bytes/{elapsed:.2f}s")
            windows.append({"seconds": elapsed, "bytes": amount, "bytesPerSecond": amount / elapsed})
        require(windows, "Dynamic share task produced no useful 5-second bandwidth window")
        require(max(row["bytesPerSecond"] for row in windows) > limit * 0.45,
                "Bandwidth upper-bound check was vacuous: transfer made too little progress")
        data = self.verify_task(task_id, manifest, both=False)
        self.verify_local(destination, "dynamic", manifest)
        self.record("an idle Agent releases its share of the task limit to the busy Agent, never exceeding the limit",
                    **data, taskBandwidthLimitMbps=rate, busyAgent=busy["address"], sharesAfterRelease=shares,
                    nodeViewSamples=len(views), physicalByteWindows=windows,
                    seconds=time.monotonic() - started)

    def node_membership(self, manifest=None):
        manifest = manifest or self.membership_fixture()
        rate = self.args.recovery_bandwidth_mbps
        first, second = self.nodes
        # One child at a time per Agent leaves never-started children queued,
        # which is exactly the work the handoff protocol may move.
        for node in self.nodes:
            self.api("/api/master/nodes/" + node, {"maxConcurrentChildren": 1}, method="PUT")
        try:
            task_id = self.create("/api/master/add-buckets", self.copy_body("members-added/", [first], rate), "members-add")
            eventually(lambda: (lambda row: row and row["status"] not in TERMINAL and row["putObjectCount"] >= 1
                                and row["childCount"] >= 3)(self.task(task_id)),
                       "payload on the first Agent before adding the second", timeout=60)
            view = self.nodes_view(task_id)
            require(view["editable"] and [node["address"] for node in view["nodes"]] == [first],
                    f"Unexpected initial membership: {view}")
            require(any(row["address"] == second and row["eligible"] for row in view["candidates"]),
                    "The second Agent was not offered as an eligible candidate")
            change = {"requestId": "members-add-1", "add": [second], "expectedNodeRevision": view["nodeRevision"]}
            accepted = self.api(f"/api/master/tasks/{task_id}/nodes", change)
            require(accepted["accepted"] and not accepted["replayed"], f"Add was not accepted: {accepted}")
            replay = self.api(f"/api/master/tasks/{task_id}/nodes", change)
            require(replay["replayed"] and replay["view"]["nodeRevision"] == accepted["view"]["nodeRevision"],
                    "Replayed node change was applied twice")
            errors = []
            joined, _ = eventually(lambda: self.planned_task_errors(task_id, errors) and
                                   (lambda pair: pair if pair[1] == "ACTIVE" else None)(self.node_state(task_id, second)),
                                   "added Agent joins the running task", timeout=90)
            # The first steps after admission prepare the joiner; they are not a fault either.
            for _ in range(6):
                self.planned_task_errors(task_id, errors)
                time.sleep(0.25)
            self.require_no_planned_errors(errors, "add")
            shares = self.require_shares_within(joined, rate)
            require(len(shares) == 2, f"Joined task does not divide its limit between two Agents: {shares}")
            status = self.task(task_id)["status"]
            require(status not in TERMINAL,
                    f"Add test transfer finished before the second Agent joined at {rate} Mbps; lower RECOVERY_BANDWIDTH_MBPS")
            data = self.verify_task(task_id, manifest, both=False)
            require(all(data["bytesByNode"].get(node, 0) > 0 for node in self.nodes),
                    "Both Agents must transfer objects after the second joined: " + str(data["bytesByNode"]))
            self.verify_s3(self.buckets["destination"], "members-added/", manifest)
            self.record("adding an Agent mid-run joins placement, splits the bandwidth limit and keeps exactly-once accounting",
                        **data, sharesAfterJoin=shares, taskBandwidthLimitMbps=rate, plannedChangeErrors=0)

            task_id = self.create("/api/master/add-buckets", self.copy_body("members-drained/", self.nodes, rate), "members-drain")
            eventually(lambda: (lambda row: row and row["status"] not in TERMINAL and row["putObjectCount"] >= 1
                                and row["childCount"] >= 3)(self.task(task_id)),
                       "payload on both Agents before draining one", timeout=60)
            view = self.nodes_view(task_id)
            removal = self.api(f"/api/master/tasks/{task_id}/nodes",
                               {"requestId": "members-drain-1", "remove": [second], "mode": "drain",
                                "expectedNodeRevision": view["nodeRevision"]})
            require(removal["accepted"], f"Drain was not accepted: {removal}")
            states = {self.node_state(task_id, second)[1]}
            errors = []
            left = eventually(lambda: self.planned_task_errors(task_id, errors) and
                              (lambda pair: pair if pair[1] == "REMOVED" else states.add(pair[1]))(self.node_state(task_id, second)),
                              "draining Agent leaves the task", timeout=90)
            for _ in range(6):
                self.planned_task_errors(task_id, errors)
                time.sleep(0.25)
            self.require_no_planned_errors(errors, "drain")
            status = self.task(task_id)["status"]
            require(status not in TERMINAL,
                    f"Drain test transfer finished before the Agent left at {rate} Mbps; lower RECOVERY_BANDWIDTH_MBPS")
            shares = self.require_shares_within(left[0], rate)
            require(len(shares) == 1, f"Remaining Agent did not take the whole limit: {shares}")
            data = self.verify_task(task_id, manifest, both=False)
            self.verify_s3(self.buckets["destination"], "members-drained/", manifest)
            task = self.task(task_id)
            require([node["address"] for node in task.get("executionNodes", [])] == [first],
                    "A removed Agent is still listed as an execution node")
            self.record("draining an Agent mid-run hands off its queue, lets it leave and completes with exact accounting",
                        **data, observedStates=sorted(state for state in states if state), sharesAfterLeave=shares,
                        taskBandwidthLimitMbps=rate, plannedChangeErrors=0)

            # Remove the scanning Agent immediately: its running work and, if the
            # scan is still serving batches, the scan move to the other Agent.
            task_id = self.create("/api/master/add-buckets", self.copy_body("members-scanner/", self.nodes, rate), "members-scanner")
            eventually(lambda: (lambda row: row and row["status"] not in TERMINAL and row["putObjectCount"] >= 1
                                and row["childCount"] >= 3)(self.task(task_id)),
                       "payload on both Agents before removing the scanner", timeout=60)
            view = self.nodes_view(task_id)
            scanners = [node["address"] for node in view["nodes"] if node.get("scanner")]
            phase = (view.get("scan") or {}).get("phase")
            require(scanners == [first] or (not scanners and phase == "COMPLETE"),
                    f"The scanning Agent is not labelled: {scanners}, phase {phase}")
            removal = self.api(f"/api/master/tasks/{task_id}/nodes",
                               {"requestId": "members-scanner-1", "remove": [first], "mode": "now",
                                "expectedNodeRevision": view["nodeRevision"]})
            require(removal["accepted"], f"Scanner removal was not accepted: {removal}")
            errors, labels = [], set()
            def scanner_left():
                self.planned_task_errors(task_id, errors)
                current, state = self.node_state(task_id, first)
                phase = (current.get("scan") or {}).get("phase")
                marked = tuple(node["address"] for node in current["nodes"] if node.get("scanner"))
                labels.add((phase, marked))
                # The removal is committed before the scan moves; in between the
                # view reports WAITING (no member scans yet). Wait past it.
                return current if state == "REMOVED" and phase != "WAITING" else None
            left = eventually(scanner_left, "scanning Agent leaves the task and the scan settles", timeout=90)
            for _ in range(6):
                self.planned_task_errors(task_id, errors)
                time.sleep(0.25)
            self.require_no_planned_errors(errors, "remove the scanner now")
            phase = (left.get("scan") or {}).get("phase")
            marked = [node["address"] for node in left["nodes"] if node.get("scanner")]
            require(marked == ([] if phase == "COMPLETE" else [second]),
                    f"After the scanner left, the view marks {marked} in phase {phase}")
            data = self.verify_task(task_id, manifest, both=False)
            self.verify_s3(self.buckets["destination"], "members-scanner/", manifest)
            final = self.nodes_view(task_id)
            require((final.get("scan") or {}).get("phase") == "COMPLETE" and
                    not [node for node in final["nodes"] if node.get("scanner")],
                    "A completed scan still marks a scanning Agent")
            self.record("removing the scanning Agent mid-run moves its work and scan without a task error",
                        **data, scanPhasesSeen=sorted({phase or "" for phase, _ in labels}), plannedChangeErrors=0,
                        taskBandwidthLimitMbps=rate)
        finally:
            for node in self.nodes:
                self.api("/api/master/nodes/" + node, {"maxConcurrentChildren": 32}, method="PUT")

    def close(self):
        for child in reversed(self.children):
            if child.poll() is None:
                # The S3 fixture is disposable and its filer spends about ten
                # seconds in a graceful stop; Agents and Master stop gracefully.
                if child is self.s3_process:
                    child.kill()
                else:
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


def wait_for_exclusive(gate, run_id, timeout=1200):
    """Require this run's other groups to exit before measuring bandwidth."""
    # The runner names the groups that share this machine: Go tests and the
    # installer may run in jobs of their own.
    allowed = {"unit", "control", "install"}

    def ready():
        try:
            state = json.loads(gate.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            # The runner replaces the gate atomically; on Windows a read can
            # race the replacement. Poll again rather than fail.
            return None
        require(state.get("runId") == run_id, "Exclusive transfer gate belongs to another run")
        groups = state.get("requiredGroups", [])
        require(isinstance(groups, list) and groups and len(groups) == len(set(groups)) and set(groups) <= allowed,
                "Exclusive transfer gate lists invalid required groups")
        require(state.get("status") in ("WAITING", "READY"), "Another acceptance group failed before exclusive transfer")
        if state["status"] == "WAITING":
            return None
        require(all(state.get("groups", {}).get(name) == "PASSED" for name in groups),
                "Exclusive transfer started before every other group passed")
        return state

    return eventually(ready, "exclusive bandwidth and recovery window", timeout)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("master", "agent", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--weed", type=Path, help="Verified local S3 fixture binary; omitted with OSS_TRANSFER_QA_S3_CONFIG")
    parser.add_argument("--scenario", choices=SCENARIOS, default="all",
                        help="core: the three transfer directions; transfer: core, then bandwidth and demand-based "
                             "shares; recovery: stop/restart and crash recovery; nodes: execution node changes; "
                             "dynamic_share: demand-based shares only; all: every scenario in one cluster")
    parser.add_argument("--fixture", type=Path, help="Read an existing Java/Go shared fixture; never modify it")
    parser.add_argument("--multipart-recovery", action="store_true", help="Run the real S3 crash-recovery integration test using the current source")
    parser.add_argument("--multipart-test-binary", type=Path, help="Use a precompiled target-platform Agent test executable; requires --multipart-recovery")
    parser.add_argument("--single-upload", action="store_true", help="Use one Agent for upload until shared-root validation is ready")
    parser.add_argument("--exclusive-gate", type=Path, help="Wait for the platform runner's other groups before bandwidth and recovery")
    parser.add_argument("--exclusive-run-id", help="Identity of this platform run's gate")
    parser.add_argument("--recovery-bandwidth-mbps", type=int, default=16,
                        help="Task cap for the stop and crash recovery transfers; lower it if they finish before injection")
    args = parser.parse_args()
    if bool(args.exclusive_gate) != bool(args.exclusive_run_id) or (args.exclusive_gate and args.scenario not in MEASURED):
        parser.error("Exclusive transfer requires a gate, its run ID, and a measured scenario: " + ", ".join(MEASURED))
    if not args.weed and not os.environ.get("OSS_TRANSFER_QA_S3_CONFIG"):
        parser.error("--weed is required without OSS_TRANSFER_QA_S3_CONFIG")
    if args.multipart_test_binary and not args.multipart_recovery:
        parser.error("--multipart-test-binary requires --multipart-recovery")
    if args.recovery_bandwidth_mbps < 1:
        parser.error("--recovery-bandwidth-mbps must be positive")
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
        steps = {"core": ("core",), "transfer": ("core", "bandwidth", "dynamic_share"),
                 "recovery": ("recovery",), "nodes": ("nodes",), "dynamic_share": ("dynamic_share",),
                 "all": ("core", "bandwidth", "dynamic_share", "recovery", "nodes")}[args.scenario]
        if "core" in steps:
            h.node_settings()
            h.core()
        # Fixture uploads are plain storage puts, not measurements; finish
        # them while other groups still run.
        if "bandwidth" in steps:
            _, bandwidth_manifest = h.rate_fixture(bandwidth=True)
        if "dynamic_share" in steps:
            dynamic_manifest = h.dynamic_share_fixture()
        if "recovery" in steps:
            directory, manifest = h.rate_fixture()
        if "nodes" in steps:
            membership_manifest = h.membership_fixture()
        if args.exclusive_gate:
            h.evidence["exclusiveWindow"] = wait_for_exclusive(args.exclusive_gate, args.exclusive_run_id)
            h.save()
        if "bandwidth" in steps:
            h.bandwidth(bandwidth_manifest)
        if "dynamic_share" in steps:
            h.dynamic_share(dynamic_manifest)
        if "recovery" in steps:
            h.recovery(directory, manifest)
        if "nodes" in steps:
            h.node_membership(membership_manifest)
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
