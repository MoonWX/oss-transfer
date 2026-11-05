# Database space

[中文](database-maintenance.zh-CN.md)

The database stores object names, scan checkpoints, assignments and transfer results. Object contents are streamed between endpoints. With millions of small files, the number of objects matters more to database size than their total size.

An unfinished scan keeps an index for deduplication and source-change detection during recovery. After scanning completes, the Agent removes this index in small background transactions. Once the Master has durably saved a batch as child tasks, acknowledged it and authorized its release, the Agent removes that scan batch in the background, even while scanning continues. Unacknowledged batches remain available for delivery; execution records used by retries and reports are retained. Permanently deleting a task removes its scan and execution records; stopping a task or moving it to the recycle bin retains them.

New writes use compact object records and compress larger values when compression saves space. Existing plain records remain readable. Cleanup makes pages reusable, but does not reduce the database file's size on disk.

Storage credentials are encrypted with AES-256-GCM in both the Master and Agent databases. Each database has its own adjacent `.key` file, restricted to its service account and system administrators. Keep the key with database backups; copying only the database does not produce a usable credential backup. This protects a database copied without its key, not an account that can read both files.

The first startup migrates existing storage configurations. Old plaintext can remain in unused database pages until one offline compaction rewrites the file. Compaction removes those pages from the new database; it does not erase older backups or guarantee physical disk erasure. Future restarts do not repeat the full rewrite.

Permanent deletion runs in the background. The recycle bin shows the cleanup stage, deleted record count and completed nodes until cleanup finishes. Closing the page or restarting a service does not discard the operation. An offline node is retried automatically when it returns; an explicitly removed node's local history is retained. In the CLI, use `oss-transfer-cli task trash` to check progress. Cleanup removes task records, not source or destination files.

Managed installations authorize release only after the installer commits a successful startup. This retains the scan copy if an upgrade rolls back the Master's database. Older Masters and installers do not authorize release, so their acknowledged batches remain; tasks already finished before authorization also retain those batches.

## Reclaim disk space

Stop the service before compacting its database. Compaction writes a separate file, packs its pages, converts old Agent scan-index records and compresses eligible values. It verifies every record before publishing the new file. The original is unchanged, and an existing output file is never overwritten. Progress is printed every 10 seconds.

For an Agent installed at the default Linux paths:

```sh
sudo systemctl stop oss-transfer-agent
sudo /opt/oss-transfer/agent/bin/oss-transfer-agent \
  --config /opt/oss-transfer/agent/config/service.json \
  --compact-state /opt/oss-transfer/agent/data/agent.compact.db
```

If compaction succeeds, keep the service stopped and install the verified copy. These commands preserve the original's owner and permissions and refuse to overwrite an existing `agent.before-compact.db`:

```sh
sudo cp --attributes-only --preserve=mode,ownership /opt/oss-transfer/agent/data/agent.db /opt/oss-transfer/agent/data/agent.compact.db &&
sudo mv -nT /opt/oss-transfer/agent/data/agent.db /opt/oss-transfer/agent/data/agent.before-compact.db &&
sudo test ! -e /opt/oss-transfer/agent/data/agent.db &&
sudo mv -nT /opt/oss-transfer/agent/data/agent.compact.db /opt/oss-transfer/agent/data/agent.db &&
sudo systemctl start oss-transfer-agent
```

Check that the node reconnects and its tasks and reports are present before deleting the retained original. For a Master, substitute `master` for `agent` in these commands. Use the installation's actual paths and service name if they were customized.

For an encrypted database, compaction also writes `agent.compact.db.key` using the same key as `agent.db.key`. Leave `agent.db.key` in place when installing the compacted file. Keep a copy of that key beside any retained database backup; the extra compact key copy can be removed after verifying startup.

The destination needs room for the compacted copy. The tool checks free space before each write transaction and reserves at least 256 MiB plus room for the next database growth. If it fails, it removes its temporary output and leaves the original intact; restart the service with the original. Other programs can still consume space during maintenance.

Compact storage uses database schema 4, automatic task takeover requires schema 5, and releasing acknowledged scan batches raises the Agent database to schema 6. Accepting background deletion requires schema 7; rejoining a task node after failover requires schema 8. Credential encryption requires schema 9, and queue handoff requires schema 10. Older binaries cannot open a database whose schema they do not support. To downgrade, restore the matching database backup made before upgrading along with the old binary.
