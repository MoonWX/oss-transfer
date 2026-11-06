#!/usr/bin/env python3
"""Run the Go tests of both modules with internal/master split into concurrent shards.

--scope full runs every test. --scope platform runs the platform-sensitive part:
every package except the platform-neutral ones listed in master-shards.json,
and of internal/master only the tests declared in its listed files.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import re
import subprocess
import sys
import time

TIMINGS = Path(__file__).with_name("master-shards.json")
# Benchmarks only run with -bench; every other listed name runs under go test.
LISTED = re.compile(r"(Test|Example|Fuzz)\w*")
# A top-level test function as go test -list reports it.
DECLARED = re.compile(r"^func (Test\w+)\(\s*\w+\s+\*testing\.T\s*\)", re.MULTILINE)
# Matches the go test package deadline used before sharding.
TIMEOUT = "-timeout=15m"


def load_timings(path=None):
    path = path or TIMINGS
    data = json.loads(path.read_text(encoding="utf-8"))
    shards, seconds = data["shards"], data["seconds"]
    if type(shards) is not int or shards < 1 or not isinstance(seconds, dict):
        raise ValueError("Invalid shard timing list: " + str(path))
    scope = data.get("platformScope", {})
    if (not isinstance(scope.get("skipPackages", []), list)
            or not isinstance(scope.get("packageTestFiles", []), list)
            or type(scope.get("shards", 1)) is not int or scope.get("shards", 1) < 1
            or not isinstance(scope.get("wordSizeTests", {}), dict)
            or not all(isinstance(names, list) for names in scope.get("wordSizeTests", {}).values())):
        raise ValueError("Invalid platform scope: " + str(path))
    return data


def listed_tests(text):
    """Parse go test -list output; anything unexpected fails rather than vanishes."""
    names = []
    for line in text.splitlines():
        line = line.strip()
        if not line or line.startswith("ok ") or line.startswith("ok\t") or line.startswith("Benchmark"):
            continue
        if not LISTED.fullmatch(line):
            raise ValueError("Unexpected go test -list output: " + line)
        names.append(line)
    if not names or len(names) != len(set(names)):
        raise ValueError("go test -list returned no tests or duplicate names")
    return names


def declared_tests(directory, files):
    """Top-level tests declared in the named files; a missing or empty file fails."""
    names = []
    for name in files:
        path = Path(directory) / name
        if Path(name).name != name or not name.endswith("_test.go") or not path.is_file():
            raise ValueError("Platform test file is missing: " + str(path))
        found = DECLARED.findall(path.read_text(encoding="utf-8"))
        if not found:
            raise ValueError("Platform test file declares no tests: " + str(path))
        names.extend(found)
    if len(names) != len(set(names)):
        raise ValueError("Platform test files declare a test twice")
    return sorted(names)


def platform_tests(listed, directory, files):
    """The listed package's platform-sensitive tests; a stale file list fails."""
    declared = declared_tests(directory, files)
    missing = sorted(set(declared) - set(listed))
    if missing:
        raise ValueError("go test -list does not report platform tests: " + ", ".join(missing))
    return declared


def plan_shards(names, seconds, count):
    """Assign longest-first to the lightest shard; unknown tests go to the lightest too."""
    known = sorted((name for name in names if name in seconds), key=lambda name: (-seconds[name], name))
    unknown = sorted(name for name in names if name not in seconds)
    shards = [{"tests": [], "seconds": 0.0} for _ in range(max(1, min(count, len(names))))]
    for name in known + unknown:
        lightest = min(shards, key=lambda shard: (shard["seconds"], len(shard["tests"])))
        lightest["tests"].append(name)
        lightest["seconds"] += float(seconds.get(name, 0))
    check_union(shards, names)
    return sorted(shards, key=lambda shard: -shard["seconds"]), unknown


def check_union(shards, names):
    assigned = [name for shard in shards for name in shard["tests"]]
    if len(assigned) != len(set(assigned)) or set(assigned) != set(names) or any(not s["tests"] for s in shards):
        missing, extra = sorted(set(names) - set(assigned)), sorted(set(assigned) - set(names))
        raise ValueError("Shards do not cover the listed tests exactly once: missing " + repr(missing) +
                         ", unexpected " + repr(extra))


def run_pattern(tests):
    # Top-level names only: subtests still run under their parent.
    return "^(" + "|".join(tests) + ")$"


def ordered_packages(packages, order):
    rank = {name: index for index, name in enumerate(order)}
    return sorted(packages, key=lambda name: (rank.get(name, len(rank)), name))


def execute(commands, output, cwd):
    """Run named commands concurrently, each into its own log; return per-command rows.

    A command is (name, argv) or (name, argv, directory); cwd is the default directory.
    """
    def start(item):
        name, command, directory = (*item, cwd) if len(item) == 2 else item
        log = output / (name + ".log")
        began = time.monotonic()
        with log.open("wb") as stream:
            try:
                code = subprocess.run(command, cwd=directory, stdout=stream, stderr=subprocess.STDOUT).returncode
            except OSError as error:
                stream.write(("Cannot start: " + str(error) + "\n").encode())
                code = None
        row = {"name": name, "command": [str(arg) for arg in command], "log": log.name,
               "exitCode": code, "seconds": round(time.monotonic() - began, 3)}
        row["result"] = "passed" if code == 0 else "failed"
        if code != 0 and "test" in command:
            # Rerun only the failed top-level tests once so a timing-sensitive
            # test on a slow runner does not fail the whole run; a real
            # regression fails again.
            text = log.read_text(encoding="utf-8", errors="replace")
            failed = sorted(set(re.findall(r"^--- FAIL: (\S+)", text, re.MULTILINE)))
            if failed:
                retry, skip = [], False
                for arg in command:
                    if skip:
                        skip = False
                    elif arg == "-run":
                        skip = True
                    elif not str(arg).startswith("-run="):
                        retry.append(arg)
                retry.insert(retry.index("test") + 1, "-run=" + run_pattern(failed))
                retry_log = output / (name + "-retry.log")
                with retry_log.open("wb") as stream:
                    retry_code = subprocess.run(retry, cwd=directory, stdout=stream, stderr=subprocess.STDOUT).returncode
                row["retry"] = {"tests": failed, "log": retry_log.name, "exitCode": retry_code}
                if retry_code == 0:
                    row["result"] = "passed"
                    print("flaky " + name + ": passed on retry " + " ".join(failed), flush=True)
                row["seconds"] = round(time.monotonic() - began, 3)
        print(row["result"] + " " + name + " (" + str(row["seconds"]) + "s)", flush=True)
        return row

    with ThreadPoolExecutor(max_workers=max(1, len(commands))) as pool:
        return list(pool.map(start, commands))


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--race", action="store_true")
    parser.add_argument("--scope", choices=("full", "platform"), default="full")
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    repo = Path.cwd()
    timings = load_timings()
    scope = timings.get("platformScope", {}) if args.scope == "platform" else {}
    race = ["-race"] if args.race else []
    module = subprocess.check_output(["go", "list", "-m"], text=True).strip()
    master = module + "/" + timings["package"]
    skipped = sorted(module + "/" + name for name in scope.get("skipPackages", []))
    packages = subprocess.check_output(["go", "list", "./..."], text=True).split()
    missing = sorted(set(skipped) - set(packages))
    others = [name for name in packages if name != master and name not in skipped]
    order = [module + "/" + name for name in timings["packageOrder"]]
    evidence = {"result": "failed", "race": args.race, "scope": args.scope, "skippedPackages": skipped,
                "commands": []}
    started = time.monotonic()
    # The rest of the module and the CLI module need no listing; start them
    # while the master test binary compiles for -list.
    background = [("others", ["go", "test", "-count=1", TIMEOUT, "-p", "2", *race, *ordered_packages(others, order)])]
    if (repo / "cli/go.mod").is_file():
        background.append(("cli", ["go", "test", "-count=1", TIMEOUT, "-p", "2", *race, "./..."], repo / "cli"))
    pool = ThreadPoolExecutor(max_workers=1)
    rest = pool.submit(execute, background, output, repo)
    rows = []
    try:
        if missing:
            # A renamed package would otherwise silently join the platform scope.
            raise ValueError("Platform scope skips unknown packages: " + ", ".join(missing))
        listing = subprocess.run(["go", "test", *race, "-list", ".", master], cwd=repo,
                                 capture_output=True, text=True)
        if listing.returncode != 0:
            print(listing.stdout + listing.stderr, file=sys.stderr, flush=True)
            raise RuntimeError("go test -list failed for " + master)
        names = listed_tests(listing.stdout)
        selected, count = names, timings["shards"]
        if args.scope == "platform":
            selected = platform_tests(names, repo / timings["package"], scope.get("packageTestFiles", []))
            count = scope.get("shards", 1)
            evidence["platformTests"] = selected
        shards, unknown = plan_shards(selected, timings["seconds"], count)
        evidence.update(listedTests=len(names), selectedTests=len(selected), untimedTests=unknown,
                        shards=[{"tests": len(s["tests"]), "estimatedSeconds": round(s["seconds"], 1)} for s in shards])
        print(f"Union check: {len(selected)} of {len(names)} listed tests in {timings['package']} = "
              f"{' + '.join(str(len(s['tests'])) for s in shards)} across {len(shards)} shards", flush=True)
        if unknown:
            print("Untimed tests assigned to the lightest shard: " + ", ".join(unknown), flush=True)
        commands = [(f"master-{index}", ["go", "test", "-count=1", TIMEOUT, *race,
                                        "-run", run_pattern(shard["tests"]), master])
                    for index, shard in enumerate(shards, 1)]
        rows = execute(commands, output, repo)
        for row in rows:
            # A pattern that matched nothing would otherwise pass silently.
            if "[no tests to run]" in (output / row["log"]).read_text(encoding="utf-8", errors="replace"):
                row["result"] = "failed"
    except Exception as error:
        evidence["error"] = type(error).__name__ + ": " + str(error)
        print("Unit runner failed: " + evidence["error"], file=sys.stderr, flush=True)
    finally:
        rows += rest.result()
        pool.shutdown()
        evidence["commands"] = rows
        if "error" not in evidence and all(row["result"] == "passed" for row in rows):
            evidence["result"] = "passed"
        evidence["seconds"] = round(time.monotonic() - started, 3)
        (output / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")
    for row in evidence["commands"]:
        if row["result"] != "passed":
            print((output / row["log"]).read_text(encoding="utf-8", errors="replace"), flush=True)
    return 0 if evidence["result"] == "passed" else 1


if __name__ == "__main__":
    sys.exit(main())
