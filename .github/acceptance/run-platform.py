#!/usr/bin/env python3
"""Run one platform's complete acceptance, with an exclusive transfer window."""
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
import json
import os
from pathlib import Path
import secrets
import signal
import subprocess
import sys
import threading
import time


REQUIRED_GROUPS = ("unit", "control", "install")


def write_json(path, value):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")
    temporary.replace(path)


def phase(name, command, cwd, *, evidence=None):
    return {"name": name, "command": [str(arg) for arg in command], "cwd": str(cwd), "evidence": evidence}


def plan(args, gate, run_id):
    repo, output, py = args.repo, args.output, sys.executable
    scripts = repo / ".github/acceptance"
    common = ["--master", args.master, "--agent", args.agent]
    storage = ["--weed", args.weed] if args.weed else []
    cli = [*common, "--cli", args.cli]
    privileged = ["sudo", "env", "PATH=" + os.environ.get("PATH", ""), py] if sys.platform == "linux" and os.geteuid() != 0 else [py]
    sources = ([phase("download-sources", ["pwsh", "-NoProfile", "-File", repo / ".github/tests/test-download-source.ps1"], repo)]
               if sys.platform == "win32" else [
                   phase("installer-sources", [py, repo / ".github/tests/test-source-selection.py", "-v"], repo),
                   phase("download-sources", [py, repo / ".github/tests/test-download-source.py", "-v"], repo)])
    if args.test_binaries:
        unit = [phase("unit", [py, scripts / "test-binaries.py", "run", "--platform", args.platform,
                               "--directory", args.test_binaries, "--repo", repo, "--output", output / "unit", "--parallel", "2"], repo)]
    else:
        command = ["go", "test", "-count=1", "-p", "2", *(["-race"] if args.race else []), "./..."]
        unit = [phase("unit", command, repo), phase("cli-unit", command, repo / "cli")]
    version = (repo / "VERSION").read_text().strip()
    control = [
        phase("smoke", [py, scripts / "runtime-smoke.py", *common, "--output", output / "smoke",
                        "--expect-agent-release-url", "https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/v" + version,
                        "--expect-agent-image", "moonwx/osstransfer:" + version], repo),
        phase("cli", [py, scripts / "cli.py", *cli, "--output", output / "cli"], repo),
        phase("local-cli", [py, scripts / "local-cli.py", *cli, "--output", output / "local-cli"], repo,
              evidence=output / "local-cli/evidence.json"),
        phase("inventory", [py, scripts / "inventory.py", *cli, *storage, "--output", output / "inventory"], repo),
    ]
    install = [*privileged, scripts / "install.py", *cli, "--repo", repo, "--output", output / "install",
               "--all-installed", "--target-platform", args.platform]
    if args.test_binaries:
        install += ["--failure-master-fixture", os.environ["QA_FAILURE_MASTER"],
                    "--rollback-cli-fixture", os.environ["QA_ROLLBACK_CLI"]]
    installation = [phase("install", install, repo, evidence=output / "install/evidence.json")]
    if args.docker_image:
        installation += [
            phase("docker", [py, scripts / "docker.py", "--image", args.docker_image, *storage, "--output", output / "docker"], repo,
                  evidence=output / "docker/evidence.json"),
            phase("compose-install", [*privileged, scripts / "setup.py", "--installer", repo / "install.sh",
                                      "--manifest", args.installer_manifest, "--image", args.docker_image,
                                      "--output", output / "setup"], repo, evidence=output / "setup/evidence.json"),
        ]
    transfer = [py, scripts / "cluster.py", *common, *storage, "--output", output / "cluster", "--scenario", "all",
                "--multipart-recovery", "--exclusive-gate", gate, "--exclusive-run-id", run_id]
    if args.test_binaries:
        transfer += ["--multipart-test-binary", os.environ["QA_MULTIPART_TEST_BINARY"]]
    return {"unit": sources + unit, "control": control, "install": installation,
            "transfer": [phase("transfer", transfer, repo)]}


class Runner:
    def __init__(self, directory, groups, run_id, *, timeout=1200):
        if set(groups) != {*REQUIRED_GROUPS, "transfer"} or any(not steps for steps in groups.values()):
            raise ValueError("Every platform must run unit, control, install and transfer groups")
        names = [step["name"] for steps in groups.values() for step in steps]
        if len(names) != len(set(names)):
            raise ValueError("Acceptance phase names must be unique")
        self.directory, self.groups, self.run_id, self.timeout = directory, groups, run_id, timeout
        self.gate = directory / "exclusive-gate.json"
        self.lock = threading.Lock()
        self.cancelled = threading.Event()
        self.processes = set()
        self.result = {"runId": run_id, "status": "RUNNING", "groups": {}, "phases": []}
        self.open_gate("WAITING")

    def open_gate(self, status):
        write_json(self.gate, {"runId": self.run_id, "status": status,
                              "requiredGroups": list(REQUIRED_GROUPS), "groups": self.result["groups"]})

    def save(self):
        write_json(self.directory / "evidence.json", self.result)

    @staticmethod
    def terminate(process):
        if process.poll() is not None:
            return
        if os.name == "nt":
            subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"], capture_output=True, timeout=20)
        else:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
        try:
            process.wait(timeout=15)
        except subprocess.TimeoutExpired:
            if os.name != "nt":
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            else:
                process.kill()
            process.wait(timeout=10)

    def run_phase(self, group, step):
        started = time.monotonic()
        log_path = self.directory / (step["name"] + ".log")
        print("START " + step["name"], flush=True)
        row = {"group": group, "name": step["name"], "status": "FAILED"}
        process = None
        try:
            with log_path.open("wb") as log:
                with self.lock:
                    if self.cancelled.is_set():
                        raise RuntimeError("Platform acceptance was cancelled")
                    process = subprocess.Popen(step["command"], cwd=step["cwd"], stdout=log, stderr=subprocess.STDOUT,
                                               start_new_session=os.name != "nt")
                    self.processes.add(process)
                row["exitCode"] = process.wait(timeout=self.timeout)
            if row["exitCode"] != 0:
                raise RuntimeError("Exit code " + str(row["exitCode"]))
            if step.get("evidence"):
                proof = json.loads(Path(step["evidence"]).read_text(encoding="utf-8"))
                if step["name"] == "install" and not (proof.get("result") == "passed" and proof.get("testServicesRemoved") is True):
                    raise RuntimeError("Native installer did not confirm success and service cleanup")
                if step["name"] == "compose-install" and not (proof.get("status") == "PASSED" and proof.get("testResourcesRemoved") is True):
                    raise RuntimeError("Compose installer did not confirm success and cleanup")
                if step["name"] == "docker" and (proof.get("failure") or proof.get("testResourcesRemoved") is not True):
                    raise RuntimeError("Docker acceptance did not confirm success and cleanup")
            row["status"] = "PASSED"
        except Exception as error:
            if process is not None:
                self.terminate(process)
            row["error"] = type(error).__name__ + ": " + str(error)
        finally:
            row["seconds"] = round(time.monotonic() - started, 3)
            with self.lock:
                if process is not None:
                    self.processes.discard(process)
                self.result["phases"].append(row)
                self.save()
            print(row["status"] + " " + step["name"] + " (" + str(row["seconds"]) + "s); " + str(log_path), flush=True)
        return row["status"] == "PASSED"

    def run_group(self, name, steps):
        return all(self.run_phase(name, step) for step in steps)

    def run(self):
        began = time.monotonic()
        pool = ThreadPoolExecutor(max_workers=4)
        try:
            futures = {pool.submit(self.run_group, name, steps): name for name, steps in self.groups.items()}
            for future in as_completed(futures):
                name = futures[future]
                passed = future.result()
                with self.lock:
                    self.result["groups"][name] = "PASSED" if passed else "FAILED"
                    # The worker returns only after its last subprocess has exited
                    # and the installer has confirmed that its services are gone.
                    if any(value == "FAILED" for value in self.result["groups"].values()):
                        self.open_gate("FAILED")
                    elif all(self.result["groups"].get(group) == "PASSED" for group in REQUIRED_GROUPS):
                        self.open_gate("READY")
                    self.save()
            self.result["status"] = "PASSED" if all(value == "PASSED" for value in self.result["groups"].values()) else "FAILED"
        except BaseException:
            with self.lock:
                self.cancelled.set()
                self.open_gate("FAILED")
                processes = list(self.processes)
            for process in processes:
                self.terminate(process)
            self.result["status"] = "FAILED"
            raise
        finally:
            pool.shutdown(wait=True)
            self.result["seconds"] = round(time.monotonic() - began, 3)
            self.save()
        return self.result["status"] == "PASSED"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--platform", required=True)
    parser.add_argument("--repo", type=Path, default=Path.cwd())
    parser.add_argument("--output", type=Path, default=Path("output"))
    parser.add_argument("--bin-dir", type=Path)
    parser.add_argument("--test-binaries", type=Path)
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--docker-image")
    parser.add_argument("--installer-manifest", type=Path)
    args = parser.parse_args()
    args.repo, args.output = args.repo.resolve(), args.output.resolve()
    if bool(args.docker_image) != bool(args.installer_manifest):
        parser.error("Docker acceptance requires both --docker-image and --installer-manifest")
    extension = ".exe" if args.platform.startswith("windows/") else ""
    for name in ("master", "agent", "cli", "weed"):
        path = None
        if args.bin_dir:
            path = args.bin_dir / (("weed" if name == "weed" else "oss-transfer-" + name) + extension)
        elif os.environ.get("QA_" + name.upper()):
            path = Path(os.environ["QA_" + name.upper()])
        if name == "weed" and os.environ.get("OSS_TRANSFER_QA_S3_CONFIG"):
            path = None
        if path is None and name != "weed":
            parser.error("Missing verified QA_" + name.upper() + " binary")
        if path is not None:
            path = path.resolve(strict=True)
        setattr(args, name, path)
    if args.weed is None and not os.environ.get("OSS_TRANSFER_QA_S3_CONFIG"):
        parser.error("Missing verified S3 fixture")
    if args.test_binaries:
        args.test_binaries = args.test_binaries.resolve(strict=True)
        for variable in ("QA_FAILURE_MASTER", "QA_ROLLBACK_CLI", "QA_MULTIPART_TEST_BINARY"):
            if not os.environ.get(variable):
                parser.error("Missing verified fixture " + variable)
    if args.race and args.test_binaries:
        parser.error("Precompiled target tests do not support --race")
    directory = args.output / "platform"
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    run_id = secrets.token_hex(16)
    groups = plan(args, directory / "exclusive-gate.json", run_id)
    return 0 if Runner(directory, groups, run_id).run() else 1


if __name__ == "__main__":
    sys.exit(main())
