# V4.0.x to V4.1.0 state migration

The offline migration command restores V4.0.x accounts, storage connections, Agent identities and task history into private Go databases. It preserves the original exports. Unfinished tasks remain stopped; their first explicit resume scans the original scope and verifies old successful objects against current source and destination content before reusing them.

The reference package is the [V4.0.6 archive](https://github.com/MoonWX/oss-transfer/releases/tag/v4.0.6), commit `835a1d8`:

```text
SHA-256: 64a0e80a28d61322fad20be17c8d47c31f77565a76daf5b1f4718590f9c992d4
```

The Java implementation is preserved in the [V4.0.6 source](https://github.com/MoonWX/oss-transfer/tree/v4.0.6/src/main/java). Older versions can have fewer keys and incomplete object history; absent records must not be reconstructed as successes.

## Export and inspect

Stop the old Master and every worker before taking copies of their data directories. Export each copy, including the Master: V4.0.x can run transfers on the Master itself. Keep the original databases and application settings for rollback.

Build the small exporter once with JDK 8 or newer. The target host only needs its existing Java runtime and V4.0.x JAR to run it. The exporter extracts the JAR's own `rocksdbjni` library; it does not start Spring, load application classes, open an HTTP port, or contact storage.

```sh
bash tools/legacy/build.sh
java -jar output/tools/legacy/legacy-exporter.jar \
  --legacy-jar /backup/master/oss-transfer.jar \
  --expected-jar-sha256 '<SHA-256 of that JAR>' \
  --database /backup/master/data/rocksdb \
  --config /backup/master/application.yml \
  --node master-node \
  --role master \
  --output /backup/exports/master.jsonl

go run ./tools/legacy/inspect /backup/exports/master.jsonl /backup/exports/worker.jsonl
```

Use the persisted `node_config.localIp` or `agent:identity.address` as `--node`. Role is `master`, `agent`, or `worker`; `worker` means the older directly addressed worker without an Agent credential. Repeat export for every execution node. A Master export's `node_config.workerIps` is checked against the supplied node snapshots.

Export currently supports POSIX filesystems. It requires a stopped, existing RocksDB database with a `CURRENT` file, takes a shared lock that conflicts with the old writer, and opens RocksDB read-only. Its log files go to a separate private directory. It never creates a missing source database. Node snapshots must still be coordinated operationally; individual database locks are not a distributed snapshot protocol, and network filesystem locking must not be assumed reliable.

The output is a new file with mode `0600`. The exporter refuses an existing destination, verifies source file identities and sizes/timestamps before publication, flushes output to disk, then publishes without replacing another file. An interrupted export has no published destination. Exports contain credentials and password hashes; keep them in the same protected backup storage as the old databases.

Each JSONL export contains:

- A header: format version, node, role, export timestamp, V4.0.x JAR SHA-256 and nested RocksDB library SHA-256.
- An optional configuration attachment containing the exact bytes of `application.yml`.
- Every RocksDB key and value, encoded separately as unpadded base64url. Entries stay in RocksDB byte order. Unknown keys are retained.
- A footer: entry count, decoded key/value byte counts, and SHA-256 over every preceding line, including newlines.

The inspector rejects altered counts or checksums, missing/truncated footers, duplicate or unordered keys, duplicate envelope fields, unknown envelope types and oversized entries. It reads the complete export before returning a plan. It does not deserialize Java types, evaluate YAML placeholders, run shell expressions or expose connection secrets and password hashes in its output. The current bounds are 1 MiB per key, 64 MiB per value, 4 MiB per config attachment and 100,000 mapping facts; exceeding a bound is an error, never a truncation.

An application file may differ from the effective configuration because of environment variables or command-line overrides. Capture those explicitly during the stopped deployment audit. The inspector preserves the file but does not infer effective values from it.

## Actual V4.0.x keys and mapping

`RocksDBService` uses one default column family. Keys are UTF-8, values are JSON written by Fastjson. The exporter preserves their original bytes rather than serializing model classes again.

| V4.0.x key | Stored fields and meaning | Go mapping / conversion requirement |
| --- | --- | --- |
| `user:<username>` | `username`, `passwordHash`, `displayName`, `role`, `createTime`, `lastLoginTime` | `users/<username>`. Preserve roles and hashes; set a new account revision. Retain timestamps in migration history if the Go user model has no matching fields. |
| `s3_config_<alias>` | `alias`, `endpoint`, `accessKey`, `secretKey`, `useV2SignerType`, `pathStyleEnable` | `storage-configuration/master` and authenticated Agent snapshots. V4.0.x dynamic connections sign with region **`default`**; set it explicitly, because Go's empty region becomes `us-east-1`. Preserve both booleans, including their default meaning. |
| `node_config` | `masterIp`, `localIp`, `port`, `workerIps`, `advertisedLocalRoots`, `nodeLabels` | Master membership plus per-Agent settings. The old Master may need a separate local Agent. Go's Master never performs object I/O. Listener port and public URL remain deployment settings. |
| `agent:member:<address>` | `instanceId`, `enrollmentId`, `address`, `label`, `credentialHash`, `joinedAt`, `lastSeen`, `enabled`, `synchronizedStorage` | `agent-members`, `agent-credentials`, `agent-addresses`. Match each Agent's private identity before activating it. `enabled=false` stays revoked. Do not copy `lastSeen` or storage synchronization as proof of a live Go connection. |
| `agent:identity` | `masterUrl`, `address`, raw `credential`, `instanceId`, `joined` | `agent-identity/self`, obtaining the matching enrollment ID from the Master snapshot. A pending or mismatched enrollment requires review, not a replacement credential generated silently. |
| `agent:invite:<token hash>` | `id`, `address`, `label`, `instanceId`, `expiresAt`, `consumed` | Retain for audit; issue new Go invitations rather than reviving old bootstrap links. |
| `agent:storage-version` | Monotonic connection configuration version | Preserve provenance, then allocate a new coordinated snapshot revision; workers with divergent connections must be reviewed. |
| `rate_limit:manual:list`, `:put`, `:head`, `:bandwidth` | JSON number. Request values are QPS; bandwidth is **bits/s**, zero means unlimited. | Agent `node-control/settings`. Byte limiter receives bandwidth divided by eight, rounded down. Manual overrides absent from the DB depend on effective old config; do not replace them with new defaults. |
| `rate_adjustment:*`, `rate_event:*`, `rate_incident:*`, `rate_incident_event:*`, `rate_incident_gap:*` | Adjustment history, trigger request details, incident indexes and gaps | Retain historical events separately. Java process IDs and in-memory 503/recovery state are not live Go incidents. Preserve the configured automatic-adjustment parameters separately. |
| `task_bandwidth:desired:<scope>` | `scopeId`, nullable `limitMbps`, fixed `members`, `revision` | Resolve the logical scope before creating Go parent allocations. Multiple old worker task IDs may share one `creation:<UUID>` scope; giving every ID the entire cap multiplies throughput. |
| `task_bandwidth:task:<id>`, `task_bandwidth:scope_tasks:<scope>` | Task-to-scope map and scope task-ID list | Keep with the desired policy until logical parents are reconstructed. |
| `task-bandwidth:policy:<base64url scope>` | Worker copy of the policy above | Compare all node copies with the desired Master policy. An acknowledgement alone is not proof of matching policy. |
| `task_creation:<UUID>`, `task_creation_ids` | `publicTaskId`, `taskIds`, original `BucketPair request`, `scanComplete`, `autoStart`, `stopped`, `deleted`, `generation`, `marker`, `nextShardIndex`, **`pendingDispatches`**, participating nodes | Older logical OSS task coordinator. `publicTaskId` identifies the parent, `taskIds` its execution ranges. Preserve unacknowledged dispatches and their original node assignment. `status=COMPLETED` here means **scanning** completed, not that all transfers completed. |
| `local:parent:<id>`, `local:parent:id:list` | `parentTaskId`, original `LocalTransferRequest`, `status`, `orchestrationComplete`, error and timestamps | Master logical parent. Do not infer successful transfer from `orchestrationComplete`. |
| `task:<id>`, `task:id:list`, `task:id:sequence` | `SubTaskStatus`; see below | Execution records live on the node that did the work. Preserve old public parent IDs. Reserve the new ID allocator beyond the maximum retained or allocated old ID; otherwise new tasks can overwrite imported IDs. |
| `task:deleted:<id>`, `task:id:list:deleted` | Retained worker task snapshots | Preserve recycle-bin state and its ledger. Do not make these runnable. |
| `local:parent:deleted:<id>` | Public parent snapshot when recycled | Preserve parent deletion independently of worker retention. |
| `task:purged:<id>`, `local:parent:purged:<id>`, `report:purged:<id>` | Durable permanent-deletion fences | Preserve even if task/report records are gone. They prevent delayed discovery and results from recreating deleted tasks. |
| `local:stop:<id>` | `{taskId, stopped}` | Preserve durable stop intent. New generations must fence old attempts. |
| `local:explore:<parent>` | `marker`, `nextShardIndex`, `complete`, **`pendingTasks`** | Master OSS-to-local discovery boundary. Keep pending ranges; the marker is not a completed-object checkpoint. |
| `local:discovery:<parent>` | Original request, discovery node, `lastCommittedRelativeKey`, `nextBatchSequence`, status, **`pendingBatch`** | Worker local-to-OSS discovery. A batch is persisted before dispatch; neither a pending batch nor the last committed key may be discarded. |
| `local:batch-assignment:*`, `local:shard-assignment:*` | Durable batch/range to internal task-ID assignment | Deduplicate uncertain dispatch acknowledgements without inventing new work or changing its original node. |
| `inventory:<UUID>:entry:<base64url key>`, `inventory:<UUID>:ready` | Immutable imported list entries `{key}`, followed by readiness marker | Preserve the exact key set. Go discovery needs an inventory input path; a bucket or prefix scan is not equivalent. Missing/unready manifests block recovery. |
| `completed:<report task id>:<base64url logical key>` | `taskId`, `direction`, `logicalKey`, `size`, `completedAt` | Historical completion evidence. It contains no source version, source ETag or target identity; it cannot directly become a verified Go `ItemResult.Target`. |
| `file-report:<id>:item:<base64url logical key>` | Per-object source/target paths and keys, size, node, result, reason/error, discovery/completion timestamps | Preserve as the authoritative retained report history. `SUCCESS`, `SKIPPED`, `FAILED` and `UNPROCESSED` must remain distinct. |
| `file-report:<id>:coverage`, `:scan:*`, `:legacy` | `ledgerCoverage`, registered/completed scans, discovered count, revision, errors and bounded backfill cursor | Keep `TRACKED`, `PARTIAL`, `LEGACY_UNKNOWN` and scan completeness. A partial legacy report must not become a complete Go report. |
| `failed:<sequence>`, `failed:count` | Node-local failure records with task, parent, shard, attempt, key, destination key, stage and error | Preserve visible exception history independently of report results. The counter is an allocation high-water mark; deleted entries leave gaps. Do not resurrect cleared exceptions merely because the report retains their failures. |
| `fallback:<sequence>`, `fallback:count` | Java SDK fallback events | Historical data only. Go does not emit the same event; do not invent new fallback activity. |
| `local-temp:<UUID>` | Root path and identity, exact temporary path, target, logical key, task, process owner, stage and file identity | Preserve ownership metadata. Do not import a Java file-key string as a Go inode identity or delete files by matching `.transfer-*`. |

`SubTaskStatus` includes ranges (`startKey`, `endKey`), original endpoint/key mapping, filters, conflict policy, counters, timestamps, `marker`, `scanComplete`, local bindings, source file entries, `parentTaskId`, `shardId`, `shardIndex`, `attempt`, `autoStart`, task bandwidth and `inventoryManifestId`. Local child IDs can be negative; they are node-local internal identities, not the public `parent-number` ID. Use `(old node, taskIndex)` plus shard identity to map them. V4.0.x's `ReportChildTaskIds` gives the established public numbering; retain a mapping when a large old range must become several Go batches of at most 128 objects.

An old logical completion key is generally `parentTaskId` when present and otherwise `taskIndex`. Local upload logical keys can already contain the destination prefix. Use the saved endpoints and file-report source/destination fields; do not assume `logicalKey` always equals the new source-relative key.

## What cannot be copied directly

Go's `execution.Job` contains an immutable list of at most 128 fully described source objects. An old OSS range task contains a range, not that list. An old completed/report record usually has size and a key but no source ETag/version or target identity. Filling those fields with current values would conceal a source change.

Old report coverage can be incomplete even when a task says `COMPLETED`. Preserve the reported historical state and its coverage; do not fabricate missing object rows. A historical success and a presently verified target are different facts.

The old service does not persist a resumable multipart upload ID and completed part list. A partially uploaded large object must be restarted at object level. Unfinished remote multipart uploads and old local spool files require separate ownership-aware cleanup; the exporter performs neither.

V4.0.x login tokens are held only in memory. Import users and invalidate those sessions. The existing Go password verifier accepts V4.0.x `salt:SHA256(salt+password)` hashes; a successful login upgrades the hash to Argon2id without changing the password. Do not create a new default admin over an imported account.

Some V4.0.x data can depend on settings outside RocksDB: public URL, configured node rates, automatic adjustment parameters, process parallelism, multipart settings, source aliases and environment overrides. The plan must account for those instead of silently applying Go defaults. Old TLS trust-all behavior must not be imported as the default trust policy.

Importing every old stopped child into `execution_jobs` is also unsafe: stopped jobs consume the new Agent admission budget, currently 1,024 unfinished jobs. Keep historical manifests outside the runnable queue and feed bounded work as recovery proceeds.

## Conversion and first resume

`legacyimport.Prepare` creates a private staging directory next to a new destination. It refuses an existing destination, including an empty directory. It first copies every export with mode `0600`, then validates the copies; all subsequent mapping reads those private copies. The layout is `master/master.db`, `agents/<instanceId>/agent.db`, `raw/<sequence>.jsonl`, `identity-map.json` and `manifest.json`. `Prepared.Master`, `Prepared.Agents` (keyed by old node identity) and `Prepared.Iterate` are the interfaces for the subsequent task/history converter. Closing the preparation releases DB handles and keeps its private files. An unsuccessful preparation removes only the newly allocated staging directory.

The native `oss-transfer-master migrate` command exposes inspection, preparation, task conversion and status. `PrepareOptions` requires the snapshot paths, an absolute destination that does not exist, the new public Master URL, and explicit `DirectNodes` approvals for fresh Agent identities on each old direct worker and the old Master. Existing enrolled Agents retain their instance and enrollment IDs. The foundation currently runs on POSIX filesystems; Windows preparation returns an explicit error until private output ACLs are implemented.

Every staging database contains `migration/state.pending=true`. Master and Agent startup refuse this state. `prepare-recovery` seals a durable key manifest for each unfinished task; it does not access storage or start transfers. `finalize` requires complete snapshots, task history, these manifests, matching Agent identities, validated service configurations and explicit acceptance of the Go execution defaults. It clears each fence with an absolute final database path, closes the databases and publishes the whole directory with a no-replace atomic rename. An interrupted publication remains restartable from staging, and its databases cannot start there. The original installation and exports are never overwritten.

Master account hashes, display names and roles are preserved; setup configuration is written in the same transaction and no default account or old login session is created. Storage configuration is validated against the Go runtime, stored at one coordinated revision, and replicated to the staged Agent DBs with the old signing region `default`. Existing Agent credentials are checked against the old Master record: V4.0.x stores base64url SHA-256, while Go indexes hex SHA-256. Only after the V4.0.x comparison succeeds is the Go index written. Revoked membership remains revoked and has no active credential/address index. Old invitation tokens are retained only in the raw snapshot.

The foundation also preserves manual node rate overrides and advertised local roots. `MapEffectiveNodeControl` maps an explicitly resolved set of nine V4.0.x node-control properties. It requires every property, rejects unknown keys and placeholders, and never reads the environment or guesses old effective values. `Prepared.WriteRuntimeConfig` requires settings for every staged Agent and an explicit Master listen address, then writes `service.json` files beside each database. Configuration files point at the eventual destination, not the staging or original directories. They are created exclusively and never overwrite an earlier file.

The native Agent accepts a typed `nodeControl` object in its service configuration:

```json
{
  "listQps": 0,
  "putQps": 1400,
  "headQps": 2800,
  "bandwidthBitsPerSecond": 2000000000,
  "autoAdjust": {
    "consecutive503Threshold": 20,
    "reduceFactor": 0.9,
    "increaseFactor": 1.2,
    "minQps": 200,
    "recoveryIntervalMs": 10000
  }
}
```

Omitting an individual field uses the current Go default; explicitly setting a request or bandwidth limit to zero means unlimited. Those defaults match the pinned V4.0.x application file (LIST 100, PUT 1400, HEAD 2800 and bandwidth 2,000,000,000 bits/s), but they do not establish the settings of an existing deployment. `--check-config` validates these fields, and the same conversion supplies the actual Agent controller. Persisted manual overrides continue to take precedence over configured targets. Old in-memory 503 counts and recovery incidents are not revived.

Java SDK retries, thread-pool sizes and multipart options are not translated into Go settings. The migration plan must explicitly set `acceptGoRuntimeDefaults: true`; this records acceptance of profile `go-runtime-defaults-1`. Without it, finalization fails. The profile uses these actual defaults:

| Setting | Go behavior |
| --- | --- |
| Request retries | AWS SDK standard retry policy, at most 3 attempts including the original request. Java `max-retry-on-503` and fixed `retry-delay-ms` are not retained. |
| Object execution | Up to 4 active child tasks per Agent, 4 object workers per child; each child contains at most 128 objects. |
| Discovery | Up to 2 concurrent scans per Agent. Each persisted batch holds up to 128 objects or 256 MiB of described content; oversized objects get their own batch. |
| Admission | At most 1,024 unfinished child jobs per Agent. Full admission applies backpressure; it does not discard acknowledged jobs. |
| Multipart upload | 8 MiB parts, up to 4 concurrent part requests, up to 32 MiB of payload buffers per write. Large request bodies can use private spool files. |
| Progress persistence | Completed results commit in batches of 32 or every 250 ms. A crash between a destination write and its ledger commit can require that object to be handled again. |

The nine explicitly mapped node-control settings and persisted manual overrides remain effective. Old task QPS and bandwidth fields preserve their **per-node** scope; the separate logical task bandwidth policy preserves its cluster scope. Both limits apply when present. The raw exports retain all old SDK and concurrency settings for audit. No V4.0.x multipart upload ID is invented; interrupted objects restart through the Go object execution path.

Create the Master configuration and imported accounts in the same transaction, with setup marked complete only after the admin account and public URL are valid. For a combined V4.0.x Master/worker, split its control state and execution state into distinct Go roles. Existing Agent credentials may be retained only when the Agent private identity and Master credential hash match; old directly addressed workers need explicitly mapped, newly provisioned Go identities.

Import unfinished tasks as stopped and keep their original checkpoint and ledger. Do not put them in an active index or let ordinary `resume` bypass conversion review. The first explicit resume performs these steps:

1. Check all selected Agent identities and enrollments. Preserve purge/recycle/stop fences. Require the full inventory when the original task used one.
2. Re-enumerate the original range, inventory or local source with the original filters and key mapping. Retain old discovered keys that are now missing as explicit unresolved or failed items; disappearance is not completion.
3. For old `SUCCESS`, compare current source and destination content with an Agent-side streaming hash before suppressing a write. Size or an S3 ETag alone is insufficient. Record the new verified source/target metadata separately from the historical result. If the source no longer exists, retain its old row in historical storage and report `SOURCE_MISSING` in the new attempt.
4. For changed or missing targets, apply the original conflict policy. An existing unequal destination under `SKIP` remains a skip, not a verified success. An `OVERWRITE` retry may write again. Handle old skips as skips and distinguish dry-run skips. Do not convert an old failed or unprocessed item into success because its target happens to exist.
5. Persist each new bounded discovery batch, node assignment and converted ledger before acknowledging it. Respect the Agent admission budget; interrupted conversion must resume by stable migration ID rather than allocate duplicate children.
6. Atomically archive the original parent, advance its generation once and reset current attempt counters. Apply node and task limits before starting any reads. Retrying this transition does not allocate another generation or count the old successes twice.

The source is live storage, not a snapshot. Hash verification must check source and target identity before and after reading, and later execution must still use the captured source fingerprint. Identical hashes prove the compared reads matched; they do not recover an unknown historical source version or prevent later business writes.

Freshly discovered keys and retained historical keys are merged through a durable unique index. Historical `TIME_FILTER` exclusions remain excluded, even if another retained completion or local-entry record lists the same key. Other retained objects still pass the original time filter: `dateAfter` includes its boundary and `dateBefore` excludes it. Missing retained objects produce failed rows instead of disappearing from the count. An exact V4.0.x inventory remains exact; it never expands to the whole bucket. Old success credit requires complete streaming SHA-256 equality and stable source/target identities before and after both reads. Unequal targets under `SKIP` remain skips with a reconciliation reason.

## Native preparation commands

Create a private JSON plan, using one snapshot for every old node. The destination must not exist. `directNodes` explicitly authorizes new Agent identities for old direct workers, including execution on the old Master; existing enrolled Agents are matched to their saved credentials instead.

```json
{
  "snapshots": ["/backup/exports/master.jsonl"],
  "destination": "/export/oss-transfer-go",
  "publicUrl": "https://transfer.example.com",
  "directNodes": {
    "10.0.0.10": {"address": "10.0.0.10", "label": "Master host execution"}
  },
  "legacyTimezone": "Asia/Shanghai",
  "acceptGoRuntimeDefaults": true,
  "masterListen": ":1685",
  "effectiveNodeControl": {
    "10.0.0.10": {
      "rate-limit.src.qps.list": "100",
      "rate-limit.dest.qps.put": "1400",
      "rate-limit.dest.qps.head": "2800",
      "rate-limit.bandwidth": "2000000000",
      "rate-limit.auto-adjust.consecutive-503-threshold": "20",
      "rate-limit.auto-adjust.reduce-factor": "0.9",
      "rate-limit.auto-adjust.increase-factor": "1.2",
      "rate-limit.auto-adjust.min-put-qps": "200",
      "rate-limit.auto-adjust.interval-ms": "10000"
    }
  }
}
```

`legacyTimezone` is the original JVM timezone and is needed only for saved date filters without an offset. Do not substitute the importing machine's timezone. This example describes one combined Master/worker; add each additional node snapshot and its effective settings, and add `directNodes` entries for direct workers. Existing enrolled Agents retain their identities. Replace the example limits with the old deployment's effective values. `masterListen` preserves the desired HTTP port.

```sh
chmod 600 migration-plan.json
oss-transfer-master migrate inspect /backup/exports/master.jsonl
oss-transfer-master migrate prepare --plan migration-plan.json
oss-transfer-master migrate status --directory /export/.oss-transfer-migration-123456789
oss-transfer-master migrate prepare-recovery --directory /export/.oss-transfer-migration-123456789
oss-transfer-master migrate status --directory /export/.oss-transfer-migration-123456789
oss-transfer-master migrate finalize --directory /export/.oss-transfer-migration-123456789
```

Preparation prints its actual staging directory. Reopening rechecks each snapshot's checksum, private file permissions, database existence and pending migration identity. A failed conversion leaves the new staging directory for inspection; it does not alter the old installation or publish the destination. Repeating task conversion with the same inputs is idempotent. Changing an imported task's meaning requires a new preparation.

Task catalogue conversion groups V4.0.x OSS execution ranges under their existing public parent, preserves `parent-number` child identities, and records the old allocation high-water mark in the same transaction as each parent and its indexes. It never allocates runnable Agent jobs from historical completion records. The recycle bin and purge fences are retained. A partial worker purge that conflicts with the Master catalogue blocks recovery instead of silently deleting the entire parent.

Object reports retain original success, skip, failure and unprocessed rows, source/destination keys and recorded timestamps. Go's `targetCommitted` and `ledgerObservedAt` fields stay empty for V4.0.x rows. Report metadata carries `containsLegacyHistory: true` and `contentVerified: false`; existing V4.0.x coverage and exact counters determine report completeness. Historical reads do not require the old Agents to be online. Raw checkpoints, inventory entries, pending assignments and unknown fields remain in private source exports and indexed records for the recovery converter.

`prepare` already converts the task catalogue. Use `convert-tasks --legacy-timezone ...` only when retrying an interrupted conversion in the same staging directory. If the original plan omitted runtime acceptance, `finalize --accept-go-runtime-defaults` records it explicitly. A failed preflight leaves the migration guard in place; address the reported incompatibility before retrying.

After finalization, the printed destination is ready for native startup:

```sh
oss-transfer-master --config /export/oss-transfer-go/master/service.json
oss-transfer-agent --config /export/oss-transfer-go/agents/<instanceId>/service.json
```

Run each Agent on its original execution host. Transfer only that Agent's private directory, keeping exactly the absolute path in `service.json`; preserve directory mode `0700` and file mode `0600`. Keep Master state and raw snapshots on the Master host. The database role guard also checks this absolute path, so copying a database to an arbitrary new location is rejected. Existing local source and target paths must still resolve to the intended storage mounts.

For service installation, use the native installer's explicit `--adopt-config` flow described in [the native deployment guide](../../deploy/go/README.md). Its `--service-name` and `--data-dir` must match the generated configuration. Ordinary first installation refuses nonempty data directories. Adoption validates and backs up the stopped database before starting the new service; it does not generate a replacement Agent invitation or reset the imported account.

Sign in with the old account password, confirm the Agents are online, and explicitly resume the chosen stopped tasks. Completed historical tasks stay historical with `contentVerified: false`; the migration never claims their contents were freshly checked. After a task resumes, its ordinary details and report show the new Go attempt. Its original parent, child rows, ledger and raw exports remain preserved separately.

## Tests and current boundary

```sh
go test -race ./internal/legacyimport
go vet ./internal/legacyimport ./tools/legacy/inspect
python3 tools/legacy/test-export.py \
  --jdk-home /path/to/jdk8 \
  --output output/go-development/qa/legacy-export
```

The integration test uses the pinned V4.0.x package's actual RocksDB JNI library. It creates a fresh isolated DB containing users, connections, task/recycle/purge records, completion/report rows, unfinished dispatch/discovery records, inventory, limits and an unknown future key. Writes remain in the WAL. It checks every exported key/value byte, source file inode/size/mtime/SHA-256, output permissions, refusal to replace an export, and refusal while a real RocksDB writer holds the database. Go tests cover corrupted, missing, duplicate and reordered entries, incomplete footers, inventory readiness, missing node snapshots and divergent node credentials/configuration without exposing secrets in the plan.

The synthetic exporter fixture establishes byte preservation and non-destructive preflight. It is separate from the real-runtime capture and native-process acceptance below.

The real-runtime capture script `tools/legacy/capture-tasks.py` starts the pinned V4.0.x JAR with an isolated SeaweedFS instance. The retained `output/go-development/qa/legacy-real-tasks-2` capture contains six logical tasks: three completed directions, another completed task in the recycle bin, a partially stopped upload and an OSS copy interrupted by SIGKILL. Four completed tasks were independently checked against 260 source-object SHA-256 values each before shutdown.

The converter has been exercised against that actual RocksDB export: six parents, sixteen historical children and 2,568 indexed report/checkpoint records. The four completed tasks retain 260 successes each; the stopped upload retains three and the interrupted copy retains one. The production ZIP writer exports 260 rows for each completed task and preserves the incomplete tasks' 178 and 260 discovered report rows. Historical report conversion alone does not establish successful continuation.

```sh
OSS_TRANSFER_LEGACY_TASK_SNAPSHOT="$PWD/output/go-development/qa/legacy-real-tasks-2/master.jsonl" \
  go test -count=1 ./internal/legacyimport ./internal/master -run 'TestRealV4'
```

The opt-in recovery tests copy the captured SeaweedFS database before starting it. The original snapshot and storage fixture stay unchanged. Native-process acceptance finalizes a new destination, starts the real Master and Agent binaries, signs in with the old password, verifies the old Agent identity and persisted 1 Mbps node limit, then explicitly raises that isolated node limit through its normal administrator API. It resumes tasks #7 and #8, checks 260 successful report rows per task and independently hashes every current source/target pair. Three old upload successes and one old copy success are reused only after streaming verification. The recycle-bin task remains present.

```sh
OSS_TRANSFER_LEGACY_RESUME_SNAPSHOT="$PWD/output/go-development/qa/legacy-real-tasks-2/master.jsonl" \
OSS_TRANSFER_NATIVE_MASTER=/path/to/oss-transfer-master \
OSS_TRANSFER_NATIVE_AGENT=/path/to/oss-transfer-agent \
  go test -count=1 ./internal/legacyimport -run '^TestRealV4FinalizedNativeProcesses$' -v
```

Use a release Master binary with embedded web assets. The fixture is a real local S3 service, not JDCloud OSS; passing this test establishes the specified V4.0.x state and recovery path, not compatibility with every historical schema or S3 provider. Unit tests additionally cover content mismatch, missing source, mutation during verification, duplicate recovery requests, database reopen, missing manifest keys, refused destination replacement and interrupted finalization.
