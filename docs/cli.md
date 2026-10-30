# Use the CLI

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

Adjust a running task's limits:

```sh
oss-transfer-cli task limits "$TRANSFER_TASK" --list-qps 100 --head-qps 500 --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli task limits "$TRANSFER_TASK" --put-qps unlimited
```

Unspecified fields keep their values. LIST limits also cover GET reads; PUT includes multipart requests. Task limits are shared across selected Agents, and node limits still apply.

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

`backup list` shows upgrade backups. `backup prune master --yes` or `backup prune agent --worker NODE --yes` removes only backups the service marks as removable.

## Language and scripts

The CLI follows the system language, with English as the fallback. Save a preference with `config set language en`, `zh`, `ja` or `auto`. `--lang` overrides it for one command.

Use `--json` for structured output. Exit codes are 0 for success, 1 for a failed operation, 2 for invalid arguments, 3 for an incomplete or uncertain result, and 130 for interruption. Read-only commands support `--watch 5`.

Account login asks for the password without displaying it. Changing an account password requires that account's login; local service access does not change web-console passwords. `logout` closes an account session and does not remove the installed service's local access.

Scripts can log in with `--password-env`, `--password-file` or `--password-stdin`. `change-password` accepts `--old-password-env` and `--new-password-env`, or the corresponding file options.
