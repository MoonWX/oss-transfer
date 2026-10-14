#!/usr/bin/env python3
"""Capture real V4 tasks and their stopped RocksDB for offline migration tests."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("comparison", ROOT / ".github/acceptance/go-compare.py")
comparison = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(comparison)
require = comparison.require


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("java", "java-package", "go-package", "weed", "output"):
        parser.add_argument("--" + name, required=True, type=Path)
    args = parser.parse_args()
    require(comparison.sha256(args.java_package) == "64a0e80a28d61322fad20be17c8d47c31f77565a76daf5b1f4718590f9c992d4", "Expected the pinned V4.0.6 release archive")
    os.umask(0o077)
    # Avoid interpreting timezone-less legacy timestamps using the test host's
    # locale. This setting is recorded in the fixture, not inferred at import.
    os.environ["JAVA_TOOL_OPTIONS"] = "-Duser.timezone=UTC"
    options = SimpleNamespace(**vars(args), node_bandwidth_mbps=2000, repetitions=1,
                              smoke=True, fixtures=["migration"], directions=[], keep_runtime=True)
    test = comparison.Comparison(options)
    evidence = {"status": "PREPARING", "legacyVersion": "4.0.6", "legacyTimeZone": "UTC", "tasks": [],
                "limitations": ["One real V4 Master also executes as its single worker", "Local SeaweedFS fixture; no JDCloud compatibility claim"]}
    try:
        test.h.start_s3()
        test.weed = test.h.children[0]
        test.start_java()
        credentials = {"legacyAdminPassword": test.secrets[0], "internalToken": test.secrets[1],
                       "storage": test.h.storage, "buckets": test.h.buckets, "s3Command": test.weed.args}
        (test.root / "private-runtime.json").write_text(json.dumps(credentials, indent=2) + "\n", encoding="utf-8")
        fixture, manifest = test.fixture("migration", [0] + [32768] * 258 + [17 * comparison.MIB])
        evidence["fixture"] = {"path": str(fixture), "manifest": "migration-manifest.json", "objects": len(manifest)}

        def task(task_id):
            rows = test.java_api("/api/master/status?includeSubtasks=false")["tasks"]
            return next((row for row in rows if int(row["taskIndex"]) == task_id), None)

        def create(direction, label):
            destination = test.root / "downloads" / label if direction == "OSS_TO_LOCAL" else fixture
            if direction == "OSS_TO_LOCAL":
                destination.mkdir(parents=True)
            endpoint, body = test.body("java", direction, destination, "migration", label + "/")
            identity = test.create(test.java_api, endpoint, body)
            entry = {"label": label, "direction": direction, "taskId": identity, "request": body}
            evidence["tasks"].append(entry)
            return identity, destination, entry

        for direction, label in (("LOCAL_TO_OSS", "completed-upload"), ("OSS_TO_OSS", "completed-copy"),
                                 ("OSS_TO_LOCAL", "completed-download"), ("LOCAL_TO_OSS", "recycled-upload")):
            task_id, destination, row = create(direction, label)
            row["snapshot"] = test.wait(test.java_api, task_id)
            if direction == "OSS_TO_LOCAL":
                require(not test.h.verify_local(destination, "migration", manifest), "V4 left incomplete download files")
            else:
                test.h.verify_s3(test.h.buckets["destination"], label + "/", manifest)
            row["sha256VerifiedObjects"] = len(manifest)
            if label == "recycled-upload":
                test.java_api("/api/master/clear", {"taskIds": [task_id]})
                row["recycled"] = True
            print("PASS real V4 " + label, flush=True)

        test.java_api("/api/master/rate-limit/bandwidth/127.0.0.1", {"qps": 1000000})
        task_id, _, row = create("LOCAL_TO_OSS", "stopped-upload")
        comparison.cluster.eventually(lambda: (task(task_id) or {}).get("putObjectCount", 0) > 0, "V4 stopped task has committed output", 45)
        test.java_api("/api/master/stop", {"taskIds": [task_id]})
        row["snapshot"] = comparison.cluster.eventually(lambda: (current if current and current["status"] == "STOPPED" else None)
                                                       if (current := task(task_id)) else None, "V4 stopped state", 30)
        require(0 < row["snapshot"]["putObjectCount"] < len(manifest), "Stopped fixture must retain both completed and pending objects")
        print("PASS real V4 stopped task with committed and pending objects", flush=True)

        task_id, _, row = create("OSS_TO_OSS", "interrupted-copy")
        def running_partial():
            value = task(task_id)
            return value if value and 0 < value.get("putObjectCount", 0) < len(manifest) and value["status"] not in comparison.cluster.TERMINAL else None
        row["snapshot"] = comparison.cluster.eventually(running_partial, "V4 running task has partial output", 45)
        evidence["workerStatus"] = test.java_api("/api/master/worker-status")
        evidence["deletedTasks"] = test.java_api("/api/master/deleted-tasks")
        # A real abrupt exit preserves a pending checkpoint. Do not call stop on
        # this task, because the importer must distinguish the old RUNNING state.
        test.java.kill()
        test.java.wait(timeout=20)
        databases = [p.parent for p in (test.root / "java-runtime").rglob("CURRENT") if (p.parent / "LOCK").is_file()]
        require(len(databases) == 1, "Expected one legacy RocksDB directory")
        exporter = test.root / "exporter"
        subprocess.run(["bash", str(ROOT / "tools/legacy/build.sh"), str(exporter)], env=dict(os.environ, JAVA_HOME=str(args.java.resolve().parents[1])), check=True, capture_output=True)
        snapshot = test.root / "master.jsonl"
        subprocess.run([str(args.java.resolve()), "-jar", str(exporter / "legacy-exporter.jar"),
                        "--legacy-jar", str(test.jar), "--expected-jar-sha256", comparison.sha256(test.jar),
                        "--database", str(databases[0]), "--output", str(snapshot), "--node", "127.0.0.1", "--role", "master",
                        "--config", str(test.root / "java-runtime/application.yml")], check=True, capture_output=True)
        evidence.update(status="PASSED", snapshot="master.jsonl", snapshotSHA256=comparison.sha256(snapshot), database=str(databases[0]),
                        jarSHA256=comparison.sha256(test.jar))
        test.result["status"] = "PASSED"
        print("PASS real V4 interrupted task exported with complete RocksDB", flush=True)
    except BaseException as error:
        evidence["status"] = "FAILED"
        evidence["errorType"] = type(error).__name__
        raise
    finally:
        test.close()
        # This evidence contains task metadata but no credentials. Snapshot and
        # private-runtime.json remain 0600 under the private fixture directory.
        text = json.dumps(evidence, indent=2) + "\n"
        for value in test.secrets + test.h.secrets:
            text = text.replace(value, "[redacted]")
        (test.root / "capture-evidence.json").write_text(text, encoding="utf-8")


if __name__ == "__main__":
    main()
