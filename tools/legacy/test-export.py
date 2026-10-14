#!/usr/bin/env python3
"""Test the read-only exporter with the exact RocksDB library in a V4 package."""
import argparse
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tarfile
import tempfile
import zipfile

ROOT = Path(__file__).resolve().parents[2]
PACKAGE_SHA256 = "64a0e80a28d61322fad20be17c8d47c31f77565a76daf5b1f4718590f9c992d4"


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def b64(value):
    return base64.urlsafe_b64encode(value).decode().rstrip("=")


def un64(value):
    return base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))


def db_identity(path):
    return {str(p.relative_to(path)): (p.stat().st_ino, p.stat().st_size,
                                     p.stat().st_mtime_ns, digest(p))
            for p in path.rglob("*") if p.is_file()}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--jdk-home", required=True, type=Path)
    parser.add_argument("--package", type=Path, default=ROOT / "output/oss-transfer.tar.gz")
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--keep-fixtures", action="store_true")
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, mode=0o700, exist_ok=False)
    package_sha = digest(args.package)
    if package_sha != PACKAGE_SHA256:
        raise RuntimeError("V4 package is not the pinned fixture artifact")
    java = args.jdk_home / "bin/java"
    javac = args.jdk_home / "bin/javac"
    env = dict(os.environ, JAVA_HOME=str(args.jdk_home))
    checks = []
    with tempfile.TemporaryDirectory(prefix="legacy-export-qa-") as scratch:
        root = Path(scratch)
        legacy = root / "oss-transfer.jar"
        with tarfile.open(args.package, "r:gz") as archive:
            member = archive.getmember("oss-transfer/oss-transfer.jar")
            if not member.isfile():
                raise RuntimeError("Legacy JAR is not a regular archive member")
            with archive.extractfile(member) as source, legacy.open("wb") as target:
                for block in iter(lambda: source.read(1024 * 1024), b""):
                    target.write(block)
        jar_sha = digest(legacy)
        library = root / "rocksdbjni.jar"
        with zipfile.ZipFile(legacy) as archive:
            names = [n for n in archive.namelist() if n.startswith("BOOT-INF/lib/rocksdbjni-") and n.endswith(".jar")]
            if len(names) != 1:
                raise RuntimeError("Ambiguous RocksDB library")
            library.write_bytes(archive.read(names[0]))
        subprocess.run(["bash", str(ROOT / "tools/legacy/build.sh"), str(root / "exporter")], env=env, check=True, capture_output=True)
        subprocess.run([str(javac), "-cp", str(library), "-d", str(root), str(ROOT / "tools/legacy/test/WriteFixture.java"), str(ROOT / "tools/legacy/test/HashFixture.java")], check=True)
        credential = b64(bytes([7]) * 32)
        hashed = subprocess.run([str(java), "-Dloader.path=" + str(root), "-Dloader.main=HashFixture", "-cp", str(legacy),
                                 "org.springframework.boot.loader.PropertiesLauncher", credential],
                                text=True, capture_output=True, check=True, timeout=30)
        credential_hash = hashed.stdout.strip()
        assert credential_hash == b64(hashlib.sha256(credential.encode()).digest()), "V4 Agent credential hash encoding changed"
        checks.append("Pinned V4 AgentRegistryService.hash executes without Spring startup and matches base64url SHA-256 before Go hex-index conversion")
        exporter = root / "exporter/legacy-exporter.jar"
        config = root / "application.yml"
        config.write_text("transfer.master-web-url: http://127.0.0.1:1685\nrate-limit.bandwidth: 8000000\n")
        config.chmod(0o600)
        manifest = "11111111-1111-1111-1111-111111111111"
        salt = "0123456789abcdef0123456789abcdef"
        password_hash = salt + ":" + hashlib.sha256((salt + "fixture password").encode()).hexdigest()
        key = "目录/a\n b.bin"
        values = {
            "node_config": {"masterIp": "master", "localIp": "master", "workerIps": ["master", "worker"], "advertisedLocalRoots": ["/data/files"]},
            "user:fixture": {"username": "fixture", "passwordHash": password_hash, "role": "admin", "createTime": 1728000000000},
            "s3_config_fixture": {"alias": "fixture", "endpoint": "http://127.0.0.1:8333", "accessKey": "fixture-access", "secretKey": "fixture-secret-must-not-print", "useV2SignerType": False, "pathStyleEnable": True},
            "agent:member:worker": {"address": "worker", "instanceId": "c022c04d-cb87-4f66-a805-d401c3b95e91", "enrollmentId": "9ea218a7-fb2e-42a2-85fa-2838ea316557", "credentialHash": credential_hash, "enabled": True},
            "task:id:list": [224], "task:id:sequence": 300,
            "task:224": {"taskIndex": 224, "parentTaskId": 224, "direction": "OSS_TO_OSS", "status": "STOPPED", "startKey": "a", "endKey": "z", "marker": "m", "inventoryManifestId": manifest},
            "task:deleted:225": {"taskIndex": 225, "status": "PARTIAL_FAILED", "direction": "OSS_TO_OSS"},
            "task:id:list:deleted": [225], "task:purged:226": True, "report:purged:226": True,
            "task_creation:creation": {"id": "creation", "publicTaskId": 224, "taskIds": [224, 225], "generation": 8, "marker": "m", "pendingDispatches": [{"worker": "worker", "firstTaskId": 227, "tasks": [{"startKey": "m", "endKey": "z"}]}]},
            "local:parent:300": {"parentTaskId": 300, "status": "STOPPED", "orchestrationComplete": False},
            "local:explore:300": {"marker": "m", "pendingTasks": [{"startKey": "m", "endKey": "z"}]},
            "local:discovery:300": {"parentTaskId": 300, "status": "STOPPED", "lastCommittedRelativeKey": "m", "pendingBatch": {"entries": [{"relativeKey": key, "size": 123}]}},
            "local:stop:300": {"taskId": 300, "stopped": True},
            "completed:224:" + b64(key.encode()): {"taskId": 224, "logicalKey": key, "size": 123, "completedAt": 1728000000000},
            "file-report:224:coverage": {"ledgerCoverage": "TRACKED", "scanComplete": False},
            "file-report:224:item:" + b64(key.encode()): {"reportTaskId": 224, "taskId": 224, "logicalKey": key, "result": "SUCCESS", "size": 123},
            "inventory:" + manifest + ":ready": True,
            "inventory:" + manifest + ":entry:" + b64(key.encode()): {"key": key},
            "rate_limit:manual:bandwidth": 8000000,
            "task_bandwidth:desired:creation:test": {"scopeId": "creation:test", "limitMbps": 5.5, "members": ["master", "worker"], "revision": 4},
            "failed:count": 1, "failed:1": {"taskIndex": 225, "key": "missing", "errorMessage": "fixture source missing"},
            "future:unknown": {"@type": "must.not.BeInstantiated", "raw": "\u0000你好"},
        }
        encoded = {k.encode(): json.dumps(v, ensure_ascii=False, separators=(",", ":")).encode() for k, v in values.items()}
        records = root / "entries.tsv"
        records.write_text("".join(b64(k) + "\t" + b64(v) + "\n" for k, v in encoded.items()))
        db = root / "rocksdb"
        writer = [str(java), "-cp", str(root) + os.pathsep + str(library), "WriteFixture", str(db), str(records)]
        subprocess.run(writer, check=True, capture_output=True)
        before = db_identity(db)
        assert any(p.suffix == ".log" and p.stat().st_size for p in db.iterdir()), "fixture should contain unflushed WAL"
        snapshot = root / "master.jsonl"
        command = [str(java), "-jar", str(exporter), "--legacy-jar", str(legacy), "--expected-jar-sha256", jar_sha,
                   "--database", str(db), "--node", "master", "--role", "master", "--config", str(config), "--output", str(snapshot)]
        result = subprocess.run(command, text=True, capture_output=True, timeout=60)
        assert result.returncode == 0, result.stderr
        assert db_identity(db) == before, "export changed source DB files"
        assert stat.S_IMODE(snapshot.stat().st_mode) == 0o600
        rows = [json.loads(line) for line in snapshot.read_bytes().splitlines()]
        actual = {un64(r["key"]): un64(r["value"]) for r in rows if r["type"] == "entry"}
        assert actual == encoded, "raw keys or values were lost or rewritten"
        assert un64(rows[1]["value"]) == config.read_bytes()
        assert rows[-1]["entries"] == len(encoded)
        assert rows[-1]["sha256"] == hashlib.sha256(b"\n".join(snapshot.read_bytes().splitlines()[:-1]) + b"\n").hexdigest()
        checks.append("Real V4 RocksDB WAL records exported byte-for-byte; database inode/content/mtime unchanged; output 0600")
        original = snapshot.read_bytes()
        assert subprocess.run(command, capture_output=True).returncode != 0
        assert snapshot.read_bytes() == original
        checks.append("Existing export is never overwritten")
        inspect = subprocess.run(["go", "run", "./tools/legacy/inspect", str(snapshot)], cwd=ROOT, text=True, capture_output=True, timeout=60)
        assert inspect.returncode == 0, inspect.stderr
        plan = json.loads(inspect.stdout)
        assert not plan["readyToImport"] and not plan["nodes"][0]["readyToImport"]
        assert plan["nodes"][0]["users"][0]["hashKind"] == "V4_SHA256"
        assert plan["nodes"][0]["connections"][0]["region"] == "default"
        assert any(i["code"] == "NODE_SNAPSHOT_MISSING" and i["scope"] == "worker" for i in plan["issues"])
        assert "fixture-secret-must-not-print" not in inspect.stdout and password_hash not in inspect.stdout
        (output / "plan.json").write_text(inspect.stdout)
        checks.append("Go preflight reports missing node snapshot and conversion requirements without credentials")
        broken = root / "truncated.jsonl"
        broken.write_bytes(b"\n".join(original.splitlines()[:-1]) + b"\n")
        rejected = subprocess.run(["go", "run", "./tools/legacy/inspect", str(broken)], cwd=ROOT, capture_output=True, timeout=60)
        assert rejected.returncode != 0
        checks.append("Go inspector rejects missing completion footer")
        # A real RocksDB process holds an exclusive lock; export must fail before publication.
        live = subprocess.Popen(writer + ["hold"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            assert live.stdout.readline().strip() == "READY"
            occupied = root / "occupied.jsonl"
            blocked = subprocess.run(command[:-1] + [str(occupied)], text=True, capture_output=True, timeout=30)
            assert blocked.returncode != 0 and not occupied.exists(), blocked.stderr
            assert "database is in use" in blocked.stderr
        finally:
            live.communicate("\n", timeout=10)
        checks.append("Live RocksDB writer prevents export; no partial destination is published")
        evidence = {"packageSha256": package_sha, "legacyJarSha256": jar_sha, "rocksdbJniSha256": digest(library),
                    "exporterSha256": digest(exporter), "snapshotSha256": hashlib.sha256(original).hexdigest(), "entryCount": len(encoded), "checks": checks,
                    "scope": "Offline exporter and mapping preflight only. No runtime conversion or migrated-task recovery is claimed."}
        if args.keep_fixtures:
            fixtures = output / "fixtures"
            fixtures.mkdir(mode=0o700)
            retained = fixtures / "master.jsonl"
            retained.write_bytes(original)
            retained.chmod(0o600)
            worker_values = {
                "node_config": {"localIp": "worker", "advertisedLocalRoots": ["/data/files"]},
                "s3_config_fixture": values["s3_config_fixture"],
                "agent:identity": {"masterUrl": "http://127.0.0.1:1685", "address": "worker", "credential": credential,
                                   "instanceId": values["agent:member:worker"]["instanceId"], "joined": True},
                "rate_limit:manual:bandwidth": 8000000,
            }
            worker_records = root / "worker.tsv"
            worker_records.write_text("".join(b64(k.encode()) + "\t" + b64(json.dumps(v, separators=(",", ":")).encode()) + "\n" for k, v in worker_values.items()))
            worker_db = root / "worker-rocksdb"
            subprocess.run([str(java), "-cp", str(root) + os.pathsep + str(library), "WriteFixture", str(worker_db), str(worker_records)], check=True, capture_output=True)
            subprocess.run([str(java), "-jar", str(exporter), "--legacy-jar", str(legacy), "--expected-jar-sha256", jar_sha,
                            "--database", str(worker_db), "--node", "worker", "--role", "agent", "--config", str(config), "--output", str(fixtures / "worker.jsonl")], check=True, capture_output=True)
            evidence["retainedFixtures"] = ["fixtures/master.jsonl", "fixtures/worker.jsonl"]
            evidence["fixturePurpose"] = "Synthetic records written with real pinned RocksDB JNI; task migration/recovery has not yet been verified."
        (output / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print(str(output / "evidence.json"))


if __name__ == "__main__":
    main()
