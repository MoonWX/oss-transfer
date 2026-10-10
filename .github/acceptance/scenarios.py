"""Black-box checks against packaged processes and an independent S3 service.

The harness owns processes, credentials, fixtures, and independent SHA-256 checks.
No application classes or former unit-test fixtures are imported here.
"""

import hashlib
import time
from pathlib import Path


def _require(condition, message):
    if not condition:
        raise AssertionError(message)


def _data(response):
    _require(isinstance(response, dict), "Response is not a JSON object")
    _require(response.get("success") is not False, "API operation failed: " + str(response))
    result = response.get("data", response)
    _require(isinstance(result, dict), "Response data is not a JSON object")
    for value in (response, result):
        _require(not value.get("partial"), "Partial response: " + str(value))
        for name in ("failedWorkers", "unreachableWorkers", "missingTaskIds", "duplicateTaskIds"):
            if value.get(name):
                raise AssertionError("Incomplete response: " + name + "=" + str(value[name]))
    return result


def _until(check, description, timeout=45, interval=0.5):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(interval)
    raise AssertionError("Timed out waiting for " + description)


def _created(h, response):
    data = _data(response)
    ids = data.get("parentTaskIds", data.get("taskIds"))
    if ids is not None:
        _require(data.get("acceptedCount") == 1 and data.get("rejectedCount", 0) == 0,
                 "Local task was not uniquely accepted: " + str(data))
        _require(len(ids) == 1, "Expected exactly one logical task")
        return int(ids[0])
    creation_ids = data.get("creationIds", [])
    _require(data.get("addedCount") == 1 and len(creation_ids) == 1,
             "OSS task was not uniquely accepted: " + str(data))

    def assigned():
        creation = _data(h.api("GET", "/api/master/task-creations/" + creation_ids[0]))["creation"]
        _require(creation.get("status") != "FAILED", "Task creation failed: " + str(creation))
        return creation.get("publicTaskId")

    return int(_until(assigned, "public OSS task ID"))


def _children(h, task_id):
    rows, offset = [], 0
    while True:
        page = _data(h.api("GET", f"/api/master/tasks/{task_id}/children?offset={offset}&limit=200"))
        _require(page.get("snapshotComplete") is True, "Child snapshot is incomplete")
        items = page.get("items")
        _require(isinstance(items, list), "Child items are missing")
        rows.extend(items)
        if not page.get("hasMore"):
            return rows
        offset += 200
        _require(offset <= 2000, "Unexpected child task count for a small fixture")


def _both_workers(h, task_id):
    rows = _children(h, task_id)
    done = {}
    for child in rows:
        node = child.get("executionNodeIp") or child.get("workerNode")
        total = done.setdefault(node, {"files": 0, "bytes": 0})
        total["files"] += int(child.get("putObjectCount") or 0)
        total["bytes"] += int(child.get("putObjectSize") or 0)
    _require(all(done.get(node, {}).get("files", 0) > 0
                 and done.get(node, {}).get("bytes", 0) >= 1024 * 1024 for node in h.workers),
             "Both agents must actually transfer payload, not only empty/tiny objects: " + str(done))
    return done


def _completed(h, task_id):
    task = h.wait_task(task_id, timeout=180)
    _require(task.get("status") == "COMPLETED", "Task did not complete: " + str(task))
    _require(int(task.get("failedObjectCount") or 0) == 0, "Completed task contains failed objects")
    _require(not task.get("partial"), "Completed task has an incomplete node snapshot")
    return task


def _report(h, task_id, expected_keys, expected_results):
    report = h.report(task_id)
    metadata, rows = report["metadata"], report["rows"]
    expected_keys = set(expected_keys)
    count = len(expected_keys)
    _require(metadata.get("taskId") == task_id, "Report metadata belongs to a different task")
    _require(all(row.get("parentTaskId") == str(task_id) for row in rows),
             "Report contains rows from a different parent task")
    _require(metadata.get("complete") is True, "Report is incomplete: " + str(metadata))
    _require(metadata.get("coverage") == "COMPLETE", "Report coverage is not complete")
    _require(metadata.get("countsReconciled") is True, "Report counts do not reconcile")
    _require(not metadata.get("unreachableNodes"), "Report has unreachable nodes")
    _require(len(rows) == count, f"Report has {len(rows)} rows; expected {count}")
    _require({row.get("result") for row in rows} <= set(expected_results),
             "Report contains unexpected results")
    logical_keys = {row["logicalKey"] for row in rows}
    _require(len(logical_keys) == count, "Report repeats logical object identities")
    _require(logical_keys == expected_keys,
             "Report object identities differ: missing=" + str(sorted(expected_keys - logical_keys))
             + ", unexpected=" + str(sorted(logical_keys - expected_keys)))
    return metadata


def upload(h):
    """CLI -> two real agents -> S3, followed by independent content verification."""
    args = ["task", "create", "local-to-oss", "ci/" + h.source_bucket,
            "--conflict", "overwrite"]
    for node in h.workers:
        args += ["--binding", node + "=/data/source"]
    task_id = _created(h, h.cli(*args))
    _completed(h, task_id)
    h.assert_bucket(h.source_bucket)
    workers = _both_workers(h, task_id)
    metadata = _report(h, task_id, h.source_manifest, {"SUCCESS"})
    h.record("upload", {"taskId": task_id, "completedByAgent": workers,
                        "independentlyVerifiedFiles": len(h.source_manifest),
                        "reportComplete": metadata["complete"]})
    return task_id


def validate_browser_copy(h, task_id, started_at, completed_at):
    """Validate the task submitted by the browser, including a coarse rate bound."""
    _completed(h, task_id)
    elapsed = completed_at - started_at
    _require(elapsed > 0, "Browser completion must follow task creation")
    size = sum(item["size"] for item in h.source_manifest.values())
    # 8 decimal Mbps. This is an elapsed-time guard against a missing/shared-
    # quota regression, not an instantaneous wire-rate measurement. Include a
    # 20% allowance for bursts/timing; record that scheduling can hide overspeed.
    minimum = size * 8 / 8_000_000 * 0.8
    _require(elapsed >= minimum,
             f"8 Mbps task copied {size} bytes in {elapsed:.3f}s (minimum {minimum:.3f}s)")
    h.assert_bucket(h.target_bucket, prefix="browser/")
    workers = _both_workers(h, task_id)
    _require(sum(item["bytes"] for item in workers.values()) == size,
             "Agent byte totals differ from the independent source manifest")
    _require(all(size * 0.4 <= workers[node]["bytes"] <= size * 0.6 for node in h.workers),
             "Copy payload is not balanced across both agents: " + str(workers))
    metadata = _report(h, task_id, h.source_manifest, {"SUCCESS"})
    h.record("browser-copy", {"taskId": task_id, "completedByAgent": workers,
                              "payloadBytes": size, "elapsedSeconds": elapsed,
                              "minimumSeconds": minimum, "configuredMbps": 8,
                              "rateCheckScope": "Elapsed-time lower bound only; scheduling delays can hide modest overspeed.",
                              "reportComplete": metadata["complete"]})


def _fingerprint(path):
    stat = path.stat()
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return {"inode": stat.st_ino, "mtimeNs": stat.st_mtime_ns,
            "size": stat.st_size, "sha256": digest.hexdigest()}


def _parts(root):
    return list(root.rglob(".transfer-*.part"))


def _control(h, verb, task_id):
    result = _data(h.cli("task", verb, str(task_id)))
    details = result.get("details", [])
    _require(len(details) == 1 and details[0].get("taskIds") == [task_id],
             "Control did not target the requested task: " + str(result))
    operation = _data(details[0]["result"])
    _require(int(operation.get("failed") or 0) == 0, "Task control failed: " + str(operation))
    _require(int(operation.get("accepted") or 0) > 0, "Task control accepted nothing: " + str(operation))


def download_recovery(h):
    """Stop, restart master, resume, SIGKILL the agent, recover the same task."""
    root = Path(h.download_dir)
    final_root = root / "browser"
    final_root.mkdir(parents=True, exist_ok=True)
    final_root.chmod(0o777)
    large_key = max(h.source_manifest, key=lambda key: h.source_manifest[key]["size"])
    large_size = h.source_manifest[large_key]["size"]
    max_other_size = max(value["size"] for key, value in h.source_manifest.items() if key != large_key)
    _require(large_size > max_other_size, "Recovery fixture must have one uniquely largest object")
    sentinel = final_root / large_key
    sentinel.parent.mkdir(parents=True, exist_ok=True)
    sentinel.write_bytes(b"Existing destination must survive an interrupted overwrite.\n")
    sentinel.chmod(0o666)
    original = _fingerprint(sentinel)
    checkpoint = final_root / "00-small.txt"
    task_id = _created(h, h.cli("task", "create", "oss-to-local", "ci/" + h.target_bucket,
                              "--binding", h.workers[0] + "=/data/download", "--prefix", "browser/",
                              "--conflict", "overwrite", "--bandwidth-mbps", "8"))

    def in_flight():
        task = h.task(task_id)
        _require(task.get("status") not in {"FAILED", "PARTIAL_FAILED", "COMPLETED", "STOPPED"},
                 "Task ended before interruption could be exercised: " + str(task))
        if not checkpoint.exists():
            return False
        if _fingerprint(checkpoint)["sha256"] != h.source_manifest["00-small.txt"]["sha256"]:
            return False
        for part in _parts(root):
            try:
                part_size = part.stat().st_size
                # Filenames identify temporary ownership, not the source key.
                # Only the unique largest source can yield a partial this large.
                if max_other_size < part_size < large_size:
                    _require(_fingerprint(sentinel) == original, "Large destination replaced before interruption")
                    return {"partialBytes": part_size, "sourceBytes": large_size,
                            "largestOtherSourceBytes": max_other_size}
            except FileNotFoundError:
                continue  # Another small object can atomically finish during the observation.
        return False

    stop_observation = _until(in_flight, "completed checkpoint and partial large download", timeout=90)
    _control(h, "stop", task_id)

    def stopped():
        task = h.task(task_id)
        _require(task.get("status") not in {"FAILED", "PARTIAL_FAILED", "COMPLETED"},
                 "Unexpected stop result: " + str(task))
        return task.get("status") == "STOPPED" and not _parts(root)

    _until(stopped, "stopped task and closed temporary files")
    checkpoint_before = _fingerprint(checkpoint)
    _require(_fingerprint(sentinel) == original, "Stop overwrote the previous destination")
    stable = {str(path.relative_to(root)): _fingerprint(path)
              for path in root.rglob("*") if path.is_file()}
    time.sleep(1)
    _require(stable == {str(path.relative_to(root)): _fingerprint(path)
                        for path in root.rglob("*") if path.is_file()}, "Files changed after stop settled")

    h.restart_master(kill=True)
    _require(h.task(task_id).get("status") == "STOPPED", "Master restart forgot the durable stop")
    _require(_fingerprint(sentinel) == original, "Stopped task wrote data during master restart")
    h.record("download-stop-persisted", {"taskId": task_id, "stateAfterMasterRestart": "STOPPED",
                                          "partialFilesAfterStop": 0, "previousDestinationPreserved": True,
                                          "beforeStop": stop_observation})
    _control(h, "resume", task_id)
    kill_observation = _until(in_flight, "resumed partial large download before SIGKILL", timeout=90)
    h.stop_agent(0, kill=True)
    # The agent process is gone: a partial target must not have become a final file.
    _require(_fingerprint(sentinel) == original, "Agent termination replaced the existing destination")
    partials_after_kill = len(_parts(root))
    _require(partials_after_kill > 0, "SIGKILL did not interrupt a real partial download")
    h.record("download-agent-killed", {"taskId": task_id, "partialFilesAfterKill": partials_after_kill,
                                       "previousDestinationPreserved": True,
                                       "beforeAgentKill": kill_observation})
    h.start_agent(0)
    state = h.task(task_id).get("status")
    manual_resume = state in {"STOPPED", "FAILED", "PARTIAL_FAILED"}
    if manual_resume:
        _control(h, "resume", task_id)
    _completed(h, task_id)
    h.assert_download()
    _require(not _parts(root), "Recovery left owned partial files behind")
    _require(_fingerprint(checkpoint) == checkpoint_before,
             "An already completed checkpoint file was rewritten during resume")
    metadata = _report(h, task_id, {"browser/" + key for key in h.source_manifest}, {"SUCCESS", "SKIPPED"})
    h.record("download-recovery", {"taskId": task_id, "sameTaskIdThroughout": True,
                                   "masterKillPreservedStoppedState": True,
                                   "agentKilledDuringActualIO": True,
                                   "beforeStop": stop_observation,
                                   "beforeAgentKill": kill_observation,
                                   "partialFilesAfterKill": partials_after_kill,
                                   "resumeAfterAgentRestart": "explicit" if manual_resume else "automatic",
                                   "completedFileUnchanged": checkpoint_before,
                                   "reportComplete": metadata["complete"]})
    return task_id


def conflicts(h):
    """Exercise real remote SKIP and OVERWRITE against intentionally different bytes."""
    key = "conflict-check.bin"
    source, old = b"new contents from the source\n", b"old destination must survive SKIP\n"
    h.s3.put_object(Bucket=h.source_bucket, Key=key, Body=source)
    h.s3.put_object(Bucket=h.target_bucket, Key=key, Body=old)
    task_ids = []
    for policy, expected, report_result in (("skip", old, "SKIPPED"), ("overwrite", source, "SUCCESS")):
        task_id = _created(h, h.cli("task", "create", "oss-to-oss", "ci/" + h.source_bucket,
                                  "ci/" + h.target_bucket, "--object-key", key,
                                  "--node", h.workers[0], "--conflict", policy))
        task_ids.append(task_id)
        _completed(h, task_id)
        response = h.s3.get_object(Bucket=h.target_bucket, Key=key)
        try:
            actual = response["Body"].read()
        finally:
            response["Body"].close()
        _require(actual == expected, f"{policy.upper()} produced incorrect destination bytes")
        _report(h, task_id, {key}, {report_result})
    h.record("conflict-policies", {"taskIds": task_ids, "skipPreservedOldBytes": True,
                                   "overwriteMatchedSourceBytes": True})
