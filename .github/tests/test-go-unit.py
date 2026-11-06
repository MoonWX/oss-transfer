#!/usr/bin/env python3
"""Check that sharded Go unit runs cover every listed test exactly once and fail closed."""
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("go_unit", ROOT / ".github/acceptance/go-unit.py")
UNIT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(UNIT)

# Stands in for go: answers the listings and records which tests each run selects.
FAKE_GO = r'''#!/usr/bin/env python3
import json, os, re, sys
state = os.environ["FAKE_GO_STATE"]
config = json.load(open(os.path.join(state, "config.json")))
args = sys.argv[1:]
with open(os.path.join(state, "calls.jsonl"), "a") as stream:
    stream.write(json.dumps(args) + "\n")
if args == ["list", "-m"]:
    print("example.test/module")
elif args == ["list", "./..."]:
    print("\n".join(config["packages"]))
elif "-list" in args:
    print("\n".join(config["listed"]))
    print("ok  \texample.test/module/internal/master\t0.01s")
elif "-run" in args:
    pattern = args[args.index("-run") + 1]
    selected = [name for name in config["runnable"] if re.fullmatch(pattern, name)]
    with open(os.path.join(state, "ran.jsonl"), "a") as stream:
        stream.write(json.dumps(selected) + "\n")
    if not selected:
        print("ok  \texample.test/module/internal/master\t0.01s [no tests to run]")
    else:
        print("ok  \texample.test/module/internal/master\t0.01s")
    sys.exit(1 if config.get("failShard") in selected else 0)
else:
    print("ok  \texample.test/module/internal/other\t0.01s")
    sys.exit(config.get("othersExit", 0))
'''


class ShardPlanTests(unittest.TestCase):
    def test_listing_ignores_benchmarks_and_summary_but_rejects_unexpected_lines(self):
        text = "TestB\nExampleA\nFuzzC\nBenchmarkD\nok  \tpkg\t0.1s\n\n"
        self.assertEqual(UNIT.listed_tests(text), ["TestB", "ExampleA", "FuzzC"])
        for bad in ("TestA\nwarning: something\n", "TestA\nTestA\n", "", "BenchmarkOnly\n", "TestA/sub\n"):
            with self.subTest(text=bad), self.assertRaises(ValueError):
                UNIT.listed_tests(bad)

    def test_longest_first_assignment_covers_each_test_once_and_untimed_go_to_lightest(self):
        names = ["TestA", "TestB", "TestC", "TestD", "TestE", "TestNew"]
        seconds = {"TestA": 10, "TestB": 9, "TestC": 8, "TestD": 2, "TestE": 1, "TestRemoved": 50}
        shards, unknown = UNIT.plan_shards(names, seconds, 3)
        self.assertEqual(unknown, ["TestNew"])
        self.assertEqual(sorted(name for shard in shards for name in shard["tests"]), sorted(names))
        self.assertEqual([shard["tests"][0] for shard in shards], ["TestA", "TestB", "TestC"])
        self.assertEqual([shard["seconds"] for shard in shards], [10, 10, 10])
        # All three weigh 10 when TestNew arrives; ties go to the fewest tests.
        self.assertEqual(next(shard["tests"] for shard in shards if "TestNew" in shard["tests"]), ["TestA", "TestNew"])

    def test_more_shards_than_tests_never_creates_an_empty_shard(self):
        shards, _ = UNIT.plan_shards(["TestOnly"], {}, 3)
        self.assertEqual([shard["tests"] for shard in shards], [["TestOnly"]])

    def test_union_check_rejects_missing_duplicate_extra_and_empty_shards(self):
        names = ["TestA", "TestB"]
        for shards in ([{"tests": ["TestA"]}],
                       [{"tests": ["TestA", "TestB"]}, {"tests": ["TestB"]}],
                       [{"tests": ["TestA", "TestB", "TestC"]}],
                       [{"tests": ["TestA", "TestB"]}, {"tests": []}]):
            with self.subTest(shards=shards), self.assertRaises(ValueError):
                UNIT.check_union(shards, names)

    def test_run_pattern_selects_exact_top_level_names_only(self):
        pattern = re.compile(UNIT.run_pattern(["TestA", "TestB"]))
        self.assertTrue(pattern.fullmatch("TestA") and pattern.search("TestB"))
        for other in ("TestAB", "XTestA", "TestC"):
            self.assertIsNone(pattern.search(other), other)

    def test_checked_in_timings_balance_the_real_master_tests(self):
        timings = UNIT.load_timings()
        names = sorted(timings["seconds"])
        shards, unknown = UNIT.plan_shards(names, timings["seconds"], timings["shards"])
        self.assertEqual(unknown, [])
        self.assertEqual(len(shards), timings["shards"])
        total = sum(timings["seconds"].values())
        # Data check, not a timing assertion: no shard carries half the package.
        self.assertLess(max(shard["seconds"] for shard in shards), total / 2)
        self.assertEqual(timings["packageOrder"][0], timings["package"])

    def test_known_slow_packages_are_ordered_first(self):
        packages = ["m/a", "m/internal/task", "m/b", "m/internal/execution"]
        self.assertEqual(UNIT.ordered_packages(packages, ["m/internal/task", "m/internal/execution"]),
                         ["m/internal/task", "m/internal/execution", "m/a", "m/b"])


@unittest.skipIf(os.name == "nt", "the fake go executable is a POSIX script")
class RunnerTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="go unit ")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.state = self.root / "state"
        self.state.mkdir()
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        go = bin_dir / "go"
        go.write_text(FAKE_GO.replace("/usr/bin/env python3", sys.executable, 1))
        go.chmod(0o755)
        self.environment = dict(os.environ, PATH=str(bin_dir) + os.pathsep + os.environ["PATH"],
                                FAKE_GO_STATE=str(self.state))
        self.timings = self.root / "timings.json"
        self.timings.write_text(json.dumps({"package": "internal/master", "shards": 3,
                                            "packageOrder": ["internal/master", "internal/slow"],
                                            "seconds": {"TestA": 5, "TestB": 4, "TestC": 3, "TestD": 1}}))
        self.listed = ["TestA", "TestB", "TestC", "TestD", "TestUntimed", "ExampleE"]
        self.configure()

    def configure(self, **changes):
        config = {"packages": ["example.test/module/internal/fast", "example.test/module/internal/master",
                               "example.test/module/internal/slow"],
                  "listed": self.listed, "runnable": self.listed, **changes}
        (self.state / "config.json").write_text(json.dumps(config))

    def run_unit(self, *extra):
        script = ("import runpy, sys; from pathlib import Path; "
                  "module = runpy.run_path(sys.argv[1], run_name='go_unit'); "
                  "module['main'].__globals__['TIMINGS'] = Path(sys.argv[2]); "
                  "sys.argv = [sys.argv[1], *sys.argv[3:]]; sys.exit(module['main']())")
        output = self.root / "out"
        completed = subprocess.run([sys.executable, "-c", script, str(ROOT / ".github/acceptance/go-unit.py"),
                                    str(self.timings), "--output", str(output), *extra],
                                   cwd=self.root, env=self.environment, capture_output=True, text=True, timeout=60)
        evidence = json.loads((output / "evidence.json").read_text())
        return completed, evidence

    def calls(self):
        return [json.loads(line) for line in (self.state / "calls.jsonl").read_text().splitlines()]

    def ran(self):
        path = self.state / "ran.jsonl"
        return [json.loads(line) for line in path.read_text().splitlines()] if path.exists() else []

    def test_every_listed_test_runs_exactly_once_with_race_and_the_rest_ordered(self):
        completed, evidence = self.run_unit("--race")
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertEqual(evidence["result"], "passed")
        self.assertEqual(sorted(name for shard in self.ran() for name in shard), sorted(self.listed))
        self.assertEqual(len(self.ran()), 3)
        self.assertEqual(evidence["untimedTests"], ["ExampleE", "TestUntimed"])
        self.assertIn("Union check: 6 of 6 listed tests", completed.stdout)
        runs = [call for call in self.calls() if call[0] == "test" and "-list" not in call]
        self.assertTrue(all("-race" in call and "-count=1" in call and "-timeout=15m" in call for call in runs))
        others = next(call for call in runs if "-run" not in call)
        self.assertEqual(others[-2:], ["example.test/module/internal/slow", "example.test/module/internal/fast"])
        self.assertNotIn("example.test/module/internal/master", others)
        self.assertEqual({row["name"] for row in evidence["commands"]}, {"master-1", "master-2", "master-3", "others"})
        self.assertEqual((evidence["scope"], evidence["skippedPackages"]), ("full", []))
        for row in evidence["commands"]:
            self.assertTrue((self.root / "out" / row["log"]).is_file())

    def test_cli_module_runs_beside_the_main_module_with_the_same_flags(self):
        (self.root / "cli").mkdir()
        (self.root / "cli/go.mod").write_text("module oss-transfer-cli\n")
        completed, evidence = self.run_unit("--race")
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        cli = next(row for row in evidence["commands"] if row["name"] == "cli")
        self.assertEqual(cli["command"], ["go", "test", "-count=1", "-timeout=15m", "-p", "2", "-race", "./..."])
        self.assertEqual(cli["result"], "passed")

    def test_platform_scope_skips_neutral_packages_and_runs_only_declared_master_tests(self):
        master = self.root / "internal/master"
        master.mkdir(parents=True)
        (master / "platform_test.go").write_text(
            "package master\n\nfunc TestB(t *testing.T) {}\n\nfunc helper(t *testing.T) {}\n"
            "func TestUntimed(t *testing.T) {}\n")
        self.timings.write_text(json.dumps({"package": "internal/master", "shards": 3,
                                            "packageOrder": ["internal/master", "internal/slow"],
                                            "platformScope": {"skipPackages": ["internal/slow"],
                                                              "packageTestFiles": ["platform_test.go"], "shards": 2},
                                            "seconds": {"TestA": 5, "TestB": 4, "TestC": 3, "TestD": 1}}))
        completed, evidence = self.run_unit("--scope", "platform")
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertEqual(sorted(name for shard in self.ran() for name in shard), ["TestB", "TestUntimed"])
        self.assertEqual(len(self.ran()), 2)
        self.assertEqual(evidence["platformTests"], ["TestB", "TestUntimed"])
        self.assertEqual((evidence["scope"], evidence["skippedPackages"]), ("platform", ["example.test/module/internal/slow"]))
        others = next(call for call in self.calls() if call[0] == "test" and "-run" not in call and "-list" not in call)
        self.assertEqual(others[-1:], ["example.test/module/internal/fast"])
        self.assertIn("Union check: 2 of 6 listed tests", completed.stdout)

    def test_platform_scope_fails_closed_on_stale_configuration(self):
        master = self.root / "internal/master"
        master.mkdir(parents=True)
        (master / "platform_test.go").write_text("package master\n\nfunc TestGone(t *testing.T) {}\n")
        for scope, error in (({"skipPackages": [], "packageTestFiles": ["platform_test.go"]}, "does not report platform tests"),
                             ({"skipPackages": [], "packageTestFiles": ["missing_test.go"]}, "Platform test file is missing"),
                             ({"skipPackages": ["internal/renamed"], "packageTestFiles": []}, "skips unknown packages")):
            with self.subTest(error=error):
                for path in (self.state / "calls.jsonl", self.state / "ran.jsonl"):
                    path.unlink(missing_ok=True)
                self.timings.write_text(json.dumps({"package": "internal/master", "shards": 3, "packageOrder": [],
                                                    "platformScope": scope, "seconds": {}}))
                completed, evidence = self.run_unit("--scope", "platform")
                self.assertEqual(completed.returncode, 1)
                self.assertIn(error, evidence["error"])
                self.assertEqual(self.ran(), [])
                shutil.rmtree(self.root / "out")

    def test_failing_shard_or_other_packages_fail_the_run_after_all_finish(self):
        for change, failed in (({"failShard": "TestC"}, "master"), ({"othersExit": 1}, "others")):
            with self.subTest(failed=failed):
                for path in (self.state / "calls.jsonl", self.state / "ran.jsonl"):
                    path.unlink(missing_ok=True)
                self.configure(**change)
                completed, evidence = self.run_unit()
                self.assertEqual(completed.returncode, 1)
                self.assertEqual(evidence["result"], "failed")
                self.assertEqual(len(evidence["commands"]), 4)
                self.assertTrue(any(row["name"].startswith(failed) and row["result"] == "failed"
                                    for row in evidence["commands"]))
                (self.root / "out" / "evidence.json").unlink()
                for log in (self.root / "out").iterdir():
                    log.unlink()
                (self.root / "out").rmdir()

    def test_listed_test_that_a_shard_does_not_run_fails_the_run(self):
        # The binary lists TestD but no -run pattern can reach it: the shard
        # holding only unreachable names reports no tests and must fail.
        self.listed = ["TestA", "TestB", "TestC"]
        self.configure(runnable=["TestA", "TestB"])
        completed, evidence = self.run_unit()
        self.assertEqual(completed.returncode, 1)
        self.assertEqual(evidence["result"], "failed")

    def test_unexpected_listing_fails_without_running_shards_but_keeps_other_results(self):
        self.configure(listed=["TestA", "panic: init failed"])
        completed, evidence = self.run_unit()
        self.assertEqual(completed.returncode, 1)
        self.assertIn("Unexpected go test -list output", evidence["error"])
        self.assertEqual(self.ran(), [])
        self.assertEqual([row["name"] for row in evidence["commands"]], ["others"])


if __name__ == "__main__":
    unittest.main()
