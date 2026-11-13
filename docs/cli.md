# Use the CLI

[中文](cli.zh-CN.md)

The command-line tool `oss-transfer-cli` provides the same functionality as the web console: manage storage connections, create and track migration tasks, adjust rate limits and compare objects. Readers who prefer to work in the terminal or who need to write automation scripts will find everything on this page.

`oss-transfer-cli --help` lists every command. `oss-transfer-cli help GROUP` (for example `help task create`, `help storage`) shows the description and examples for that group.

## Connect to a Master

After you install a Master or an Agent (execution node), the CLI is already on the host. The installer places it at `/usr/local/bin/oss-transfer-cli` (Windows: `bin\oss-transfer-cli.exe` in the installation directory). When you run it as root or as the service owner, the CLI connects to the local cluster automatically, with no login and no address required:

```sh
sudo oss-transfer-cli task list
sudo oss-transfer-cli node list
```

Administrators on enrolled Agent hosts can manage the cluster in the same way. Other system users need to sign in with a web-console account.

In a Docker Compose deployment, run the CLI inside the Master container: `docker compose exec master oss-transfer-cli task list`.

To use the CLI from another computer, first [install the CLI](install.md#install-the-cli-on-another-computer), then save the Master address and sign in:

```sh
oss-transfer-cli config set master http://10.0.0.5:1685
oss-transfer-cli login
oss-transfer-cli whoami
```

`login` signs in as `admin` by default (add `--username` for a different account), and the password is not echoed. On success it prints `Signed in to http://10.0.0.5:1685 as admin`. `whoami` shows the current Master, user and role. `logout` ends the session (`Signed out of …`).

After the first login, change the initial password with `change-password`. The new password must be at least 8 characters, must not be a common or easily guessed password (such as `12345678`), and must differ from both the user name and the current password.

Each session is bound to one Master address. Connecting to another Master requires a separate login. `--host URL` selects a Master for a single command.

Without `--host`, the address is resolved in the following order: the environment variable `OSS_TRANSFER_MASTER_SERVER`, the saved setting (`config set master`), the local installation. `config get master` shows the current address and its source, for example *Source: Detected from the local installation*. `config set master auto` returns to the local installation. When none of these sources exists, the CLI falls back to `http://localhost:1685`. If it cannot connect, the CLI explains the reason and suggests running `config set master URL` or using `--host`.

## A complete migration

The following example copies the objects under the prefix `2026/` in bucket `photos` on the object store `source` to bucket `archive` on another object store `target`, then verifies the result. Run it on the Master host, or sign in with `login` first.

1. Confirm that Agents are online. To add a new node, `agent invite` prints the join command to run on the new host (see [add and remove Agents](#add-and-remove-agents)).

   ```sh
   oss-transfer-cli node list
   ```

2. Add two storage connections. Credentials come from environment variables or files, never from the command line. Saving the connection causes an Agent to check it (see [storage connections](#storage-connections)).

   ```sh
   export SRC_AK=... SRC_SK=... DST_AK=... DST_SK=...
   oss-transfer-cli storage add source --endpoint https://s3.example.com --access-key-env SRC_AK --secret-key-env SRC_SK
   oss-transfer-cli storage add target --endpoint http://10.0.0.5:9000 --access-key-env DST_AK --secret-key-env DST_SK
   oss-transfer-cli storage test target --bucket archive
   ```

3. Create the task. `--conflict changed` makes later incremental rounds overwrite only objects that have changed (see [conflict policies](operations.md#conflict-policies)):

   ```sh
   oss-transfer-cli task create oss-to-oss source/photos target/archive --prefix 2026/ --conflict changed
   ```

   The output reads `Created 1 tasks: #12` and `Watch progress: oss-transfer-cli task show 12 --watch 5`. A successful creation only means the task has been accepted.

4. Follow the task until the status becomes *Completed* (or *Partially failed*). Press Ctrl-C to stop watching.

   ```sh
   oss-transfer-cli task show 12 --watch 5
   ```

5. If objects failed, read why and retry them (see [exceptions and retries](#exceptions-and-retries)):

   ```sh
   oss-transfer-cli exceptions list failed --task-id 12
   oss-transfer-cli task retry 12
   ```

6. Save the report. When every object has a final result, the output ends with *Report complete: Yes* (see [task reports](#task-reports)):

   ```sh
   oss-transfer-cli task report 12 --output task-12.zip
   ```

7. Compare source and destination, then inspect the differences (see [compare stored objects](#compare-stored-objects)):

   ```sh
   oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --source-prefix 2026/ --dest-prefix 2026/
   oss-transfer-cli object status COMPARISON_ID
   oss-transfer-cli object export COMPARISON_ID --output comparison.zip
   ```

   A completed comparison without differences prints *No differences found*.

## Output, language and exit codes

The CLI translates states, policies and reasons for human-readable output, displays numbers in full with readable sizes, and shows times in the local time zone with a UTC offset, such as `2026-11-10 08:30:00 +08:00`. Write operations print a short confirmation, such as `#12  Stop requested`. Errors read `<description> (HTTP n)`, with a hint when one is available, for example `Hint: run oss-transfer-cli login to sign in.`. When the Master advises path-style addressing, it also gives the `storage update NAME --path-style true` command.

With `--json`, the output is the structured JSON returned by the Master. Field names and codes (`RUNNING`, `OVERWRITE_CHANGED`, `SOURCE_CHANGED` and so on) do not change with the language; use this form in scripts. Combined with `--watch`, each refresh prints one line of JSON.

`--verbose` writes request paths and HTTP status codes to standard error, and when a storage error has been translated, also shows the original error (`Details: …`). Without this option, the CLI prints `Hint: use --verbose to see the original error.` when there is one.

`--watch N` refreshes a read-only command every N seconds (1 to 3600), for example `task list`, `task show`, `node list`, `limit list` or `update status`.

`--version` prints `OSS Transfer CLI v<version>` followed by a line containing the commit, branch and build time. `--version --json` outputs the same information as fields, for use in scripts.

Exit codes:

| Exit code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | The operation failed: the Master or the storage refused or failed it, or the session is missing or not authorized (HTTP 401 or 403) |
| 2 | Command-line error found before any request: unknown command or option, missing or invalid argument, missing `--yes`, `--output` file already exists |
| 3 | Incomplete result: a partial list (some nodes did not respond), an incomplete report or comparison export, or only some tasks were created |
| 130 | Interrupted (Ctrl-C) |

The CLI follows the system language and falls back to English when it cannot be recognized. `config set language en`, `zh`, `ja` or `auto` saves a language preference. `--lang` (or `OSS_TRANSFER_LANG`) overrides it for a single command. JSON keys and codes are the same in every language.

## Storage connections

```sh
oss-transfer-cli storage list
oss-transfer-cli storage add NAME --endpoint URL --access-key-env VAR --secret-key-env VAR [options]
oss-transfer-cli storage update NAME [options]
oss-transfer-cli storage test NAME [--bucket BUCKET [--write]]
oss-transfer-cli storage buckets NAME [--bucket BUCKET]
oss-transfer-cli storage remove NAME --yes
```

| Option | Description |
| --- | --- |
| `--endpoint` | `http://` or `https://`, host and optional port, no path, for example `https://s3.example.com` or `http://10.0.0.5:9000` |
| `--region` | Signing region; `us-east-1` when omitted |
| `--access-key-env`, `--secret-key-env` (or `-file`) | Environment variable or file holding the key |
| `--session-token-env` (or `-file`), `--clear-session-token` | Temporary credentials |
| `--path-style auto\|true\|false` | Addressing mode. `auto` (default) uses path-style when the endpoint has a port, an IP address, `localhost`, a hostname without a dot, or a name ending in `.internal`, `.local` or `.lan`; otherwise it uses virtual-hosted. `true` always uses path-style; `false` always uses virtual-hosted |
| `--v2-signer default\|true\|false` | Signature version: `default` is V4; `true` uses V2 for services that require it |
| `--validate true\|false` | Check the connection before saving; default `true` |

`storage list` shows each connection (credentials are masked), its signature version and its addressing mode, for example *Automatic (Path-style)*.

`storage test` first lists the buckets, then lists one object in the bucket specified with `--bucket` (or the first bucket). When it passes, the output reads *Connection test passed: buckets listed and one bucket reached with the current addressing*, the addressing mode, the bucket-level result and any warnings. Keys that are only authorized for a single bucket cannot list buckets: saving only warns, and you can use `storage test NAME --bucket BUCKET` to check that bucket separately.

When `--bucket` is specified, the command also checks each request type used by a migration (list, HEAD, GET); adding `--write` also checks write operations. If any required check fails, the exit code is 1; see [check each permission of a bucket](planning.md#check-each-permission-of-a-bucket). Failures show the cause and what to do about them; see [connection check failures](operations.md#a-storage-connection-check-fails).

Changes to a connection are synchronized to every node. The command lists nodes that are still waiting for the synchronization.

## Inspect and control tasks

List all tasks, including those beyond the first page:

```sh
oss-transfer-cli task list --all
oss-transfer-cli task list --status PARTIAL_FAILED
```

Select an ID from the list and inspect it:

```bash
read -r -p 'Task ID: ' TRANSFER_TASK
oss-transfer-cli task show "$TRANSFER_TASK"
oss-transfer-cli task children "$TRANSFER_TASK" --active
oss-transfer-cli task progress "$TRANSFER_TASK" --watch 5
```

`task show` displays the status, direction, conflict policy, counts, rate limits and nodes. When something needs attention, it adds an *Attention* line with the reason (see [task details](operations.md#task-details-attention-and-failure-summary)). For common storage errors, a translated explanation appears above the raw failure reason. A task under *Skip existing objects or files* that skipped objects notes that those objects were matched by name only.

Child tasks are sorted by the console's status priority. When there is a next page, the command prints the command to fetch it. `--order id` lists them by number. `task progress` shows the bytes of files being transferred; add `--child ID-N` to inspect a specific child task.

Stop and resume:

```sh
oss-transfer-cli task stop "$TRANSFER_TASK"
oss-transfer-cli task resume "$TRANSFER_TASK"
```

Each command prints one line per task, such as `#12  Stop requested` or `No action needed (current state: …)`. Resuming continues unfinished work; completed progress is not lost.

To retry failed tasks, pass their IDs to `task retry`, or use `task retry --all --yes` to retry all failed and partially failed tasks. If a request times out, the CLI reports the task IDs whose result is unknown and stops sending further batches. Check those tasks before retrying. See [retry, resume and continue](operations.md#retry-resume-and-recover) for the differences between these operations.

For a running task, `task retry <ID>` retries only the failed objects in finished child tasks without stopping the task. Those child tasks are re-executed on their original nodes within the node's concurrency limit; objects that already succeeded or were skipped are not transferred again. The output shows the number of retried child tasks and objects. Child tasks that are still running keep their failed objects until they finish. `task retry --all --yes` still selects only failed and partially failed tasks. See [retry failed objects of a running task](operations.md#retry-failed-objects-of-a-running-task).

`task delete` moves records to the recycle bin. `task restore` restores them. `task purge ID --yes` starts permanent cleanup of records; use `task cleanup-status ID --watch 5` to follow the progress. These operations do not delete migrated files.

## Create tasks and set rate limits

The CLI supports three directions: between object stores (`oss-to-oss`), download to local (`oss-to-local`) and upload from local (`local-to-oss`). `help task create` lists every option; [task options](operations.md#task-options) explains their meaning.

```sh
oss-transfer-cli task create oss-to-oss source/photos target/archive --prefix 2026/ --mapping replace-prefix --target-prefix backup/2026/ --conflict changed
oss-transfer-cli task create oss-to-oss source/photos target/archive --node 10.0.0.2,10.0.0.3 --date-after 2026-11-01T00:00:00
oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download
oss-transfer-cli task create local-to-oss target/archive --binding 10.0.0.2=/data/upload --target-prefix upload/
```

Several commonly used options:

- `--conflict skip|overwrite|changed` chooses the [conflict policy](operations.md#conflict-policies); the default is `skip`. `--overwrite` is equivalent to `--conflict overwrite` and cannot be combined with another `--conflict` value. `changed` requires Agents of v4.1.42 or later.
- `--date-after` and `--date-before` accept `YYYY-MM-DDTHH:mm:ss` (interpreted in the Master's time zone) or RFC 3339 format with a time zone offset.
- `--distribution balanced|affinity` chooses how child tasks are distributed.
- `import buckets|tasks|local FILE` creates tasks in bulk from a file; see `help task import`.

On success the output reads `Created N tasks: #ID` with the command to follow progress. When the Master rejects every row, no task is created: the CLI prints `No task was created: …` and exits with 1 (`submissionStatus` is `rejected` in `--json`). When it rejects only some rows, the created tasks are listed, followed by `N tasks not created: …` and exit code 3.

When the connection drops before the result is returned, the CLI prints a request ID. Check `task list` first, then retry with the same arguments and `--request-id ID` to avoid creating duplicate tasks.

Local transfers run the same preflight checks as the console, including the [download write probe](operations.md#a-download-precheck-fails). `--validate-only` checks the configuration without creating a task. `--bandwidth-mbps` (0.1 to 1,000,000 Mbps) and `--list-qps`, `--head-qps`, `--put-qps` set rate limits when the task is created; they are totals across all of the task's nodes.

### Ranged download

Downloads to a local directory (`oss-to-local`) accept `--ranged-download auto|off`.

The default, `auto`, has each Agent download objects of 128 MiB or larger as concurrent 8 MiB byte ranges (4 concurrent ranges per object, up to 16 when the node has idle capacity), writing them to the file strictly in order. This setting is appropriate for almost every task.

When rate limits are set, each object holds at most about one second's worth of the task's bandwidth share on that node (or the node's bandwidth limit, if that is lower). With a LIST / GET limit in place, the ranges also do not exceed about one second's worth of the task's GET share on that node, because each range is a GET. When one second covers fewer than two ranges, the object is downloaded as a single stream (see [large objects](operations.md#large-objects)).

`off` downloads each object as a single sequential stream. Use it only when the target storage or network is not well suited for concurrent range downloads (memory-constrained Agents, storage that throttles many concurrent GETs), or when technical support asks.

This setting is fixed for the task's entire lifetime and does not change on resume, retry or node changes. `task show` displays it for tasks with a local destination. Other directions do not accept this parameter. In the JSON for `task import local`, the corresponding field is `"rangedDownload": "auto"` or `"off"`; omitting it defaults to `auto`. Agents that predate this option always download as a single stream regardless of the setting (see [ranged download option](operations.md#ranged-download-option)).

```sh
oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download --ranged-download off
```

### Adjust rate limits of a running task

```sh
oss-transfer-cli task limits "$TRANSFER_TASK" --list-qps 100 --head-qps 500 --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli task limits "$TRANSFER_TASK" --put-qps unlimited
```

Fields not specified keep their current values. `unlimited` removes the corresponding limit (for QPS options, 0 also works). Running `task limits ID` or `task bandwidth ID` without specifying new values shows the task's current limits: the bandwidth limit, the request limit scope, the LIST / GET, HEAD and PUT limits, whether all nodes have applied them, and the reason for each node that has not. `task bandwidth ID MBPS|unlimited` changes only the bandwidth limit.

QPS values are average rates and may be fractional, ranging from 0.000001 to 1,000,000,000. `--list-qps 0.5` means the entire task issues one list or GET request every 2 seconds. The LIST limit also applies to GET reads (including range reads); PUT includes multipart upload requests. The limits of a stopped or finished task cannot be changed.

`task show ID` displays the current limits and their scope, and shows the live request rates alongside the task limit and the effective limit. The *Request rate*, *Task request limit* and *Effective request limit* lines are rounded to one decimal place, so a limit below 0.05 shows as 0; the *LIST / GET QPS*, *HEAD QPS* and *PUT QPS* lines show exact values. `task nodes ID` shows each node's share.

Task limits are totals: the Master distributes them among the active Agents by demand. Nodes that have work share almost all of the limit evenly, while idle nodes keep a small floor (see [task limits across nodes](operations.md#task-limits-across-nodes)). Node limits (see [set node limits](#set-node-limits)) still apply; a request must satisfy both.

## Exceptions and retries

```sh
oss-transfer-cli exceptions list failed --task-id "$TRANSFER_TASK"
oss-transfer-cli exceptions list local --task-id "$TRANSFER_TASK"
oss-transfer-cli task retry "$TRANSFER_TASK"
oss-transfer-cli exceptions export failed --task-id "$TRANSFER_TASK" --output failures.csv
oss-transfer-cli exceptions clear failed --task-id "$TRANSFER_TASK" --yes
```

`exceptions list` displays a table: task, key, size, node, a short reason (for example access denied, DNS / path-style, connection refused, timeout, TLS, bucket missing, signature, throttling, source deleted or changed), the retry result (*Not retried*, *Retrying*, *Retry incomplete*, *Recovered*, *Failed again*, *Skipped*) and the local time. The footer shows the record count; for `local`, it also gives the command for the next page.

Full error messages are available in `--json` output and in the CSV exported by `exceptions export`. The CSV also has `reasonCode` and `retryReasonCode` columns (for example `SOURCE_CHANGED`, `SOURCE_DELETED`). The CLI lists resolved records as well; the console shows only unresolved records by default.

Retry failed objects with `task retry`. Clearing removes only records that have been resolved by a successful retry or skip (`Cleared N handled records`); unresolved failures remain. Use `local` for upload and download failures. `fallback` lists records of the legacy engine's fallback migrations, which the current version no longer produces.

## Task reports

Export the report of a stopped or finished task:

```sh
oss-transfer-cli task report "$TRANSFER_TASK" --output "task-$TRANSFER_TASK.zip"
```

Without `--output`, the file is `task-ID-report.zip` in the current directory; an existing file is never overwritten.

After saving, the CLI shows the save location, *Report complete* (yes or no), the object results (*N succeeded, N skipped, N failed, N not processed, N unknown*). When the report is incomplete, it also lists each reason with the nodes that could not be read. An incomplete report exits with 3; the ZIP is kept. `--json` adds `complete`, `incompleteReasons` (the reason codes), `resultCounts` and `unreachableNodes`. The files and codes in the report are described in [task reports](operations.md#task-reports).

`--output -` writes the ZIP to standard output, which is useful for retrieving the report from a container: `docker compose exec master oss-transfer-cli task report 12 --output - > task-12.zip`. In this case the completeness information goes to standard error. It cannot be combined with `--json` (exit code 2).

`exceptions export local --output FILE.zip` saves the report of all local transfers, with the same completeness check.

## Change a task's execution nodes

Show which Agents run a task, and which Agents can be added:

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK"
```

This command produces detailed output: the execution nodes and their states, the scanning node, pending child tasks, each node's demand and current share of the task's limits, removed nodes and the Agents available to add.

The demand column reads *On demand (busy)* or *Idle (released)*, with *adjusting* appended while a node has not yet confirmed a reduction. A node that does not yet hold a share or no longer holds one (joining or disconnected) shows `—` in the demand and share columns. With an older Master that divides limits equally, the demand column shows `—` for every node. Only a node that is scanning (or taking the scan over) is marked as the scanner.

A note under the table reports when the scan has finished, when it is moving to a new scanning node, or when the original scanning node has left and another node will take over. A moved scan continues from the checkpoint saved on the Master. An offline node that is being removed is shown as leaving when its execution lease expires; for a task without lease fencing, it leaves after it reconnects. Members of a stopped task are shown as *Stopped*, and those of a finished task as *Finished*.

Agents can be added or removed while the task exists. `--add` and `--remove` can be repeated, or accept comma-separated values. Without `--yes`, the command prints the planned change (and the preflight result for local transfers) without submitting it:

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK" --add 10.0.0.3 --yes
oss-transfer-cli task nodes "$TRANSFER_TASK" --remove 10.0.0.2 --yes
oss-transfer-cli task nodes "$TRANSFER_TASK" --remove 10.0.0.2 --now --yes
```

By default, removal drains the node: it receives no new work, its queued work moves to the other nodes, and it leaves once its running work finishes. `--now` revokes the node's running work, which continues on the remaining nodes. Some tasks do not support `--now`; `task nodes` shows the reason.

A node being added shows as *Joining* until it passes the handshake and shared-directory check. When the task has rate limits, the node also waits until the other nodes have confirmed their reduced shares before it begins receiving work.

After the change is accepted, the CLI prints the resulting membership. While nodes are still joining or draining, the change completes in the background; follow it with `task nodes "$TRANSFER_TASK" --watch 5`. The stages of a change (for example a leaving node waiting for its lease to expire, or the scan moving to another node) appear in this view and are not reported as task errors.

Upload and download tasks require a shared directory for each added node. The CLI runs the Master's shared-directory preflight before submitting, and stops if any binding fails:

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK" --add 10.0.0.3 --binding 10.0.0.3=/mnt/share --yes
```

Only one change can be in progress at a time. While nodes are joining or draining, the Master accepts only these changes: removing a joining node (cancels the join), adding back a draining or immediately removed node before it has left (cancels the removal; local transfers keep the existing path, so do not specify `--binding`), removing a draining node with `--now`, or removing a disconnected node whose work has been taken over. Other changes are rejected with a reason.

At least one node that can receive work must remain; joining nodes do not count. Therefore, add the replacement node first and remove the old node after the replacement has joined.

Each change carries the membership revision that the CLI read. If someone changed the nodes in the meantime, the Master rejects the change. Run `task nodes` again and resubmit. Stopped or failed tasks can be changed; the change takes effect on resume. Completed tasks and single-object or single-file tasks cannot be changed.

When a connection drops before the result is returned, the CLI prints a request ID and the membership revision. Check `task nodes` first. To retry, keep the same arguments and add the printed `--request-id` and `--expected-revision`. The Master returns the original result instead of applying the change twice. A retry with `--request-id` skips the local preflight, because the Master already checked the original request.

`task show` also lists the node membership. It marks joining and draining nodes, shows each node's demand and effective share, and adds the same scan notes. Fencing, bandwidth redistribution and limits are described in [change a task's execution nodes](operations.md#change-a-tasks-execution-nodes).

## Add and remove Agents

The `agent` and `node` command groups manage the same set of execution nodes. Use `agent invite` to add an Agent, `node list` and `node show` to inspect nodes, `agent update` to change a node's name, concurrency or local root directories, and `agent remove` (or `node remove`) to remove a node. `node add` and `node invite` also print join commands.

```sh
oss-transfer-cli node list
oss-transfer-cli agent invite --label storage-01
oss-transfer-cli agent invite-status INVITATION_ID
```

`agent invite` prints the join commands, shows the expiry in local time (*The join credential works once and expires at …*; invitations are valid for 30 minutes) and shows the download source:

- *Native installation (Linux / macOS / FreeBSD, root or sudo)*: `curl -fsSL '<link>' | bash` (an IPv6 Master address adds `--globoff`); when run by another user, the installer restarts itself through `sudo`;
- *Native installation (Windows PowerShell)*;
- *Docker (macOS / Linux, no root needed)* and *Docker (Windows PowerShell)*: plain `docker run` commands that create the container `oss-transfer-agent-<6 characters>` and its data volume;
- *Docker Compose (Linux, root or sudo)*: the installer's Compose method, in the same command form.

Run the corresponding command on the new host. The Agent connects to the Master address in the command, which defaults to the address the CLI is currently using. If that address is `localhost` or another loopback address, the CLI warns you: use `--master-url` to specify an address the Agent can reach, such as a LAN address. A Docker Agent on the same host as the Master can use `http://host.docker.internal:1685`. `agent invite-status` shows whether the Agent has joined.

```sh
oss-transfer-cli agent update storage-01 --child-concurrency 32 --object-concurrency 4
oss-transfer-cli agent update storage-01 --local-root /mnt/data --local-root /mnt/archive
oss-transfer-cli agent remove storage-01 --yes
```

`--child-concurrency` (1 to 256, default 32) and `--object-concurrency` (objects per child task, 1 to 64, default 4) take effect immediately; their product may not exceed 1024 (see [node concurrency](operations.md#node-concurrency)). `--local-root` replaces the list of directories the node may use for local transfers.

When the node still has active tasks, `agent remove` is refused. After removal, the command prints on standard error: *Removed node NODE. Its Agent service stops by itself and does not restart; to delete its program and data, run the installer on that host with --uninstall --role agent* (see [remove a node](operations.md#remove-a-node)).

## Set node limits

Node limits cap the LIST, PUT and HEAD requests per second and the bandwidth of a single Agent, shared by all of its tasks (see [node limits](operations.md#node-limits)).

```sh
oss-transfer-cli limit list
oss-transfer-cli limit set put 500 --all
oss-transfer-cli limit set bandwidth 200 --worker 10.0.0.2
oss-transfer-cli limit set head unlimited --worker 10.0.0.2
```

`limit list` shows one row per node and type (`list`, `put`, `head`, `bandwidth`): the target limit and its source (*Configured target* when it equals the value in `nodeControl` of the Agent's `service.json` or the built-in default, otherwise *Saved target*), the running limit, the measured rate and the status. The status is *Normal*, *Reduced*, *Recovering*, *Recovered* or *Unlimited* (see [limit status](operations.md#limit-status)). Values are printed exactly. A node that does not respond is omitted, and the command exits with code 3. Add `--watch 5` to follow an automatic reduction.

`limit set TYPE VALUE` requires either `--worker NODE` or `--all`. `--all` sets the same value on every node. Request limits are in requests per second and may be fractional, ranging from 0.01 to 1,000,000. Bandwidth defaults to Mbps; use `--unit bps|Kbps|Mbps|Gbps` to specify another unit. The value must be greater than 0; `unlimited` removes the limit.

The value becomes the node's saved target: it takes effect immediately, ends any automatic reduction and survives restarts. If a limit has never been saved and is running at its configured value, setting that same value stores nothing, so later edits to `service.json` still apply to it. When an Agent does not confirm the change, the output lists that node and the command exits with code 1.

```sh
oss-transfer-cli limit history --limit 100
oss-transfer-cli limit adjustment --worker 10.0.0.2 --event-id EVENT_ID
```

`limit history` lists the most recent manual and automatic adjustments across all nodes in reverse chronological order; `--limit` accepts 1 to 200 (default 50). `limit adjustment` shows a single adjustment, the request that triggered it and every step of that round: pass the node and the `id` from a history entry. To see the next 100 steps, add `--cursor` with the `nextCursor` from the output. Both commands print raw fields, with times in milliseconds since 1970 and bandwidth in bit/s; use `--json` in scripts. See also [storage throttling](operations.md#storage-throttling).

The V4 commands `rate-limit`, `list-rate-limit`, `rate-limit-history [N]` and `set-rate-limit TYPE VALUE [IP]` still work. `set-rate-limit` treats a bandwidth value without a K, M or G suffix as bit/s, treats 0 as unlimited, and applies to every node when no IP is given.

## Compare stored objects

`object compare` starts a comparison: use `--scope object` to compare a single object, or `--scope prefix` to compare a bucket or prefix. The comparison is executed by the Master on an Agent. `--mode metadata` compares size and metadata through HEAD requests; `--mode content` computes SHA-256 on both sides by streaming, without staging anything on disk. The checks and results are described in [object comparison](operations.md#compare-objects); see `help object` for the parameters.

```sh
oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --scope prefix --source-prefix 2026/ --dest-prefix 2026/ --mode metadata
oss-transfer-cli object list
```

`--source-bucket` and `--dest-bucket` use the `CONNECTION/BUCKET` format. Each `object compare` invocation starts a new comparison. Specifying `--source-key` and `--dest-key` compares a single object; specifying `--source-prefix` and `--dest-prefix` compares a prefix; specifying neither compares the entire bucket. The default mode is `metadata`.

A prefix comparison prints the new comparison's ID and the command to follow it. A single-object comparison waits up to 15 seconds; if it completes within that time, the result is printed in the same format as `object status`, otherwise it prints the ID and the follow-up command. With `--json`, a completed comparison outputs the final status page; otherwise it outputs the comparison record.

`object list` lists the comparisons across all nodes in reverse chronological order: ID, status, mode, scope, source, destination, objects checked, differences, start time and node. When some nodes do not respond, the output lists those nodes and the command exits with code 3.

Select an ID from the list to inspect and export:

```bash
read -r -p 'Comparison ID: ' TRANSFER_COMPARISON
oss-transfer-cli object status "$TRANSFER_COMPARISON"
oss-transfer-cli object status --status MISSING_TARGET,DIFFERENT --limit 1000 "$TRANSFER_COMPARISON"
oss-transfer-cli object export --output "comparison-$TRANSFER_COMPARISON.zip" "$TRANSFER_COMPARISON"
```

Options go before the ID. New comparison IDs never begin with `-`; IDs generated by older versions may. When such an ID is a complete ID (43 characters consisting of letters, digits, `-` and `_`), the CLI accepts it directly. Placing `--` before the ID always works, for example `object status --limit 100 -- "$TRANSFER_COMPARISON"`.

`object status` shows the comparison's status, mode, scope, source and destination, every count, the stored differences, the elapsed time and rate, the bytes read and the read rate in bytes per second (content mode; a single object shows only the read rate), the checkpoint (not shown for single-object or completed comparisons), whether the comparison can be resumed, why it stopped storing differences and the last stored key, and any error with its detail. Below that is a page of stored result rows: number, result, source key, destination key, both sizes and an explanation (the differing properties, or the error and its detail). A completed comparison without differences prints *No differences found*.

`--status` keeps only the specified results, separated by commas: `DIFFERENT`, `ETAG_MISMATCH`, `MISSING_TARGET`, `EXTRA_TARGET`, `CHANGED_DURING_COMPARE`, `ERROR` and `BOTH_MISSING`. `--limit` sets the page size, from 1 to 1,000 (default 50), and `--after N` starts after row N. When more result rows follow, the command prints the `--after` value for the next page. If the comparison stopped storing differences, the command exits with code 3.

`object export ID --output FILE` saves a ZIP containing `differences.csv`, `retry-source-keys.incr` and `summary.json` (see [export results](operations.md#export-results)). With `--format jsonl`, each line contains one result row instead. `--status` works the same as above and limits the exported rows; the retry key list always includes every `MISSING_TARGET`, `DIFFERENT` and `ETAG_MISMATCH` row.

The output file must not already exist: the CLI downloads to a temporary file in the same directory and publishes it under the specified name only after the download completes and passes verification. After a ZIP export, the command prints the counts, whether the export is complete (with the reason from `summary.json`, such as `RESULTS_UNAVAILABLE` or `RETRY_KEYS_MISMATCH`) and the number of retry keys. When keys were left out of `retry-source-keys.incr`, a hint recommends using `object export --format jsonl` to obtain their exact values. An incomplete export exits with code 3. A JSON Lines export is checked by the same rules as the ZIP's `summary.json`: the CLI reports the export as incomplete when the comparison has not completed, did not store every difference, or, for results that stored every difference, the exported row count does not match the sum of the selected result counts (`ROW_COUNT_MISMATCH`, for example because the result files no longer exist on the node) (see [export results](operations.md#export-results)).

```sh
oss-transfer-cli object resume "$TRANSFER_COMPARISON"
oss-transfer-cli object cancel "$TRANSFER_COMPARISON"
oss-transfer-cli object delete "$TRANSFER_COMPARISON"
```

`object resume` continues an interrupted, failed or canceled prefix comparison from its checkpoint (see [resume a comparison](operations.md#resume-a-comparison)). The command reports that the comparison was resumed only when it is actually running again. If the comparison is already running, the command states that there is nothing to resume and prints the command to follow it. In other cases, it prints the comparison's current status, as `object cancel` does. A comparison that is still being canceled must finish stopping before it can be resumed. `object cancel` stops a running comparison; a canceled prefix comparison can be resumed later. `object delete` deletes the results of a comparison that is not running.

When the node running the comparison is offline, these commands, along with `object status` and `object export`, fail immediately with *Agent is offline*. A comparison on a node that has been removed from the cluster is treated as not found, and `object delete` removes it from the list. A Master of 4.1.40 or earlier does not support `object list`, `object export`, `object resume` or `--status`; the CLI then advises upgrading the Master.

## Update the cluster

```sh
oss-transfer-cli update check
oss-transfer-cli update master --yes
oss-transfer-cli update agents --all --yes
oss-transfer-cli update status --watch 5
```

`update check` prints *Current version v… → update available: v…*, the release notes link and the two commands to run next, or states that the current version is already the latest stable release. `update master --yes` checks and installs the latest stable release on its own; if the Master is already current, it says so and exits with 0. `update agents --all --yes` updates all eligible Agents to the Master's version one at a time; `update agent --worker NODE --yes` updates a single one.

Accepting the request does not mean the upgrade has finished. Use `update status --watch 5` to follow the progress: it shows versions, translated phases, whether each node matches the Master, and the reason for any node that cannot be updated online (see [software updates](operations.md#software-updates)).

Online updates install only the latest stable release. `update master --version X` with another stable version is rejected with an explanation; run the installer with `--version` on the Master host instead. To install a prerelease (a version of the form `X.Y.Z-beta.N`), specify the exact version and confirm explicitly:

```sh
oss-transfer-cli update master --version <prerelease-version> --allow-prerelease --yes
```

Without `--allow-prerelease`, a prerelease version is rejected before any request is sent, with exit code 2. The same applies when `update agent` or `update agents` is given a prerelease `--version`. Without `--version`, Agents follow the Master's current version (including a prerelease) and do not need the flag. Missing `--yes` and other usage errors also exit with 2. See [prerelease updates](operations.md#try-a-prerelease).

`backup list` shows upgrade backups. `backup prune master --yes` or `backup prune agent --worker NODE --yes` deletes only backups that the service considers safe to remove.

## Scripts

When writing scripts, use `--json` together with the [exit codes](#output-language-and-exit-codes). Commands that delete or clean up require `--yes`. Task IDs must be specified explicitly, for example `224,225` or `224-226`; commands never default to all tasks.

To sign in without an interactive prompt, use `--password-env VAR`, `--password-file FILE` or `--password-stdin`. `change-password` accepts `--old-password-env` and `--new-password-env`, or the corresponding file options. `OSS_TRANSFER_AUTH_TOKEN` supplies a login token. The password entered during account login is not displayed. Changing an account password requires being signed in to that account; local service access does not change web-console passwords. `logout` closes the account session and does not remove the installed service's local access.

`OSS_TRANSFER_CLI_CONFIG_DIR` sets the directory for preferences and sessions (an absolute path). The default is `~/.config/oss-transfer-cli` on Linux, `~/Library/Application Support/oss-transfer-cli` on macOS and `%APPDATA%\oss-transfer-cli` on Windows.
