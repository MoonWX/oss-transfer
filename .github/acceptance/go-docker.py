#!/usr/bin/env python3
"""Exercise a Go candidate image with isolated Docker volumes and real S3 I/O.

Supply a verified Linux weed binary matching the image architecture. This test
requires Docker and the pinned acceptance requirements; it does not publish an
image, access cloud credentials, or modify existing services.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen

import boto3
from botocore.config import Config


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def eventually(check, description, timeout=60):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (URLError, ConnectionError, TimeoutError):
            pass
        time.sleep(0.2)
    raise AssertionError("Timed out: " + description)


class Acceptance:
    def __init__(self, args):
        self.args = args
        self.root = args.output.resolve()
        self.root.mkdir(parents=True, mode=0o700, exist_ok=False)
        self.prefix = "oss-go-qa-" + secrets.token_hex(6)
        self.network = self.prefix + "-network"
        self.containers, self.volumes, self.secrets = [], [], []
        self.token, self.origin = "", ""
        self.evidence = {"checks": [], "image": args.image, "weedSha256": sha256(args.weed)}

    def scrub(self, text):
        for value in self.secrets:
            text = text.replace(value, "[redacted]")
        return re.sub(r"Setup token: \S+", "Setup token: [redacted]", text)

    def docker(self, *arguments, env=None, timeout=60, check=True):
        result = subprocess.run(["docker", *map(str, arguments)], env=env, text=True,
                                capture_output=True, timeout=timeout)
        if check and result.returncode:
            raise RuntimeError("Docker " + str(arguments[0]) + " failed: " + self.scrub(result.stderr))
        return result.stdout.strip()

    def record(self, name, **details):
        self.evidence["checks"].append({"name": name, **details})
        self.save()
        print("PASS " + name, flush=True)

    def save(self):
        (self.root / "evidence.json").write_text(self.scrub(json.dumps(self.evidence, indent=2)) + "\n", encoding="utf-8")

    def volume(self, role):
        name = self.prefix + "-" + role + "-data"
        self.docker("volume", "create", name)
        self.volumes.append(name)
        return name

    def start(self, role, arguments=(), mounts=(), environment=None, port=None, volume=None):
        name = self.prefix + "-" + role
        self.containers.append(name)
        command = ["run", "-d", "--name", name, "--network", self.network, "--network-alias", role,
                   "--read-only", "--tmpfs", "/tmp:mode=1777", "--cap-drop", "ALL", "--security-opt", "no-new-privileges"]
        if volume:
            command += ["-v", volume + ":/data"]
        for mount in mounts:
            command += ["-v", mount]
        if port:
            command += ["-p", "127.0.0.1::" + str(port)]
        for key in environment or {}:
            command += ["-e", key]
        self.docker(*command, self.args.image, *arguments, env=dict(os.environ, **(environment or {})))
        return name

    def origin_for(self, name, port):
        binding = self.docker("port", name, str(port) + "/tcp").splitlines()[0]
        return "http://" + binding

    def api(self, path, body=None, key=None):
        headers = {"Content-Type": "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        if key:
            headers["Idempotency-Key"] = key
        request = Request(self.origin + path, data=None if body is None else json.dumps(body).encode(), headers=headers)
        with urlopen(request, timeout=30) as response:
            payload = json.load(response)
        require(payload.get("success") is not False, "API returned failure: " + path)
        result = payload.get("data", payload)
        require(not result.get("partial") and not result.get("failed") and not result.get("failedCount"), "API result was incomplete: " + path)
        return result

    def online(self):
        nodes = self.api("/api/master/nodes")["nodes"]
        return nodes[0] if len(nodes) == 1 and nodes[0]["online"] else None

    def create_task(self, body, key):
        result = self.api("/api/master/local-transfers", [body], key=key)
        ids = result.get("parentTaskIds", [])
        require(len(ids) == 1 and not result.get("rejectedCount"), "Expected one task")
        return ids[0]

    def wait_task(self, task_id, count, size):
        def complete():
            task = self.api("/api/master/tasks/" + str(task_id))
            if task["status"] in ("FAILED", "PARTIAL_FAILED", "STOPPED"):
                raise AssertionError("Transfer ended as " + task["status"])
            if task["status"] != "COMPLETED":
                return None
            require(task["putObjectCount"] == count and task["putObjectSize"] == size and not task["failedObjectCount"], "Completed task counters differ from real fixture")
            return task
        return eventually(complete, "transfer completion", 150)

    def run(self):
        inspect = json.loads(self.docker("image", "inspect", self.args.image))[0]
        require(inspect["Os"] == "linux", "Candidate must be a Linux image")
        require(inspect["Config"]["User"] == "65532:65532", "Candidate must run as UID/GID 65532")
        require(not inspect["Config"].get("Entrypoint"), "Entrypoint prevents Agent/CLI commands")
        self.evidence.update(imageId=inspect["Id"], architecture=inspect["Architecture"])
        cli = json.loads(self.docker("run", "--rm", "--network", "none", self.args.image, "oss-transfer-cli", "--version", "--json"))
        require(bool(cli.get("version")), "Packaged CLI did not start")
        self.docker("network", "create", self.network)
        master_volume, agent_volume, weed_volume = (self.volume(role) for role in ("master", "agent", "weed"))
        master = self.start("master", port=1685, volume=master_volume)
        self.origin = self.origin_for(master, 1685)
        provenance_path = self.root / "image-provenance.json"
        self.docker("cp", master + ":/usr/local/share/oss-transfer/provenance.json", provenance_path)
        provenance = json.loads(provenance_path.read_text(encoding="utf-8"))
        for row in provenance["binaries"]:
            if row["platform"] == "linux/" + inspect["Architecture"]:
                binary = self.root / ("image-oss-transfer-" + row["program"])
                self.docker("cp", master + ":/usr/local/bin/oss-transfer-" + row["program"], binary)
                require(sha256(binary) == row["sha256"], "Image program differs from verified native archive")
                binary.unlink()
        require(cli["version"] == provenance["version"], "CLI version differs from native package manifest")
        if os.environ.get("GITHUB_SHA"):
            labels = inspect["Config"].get("Labels", {})
            require(provenance["commit"] == os.environ["GITHUB_SHA"] and
                    labels.get("org.opencontainers.image.revision") == provenance["commit"] and
                    labels.get("org.opencontainers.image.version") == provenance["version"],
                    "Image release identity differs from the tested commit")
        self.evidence["release"] = {"version": provenance["version"], "commit": provenance["commit"]}
        status = eventually(lambda: self.api("/api/setup/status"), "Master setup API")
        require(not status["configured"] and status["tokenRequired"], "Fresh Master skipped setup")
        html = urlopen(self.origin, timeout=10).read().decode()
        require('<div id="root"' in html, "Embedded setup UI missing")
        token = eventually(lambda: re.search(r"Setup token: ([A-Za-z0-9_-]+)", self.docker("logs", master)), "setup token")[1]
        credentials = {"username": "docker-qa", "password": secrets.token_urlsafe(24)}
        self.secrets += [token, credentials["password"]]
        self.api("/api/setup/complete", dict(credentials, setupToken=token, publicUrl="http://master:1685"))
        self.token = self.api("/api/auth/login", credentials)["token"]
        self.secrets.append(self.token)
        self.record("nonroot image serves initial setup and packaged CLI")
        invite = self.api("/api/master/agents/invitations", {"address": "docker-agent", "label": "Docker QA"})["inviteToken"]
        self.secrets.append(invite)
        self.docker("run", "--rm", "--network", self.network, "-e", "OSS_TRANSFER_JOIN_TOKEN", "-v", agent_volume + ":/data",
                    self.args.image, "oss-transfer-agent", "--master-url", "http://master:1685", "--data-dir", "/data", "--enroll-only",
                    env=dict(os.environ, OSS_TRANSFER_JOIN_TOKEN=invite))
        files = self.root / "files"
        source, destination = files / "source", files / "destination"
        source.mkdir(parents=True)
        destination.mkdir()
        files.chmod(0o755)
        source.chmod(0o755)
        destination.chmod(0o777)
        manifest = {}
        for name, size in (("empty", 0), ("unicode 空格.bin", 65537), ("large.bin", 17 * 1024 * 1024)):
            path = source / name
            pattern = hashlib.sha256(name.encode()).digest()
            path.write_bytes((pattern * (size // 32 + 1))[:size])
            path.chmod(0o644)
            manifest[name] = {"size": size, "sha256": sha256(path)}
        agent = self.start("agent", ("oss-transfer-agent", "--data-dir", "/data"), mounts=[str(files) + ":/files"], volume=agent_volume)
        member = eventually(self.online, "Agent joined")
        configuration = json.loads(self.docker("inspect", agent))[0]["Config"]
        require(not any("JOIN_TOKEN" in item for item in configuration["Env"]), "Long-running Agent retained invitation")
        self.record("one-use enrollment; running Agent holds no invitation", nodeId=member["id"])
        access, secret = "docker-qa-" + secrets.token_hex(8), secrets.token_urlsafe(32)
        self.secrets += [access, secret]
        weed = self.start("weed", ("/qa/weed", "mini", "-dir=/data", "-ip=weed", "-ip.bind=0.0.0.0", "-admin.ui=false", "-webdav=false", "-s3.port.iceberg=0", "-s3.port.lance=0", "-s3.port=8333"),
                          mounts=[str(self.args.weed.resolve()) + ":/qa/weed:ro"], environment={"AWS_ACCESS_KEY_ID": access, "AWS_SECRET_ACCESS_KEY": secret}, port=8333, volume=weed_volume)
        s3 = boto3.client("s3", endpoint_url=self.origin_for(weed, 8333), region_name="us-east-1", aws_access_key_id=access,
                          aws_secret_access_key=secret, config=Config(signature_version="s3v4", s3={"addressing_style": "path"}, retries={"max_attempts": 1}, connect_timeout=2, read_timeout=10))
        def s3_ready():
            try:
                s3.list_buckets()
                return True
            except Exception:
                return False
        eventually(s3_ready, "isolated S3 service")
        bucket = self.prefix + "-objects"
        s3.create_bucket(Bucket=bucket)
        self.api("/api/master/s3-config", {"alias": "docker-qa", "endpoint": "http://weed:8333", "accessKey": access, "secretKey": secret, "pathStyleEnable": True, "validate": True})
        upload = {"direction": "LOCAL_TO_OSS", "sourceEndpoint": {"type": "LOCAL", "nodeBindings": [{"executionNodeIp": "docker-agent", "localPath": "/files/source"}]}, "destinationEndpoint": {"type": "OSS", "s3Alias": "docker-qa", "bucket": bucket}, "distributionMode": "balanced", "conflictPolicy": "OVERWRITE"}
        upload_id = self.create_task(upload, "docker-upload")
        total = sum(row["size"] for row in manifest.values())
        self.wait_task(upload_id, len(manifest), total)
        objects = {row["Key"]: row["Size"] for row in s3.list_objects_v2(Bucket=bucket).get("Contents", [])}
        require(objects == {key: row["size"] for key, row in manifest.items()}, "Uploaded keys or sizes differ")
        for key, row in manifest.items():
            with s3.get_object(Bucket=bucket, Key=key)["Body"] as stream:
                require(hashlib.sha256(stream.read()).hexdigest() == row["sha256"], "Uploaded payload SHA differs")
        self.record("bind source uploads to real S3", objects=len(manifest), bytes=total)
        self.docker("restart", "--time", "40", agent, master, timeout=100)
        self.origin = self.origin_for(master, 1685)
        eventually(lambda: self.api("/api/setup/status")["configured"], "configured Master after restart")
        self.token = self.api("/api/auth/login", credentials)["token"]
        self.secrets.append(self.token)
        recovered = eventually(self.online, "Agent after restart")
        require(recovered["id"] == member["id"], "Restart replaced the Agent identity")
        require(self.create_task(upload, "docker-upload") == upload_id, "Restart lost task idempotency")
        self.wait_task(upload_id, len(manifest), total)
        download = {"direction": "OSS_TO_LOCAL", "sourceEndpoint": {"type": "OSS", "s3Alias": "docker-qa", "bucket": bucket}, "destinationEndpoint": {"type": "LOCAL", "nodeBindings": [{"executionNodeIp": "docker-agent", "localPath": "/files/destination"}]}, "distributionMode": "balanced", "conflictPolicy": "OVERWRITE"}
        download_id = self.create_task(download, "docker-download")
        self.wait_task(download_id, len(manifest), total)
        published = set()
        for path in destination.iterdir():
            namespace = re.fullmatch(r"\.oss-transfer-staging-([a-f0-9]{64})-([a-f0-9]{32})", path.name)
            if not namespace:
                published.add(path.name)
                continue
            require(not path.is_symlink() and path.is_dir() and path.stat().st_uid == 65532 and path.stat().st_mode & 0o777 == 0o700,
                    "Staging namespace is not private")
            # The nonroot Agent owns this 0700 directory. Docker copies it for
            # inspection without changing host access or weakening its ACL.
            copied = self.root / ("inspected-" + path.name)
            self.docker("cp", agent + ":/files/destination/" + path.name, copied)
            require({item.name for item in copied.iterdir()} == {".oss-transfer-namespace"}, "Download left temporary payloads")
            marker = (copied / ".oss-transfer-namespace").read_text(encoding="utf-8")
            require(marker == "oss-transfer temporary namespace v1\n" + namespace[1] + "\n" + namespace[2] + "\n", "Invalid staging namespace marker")
        require(published == set(manifest), "Download left extra or missing files")
        inspected = self.root / "inspected-downloads"
        inspected.mkdir(mode=0o700)
        for key, row in manifest.items():
            original = destination / key
            require(not original.is_symlink() and original.is_file(), "Download is not a regular file")
            require(original.stat().st_size == row["size"], "Downloaded bind file size differs")
            # Downloads remain private to the Agent UID. Read the published
            # file through Docker without granting the runner access on disk.
            copied = inspected / key
            self.docker("cp", agent + ":/files/destination/" + key, copied)
            require(sha256(copied) == row["sha256"], "Downloaded bind file SHA differs")
        self.record("restart retains administrator, Agent, storage and task state; bind download SHA matches", objects=len(manifest), bytes=total)

    def close(self):
        for name in reversed(self.containers):
            result = subprocess.run(["docker", "logs", name], capture_output=True, text=True, timeout=15)
            (self.root / (name.removeprefix(self.prefix + "-") + ".log")).write_text(self.scrub(result.stdout + result.stderr), encoding="utf-8")
            self.docker("rm", "-f", name, check=False)
        for name in self.volumes:
            self.docker("volume", "rm", name, check=False)
        self.docker("network", "rm", self.network, check=False)
        self.save()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--weed", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    require(args.weed.is_file(), "Verified native Linux weed binary is required")
    acceptance = Acceptance(args)
    try:
        acceptance.run()
    except Exception as error:
        acceptance.evidence["failure"] = acceptance.scrub(str(error))
        raise
    finally:
        acceptance.close()
