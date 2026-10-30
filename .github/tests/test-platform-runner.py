#!/usr/bin/env python3
"""Exercise platform coordination with real child processes and transfer gates."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
import types
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[2]
SELF = Path(__file__).resolve()


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, ROOT / ".github/acceptance" / filename)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


RUNNER = load("platform_runner_tests", "run-platform.py")


def load_cluster():
    # The gate needs no S3 client, server or credentials. Keep its real polling
    # and validation while stubbing only optional import-time dependencies.
    with mock.patch.dict(sys.modules, {"boto3": types.ModuleType("boto3"),
                                      "botocore": types.ModuleType("botocore"),
                                      "botocore.config": types.SimpleNamespace(Config=object)}):
        return load("platform_gate_tests", "cluster.py")


def read_json(path):
    return json.loads(path.read_text(encoding="utf-8")) if path.exists() else {}


def fixture_worker(config_path):
    config = read_json(Path(config_path))
    directory, name = Path(config["directory"]), config["name"]
    if config.get("proof") is not None:
        RUNNER.write_json(directory / (name + "-proof.json"), config["proof"])
    (directory / (name + "-started")).write_text(str(os.getpid()))
    if config.get("gate"):
        cluster = load_cluster()

        class ObservedGate:
            def __init__(self):
                self.reads = 0

            def read_text(self, **kwargs):
                text = Path(config["gate"]).read_text(**kwargs)
                self.reads += 1
                RUNNER.write_json(directory / "gate-observed.json",
                                  {"reads": self.reads, "state": json.loads(text)})
                return text

        cluster.wait_for_exclusive(ObservedGate(), config["runId"], timeout=config.get("gateTimeout", 10))
        (directory / "transfer-continued").write_text("bandwidth and recovery may run")
    elif config.get("blocked", True):
        deadline = time.monotonic() + 15
        while not (directory / (name + "-release")).exists():
            if time.monotonic() > deadline:
                raise TimeoutError("Test did not release " + name)
            time.sleep(0.02)
    (directory / (name + "-finished")).write_text("exiting")
    return config.get("exitCode", 0)


def fixture_runner(spec_path):
    spec = read_json(Path(spec_path))
    runner = RUNNER.Runner(Path(spec["directory"]), spec["groups"], spec["runId"], timeout=15)
    return 0 if runner.run() else 1


class ProcessCoordinationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="platform runner ")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.run_id = "fresh-test-run"
        self.sequence = 0

    def wait_for(self, condition, description):
        deadline = time.monotonic() + 12
        while time.monotonic() < deadline:
            result = condition()
            if result:
                return result
            time.sleep(0.02)
        self.fail("Timed out waiting for " + description)

    def command(self, directory, name, **config):
        config = dict(config, directory=str(directory), name=name)
        path = directory / (name + "-config.json")
        RUNNER.write_json(path, config)
        return [sys.executable, str(SELF), "--fixture-worker", str(path)]

    def groups(self, directory, *, cleanup=True, exit_codes=None):
        exit_codes = exit_codes or {}
        groups = {}
        for name in ("unit", "control", "install", "transfer"):
            config = {"exitCode": exit_codes.get(name, 0)}
            if name == "install":
                config["proof"] = {"result": "passed", "testServicesRemoved": cleanup}
            if name == "transfer":
                config.update(gate=str(directory / "exclusive-gate.json"), runId=self.run_id)
            groups[name] = [RUNNER.phase(name, self.command(directory, name, **config), directory,
                                        evidence=directory / "install-proof.json" if name == "install" else None)]
        return groups

    def start(self, directory, groups):
        spec = directory / "runner.json"
        spec.write_text(json.dumps({"directory": str(directory), "groups": groups, "runId": self.run_id},
                                   default=str), encoding="utf-8")
        log = (directory / "driver.log").open("wb")
        self.addCleanup(log.close)
        child = subprocess.Popen([sys.executable, str(SELF), "--fixture-runner", str(spec)],
                                 stdout=log, stderr=subprocess.STDOUT, start_new_session=os.name != "nt")
        self.addCleanup(self.stop, child, directory)
        return child

    def stop(self, child, directory):
        # Release owned fixtures even when an assertion fails; they run no real
        # services and are the only descendants created by this test.
        for path in directory.glob("*-config.json"):
            (directory / (read_json(path)["name"] + "-release")).touch()
        if child.poll() is None:
            try:
                child.wait(timeout=12)
            except subprocess.TimeoutExpired:
                RUNNER.Runner.terminate(child)

    def directory(self):
        self.sequence += 1
        directory = self.root / str(self.sequence)
        directory.mkdir()
        return directory

    def release(self, directory, *names):
        for name in names:
            (directory / (name + "-release")).touch()

    def finished(self, child, directory, expected):
        self.assertEqual(child.wait(timeout=12), expected, (directory / "driver.log").read_text())
        return read_json(directory / "evidence.json")

    def test_all_four_groups_must_be_registered_before_any_command_starts(self):
        for missing in ("unit", "control", "install", "transfer"):
            with self.subTest(missing=missing):
                directory = self.directory()
                groups = self.groups(directory)
                del groups[missing]
                child = self.start(directory, groups)
                self.assertNotEqual(child.wait(timeout=12), 0)
                self.assertEqual(list(directory.glob("*-started")), [])
                self.assertIn("Every platform must run", (directory / "driver.log").read_text())

    def test_cancelled_runner_does_not_start_a_later_real_command(self):
        directory = self.directory()
        groups = self.groups(directory)
        runner = RUNNER.Runner(directory, groups, self.run_id)
        self.release(directory, "unit")
        runner.cancelled.set()
        self.assertFalse(runner.run_phase("unit", groups["unit"][0]))
        self.assertEqual(list(directory.glob("*-started")), [])
        self.assertEqual(runner.processes, set())
        row = read_json(directory / "evidence.json")["phases"][0]
        self.assertEqual(row["status"], "FAILED")
        self.assertIn("cancelled", row["error"])
        # The same executable and paths really do start when cancellation is
        # absent; a broken fixture cannot satisfy the negative assertion above.
        runner.cancelled.clear()
        self.assertTrue(runner.run_phase("unit", groups["unit"][0]))
        self.assertTrue((directory / "unit-started").exists())

    @unittest.skipIf(os.name == "nt", "POSIX process-group exit race")
    def test_terminate_accepts_process_exit_between_poll_and_signal(self):
        child = subprocess.Popen([sys.executable, "-c", "pass"], start_new_session=True)
        self.addCleanup(child.wait)

        class ExitsAfterPoll:
            pid = child.pid

            def poll(self):
                # Reproduce the race deterministically: poll's observation is
                # stale, while the actual child and its process group are gone.
                child.wait(timeout=12)
                return None

            def wait(self, **kwargs):
                return child.wait(**kwargs)

        RUNNER.Runner.terminate(ExitsAfterPoll())
        self.assertEqual(child.returncode, 0)

    def test_passing_evidence_cannot_release_gate_while_installer_is_still_alive(self):
        directory = self.directory()
        child = self.start(directory, self.groups(directory))
        self.wait_for(lambda: len(list(directory.glob("*-started"))) == 4, "all four real subprocesses")
        self.assertTrue(read_json(directory / "install-proof.json")["testServicesRemoved"])
        self.release(directory, "unit", "control")

        def observed_wait():
            observed = read_json(directory / "gate-observed.json")
            state = observed.get("state", {})
            completed = read_json(directory / "evidence.json").get("groups", {})
            return observed.get("reads", 0) >= 2 and state.get("status") == "WAITING" \
                and completed.get("unit") == "PASSED" and completed.get("control") == "PASSED"

        # Observe the actual gate reader, not a short sleep that merely hopes
        # the transfer had enough CPU time to attempt its exclusive phase.
        self.wait_for(observed_wait, "transfer observing the still-running installer")
        self.assertFalse((directory / "install-finished").exists())
        self.assertFalse((directory / "transfer-continued").exists())
        self.release(directory, "install")
        evidence = self.finished(child, directory, 0)
        self.assertEqual(evidence["status"], "PASSED")
        self.assertEqual(evidence["groups"], dict.fromkeys(("unit", "control", "install", "transfer"), "PASSED"))
        self.assertTrue((directory / "transfer-continued").exists())
        self.assertEqual({row["name"] for row in evidence["phases"]}, {"unit", "control", "install", "transfer"})

    def test_failed_commands_or_missing_cleanup_fail_job_and_block_transfer(self):
        cases = [(name, 7, True) for name in ("unit", "control", "install")]
        cases.append(("install", 0, False))
        for name, code, cleanup in cases:
            with self.subTest(group=name, exit=code, cleanup=cleanup):
                directory = self.directory()
                child = self.start(directory, self.groups(directory, cleanup=cleanup, exit_codes={name: code}))
                self.release(directory, "unit", "control", "install")
                evidence = self.finished(child, directory, 1)
                self.assertEqual(evidence["status"], "FAILED")
                self.assertEqual(evidence["groups"][name], "FAILED")
                row = next(row for row in evidence["phases"] if row["name"] == name)
                self.assertEqual(row["exitCode"], code)
                if not cleanup:
                    self.assertIn("service cleanup", row["error"])
                self.assertEqual(read_json(directory / "exclusive-gate.json")["status"], "FAILED")
                self.assertFalse((directory / "transfer-continued").exists())

    def test_real_transfer_exit_code_changes_total_result_after_gate_opens(self):
        directory = self.directory()
        child = self.start(directory, self.groups(directory, exit_codes={"transfer": 9}))
        self.release(directory, "unit", "control", "install")
        evidence = self.finished(child, directory, 1)
        self.assertTrue((directory / "transfer-continued").exists())
        self.assertEqual(evidence["groups"]["transfer"], "FAILED")
        row = next(row for row in evidence["phases"] if row["name"] == "transfer")
        self.assertEqual(row["exitCode"], 9)
        self.assertEqual(evidence["status"], "FAILED")

    def test_invalid_gate_or_timeout_never_reaches_exclusive_work(self):
        ready = {"runId": self.run_id, "status": "READY", "requiredGroups": ["unit", "control", "install"],
                 "groups": {"unit": "PASSED", "control": "PASSED", "install": "PASSED"}}
        cases = [(dict(ready, runId="previous-run"), "another run"),
                 (dict(ready, requiredGroups=["unit", "control"]), "omitted a required group"),
                 (dict(ready, groups={"unit": "PASSED", "control": "PASSED"}), "before every other group passed"),
                 (dict(ready, status="FAILED"), "group failed"),
                 (dict(ready, status="WAITING"), "Timed out")]
        for state, error in cases:
            with self.subTest(error=error):
                directory = self.directory()
                gate = directory / "exclusive-gate.json"
                RUNNER.write_json(gate, state)
                command = self.command(directory, "transfer", gate=str(gate), runId=self.run_id, gateTimeout=0.3)
                completed = subprocess.run(command, text=True, capture_output=True, timeout=12)
                self.assertNotEqual(completed.returncode, 0)
                self.assertIn(error, completed.stderr)
                self.assertFalse((directory / "transfer-continued").exists())

    def test_native_docker_and_compose_install_phases_execute_serially(self):
        directory = self.directory()
        groups = self.groups(directory)
        groups["install"] += [
            RUNNER.phase("docker", self.command(directory, "docker", proof={"testResourcesRemoved": True}), directory,
                         evidence=directory / "docker-proof.json"),
            RUNNER.phase("compose-install", self.command(directory, "compose-install",
                         proof={"status": "PASSED", "testResourcesRemoved": True}), directory,
                         evidence=directory / "compose-install-proof.json"),
        ]
        child = self.start(directory, groups)
        self.wait_for(lambda: (directory / "install-started").exists(), "native installer start")
        self.assertFalse((directory / "docker-started").exists())
        self.release(directory, "install")
        self.wait_for(lambda: (directory / "docker-started").exists(), "Docker acceptance start")
        self.assertTrue((directory / "install-finished").exists())
        self.assertFalse((directory / "compose-install-started").exists())
        self.release(directory, "docker")
        self.wait_for(lambda: (directory / "compose-install-started").exists(), "Compose installer start")
        self.assertTrue((directory / "docker-finished").exists())
        self.release(directory, "unit", "control")
        self.wait_for(lambda: read_json(directory / "gate-observed.json").get("reads", 0) >= 2, "gate polling")
        self.assertFalse((directory / "transfer-continued").exists())
        self.release(directory, "compose-install")
        self.assertEqual(self.finished(child, directory, 0)["status"], "PASSED")

    def test_compose_cleanup_failure_blocks_exclusive_phase(self):
        directory = self.directory()
        groups = self.groups(directory)
        groups["install"].append(RUNNER.phase("compose-install", self.command(directory, "compose-install",
                    proof={"status": "PASSED", "testResourcesRemoved": False}, blocked=False), directory,
                    evidence=directory / "compose-install-proof.json"))
        child = self.start(directory, groups)
        self.release(directory, "unit", "control", "install")
        evidence = self.finished(child, directory, 1)
        self.assertFalse((directory / "transfer-continued").exists())
        row = next(row for row in evidence["phases"] if row["name"] == "compose-install")
        self.assertIn("success and cleanup", row["error"])

    def test_docker_missing_cleanup_or_recorded_failure_blocks_exclusive_phase(self):
        for proof in ({"testResourcesRemoved": False}, {},
                      {"testResourcesRemoved": True, "failure": "container acceptance failed"}):
            with self.subTest(proof=proof):
                directory = self.directory()
                groups = self.groups(directory)
                groups["install"].append(RUNNER.phase("docker", self.command(directory, "docker",
                            proof=proof, blocked=False), directory, evidence=directory / "docker-proof.json"))
                child = self.start(directory, groups)
                self.release(directory, "unit", "control", "install")
                evidence = self.finished(child, directory, 1)
                self.assertEqual(evidence["status"], "FAILED")
                self.assertEqual(evidence["groups"]["install"], "FAILED")
                self.assertFalse((directory / "transfer-continued").exists())
                self.assertEqual(read_json(directory / "exclusive-gate.json")["status"], "FAILED")
                row = next(row for row in evidence["phases"] if row["name"] == "docker")
                self.assertEqual(row["exitCode"], 0)
                self.assertIn("Docker acceptance did not confirm success and cleanup", row["error"])


class PlatformPlanTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="platform plan ")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.args = argparse.Namespace(repo=ROOT, output=self.root, platform="linux/amd64", race=True,
                    test_binaries=None, docker_image=None, installer_manifest=None,
                    master=self.root / "master", agent=self.root / "agent", cli=self.root / "cli", weed=self.root / "weed")

    def plan(self):
        return RUNNER.plan(self.args, self.root / "exclusive-gate.json", "plan-run")

    def test_native_runs_every_package_in_both_modules_with_race_and_bounded_parallelism(self):
        for platform in ("linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"):
            with self.subTest(platform=platform):
                self.args.platform, self.args.race = platform, platform != "windows/arm64"
                groups = self.plan()
                go_steps = [step for steps in groups.values() for step in steps if step["command"][0] == "go"]
                self.assertEqual({step["cwd"] for step in go_steps}, {str(ROOT), str(ROOT / "cli")})
                self.assertEqual(len(go_steps), 2)
                for step in go_steps:
                    command = step["command"]
                    self.assertEqual(command[1], "test")
                    self.assertIn("./...", command)
                    self.assertIn("-timeout=15m", command)
                    self.assertEqual("-race" in command, self.args.race)
                    self.assertEqual(command[command.index("-p") + 1], "2")
                    self.assertFalse(any(arg.startswith(("-run", "-skip")) for arg in command))
        self.assertEqual({step["name"] for step in groups["control"]}, {"smoke", "cli", "local-cli", "inventory"})
        transfer = groups["transfer"][0]["command"]
        self.assertEqual(transfer[transfer.index("--scenario") + 1], "all")
        self.assertIn("--multipart-recovery", transfer)
        self.assertEqual(transfer[transfer.index("--exclusive-run-id") + 1], "plan-run")

    def test_precompiled_plan_uses_inventory_verifier_and_target_fixtures(self):
        self.args.platform, self.args.race = "freebsd/arm64", False
        self.args.test_binaries = self.root / "test-artifacts"
        self.args.test_binaries.mkdir()
        fixtures = {"QA_FAILURE_MASTER": str(self.root / "failure-master"),
                    "QA_ROLLBACK_CLI": str(self.root / "rollback-cli"),
                    "QA_MULTIPART_TEST_BINARY": str(self.root / "agent.test")}
        with mock.patch.dict(os.environ, fixtures):
            groups = self.plan()
        unit = next(step for step in groups["unit"] if step["name"] == "unit")
        command = unit["command"]
        self.assertEqual(command[1:3], [str(ROOT / ".github/acceptance/test-binaries.py"), "run"])
        for option, expected in (("--platform", "freebsd/arm64"), ("--directory", str(self.args.test_binaries)),
                                 ("--repo", str(ROOT)), ("--parallel", "2")):
            self.assertEqual(command[command.index(option) + 1], expected)
        self.assertFalse(any(arg.startswith(("-run", "-skip")) for arg in command))
        install = groups["install"][0]["command"]
        for option, variable in (("--failure-master-fixture", "QA_FAILURE_MASTER"), ("--rollback-cli-fixture", "QA_ROLLBACK_CLI")):
            self.assertEqual(install[install.index(option) + 1], fixtures[variable])
        transfer = groups["transfer"][0]["command"]
        self.assertEqual(transfer[transfer.index("--multipart-test-binary") + 1], fixtures["QA_MULTIPART_TEST_BINARY"])
        # Run the actual planned command: an artifact without the source/package
        # inventory must fail verification, rather than pass with zero tests.
        (self.args.test_binaries / "manifest.json").write_text('{}')
        completed = subprocess.run(command, cwd=unit["cwd"], capture_output=True, text=True, timeout=12)
        self.assertNotEqual(completed.returncode, 0)
        evidence = read_json(self.root / "unit/evidence.json")
        self.assertEqual(evidence["phase"], "verify")
        self.assertEqual(evidence["result"], "failed")
        self.assertEqual(evidence["packages"], [])

    def test_docker_and_native_install_share_one_serial_group_and_cleanup_proofs(self):
        self.args.docker_image = "oss-transfer:test"
        self.args.installer_manifest = self.root / "manifest.tsv"
        groups = self.plan()
        self.assertEqual([step["name"] for step in groups["install"]], ["install", "docker", "compose-install"])
        self.assertIn("--all-installed", groups["install"][0]["command"])
        self.assertEqual(groups["install"][0]["evidence"], self.root / "install/evidence.json")
        self.assertEqual(groups["install"][1]["evidence"], self.root / "docker/evidence.json")
        self.assertEqual(groups["install"][-1]["evidence"], self.root / "setup/evidence.json")


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--fixture-worker":
        sys.exit(fixture_worker(sys.argv[2]))
    if len(sys.argv) == 3 and sys.argv[1] == "--fixture-runner":
        sys.exit(fixture_runner(sys.argv[2]))
    unittest.main()
