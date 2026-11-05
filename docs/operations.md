# Concurrency, comparison and updates

[中文](operations.zh-CN.md)

## Node concurrency

Open **Execution nodes → Edit node** to change child-task concurrency and the object concurrency within each child. The defaults are 32 children and 4 objects per child, allowing up to 128 simultaneous object transfers on a node. Existing saved settings are retained.

Changes apply to running work and survive a restart. Reducing a limit lets the current requests finish before admitting more. Increasing it can help when the network is underused; node and task bandwidth/QPS limits still apply. Child concurrency accepts 1–64, object concurrency 1–32, with a combined maximum of 256.

## Compare objects

Under **Tools → Compare objects**, choose a single object or a bucket/prefix on each side. For prefix comparison, the source prefix is replaced with the target prefix: `images/a.jpg` under `images/` matches `backup/a.jpg` under `backup/`. An empty prefix covers the whole bucket. Results include objects missing from either side.

**Metadata** compares size and transferable metadata through HEAD requests. It does not read object contents or prove byte equality. An ETag is not treated as a portable content hash: multipart uploads and different storage providers can produce different ETags for identical content.

**Content** reads both objects through streams and computes SHA-256. It does not create local object files, but it does transfer the full contents across the network and may incur storage read charges. Objects that change while being read are reported separately. A bucket-wide comparison is not an atomic snapshot; pause writes if you need a fixed dataset verified.

One comparison runs per Agent. Results are saved in batches; only differences and errors are retained. Each node keeps the latest 10 comparisons and the first 10,000 differences per comparison. Counts continue after that limit, and the page marks truncated results. Narrow the prefix to inspect remaining differences, or delete completed comparison results.

## Update installed services

Open **Tools → Software updates** to update the Master. Then open **Execution nodes → Software updates** and choose **Update all nodes** to bring eligible Agents to the Master's version. Each node must reconnect at the target version and finish its update before the next starts. A failed update stops the queue. The Master saves the queue, so closing the page or restarting the Master does not discard it. You can also update one node.

Wait for active tasks to finish when possible. If you upgrade with tasks running, update the Master first, then one Agent at a time. The native installer asks the Master to reserve that Agent’s assignments during the upgrade before stopping it. The reservation ends after verified startup or a successful rollback and expires after 30 minutes if the installer cannot finish. An older Master must be upgraded before the Agent installer can use this protection.

Online replacement supports managed native installations. The updater verifies the release archive, downloads it before stopping the service, and runs the native installer in a separate process. The installer takes a temporary backup of the programs, configuration, service definition and stopped database. It deletes that backup after the new service passes its health check and confirms the installation. If startup fails, it attempts to restore the previous installation and keeps the recovery files. Agent transfers interrupted by the restart resume after it reconnects.

Backups use the host's [default backup directory](install.md#paths-and-ports), even if an earlier installation saved a shared-storage path. An explicit `--backup-dir` or PowerShell `-BackupDir` selects another directory. Before stopping the service, the installer cleans eligible backups and checks available space. **Upgrade backups → Clean up backups** removes completed upgrade backups; failed backups and older backups without a completion marker are kept. Database backup progress is printed every 10 seconds, including the final disk sync.

For a planned downgrade, make a separate backup of each service's programs, configuration and stopped database before upgrading. Successful upgrades do not leave a rollback copy. See [manual recovery](#recover-from-a-failed-upgrade) if automatic rollback could not finish.

The first upgrade from a version without online updates still uses the installation script. Containers use [Docker Compose updates](docker-compose.md); replacing a binary inside a running container would be lost when the container is recreated.

## Try a prerelease

Update checks only offer the latest stable release. Prereleases, such as `4.1.29-beta.1` or `4.1.29-rc.1`, are never offered automatically, and a stable installation is never moved to one unless you choose it. Versions are ordered by [SemVer](https://semver.org/) precedence: `4.1.29-beta.1` < `4.1.29-beta.2` < `4.1.29-rc.1` < `4.1.29`.

To install a prerelease on a managed native Master:

1. Open the Software updates page with the prerelease channel parameter: `/tools?tab=updates&channel=prerelease`. A **Specific version** control appears on the Master card; without the parameter it is hidden.
2. Enter the exact version, for example `4.1.29-beta.1`, and choose **Install this version**.
3. Confirm the warning. A prerelease is an experimental build that normal updates never offer.

From the CLI, run `oss-transfer-cli update master --version 4.1.29-beta.1 --allow-prerelease --yes`. Without `--allow-prerelease`, a prerelease version is rejected as invalid usage (exit code 2). The API equivalent is `POST /api/master/software-updates/master` with `{"version": "4.1.29-beta.1", "allowPrerelease": true}`; without the flag the Master rejects a prerelease version.

The Master first verifies that the exact release is published: on GitHub it reads `releases/tags/vX.Y.Z-…` and accepts the prerelease mark only because the flag was given (drafts are always rejected); on OSS it reads the versioned directory, never `latest/`. The download then uses the same archive digest, SHA-256 and version checks as a stable update, and the version must be newer than the running one.

**Agents.** After the Master runs the prerelease, **Update all nodes** (or `update agents --all --yes`) brings Agents to exactly the Master's version; choosing the Master's version is the explicit opt-in. Agents must run a release with prerelease update support (the release that introduced this feature or later). Older Agents are skipped with a message; update them once with the installer. While the Master runs a stable release, Agents are never offered or sent a prerelease.

**Back to stable.** When a stable release newer than the prerelease is published, for example `4.1.29` after `4.1.29-beta.2`, the normal update check offers it. Update the Master and then the Agents as usual. Moving from a prerelease back to an older stable release, such as `4.1.28`, is a downgrade: online updates refuse it, and its database may already use a newer format. Make a separate backup before installing a prerelease if you may need to return.

Docker Compose installations select a prerelease with `OSS_TRANSFER_IMAGE=moonwx/osstransfer:4.1.29-beta.1`; see [installing a specific version](install.md#install-a-specific-or-prerelease-version). Masters released before this feature reject the `allowPrerelease` field.

## Recover from a failed upgrade

Use the backup directory printed by the installer. Stop the affected service, then run a Master or Agent binary that can read both the current and backup database formats with `--config <backup-directory>/service.json --restore-state <backup-directory>/database`. Keep `database.key` beside the snapshot when present. The command restores the database to the data directory recorded in the saved configuration. If the installed database has lost its key, restore its matching `master.db.key` or `agent.db.key` from a backup of that same database first.

Restore the saved program, configuration, installation metadata, service definition and CLI to their original locations, then start the service. Transferred files remain where they are. For your own backups, stop the service and copy its configuration and data directory, including `master.db.key` or `agent.db.key`; retain the matching program version for a planned downgrade.

## Reset the administrator password

For a default Linux installation:

```sh
sudo systemctl stop oss-transfer-master
sudo /opt/oss-transfer/master/bin/oss-transfer-master \
  --config /opt/oss-transfer/master/config/service.json --reset-admin-password
sudo systemctl start oss-transfer-master
```

Use the printed password to sign in and choose a new one. On other systems, stop the service and run its Master binary with the same arguments. Use the installation's actual paths if customized.

## Disconnected nodes

Multi-node tasks can move unfinished child tasks to another selected node after an Agent loses contact. All participating Agents must support task leases. The Master renews execution permission every 20 seconds; an isolated Agent stops starting work and cancels active transfers when its 60-second lease expires. Reassignment waits another 10 seconds. A returning node can rejoin when both sides support recovery fencing and the original shared directory is still accessible. Its old execution permission is revoked before it receives a new ownership epoch. Older Agents remain excluded from that task.

The recovered node can receive new assignments and queued children from healthy nodes, even after scanning has finished. Redistribution uses the task's existing placement policy and its load estimate of remaining bytes plus 64 KiB per object. Only children that have never started can move: the current owner must atomically revoke their execution permission before the Master changes ownership. Running and interrupted transfers stay with their current owner. Rebalancing runs in bounded batches after recovery; it does not continuously reshuffle the queue.

Takeover supports OSS-to-OSS and verified shared directories. A small hidden marker stays in each shared directory so another node can verify that it still sees the original volume. A private local directory, a missing marker, or an older Agent prevents automatic reassignment. Completed children stay unchanged; incomplete children keep their numbers and saved file results. The new owner checks already completed objects before continuing. If the scanner fails, the replacement rebuilds its scan index from the Master’s saved assignments and resumes from the last source-page checkpoint covered by those assignments. Scan-ahead results that never reached the Master must be discovered again; the dataset contents are not downloaded for this check.

A request already accepted by object storage may finish after its client is cancelled. Takeover therefore cannot promise exactly-once writes across arbitrary S3 services. Sources should remain stable during a transfer. Enabling task leases raises the database format to schema 5; safely rejoining a retired node requires schema 8, credential encryption requires schema 9, and queue handoff requires schema 10. Downgrading requires the matching pre-upgrade database backups for both Master and Agents.

## Database space

Agents release scan batches after the Master durably saves, acknowledges and authorizes their release. Completed scans also release their deduplication indexes in the background. Permanently deleted tasks also release their scan and execution records. Freed pages are reused; to shrink an existing database file, follow [database maintenance](database-maintenance.md).

## Resume a scan

Stopping a task or restarting its scanning Agent preserves the scan position in the Agent database. OSS scans continue with the saved listing token. Local scans reopen the active directory path and skip completed subdirectories and previously processed files. A very large active directory still needs its names read and sorted on restart; files before the saved position are not individually inspected. File-list tasks also preserve their position.

Checkpoints are committed with the preceding scan results. A restart may replay a small uncommitted portion, which is deduplicated before creating child tasks. Checkpoints add bounded traversal state per task, not a second copy of every file name. Upgrade the scanning Agent to enable this behavior. Unfinished scans from older versions without a checkpoint need one compatibility rescan to establish their first checkpoint. The Master also retains checkpoints covered by saved assignments. A replacement scanner uses that boundary, which can be behind the original Agent’s private scan position. Tasks without a transferable checkpoint still need a compatibility rescan. Shared local paths may differ between nodes, but the original volume and active directory identities must match.

This continues the same traversal; changes behind its saved position may not be discovered. Keep the source stable or use a snapshot when a fixed dataset is required. A rejected OSS token or a replaced active local directory stops the scan with an error rather than silently restarting or skipping data. An already completed scan is not repeated when its transfers resume.
