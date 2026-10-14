#!/usr/bin/env python3
"""Real local-inventory CLI uploads, durable sealing and exact-key S3 transfers."""

import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import csv
import io
import zipfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
import os
from pathlib import Path
import socket
import subprocess
import threading
from urllib.error import HTTPError
from urllib.request import Request, urlopen

MODULE = importlib.util.spec_from_file_location("cluster_inventory", Path(__file__).with_name("go-cluster.py"))
cluster = importlib.util.module_from_spec(MODULE)
MODULE.loader.exec_module(cluster)
require, eventually = cluster.require, cluster.eventually


class ResponseLoss:
    """Forward to the real Master, then discard one confirmed mutation response."""
    def __init__(self, harness):
        owner = self
        self.harness = harness
        self.lock = threading.Lock()
        self.drop_suffix = None
        self.dropped = []

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def forward(self):
                length = int(self.headers.get("Content-Length", "0"))
                require(0 <= length <= 1024 * 1024, "Unexpected oversized CLI request")
                body = self.rfile.read(length) if length else None
                headers = {name: self.headers[name] for name in ("Authorization", "Content-Type", "Idempotency-Key") if name in self.headers}
                request = Request(harness.origin + self.path, data=body, headers=headers, method=self.command)
                try:
                    response = urlopen(request, timeout=40)
                except HTTPError as error:
                    response = error
                with response:
                    payload, status = response.read(), response.status
                with owner.lock:
                    drop = status == 200 and owner.drop_suffix and self.path.endswith(owner.drop_suffix)
                    if drop:
                        owner.dropped.append({"path": self.path, "status": status})
                        owner.drop_suffix = None
                if drop:
                    self.close_connection = True
                    self.connection.shutdown(socket.SHUT_RDWR)
                    self.connection.close()
                    return
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            do_GET = do_POST = do_PUT = forward

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.origin = "http://127.0.0.1:" + str(self.server.server_port)

    def close(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)


class InventoryAcceptance:
    def __init__(self, args):
        args.scenario, args.single_upload, args.fixture = "inventory", False, None
        self.args = args
        self.h = cluster.Harness(args)
        self.proxy = None
        self.counter = 0
        self.h.evidence["binaries"]["cli"] = cluster.store_qa.binary_provenance(args.cli, "--version")

    def cli(self, directory, request_id, expected=0, nodes=None):
        command = ["import-inventory",
                   "cluster-qa/" + self.h.buckets["source"], "cluster-qa/" + self.h.buckets["destination"],
                   str(directory), "--overwrite", "--request-id", request_id]
        for node in nodes or []:
            command.extend(["--node", node])
        payload = self.cli_command(command, expected)
        require(payload.get("requestId") == request_id, "CLI lost the reusable root request ID")
        return payload

    def cli_command(self, arguments, expected=0):
        self.counter += 1
        command = [str(self.args.cli.resolve()), "--host", self.proxy.origin, "--json", *arguments]
        environment = dict(self.h.environment, OSS_TRANSFER_AUTH_TOKEN=self.h.token,
                           OSS_TRANSFER_CONFIG_FILE=str(self.h.root / "no-java-config"), OSS_TRANSFER_INTERNAL_TOKEN="")
        result = subprocess.run(command, env=environment, cwd=self.h.root, capture_output=True,
                                text=True, encoding="utf-8", timeout=150)
        (self.h.root / f"cli-inventory-{self.counter}.log").write_text(self.h.scrub(result.stdout + "\n" + result.stderr), encoding="utf-8")
        require(result.returncode == expected, f"Inventory CLI exited {result.returncode}, expected {expected}; inspect sanitized CLI log")
        return json.loads(result.stdout)

    def task_ids(self):
        return {int(row["taskIndex"]) for row in self.h.api("/api/master/status")["tasks"]}

    def run(self):
        h = self.h
        h.start_s3()
        h.start_master(first=True)
        for index in range(2):
            h.start_agent(index, first=True)
        eventually(h.online, "two inventory Agents online")
        h.api("/api/master/s3-config", h.storage)
        self.proxy = ResponseLoss(h)

        manifest, contents = {}, {}
        for index in range(258):
            key = f"selected/object-{index:04d}"
            contents[key] = hashlib.sha256(key.encode()).digest() * (1 + index % 8)
        contents[" selected/leading and trailing "] = b"untrimmed object key"
        contents["中文目录/空 格.txt"] = b"unicode object key"
        for key, payload in contents.items():
            manifest[key] = {"size": len(payload), "sha256": hashlib.sha256(payload).hexdigest()}
        all_contents = dict(contents, **{"outside-inventory/keep-out": b"must not copy", "selected/not-listed": b"must not scan prefix"})
        with ThreadPoolExecutor(max_workers=8) as pool:
            list(pool.map(lambda pair: h.s3.put_object(Bucket=h.buckets["source"], Key=pair[0], Body=pair[1]), all_contents.items()))
        directory = h.root / "inventories"
        directory.mkdir()
        keys = list(contents)
        (directory / "selected.incr").write_bytes(("\r\n  \t\r\n" + "\r\n".join(keys + keys[:3]) + "\r\n").encode())
        (directory / "not-an-inventory.txt").write_text("outside-inventory/keep-out\n", encoding="utf-8")

        # A committed batch with a lost response must remain an upload, with no
        # task yet. Killing the Master proves this is in its database, not RAM.
        self.proxy.drop_suffix = "/batches/2"
        failed = self.cli(directory, "inventory-durable-upload", expected=1, nodes=h.nodes)
        require(not self.task_ids(), "Unsealed partial inventory created a runnable task")
        require(len(self.proxy.dropped) == 1, "Batch response was not actually discarded")
        import_id = failed["files"][0]["importId"]
        before = h.api("/api/master/inventory-imports/" + import_id)
        require(before["state"] == "UPLOADING" and before["uniqueCount"] > 128, "Second batch was not durable before disconnect")
        for process in [h.master, *h.agent_processes]:
            process.kill()
            process.wait(timeout=15)
        h.start_master(first=False)
        for index in range(2):
            h.start_agent(index, first=False)
        eventually(h.online, "original inventory Agents reconnected")
        after = h.api("/api/master/inventory-imports/" + import_id)
        require(after["state"] == "UPLOADING" and after["uniqueCount"] == before["uniqueCount"], "Master restart lost acknowledged inventory batches")
        require(not self.task_ids(), "Restart auto-ran an unsealed inventory")
        result = self.cli(directory, "inventory-durable-upload", nodes=h.nodes)
        require(len(result["taskIds"]) == 1, "One inventory file did not produce one parent")
        task_id = int(result["taskIds"][0])
        h.verify_task(task_id, manifest, both=False)
        used = {row["executionNodeIp"] for row in h.children_for(task_id)}
        require(used == set(h.nodes), "Selected Agents did not both receive inventory work")
        h.verify_s3(h.buckets["destination"], "", manifest)
        h.verify_report(task_id, manifest, "OSS_TO_OSS", "", "")
        h.record("CLI-local CRLF inventory batches survive SIGKILL before seal; exact-key SHA and duplicate-free parent/report verified",
                 taskId=task_id, uniqueKeys=len(manifest), rawKeys=len(keys) + 3, agents=sorted(used))

        # The normal task DTO must retain its exact-key scope. Exporting that
        # DTO to JSON and importing it as a bucket/range task must fail locally.
        shown = self.cli_command(["task", "list"])
        exported = shown.get("tasks") or shown.get("data", {}).get("tasks")
        require(exported and len(exported) == 1 and exported[0].get("inventoryManifestId"),
                "CLI task output discarded the sealed inventory scope")
        export_path = h.root / "exported-inventory.json"
        export_path.write_text(json.dumps(exported), encoding="utf-8")
        rejected = self.cli_command(["task", "import", "tasks", str(export_path)], expected=1)
        require("import-inventory" in rejected.get("error", "") and self.task_ids() == {task_id},
                "Ordinary JSON import expanded an inventory task to a bucket scan")
        h.record("CLI task JSON preserves inventory scope and ordinary reimport explicitly rejects it")

        # A real parent is created before the transport discards finalize's
        # response. Retrying the same CLI invocation must find that parent.
        self.proxy.drop_suffix = "/finalize"
        failed = self.cli(directory, "inventory-lost-finalize", expected=1, nodes=[h.nodes[1]])
        require(len(self.task_ids()) == 2 and len(self.proxy.dropped) == 2, "Finalize was not committed before response loss")
        retry = self.cli(directory, "inventory-lost-finalize", nodes=[h.nodes[1]])
        second = int(retry["taskIds"][0])
        require(len(self.task_ids()) == 2 and second != task_id, "Finalize replay created another parent")
        h.verify_task(second, manifest, both=False)
        require({row["executionNodeIp"] for row in h.children_for(second)} == {h.nodes[1]}, "Inventory ignored explicit Agent selection")
        h.verify_s3(h.buckets["destination"], "", manifest)
        h.record("lost finalize response reuses the persisted parent; explicit Agent selection is preserved", taskId=second)

        # Mutation of the CLI-local file may never reinterpret the same request
        # identity as a different selection or silently create another task.
        (directory / "selected.incr").write_text("outside-inventory/keep-out\n", encoding="utf-8")
        rejected = self.cli(directory, "inventory-lost-finalize", expected=1, nodes=[h.nodes[1]])
        require("409" in rejected.get("error", "") and len(self.task_ids()) == 2, "Changed inventory was accepted under its original request ID")
        h.record("changed local manifest is rejected under the same request ID before creating work")

        empty_dir = h.root / "empty-inventory"
        empty_dir.mkdir()
        (empty_dir / "empty.incr").write_text("\n  \t\r\n", encoding="utf-8")
        empty = self.cli(empty_dir, "inventory-empty", nodes=[h.nodes[0]])
        empty_id = int(empty["taskIds"][0])
        current = h.wait_task(empty_id)
        require(current["listObjectCount"] == 0 and current["putObjectCount"] == 0, "Empty inventory fell back to scanning the bucket")
        h.record("empty inventory finishes without scanning or copying bucket objects", taskId=empty_id)

        missing_dir = h.root / "missing-inventory"
        missing_dir.mkdir()
        (missing_dir / "missing.incr").write_text(keys[0] + "\nmissing-object\n", encoding="utf-8")
        missing = self.cli(missing_dir, "inventory-missing", nodes=[h.nodes[0]])
        missing_id = int(missing["taskIds"][0])
        task = h.wait_task(missing_id, wanted="PARTIAL_FAILED")
        require(task["listObjectCount"] == 2 and task["putObjectCount"] == 1 and task["failedObjectCount"] == 1,
                "Missing listed object was skipped silently or bucket scanning added objects")
        with zipfile.ZipFile(io.BytesIO(h.task_report(missing_id))) as report:
            rows = list(csv.DictReader(io.StringIO(report.read("file-results.csv").decode("utf-8-sig"))))
            indexed = {row["logicalKey"]: row for row in rows}
            require(len(rows) == 2 and set(indexed) == {keys[0], "missing-object"}
                    and indexed[keys[0]]["result"] == "SUCCESS" and indexed["missing-object"]["result"] == "FAILED",
                    "Missing inventory object is absent from the final task report")
        h.verify_s3(h.buckets["destination"], "", manifest)
        h.record("missing selected object is an explicit failure; no unlisted source objects are copied", taskId=missing_id)

    def close(self):
        if self.proxy:
            self.proxy.close()
        self.h.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("master", "agent", "cli", "weed", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    acceptance = InventoryAcceptance(parser.parse_args())
    try:
        acceptance.run()
        acceptance.h.evidence["status"] = "PASSED"
    except BaseException as error:
        acceptance.h.evidence.update(status="FAILED", error=acceptance.h.scrub(str(error)))
        raise
    finally:
        acceptance.close()
        acceptance.h.save()


if __name__ == "__main__":
    main()
