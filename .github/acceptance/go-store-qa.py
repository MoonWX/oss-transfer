#!/usr/bin/env python3
"""Run Go Store acceptance against a loopback SeaweedFS process.

The caller supplies a verified native `weed` binary (the repository's existing
acceptance service is SeaweedFS 4.48). No cloud credentials or daemon are used.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[2]


def file_sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def binary_provenance(path, version_argument):
    path = path.resolve()
    result = subprocess.run([str(path), version_argument], capture_output=True, text=True, timeout=15)
    return {"path": str(path), "sha256": file_sha256(path), "version": (result.stdout + result.stderr).strip()}


def redact_log(path, values):
    text = path.read_text(errors="replace")
    for value in values:
        if value:
            text = text.replace(value, "[redacted]")
    path.write_text(text)


def reserve_ports(count):
    sockets = []
    try:
        for _ in range(count):
            server = socket.socket()
            server.bind(("127.0.0.1", 0))
            sockets.append(server)
        return [server.getsockname()[1] for server in sockets]
    finally:
        for server in sockets:
            server.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--weed", required=True, type=Path)
    parser.add_argument("--evidence", type=Path)
    parser.add_argument("--java", type=Path)
    parser.add_argument("--java-jar", type=Path)
    args = parser.parse_args()
    if bool(args.java) != bool(args.java_jar):
        parser.error("--java and --java-jar must be provided together")
    evidence = (args.evidence or Path(tempfile.mkdtemp(prefix="oss-transfer-store-qa-"))).resolve()
    evidence.mkdir(mode=0o700, parents=True, exist_ok=True)
    provenance = {"seaweedfs": binary_provenance(args.weed, "version"),
                  "go": subprocess.run(["go", "version"], text=True, capture_output=True, check=True).stdout.strip()}
    if args.java:
        provenance["java"] = binary_provenance(args.java, "-version")
        provenance["jar"] = {"path": str(args.java_jar.resolve()), "sha256": file_sha256(args.java_jar)}
        version_file = args.java_jar.parent / "VERSION"
        if version_file.is_file():
            provenance["jar"]["releaseVersion"] = version_file.read_text()
    (evidence / "provenance.json").write_text(json.dumps(provenance, indent=2) + "\n")
    work = Path(tempfile.mkdtemp(prefix="runtime-", dir=evidence))
    data = work / "s3-data"
    data.mkdir()
    access, secret = "go-qa-" + secrets.token_hex(8), secrets.token_urlsafe(32)
    ports = reserve_ports(8)
    flags = ["master.port", "master.port.grpc", "volume.port", "volume.port.grpc", "filer.port", "filer.port.grpc", "s3.port", "s3.port.grpc"]
    command = [str(args.weed.resolve()), "mini", "-dir=" + str(data), "-ip=127.0.0.1", "-ip.bind=127.0.0.1",
               "-admin.ui=false", "-webdav=false", "-s3.port.iceberg=0", "-s3.port.lance=0",
               "-s3.allowDeleteBucketNotEmpty=false"] + ["-" + flag + "=" + str(port) for flag, port in zip(flags, ports)]
    endpoint = "http://127.0.0.1:" + str(ports[6])
    environment = dict(os.environ, AWS_ACCESS_KEY_ID=access, AWS_SECRET_ACCESS_KEY=secret)
    log = evidence / "seaweedfs.log"
    with log.open("w") as output:
        process = subprocess.Popen(command, cwd=work, env=environment, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            deadline = time.monotonic() + 60
            while True:
                if process.poll() is not None:
                    raise RuntimeError("SeaweedFS exited before readiness; inspect " + str(log))
                try:
                    with urllib.request.urlopen(endpoint, timeout=1) as response:
                        if response.status == 200:
                            break
                except urllib.error.HTTPError as error:
                    if error.code == 403:
                        break
                except (urllib.error.URLError, TimeoutError):
                    pass
                if time.monotonic() > deadline:
                    raise RuntimeError("SeaweedFS did not become ready")
                time.sleep(0.2)
            env = dict(os.environ, OSS_TRANSFER_STORE_QA_ENDPOINT=endpoint,
                       OSS_TRANSFER_STORE_QA_ACCESS_KEY=access, OSS_TRANSFER_STORE_QA_SECRET_KEY=secret,
                       OSS_TRANSFER_STORE_QA_REPORT=str(evidence / "go-store-results.json"),
                       OSS_TRANSFER_STORE_QA_FIXTURE=str(evidence / "fixture"))
            env.setdefault("GOCACHE", "/tmp/oss-transfer-go-build")
            result = subprocess.run(["go", "test", "-count=1", "-timeout=12m", "-v", "./internal/transferstore", "./internal/agentservice", "-run", "^(TestRealS3StoreAcceptance|TestRealS3CrashRecovery)$"],
                                    cwd=ROOT, env=env, text=True, capture_output=True, timeout=750)
            text = result.stdout + result.stderr
            for value in (access, secret):
                text = text.replace(value, "[redacted]")
            (evidence / "go-store-test.log").write_text(text)
            print(text, end="", flush=True)
            if result.returncode:
                raise RuntimeError("Real S3 Store acceptance failed; evidence: " + str(evidence))
            if args.java:
                java_baseline(args, evidence, work, endpoint, access, secret)
            print("Evidence: " + str(evidence), flush=True)
        finally:
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=10)
            output.flush()
            redact_log(log, (access, secret))


def java_baseline(args, evidence, work, endpoint, access, secret):
    """Run the release JAR with one loopback worker and a private database."""
    directory = work / "java"
    directory.mkdir(mode=0o700)
    port = reserve_ports(1)[0]
    password, internal = secrets.token_urlsafe(24), secrets.token_urlsafe(32)
    config = directory / "application.yml"
    config.write_text("\n".join([
        "server.address: 127.0.0.1", "server.port: " + str(port), "transfer.port: " + str(port),
        "transfer.local-ip: 127.0.0.1", "transfer.master-ip: 127.0.0.1", "transfer.worker-ips: [127.0.0.1]",
        "transfer.role: master", "transfer.master-web-url: http://127.0.0.1:" + str(port),
        "thread-count.src.explore: 2", "thread-count.src.list: 2", "thread-count.dest.put: 4", "thread-count.dest.part: 4",
        "local-transfer.small-file-concurrency: 4", "local-transfer.batch.max-items: 1000", "local-transfer.batch.max-bytes: 4194304",
        "src.list.max-keys: 1000", "src.list.batch-size: 1000", "buffer-queue.list.size: 2048", "buffer-queue.put.size: 2048",
        "buffer-queue.progress.size: 2048", "buffer-queue.failed.size: 256", "buffer-queue.fallback.size: 256",
        "dest.multipart.threshold: 5242880", "dest.multipart.partsize: 5242880", "dest.multipart.max-in-flight-parts-per-object: 4",
        "rate-limit.src.qps.list: 1000000", "rate-limit.dest.qps.put: 1000000", "rate-limit.dest.qps.head: 1000000",
        "rate-limit.bandwidth: 1000000000000", "connection-pool.max-connections: 32", "transfer.cleanMetadata: false", ""
    ]))
    config.chmod(0o600)
    environment = dict(os.environ, OSS_TRANSFER_BOOTSTRAP_PASSWORD=password, OSS_TRANSFER_INTERNAL_TOKEN=internal)
    origin = "http://127.0.0.1:" + str(port)
    token = ""

    def request(path, body=None):
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = "Bearer " + token
        payload = None if body is None else json.dumps(body).encode()
        with urllib.request.urlopen(urllib.request.Request(origin + path, data=payload, headers=headers), timeout=15) as response:
            return json.load(response)

    log = evidence / "java-baseline.log"
    with log.open("w") as output:
        process = subprocess.Popen([str(args.java.resolve()), "-Xms64m", "-Xmx768m", "-jar", str(args.java_jar.resolve())],
                                   cwd=directory, env=environment, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        try:
            deadline = time.monotonic() + 90
            while True:
                if process.poll() is not None:
                    raise RuntimeError("Java baseline exited before startup; inspect " + str(log))
                try:
                    request("/api/auth/current")
                    break
                except (urllib.error.URLError, TimeoutError):
                    if time.monotonic() > deadline:
                        raise RuntimeError("Java baseline startup timed out")
                    time.sleep(0.25)
            token = request("/api/auth/login", {"username": "admin", "password": password})["token"]
            response = request("/api/master/s3-config", {"alias": "go-store-qa", "endpoint": endpoint,
                                "accessKey": access, "secretKey": secret, "validate": True,
                                "pathStyleEnable": True, "useV2SignerType": False})
            if response.get("success") is False or response.get("failedCount", 0) or response.get("partial"):
                raise RuntimeError("Java baseline could not configure its isolated S3 connection")
            env = dict(os.environ, OSS_TRANSFER_STORE_QA_ENDPOINT=endpoint, OSS_TRANSFER_STORE_QA_ACCESS_KEY=access,
                       OSS_TRANSFER_STORE_QA_SECRET_KEY=secret, OSS_TRANSFER_STORE_QA_JAVA_URL=origin,
                       OSS_TRANSFER_STORE_QA_JAVA_TOKEN=token, OSS_TRANSFER_STORE_QA_BENCH_REPORT=str(evidence / "java-go-benchmark.json"))
            env.setdefault("GOCACHE", "/tmp/oss-transfer-go-build")
            result = subprocess.run(["go", "test", "-count=1", "-timeout=15m", "-v", "./internal/transferstore", "-run", "^TestRealS3JavaBaseline$"],
                                    cwd=ROOT, env=env, text=True, capture_output=True, timeout=930)
            text = result.stdout + result.stderr
            for value in (access, secret, password, internal, token):
                text = text.replace(value, "[redacted]")
            (evidence / "java-go-benchmark.log").write_text(text)
            print(text, end="", flush=True)
            if result.returncode:
                raise RuntimeError("Java/Go benchmark failed; inspect " + str(evidence))
        finally:
            process.terminate()
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=10)
            output.flush()
            redact_log(log, (access, secret, password, internal, token))


if __name__ == "__main__":
    main()
