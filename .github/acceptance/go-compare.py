#!/usr/bin/env python3
"""Compare released Java tasks with native Go Master/Agent tasks on one host.

Requires the acceptance boto3 environment and psutil==7.1.3. Both applications
use the same isolated SeaweedFS 4.48 process. Output includes fixture hashes,
artifact hashes, each measured run, process samples and unrounded summaries.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import random
import secrets
import shutil
import statistics
import subprocess
import tarfile
import threading
import time
from types import SimpleNamespace
from urllib.request import Request, urlopen
from urllib.error import HTTPError, URLError

import psutil


SPEC = importlib.util.spec_from_file_location("cluster_qa", Path(__file__).with_name("go-cluster.py"))
cluster = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(cluster)
MIB = 1024 * 1024
require = cluster.require
sha256 = cluster.sha256


def unpack_files(archive, destination, names):
    destination.mkdir(parents=True)
    result = {}
    with tarfile.open(archive, "r:gz") as package:
        for name in names:
            matches = [member for member in package.getmembers() if Path(member.name).name == name and member.isfile()]
            require(len(matches) == 1, "Archive must contain exactly one " + name)
            with package.extractfile(matches[0]) as source, (destination / name).open("wb") as target:
                shutil.copyfileobj(source, target)
            result[name] = destination / name
    return result


class Sampler:
    def __init__(self, groups):
        self.groups = {name: [psutil.Process(child.pid) for child in children] for name, children in groups.items()}
        self.samples = []
        self.stop = threading.Event()
        interfaces = psutil.net_io_counters(pernic=True)
        self.loopback = next((name for name in ("lo0", "lo") if name in interfaces), None)

    def snapshot(self):
        row = {"elapsedSeconds": time.monotonic() - self.started}
        for name, processes in self.groups.items():
            cpu, rss = 0.0, 0
            for process in processes:
                with process.oneshot():
                    value = process.cpu_times()
                    cpu += value.user + value.system
                    rss += process.memory_info().rss
            row[name] = {"cpuSeconds": cpu, "rssBytes": rss}
        if self.loopback:
            row["loopbackBytesSent"] = psutil.net_io_counters(pernic=True)[self.loopback].bytes_sent
        return row

    def loop(self):
        while not self.stop.wait(0.05):
            self.samples.append(self.snapshot())

    def __enter__(self):
        self.started = time.monotonic()
        self.samples.append(self.snapshot())
        self.thread = threading.Thread(target=self.loop, daemon=True)
        self.thread.start()
        return self

    def __exit__(self, *_):
        self.stop.set()
        self.thread.join(timeout=10)
        require(not self.thread.is_alive(), "Process sampler did not stop")
        self.samples.append(self.snapshot())

    def summary(self):
        seconds = self.samples[-1]["elapsedSeconds"]
        return {name: {"cpuSeconds": self.samples[-1][name]["cpuSeconds"] - self.samples[0][name]["cpuSeconds"],
                       "meanCPUPercent": 100 * (self.samples[-1][name]["cpuSeconds"] - self.samples[0][name]["cpuSeconds"]) / seconds,
                       "sampledPeakRSSBytes": max(row[name]["rssBytes"] for row in self.samples),
                       "startingRSSBytes": self.samples[0][name]["rssBytes"]} for name in self.groups}

    def network_summary(self):
        if not self.loopback:
            return None
        bits = 8 * (self.samples[-1]["loopbackBytesSent"] - self.samples[0]["loopbackBytesSent"])
        rates = [8 * (after["loopbackBytesSent"] - before["loopbackBytesSent"]) /
                 (after["elapsedSeconds"] - before["elapsedSeconds"])
                 for before, after in zip(self.samples, self.samples[1:])]
        return {"interface": self.loopback, "bytesSent": bits // 8,
                "meanBitsPerSecond": bits / self.samples[-1]["elapsedSeconds"],
                "maxSampleWindowBitsPerSecond": max(rates),
                "scope": "Entire host loopback: S3 GET and PUT, control traffic and unrelated local traffic; sent bytes only"}


class ComparisonHarness(cluster.Harness):
    def spawn(self, name, binary, arguments, environment=None):
        environment = dict(environment or self.environment)
        limit = self.args.seaweed_memory_limit if name == "seaweedfs" else self.args.go_memory_limit
        if limit:
            environment["GOMEMLIMIT"] = limit
        return super().spawn(name, binary, arguments, environment)


class Comparison:
    def __init__(self, args):
        # The offline V4 fixture recorder imports this class with its original
        # option set. Keep those callers on the original benchmark defaults.
        for name, default in {"profile": "standard", "java_max_heap_mib": 1536,
                              "go_memory_limit": "", "seaweed_memory_limit": "",
                              "engines": ["java", "go"], "paired_component": False,
                              "repetition_start": 1, "task_timeout": 300,
                              "poll_interval": 0.1}.items():
            if not hasattr(args, name):
                setattr(args, name, default)
        self.args = args
        self.root = args.output.resolve()
        self.root.mkdir(mode=0o700, parents=True, exist_ok=False)
        shutil.copyfile(Path(__file__), self.root / "harness-source.py")
        self.bandwidth_bits = int(args.node_bandwidth_mbps * 1_000_000)
        self.secrets, self.logs = [], []
        self.java, self.java_token = None, ""
        artifacts = self.root / "artifacts"
        baseline = unpack_files(args.java_package, artifacts / "java", ["oss-transfer.jar", "VERSION"])
        current = unpack_files(args.go_package, artifacts / "go", ["oss-transfer-master", "oss-transfer-agent", "VERSION", "SHA256SUMS"])
        checksums = {Path(line.split()[1]).name: line.split()[0] for line in current["SHA256SUMS"].read_text().splitlines() if line.strip()}
        for role in ("master", "agent"):
            path = current["oss-transfer-" + role]
            require(sha256(path) == checksums[path.name], "Go binary differs from package checksum: " + role)
            path.chmod(0o700)
        self.jar = baseline["oss-transfer.jar"]
        self.h = ComparisonHarness(SimpleNamespace(master=current["oss-transfer-master"], agent=current["oss-transfer-agent"],
                                                 weed=args.weed, output=self.root / "runtime", fixture=None,
                                                 scenario="comparison", single_upload=True,
                                                 go_memory_limit=args.go_memory_limit, seaweed_memory_limit=args.seaweed_memory_limit))
        self.h.nodes = ["compare-agent"]
        self.result = {"status": "PREPARING", "measurement": "Full task submission through terminal status; source discovery, scheduling and durable bookkeeping included",
                       "platform": platform.platform(), "logicalCPUs": psutil.cpu_count(), "psutilVersion": psutil.__version__,
                       "smokeOnly": args.smoke, "parameters": {"repetitions": args.repetitions, "objectConcurrencyCeiling": 16,
                           "partConcurrencyPerObject": 4, "partSizeBytes": 8 * MIB, "listQPS": 100, "putQPS": 1400,
                           "headQPS": 2800, "bandwidthBitsPerSecond": self.bandwidth_bits, "taskBandwidthLimit": None,
                           "directions": args.directions, "fixtureSelection": args.fixtures, "engines": args.engines,
                           "pairedComponent": args.paired_component, "taskTimeoutSeconds": args.task_timeout,
                           "javaHeapInitialMiB": 128, "javaHeapMaximumMiB": args.java_max_heap_mib,
                           "goMemoryLimitOverride": args.go_memory_limit or None,
                           "seaweedMemoryLimitOverride": args.seaweed_memory_limit or None, "fixtureProfile": args.profile,
                           "fixtureContent": "Deterministic pseudorandom bytes, independently SHA-256 verified",
                           "conflictPolicy": "OVERWRITE", "pollIntervalSeconds": args.poll_interval, "RSSSamplingSeconds": 0.05},
                       "limits": ["Single host and loopback S3; not JDCloud or cross-host network evidence",
                                  "Java has one global 16-object pool; Go has four child jobs with four object workers each",
                                  "JVM warmup and OS caches are not reset; run order alternates each repetition",
                                  "RSS is the maximum observed at 50 ms intervals, not an exact allocator high-water mark",
                                  "Loopback maximum is a sampled interval rate, not a packet-level instantaneous peak",
                                  "CPU/RSS for Go sum its Master and Agent; SeaweedFS is reported separately",
                                  "Integrity verification and fixture seeding occur outside the timed task window"],
                       "artifacts": {"javaPackage": {"path": str(args.java_package.resolve()), "sha256": sha256(args.java_package), "version": baseline["VERSION"].read_text()},
                                     "goPackage": {"path": str(args.go_package.resolve()), "sha256": sha256(args.go_package), "version": current["VERSION"].read_text()},
                                     "javaJarSHA256": sha256(self.jar), "goMasterSHA256": sha256(current["oss-transfer-master"]),
                                     "goAgentSHA256": sha256(current["oss-transfer-agent"]),
                                     "javaRuntime": cluster.store_qa.binary_provenance(args.java, "-version"),
                                     "seaweedfs": cluster.store_qa.binary_provenance(args.weed, "version")},
                       "fixtures": {}, "warmups": [], "runs": []}
        self.save()
        require(" 4.48 " in self.result["artifacts"]["seaweedfs"]["version"], "Comparison requires SeaweedFS 4.48")

    def save(self):
        (self.root / "results.json").write_text(json.dumps(self.result, indent=2) + "\n")

    def java_api(self, path, body=None):
        headers = {"Content-Type": "application/json"}
        if self.java_token:
            headers["Authorization"] = "Bearer " + self.java_token
        request = Request(self.java_origin + path, data=None if body is None else json.dumps(body).encode(), headers=headers)
        with urlopen(request, timeout=30) as response:
            result = json.load(response)
        require(result.get("success") is not False and not result.get("partial"), "Java API failed: " + path)
        data = result.get("data", result)
        require(isinstance(data, dict) and data.get("success") is not False and not data.get("failedCount") and not data.get("partial"),
                "Java API returned incomplete data: " + path)
        return data

    def start(self):
        self.h.start_s3()
        self.weed = self.h.children[0]
        self.h.start_master(first=True)
        self.h.start_agent(0, first=True)
        cluster.eventually(lambda: any(node["online"] for node in self.h.api("/api/master/nodes")["nodes"]), "Go Agent online")
        self.h.api("/api/master/s3-config", self.h.storage)
        self.start_java()
        self.result["processes"] = {"javaMasterWorkerPID": self.java.pid, "goMasterPID": self.h.master.pid,
                                    "goAgentPID": self.h.agent_processes[0].pid, "seaweedfsPID": self.weed.pid}
        self.save()
        for engine, api, node in (("go", self.h.api, self.h.nodes[0]), ("java", self.java_api, "127.0.0.1")):
            for kind, value in (("list", 100), ("put", 1400), ("head", 2800), ("bandwidth", self.bandwidth_bits)):
                api("/api/master/rate-limit/" + kind + "/" + node, {"qps": value})
            settings = api("/api/master/worker-status")
            limits = settings["workers"][node]["rateLimit"]
            for kind, value in (("list", 100), ("put", 1400), ("head", 2800), ("bandwidth", self.bandwidth_bits)):
                require(limits[kind]["limitBps" if kind == "bandwidth" else "limitQps"] == value,
                        engine + " effective " + kind + " limit differs from comparison settings")
            (self.root / (engine + "-worker-settings.json")).write_text(json.dumps(settings, indent=2) + "\n")

    def start_java(self):
        directory = self.root / "java-runtime"
        directory.mkdir(mode=0o700)
        port = cluster.store_qa.reserve_ports(1)[0]
        self.java_origin = "http://127.0.0.1:" + str(port)
        password, internal = secrets.token_urlsafe(24), secrets.token_urlsafe(32)
        self.secrets.extend([password, internal])
        settings = {"server.address": "127.0.0.1", "server.port": port, "transfer.port": port,
                    "transfer.local-ip": "127.0.0.1", "transfer.master-ip": "127.0.0.1", "transfer.worker-ips": ["127.0.0.1"],
                    "transfer.role": "master", "transfer.master-web-url": self.java_origin,
                    "thread-count.src.explore": 2, "thread-count.src.list": 2, "thread-count.dest.put": 16,
                    "thread-count.dest.part": 64, "local-transfer.small-file-concurrency": 16,
                    "local-transfer.batch.max-items": 128, "local-transfer.batch.max-bytes": 256 * MIB,
                    "src.list.max-keys": 1000, "src.list.batch-size": 128, "buffer-queue.list.size": 2048,
                    "buffer-queue.put.size": 2048, "buffer-queue.progress.size": 2048,
                    "buffer-queue.failed.size": 256, "buffer-queue.fallback.size": 256,
                    "dest.multipart.threshold": 8 * MIB, "dest.multipart.partsize": 8 * MIB,
                    "dest.multipart.max-in-flight-parts-per-object": 4, "connection-pool.max-connections": 80,
                    "rate-limit.src.qps.list": 100, "rate-limit.dest.qps.put": 1400,
                    "rate-limit.dest.qps.head": 2800, "rate-limit.bandwidth": self.bandwidth_bits, "transfer.cleanMetadata": False}
        (directory / "application.yml").write_text("\n".join(key + ": " + json.dumps(value) for key, value in settings.items()) + "\n")
        (self.root / "java-settings.json").write_text(json.dumps(settings, indent=2) + "\n")
        self.java_log = self.root / "java.log"
        stream = self.java_log.open("w")
        self.logs.append(stream)
        self.java = subprocess.Popen([str(self.args.java.resolve()), "-Xms128m", f"-Xmx{self.args.java_max_heap_mib}m", "-jar", str(self.jar)],
                                     cwd=directory, env=dict(os.environ, OSS_TRANSFER_BOOTSTRAP_PASSWORD=password,
                                                           OSS_TRANSFER_INTERNAL_TOKEN=internal), stdout=stream, stderr=subprocess.STDOUT)
        def ready():
            require(self.java.poll() is None, "Java process exited; inspect java.log")
            try:
                # The unauthenticated legacy endpoint deliberately returns
                # success:false; login is checked separately after readiness.
                with urlopen(self.java_origin + "/api/auth/current", timeout=2) as response:
                    json.load(response)
                return True
            except (URLError, TimeoutError):
                return False
        cluster.eventually(ready, "Java release readiness", 90)
        self.java_token = self.java_api("/api/auth/login", {"username": "admin", "password": password})["token"]
        self.secrets.append(self.java_token)
        self.java_api("/api/master/s3-config", self.h.storage)

    def fixture(self, name, sizes):
        directory = self.root / "fixtures" / name
        directory.mkdir(parents=True)
        manifest = {}
        for index, size in enumerate(sizes):
            key = f"{index:05d}.bin"
            path = directory / key
            generator = random.Random(hashlib.sha256((name + ":" + str(index)).encode()).digest())
            digest = hashlib.sha256()
            with path.open("wb") as target:
                remaining = size
                while remaining:
                    data = generator.randbytes(min(remaining, 64 * 1024))
                    target.write(data)
                    digest.update(data)
                    remaining -= len(data)
            manifest[key] = {"size": size, "sha256": digest.hexdigest()}
        prefix = name + "/"
        def seed(item):
            key, _ = item
            with (directory / key).open("rb") as source:
                self.h.s3.put_object(Bucket=self.h.buckets["source"], Key=prefix + key, Body=source)
        # A long component starts with a fresh S3 directory. Keep only the
        # source representation used by this direction to bound QA disk usage.
        needs_s3_source = self.args.profile != "cloud-long" or (name != "warmup" and any(
            direction != "LOCAL_TO_OSS" for direction in self.args.directions))
        if needs_s3_source:
            with ThreadPoolExecutor(max_workers=8) as pool:
                list(pool.map(seed, manifest.items()))
        (self.root / (name + "-manifest.json")).write_text(json.dumps(manifest, indent=2) + "\n")
        self.result["fixtures"][name] = {"files": len(manifest), "bytes": sum(item["size"] for item in manifest.values()),
                                           "manifestSHA256": sha256(self.root / (name + "-manifest.json")),
                                           "seededS3Source": needs_s3_source}
        if self.args.profile == "cloud-long" and name != "warmup" and "LOCAL_TO_OSS" not in self.args.directions:
            shutil.rmtree(directory)
        self.save()
        return directory, manifest

    def body(self, engine, direction, directory, name, prefix):
        node = self.h.nodes[0] if engine == "go" else "127.0.0.1"
        local = {"type": "LOCAL", "nodeBindings": [{"executionNodeIp": node, "localPath": str(directory)}]}
        source = {"type": "OSS", "s3Alias": "cluster-qa", "bucket": self.h.buckets["source"], "prefix": name + "/"}
        destination = {"type": "OSS", "s3Alias": "cluster-qa", "bucket": self.h.buckets["destination"], "prefix": prefix}
        common = {"distributionMode": "balanced", "conflictPolicy": "OVERWRITE"}
        if direction == "OSS_TO_OSS":
            return "/api/master/add-buckets", dict(common, sourceS3Alias="cluster-qa", sourceBucket=source["bucket"],
                destS3Alias="cluster-qa", destBucket=destination["bucket"], prefix=name + "/", eligibleWorkerNodes=[node],
                keyMapping={"mode": "REPLACE_PREFIX", "sourcePrefix": name + "/", "targetPrefix": prefix})
        return "/api/master/local-transfers", dict(common, direction=direction,
            sourceEndpoint=local if direction == "LOCAL_TO_OSS" else source,
            destinationEndpoint=destination if direction == "LOCAL_TO_OSS" else local)

    def create(self, api, path, body):
        response = api(path, [body])
        ids = response.get("parentTaskIds", response.get("taskIds"))
        if ids is None:
            creation_ids = response.get("creationIds", [])
            require(len(creation_ids) == 1, "Expected exactly one asynchronous Java task creation")
            def assigned():
                value = api("/api/master/task-creations/" + creation_ids[0])["creation"]
                require(value.get("status") != "FAILED", "Java task creation failed")
                return value.get("publicTaskId")
            return int(cluster.eventually(assigned, "Java task ID"))
        require(len(ids) == 1 and not response.get("rejectedCount"), "Expected exactly one logical task")
        return int(ids[0])

    def wait(self, api, task_id):
        deadline = time.monotonic() + self.args.task_timeout
        next_progress = time.monotonic() + 30
        while time.monotonic() < deadline:
            tasks = api("/api/master/status?includeSubtasks=false")["tasks"]
            task = next((row for row in tasks if int(row["taskIndex"]) == task_id), None)
            require(task is not None, "Created task is absent from status")
            if task["status"] in cluster.TERMINAL:
                require(task["status"] == "COMPLETED" and task.get("failedObjectCount", 0) == 0,
                        "Measured task ended unsuccessfully: " + str(task.get("status")))
                return task
            if time.monotonic() >= next_progress:
                print(f"PROGRESS task {task_id}: {task['status']}, migrated {task.get('putObjectCount', 0)}, scanned {task.get('listObjectCount', 0)}", flush=True)
                next_progress = time.monotonic() + 30
            time.sleep(self.args.poll_interval)
        raise RuntimeError("Measured task timed out")

    def run_once(self, engine, direction, name, directory, manifest, repetition, warmup=False):
        api = self.h.api if engine == "go" else self.java_api
        prefix = f"{name}/{direction}/{engine}/{repetition}/"
        target = self.root / "downloads" / prefix if direction == "OSS_TO_LOCAL" else directory
        if direction == "OSS_TO_LOCAL":
            target.mkdir(parents=True)
        path, body = self.body(engine, direction, target, name, prefix)
        group = [self.h.master, self.h.agent_processes[0]] if engine == "go" else [self.java]
        load = list(os.getloadavg()) if hasattr(os, "getloadavg") else None
        with Sampler({"application": group, "seaweedfs": [self.weed]}) as sampler:
            started = time.monotonic()
            task_id = self.create(api, path, body)
            task = self.wait(api, task_id)
            elapsed = time.monotonic() - started
        print(f"VERIFY {name}-{direction}-{engine}-{repetition}: {len(manifest)} destination objects", flush=True)
        if direction == "OSS_TO_LOCAL":
            require(not self.h.verify_local(target, name, manifest), "Temporary destination files remain")
        else:
            self.h.verify_s3(self.h.buckets["destination"], prefix, manifest)
        total = sum(item["size"] for item in manifest.values())
        require(task.get("putObjectCount") == len(manifest) and task.get("putObjectSize") == total,
                "Task counters differ from independently verified content")
        run = {"engine": engine, "direction": direction, "fixture": name, "repetition": repetition, "taskId": task_id,
               "objects": len(manifest), "bytes": total, "wallSeconds": elapsed, "MiBPerSecond": total / MIB / elapsed,
               "objectsPerSecond": len(manifest) / elapsed, "resources": sampler.summary(), "loadAverageBefore": load,
               "loopback": sampler.network_summary(),
               "sha256VerifiedObjects": len(manifest), "taskStart": task.get("startTime"), "taskEnd": task.get("endTime")}
        key = f"{name}-{direction}-{engine}-{repetition}"
        (self.root / (key + "-samples.json")).write_text(json.dumps(sampler.samples, indent=2) + "\n")
        (self.root / (key + "-task.json")).write_text(json.dumps(task, indent=2) + "\n")
        self.result["warmups" if warmup else "runs"].append(run)
        self.save()
        print(f"{'WARMUP' if warmup else 'PASS'} {key}: {elapsed:.3f}s, {run['MiBPerSecond']:.2f} MiB/s, SHA256 {len(manifest)} files", flush=True)
        if direction == "OSS_TO_LOCAL" and not self.args.keep_runtime:
            shutil.rmtree(target)

    def execute(self):
        self.start()
        directory, manifest = self.fixture("warmup", [32 * 1024] * 10 + [16 * MIB])
        for engine in ("java", "go"):
            self.run_once(engine, "LOCAL_TO_OSS", "warmup", directory, manifest, 0, warmup=True)
        if self.args.smoke:
            fixtures = [("smoke", [32 * 1024] * 9 + [16 * MIB])]
        elif self.args.profile == "cloud-small":
            fixtures = [("small", [16 * 1024] * 2048), ("large", [16 * MIB] * 8),
                        ("mixed", [32 * 1024] * 512 + [MIB] * 32 + [16 * MIB] * 2)]
            fixtures = [(name, sizes) for name, sizes in fixtures if name in self.args.fixtures]
        elif self.args.profile == "cloud-long":
            fixtures = [("small", [4 * 1024] * 100000), ("large", [48 * MIB] * 16)]
            fixtures = [(name, sizes) for name, sizes in fixtures if name in self.args.fixtures]
        else:
            fixtures = [("small", [16 * 1024] * 1024), ("large", [64 * MIB] * 16),
                        ("mixed", [16 * MIB if index % 64 == 0 else 16 * 1024 for index in range(1024)])]
            fixtures = [(name, sizes) for name, sizes in fixtures if name in self.args.fixtures]
        self.result["status"] = "RUNNING"
        for name, sizes in fixtures:
            directory, manifest = self.fixture(name, sizes)
            for direction in self.args.directions:
                for repetition in range(self.args.repetition_start, self.args.repetition_start + self.args.repetitions):
                    for engine in [item for item in (("java", "go") if repetition % 2 else ("go", "java")) if item in self.args.engines]:
                        self.run_once(engine, direction, name, directory, manifest, repetition)
        self.result["summary"] = []
        for name, _ in fixtures:
            for direction in self.args.directions:
                summary = {"fixture": name, "direction": direction}
                for engine in self.args.engines:
                    runs = [run for run in self.result["runs"] if run["engine"] == engine and run["fixture"] == name and run["direction"] == direction]
                    summary[engine] = {"medianWallSeconds": statistics.median(run["wallSeconds"] for run in runs),
                        "medianMiBPerSecond": statistics.median(run["MiBPerSecond"] for run in runs),
                        "medianCPUSeconds": statistics.median(run["resources"]["application"]["cpuSeconds"] for run in runs),
                        "maxSampledRSSBytes": max(run["resources"]["application"]["sampledPeakRSSBytes"] for run in runs)}
                if "go" in summary and "java" in summary:
                    summary["goToJavaMedianWallRatio"] = summary["go"]["medianWallSeconds"] / summary["java"]["medianWallSeconds"]
                self.result["summary"].append(summary)
        self.result["status"] = "PASSED"
        self.save()

    def close(self):
        if self.java is not None and self.java.poll() is None:
            self.java.terminate()
            try:
                self.java.wait(timeout=20)
            except subprocess.TimeoutExpired:
                self.java.kill()
                self.java.wait(timeout=10)
        for stream in self.logs:
            stream.close()
        self.h.close()
        self.h.evidence["status"] = self.result["status"]
        self.h.evidence["comparisonResults"] = "../results.json"
        self.h.save()
        if hasattr(self, "java_log") and self.java_log.exists():
            cluster.store_qa.redact_log(self.java_log, self.secrets + self.h.secrets)
        if not self.args.keep_runtime:
            for relative in ("fixtures", "downloads", "java-runtime", "runtime/seaweed-data", "runtime/master-data", "runtime/agent-0-data"):
                path = self.root / relative
                if path.is_dir():
                    shutil.rmtree(path)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("java", "java-package", "go-package", "weed", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--repetitions", type=int, default=3)
    parser.add_argument("--node-bandwidth-mbps", type=int, default=2000)
    parser.add_argument("--profile", choices=("standard", "cloud-small", "cloud-long"), default="standard")
    parser.add_argument("--java-max-heap-mib", type=int, default=1536)
    parser.add_argument("--go-memory-limit", default="")
    parser.add_argument("--seaweed-memory-limit", default="")
    parser.add_argument("--engines", nargs="+", choices=("java", "go"), default=["java", "go"])
    parser.add_argument("--paired-component", action="store_true", help="One independent component of an externally paired experiment; not a full comparison")
    parser.add_argument("--repetition-start", type=int, default=1)
    parser.add_argument("--task-timeout", type=int, default=300)
    parser.add_argument("--poll-interval", type=float, default=0.1)
    parser.add_argument("--fixtures", nargs="+", choices=("small", "large", "mixed"), default=["small", "large", "mixed"])
    parser.add_argument("--directions", nargs="+", choices=("LOCAL_TO_OSS", "OSS_TO_OSS", "OSS_TO_LOCAL"),
                        default=["LOCAL_TO_OSS", "OSS_TO_OSS", "OSS_TO_LOCAL"])
    parser.add_argument("--smoke", action="store_true", help="Ten-file API compatibility check; not formal performance evidence")
    parser.add_argument("--keep-runtime", action="store_true")
    args = parser.parse_args()
    require(args.repetitions >= (1 if args.smoke or args.paired_component else 3), "Formal comparison requires at least three repetitions")
    require(args.repetition_start > 0 and args.task_timeout > 0 and args.poll_interval > 0, "Repetition index and timeouts must be positive")
    require(args.paired_component or set(args.engines) == {"java", "go"}, "Single-engine execution requires --paired-component")
    require(args.profile != "cloud-long" or (args.paired_component and len(args.directions) == 1 and len(args.engines) == 1 and args.repetitions == 1 and len(args.fixtures) == 1 and args.fixtures[0] in ("small", "large")),
            "Long cloud workload requires a single independently cleaned component")
    require(args.node_bandwidth_mbps > 0, "Node bandwidth must be positive")
    require(args.java_max_heap_mib >= 128, "Java maximum heap must be at least its initial 128 MiB heap")
    comparison = Comparison(args)
    try:
        comparison.execute()
    except BaseException as error:
        comparison.result["status"] = "FAILED"
        comparison.result["error"] = comparison.h.scrub(str(error))
        comparison.save()
        raise
    finally:
        comparison.close()
    print("Evidence: " + str(comparison.root), flush=True)


if __name__ == "__main__":
    main()
