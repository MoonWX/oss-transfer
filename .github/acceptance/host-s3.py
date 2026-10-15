#!/usr/bin/env python3
"""Run the pinned S3 fixture on the VM host, keeping its credentials private."""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import platform
import secrets
import shutil
import signal
import subprocess
import time
from urllib.request import urlopen

import boto3
from botocore.config import Config
from botocore.exceptions import ClientError


def sibling(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


PREPARE = sibling("host_s3_prepare", "prepare.py")
STORE = sibling("host_s3_store", "store-qa.py")


class FixtureError(RuntimeError):
    """A fixed diagnostic string that contains no fixture credentials."""


def private_json(path, data):
    with path.open("w", encoding="utf-8") as stream:
        os.chmod(path, 0o600)
        json.dump(data, stream, indent=2)
        stream.write("\n")


def client(endpoint, access, secret):
    return boto3.client("s3", endpoint_url=endpoint, region_name="us-east-1",
                        aws_access_key_id=access, aws_secret_access_key=secret,
                        config=Config(signature_version="s3v4", s3={"addressing_style": "path"},
                                      connect_timeout=2, read_timeout=3, retries={"max_attempts": 0},
                                      request_checksum_calculation="when_required",
                                      response_checksum_validation="when_required"))


def identity(pid):
    result = subprocess.run(["ps", "-ww", "-p", str(pid), "-o", "stat=", "-o", "lstart=", "-o", "command="],
                            capture_output=True, text=True, check=False)
    fields = result.stdout.strip().split(None, 1)
    return fields[1] if result.returncode == 0 and len(fields) == 2 and not fields[0].startswith("Z") else ""


def owned_process(state):
    current = identity(state["pid"])
    if not current:
        return False
    if current != state["processIdentity"]:
        raise FixtureError("Fixture PID belongs to a different process; refusing to terminate it")
    return True


def scrub_log(directory, state):
    path = directory / "seaweedfs.log"
    if path.exists():
        STORE.redact_log(path, (state["accessKey"], state["secretKey"]))


def wait_gone(pid, timeout):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not identity(pid):
            return True
        time.sleep(0.1)
    return not identity(pid)


def stop(directory):
    state_path = directory / "process.json"
    if not state_path.is_file():
        raise FixtureError("Fixture process state is missing")
    state = json.loads(state_path.read_text(encoding="utf-8"))
    try:
        if owned_process(state):
            try:
                os.killpg(state["pid"], signal.SIGTERM)
            except ProcessLookupError:
                pass  # The fixture can exit between the ownership check and signal.
            # BSD can replace a dying process's command with an exit marker.
            # Wait for disappearance without treating that transient marker as
            # a reused PID; recheck full ownership before any second signal.
            if not wait_gone(state["pid"], 15) and owned_process(state):
                try:
                    os.killpg(state["pid"], signal.SIGKILL)
                except ProcessLookupError:
                    pass
                if not wait_gone(state["pid"], 5):
                    raise FixtureError("Host S3 fixture did not exit after SIGKILL")
        state_path.unlink()
        (directory / "config.json").unlink(missing_ok=True)
    finally:
        scrub_log(directory, state)
    print("Host S3 fixture stopped", flush=True)


def start(args):
    directory = args.directory.resolve()
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    system = platform.system().lower()
    architecture = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    asset = f"{system}_{architecture}.tar.gz"
    if asset not in PREPARE.WEED or system not in ("linux", "darwin"):
        raise FixtureError("Host S3 fixture requires Linux or macOS amd64/arm64")
    archive = args.archive.resolve() if args.archive else directory / asset
    if not args.archive:
        with urlopen("https://github.com/seaweedfs/seaweedfs/releases/download/4.48/" + asset,
                     timeout=120) as source, archive.open("wb") as target:
            shutil.copyfileobj(source, target)
    if PREPARE.digest(archive) != PREPARE.WEED[asset]:
        raise FixtureError("Host SeaweedFS fixture archive checksum mismatch")
    weed = directory / "weed"
    PREPARE.copy_member(archive, "weed", weed)
    weed.chmod(0o700)
    PREPARE.verify_binary(weed, system, architecture)
    provenance = dict(STORE.binary_provenance(weed, "version"), mode="host-native", release="4.48",
                      platform=system + "/" + architecture, archive=asset, archiveSHA256=PREPARE.WEED[asset])
    del provenance["path"]
    access, secret = "host-qa-" + secrets.token_hex(8), secrets.token_urlsafe(32)
    ports = STORE.reserve_ports(8)
    flags = ("master.port", "master.port.grpc", "volume.port", "volume.port.grpc",
             "filer.port", "filer.port.grpc", "s3.port", "s3.port.grpc")
    data = directory / "data"
    data.mkdir(mode=0o700)
    command = [str(weed), "mini", "-dir=" + str(data), "-ip=127.0.0.1", "-ip.bind=127.0.0.1",
               "-admin.ui=false", "-webdav=false", "-s3.port.iceberg=0", "-s3.port.lance=0"]
    command += [f"-{flag}={port}" for flag, port in zip(flags, ports)]
    log_path = directory / "seaweedfs.log"
    with log_path.open("wb") as log:
        os.chmod(log_path, 0o600)
        process = subprocess.Popen(command, cwd=directory, start_new_session=True,
                                   stdin=subprocess.DEVNULL, stdout=log, stderr=subprocess.STDOUT,
                                   env=dict(os.environ, AWS_ACCESS_KEY_ID=access, AWS_SECRET_ACCESS_KEY=secret))
    state = dict(pid=process.pid, processIdentity=identity(process.pid), accessKey=access, secretKey=secret)
    try:
        if not state["processIdentity"]:
            raise FixtureError("Host S3 fixture exited during startup")
        private_json(directory / "process.json", state)
        endpoint = "http://127.0.0.1:" + str(ports[6])
        s3 = client(endpoint, access, secret)
        deadline = time.monotonic() + 60
        while True:
            if process.poll() is not None:
                raise FixtureError("Host S3 fixture exited before readiness")
            try:
                s3.list_buckets()
                break
            except Exception:
                if time.monotonic() >= deadline:
                    raise FixtureError("Host S3 fixture did not become ready") from None
                time.sleep(0.2)
        # A responding anonymous fixture would make authentication tests vacuous.
        try:
            client(endpoint, access, secrets.token_urlsafe(32)).list_buckets()
        except ClientError as error:
            if error.response.get("ResponseMetadata", {}).get("HTTPStatusCode") not in (401, 403):
                raise FixtureError("Host S3 fixture returned an unexpected authentication response") from None
        else:
            raise FixtureError("Host S3 fixture accepted invalid credentials")
        private_json(directory / "config.json", dict(format=1, endpoint=f"http://{args.guest_host}:{ports[6]}",
                                                     accessKey=access, secretKey=secret, provenance=provenance))
        (directory / "provenance.json").write_text(json.dumps(provenance, indent=2) + "\n", encoding="utf-8")
    except BaseException:
        if process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        scrub_log(directory, state)
        (directory / "process.json").unlink(missing_ok=True)
        (directory / "config.json").unlink(missing_ok=True)
        raise
    print("Host S3 fixture ready; private configuration: " + str(directory / "config.json"), flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("start", "stop"))
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--guest-host", default="10.0.2.2", help="Host loopback address as seen from the VM's user-mode network")
    parser.add_argument("--archive", type=Path, help="Reuse a downloaded pinned host fixture archive; checksum is always verified")
    args = parser.parse_args()
    try:
        if args.command == "start":
            start(args)
        else:
            stop(args.directory.resolve())
    except Exception as error:
        # SDK/process error strings can contain configuration. Keep CI output
        # credential-free; the fixture's private, scrubbed log retains diagnostics.
        detail = str(error) if isinstance(error, FixtureError) else type(error).__name__
        parser.exit(1, "Host S3 fixture failed: " + detail + "; inspect its private log\n")


if __name__ == "__main__":
    main()
