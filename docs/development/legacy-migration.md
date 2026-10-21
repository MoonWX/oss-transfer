# Convert V4.0.x state to V4.1.x

Use `oss-transfer-master migrate` to convert V4.0.x databases before starting V4.1.x services. Conversion preserves accounts, storage connections, task history, report coverage and recycle-bin state. Unfinished tasks stay stopped until explicitly resumed.

The process runs on Linux or macOS and writes to a new directory. Keep the original installation stopped and retain its backups until the converted services and task results have been checked.

## Export the old databases

Stop the old Master and every execution node, then copy their data directories and effective configuration. Include the Master in the exports because it may also have performed transfers. Record environment variables and command-line overrides alongside `application.yml`.

Build the exporter with JDK 8 or newer. Running it requires a Java runtime and the original V4.0.x JAR; the exporter uses that JAR's `rocksdbjni` library to read the copied database.

```sh
bash tools/legacy/build.sh
java -jar output/tools/legacy/legacy-exporter.jar \
  --legacy-jar /backup/master/oss-transfer.jar \
  --expected-jar-sha256 '<SHA-256 of that JAR>' \
  --database /backup/master/data/rocksdb \
  --config /backup/master/application.yml \
  --node 10.0.0.10 \
  --role master \
  --output /backup/exports/master.jsonl
```

Set `--node` to the persisted `node_config.localIp` or `agent:identity.address`. Use role `master`, `agent`, or `worker`; `worker` identifies an older directly addressed execution node. Repeat the export for each node with a separate output file.

The exporter opens an existing RocksDB database read-only and checks its lock and file identities. Stop all nodes before taking the copies so their snapshots cover the same cluster state. Exports contain credentials and password hashes and are written with mode `0600`; store them with the protected database backups.

Inspect the complete set:

```sh
oss-transfer-master migrate inspect \
  /backup/exports/master.jsonl /backup/exports/worker.jsonl
```

Inspection checks export checksums, node coverage and identity mappings. It reports conflicts that need to be resolved before conversion. The original configuration file is retained as evidence; supply effective values explicitly in the plan below.

## Create the migration plan

Save a private `migration-plan.json`. This example covers one combined Master/worker at `10.0.0.10`:

```json
{
  "snapshots": ["/backup/exports/master.jsonl"],
  "destination": "/export/oss-transfer",
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

`destination` must be an absolute path that does not yet exist. Add every node export to `snapshots` and provide all nine `effectiveNodeControl` properties for every execution node. Replace the example values with the stopped deployment's effective settings. Bandwidth uses bits per second; zero request or bandwidth limits mean unlimited. Persisted manual limits take precedence over these configured targets.

`directNodes` authorizes a new Agent identity for each old directly addressed worker, including execution on the old Master. Existing enrolled Agents retain their identities after their private credential is matched against the Master snapshot.

Set `publicUrl` to the address the new Agents and browsers will reach. `masterListen` selects the bind address and port. `legacyTimezone` is the original JVM timezone, required when saved date filters lack an offset.

Read the execution defaults below before setting `acceptGoRuntimeDefaults: true`. The converter records this choice as profile `go-runtime-defaults-1` and requires it before finalization.

## Prepare and finalize

```sh
chmod 600 migration-plan.json
oss-transfer-master migrate prepare --plan migration-plan.json
```

`prepare` prints a private staging directory and imports the task catalogue. Use that printed path for the remaining commands:

```sh
STAGING=/export/.oss-transfer-migration-123456789
oss-transfer-master migrate status --directory "$STAGING"
oss-transfer-master migrate prepare-recovery --directory "$STAGING"
oss-transfer-master migrate status --directory "$STAGING"
oss-transfer-master migrate finalize --directory "$STAGING"
```

`prepare-recovery` writes the retained key manifests for unfinished tasks. Storage reads begin when a task is later resumed. `finalize` checks node snapshots, task history, recovery manifests, identities, runtime acceptance and service configuration, then publishes the destination directory.

A conversion failure leaves the reported staging directory available for inspection. `convert-tasks --directory "$STAGING" --legacy-timezone Asia/Shanghai` retries task conversion with the same inputs. If runtime acceptance was omitted from the plan, pass `--accept-go-runtime-defaults` to `finalize` after reviewing the defaults. A change to a task's original scope requires a new preparation.

The destination contains `master/master.db`, `agents/<instanceId>/agent.db`, their `service.json` files, raw exports, `identity-map.json` and `manifest.json`.

## Install the converted services

Each Agent must run on its intended execution host. Copy only its private directory there, preserving the absolute path recorded in `service.json`, directory mode `0700` and file mode `0600`. Keep Master state and raw exports on the Master host. Check that the old local source and target paths still resolve to the intended mounts.

Use the native installer's [adoption flow](../../deploy/native/README.md#register-existing-data-as-a-service) for service registration. The service name and data path must match the generated configuration. For the Master in this example:

```sh
RELEASE_URL=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest
curl -fSL "$RELEASE_URL/install.sh" -o install.sh
sudo bash install.sh --role master --method native --release-url "$RELEASE_URL" \
  --adopt-config /export/oss-transfer/master/service.json \
  --service-name oss-transfer-master --data-dir /export/oss-transfer/master --yes
```

The Agent's generated service name is `oss-transfer-agent-<instanceId>`. Use its own `service.json`, service name and data path when adopting it. Program and configuration installation directories must be empty; the installer snapshots the stopped database before startup.

For a foreground check, run each binary on its host:

```sh
oss-transfer-master --config /export/oss-transfer/master/service.json
oss-transfer-agent --config '/export/oss-transfer/agents/<instanceId>/service.json'
```

Sign in with the existing account password and confirm the Agents are online. Successful login upgrades the old password hash to Argon2id. Imported accounts keep their roles and display names; login sessions are renewed.

## First resume

Resuming an imported unfinished task creates a new attempt while retaining its original parent, children, report rows and checkpoints in history. Recovery checks the selected Agent identities, saved stop and deletion state, and any inventory required by the task.

The scan uses the original range, inventory, local source, filters and target naming. Retained keys are merged with newly discovered keys through a durable unique index. Historical `TIME_FILTER` exclusions remain excluded; `dateAfter` includes its boundary and `dateBefore` excludes it. Missing retained sources produce failed rows with `SOURCE_MISSING`.

Before reusing an old `SUCCESS`, the Agent reads the complete current source and destination and compares SHA-256 hashes. It checks both identities before and after the reads. An unequal existing target follows the saved conflict policy: `SKIP` records a skip and `OVERWRITE` permits a new write. Previous skips, dry-run results and failures keep their original meaning in history.

Recovery persists each batch and assignment before acknowledging it. Advancing the task generation and resetting attempt counters happens once, so retrying an interrupted recovery does not double-count old successes. Node and task limits apply during verification and transfer.

Interrupted multipart objects restart at object level: V4.0.x did not save the upload IDs and completed-part lists required to continue them. Old remote uploads and local spool files need ownership-aware cleanup. A source changed by the business after verification can require another transfer; recovery checks current content rather than recreating an unavailable historical version.

Completed tasks retain their historical status and report coverage. Legacy report metadata includes `containsLegacyHistory: true` and `contentVerified: false`; `targetCommitted` and `ledgerObservedAt` remain empty for those rows. New attempts record current results separately.

## Execution defaults

Retry, concurrency and multipart settings use the Go execution profile after conversion:

| Setting | Value |
| --- | --- |
| Request retries | AWS SDK standard policy, at most 3 attempts including the original request |
| Object execution | Up to 4 active child tasks per Agent and 4 object workers per child |
| Discovery | Up to 2 scans per Agent; batches of at most 128 objects or 256 MiB of described content; oversized objects use a separate batch |
| Admission | At most 1,024 unfinished child jobs per Agent; new work waits when the budget is full |
| Multipart upload | 8 MiB parts, up to 4 concurrent part requests and 32 MiB of payload buffers per write; large request bodies may use spool files |
| Result persistence | Batches of 128 results or every 1 second; final results and child completion commit together immediately |

A crash between a destination write and its result commit can cause the object to be handled again under the saved conflict policy. The mapped node-control properties and persisted manual overrides remain effective. Old task QPS and bandwidth fields retain their per-node scope; a logical task bandwidth policy retains its cluster scope. Both apply when present.

## State mapping reference

V4.0.x uses UTF-8 keys and Fastjson values in RocksDB's default column family. The exporter preserves the bytes. The reference schema is the [V4.0.6 source](https://github.com/MoonWX/oss-transfer/tree/v4.0.6/src/main/java), commit `835a1d8`.

| V4.0.x keys | Conversion |
| --- | --- |
| `user:<username>` | Preserve password hashes, roles and display names in `users`; keep remaining timestamps in migration history. |
| `s3_config_<alias>` | Write one coordinated storage snapshot to Master and Agents. Preserve credentials, signer and path-style flags; set the old dynamic signing region to `default`. |
| `node_config` | Map membership and labels; preserve recorded directory metadata. Split a combined Master/worker into separate roles. Resolve listener and public URL from the plan. |
| `agent:member:<address>`, `agent:identity` | Match the Agent's credential with the Master's base64url SHA-256 hash before writing the Go hex SHA-256 index. Preserve instance and enrollment IDs; revoked members stay revoked. |
| `agent:invite:<token hash>`, `agent:storage-version` | Retain invitation history and storage provenance. New invitations and configuration revisions are issued for the new runtime. |
| `rate_limit:manual:list`, `:put`, `:head`, `:bandwidth` | Preserve manual overrides in Agent node-control settings. Bandwidth is bits/s. |
| `rate_adjustment:*`, `rate_event:*`, `rate_incident:*`, `rate_incident_event:*`, `rate_incident_gap:*` | Retain historical events. Active 503 counts and recovery incidents start with the new process. |
| `task_bandwidth:desired:<scope>`, `task_bandwidth:task:<id>`, `task_bandwidth:scope_tasks:<scope>`, `task-bandwidth:policy:<base64url scope>` | Reconstruct logical task scopes and compare worker copies with the Master policy before allocating limits. |
| `task_creation:<UUID>`, `task_creation_ids` | Group execution ranges by `publicTaskId`; retain generation, stop/delete state and pending dispatches. Coordinator `COMPLETED` describes scanning, so transfer completion comes from execution results. |
| `local:parent:<id>`, `local:parent:id:list` | Preserve the logical local-file parent and original request. `orchestrationComplete` describes dispatch completion. |
| `task:<id>`, `task:id:list`, `task:id:sequence` | Preserve task history and public parent IDs. Reserve the new allocator beyond the old allocation high-water mark. |
| `task:deleted:<id>`, `task:id:list:deleted`, `local:parent:deleted:<id>` | Preserve the recycle bin and retained reports. |
| `task:purged:<id>`, `local:parent:purged:<id>`, `report:purged:<id>`, `local:stop:<id>` | Preserve permanent-deletion and stop records. Conflicting partial worker purges block recovery. |
| `local:explore:<parent>`, `local:discovery:<parent>` | Preserve scan markers, pending ranges and pending batches for recovery. |
| `local:batch-assignment:*`, `local:shard-assignment:*` | Retain the original node assignments and internal task IDs to deduplicate uncertain dispatches. |
| `inventory:<UUID>:entry:<base64url key>`, `inventory:<UUID>:ready` | Preserve the exact inventory. Missing entries or readiness markers block recovery. |
| `completed:<report task id>:<base64url logical key>` | Keep historical completion records; current source and target identities are established during recovery. |
| `file-report:<id>:item:<base64url logical key>`, `file-report:<id>:coverage`, `:scan:*`, `:legacy` | Preserve source/target mappings, statuses, timestamps and `TRACKED`, `PARTIAL` or `LEGACY_UNKNOWN` coverage. |
| `failed:<sequence>`, `failed:count` | Retain exception history and the allocation high-water mark, including gaps from cleared entries. |
| `fallback:<sequence>`, `fallback:count` | Retain old SDK fallback history. |
| `local-temp:<UUID>` | Retain ownership metadata for cleanup review. Java file-key strings do not serve as Go inode identities. |

Map old execution records by node identity and task ID, plus their saved child identity. Local internal IDs can be negative. Preserve the public `parent-number` numbering and retain the mapping when one old range becomes several batches. An old logical key may already include its destination prefix; derive source and target names from the saved endpoints and report fields.

Historical records stay outside the runnable Agent queue. A Go job needs an immutable list of fully described objects; recovery creates those lists from the original scope. Reports retain missing coverage and unknown fields in the private raw exports.

## Export format and staging

Each JSONL export contains a header with node, role, timestamp and JAR/library hashes; an optional configuration attachment; ordered base64url key/value entries; and a footer with counts and a SHA-256 checksum over all preceding lines. Unknown database keys retain their original bytes.

Inspection validates the complete file, including ordering, duplicate fields and footer counts. Limits are 1 MiB per key, 64 MiB per value, 4 MiB per configuration attachment and 100,000 mapping facts. Files exceeding these limits fail inspection.

`legacyimport.Prepare` copies exports into a private sibling staging directory before mapping them. `Prepared.Master`, `Prepared.Agents` and `Prepared.Iterate` provide access to the staged databases and raw records. Reopening staging rechecks checksums, permissions and migration identity.

Staged databases carry `migration/state.pending=true`, which blocks service startup. Finalization records the final absolute database paths and publishes the directory with an atomic rename that refuses an existing destination. An interrupted finalization can be retried from staging. Service startup checks both role and final path.

`MapEffectiveNodeControl` requires all nine resolved settings in the plan and rejects placeholders and unknown properties. `WriteRuntimeConfig` writes one `service.json` per role with the final paths. Divergent node connections, missing identities or incomplete inventories must be resolved before finalization.

## Tests

```sh
go test -race ./internal/legacyimport
go vet ./internal/legacyimport ./tools/legacy/inspect
python3 tools/legacy/test-export.py \
  --jdk-home /path/to/jdk8 \
  --output output/packages/qa/legacy-export
```

The exporter test uses the actual V4.0.6 RocksDB JNI library. It compares every exported key/value byte and checks source files, output permissions, existing-output rejection and locking against an active writer. Go tests cover damaged exports, missing node snapshots, identity conflicts, content changes during recovery and interrupted finalization.

The recorded fixture at `output/packages/qa/legacy-real-tasks-2` was captured by `tools/legacy/capture-tasks.py` using V4.0.6 and SeaweedFS. It contains six logical tasks: four completed, including one recycled task, and two interrupted tasks. The test archive is pinned to SHA-256 `64a0e80a28d61322fad20be17c8d47c31f77565a76daf5b1f4718590f9c992d4`.

With that fixture available, check history conversion:

```sh
OSS_TRANSFER_LEGACY_TASK_SNAPSHOT="$PWD/output/packages/qa/legacy-real-tasks-2/master.jsonl" \
  go test -count=1 ./internal/legacyimport ./internal/master -run 'TestRealV4'
```

For process-level recovery, supply release binaries with embedded web assets:

```sh
OSS_TRANSFER_LEGACY_RESUME_SNAPSHOT="$PWD/output/packages/qa/legacy-real-tasks-2/master.jsonl" \
OSS_TRANSFER_NATIVE_MASTER=/path/to/oss-transfer-master \
OSS_TRANSFER_NATIVE_AGENT=/path/to/oss-transfer-agent \
  go test -count=1 ./internal/legacyimport -run '^TestRealV4FinalizedNativeProcesses$' -v
```

This test copies the storage fixture, starts the finalized services, signs in with the old password and checks the Agent identity and saved 1 Mbps limit. It raises that isolated test limit through the administrator API, resumes tasks #7 and #8, and hashes all 520 source/target pairs. It also checks the recycle-bin task and reuse of four old successful results. Provider compatibility and performance are tracked separately in the [acceptance inventory](../migration-acceptance.md).
