#!/usr/bin/env python3
"""Run one platform's acceptance groups concurrently, with an exclusive transfer window."""
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


# Groups that share a runner with the transfers. Go tests join as "unit" when
# they run here (emulated and VM platforms, or a local run); native CI runs
# them in separate jobs, and Windows CI also runs installation separately.
RUNNER_GROUPS = ("unit", "control", "install")
# Each transfer group drives its own SeaweedFS, Master and two Agents, so the
# measured scenarios run side by side instead of one after another.
TRANSFER_GROUPS = ("transfer", "recovery", "nodes")
FIXTURE_VARIABLES = ("QA_FAILURE_MASTER", "QA_ROLLBACK_CLI", "QA_MULTIPART_TEST_BINARY")
# Recovery task cap in Mbps. Lower a platform's entry if its transfer finishes
# before the stop or crash can be injected; cluster.py fails loudly when it does.
RECOVERY_BANDWIDTH_MBPS = {"default": 16}
# Lines of a failed phase's log repeated in the job log.
FAILURE_TAIL_LINES = 150


def write_json(path, value):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")
    # Windows refuses to replace a file another process has open; the transfer
    # groups poll the gate file, so retry briefly instead of failing the run.
    for attempt in range(100):
        try:
            temporary.replace(path)
            return
        except PermissionError:
            if attempt == 99:
                raise
            time.sleep(0.05)


def phase(name, command, cwd, *, evidence=None):
    return {"name": name, "command": [str(arg) for arg in command], "cwd": str(cwd), "evidence": evidence}


def verified_fixtures():
    """Executables published by test-binaries.py verify, or None to build them from source."""
    present = {name: os.environ[name] for name in FIXTURE_VARIABLES if os.environ.get(name)}
    if present and len(present) != len(FIXTURE_VARIABLES):
        raise ValueError("Set every verified fixture or none: " + ", ".join(FIXTURE_VARIABLES))
    return present or None


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
    groups = {}
    fixtures = verified_fixtures()
    install = [*privileged, scripts / "install.py", *cli, "--repo", repo, "--output", output / "install",
               "--all-installed", "--target-platform", args.platform]
    if fixtures:
        install += ["--failure-master-fixture", fixtures["QA_FAILURE_MASTER"],
                    "--rollback-cli-fixture", fixtures["QA_ROLLBACK_CLI"]]
    installation = [phase("install", install, repo, evidence=output / "install/evidence.json")]
    if args.docker_image:
        installation += [
            phase("docker", [py, scripts / "docker.py", "--image", args.docker_image, *storage, "--output", output / "docker"], repo,
                  evidence=output / "docker/evidence.json"),
            phase("compose-install", [*privileged, scripts / "setup.py", "--installer", repo / "install.sh",
                                      "--manifest", args.installer_manifest, "--image", args.docker_image,
                                      "--output", output / "setup"], repo, evidence=output / "setup/evidence.json"),
        ]
    if args.install == "only":
        return {"install": installation}
    # internal/master is the long pole and mostly waits on leases and restarts,
    # so both runners split it into concurrent shards next to the other packages.
    if args.unit != "none" and args.test_binaries:
        parallel = "4" if (os.cpu_count() or 1) > 2 else "2"
        groups["unit"] = [phase("unit", [py, scripts / "test-binaries.py", "run", "--platform", args.platform,
                                        "--directory", args.test_binaries, "--repo", repo, "--output", output / "unit",
                                        "--parallel", parallel, "--scope", args.unit], repo)]
    elif args.unit != "none":
        groups["unit"] = [phase("unit", [py, scripts / "go-unit.py", "--output", output / "unit", "--scope", args.unit,
                                        *(["--race"] if args.race else [])], repo)]
    version = (repo / "VERSION").read_text().strip()
    groups["control"] = [
        phase("smoke", [py, scripts / "runtime-smoke.py", *common, "--output", output / "smoke",
                        "--expect-agent-release-url", "https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/v" + version,
                        "--expect-agent-image", "moonwx/osstransfer:" + version], repo),
        phase("cli", [py, scripts / "cli.py", *cli, "--output", output / "cli"], repo),
        phase("local-cli", [py, scripts / "local-cli.py", *cli, "--output", output / "local-cli"], repo,
              evidence=output / "local-cli/evidence.json"),
        phase("inventory", [py, scripts / "inventory.py", *cli, *storage, "--output", output / "inventory"], repo),
        *sources,
    ]
    if args.install == "here":
        groups["install"] = installation
    rate = RECOVERY_BANDWIDTH_MBPS.get(args.platform, RECOVERY_BANDWIDTH_MBPS["default"])

    def cluster(name, scenario, directory, *extra, gated=True):
        command = [py, scripts / "cluster.py", *common, *storage, "--output", output / directory, "--scenario", scenario,
                   "--recovery-bandwidth-mbps", rate, *extra]
        if gated:
            command += ["--exclusive-gate", gate, "--exclusive-run-id", run_id]
        return [phase(name, command, repo)]

    multipart = ["--multipart-recovery", *(["--multipart-test-binary", fixtures["QA_MULTIPART_TEST_BINARY"]] if fixtures else [])]
    if args.transfer == "core":
        # Three transfer directions and multipart crash recovery; nothing is
        # measured, so there is no exclusive window to wait for.
        groups["transfer"] = cluster("transfer", "core", "cluster", *multipart, gated=False)
    else:
        groups["transfer"] = cluster("transfer", "transfer", "cluster", *multipart)
        groups["recovery"] = cluster("recovery", "recovery", "cluster-recovery")
        groups["nodes"] = cluster("nodes", "nodes", "cluster-nodes")
    return groups


def failure_tail(path):
    try:
        lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError as error:
        return "Log unavailable: " + str(error)
    skipped = len(lines) - FAILURE_TAIL_LINES
    return "\n".join((["[" + str(skipped) + " earlier lines are in the uploaded log]"] if skipped > 0 else [])
                     + lines[-FAILURE_TAIL_LINES:])


class Runner:
    def __init__(self, directory, groups, run_id, *, timeout=1200):
        names = set(groups)
        # Which groups a job runs is the plan's decision (see plan() and its
        # tests); reject names no plan produces and groups without phases.
        if not names or not names <= {*RUNNER_GROUPS, *TRANSFER_GROUPS} or any(not steps for steps in groups.values()):
            raise ValueError("Unknown or empty acceptance groups: " + ", ".join(sorted(names)))
        names = [step["name"] for steps in groups.values() for step in steps]
        if len(names) != len(set(names)):
            raise ValueError("Acceptance phase names must be unique")
        self.directory, self.groups, self.run_id, self.timeout = directory, groups, run_id, timeout
        # Measured transfers wait for every other group on this runner.
        self.required = [name for name in RUNNER_GROUPS if name in groups]
        self.gate = directory / "exclusive-gate.json"
        self.lock = threading.Lock()
        self.cancelled = threading.Event()
        self.processes = set()
        self.result = {"runId": run_id, "status": "RUNNING", "groups": {}, "phases": []}
        self.open_gate("WAITING")

    def open_gate(self, status):
        write_json(self.gate, {"runId": self.run_id, "status": status,
                              "requiredGroups": self.required, "groups": self.result["groups"]})

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
            # Record first: terminate() itself can fail (EPERM on a sudo
            # child), and the failure tail below needs the error.
            row["error"] = type(error).__name__ + ": " + str(error)
            if process is not None:
                self.terminate(process)
        finally:
            row["seconds"] = round(time.monotonic() - started, 3)
            with self.lock:
                if process is not None:
                    self.processes.discard(process)
                self.result["phases"].append(row)
                self.save()
                if row["status"] != "PASSED" and process is not None:
                    # The job log shows why a phase failed without downloading
                    # the evidence artifact.
                    print("::group::" + step["name"] + " log (" + row["error"] + ")\n"
                          + failure_tail(log_path) + "\n::endgroup::", flush=True)
            print(row["status"] + " " + step["name"] + " (" + str(row["seconds"]) + "s); " + str(log_path), flush=True)
        return row["status"] == "PASSED"

    def run_group(self, name, steps):
        return all(self.run_phase(name, step) for step in steps)

    def run(self):
        began = time.monotonic()
        pool = ThreadPoolExecutor(max_workers=len(self.groups))
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
                    elif all(self.result["groups"].get(group) == "PASSED" for group in self.required):
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
    parser.add_argument("--unit", choices=("full", "platform", "none"), default="full",
                        help="Go tests on this runner: every test, the platform-sensitive scope, or none "
                             "(native CI runs them in a separate job)")
    parser.add_argument("--transfer", choices=("full", "core"), default="full",
                        help="full: every transfer scenario in three clusters; core: transfer directions and "
                             "multipart recovery only")
    parser.add_argument("--install", choices=("here", "separate", "only"), default="here",
                        help="here: installation runs beside the other groups; separate: another job runs it; "
                             "only: run just the installation")
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
    try:
        fixtures = verified_fixtures()
    except ValueError as error:
        parser.error(str(error))
    if args.test_binaries:
        args.test_binaries = args.test_binaries.resolve(strict=True)
        if not fixtures:
            parser.error("Missing verified fixtures " + ", ".join(FIXTURE_VARIABLES))
    if args.race and args.test_binaries:
        parser.error("Precompiled target tests do not support --race")
    directory = args.output / "platform"
    directory.mkdir(mode=0o700, parents=True, exist_ok=False)
    run_id = secrets.token_hex(16)
    groups = plan(args, directory / "exclusive-gate.json", run_id)
    return 0 if Runner(directory, groups, run_id).run() else 1


if __name__ == "__main__":
    sys.exit(main())
