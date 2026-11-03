#!/usr/bin/env python3
"""Run the main module's Go tests with internal/master split into concurrent shards."""
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
# Matches the go test package deadline used before sharding.
TIMEOUT = "-timeout=15m"


def load_timings(path=None):
    path = path or TIMINGS
    data = json.loads(path.read_text(encoding="utf-8"))
    shards, seconds = data["shards"], data["seconds"]
    if type(shards) is not int or shards < 1 or not isinstance(seconds, dict):
        raise ValueError("Invalid shard timing list: " + str(path))
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
    """Run named commands concurrently, each into its own log; return per-command rows."""
    def start(item):
        name, command = item
        log = output / (name + ".log")
        began = time.monotonic()
        with log.open("wb") as stream:
            try:
                code = subprocess.run(command, cwd=cwd, stdout=stream, stderr=subprocess.STDOUT).returncode
            except OSError as error:
                stream.write(("Cannot start: " + str(error) + "\n").encode())
                code = None
        row = {"name": name, "command": [str(arg) for arg in command], "log": log.name,
               "exitCode": code, "seconds": round(time.monotonic() - began, 3)}
        row["result"] = "passed" if code == 0 else "failed"
        print(row["result"] + " " + name + " (" + str(row["seconds"]) + "s)", flush=True)
        return row

    with ThreadPoolExecutor(max_workers=max(1, len(commands))) as pool:
        return list(pool.map(start, commands))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--race", action="store_true")
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    repo = Path.cwd()
    timings = load_timings()
    race = ["-race"] if args.race else []
    module = subprocess.check_output(["go", "list", "-m"], text=True).strip()
    master = module + "/" + timings["package"]
    others = [name for name in subprocess.check_output(["go", "list", "./..."], text=True).split() if name != master]
    order = [module + "/" + name for name in timings["packageOrder"]]
    evidence = {"result": "failed", "race": args.race, "commands": []}
    started = time.monotonic()
    pool = ThreadPoolExecutor(max_workers=1)
    # The rest of the module needs no listing; start it while the master test
    # binary compiles for -list.
    rest = pool.submit(execute, [("others", ["go", "test", "-count=1", TIMEOUT, "-p", "2", *race,
                                            *ordered_packages(others, order)])], output, repo)
    rows = []
    try:
        listing = subprocess.run(["go", "test", *race, "-list", ".", master], cwd=repo,
                                 capture_output=True, text=True)
        if listing.returncode != 0:
            print(listing.stdout + listing.stderr, file=sys.stderr, flush=True)
            raise RuntimeError("go test -list failed for " + master)
        names = listed_tests(listing.stdout)
        shards, unknown = plan_shards(names, timings["seconds"], timings["shards"])
        evidence.update(listedTests=len(names), untimedTests=unknown,
                        shards=[{"tests": len(s["tests"]), "estimatedSeconds": round(s["seconds"], 1)} for s in shards])
        print(f"Union check: {len(names)} listed tests in {timings['package']} = "
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
