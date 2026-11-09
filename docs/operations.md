# Concurrency, comparison and updates

[中文](operations.zh-CN.md)

## Node concurrency

Open **Execution nodes → Edit node** to change **Subtask concurrency** and **Objects per subtask**. The defaults are 32 child tasks and 4 objects per child task, allowing up to 128 simultaneous object transfers on a node. Existing saved settings are retained. From the CLI, run `node update <node> --child-concurrency 32 --object-concurrency 4` ([CLI](cli.md#manage-nodes-and-exceptions)).

Changes apply to running work and survive a restart. Reducing a limit lets the current requests finish before admitting more. Increasing it can help when the network is underused; node and task bandwidth/QPS limits still apply. Subtask concurrency accepts 1–256 and objects per subtask 1–64, with a combined maximum of 1024. Idle object slots also let large objects fetch more ranges at once ([large objects](#large-objects)).

## Node limits

Two kinds of limits apply to a task's requests. Node limits belong to one Agent and are shared by all tasks running on it. Task limits are totals for one task, divided among its nodes ([task limits across nodes](#task-limits-across-nodes)). A request is sent only when both allow it; requests outside tasks, such as object comparisons, count only against node limits. When the storage service throttles requests, the Agent temporarily lowers its own node request limits ([storage throttling](#storage-throttling)); task limits never change on their own.

Each Agent has four node limits:

| Limit | Default | Covers |
| --- | --- | --- |
| LIST | 100 requests/s | Object listings (ListObjects, ListBuckets) |
| PUT | 1,400 requests/s | Uploads (PutObject, UploadPart, CreateMultipartUpload, CompleteMultipartUpload) |
| HEAD | 2,800 requests/s | Object metadata (HeadObject) |
| Bandwidth | 2,000 Mbps | Data read from the source, every range of a large object included |

Object GETs, including the range reads of large objects, have no node request limit: node bandwidth applies to them, and a task's LIST / GET limit caps their rate. Multipart listings (ListParts) and aborts have no node limit either. Each retry counts as a new request.

**Change node limits.** Open **Execution nodes**. On **Live status**, each node card has a **Requests and bandwidth** section with a row per limit. Choose **Adjust** on a row to change that limit on that node, or **Adjust limits** at the top of the page to apply one value to every node (**Apply to: All workers**); each node then gets that value, not a share of it. Request limits are entered in requests per second and bandwidth in Mbps; 0 means unlimited. The Master accepts 0.01–1,000,000 requests/s and up to 1,000,000 Mbps. From the CLI, use `limit set` ([CLI](cli.md#set-node-limits)).

The saved value becomes the node's target. It applies at once, ends any automatic reduction of that limit and is kept across Agent restarts. If some Agents do not confirm the change, the dialog stays open, says how many nodes did not confirm and lists each of them with its reason: common reasons such as a timeout are translated, others are shown as the Agent reported them.

**What a row shows.** **Active limit** is the limit the node enforces now; it is below the target only during an automatic reduction. The target is labeled **Configured target** when it equals the configured value (from `nodeControl`, or the built-in default), or **Saved target** when another value was saved from the console or CLI. **Measured rate** is the requests sent per second, retries included, averaged over the last 5 seconds; for bandwidth, the data read from the source. The chip beside the limit's name shows its [status](#limit-status).

**Configuration file.** The defaults above are built into the Agent. To change them, add a `nodeControl` object to the Agent's `service.json` (`/opt/oss-transfer/agent/config/service.json` on Linux; see [paths](install.md#paths-and-ports)) with any of `listQps`, `putQps`, `headQps` and `bandwidthBitsPerSecond` (bit/s). A value of 0 means unlimited, and keys you leave out keep their defaults. A newly installed Agent has no `nodeControl` object; an Agent converted from V4 has one with all four limits. Edits take effect when the Agent restarts:

- A limit saved from the console or CLI keeps its saved value. There is no reset: saving the configured value shows **Configured target** again, but the value stays saved. The exception is a limit that has never been saved and runs at its configured value: saving that same value stores nothing, so later `service.json` edits still apply to it.
- A request limit that is turned on (from 0), lowered or turned off (set to 0) applies at once. If the limit is being reduced automatically and its new target is at or below the active limit, the reduction ends.
- A raised request limit takes effect gradually: after the restart the row shows *Automatically reduced* and then *Recovering gradually* while the active limit climbs by 20% every 10 seconds until it reaches the new value.
- A changed `bandwidthBitsPerSecond` applies at once, since bandwidth is never lowered automatically. Bandwidth is enforced in whole bytes per second, so a value that is not a multiple of 8 is rounded down (500000004 becomes 500000000 bit/s).

Changes from the console or CLI need no restart. Agents installed with Docker have no `service.json` and start with the defaults. The `comparison` object in the same file limits the comparison results an Agent keeps ([result storage and retention](#result-storage-and-retention)).

**Request limits are average rates.** Node and task request limits space requests evenly, one every 1/rate seconds, with no bursts, so fractions are valid: 0.5 means one request every 2 seconds. Node limits go down to 0.01 requests/s and task limits down to 0.000001. A task limit is divided among the task's nodes, and each node spaces requests by its own share: at 0.5/s on two busy nodes, each node sends one request every 4 seconds. Under a task limit, a node's first request of each type also waits one interval. Measured rates are averaged over 5 seconds and so change in steps of 0.2 requests/s per node: a node sending 0.5 requests/s shows 0.4 or 0.6.

## Task limits across nodes

Set task limits in **Create transfer** under **Transfer options → Task limits**, and change them in the task details with **Adjust** (**Set limit** while the task has no bandwidth limit) or with `task limits` ([CLI](cli.md#create-tasks-and-set-limits)). **Task bandwidth limit** accepts 0.1–1,000,000 Mbps; left blank, only node limits apply. **LIST / GET QPS** caps listings and every object GET, range reads included; **HEAD QPS** caps metadata requests; **PUT QPS** caps uploads, multipart requests included. A blank or 0 QPS sets no limit for that type. The limits of a stopped or finished task cannot be changed; a stopped task resumes with the limits it had.

The task details show the limits as **Request limit (task total)**, with up to six decimals. The **Request rate** table shows, per request type:

- **Current**: requests per second, retries included, averaged over the last 5 seconds. GET counts object reads; LIST includes multipart listings.
- **Task limit**: the task's limits.
- **Effective limit**: the most the nodes can send. For HEAD and PUT it adds up, over the nodes, the lower of the task's share and the node limit; for LIST / GET it adds up the task's shares alone, because node LIST limits do not cover GETs. Node limits are shared with other tasks, so the actual ceiling can be lower.

The table rounds to one decimal, so a limit below 0.05 requests/s shows as 0. Rates appear only when every node reports them.

A task's bandwidth limit and its cluster-wide LIST / GET, HEAD and PUT limits are totals for the whole task. The Master divides them among the active nodes by demand, as the task runs. A node is busy while it:

- has unfinished child tasks;
- can receive new work: the scan is still running or batches are waiting to be assigned;
- takes part in a queued handoff; or
- is the scanner of an unfinished scan.

While the task is not running, for example while it resumes or stops, and while the work or scan of a fenced node is being moved, every node counts as busy. Every other node is idle and keeps 1/64 of an equal share (at least one byte per second); the busy nodes share the rest equally, within any per-node cap. For example, with a 1000 Mbps limit on two nodes, a node that finishes its work releases its share while the other is still transferring its last large object: the busy node gets about 992 Mbps instead of 500. A node counts as idle only after two reconciliation steps in a row (about half a second). A node that receives work again is raised at once, as soon as the other nodes have confirmed their lower shares.

At no moment do the shares that Agents may enforce add up to more than the task limit. A node's share is lowered first; the share it released goes to other nodes only after that node has confirmed the lower value. While you lower a limit, the sum stays within the previous limit and falls to the new one as nodes confirm. A node that is offline keeps its last share until it confirms or, with lease fencing, until its execution lease expires and it is fenced; without lease fencing it keeps it until it reconnects. Per-node limits (`PER_NODE` scope) are not divided, and node bandwidth and request limits still apply.

Bandwidth limits, both a task's share on a node and the node's own bandwidth limit, are enforced over time rather than at every instant. A transfer that pauses, for example while a slow disk catches up, may then read faster than the limit until it has made up the pause, but by at most 100 ms of the limit (at least 64 KiB, at most 16 MiB). Over any period an Agent therefore reads at most about the limit times that period plus this amount, and a newly set or changed limit starts with none of it. This lets a limited task use nearly all of its share even when its destination stalls briefly. Request limits have no such allowance: requests are spaced evenly.

The execution nodes dialog and `task nodes` show each node's current share and its demand: *On demand (busy)* or *Idle (released)*. *Adjusting: waiting for the node to confirm its reduction* means the node still holds its previous, larger share until it confirms. An older Master, which divides limits equally, shows no demand. A busy node can receive almost the whole limit, but a single large object may not use all of it: a download to a local directory reads about one second of the node's share in parallel ranges, while a transfer to OSS of a task with its own bandwidth, LIST / GET or PUT limit streams each object and is bounded by the throughput of one connection ([large objects](#large-objects)).

Tasks that were already running when the Master was upgraded from a release that divided limits equally keep the equal split until every node has confirmed it, then switch to demand-based shares. Agents need no upgrade; the Agent protocol is unchanged.

**Downgrading the Master.** An older Master divides limits equally and uses a newer limit revision, which Agents accept. For a moment the shares may then add up to more than the limit (at most the dynamic share plus the equal share of each node). To avoid that window, stop running tasks before downgrading.

## Storage throttling

A storage service throttles a client that sends too many requests: it answers with HTTP 503 or 429, or with an error code such as `SlowDown`, `Throttling`, `RequestLimitExceeded` or `TooManyRequests`, whatever the status. The Agent then retries the request after a pause and lowers its own limit for that type of request, raising it again step by step once the throttling stops.

Short-lived throttling therefore does not fail objects. 503 or 429 responses in the storage provider's logs, or reductions in the limit history, show that the Agent slowed down, not that objects failed.

### Retries

A throttled request gets up to 10 attempts. Before each retry the Agent waits a random time of up to 1, 2, 4, 8 and 16 seconds, then up to 20 seconds: about a minute over all ten attempts on average, at most 111 seconds. Each retry passes the node and task request limits again, so retries slow down with the limits. Other retryable errors, such as HTTP 500 or a dropped connection, keep 3 attempts. Throttled retries do not use up the S3 SDK's retry quota, and stopping a task ends a waiting retry at once.

An object request (GET, HEAD or PUT) that is still throttled on its 10th attempt fails that object. The error contains `exceeded maximum number of attempts, 10` and the error code, such as `SlowDown`. Retry those objects once the throttling has eased ([retry failed objects](#retry-failed-objects-of-a-running-task)). A range of a large object is read again only when its body is interrupted, up to three reads in total, so one range makes at most 30 GET requests.

A listing that is still throttled on its 10th attempt stops the scan with that error instead. The same applies to the HEAD requests with which a task by file list or single object looks up its keys. Once the objects already listed are done, the task ends as partially failed (failed if no object was transferred or skipped). Objects not yet listed are not recorded as failed objects, so retrying failed objects while the task runs does not transfer them. Retry the task after it has ended: the scan then continues where it stopped ([resume a scan](#resume-a-scan)).

### Automatic reduction

Each Agent counts throttled responses separately for its LIST, PUT and HEAD limits over the last 5 seconds. When one limit has at least 5 throttled responses and they are at least 1% of that limit's responses, the Agent lowers it. Successful responses in between do not reset the count, so throttling among many concurrent requests is still noticed, while sporadic throttling, such as 5 of 1,000 responses, lowers nothing.

The limit drops to 90% of the lower of the active limit and the most requests of that type sent in one of the last 5 seconds, so a limit far above the actual traffic drops at once: a limit of 1,000/s with traffic peaking at 300/s becomes 270. It never drops below 200 requests/s, or below the target if that is lower. Limits already at that floor are not lowered, nor are unlimited ones; with the defaults this includes the LIST limit of 100 requests/s. After a reduction, the same limit is not lowered again for 5 seconds and needs new throttled responses, so one burst lowers it once.

Only the active limit changes: the target, task limits and bandwidth limits are never adjusted automatically. Throttled object GETs, multipart listings and aborts are retried but lower no limit; a task LIST / GET limit caps GETs. Each Agent detects throttling and adjusts its limits on its own, and an active reduction is kept when the Agent restarts, unless the limit's new target in `service.json` is at or below the active limit, or 0.

If throttling keeps coming back, set the limit below the rate at which it starts on every Agent that uses the bucket, or limit the task. **Measured rate before reduction** on the adjustment details shows that rate.

### Recovery

Once throttling stops, a lowered limit climbs back to its target. The first step comes 10 seconds after the last throttling that met the rule above; continued throttling keeps postponing it. The size of the steps depends on how long ago the limit was last lowered:

| Since the last reduction | Each step adds | Next step |
| --- | --- | --- |
| Less than 10 minutes | 5% of the throttled rate | After 30 s; each further reduction within those 10 minutes doubles this, up to 8 minutes |
| 10 minutes or more, or after an Agent restart | 20% of the active limit | After 10 s |

A reduction lands at 90% of the throttled rate, so the first two steps return to that rate and later ones probe above it. With the defaults, a PUT limit throttled at its full 1,400 requests/s drops to 1,260, rises to 1,330 after 10 seconds and is back at 1,400 another 30 seconds later. Probing slowly keeps several Agents that share one bucket's request limit from being throttled again every few seconds. Saving a limit from the console or CLI ends a reduction at once.

### Limit status

Each limit row on **Execution nodes**, and each row of `limit list`, shows a status:

| Console | CLI | Meaning |
| --- | --- | --- |
| *Automatically reduced* | *Reduced* | Lowered after throttling; lasts until the first recovery step, at least 10 seconds |
| *Recovering gradually* | *Recovering* | Raised at least once, still below the target; a new reduction returns it to *Automatically reduced* |
| *Target restored* | *Recovered* | Back at the target within the last minute |
| *At target limit* | *Normal* | Running at the target |
| *Unlimited* | *Unlimited* | No limit: the target is 0 |
| *Status unconfirmed* | — | Auto-refresh is paused, the last status request failed, or the status is older than 30 seconds (or two refresh intervals, if longer); the values are from the last reading |

Bandwidth is never adjusted, so its row shows only *At target limit* or *Unlimited* (or *Status unconfirmed*). While a request limit is reduced or recovering, its row shows a progress bar, **Active limit / target limit**, and the node card shows *The node may be throttled. Its active limits were adjusted automatically.* with **View details**. The other console pages show a banner for each such node, and on any page a notification appears when a node's limits are reduced, raised or back at target.

### Limit history

The **Limit history** tab on **Execution nodes** lists the 50 most recent adjustments across all nodes, newest first: the change of the active limit, the stage (*Manual change*, *Automatically reduced*, *Recovering gradually* or *Target restored*), the reason and the target at the time. A reduction's reason gives the throttled responses in the window and their share, for example *Storage throttling (HTTP 503, 429, SlowDown or similar): 12 responses in 5 s, 2.4% of these requests.*

**View details** opens **Adjustment details**: the request that triggered the reduction (action, task, bucket, key, HTTP status code, error code, and the request ID to give the storage provider), the trigger conditions and every step of that round. **Increase per step** shows the step used 10 minutes after a reduction (20% by default); before that, the steps are the smaller probes described above. Records from older versions show only HTTP 503 counts.

Each Agent keeps every adjustment in its database. From the CLI, `limit history --limit 200` lists up to 200 and `limit adjustment` shows one round ([CLI](cli.md#set-node-limits)).

### Tune automatic reduction

The `autoAdjust` object inside `nodeControl` in the Agent's `service.json` changes these rules. Keys that are left out keep their defaults:

| Key | Default | Effect |
| --- | --- | --- |
| `throttleThreshold` | 5 | Throttled responses needed in the window (1–1,000,000); `consecutive503Threshold` is accepted as its earlier name |
| `throttleRatio` | 0.01 | Share of that limit's responses that must be throttled (above 0, at most 1) |
| `throttleWindowMs` | 5000 | Length of the window, and the pause between two reductions of one limit (1,000–60,000, rounded up to whole seconds) |
| `reduceFactor` | 0.9 | Factor applied by a reduction (above 0, below 1) |
| `minQps` | 200 | Lowest value a reduction sets; a target at or below it is never lowered (above 0) |
| `recoveryIntervalMs` | 10000 | Wait before the first recovery step, and between steps after 10 minutes without a reduction; probes wait three times as long |
| `increaseFactor` | 1.2 | Factor of each step after 10 minutes without a reduction (above 1) |

For example, to let reductions lower the default LIST limit as well, set `minQps` below 100. Add the key to the existing JSON object and keep the other keys:

```json
"nodeControl": {
  "autoAdjust": { "minQps": 20 }
}
```

A `minQps` at or above every target keeps all limits from being lowered; throttled requests are still retried. An unknown key, an invalid value, or a `throttleThreshold` that differs from `consecutive503Threshold` keeps the Agent from starting. Check the file before restarting the Agent:

```sh
sudo /opt/oss-transfer/agent/bin/oss-transfer-agent --config /opt/oss-transfer/agent/config/service.json --check-config
sudo systemctl restart oss-transfer-agent
```

## Large objects

Within one Agent, an object of 128 MiB or more is transferred as parallel byte ranges instead of one stream. Transfers to OSS send the ranges as multipart parts. Downloads to a local directory fetch 8 MiB ranges concurrently but write the temporary file strictly in order, as one sequential stream: a range that arrives early waits in memory until the ranges before it are written. Network and FUSE filesystems that are slow at scattered writes therefore see the same write pattern as a single-stream download. The file appears under its final name only after every range has been written ([temporary files](#temporary-files)).

Each object is guaranteed four concurrent ranges and borrows more, up to 16 in total, while the node has idle object slots. A single large object can therefore use an otherwise idle node, while a busy node is not oversubscribed. Every range in flight holds memory from the Agent's 256 MiB transfer memory budget, which all uploads and downloads on the Agent share. A downloaded range keeps its slot and its memory until it has been written, so one object holds at most 16 ranges (128 MiB), and fetching pauses when the disk falls behind.

From object storage, every range is a separate conditional GET pinned to the source version. It counts against the task's LIST / GET request limit (node request limits do not cover object GETs; node bandwidth still applies), and a source that changes during the transfer fails the object. An interrupted range is fetched again, up to three reads in total. Storage that does not serve conditional byte ranges is read as one stream. Live progress counts the bytes of all ranges together and does not move backwards when a range is fetched again.

Limits also bound the ranges of a download to a local directory. Each object holds at most about one second of what its reads may take on that node: the lower of the task's bandwidth share and the node bandwidth limit, divided by the 8 MiB range size, and, with a LIST / GET limit, no more ranges than the task's GETs per second on that node; never more than 16. An object below two ranges per second (16 MiB/s, about 134 Mbps, or 2 GETs/s) is downloaded as one stream. This is decided when the object starts; the window is checked again before each further range, so a share or node limit that drops shrinks it at once. Ranges wait for the limit while holding memory, so the window keeps an object from pinning more of the shared budget than it reads in about a second, and all its ranges together still stay within the share. Transfers to OSS stream each object when the task has its own bandwidth, LIST / GET or PUT limit (a HEAD-only limit does not count); a node limit alone does not make them stream.

Under a bandwidth limit, the ranges of an object downloaded to a local directory take the bandwidth lowest offset first, so they complete one after another, in the order they are written, rather than all together at the end of the window. A range that has waited 10 seconds is read next, so that a server or proxy does not close its open request for lack of reads.

### Ranged download option

Download tasks (object storage to a local directory) have a **Ranged download** option in the **Transfer options** section of **Create transfer**: **Auto (default)** or **Off**. Auto, described above, is right for almost every task. Choose Off only if parallel range reads cause trouble, for example storage that throttles many concurrent GET requests or an Agent short of memory, or when support asks; every object of that task is then downloaded as one sequential stream. The choice is fixed when the task is created and kept through stop, resume, retry and node changes. Task details and `task show` display it for downloads to a local directory, and tasks created before this option behave as Auto.

From the CLI, add `--ranged-download off` to `task create oss-to-local` (see [CLI](cli.md#create-tasks-and-set-limits)). In the API and in import files, a task request row takes `"rangedDownload": "auto"` or `"off"`; omitting it means auto, and `off` is rejected for tasks whose destination is not a local directory. Task details and the rows saved by **Export JSON** include `rangedDownload`.

Agents from before this option never download in ranges, so they already behave as Off. During a rolling upgrade the Master sends the choice only to Agents that report support for it, and asks an Agent that did not support it again on its next delivery. A child task delivered without the choice keeps one stream per object, for Auto tasks too, until the task is stopped and resumed. This covers children accepted by an Agent before it was upgraded and children delivered by a Master from before this option, including to Agents that were already upgraded. An Off choice is therefore never lost during an upgrade.

## Temporary files

A download to a local directory, ranged or not, is written to a temporary file that is synced and published under its final name only after every byte has arrived, so a failed, stopped or interrupted download never leaves a partial file at the destination. Each Agent keeps these files in its own hidden staging directory, `.oss-transfer-staging-…`, inside each destination directory it writes to, and records the directory and each file in its database before writing. When the Agent restarts, it removes temporary files left by a crash; it also records each multipart upload to OSS and aborts those a crash left incomplete. Staging directories stay in place after a transfer, and scans of a local source skip them. If a destination directory or its staging directory is deleted, for example when the directory is removed and created again during a task, the Agent creates a new staging directory the next time it writes there.

## Disconnected nodes

Multi-node tasks can move unfinished child tasks to another selected node after an Agent loses contact. All participating Agents must support task leases. The Master renews execution permission every 20 seconds; an isolated Agent stops starting work and cancels active transfers when its 60-second lease expires. Reassignment waits another 10 seconds. The execution nodes dialog marks such a node **Offline** while it is unreachable and **Disconnected** once its work has been taken over. A returning node can rejoin when both sides support recovery fencing and the original shared directory is still accessible. Its old execution permission is revoked before it receives a new ownership epoch. Older Agents remain excluded from that task.

The recovered node can receive new assignments and queued children from healthy nodes, even after scanning has finished. Redistribution uses the task's existing placement policy and its load estimate of remaining bytes plus 64 KiB per object. Only children that have never started can move: the current owner must atomically revoke their execution permission before the Master changes ownership. Running and interrupted transfers stay with their current owner. Rebalancing runs in bounded batches after recovery; it does not continuously reshuffle the queue.

Takeover supports OSS-to-OSS and verified shared directories. A small hidden marker stays in each shared directory so another node can verify that it still sees the original volume. A private local directory, a missing marker, or an older Agent prevents automatic reassignment. Completed children stay unchanged; incomplete children keep their numbers and saved file results. The new owner checks already completed objects before continuing. If the scanner fails, another node takes the scan over ([scan handoff](#scan-handoff)).

A request already accepted by object storage may finish after its client is cancelled. Takeover therefore cannot promise exactly-once writes across arbitrary S3 services. Sources should remain stable during a transfer. Enabling task leases raises the database format to schema 5; safely rejoining a retired node requires schema 8, credential encryption requires schema 9, and queue handoff requires schema 10. Downgrading requires the matching pre-upgrade database backups for both Master and Agents.

## Change a task's execution nodes

Open a task, then **Execution details → Execution nodes → Edit node**, or use [`oss-transfer-cli task nodes`](cli.md#change-a-tasks-execution-nodes). The Master endpoint is `GET`/`POST /api/master/tasks/{id}/nodes`. The dialog lists the current nodes with their state, the scanning node, unfinished child tasks, and each node's share of the task's bandwidth and QPS limits with its demand ([task limits across nodes](#task-limits-across-nodes)).

**Adding.** Only online, enrolled Agents that support task leases, recovery fencing and queued handoff can be added; the same checks gate automatic takeover. Older Agents are listed but rejected with a reason, as is a task whose current nodes are too old; an Agent that is being updated is rejected until its update finishes. For uploads and downloads, each added node needs the local path of the task's shared directory. The same preflight as task creation writes a test file through one node and reads it through the current and added nodes. A new node starts as *Joining*: it gets no work until the Master has revoked any old authority on it, verified the task's shared-directory marker and given it a fresh ownership epoch. It then receives future child tasks, and queued children that never started are moved to it with the queue handoff described above. Adding a node to a single-node task first enables lease fencing for the task; the existing node's running transfers pause briefly while that is armed and continue from their saved progress. Adding a node switches an affinity task to balanced placement, so that the added node receives work instead of only holding a share of the limits.

**Removing.** At least one node that can receive work must remain; joining nodes do not count, because a joiner may never finish its handshake. To replace a node, add the new one first and remove the old one after it has joined.

- **Remove gracefully (recommended)**, the default: the node receives no new children. Its queued children that never started move to the remaining nodes; the current owner revokes each one atomically before ownership changes. Running children finish on it. It then leaves the task.
- **Remove now**: the node's execution lease is revoked immediately, which cancels its running transfers. The Master collects their last saved progress, and the children continue on the remaining nodes through the takeover path; completed objects are checked rather than copied again. This needs takeover support (OSS-to-OSS or a verified shared directory) and is rejected otherwise.

If a peer disconnects while the only other node is draining, the peer's work moves to the draining node, which runs it or hands it off again before it leaves. A leaving node is fenced before anything moves: an online node acknowledges a durable lease revocation and stops all I/O for the task; an offline node is handled like a disconnected node and leaves when its lease expires. Without lease fencing (an exclusive local directory, or an Agent without lease support) its work cannot move: it leaves only after it reconnects and finishes or hands off its work. Failed children still assigned to it move with it and are retried by the remaining nodes. If it was scanning, the scan moves to a remaining node ([scan handoff](#scan-handoff)). A removed node never rejoins the task automatically; add it again to bring it back.

**Progress.** While the dialog is open it refreshes at the console's refresh interval, and pauses while the page is hidden. A refresh only updates states, unfinished child tasks and shares: nodes you marked, local paths and a passed preflight stay as they were, and the confirmation step keeps the summary you are confirming. Each node row shows how its change stands. A joining node reads *Completing handshake and checks; work is assigned after it joins*. A leaving node counts the child tasks it still has *To finish or hand off*. An offline leaving node reads *Offline; it leaves when its execution lease expires and its work continues on the other nodes*, or, without lease fencing, *Offline; it leaves after it reconnects and finishes or hands off its work*. A note above the list tells where the scan stands ([scan handoff](#scan-handoff)).

The stages of a requested change are progress, not task errors: a leaving node refusing work once it has been fenced, an offline leaving node waiting for its lease to expire, the scan moving to another node, and a node that has just joined before its first preparation. An unexpected disconnect or a failing node is still reported on the task, naming the node. The task detail shows such a runtime problem once it has persisted for about four seconds (with a refresh interval of four seconds or less, also across two refreshes), and removes it after it has been absent as long, so a problem raised and cleared by a single step does not flash. An ended task's error shows at once. The task's limit dialog shows *Synchronizing rate limits…* while a node still holds its older limits, but only until the Master has tried to deliver the new ones; after a failed delivery it shows *Not applied on all nodes*, a warning that outside the dialog follows the same four-second rule. Known runtime messages are shown in the console's language; other details follow a translated label.

**Bandwidth and QPS.** When a node joins, the other nodes first confirm their reduced shares; the new node receives its first execution lease once all reserved shares fit the task limit. If an offline node still holds its last share, the new node starts with the room that remains and is raised once that share is released; the offline node delays the join only when no room remains. When a node leaves, the other shares are raised only after it has been fenced. A draining node keeps its share while it still runs child tasks; once it has none left it drops to the idle share. The task's bandwidth limit must allow at least one byte per second for every node, joining nodes included.

**Limits.** Single-object tasks, single-file uploads, completed tasks, deleted tasks and tasks migrated from the previous version cannot change nodes. Stopped or failed tasks can; the change applies when they resume. Until lease fencing has been enabled for a task (for example a single-node task, running or stopped), every current node must be online for a change, because an unreachable node could still hold unfenced work. While a task is stopping or resuming, retry after it settles. Only one change is in progress at a time. While nodes are joining or draining, the Master accepts only these changes, so a pending change can always be undone: removing a joining node (cancels the join), adding a draining or immediately removed node back before it has left (cancels its removal; local transfers keep its existing path, so no new path or preflight is needed), removing a draining node immediately, or removing a disconnected node whose work has already been taken over. Anything else is rejected until the previous change finishes. Every change carries a request ID and the membership revision it was based on, so a retried request is applied once and a change based on stale information is rejected. Changes are saved in the Master database and continue after a Master or Agent restart. The first change raises the Master database to schema 11.

## Retry failed objects of a running task

A running task can retry its failed objects without stopping. In the task details or on **Exceptions**, choose **Retry failed items**, or run `task retry <ID>` in the CLI. The Master reopens each finished child task that has failed objects (FAILED or PARTIAL_FAILED) on its own node, in the task's current run. Only the failed objects are transferred again; succeeded and skipped objects keep their results, and the rest of the task keeps transferring. A reopened child task waits for a free slot under the node's concurrency limit, and it makes its node busy for the task's bandwidth and QPS shares. Exception records show **Retrying** and then the result.

Child tasks that are still running keep their failed objects until they finish; retry again afterwards. Child tasks on a node that is leaving the task are skipped, because the node that takes over its work retries them. A repeated request does not reopen a child task twice. A stop requested during the retry stops the reopened child tasks with the rest of the task, and their objects show **Retry incomplete**. The node's Agent must be upgraded to this release; for an older Agent the Master reports the reason and leaves its child tasks unchanged. One request reopens at most 64 child tasks; retry again for more. Stopped, failed and partially failed tasks keep the ordinary retry, which resumes them.

## Resume a scan

Stopping a task or restarting its scanning Agent preserves the scan position in the Agent database. OSS scans continue with the saved listing token. Local scans reopen the active directory path and skip completed subdirectories and previously processed files. A very large active directory still needs its names read and sorted on restart; files before the saved position are not individually inspected. File-list tasks also preserve their position.

Checkpoints are committed with the preceding scan results. A restart may replay a small uncommitted portion, which is deduplicated before creating child tasks. Checkpoints add bounded traversal state per task, not a second copy of every file name. Upgrade the scanning Agent to enable this behavior. Unfinished scans from older versions without a checkpoint need one compatibility rescan to establish their first checkpoint.

This continues the same traversal; changes behind its saved position may not be discovered. Keep the source stable or use a snapshot when a fixed dataset is required. A rejected OSS token or a replaced active local directory stops the scan with an error rather than silently restarting or skipping data. An already completed scan is not repeated when its transfers resume.

### Scan handoff

When the scanning node leaves the task or loses contact, a remaining node takes the scan over once the old scanner has been fenced. The Master retains the checkpoints covered by the assignments it has saved. The new scanner rebuilds its scan index from those assignments and continues from the last source-page checkpoint they cover, which can be behind the original Agent's private scan position; scan-ahead results that never reached the Master are listed again, without downloading object contents. Tasks without a transferable checkpoint still need a compatibility rescan. Shared local paths may differ between nodes, but the original volume and active directory identities must match. Once the scan has finished and every batch has been assigned, no scanner is needed and nothing moves.

The execution nodes dialog marks only a node that is scanning, or taking the scan over, as the **Scanner node**, and a note tells where the scan stands: *Handing over the scan; it continues from the Master's checkpoint* while the new scanner rebuilds, *The scanning node has left; another node takes over the scan from the Master's checkpoint* between the old scanner being fenced and the new one starting, and *The scan is complete; no scanning node is needed*, with a *Scan complete* tag, once every batch has been assigned. `task nodes` and `task show` print matching notes.

## Database space

Agents release scan batches after the Master durably saves, acknowledges and authorizes their release. Completed scans also release their deduplication indexes in the background. Permanently deleted tasks also release their scan and execution records. Freed pages are reused; to shrink an existing database file, follow [database maintenance](database-maintenance.md).

## Compare objects

Under **Tools → Compare objects**, choose a single object or a bucket/prefix on each side. For prefix comparison, the source prefix is replaced with the target prefix: `images/a.jpg` under `images/` matches `backup/a.jpg` under `backup/`. An empty prefix covers the whole bucket. Results include objects missing from either side.

Each Agent runs one comparison at a time. The Master starts a comparison on an online Agent that is not running one, trying Agents of this version first, and the results stay on that Agent. From the CLI, use `object compare` ([CLI](cli.md#compare-stored-objects)).

**Metadata (does not read content)** sends a HEAD request for each object on both sides and compares the size, `Content-Type`, `Content-Encoding`, `Content-Disposition`, `Cache-Control` and custom metadata: the properties that transfers preserve. It does not read object contents, so matching metadata does not prove matching contents. Modification times and storage classes are not compared, and an ETag is not treated as a portable content hash: multipart uploads and different storage providers can produce different ETags for identical content. ETags serve only as a warning. When everything else matches and both ETags are plain MD5 ETags (32 hexadecimal digits, without the `-N` part count of a multipart upload) that differ, the result is *ETag differs* (`ETAG_MISMATCH`) instead of *Match*. The contents then probably differ, but not certainly: some services return MD5-like ETags that are not the MD5 of the contents, for example Amazon S3 for objects encrypted with SSE-KMS or SSE-C. Confirm such objects with a content comparison. When either ETag comes from a multipart upload, ETags are not compared.

**Content SHA-256 (streaming)** reads both objects completely, both sides at once, and compares their size and SHA-256; it does not compare metadata. Objects of different sizes are reported as different without being read. It does not create local object files, but it does transfer the full contents across the network and may incur storage read charges. Objects of 8 MiB or less, and objects on storage that does not serve byte ranges, are read with one request, tried up to three times (again after 1 and 2 seconds). Larger objects are read in 8 MiB ranges; a range that fails is read again up to three more times, after 1, 2 and 4 seconds. A read that receives no data for 60 seconds counts as failed. After reading, both objects are checked again with HEAD requests; an object that changed in the meantime is reported as *Changed during comparison*. Progress includes the bytes read on both sides, so a large object shows progress while it is being read.

Each compared object gets one of these results:

| Result | Console | Meaning |
| --- | --- | --- |
| `MATCH` | *Match* | Counted, not listed |
| `DIFFERENT` | *Different* | Both objects exist and differ. The row names what differs: `size`, `contentType`, `contentEncoding`, `contentDisposition`, `cacheControl` or `metadata` in metadata mode; `size` or `sha256` in content mode |
| `ETAG_MISMATCH` | *ETag differs* | Metadata mode only: everything compared matches, but the two plain MD5 ETags differ. Only a hint: single-part ETags that are not MD5s, for example of SSE-KMS or SSE-C objects, can be flagged too |
| `MISSING_TARGET` | *Missing from target* | Only on the source; a HEAD request confirmed that the target object is missing |
| `EXTRA_TARGET` | *Only on target* | Only on the target; a HEAD request confirmed that the source object is missing |
| `CHANGED_DURING_COMPARE` | *Changed during comparison* | The object changed, appeared or disappeared on either side while it was compared; compare it again later |
| `ERROR` | *Read failed* | One side could not be read; the row gives the cause ([errors and logs](#errors-and-logs)) |
| `BOTH_MISSING` | *Missing on both sides* | Single-object comparison: neither key exists |

Every result except `MATCH` is stored as a difference row. The counts cover every compared object; *differences* in the lists means every result except *Match*.

**Speed.** An Agent compares up to 32 objects at once in metadata mode and up to 8 in content mode, and lists each side in pages of 1,000 keys. Comparisons have no task limits, but they share the node's request and bandwidth limits with the transfers on that node ([node limits](#node-limits)), so a large comparison and busy transfers slow each other down. In metadata mode each object costs a HEAD request on each side, so the default HEAD limit of 2,800 requests/s allows up to about 1,400 objects per second; the latency of the storage service often sets a lower rate. In content mode the reads of both sides count against the node bandwidth limit, so each byte counts twice: at the default 2,000 Mbps, a comparison covers up to about 125 MB of objects per second.

### View results

The page lists the comparisons of all nodes, newest first, with their status, source and target, mode, objects checked, differences, start time and node. Choose one to show it; its ID stays in the page address (`?comparison=ID`). The list refreshes every 5 seconds while a comparison runs. A comparison on a node that cannot be reached shows *Node unavailable*, without counts, and a note says how many nodes could not be reached; while a node is unavailable, the list refreshes on its own every 15 seconds, so the comparison shows its status again once the node is back. Choosing such a comparison shows that its node is unavailable and tries again, waiting twice as long each time, up to 60 seconds. Comparisons that their node has deleted, and comparisons on nodes removed from the cluster, are no longer listed; opening one says that the comparison no longer exists. A node enrolled again with the same data directory keeps its comparisons. Comparisons that an Agent of this version reports but the Master does not know, for example after the Master's database was restored, are listed again and can be viewed, canceled and deleted as usual.

While a comparison runs, the page shows the objects checked, the rate in objects per second and the elapsed time, and in content mode the bytes read. The elapsed time counts only the time the comparison actually ran. The counts double as filters: *All differences* or a single result lists only those rows, while *Checked* and *Match* are plain counts. Each row shows the source and target keys, the result with the differing properties or the error and its detail, both sizes and, in content mode, both SHA-256 values. Rows are listed 100 per page with **Previous page** and **Next page**; while the comparison runs, the current page refreshes every 1.5 seconds without moving. When the selected result is rare in a long list, the page reads ahead several pages on its own; if it has still found nothing, **Keep searching** reads further.

**Cancel comparison** stops a running comparison; a canceled prefix comparison can be resumed later ([resume a comparison](#resume-a-comparison)). **Delete comparison results** deletes a comparison that is not running; cancel a running one first. If a comparison has been deleted, for example by the retention rules, its page says so and stops refreshing. If its result files are gone from the node, for example because they were removed by hand, the comparison keeps its counts and status, but its page lists no rows and shows *Comparison results are unavailable on this node*; it cannot be resumed.

The API: `GET /api/master/comparisons` lists the comparisons; when some nodes do not answer, `complete` is `false`, `unavailableNodes` names them and their comparisons have the status `UNAVAILABLE`. `GET /api/master/comparisons/{id}?after=0&limit=1000&status=DIFFERENT,MISSING_TARGET` returns the comparison and a page of rows: `limit` is 1–1,000 (default 50), and `status` is optional. Continue with `after` set to the returned `nextAfter`, which is absent on the last page. A comparison on an offline node answers HTTP 503 *Agent is offline* at once; a comparison that no longer exists, including one on a node removed from the cluster, answers HTTP 404 *Comparison not found*. The same applies to cancel, delete, export and resume, except that deleting a comparison of a removed node succeeds and only removes it from the list. A filtered page may hold fewer rows than `limit`, or none, and still have a `nextAfter`. Besides its settings, a comparison record holds the counts `compared`, `matched`, `different`, `etagMismatch`, `missing`, `extra`, `changed`, `errors` and `bothMissing`, the number of stored rows `rows`, `rowsTruncated`, `truncatedReason`, `lastStoredKey`, `checkpoint`, `resumable`, `bytesRead`, `elapsedMillis`, `error` and `errorDetail`.

### Export results

**Export ZIP** in the console, or `object export` in the CLI ([CLI](cli.md#compare-stored-objects)), downloads a ZIP file (`comparison-ID.zip` from the console) with three files:

| File | Contents |
| --- | --- |
| `differences.csv` | The stored rows in key order, with the columns `number`, `key`, `status`, `sourceKey`, `destinationKey`, `sourceSize`, `destinationSize`, `sourceSha256`, `destinationSha256`, `reasons` (separated by `;`), `error` and `errorDetail`. A status filter (`--status` in the CLI, `status` in the API) keeps only those results |
| `retry-source-keys.incr` | The source key of every `MISSING_TARGET`, `DIFFERENT` and `ETAG_MISMATCH` row, one per line, whatever the status filter |
| `summary.json` | The comparison (ID, node, status, mode, scope, source and target), its counts, `storedRows`, `exportedRows`, `statusFilter`, `complete` and `incompleteReasons`, `truncatedReason` and `lastStoredKey`, `retryStatuses`, `retryKeys`, `retryKeysOmitted`, `retryPrefixesMatch`, `csvEscaping`, and the start, end and export times |

`complete` is `true` only when the comparison has completed, its results are available on the node, it stored every difference and the file holds all stored rows (with a status filter, all stored rows with those results). Otherwise `incompleteReasons` names each reason: `NOT_COMPLETED:<status>`, `ROWS_TRUNCATED:<reason>`, `RESULTS_UNAVAILABLE` (the node no longer has the result files), `ROW_COUNT_MISMATCH` or `RETRY_KEYS_MISMATCH`. For results that hold every difference, the export is checked against the counts: a filtered export must hold as many rows as the counts of the selected results add up to (`DIFFERENT` counts as `different`, `ETAG_MISMATCH` as `etagMismatch`, `MISSING_TARGET` as `missing`, `EXTRA_TARGET` as `extra`, `CHANGED_DURING_COMPARE` as `changed`, `ERROR` as `errors`, `BOTH_MISSING` as `bothMissing`), otherwise `ROW_COUNT_MISMATCH`; and `retryKeys` plus `retryKeysOmitted` must equal `missing` + `different` + `etagMismatch`, otherwise `RETRY_KEYS_MISMATCH`. These count checks do not apply to results of [older Agents](#older-agents), which counted `BOTH_MISSING` as errors. A running comparison can be exported too: the file then holds the rows stored so far and is marked incomplete. The rows are read from the node page by page while the file downloads; if the node stops answering, the download fails instead of ending in a shortened file. The CLI also exports JSON Lines (`--format jsonl`): one stored row per line, as in the API, filtered like the CSV file and without a summary.

**CSV escaping.** The CSV file is UTF-8 with standard quoting. So that spreadsheet programs do not run a key as a formula, every value that begins with `=`, `+`, `-`, `@`, `'`, a tab, CR or LF gets a leading apostrophe (`'`). To recover the value, remove one leading apostrophe from each value that begins with one. Values that contain CR or LF are quoted, and their bytes are kept; some CSV readers, for example Go's `encoding/csv`, turn a CR LF inside a value into LF. `retry-source-keys.incr` and JSON Lines hold the keys unchanged; use the JSON Lines export (`object export --format jsonl`) when you need every key exactly.

**Transfer the listed objects again.** `retry-source-keys.incr` is a key list for `import-inventory` (see `oss-transfer-cli help tools`), which creates a task that copies each listed key from the source bucket to the same key in the target bucket. It therefore fits only when the comparison's source and target prefixes are equal (`retryPrefixesMatch` is `true`); otherwise the objects would be written under their source keys, so use a transfer task that replaces the prefix instead. Put the file in a directory of its own and add `--overwrite`: without it, the task skips objects that already exist on the target, which includes every `DIFFERENT` and `ETAG_MISMATCH` key. Remove the `ETAG_MISMATCH` keys first if a content comparison found those objects equal.

```sh
mkdir retry && unzip comparison-ID.zip retry-source-keys.incr -d retry
oss-transfer-cli import-inventory source/photos target/archive retry --overwrite
```

The file leaves out keys that `import-inventory` cannot read: longer than 1,024 bytes, not valid UTF-8, containing NUL, CR or LF, or consisting only of whitespace. `retryKeysOmitted` counts them; find them, exactly, in the JSON Lines export (`object export --format jsonl`). `import-inventory` accepts up to 10 million keys, or 1 GiB of keys, per file; split a longer list into several `.incr` files in the same directory, each of which becomes a task.

The API: `GET /api/master/comparisons/{id}/export?format=zip&status=DIFFERENT` with a bearer token; `format` is `zip` (default) or `jsonl`.

### Resume a comparison

A prefix comparison records a checkpoint about once a second: the key up to which every object has been compared and counted. When the Agent restarts or is updated during a comparison, the comparison continues from its checkpoint once the Agent is running again, provided the node's storage connections have not changed since the comparison was started or last resumed. Any change to any storage connection in the cluster counts, because the Master sends every connection to every node. Otherwise it stays *Comparison interrupted* with the detail `storage connections changed; resume manually`; resuming it manually enables automatic resume again.

**Resume comparison** in the console, or `object resume ID` in the CLI, continues an interrupted, failed or canceled prefix comparison, for example once storage access has been fixed; canceling therefore also works as a pause. The comparison runs again on the node that holds its results, which must be online and not running another comparison, with the storage connections as they are now: make sure they still point to the same buckets. Both sides are listed again after the checkpoint key. Counts and stored differences continue, so no object is counted twice or skipped, and the elapsed time adds up. Objects that were in progress at the interruption, such as a large object being read in content mode, are compared again from the start. A comparison that is still being canceled cannot be resumed (*Comparison cannot be resumed*); resume it once it has stopped. The API equivalent is `POST /api/master/comparisons/{id}/resume`.

A resumed comparison is no more a snapshot than the original: changes made behind the checkpoint while it was stopped are not seen. Single-object comparisons, comparisons that could not save their results, and comparisons on Agents of 4.1.40 or earlier cannot be resumed; start them again.

### Errors and logs

A row that could not be read shows *Read failed* with a fixed message and a detail that names the side, the request, the HTTP status, the storage error code and the request ID to give the storage provider, for example `target HEAD: HTTP 403 AccessDenied (request ID 1A2B3C)`. Details never contain credentials, signatures, URLs or host names.

A LIST request that fails with HTTP 5xx or 429, a throttling code such as `SlowDown`, a network error or a timeout (3 minutes per attempt) is retried up to 5 times, after 1, 2, 4, 8 and 16 seconds. Errors such as `AccessDenied` (HTTP 403), `NoSuchBucket` (HTTP 404) or `InvalidAccessKeyId` fail at once. Each HEAD request, retries included, is limited to 2 minutes.

When a listing still fails, or the storage lists keys out of order, the comparison fails. The page shows *Comparison could not finish; check storage access and retry.* with a detail such as `source LIST: HTTP 403 AccessDenied (request ID 1A2B3C)` or `source listing is not in ascending byte order after key "…"`, and the Agent writes `comparison <ID> failed: <detail>` to its service log (`/opt/oss-transfer/agent/data/logs/service.log` on Linux). Fix the cause, then resume the comparison: it continues after the last key it had compared. A comparison whose results the Agent could not write fails with *Cannot save comparison results* and cannot be resumed; start it again.

### Result storage and retention

Results stay on the Agent that ran the comparison, in files under `comparisons` in its data directory (`dataDirectory` in `service.json`; `/opt/oss-transfer/agent/data/comparisons` on Linux), one subdirectory per comparison. They are not stored in `agent.db`, so they do not grow the database, and database backups and compaction leave them out ([database maintenance](database-maintenance.md#comparison-results)). With keys of about 40 bytes, a stored difference takes about 260 bytes, so 2 GiB holds about 8 million differences; longer keys, and the SHA-256 values of content mode, take more.

Results are deleted automatically, but never those of the running comparison:

- 30 days after a comparison ended;
- while the comparisons on the node take more than 2 GiB together, the comparison that ended earliest first;
- while the node holds more than 500 comparisons, the comparison that ended earliest first.

These rules are applied when the Agent starts, whenever a comparison ends and every hour, so while a comparison runs, the total can exceed the budget until the next check deletes older results. **Delete comparison results** or `object delete` removes a comparison at once; its files are deleted in the background, also across restarts.

A comparison stops storing differences, but goes on comparing and counting, when its own differences would take more than the whole budget (`STORAGE_BUDGET`), or when writing them would leave less than 1 GiB free on the disk that holds the data directory (`LOW_DISK_SPACE`). Its page then shows the reason and the last stored key: the rows are complete up to that key, and later differences are only counted. Raise the budget or free disk space, then compare again, or compare the remaining keys under narrower prefixes. A third reason, `LEGACY_LIMIT`, marks results of [older Agents](#older-agents), which store at most 10,000 differences.

**Configuration file.** To change these limits, add a `comparison` object to the Agent's `service.json` and restart the Agent. Keys that are left out, or set to 0, keep their defaults:

| Key | Default | Effect |
| --- | --- | --- |
| `budgetBytes` | 2147483648 (2 GiB) | Total size of the stored differences on the node, and the most that one comparison stores; at least 67108864 (64 MiB) |
| `retentionDays` | 30 | Days a comparison is kept after it ended; at most 3,650 |
| `minimumFreeBytes` | 1073741824 (1 GiB) | Free space that storing differences leaves on the disk of the data directory |

For example, to keep up to 10 GiB of differences for 90 days, add the object to the existing JSON object and keep the other keys:

```json
"comparison": {
  "budgetBytes": 10737418240,
  "retentionDays": 90
}
```

A lower budget or retention deletes older results when the Agent starts. An unknown key or a value out of range keeps the Agent from starting, and the Master rejects a `comparison` object in its own `service.json`. Check the file with `--check-config` before restarting, as for [automatic reduction](#tune-automatic-reduction). The limit of 500 comparisons is fixed. Agents installed with Docker have no `service.json` and use the defaults.

### Older Agents

Agents of 4.1.40 or earlier keep their previous behavior until they are updated. They compare one object at a time, store only the first 10,000 differences of a comparison (the page then says that the node's version stores at most that many) and keep only their 10 most recent comparisons, deleting the oldest when another starts. They lack the ETag warning and error details, and on storage that reports listing times with milliseconds they mark almost every object *Changed during comparison*. A restart or update ends their running comparison for good. The Master lists, filters and exports the rows they stored, but an export cannot go beyond those 10,000 rows, and resuming is refused with *Update this node to resume comparisons*. New comparisons go to updated Agents first.

Update the Agents ([update installed services](#update-installed-services)). The comparisons that an Agent kept from before its update stay readable, count as 512 bytes per difference toward the budget and are deleted under the same rules.

### Limitations

- A comparison is not a point-in-time snapshot: each object is compared when its turn comes. Pause writes if you need a fixed dataset verified.
- Only S3-compatible object storage can be compared; local directories cannot.
- Listings must return keys in ascending byte order, as S3 requires. Storage that lists in another order fails the comparison with a detail that names the key after which the order broke.
- Keys are matched byte for byte after removing each prefix, without case folding, Unicode normalization or slash cleanup.

## Update installed services

Open **Tools → Software updates** to update the Master. Then open **Execution nodes → Software updates** and choose **Update all nodes** to bring eligible Agents to the Master's version. Each node must reconnect at the target version and finish its update before the next starts. A failed update stops the queue. The Master saves the queue, so closing the page or restarting the Master does not discard it. You can also update one node.

Wait for active tasks to finish when possible. If you upgrade with tasks running, update the Master first, then one Agent at a time. The native installer asks the Master to reserve that Agent’s assignments during the upgrade before stopping it. The reservation ends after verified startup or a successful rollback and expires after 30 minutes if the installer cannot finish. An older Master must be upgraded before the Agent installer can use this protection.

Online replacement supports managed native installations. The updater on the Master and on each Agent downloads the release itself, from GitHub or the JD Cloud OSS mirror, and verifies the archive before stopping the service; hosts without internet access upgrade with the [offline installation](offline-install.md#upgrade-and-reinstall) steps instead. The updater then runs the native installer in a separate process. The installer takes a temporary backup of the programs, configuration, service definition and stopped database. It deletes that backup after the new service passes its health check and confirms the installation. If startup fails, it attempts to restore the previous installation and keeps the recovery files. Agent transfers interrupted by the restart resume after it reconnects; for a comparison that was running, see [resume a comparison](#resume-a-comparison).

On Linux the update runs in a transient systemd unit named `oss-transfer-update-…`, which also works with systemd 219 (CentOS 7 / RHEL 7). Before systemd 236, a failed update leaves that unit listed as failed until you run `systemctl reset-failed`. Before systemd 240, systemd cannot report an update helper that fails to start; such an update is shown as failed after an hour without progress.

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
