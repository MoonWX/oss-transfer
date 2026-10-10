#!/usr/bin/env python3
"""Exercise built release artifacts against real processes and S3 storage."""
import argparse
import csv
import hashlib
import http.client
import io
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import time
import traceback
import urllib.error
import urllib.request
import uuid
import zipfile

import boto3
from botocore.config import Config

from scenarios import upload, validate_browser_copy, download_recovery, conflicts


# Official SeaweedFS multi-architecture release, not a mutable latest tag.
S3_IMAGE = "chrislusf/seaweedfs@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d"
ROOT = Path(__file__).resolve().parents[2]


def sha256(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


class Harness:
    def __init__(self, image, evidence, quick=False, startup_timeout=120):
        self.quick = quick
        self.startup_timeout = startup_timeout
        self.image = image
        self.evidence = Path(evidence).resolve()
        self.evidence.mkdir(parents=True, exist_ok=True)
        self.temp = Path(tempfile.mkdtemp(prefix="osstransfer-acceptance-"))
        self.temp.chmod(0o755)
        self.data = self.temp / "data"
        self.data.mkdir(mode=0o777)
        self.data.chmod(0o777)
        self.source_dir = self.data / "source"
        self.download_dir = self.data / "download"
        for directory in (self.source_dir, self.download_dir):
            directory.mkdir(mode=0o777)
            directory.chmod(0o777)
        self.prefix = "oss-acceptance-" + uuid.uuid4().hex[:12]
        self.network = self.prefix
        self.master = self.prefix + "-master"
        self.agents = [self.prefix + "-agent-1", self.prefix + "-agent-2"]
        self.cli_name = self.prefix + "-cli"
        self.workers = ["198.51.100.20", "198.51.100.21"]
        if quick:
            self.workers = self.workers[:1]
            self.agents = self.agents[:1]
        self.names, self.volumes, self.events = [], [], []
        self.access_key = "acceptance-" + secrets.token_hex(8)
        self.secret_key = secrets.token_urlsafe(32)
        self.password = self.token = ""
        self.source_bucket, self.target_bucket = "acceptance-source", "acceptance-target"
        self.sensitive = [self.access_key, self.secret_key]
        self.started = time.monotonic()

    def clean_text(self, text):
        for value in self.sensitive:
            if value:
                text = text.replace(value, "[redacted]")
        return text

    def run(self, *args, timeout=180, check=True, **kwargs):
        result = subprocess.run(args, text=True, capture_output=True, timeout=timeout, **kwargs)
        if check and result.returncode:
            raise RuntimeError(self.clean_text("Command failed: " + " ".join(args[:3]) + "\n" + result.stderr[-5000:] + result.stdout[-2000:]))
        return result

    def docker(self, *args, **kwargs):
        return self.run("docker", *args, **kwargs).stdout.strip()

    def record(self, name, details=None):
        self.events.append({"check": name, "elapsedSeconds": round(time.monotonic() - self.started, 3), "details": details})
        (self.evidence / "checks.json").write_text(self.clean_text(json.dumps(self.events, indent=2, ensure_ascii=False)))
        print("PASS " + name, flush=True)

    def wait(self, check, description, timeout=120):
        deadline = time.monotonic() + timeout
        last = ""
        while time.monotonic() < deadline:
            try:
                value = check()
                if value:
                    return value
            except (urllib.error.URLError, ConnectionError, TimeoutError, http.client.RemoteDisconnected) as error:
                last = str(error)
            time.sleep(0.5)
        raise AssertionError("Timed out: " + description + ("; " + last if last else ""))

    def request(self, method, path, body=None, expected=200, authenticated=True):
        headers = {"Content-Type": "application/json"}
        if authenticated and self.token:
            headers["Authorization"] = "Bearer " + self.token
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(self.origin + path, data=data, headers=headers, method=method)
        try:
            response = urllib.request.urlopen(request, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            content = response.read()
            if response.status != expected:
                raise AssertionError(self.clean_text("{} {} returned {}, expected {}: {}".format(method, path, response.status, expected, content[:1500].decode(errors="replace"))))
        return content

    def api(self, method, path, body=None):
        return json.loads(self.request(method, path, body))

    def login(self):
        result = json.loads(self.request("POST", "/api/auth/login", {"username": "admin", "password": self.password}, authenticated=False))
        self.token = result["token"]
        self.sensitive.append(self.token)
        if self.cli_name in self.names:
            result = self.run("docker", "exec", "-i", "-e", "OSS_TRANSFER_MASTER_SERVER=http://master:1685", self.cli_name,
                              "/opt/oss-transfer/oss-transfer-cli", "--json", "login", "--username", "admin", "--password-stdin", input=self.password + "\n")
            json.loads(result.stdout)

    def cli(self, *args):
        result = self.run("docker", "exec", "-e", "OSS_TRANSFER_MASTER_SERVER=http://master:1685", self.cli_name,
                          "/opt/oss-transfer/oss-transfer-cli", "--json", *map(str, args))
        return json.loads(result.stdout)

    def task(self, task_id):
        result = self.api("GET", "/api/master/status?includeSubtasks=false")
        if result.get("partial") or result.get("unavailableWorkers"):
            raise AssertionError("Incomplete cluster task status: " + json.dumps(result))
        rows = [row for row in result.get("tasks", []) if int(row["taskIndex"]) == int(task_id)]
        if len(rows) != 1:
            raise AssertionError("Expected exactly one public task {}: {}".format(task_id, rows))
        return rows[0]

    def wait_task(self, task_id, timeout=180):
        def finished():
            row = self.task(task_id)
            if row["status"] in ("FAILED", "PARTIAL_FAILED", "STOPPED"):
                raise AssertionError("Task failed to complete: " + json.dumps(row))
            if row["status"] == "COMPLETED":
                if row.get("failedObjectCount", 0):
                    raise AssertionError("Completed task contains failures: " + json.dumps(row))
                return row
            return False
        return self.wait(finished, "task {} completes".format(task_id), timeout)

    def report(self, task_id):
        payload = self.request("GET", "/api/master/task-reports/{}".format(task_id))
        path = self.evidence / "report-{}.zip".format(task_id)
        path.write_bytes(payload)
        with zipfile.ZipFile(io.BytesIO(payload)) as archive:
            return {"metadata": json.loads(archive.read("report-metadata.json")),
                    "rows": list(csv.DictReader(io.StringIO(archive.read("file-results.csv").decode("utf-8-sig"))))}

    def port(self, name, internal):
        detail = json.loads(self.docker("inspect", name))[0]
        return detail["NetworkSettings"]["Ports"][str(internal) + "/tcp"][0]["HostPort"]

    def container(self, name, args, command=()):
        # Track before creation as Docker can create a container but fail to start it.
        self.names.append(name)
        return self.docker("run", "-d", "--name", name, "--label", "osstransfer.acceptance=" + self.prefix,
                           "--network", self.network, *args, *command)

    def connected(self):
        snapshots = self.api("GET", "/api/master/worker-status").get("workers", {})
        nodes = self.api("GET", "/api/master/nodes")["data"]["nodes"]
        return all(any(n.get("address") == address and n.get("online") is True for n in nodes)
                   and snapshots.get(address, {}).get("success") is True for address in self.workers)

    def stop_agent(self, index, kill=True):
        self.docker("kill" if kill else "stop", self.agents[index])

    def start_agent(self, index):
        self.docker("start", self.agents[index])
        self.wait(self.connected, "agent reconnects", 120)

    def restart_master(self, kill=True):
        self.docker("kill" if kill else "stop", self.master)
        self.docker("start", self.master)
        self.origin = "http://127.0.0.1:" + self.port(self.master, 1685)
        self.wait(lambda: self.request("GET", "/api/auth/current", authenticated=False), "master restarts")
        self.login()
        self.wait(self.connected, "cluster reconnects")

    def manifest(self):
        lengths = {"00-small.txt": 4096, "01-empty": 0, "02-中文 空格.bin": 3073, "90-large.bin": 12 * 1024 * 1024}
        lengths.update({"10-medium-{}.bin".format(i): 6 * 1024 * 1024 for i in range(4)})
        # OSS balanced dispatch splits at 128 objects. Put equal payload on both
        # sides of that real boundary instead of merely declaring two workers.
        lengths.update({"10-medium-2.bin-padding-{:03d}".format(i): 1024 for i in range(122)})
        if self.quick:
            lengths = {"arm64-probe.bin": 65536}
        self.source_manifest = {}
        for name, length in lengths.items():
            path = self.source_dir / name
            # Deterministic, non-uniform data; expected hashes originate outside the app.
            block = hashlib.shake_256(name.encode()).digest(65536)
            with path.open("wb") as stream:
                for offset in range(0, length, len(block)):
                    stream.write(block[:min(len(block), length - offset)])
            path.chmod(0o644)
            self.source_manifest[name] = {"size": length, "sha256": sha256(path)}
        (self.evidence / "expected-files.json").write_text(json.dumps(self.source_manifest, ensure_ascii=False, indent=2))

    def assert_bucket(self, bucket, prefix=""):
        actual, multipart = {}, {}
        for page in self.s3.get_paginator("list_objects_v2").paginate(Bucket=bucket):
            for obj in page.get("Contents", []):
                if not obj["Key"].startswith(prefix):
                    raise AssertionError("Unexpected key outside destination prefix: " + obj["Key"])
                body = self.s3.get_object(Bucket=bucket, Key=obj["Key"])["Body"]
                digest, size = hashlib.sha256(), 0
                try:
                    for chunk in body.iter_chunks(1024 * 1024):
                        digest.update(chunk)
                        size += len(chunk)
                finally:
                    body.close()
                actual[obj["Key"][len(prefix):]] = {"size": size, "sha256": digest.hexdigest()}
                # This pinned S3 implementation preserves multipart ETags. Check
                # service output, rather than assuming our threshold was applied.
                if obj["Size"] > 8 * 1024 * 1024:
                    etag = obj["ETag"].strip('"')
                    if "-" not in etag or not etag.rsplit("-", 1)[1].isdigit() or int(etag.rsplit("-", 1)[1]) < 2:
                        raise AssertionError("Large object did not use multipart: " + obj["Key"])
                    multipart[obj["Key"]] = etag
        (self.evidence / (bucket + "-actual.json")).write_text(json.dumps(actual, ensure_ascii=False, indent=2))
        if actual != self.source_manifest:
            raise AssertionError("S3 keys, sizes or content hashes differ: " + bucket)
        self.record("independent S3 content verification", {"bucket": bucket, "prefix": prefix, "files": len(actual), "multipartETags": multipart})

    def assert_download(self):
        actual = {}
        for path in self.download_dir.rglob("*"):
            if path.is_file():
                actual[path.relative_to(self.download_dir).as_posix()] = {"size": path.stat().st_size, "sha256": hashlib.sha256(self.read_file(path)).hexdigest()}
        expected = {"browser/" + key: value for key, value in self.source_manifest.items()}
        (self.evidence / "download-actual.json").write_text(json.dumps(actual, ensure_ascii=False, indent=2))
        if actual != expected:
            raise AssertionError("Download contains missing, extra, partial or corrupted files")
        self.record("independent downloaded file verification", {"files": len(actual)})

    def read_file(self, path):
        path = Path(path)
        relative = path.relative_to(self.data)
        try:
            return path.read_bytes()
        except PermissionError:
            # Linux preserves the Agent's UID and private file permissions. The
            # independent helper has a read-only data mount; do not chmod output
            # or change the identity/permissions of the application under test.
            result = subprocess.run(["docker", "exec", "--user", "0", self.cli_name,
                                     "cat", "/data/" + relative.as_posix()],
                                    capture_output=True, timeout=30)
            if result.returncode:
                raise AssertionError("Cannot read acceptance file: " + self.clean_text(result.stderr.decode(errors="replace")))
            return result.stdout

    def setup(self):
        self.manifest()
        self.docker("network", "create", "--label", "osstransfer.acceptance=" + self.prefix, self.network)
        s3_name = self.prefix + "-s3"
        self.container(s3_name, ["--network-alias", "s3", "-p", "127.0.0.1::8333",
                                "-e", "AWS_ACCESS_KEY_ID=" + self.access_key, "-e", "AWS_SECRET_ACCESS_KEY=" + self.secret_key,
                                S3_IMAGE], ["mini", "-dir=/data", "-s3.port=8333", "-admin.ui=false", "-webdav=false"])
        self.s3 = boto3.client("s3", endpoint_url="http://127.0.0.1:" + self.port(s3_name, 8333),
                               aws_access_key_id=self.access_key, aws_secret_access_key=self.secret_key, region_name="us-east-1",
                               config=Config(signature_version="s3v4", s3={"addressing_style": "path"}, retries={"max_attempts": 0}, connect_timeout=3, read_timeout=30))
        # Catch only transient connectivity while the real S3 process starts.
        from botocore.exceptions import EndpointConnectionError, ConnectionClosedError, ReadTimeoutError, ClientError
        def storage_ready():
            try:
                return self.s3.list_buckets()
            except (EndpointConnectionError, ConnectionClosedError, ReadTimeoutError):
                return False
        self.wait(storage_ready, "S3 signed API readiness")
        rejected = boto3.client("s3", endpoint_url=self.s3.meta.endpoint_url,
                               aws_access_key_id=self.access_key, aws_secret_access_key="invalid-secret",
                               region_name="us-east-1", config=Config(signature_version="s3v4", retries={"max_attempts": 0}))
        try:
            rejected.list_buckets()
        except ClientError as error:
            if error.response["ResponseMetadata"]["HTTPStatusCode"] != 403:
                raise
        else:
            raise AssertionError("S3 accepted an invalid request signature")
        for bucket in (self.source_bucket, self.target_bucket):
            self.s3.create_bucket(Bucket=bucket)
        config = self.temp / "application.yml"
        config.write_text("\n".join([
            "thread-count.src.explore: 2", "thread-count.src.list: 2", "thread-count.dest.put: 4", "thread-count.dest.part: 2",
            "local-transfer.small-file-concurrency: 4", "local-transfer.batch.max-items: 2", "local-transfer.batch.max-bytes: 4194304",
            "src.list.max-keys: 2", "src.list.batch-size: 2", "buffer-queue.list.size: 128", "buffer-queue.put.size: 128",
            "buffer-queue.progress.size: 128", "buffer-queue.failed.size: 128", "buffer-queue.fallback.size: 128",
            "dest.multipart.threshold: 8388608", "dest.multipart.partsize: 5242880", "dest.multipart.max-in-flight-parts-per-object: 2",
            "connection-pool.max-connections: 32", "connection-pool.socket-timeout: 15000", "rate-limit.auto-adjust.interval-ms: 10000", ""
        ]))
        config.chmod(0o644)
        def common(name):
            volume = name + "-state"
            self.volumes.append(volume)
            initializer = name + "-init"
            self.names.append(initializer)
            self.docker("run", "--rm", "--name", initializer,
                        "--label", "osstransfer.acceptance=" + self.prefix,
                        "--entrypoint", "/bin/sh", "--user", "0",
                        "-v", volume + ":/state", "-v", str(config) + ":/acceptance.yml:ro", self.image,
                        "-c", "mkdir -p /state/config && cp /acceptance.yml /state/config/application.yml && chown -R 10001:10001 /state")
            # Successful --rm already removed it. On failure/timeout the exact
            # name remains registered so close() can remove a surviving helper.
            self.names.remove(initializer)
            return ["-e", "JAVA_OPTS=-Xms64m -Xmx384m", "-v", volume + ":/app/data",
                    "-v", str(self.data) + ":/data"]
        self.container(self.master, common(self.master) + ["--network-alias", "master", "-p", "127.0.0.1::1685",
                        "-e", "TRANSFER_MASTER_WEB_URL=http://master:1685", self.image])
        self.origin = "http://127.0.0.1:" + self.port(self.master, 1685)
        self.wait(lambda: self.request("GET", "/api/auth/current", authenticated=False), "master readiness", self.startup_timeout)
        self.password = self.docker("exec", self.master, "cat", "/app/data/secrets/admin_password")
        self.sensitive.append(self.password)
        self.login()
        self.request("GET", "/api/master/status", expected=401, authenticated=False)
        for index, address in enumerate(self.workers):
            invite = self.api("POST", "/api/master/agents/invitations", {"address": address, "label": "acceptance-agent-" + str(index + 1)})
            join_token = invite["joinUrl"].rsplit("/", 1)[1]
            self.sensitive.append(join_token)
            self.container(self.agents[index], common(self.agents[index]) + ["-e", "TRANSFER_ROLE=agent",
                           "-e", "TRANSFER_AGENT_MASTER_URL=http://master:1685", "-e", "TRANSFER_AGENT_JOIN_TOKEN=" + join_token,
                           "-e", "TRANSFER_AGENT_ADDRESS=" + address, self.image])
        self.wait(self.connected, "configured agents return live status", self.startup_timeout)
        # Run the packaged CLI itself, not the wrapper that loads the internal token.
        self.container(self.cli_name, ["--entrypoint", "/bin/sleep", "-e", "XDG_CONFIG_HOME=/tmp/cli-config",
                                      "-v", str(self.data) + ":/data:ro", self.image], ["infinity"])
        self.login()
        self.cli("whoami")
        result = self.api("POST", "/api/master/s3-config", {"alias": "ci", "endpoint": "http://s3:8333",
                           "accessKey": self.access_key, "secretKey": self.secret_key, "validate": True,
                           "useV2SignerType": False, "pathStyleEnable": True})
        if not result.get("success") or result.get("failedCount", 0) or result.get("partial"):
            raise AssertionError("S3 configuration failed to synchronize to every node: " + self.clean_text(json.dumps(result)))
        if not self.connected():
            raise AssertionError("S3 configuration reported success before every Agent was ready")
        self.record("real login, unauthorized API rejection and agent enrollment", {"agents": len(self.workers), "invalidS3SignatureRejected": True})

    def browser(self):
        result_path = self.temp / "browser-result.json"
        env = dict(os.environ, ACCEPTANCE_BASE_URL=self.origin, ACCEPTANCE_PASSWORD=self.password,
                   ACCEPTANCE_SOURCE_BUCKET=self.source_bucket, ACCEPTANCE_TARGET_BUCKET=self.target_bucket,
                   ACCEPTANCE_S3_ALIAS="ci", ACCEPTANCE_WORKERS=json.dumps(self.workers), ACCEPTANCE_RESULT=str(result_path),
                   ACCEPTANCE_EVIDENCE=str(self.evidence / "browser"))
        result = self.run("npm", "run", "test:acceptance", cwd=str(ROOT / "frontend"), env=env, timeout=180, check=False)
        (self.evidence / "browser.log").write_text(self.clean_text(result.stdout + result.stderr))
        if result.returncode:
            raise AssertionError("Real browser flow failed; see browser.log")
        details = json.loads(result_path.read_text())
        start = time.monotonic() - (time.time() - details["startedAtUnixMs"] / 1000)
        self.record("packaged WebUI created exactly one real OSS migration", details)
        completed = start + (details["completedAtUnixMs"] - details["startedAtUnixMs"]) / 1000
        return details["taskId"], start, completed

    def diagnostics(self):
        errors = []
        for name in self.names:
            try:
                result = self.run("docker", "logs", "--tail", "2000", name, check=False, timeout=20)
                (self.evidence / (name.removeprefix(self.prefix + "-") + ".log")).write_text(self.clean_text(result.stdout + result.stderr))
            except Exception as error:
                errors.append(name + ": " + str(error))
        if self.token:
            for name, endpoint in [("tasks", "/api/master/status"), ("nodes", "/api/master/nodes")]:
                try:
                    (self.evidence / (name + ".json")).write_text(self.clean_text(json.dumps(self.api("GET", endpoint), indent=2)))
                except Exception as error:
                    errors.append(name + ": " + str(error))
        if errors:
            message = self.clean_text("\n".join(errors))
            print("Diagnostic collection incomplete: " + message, flush=True)
            try:
                (self.evidence / "diagnostic-errors.log").write_text(message)
            except Exception:
                pass  # Diagnostics never replace the original test failure.

    def close(self):
        errors = []

        def remove(*args):
            try:
                result = self.run("docker", *args, check=False, timeout=30)
                if result.returncode:
                    errors.append("docker " + " ".join(args) + ": " + result.stderr.strip())
            except Exception as error:
                errors.append("docker " + " ".join(args) + ": " + str(error))

        for name in reversed(self.names):
            # -v also removes anonymous volumes declared by the S3/CLI images.
            remove("rm", "-f", "-v", name)
        for volume in self.volumes:
            remove("volume", "rm", volume)
        remove("network", "rm", self.network)
        try:
            shutil.rmtree(self.temp)
        except Exception as error:
            errors.append(str(self.temp) + ": " + str(error))
        if errors:
            message = self.clean_text("\n".join(errors))
            print("Acceptance cleanup incomplete: " + message, flush=True)
            try:
                (self.evidence / "cleanup-errors.log").write_text(message)
            except Exception:
                pass  # Try every owned resource without replacing a test error.


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--evidence", default="output/acceptance")
    parser.add_argument("--quick", action="store_true", help="One-agent real file upload for the second architecture")
    parser.add_argument("--startup-timeout", type=int, default=120, help="Seconds allowed for initial Master and Agent readiness")
    args = parser.parse_args()
    if args.startup_timeout <= 0:
        parser.error("--startup-timeout must be positive")
    h = Harness(args.image, args.evidence, args.quick, args.startup_timeout)
    try:
        h.setup()
        if args.quick:
            response = h.cli("task", "create", "local-to-oss", "ci/" + h.source_bucket,
                             "--binding", h.workers[0] + "=/data/source")
            data = response.get("data", response)
            if data.get("acceptedCount") != 1 or data.get("rejectedCount", 0) or len(data.get("parentTaskIds", [])) != 1:
                raise AssertionError("Second-architecture task was not uniquely accepted")
            h.wait_task(data["parentTaskIds"][0])
            h.assert_bucket(h.source_bucket)
            h.record("second-architecture actual file transfer complete")
            return
        upload(h)
        task_id, started, completed = h.browser()
        validate_browser_copy(h, task_id, started, completed)
        download_recovery(h)
        conflicts(h)
        h.record("acceptance complete")
    except Exception as error:
        failure = h.clean_text(traceback.format_exc())
        (h.evidence / "failure.txt").write_text(failure)
        print(failure, flush=True)
        raise SystemExit(1)
    finally:
        try:
            h.diagnostics()
        finally:
            h.close()


if __name__ == "__main__":
    main()
