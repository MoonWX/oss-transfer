# Concurrency, comparison and updates

[中文](operations.zh-CN.md)

## Node concurrency

Open **Execution nodes → Edit node** to change child-task concurrency and the object concurrency within each child. The defaults are 32 children and 4 objects per child, allowing up to 128 simultaneous object transfers on a node. Existing saved settings are retained.

Changes apply to running work and survive a restart. Reducing a limit lets the current requests finish before admitting more. Increasing it can help when the network is underused; node and task bandwidth/QPS limits still apply. Child concurrency accepts 1–256, object concurrency 1–64, with a combined maximum of 1024.

## Large objects

Within one Agent, an object of 128 MiB or more is transferred as parallel byte ranges instead of one stream. Uploads to OSS send the ranges as multipart parts. Downloads to a local directory fetch 8 MiB ranges concurrently but write the temporary file strictly in order, as one sequential stream: a range that arrives early waits in memory until the ranges before it are written. Network and FUSE filesystems that are slow at scattered writes therefore see the same write pattern as a single-stream download. The file is synced and published under its final name only after every range has been written, so a failed, stopped or interrupted download never leaves a partial file at the destination; a temporary file left by a crash is removed when the Agent restarts.

Each object is guaranteed four concurrent ranges and borrows up to 16 while the node has idle object slots. A single large object can therefore use an otherwise idle node, while a busy node is not oversubscribed. A downloaded range holds its slot and its memory until it is written, so each object keeps at most 16 ranges (128 MiB) in memory, and all uploads and downloads on an Agent share its 256 MiB transfer memory budget. When the disk falls behind, fetching pauses. Every range is a separate conditional GET pinned to the source version: it counts against the task's LIST / GET request limit (node request limits do not cover object GETs; node bandwidth still applies), and a source changed during the transfer fails the object. An interrupted range is fetched again, up to three reads in total. A task with its own bandwidth, LIST / GET or PUT limit (a HEAD-only limit does not count) still downloads to a local directory in ranges, but bounded by its current share on that node: each object holds at most about one second of the share in ranges, that is, the share in bytes per second (and, with a LIST / GET limit, the GETs per second) divided by the 8 MiB range size, between 2 and 16 ranges. With a share under two ranges per second (16 MiB/s, about 134 Mbps, or 2 GETs/s) the object is downloaded as one stream. A range waiting on the task limit therefore never pins more memory than the task drains in about a second, and cannot starve unthrottled tasks of the shared budget. Bytes still take task bandwidth as they are read, so the ranges together never exceed the share. The window is computed when each object starts, so a share that changes as nodes become busy or idle applies from the next object. Uploads to OSS of a throttled task still stream each object.

Live progress counts the bytes of all ranges together and does not move backwards when a range is fetched again.

### Ranged download option

Download tasks (object storage to a local directory) have a **Ranged download** option in the **Transfer options** section of **Create transfer**: **Auto (default)** or **Off**. Auto, described above, is right for almost every task. Choose Off only if parallel range reads cause trouble, for example storage that throttles many concurrent GET requests or an Agent short of memory, or when support asks; every object of that task is then downloaded as one sequential stream. The choice is fixed when the task is created and kept through stop, resume, retry and node changes. Task details show it, and tasks created before this option behave as Auto.

From the CLI, add `--ranged-download off` to `task create oss-to-local` (see [CLI](cli.md#create-tasks-and-set-limits)). In the API and in import files, a task request row takes `"rangedDownload": "auto"` or `"off"`; omitting it means auto, and `off` is rejected for tasks whose destination is not a local directory. Task details and exported rows include `rangedDownload`.

Agents from before this option never download in ranges, so they already behave as Off. During a rolling upgrade the Master sends the choice only to Agents that report support for it, and asks an Agent that did not support it again on its next delivery. A child task delivered without the choice keeps one stream per object, for Auto tasks too, until the task is stopped and resumed. This covers children accepted by an Agent before it was upgraded and children delivered by a Master from before this option, including to Agents that were already upgraded. An Off choice is therefore never lost during an upgrade.

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

For a planned downgrade, make a separate backup of each service's programs, configuration and stopped database before upgrading. Downgrading the Master below the release that divides task limits by demand briefly allows running tasks to exceed their limits; see [task limits across nodes](#task-limits-across-nodes). Successful upgrades do not leave a rollback copy. See [manual recovery](#recover-from-a-failed-upgrade) if automatic rollback could not finish.

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

## Change a task's execution nodes

Open a task, then **Execution details → Execution nodes → Edit nodes**, or use [`oss-transfer-cli task nodes`](cli.md#change-a-tasks-execution-nodes). The Master endpoint is `GET`/`POST /api/master/tasks/{id}/nodes`. The dialog lists the current nodes with their state, the scanning node, unfinished child tasks and each node's effective share of the task's bandwidth and QPS limits.

**Adding.** Only online, enrolled Agents that support task leases, recovery fencing and queued handoff can be added; the same checks gate automatic takeover. Older Agents are listed but rejected with a reason, as is a task whose current nodes are too old. For uploads and downloads, each added node needs the local path of the task's shared directory. The same preflight as task creation writes a test file through one node and reads it through the current and added nodes. A new node starts as *joining*: it gets no work until the Master has revoked any old authority on it, verified the task's shared-directory marker and given it a fresh ownership epoch. It then receives future child tasks, and queued children that never started are moved to it with the queue handoff described above. Adding a node to a single-node task first enables lease fencing for the task; the existing node's running transfers pause briefly while that is armed and continue from their saved progress. Adding a node switches an affinity task to balanced placement: under affinity an added node would receive no work but still take half of the primary's bandwidth and QPS share.

**Removing.** At least one node that can receive work must remain; joining nodes do not count, because a joiner may never finish its handshake. To replace a node, add the new one first and remove the old one after it has joined.

- *Remove gracefully* (drain; the default): the node receives no new children. Its queued children that never started move to the remaining nodes; the current owner revokes each one atomically before ownership changes. Running children finish on it. It then leaves the task.
- *Remove now*: the node's execution lease is revoked immediately, which cancels its running transfers. The Master collects their last saved progress, and the children continue on the remaining nodes through the takeover path; completed objects are checked rather than copied again. This needs takeover support (OSS-to-OSS or a verified shared directory) and is rejected otherwise.

If a peer disconnects while the only other node is draining, the peer's work moves to the draining node, which runs it or hands it off again before it leaves. A leaving node is fenced before anything moves: an online node acknowledges a durable lease revocation and stops all I/O for the task; an offline node is handled like a disconnected node and leaves when its lease expires. Without lease fencing (an exclusive local directory, or an Agent without lease support) its work cannot move: it leaves only after it reconnects and finishes or hands off its work. Failed children still assigned to it move with it and are retried by the remaining nodes. If it was scanning, the scan moves to a remaining node from the last checkpoint covered by saved assignments; when the scan had already finished and every batch was assigned, nothing needs to move. A removed node never rejoins the task automatically; add it again to bring it back.

**Progress.** While the dialog is open it refreshes at the console's refresh interval, and pauses while the page is hidden. A refresh only updates states, unfinished child tasks and shares: nodes you marked, local paths and a passed preflight stay as they were, and the confirmation step keeps the summary you are confirming. Only a node that is scanning, or taking the scan over, is marked as the scanning node. Once the scan has finished and every batch has been assigned, the dialog shows *Scan complete* and marks no node. Removing the scanning node mid-scan moves the scan to a remaining node, which continues from the checkpoint saved on the Master rather than listing the source again.

The stages of a requested change are progress, not task errors: a leaving node refusing work once it has been fenced, an offline leaving node waiting for its lease to expire, the scan moving to another node, and a node that has just joined before its first preparation. The dialog shows them on the node rows. Without lease fencing (an exclusive local directory, or an Agent without lease support) an offline leaving node cannot be waited out: the dialog says it leaves after it reconnects. An unexpected disconnect or a failing node is still reported on the task, naming the node. The task detail shows such a runtime problem once it has persisted for about four seconds (with a refresh interval of four seconds or less, also across two refreshes), and removes it after it has been absent as long, so a problem raised and cleared by a single step does not flash. An ended task's error shows at once. The limit status reports a node's older limits as *syncing* only until the Master has tried to deliver the new ones; after a failed delivery it reports them as not applied. Known runtime messages are shown in the console's language; other details follow a translated label.

**Bandwidth and QPS.** See [task limits across nodes](#task-limits-across-nodes). When a node joins, the other nodes first confirm their reduced shares; the new node receives its first execution lease once all reserved shares fit the task limit. A node that is offline then delays the join only if its last share is still needed to stay within the limit. When a node leaves, the other shares are raised only after it has been fenced. A draining node keeps its share while it still runs child tasks; once it has none left it drops to the idle share. A joining node needs at least one byte per second of the task's bandwidth limit.

**Limits.** Single-object tasks, single-file uploads, completed tasks, deleted tasks and tasks migrated from the previous version cannot change nodes. Stopped or failed tasks can; the change applies when they resume. Until lease fencing has been enabled for a task (for example a single-node task, running or stopped), every current node must be online for a change, because an unreachable node could still hold unfenced work. While a task is stopping or resuming, retry after it settles. Only one change is in progress at a time. While nodes are joining or draining, the Master accepts only these changes, so a pending change can always be undone: removing a joining node (cancels the join), adding a draining or immediately removed node back before it has left (cancels its removal; local transfers keep its existing path, so no new path or preflight is needed), removing a draining node immediately, or removing a disconnected node whose work has already been taken over. Anything else is rejected until the previous change finishes. Every change carries a request ID and the membership revision it was based on, so a retried request is applied once and a change based on stale information is rejected. Changes are saved in the Master database and continue after a Master or Agent restart. The first change raises the Master database to schema 11.

## Task limits across nodes

A task's bandwidth limit and its cluster-wide LIST / HEAD / PUT limits are totals for the whole task. The Master divides them among the active nodes by demand, as the task runs. A node is busy while it has unfinished child tasks, can receive new work (the scan is still running or batches are waiting to be assigned), takes part in a queued handoff, or is the scanner of an unfinished scan. Every other node is idle and keeps 1/64 of an equal share (at least one byte per second); the busy nodes share the rest equally, within any per-node cap. For example, with a 1000 Mbps limit on two nodes, a node that finishes its work releases its share while the other is still transferring its last large object: the busy node gets about 992 Mbps instead of 500. A node counts as idle only after two reconciliation steps in a row (about half a second); a node that receives work again is raised in the next step.

At no moment do the shares that Agents may enforce add up to more than the task limit. A node's share is lowered first; the share it released goes to other nodes only after that node has confirmed the lower value. While you lower a limit, the sum stays within the previous limit and falls to the new one as nodes confirm. A node that is offline keeps its last share until it confirms or, with lease fencing, until its execution lease expires and it is fenced; without lease fencing it keeps it until it reconnects. Per-node limits (`PER_NODE` scope) are not divided, and node bandwidth and request limits still apply.

The node view and `task nodes` show each node's demand (*busy* or *idle*) and its current share. *Adjusting* means the node still holds its previous, larger share until it confirms the reduction. When a task has a bandwidth, LIST / GET or PUT limit, a single large object downloaded to a local directory uses about one second of the node's share in parallel ranges, and one stream under two ranges per second; uploads to OSS stream each object (see [large objects](#large-objects)). A busy node uploading one large object can therefore receive almost the whole limit but still be bounded by the throughput of one connection.

Tasks that were already running before the upgrade keep the equal split until every node has confirmed it, then switch to demand-based shares. Agents need no upgrade; the Agent protocol is unchanged.

**Downgrading the Master.** An older Master divides limits equally and uses a newer limit revision, which Agents accept. For a moment the shares may then add up to more than the limit (at most the dynamic share plus the equal share of each node). To avoid that window, stop running tasks before downgrading.

## Database space

Agents release scan batches after the Master durably saves, acknowledges and authorizes their release. Completed scans also release their deduplication indexes in the background. Permanently deleted tasks also release their scan and execution records. Freed pages are reused; to shrink an existing database file, follow [database maintenance](database-maintenance.md).

## Resume a scan

Stopping a task or restarting its scanning Agent preserves the scan position in the Agent database. OSS scans continue with the saved listing token. Local scans reopen the active directory path and skip completed subdirectories and previously processed files. A very large active directory still needs its names read and sorted on restart; files before the saved position are not individually inspected. File-list tasks also preserve their position.

Checkpoints are committed with the preceding scan results. A restart may replay a small uncommitted portion, which is deduplicated before creating child tasks. Checkpoints add bounded traversal state per task, not a second copy of every file name. Upgrade the scanning Agent to enable this behavior. Unfinished scans from older versions without a checkpoint need one compatibility rescan to establish their first checkpoint. The Master also retains checkpoints covered by saved assignments. A replacement scanner uses that boundary, which can be behind the original Agent’s private scan position. Tasks without a transferable checkpoint still need a compatibility rescan. Shared local paths may differ between nodes, but the original volume and active directory identities must match.

This continues the same traversal; changes behind its saved position may not be discovered. Keep the source stable or use a snapshot when a fixed dataset is required. A rejected OSS token or a replaced active local directory stops the scan with an error rather than silently restarting or skipping data. An already completed scan is not repeated when its transfers resume.
