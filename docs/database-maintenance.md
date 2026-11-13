# Database space

[中文](database-maintenance.zh-CN.md)

The database file grows as a migration progresses. This page explains why the database file grows and how to reclaim disk space. Object comparison results are not stored in the database; the last section, [Comparison results](#comparison-results), covers them separately. If you have not yet started migrating, see the disk estimation method in [Planning](planning.md).

The database stores object names, scan checkpoints, child-task assignments and transfer results. Object contents are streamed directly between the source and the destination without passing through the database, so database size depends on the number of objects and has little to do with the size of individual objects. This becomes especially apparent when small files reach millions.

While a scan has not yet finished, the database retains an index used for deduplication and for detecting source-side changes during recovery. After the scan completes, the Agent (execution node) removes this index in small background transactions. Scan batches follow a separate release rule: after the Master has durably saved a batch as child tasks, acknowledged receipt and authorized its release, the Agent deletes that batch in the background, even while the scan is still in progress. Batches that have not been acknowledged remain available for delivery, and execution records used by retries and reports are retained.

Permanently deleting a task removes its scan and execution records. Stopping a task or moving it to the recycle bin does not.

Permanent deletion runs in the background. Closing the page or restarting the service does not cancel the operation, which will continue to completion afterwards. Until cleanup finishes, the recycle bin page shows the cleanup stage, the number of deleted records and the completed nodes. In the CLI, `oss-transfer-cli task trash` provides the same information. Nodes that are offline retry automatically when they come back online; nodes that have been removed retain their own local history. Cleanup deletes task records only and does not touch files at the source or the destination.

New object records use a compact format, and larger values are compressed only when compression saves space. Existing uncompressed records are still read normally. Pages freed by deletion and cleanup are reused for subsequent writes, but the database file itself does not shrink. To reduce the file size, run an offline compaction; see [Reclaim disk space](#reclaim-disk-space).

Storage credentials in both the Master and Agent databases are encrypted with AES-256-GCM. Each database has a `.key` file beside it, readable only by the corresponding service account and system administrators. The key must be backed up alongside the database. Copying the database alone does not produce a usable credential backup. This mechanism protects against the database file being copied without the key. If an attacker can read both files, the encryption provides no protection.

On first startup, the program migrates existing storage configurations. Old plaintext credentials may remain in unused database pages until an offline compaction is performed. The compacted database no longer contains those pages, although plaintext in older backups cannot be removed by compaction, and physical disk erasure is not guaranteed. Subsequent restarts do not rewrite the entire database.

In native installations, the installer authorizes release of scan copies only after confirming a successful service startup. This is a safety measure: if an upgrade rolls back the Master's database, the scan copies remain. Older Masters and installers do not have the ability to authorize release, so their acknowledged batches are retained. Tasks that finished before authorization also retain those batches.

## Reclaim disk space

Before compacting, stop the corresponding service and back up the database as described in [Backup and restore](operations.md#backup-and-restore).

The compaction tool writes a new file with tightly packed data pages, converts old-format Agent scan-index records and compresses eligible values. After writing, it verifies every record before publishing the new file. The original file is never modified, and an existing output file is never overwritten. Progress is printed every 10 seconds.

For an Agent installed at the default Linux paths:

```sh
sudo systemctl stop oss-transfer-agent
sudo /opt/oss-transfer/agent/bin/oss-transfer-agent \
  --config /opt/oss-transfer/agent/config/service.json \
  --compact-state /opt/oss-transfer/agent/data/agent.compact.db
```

After compaction succeeds, keep the service stopped and replace the original file with the compacted one. The following commands preserve the original file's owner and permissions and refuse to overwrite an existing `agent.before-compact.db`:

```sh
sudo cp --attributes-only --preserve=mode,ownership /opt/oss-transfer/agent/data/agent.db /opt/oss-transfer/agent/data/agent.compact.db &&
sudo mv -nT /opt/oss-transfer/agent/data/agent.db /opt/oss-transfer/agent/data/agent.before-compact.db &&
sudo test ! -e /opt/oss-transfer/agent/data/agent.db &&
sudo mv -nT /opt/oss-transfer/agent/data/agent.compact.db /opt/oss-transfer/agent/data/agent.db &&
sudo systemctl start oss-transfer-agent
```

After starting the service, confirm that the node has reconnected and that its tasks and reports are still present before deleting the retained original. To compact a Master database, substitute `master` for `agent` in these commands. If you customized the installation path or service name, use the actual values.

If the database is encrypted, compaction also writes `agent.compact.db.key` with the same key as `agent.db.key`. Leave `agent.db.key` in place when replacing the file. Keep a copy of the key beside every database backup you retain. After confirming that the service starts normally, you can delete the extra compact-copy key.

The destination needs enough free space for the compacted copy. The tool checks free space before each write transaction and reserves at least 256 MiB plus the amount needed for the next database growth. If compaction fails (insufficient space is the most common cause), the tool removes its temporary output and leaves the original intact. Restart the service with the original file. Other programs may also consume disk space during maintenance, and this should be taken into account.

Each feature corresponds to a database schema version:

| Schema | Feature |
|--------|---------|
| 4 | Compact storage |
| 5 | Automatic task takeover |
| 6 | Release of acknowledged scan batches (Agent database) |
| 7 | Accept background deletion |
| 8 | Agent rejoins task after failover |
| 9 | Credential encryption |
| 10 | Queue handoff |
| 11 | Change a task's execution nodes (Master database) |

Older binaries cannot open a database whose schema version they do not support. To downgrade, restore both the database backup made before the upgrade and the old binary.

## Comparison results

Agents store object comparison results as files under `comparisons` in their data directory (`/opt/oss-transfer/agent/data/comparisons` on Linux), not in `agent.db`. Database compaction does not touch these files. Database backups made with `--backup-state` do not include them, and neither do the installer's upgrade backups.

Agents clean up these results automatically: they delete a result 30 days after the comparison ends, and delete older results sooner when the stored results exceed the node's storage budget. See [Result storage and retention](operations.md#result-storage-and-retention) for the cleanup rules and configuration.

To obtain a deleted result, run the comparison again. The new comparison checks the objects as they are at that time. Deleting results does not affect any task, transfer or stored object. You can delete them with **Delete comparison results** in the console or with `object delete` in the CLI.

If you need to manually delete files under `comparisons`, stop the Agent first. After direct deletion, the affected comparisons retain only their summary counts. The difference lists are lost and the comparisons cannot be resumed. The page shows *Comparison results are unavailable on this node*, and exports are marked incomplete (`RESULTS_UNAVAILABLE`). When an Agent starts after an older database has been restored, it truncates each comparison's file to the length recorded in the database and deletes files for comparisons the database does not know about.

Agents of 4.1.40 or earlier stored up to 10,000 differences per comparison inside `agent.db`. An upgraded Agent still reads these records and deletes them under the same retention rules, in small transactions. As with other cleanup, the freed pages are reused for subsequent writes, and only compaction shrinks the file.
