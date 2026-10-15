# Use the CLI

The command-line interface (CLI) connects to the same Master and uses the same account as the console. The examples below use Bash and a default Linux installation. On other systems, use the CLI path printed by the installer. With Docker Compose, run it through `docker compose exec master oss-transfer-cli`.

## Log in

The default Master address is `http://localhost:1685`. To connect elsewhere, set its address once in your shell:

```bash
read -r -p 'Master URL: ' OSS_TRANSFER_MASTER_SERVER
export OSS_TRANSFER_MASTER_SERVER
```

Use your console account. These Bash commands read the password without displaying it or putting it in command history:

```bash
read -r -p 'Username: ' TRANSFER_USER
read -r -s -p 'Password: ' TRANSFER_PASSWORD
printf '\n'
printf '%s' "$TRANSFER_PASSWORD" | oss-transfer-cli login --username "$TRANSFER_USER" --password-stdin
unset TRANSFER_PASSWORD TRANSFER_USER
```

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

## Language

The CLI follows the system language and supports English, Chinese and Japanese. It uses English for other locales. To save a different preference:

```sh
oss-transfer-cli config set language en
```

Use `zh` for Chinese, `ja` for Japanese, or `auto` to follow the system again.
