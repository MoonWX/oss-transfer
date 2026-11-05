# Use the CLI

[中文](cli.zh-CN.md)

The command-line interface (CLI) manages the same tasks and settings as the web console. On an installed Master or Agent, run it as the service owner or system administrator: it uses the local cluster without a login or address argument.

```sh
oss-transfer-cli task list
oss-transfer-cli node list
```

In Docker Compose, use `docker compose exec master oss-transfer-cli task list`. Administrators of enrolled Agent hosts can manage the cluster through this local access. Other OS users use `login` with a web-console account.

## Choose another Master

Use `--host` for a single command, or save a connection for later commands:

```bash
read -r -p 'Master URL: ' TRANSFER_MASTER
oss-transfer-cli config set master "$TRANSFER_MASTER"
oss-transfer-cli login
oss-transfer-cli task list
```

`config set master auto` returns to the installed cluster. `config get master` shows the current address and where it came from. Address selection follows `--host`, `OSS_TRANSFER_MASTER_SERVER`, the saved preference, then the installation. A standalone CLI without any of these uses `http://localhost:1685`. Sessions belong to one Master address; connecting to another Master requires its own login.

## Inspect and control tasks

List all tasks, including those beyond the first page:

```sh
oss-transfer-cli task list --all
```

Select an ID from the list:

```bash
read -r -p 'Task ID: ' TRANSFER_TASK
oss-transfer-cli task show "$TRANSFER_TASK"
oss-transfer-cli task children "$TRANSFER_TASK" --active
oss-transfer-cli task progress "$TRANSFER_TASK" --watch 5
```

Children use the console's status priority. When another page is available, the command prints the next-page command. `--order id` lists them by number. `task progress` shows in-flight file bytes; add `--child ID-N` to inspect one child task.

```sh
oss-transfer-cli task stop "$TRANSFER_TASK"
oss-transfer-cli task resume "$TRANSFER_TASK"
```

Resume continues unfinished work and keeps completed progress. To retry failed tasks, pass their IDs to `task retry`, or use `task retry --all --yes` for every failed or partially failed task. If a request times out, the CLI reports the IDs whose result is unknown and stops sending further batches. Check those tasks before retrying.

Export a stopped or finished task's report:

```sh
oss-transfer-cli task report "$TRANSFER_TASK" --output "task-$TRANSFER_TASK.zip"
```

`task delete` moves records to the recycle bin. `task restore` restores them. `task purge ID --yes` starts permanent record cleanup; use `task cleanup-status ID --watch 5` to follow it. These operations do not delete migrated files.

## Create tasks and set limits

The CLI supports object storage to object storage, uploads and downloads. Help lists each direction's required paths, storage connections and nodes:

```sh
oss-transfer-cli help task create
oss-transfer-cli help task import
oss-transfer-cli help storage
```

Local transfers run the same preflight checks as the console. `--validate-only` checks a configuration without creating a task.

Downloads to a local directory (`oss-to-local`) accept `--ranged-download auto|off`. The default, `auto`, has each Agent download objects of 128 MiB or more as concurrent byte ranges and write them to the file in order, using idle node capacity. This is right for almost every task. `off` downloads each object as one sequential stream; choose it only when the target storage or network handles parallel range downloads badly (memory-constrained Agents, storage that throttles many concurrent GETs), or when support asks. The setting is fixed for the task's lifetime and is kept on resume, retry and node changes. `task show` displays it for local-destination tasks. Other directions reject the flag. In `task import local` JSON, the matching field is `"rangedDownload": "auto"` or `"off"`.

```sh
oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download --ranged-download off
```

Adjust a running task's limits:

```sh
oss-transfer-cli task limits "$TRANSFER_TASK" --list-qps 100 --head-qps 500 --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli task limits "$TRANSFER_TASK" --put-qps unlimited
```

Unspecified fields keep their values. LIST limits also cover GET reads; PUT includes multipart requests. Task limits are shared across selected Agents, and node limits still apply.

## Change a task's execution nodes

Show which Agents run a task, their state, the scanner, pending child tasks, each node's share of the task limits, removed nodes and the Agents that can be added:

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
- A node being added shows `Joining` until it passes the handshake and shared-directory check. Only then does it receive work.

Uploads and downloads need one shared directory per added node. The CLI runs the Master's shared-directory preflight before it submits, and stops if any binding fails:

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK" --add 10.0.0.3 --binding 10.0.0.3=/mnt/share --yes
```

Only one change can be in progress. While nodes are joining or draining, the Master accepts only these changes: removing a joining node (cancels the join), adding a draining or immediately removed node back before it has left (cancels its removal; local transfers keep its path, so give no `--binding`), removing a draining node with `--now`, or removing a disconnected node whose work was taken over. It rejects anything else with a message. At least one node that can receive work must remain, and joining nodes do not count: add a replacement first, then remove the old node once it has joined. Each change carries the membership revision the CLI read. If someone changed the nodes in the meantime, the Master rejects the change; run `task nodes` again and resubmit. Stopped or failed tasks can be changed; the change applies on resume. Completed tasks and single-object or single-file tasks cannot be changed.

When a connection fails before the result is known, the CLI prints a request ID and membership revision. Check `task nodes` first. To retry, keep the same arguments and add the printed `--request-id` and `--expected-revision`; the Master returns the original result instead of applying the change twice. A retry with `--request-id` skips the local preflight, because the Master already checked the original request.

`task show` lists the node membership too. It marks joining and draining nodes and shows each node's effective share. [Execution node changes](operations.md#change-a-tasks-execution-nodes) describe fencing, bandwidth redistribution and limits.

## Manage nodes and exceptions

```sh
oss-transfer-cli node list
oss-transfer-cli agent invite --label storage-01
```

Run the printed join command on the new node. The invitation includes an ID for `agent invite-status`. If Agents need a private Master address, set `--master-url` when creating the invitation. `node update` supports live changes to `--child-concurrency` and `--object-concurrency`.

```sh
oss-transfer-cli exceptions list failed --task-id "$TRANSFER_TASK"
oss-transfer-cli task retry "$TRANSFER_TASK"
oss-transfer-cli exceptions clear failed --task-id "$TRANSFER_TASK" --yes
```

Clearing removes only records resolved by a successful retry or skip. Unresolved failures remain visible. Use `local` for local-transfer failures, or `fallback` to inspect fallback records.

## Compare stored objects

`object compare` checks one object with `--scope object`, or a bucket/prefix with `--scope prefix`. `--mode metadata` compares metadata; `--mode content` streams both objects through SHA-256 without staging them on disk. Use `object status`, `object cancel` and `object delete` with the returned comparison ID. [Comparison details](operations.md#compare-objects) describe the checks; `help object` lists the arguments.

## Update the cluster

```sh
oss-transfer-cli update check
oss-transfer-cli update status
oss-transfer-cli help update
```

Update a Master with `update master --version VERSION --yes`, then run `update agents --all --yes` to bring Agents to its version. To select one Agent, use `update agent --worker NODE --yes`. Follow the result with `update status --watch 5`; accepting the request does not mean the upgrade has finished. Online upgrades require a managed native installation.

`update check` only reports stable releases. To install a prerelease such as `4.1.29-beta.1`, give the exact version and confirm it explicitly:

```sh
oss-transfer-cli update master --version 4.1.29-beta.1 --allow-prerelease --yes
```

Without `--allow-prerelease`, a prerelease version is rejected before any request with exit code 2. The same applies when `update agent` or `update agents` is given a prerelease `--version`. Without `--version`, Agents follow the Master's current version, including a prerelease, without the flag. See [prerelease updates](operations.md#try-a-prerelease).

`backup list` shows upgrade backups. `backup prune master --yes` or `backup prune agent --worker NODE --yes` removes only backups the service marks as removable.

## Language and scripts

The CLI follows the system language, with English as the fallback. Save a preference with `config set language en`, `zh`, `ja` or `auto`. `--lang` overrides it for one command.

Use `--json` for structured output. Exit codes are 0 for success, 1 for a failed operation, 2 for invalid arguments, 3 for an incomplete or uncertain result, and 130 for interruption. Read-only commands support `--watch 5`.

Account login asks for the password without displaying it. Changing an account password requires that account's login; local service access does not change web-console passwords. `logout` closes an account session and does not remove the installed service's local access.

Scripts can log in with `--password-env`, `--password-file` or `--password-stdin`. `change-password` accepts `--old-password-env` and `--new-password-env`, or the corresponding file options.
