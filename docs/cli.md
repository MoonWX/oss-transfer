# Use the CLI

The command-line interface (CLI) connects to the same Master and uses the same account as the console. The examples below use Bash and a default Linux installation. On other systems, use the CLI path printed by the installer. With Docker Compose, run it through `docker compose exec master oss-transfer-cli`.

## Log in

The default Master address is `http://localhost:1685`. To connect elsewhere, set its address once in your shell:

```bash
read -r -p 'Master URL: ' OSS_TRANSFER_MASTER_SERVER
export OSS_TRANSFER_MASTER_SERVER
```

Log in as `admin` with the password printed by the installer. Password input is hidden:

```bash
oss-transfer-cli login
oss-transfer-cli change-password
```

The first login requires a password change before running other commands. For another account, use `login --username name`. Scripts can supply `--password-env`, `--password-file`, or `--password-stdin`; `change-password` accepts `--old-password-env` and `--new-password-env` (or the corresponding `-file` options).

The session is saved for this Master address. Keep using the same address and port for subsequent commands.

## Inspect and control tasks

```sh
oss-transfer-cli task list
```

Select a task ID from the list:

```bash
read -r -p 'Task ID: ' TRANSFER_TASK
oss-transfer-cli task show "$TRANSFER_TASK"
```

Stop that task:

```sh
oss-transfer-cli task stop "$TRANSFER_TASK"
```

Resume it when ready:

```sh
oss-transfer-cli task resume "$TRANSFER_TASK"
```

After the task stops or finishes, export its report:

```sh
oss-transfer-cli task report "$TRANSFER_TASK" --output "task-$TRANSFER_TASK.zip"
```

For task creation, storage connections and node management, inspect the corresponding command help:

```sh
oss-transfer-cli help task
oss-transfer-cli help storage
oss-transfer-cli help node
```

Add the global `--json` option for machine-readable output, for example `oss-transfer-cli --json task list`.

## Set task limits

Read the current limits or change individual fields while a task is running:

```sh
oss-transfer-cli task limits "$TRANSFER_TASK"
oss-transfer-cli task limits "$TRANSFER_TASK" --list-qps 100 --head-qps 500 --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli task limits "$TRANSFER_TASK" --put-qps unlimited
```

Unspecified fields keep their current values. `--list-qps` covers both object listing and GET reads; PUT includes multipart upload requests. New tasks share these limits across their selected Agents, and node limits still apply. The command prints the scope for older tasks that use per-node limits. These QPS flags also work with `task create`.

## Add an Agent

Generate the join commands:

```sh
oss-transfer-cli agent invite --label storage-01
```

Run the command for the Agent host's operating system or Docker. To use a private network address, add `--master-url` when creating the invitation.

## Language

The CLI follows the system language and supports English, Chinese and Japanese. It uses English for other locales. To save a different preference:

```sh
oss-transfer-cli config set language en
```

Use `zh` for Chinese, `ja` for Japanese, or `auto` to follow the system again.
