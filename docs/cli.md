# Use the CLI

[中文](cli.zh-CN.md)

The command-line interface, `oss-transfer-cli`, manages the same tasks and settings as the web console. This page starts with connecting and a complete migration, then describes each command group. `oss-transfer-cli --help` lists every command, and `oss-transfer-cli help GROUP` (for example `help task create`, `help storage`) explains a group with examples.

## Connect to a Master

**On a Master or Agent (execution node) host.** The installer puts the CLI at `/usr/local/bin/oss-transfer-cli` (Windows: `bin\oss-transfer-cli.exe` in the installation). Run it as root or as the service owner: it uses the local cluster without a login or address.

```sh
sudo oss-transfer-cli task list
sudo oss-transfer-cli node list
```

Administrators of enrolled Agent hosts can manage the cluster through this local access. Other OS users sign in with a web-console account.

**In Docker Compose**, run it in the Master container: `docker compose exec master oss-transfer-cli task list`.

**From another computer**, [install the CLI](install.md#install-the-cli-on-another-computer), save the Master address and sign in:

```sh
oss-transfer-cli config set master http://10.0.0.5:1685
oss-transfer-cli login
oss-transfer-cli whoami
```

`login` signs in as `admin` unless you add `--username`, asks for the password without showing it, and prints `Signed in to http://10.0.0.5:1685 as admin`. `whoami` shows the Master, the user and the role. `logout` ends the session (`Signed out of …`). After the first login, change the initial password with `change-password`; a new password needs at least 8 characters, must not be a common or easily guessed password (such as `12345678`) and must differ from the user name and from the current password. Sessions belong to one Master address; another Master needs its own login.

`--host URL` selects a Master for one command. Otherwise the address comes from `OSS_TRANSFER_MASTER_SERVER`, the saved preference (`config set master`), then the local installation. `config get master` shows the address and its source in words, for example *Source: Detected from the local installation*. `config set master auto` returns to the local installation. A CLI with none of these uses `http://localhost:1685`; if nothing answers there, it says so and suggests `config set master URL` or `--host`.

## A migration from start to finish

This walk-through copies the prefix `2026/` of bucket `photos` on one object store to bucket `archive` on another, then checks the result. Run it on the Master host, or after `login`.

1. Check that Agents are online. Add one if needed: `agent invite` prints the join commands to run on the new host ([add and remove Agents](#add-and-remove-agents)).

   ```sh
   oss-transfer-cli node list
   ```

2. Add the two storage connections. Credentials come from environment variables or files, never from the command line. Saving checks the connection with an Agent ([storage connections](#storage-connections)).

   ```sh
   export SRC_AK=... SRC_SK=... DST_AK=... DST_SK=...
   oss-transfer-cli storage add source --endpoint https://s3.example.com --access-key-env SRC_AK --secret-key-env SRC_SK
   oss-transfer-cli storage add target --endpoint http://10.0.0.5:9000 --access-key-env DST_AK --secret-key-env DST_SK
   oss-transfer-cli storage test target --bucket archive
   ```

3. Create the task. `--conflict changed` makes later rounds overwrite only objects that changed ([conflict policies](operations.md#conflict-policies)):

   ```sh
   oss-transfer-cli task create oss-to-oss source/photos target/archive --prefix 2026/ --conflict changed
   ```

   It prints `Created 1 tasks: #12` and `Follow progress: oss-transfer-cli task show 12 --watch 5`. Creation only means the task was accepted.

4. Follow it until it is *Completed* (or *Partially failed*). Stop with Ctrl-C.

   ```sh
   oss-transfer-cli task show 12 --watch 5
   ```

5. If objects failed, read why and retry them ([exceptions and retries](#exceptions-and-retries)):

   ```sh
   oss-transfer-cli exceptions list failed --task-id 12
   oss-transfer-cli task retry 12
   ```

6. Save the report. It ends with *Report complete: Yes* when every object has a final result ([task reports](#task-reports)):

   ```sh
   oss-transfer-cli task report 12 --output task-12.zip
   ```

7. Compare source and target, then look at the differences ([compare stored objects](#compare-stored-objects)):

   ```sh
   oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --source-prefix 2026/ --dest-prefix 2026/
   oss-transfer-cli object status COMPARISON_ID
   oss-transfer-cli object export COMPARISON_ID --output comparison.zip
   ```

   A completed comparison without differences prints *No differences found*.

## Output, languages and exit codes

**Human output.** The CLI shows translated states, policies and reasons, numbers in full, readable sizes and local times with their UTC offset, such as `2026-11-10 08:30:00 +08:00`. Write commands print a short confirmation, such as `#12  Stop requested`. Errors read `<sentence> (HTTP n)` and come with a hint when there is one, for example `Hint: run oss-transfer-cli login to sign in.` or, when the Master advises path-style addressing, the `storage update NAME --path-style true` command.

**`--json`** prints the Master's structured result with the original codes (`RUNNING`, `OVERWRITE_CHANGED`, `SOURCE_CHANGED` and so on), unchanged by the language; use it in scripts. With `--watch`, it prints one JSON line per refresh.

**`--verbose`** writes the request paths and HTTP status codes to standard error, and the original error behind a translated storage message (`Details: …`). Without it, the CLI prints `Hint: use --verbose to see the original error.` when there is one.

**`--watch N`** refreshes a read-only command every N seconds (1–3600), for example `task list`, `task show`, `node list`, `limit list` or `update status`.

**`--version`** prints `OSS Transfer CLI v4.1.42` and a line with the commit, branch and build time. `--version --json` prints the same fields for scripts.

**Exit codes:**

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | The operation failed: the Master or the storage refused or failed it, or the session is missing or not allowed (HTTP 401 or 403) |
| 2 | Command-line error found before any request: unknown command or option, missing or invalid argument, missing `--yes`, an `--output` file that already exists |
| 3 | Incomplete result: a partial list (some nodes did not answer), an incomplete report or comparison export, or tasks of which only some were created |
| 130 | Interrupted (Ctrl-C) |

**Language.** The CLI follows the system language, with English as the fallback. Save a preference with `config set language en`, `zh`, `ja` or `auto`. `--lang` (or `OSS_TRANSFER_LANG`) overrides it for one command. JSON keys and codes are the same in every language.

## Storage connections

```sh
oss-transfer-cli storage list
oss-transfer-cli storage add NAME --endpoint URL --access-key-env NAME --secret-key-env NAME [options]
oss-transfer-cli storage update NAME [options]
oss-transfer-cli storage test NAME [--bucket BUCKET [--write]]
oss-transfer-cli storage buckets NAME [--bucket BUCKET]
oss-transfer-cli storage remove NAME --yes
```

| Option | Meaning |
| --- | --- |
| `--endpoint` | `http://` or `https://`, host and optional port, no path, for example `https://s3.example.com` or `http://10.0.0.5:9000` |
| `--region` | Signing region; `us-east-1` when omitted |
| `--access-key-env`, `--secret-key-env` (or `-file`) | Environment variable or file holding the key |
| `--session-token-env` (or `-file`), `--clear-session-token` | Temporary credentials |
| `--path-style auto\|true\|false` | Addressing. `auto` (default) uses path-style for endpoints with a port, an IP address, `localhost`, a name without a dot, or `.internal`, `.local`, `.lan`; otherwise virtual-hosted. `true` always uses path-style, `false` always virtual-hosted |
| `--v2-signer default\|true\|false` | Signature version: `default` is V4; `true` uses V2 for services that need it |
| `--validate true\|false` | Check the connection before saving; default `true` |

`storage list` shows each connection with masked credentials, its signature version and its addressing, for example *Automatic (Path-style)*. `storage test` lists the buckets, then lists one object in the bucket given with `--bucket` (or the first bucket) and prints *Connection test passed: buckets listed and one bucket reached with the current addressing*, the addressing, the bucket-level check and any warning. Keys limited to one bucket cannot list buckets: saving only warns, and `storage test NAME --bucket BUCKET` checks that bucket. With `--bucket`, it also checks each request a migration makes (list, HEAD, GET), and `--write` adds the write checks; it exits 1 when a required check fails ([check each permission of a bucket](planning.md#check-each-permission-of-a-bucket)). Failures print the cause and what to do; see [connection check failures](operations.md#a-storage-connection-check-fails). Changes are synchronized to every node; the command reports nodes that are still waiting.

## Inspect and control tasks

List all tasks, including those beyond the first page:

```sh
oss-transfer-cli task list --all
oss-transfer-cli task list --status PARTIAL_FAILED
```

Select an ID from the list:

```bash
read -r -p 'Task ID: ' TRANSFER_TASK
oss-transfer-cli task show "$TRANSFER_TASK"
oss-transfer-cli task children "$TRANSFER_TASK" --active
oss-transfer-cli task progress "$TRANSFER_TASK" --watch 5
```

`task show` prints the state, direction, conflict policy, counts, limits, nodes and, when something needs attention, an *Attention* line with the cause ([task details](operations.md#task-details-attention-and-failure-summary)). For common storage errors it adds a translated cause above the raw failure reason. A task under *Skip existing objects or files* that skipped objects gets a note that they were matched by name only.

Children use the console's status priority. When another page is available, the command prints the next-page command. `--order id` lists them by number. `task progress` shows in-flight file bytes; add `--child ID-N` to inspect one child task.

```sh
oss-transfer-cli task stop "$TRANSFER_TASK"
oss-transfer-cli task resume "$TRANSFER_TASK"
```

Each prints one line per task, such as `#12  Stop requested` or `No action needed (current state: …)`. Resume continues unfinished work and keeps completed progress. To retry failed tasks, pass their IDs to `task retry`, or use `task retry --all --yes` for every failed or partially failed task. If a request times out, the CLI reports the IDs whose result is unknown and stops sending further batches. Check those tasks before retrying. [Retry, resume and continue](operations.md#retry-resume-and-continue) compares the actions.

On a running task, `task retry <ID>` retries only the failed objects of its finished subtasks, without stopping the task. Those subtasks run again on their nodes within the node's concurrency limit, and succeeded or skipped objects are not transferred again. The output shows the retried subtasks and objects. Subtasks that are still running keep their failed objects until they finish, and `task retry --all --yes` still selects only failed or partially failed tasks. See [retry failed objects of a running task](operations.md#retry-failed-objects-of-a-running-task).

`task delete` moves records to the recycle bin. `task restore` restores them. `task purge ID --yes` starts permanent record cleanup; use `task cleanup-status ID --watch 5` to follow it. These operations do not delete migrated files.

## Create tasks and set limits

The CLI supports object storage to object storage (`oss-to-oss`), downloads (`oss-to-local`) and uploads (`local-to-oss`). `help task create` lists every option; [task options](operations.md#task-options) explains them.

```sh
oss-transfer-cli task create oss-to-oss source/photos target/archive --prefix 2026/ --mapping replace-prefix --target-prefix backup/2026/ --conflict changed
oss-transfer-cli task create oss-to-oss source/photos target/archive --node 10.0.0.2,10.0.0.3 --date-after 2026-11-01T00:00:00
oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download
oss-transfer-cli task create local-to-oss target/archive --binding 10.0.0.2=/data/upload --target-prefix upload/
```

- `--conflict skip|overwrite|changed` chooses the [conflict policy](operations.md#conflict-policies); the default is `skip`. `--overwrite` equals `--conflict overwrite` and cannot be combined with another `--conflict`. `changed` needs Agents of v4.1.42 or later.
- `--date-after` and `--date-before` take `YYYY-MM-DDTHH:mm:ss`, read in the Master's time zone, or RFC 3339 with an offset.
- `--distribution balanced|affinity` chooses how child tasks are placed.
- `import buckets|tasks|local FILE` creates many tasks from a file; see `help task import`.

The output is `Created N tasks: #ID` with the command to follow progress. If the Master refuses every row, nothing is created: the CLI prints `No task was created: …` and exits with 1 (`submissionStatus` is `rejected` in `--json`). If it refuses some rows, it names the created tasks, prints `N tasks not created: …` and exits with 3. When a connection fails before the result is known, the CLI prints a request ID: check `task list`, then retry with the same arguments and `--request-id ID`, so that no duplicate is created.

Local transfers run the same preflight checks as the console, including the [write probe for downloads](operations.md#a-download-precheck-fails). `--validate-only` checks a configuration without creating a task. `--bandwidth-mbps` (0.1–1,000,000 Mbps) and `--list-qps`, `--head-qps` and `--put-qps` set the task's limits when it is created; like the limits below, they are totals for all of the task's nodes.

Downloads to a local directory (`oss-to-local`) accept `--ranged-download auto|off`. The default, `auto`, has each Agent download objects of 128 MiB or more as concurrent 8 MiB byte ranges, four per object and up to 16 while the node has idle capacity, and write them to the file strictly in order. This is right for almost every task. Limits keep the ranges bounded: each object holds at most about one second's worth of the task's bandwidth share on that node (or of the node bandwidth limit, if that is lower) and, with a LIST / GET limit, of the task's GETs per second there, since every range is a GET. An object is downloaded as one stream when one second covers fewer than two ranges ([large objects](operations.md#large-objects)). `off` downloads each object as one sequential stream; choose it only when the target storage or network handles parallel range downloads badly (memory-constrained Agents, storage that throttles many concurrent GETs), or when support asks. The setting is fixed for the task's lifetime and is kept on resume, retry and node changes. `task show` displays it for local-destination tasks. Other directions reject the flag. In `task import local` JSON, the matching field is `"rangedDownload": "auto"` or `"off"`; omitting it means `auto`. Agents from before this option download every object as one stream, whatever the setting ([ranged download option](operations.md#ranged-download-option)).

```sh
oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download --ranged-download off
```

Adjust a running task's limits:

```sh
oss-transfer-cli task limits "$TRANSFER_TASK" --list-qps 100 --head-qps 500 --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli task limits "$TRANSFER_TASK" --put-qps unlimited
```

Unspecified fields keep their values, and `unlimited` removes a limit (0 also does for the QPS options); `task limits ID` or `task bandwidth ID` without new values shows the task's current limits: the bandwidth limit, the request limit scope, the LIST / GET, HEAD and PUT limits, whether every node has applied them, and the reason for each node that has not. `task bandwidth ID MBPS|unlimited` changes only the bandwidth limit. QPS values are average rates and may be fractional, from 0.000001 to 1,000,000,000: `--list-qps 0.5` allows one listing or GET every 2 seconds for the whole task. LIST limits also cover GET reads, range reads included; PUT includes multipart requests. A stopped or finished task's limits cannot be changed.

`task show ID` shows the current limits and their scope, and the live request rates next to the task and effective limits. The *Request rate*, *Task request limit* and *Effective request limit* lines are rounded to one decimal, so a limit below 0.05 shows as 0; the *LIST / GET QPS*, *HEAD QPS* and *PUT QPS* lines show exact values. `task nodes ID` shows each node's share. Task limits are totals: the Master divides them among the active Agents by demand, so the nodes that have work share almost all of the limit evenly while idle nodes keep a small floor ([task limits across nodes](operations.md#task-limits-across-nodes)). Node limits ([set node limits](#set-node-limits)) still apply: a request must pass both.

## Exceptions and retries

```sh
oss-transfer-cli exceptions list failed --task-id "$TRANSFER_TASK"
oss-transfer-cli exceptions list local --task-id "$TRANSFER_TASK"
oss-transfer-cli task retry "$TRANSFER_TASK"
oss-transfer-cli exceptions export failed --task-id "$TRANSFER_TASK" --output failures.csv
oss-transfer-cli exceptions clear failed --task-id "$TRANSFER_TASK" --yes
```

`exceptions list` prints a table: task, key, size, node, a short reason (for example access denied, DNS / path-style, connection refused, timeout, TLS, bucket missing, signature, throttling, source deleted or changed), the retry result (*Not retried*, *Retrying*, *Retry incomplete*, *Recovered*, *Failed again*, *Skipped*) and the local time. The footer gives the number of records and, for `local`, the command for the next page. Full error messages are in `--json` output and in the CSV of `exceptions export`, which also has the columns `reasonCode` and `retryReasonCode` (for example `SOURCE_CHANGED`, `SOURCE_DELETED`). The CLI lists resolved records too; the console shows unresolved ones by default.

Retry failed objects with `task retry`. Clearing removes only records resolved by a successful retry or skip (`Cleared N handled records`); unresolved failures remain. Use `local` for upload and download failures. `fallback` lists records of the legacy engine's fallback, which the current version no longer creates.

## Task reports

Export a stopped or finished task's report:

```sh
oss-transfer-cli task report "$TRANSFER_TASK" --output "task-$TRANSFER_TASK.zip"
```

Without `--output`, the file is `task-ID-report.zip` in the current directory; an existing file is never overwritten. After saving, the CLI prints where it saved the file, *Report complete* (yes or no), the object results (*N succeeded, N skipped, N failed, N not processed, N unknown*) and, when the report is incomplete, each reason as a sentence, naming the nodes that could not be read. An incomplete report exits with 3; the ZIP is kept. `--json` adds `complete`, `incompleteReasons` (the codes), `resultCounts` and `unreachableNodes`. The report's files and codes are described in [task reports](operations.md#task-reports).

`--output -` writes the ZIP to standard output, for example to take it out of a container: `docker compose exec master oss-transfer-cli task report 12 --output - > task-12.zip`. The completeness lines then go to standard error. It cannot be combined with `--json` (exit 2).

`exceptions export local --output FILE.zip` saves the report of all local transfers, with the same completeness check.

## Change a task's execution nodes

Show which Agents run a task, their state, the scanner, pending child tasks, each node's demand and current share of the task limits, removed nodes and the Agents that can be added. The demand column reads *On demand (busy)* or *Idle (released)*, with *adjusting* while a node still has to confirm a reduction. A node that does not hold a share yet, or no longer holds one (joining or disconnected), shows `—` in the demand and share columns. With an older Master, which divides limits equally, the demand column shows `—` for every node. Only a node that is scanning, or taking the scan over, is marked as the scanner. A note under the table says when the scan has finished, when it is moving to a new scanning node, or when the scanning node has left and another node will take over; a moved scan continues from the checkpoint saved on the Master. An offline node that is being removed is shown as leaving when its execution lease expires, or, for a task without lease fencing, after it reconnects. Members of a stopped task are shown as *Stopped*, and those of a finished task as *Finished*:

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK"
```

Add or remove Agents while the task exists. `--add` and `--remove` can be repeated or take comma-separated lists. Without `--yes`, the command prints the planned change (and the preflight result for local transfers) and submits nothing:

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK" --add 10.0.0.3 --yes
oss-transfer-cli task nodes "$TRANSFER_TASK" --remove 10.0.0.2 --yes
oss-transfer-cli task nodes "$TRANSFER_TASK" --remove 10.0.0.2 --now --yes
```

- By default a removed node drains: it receives no new work, its queued work moves to the other nodes, and it leaves once its running work finishes.
- `--now` revokes the node's running work, which continues on the remaining nodes. Some tasks do not support it; `task nodes` then shows the reason.
- A node being added shows `Joining` until it passes the handshake and shared-directory check; with task limits, it also waits until the other nodes have confirmed their reduced shares. Only then does it receive work.

An accepted change prints the resulting members. While nodes are still joining or draining, the change completes in the background; follow it with `task nodes "$TRANSFER_TASK" --watch 5`. The stages of a change, such as a leaving node waiting for its lease or the scan moving to another node, appear in this view rather than as task errors.

Uploads and downloads need one shared directory per added node. The CLI runs the Master's shared-directory preflight before it submits, and stops if any binding fails:

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK" --add 10.0.0.3 --binding 10.0.0.3=/mnt/share --yes
```

Only one change can be in progress. While nodes are joining or draining, the Master accepts only these changes: removing a joining node (cancels the join), adding a draining or immediately removed node back before it has left (cancels its removal; local transfers keep its path, so give no `--binding`), removing a draining node with `--now`, or removing a disconnected node whose work was taken over. It rejects anything else with a message. At least one node that can receive work must remain, and joining nodes do not count: add a replacement first, then remove the old node once it has joined. Each change carries the membership revision the CLI read. If someone changed the nodes in the meantime, the Master rejects the change; run `task nodes` again and resubmit. Stopped or failed tasks can be changed; the change applies on resume. Completed tasks and single-object or single-file tasks cannot be changed.

When a connection fails before the result is known, the CLI prints a request ID and membership revision. Check `task nodes` first. To retry, keep the same arguments and add the printed `--request-id` and `--expected-revision`; the Master returns the original result instead of applying the change twice. A retry with `--request-id` skips the local preflight, because the Master already checked the original request.

`task show` lists the node membership too. It marks joining and draining nodes, shows each node's demand and effective share, and adds the same scan notes. [Execution node changes](operations.md#change-a-tasks-execution-nodes) describe fencing, bandwidth redistribution and limits.

## Add and remove Agents

The `agent` and `node` command groups manage the same execution nodes. Use `agent invite` to add an Agent, `node list` and `node show` to inspect nodes, `agent update` to change a node's name, concurrency or local roots, and `agent remove` (or `node remove`) to remove one. `node add` and `node invite` also print join commands.

```sh
oss-transfer-cli node list
oss-transfer-cli agent invite --label storage-01
oss-transfer-cli agent invite-status INVITATION_ID
```

`agent invite` prints the join commands, with the expiry in local time (*The join credential works once and expires at …*; invitations last 30 minutes) and the download source:

- *Native installation (Linux / macOS / FreeBSD, as root or with sudo)*: `curl -fsSL '<link>' | sh -c 'if [ "$(id -u)" = 0 ]; then exec bash; else exec sudo bash; fi'`, which runs the installer directly as root and through `sudo` otherwise (an IPv6 Master address adds `--globoff`);
- *Native installation (Windows PowerShell)*;
- *Docker (macOS / Linux, no root needed)* and *Docker (Windows PowerShell)*: plain `docker run` commands that create the container `oss-transfer-agent-<6 characters>` and its volume;
- *Docker Compose (Linux, as root or with sudo)*: the installer's Compose method, in the same form.

Run the command for the new host there. Agents connect to the Master address in the command, which defaults to the address the CLI uses. If that is `localhost` or another loopback address, the CLI warns you: add `--master-url` with an address the Agent can reach, such as a LAN address, or `http://host.docker.internal:1685` for a Docker Agent on the Master's own host. `agent invite-status` shows whether the Agent has joined.

```sh
oss-transfer-cli agent update storage-01 --child-concurrency 32 --object-concurrency 4
oss-transfer-cli agent update storage-01 --local-root /mnt/data --local-root /mnt/archive
oss-transfer-cli agent remove storage-01 --yes
```

`--child-concurrency` (1–256, default 32) and `--object-concurrency` (objects per child task, 1–64, default 4) apply at once; their product may not exceed 1024 ([node concurrency](operations.md#node-concurrency)). `--local-root` replaces the list of directories the node may use for local transfers.

`agent remove` refuses while the node has active tasks. After removal it prints *Removed node NODE. Its Agent service stops by itself and does not restart; to delete its program and data, run the installer on that host with --uninstall --role agent* on standard error ([remove a node](operations.md#remove-a-node)).

## Set node limits

Node limits cap the LIST, PUT and HEAD requests per second and the bandwidth of one Agent, shared by all of its tasks ([node limits](operations.md#node-limits)):

```sh
oss-transfer-cli limit list
oss-transfer-cli limit set put 500 --all
oss-transfer-cli limit set bandwidth 200 --worker 10.0.0.2
oss-transfer-cli limit set head unlimited --worker 10.0.0.2
```

`limit list` shows a row per node and type (`list`, `put`, `head`, `bandwidth`): the target limit and its source (*Configured target* when it equals the value from `nodeControl` in the Agent's `service.json` or the built-in default, otherwise *Saved target*), the running limit, the measured rate and the status: *Normal*, *Reduced*, *Recovering*, *Recovered* or *Unlimited* ([limit status](operations.md#limit-status)). Values are printed exactly. A node that does not answer is left out, and the command exits with code 3. Add `--watch 5` to follow an automatic reduction.

`limit set TYPE VALUE` needs either `--worker NODE` or `--all`; `--all` sets the same value on each node. Request limits are in requests per second and may be fractional, from 0.01 to 1,000,000. Bandwidth is in Mbps unless `--unit bps|Kbps|Mbps|Gbps` says otherwise. The value must be greater than 0; `unlimited` removes the limit. The value becomes the node's saved target: it applies at once, ends an automatic reduction and survives restarts. If a limit has never been saved and runs at its configured value, setting that value stores nothing, so later `service.json` edits still apply to it. If an Agent does not confirm, the output lists it and the command exits with code 1.

```sh
oss-transfer-cli limit history --limit 100
oss-transfer-cli limit adjustment --worker 10.0.0.2 --event-id EVENT_ID
```

`limit history` lists the most recent manual and automatic adjustments across all nodes, newest first; `--limit` accepts 1–200 (default 50). `limit adjustment` shows one adjustment with the request that triggered it and every step of its round: pass the node and the `id` of a history entry, and `--cursor` with the printed `nextCursor` for the next 100 steps. Both print the raw fields, with times in milliseconds since 1970 and bandwidth in bit/s; use `--json` in scripts. See [storage throttling](operations.md#storage-throttling).

The V4 commands `rate-limit`, `list-rate-limit`, `rate-limit-history [N]` and `set-rate-limit TYPE VALUE [IP]` still work. `set-rate-limit` reads a bandwidth without a K, M or G suffix as bit/s, treats 0 as unlimited, and applies to every node when no IP is given.

## Compare stored objects

`object compare` starts a comparison of one object (`--scope object`) or of a bucket/prefix (`--scope prefix`) with its target; the Master runs it on an Agent. `--mode metadata` compares size and metadata through HEAD requests; `--mode content` streams both objects through SHA-256 without staging them on disk. [Comparison details](operations.md#compare-objects) describe the checks and results; `help object` lists the arguments.

```sh
oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --scope prefix --source-prefix 2026/ --dest-prefix 2026/ --mode metadata
oss-transfer-cli object list
```

`--source-bucket` and `--dest-bucket` take `CONNECTION/BUCKET`. Every `object compare` starts a comparison. With `--source-key` and `--dest-key` it compares a single object; with `--source-prefix` and `--dest-prefix` it compares a prefix, and with neither keys nor prefixes it compares the whole bucket. The mode defaults to `metadata`. A prefix comparison prints the new comparison's ID and the command to follow it. A single-object comparison waits up to 15 seconds and, if it finishes by then, prints the result as `object status` does; otherwise it prints the ID and the follow-up command. With `--json` it prints the final status page of a finished comparison, otherwise the comparison record. `object list` shows the comparisons of all nodes, newest first: ID, status, mode, scope, source, target, objects checked, differences, start time and node. If some nodes do not answer, it names them and exits with code 3.

Select an ID from the list:

```bash
read -r -p 'Comparison ID: ' TRANSFER_COMPARISON
oss-transfer-cli object status "$TRANSFER_COMPARISON"
oss-transfer-cli object status --status MISSING_TARGET,DIFFERENT --limit 1000 "$TRANSFER_COMPARISON"
oss-transfer-cli object export --output "comparison-$TRANSFER_COMPARISON.zip" "$TRANSFER_COMPARISON"
```

Put options before the ID. New comparison IDs never begin with `-`. IDs issued by earlier versions may; the CLI accepts such an ID directly when it is a complete ID (43 characters of letters, digits, `-` and `_`), and `--` before the ID always works, for example `object status --limit 100 -- "$TRANSFER_COMPARISON"`.

`object status` shows the comparison's status, mode, scope, source and target, every count, the stored differences, the elapsed time and rate, the bytes read and the read rate in bytes per second (content mode; a single object shows only the read rate), the checkpoint (not for single-object or completed comparisons), whether it can be resumed, why it stopped storing differences and the last stored key, and any error with its detail. A page of stored rows follows: number, result, source and target key, both sizes and an explanation (the differing properties, or the error and its detail). A completed comparison without differences prints *No differences found*. `--status` keeps only the given results, separated by commas: `DIFFERENT`, `ETAG_MISMATCH`, `MISSING_TARGET`, `EXTRA_TARGET`, `CHANGED_DURING_COMPARE`, `ERROR` and `BOTH_MISSING`. `--limit` sets the page size, 1–1,000 (default 50), and `--after N` starts after row N; when more rows follow, the command prints the `--after` value of the next page. If the comparison stopped storing differences, the command exits with code 3.

`object export ID --output FILE` saves a ZIP with `differences.csv`, `retry-source-keys.incr` and `summary.json` ([export results](operations.md#export-results)), or, with `--format jsonl`, one stored row per line. `--status` limits the exported rows as above; the retry key list always covers every `MISSING_TARGET`, `DIFFERENT` and `ETAG_MISMATCH` row. The file must not exist yet: the CLI downloads to a temporary file beside it and publishes it only once the download is complete and verified. After a ZIP, it prints the counts, whether the export is complete, with the reasons from `summary.json` such as `RESULTS_UNAVAILABLE` or `RETRY_KEYS_MISMATCH`, and how many retry keys it holds; when keys were left out of `retry-source-keys.incr`, it recommends `object export --format jsonl` for their exact values. An incomplete export exits with code 3. A JSON Lines export is checked the same way as a ZIP's `summary.json`: the CLI reports it incomplete when the comparison has not completed, did not store every difference, or, for results that hold every difference, when the exported rows do not add up to the counts of the selected results (`ROW_COUNT_MISMATCH`), for example because the node no longer has the result files ([export results](operations.md#export-results)).

```sh
oss-transfer-cli object resume "$TRANSFER_COMPARISON"
oss-transfer-cli object cancel "$TRANSFER_COMPARISON"
oss-transfer-cli object delete "$TRANSFER_COMPARISON"
```

`object resume` continues an interrupted, failed or canceled prefix comparison from its checkpoint ([resume a comparison](operations.md#resume-a-comparison)). It prints that the comparison was resumed only when it is running again; for a comparison that is already running, it says there is nothing to resume and prints the command to follow it. Otherwise it prints the current status, as `object cancel` does; a comparison that is still being canceled cannot be resumed until it has stopped. `object cancel` stops a running comparison; a canceled prefix comparison can be resumed later. `object delete` deletes the results of a comparison that is not running. For a comparison on an offline node, these commands and `object status` and `object export` fail at once with *Agent is offline*; a comparison on a node removed from the cluster is reported as not found, and `object delete` removes it from the list. A Master of 4.1.40 or earlier supports none of `object list`, `object export`, `object resume` and `--status`; the CLI then asks you to update the Master.

## Update the cluster

```sh
oss-transfer-cli update check
oss-transfer-cli update master --yes
oss-transfer-cli update agents --all --yes
oss-transfer-cli update status --watch 5
```

`update check` prints *Current version v4.1.41 → update available: v4.1.42*, the release notes link and the two commands to run, or that the current version is up to date. `update master --yes` checks for itself and installs the latest stable release; if the Master is already current, it says so and exits with 0. `update agents --all --yes` brings every eligible Agent to the Master's version, one at a time; `update agent --worker NODE --yes` updates one. Accepting the request does not mean the upgrade has finished: follow it with `update status --watch 5`, which shows versions, translated phases, whether each node matches the Master and, for nodes that cannot update online, why ([software updates](operations.md#software-updates)).

Online updates install only the latest stable release: `update master --version X` with another stable version is refused with a message; run the installer with `--version` on the Master host instead. To install a prerelease such as `4.1.43-beta.1`, give the exact version and confirm it explicitly:

```sh
oss-transfer-cli update master --version 4.1.43-beta.1 --allow-prerelease --yes
```

Without `--allow-prerelease`, a prerelease version is rejected before any request with exit code 2. The same applies when `update agent` or `update agents` is given a prerelease `--version`. Without `--version`, Agents follow the Master's current version, including a prerelease, without the flag. Missing `--yes` and other usage errors exit with 2. See [prerelease updates](operations.md#try-a-prerelease).

`backup list` shows upgrade backups. `backup prune master --yes` or `backup prune agent --worker NODE --yes` removes only backups the service marks as removable.

## Scripts

- Use `--json` and check the [exit code](#output-languages-and-exit-codes). Commands that change something require `--yes` when they remove or purge.
- Log in without a prompt with `--password-env NAME`, `--password-file FILE` or `--password-stdin`. `change-password` accepts `--old-password-env` and `--new-password-env`, or the corresponding file options. `OSS_TRANSFER_AUTH_TOKEN` supplies a login token.
- `OSS_TRANSFER_CLI_CONFIG_DIR` sets the directory for preferences and sessions (an absolute path). By default it is `~/.config/oss-transfer-cli` on Linux, `~/Library/Application Support/oss-transfer-cli` on macOS and `%APPDATA%\oss-transfer-cli` on Windows.
- Account login asks for the password without displaying it. Changing an account password requires that account's login; local service access does not change web-console passwords. `logout` closes an account session and does not remove the installed service's local access.
- Select task IDs explicitly, as `224,225` or `224-226`; commands never default to all tasks.
