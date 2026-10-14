# OSS Transfer

[中文](README.zh-CN.md)

OSS Transfer copies objects between S3-compatible buckets and transfers files between buckets and local directories. A Master hosts the console and schedules tasks. Agents run the transfers on the hosts where your data is accessible.

## Install the Master

Native installation supports Linux, macOS and Windows on amd64 and arm64. On Linux or macOS, download the installer and run it:

```sh
RELEASE_URL=https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest
curl -fSL "$RELEASE_URL/install.sh" -o install.sh
sudo bash install.sh --role master --method native --lang en
```

The installer asks for the listening port and registers a systemd service on Linux or a launchd service on macOS. For a macOS user service, run `bash install.sh --role master --method native --user` without `sudo`.

On Windows, open PowerShell as Administrator:

```powershell
$ReleaseUrl = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest'
Invoke-WebRequest "$ReleaseUrl/Install.ps1" -OutFile Install.ps1
.\Install.ps1 -Role master -ReleaseUrl $ReleaseUrl -Yes
```

Open `http://HOST:1685`, or the port you chose. Enter the setup token from the service log, create an administrator account, and set the Master URL to an address your browser and Agent hosts can reach.

The installer prints the log path. With the default Linux paths, read it using:

```sh
sudo cat /var/lib/oss-transfer-go/master/logs/service.log
```

Custom paths, service commands and offline installation are covered in the [native deployment guide](deploy/go/README.md). Platform archives are also available on [GitHub](https://github.com/MoonWX/oss-transfer/releases).

## Add Agents

A single-host installation needs an Agent alongside the Master. To add one now or expand the cluster later:

1. Open **Execution nodes → Add node** and create an invitation.
2. Copy the **Linux / macOS** or **Windows** command and run it on the execution host with administrator privileges.
3. Wait for the node to appear online before assigning tasks to it.

Agents connect to the Master URL and save their enrollment credentials locally. Keep Master and Agents on the same release.

For local transfers, enter the path on the Agent host, for example `/mnt/storage/import` or `D:\transfer\import`. Its service account must be able to read the source or write to the destination. Mount network storage on the host before using it in a task.

If several Agents handle one local-file task, mount the same shared filesystem at the same absolute path on each host. The shared-access check verifies this before dispatching work.

## Run a transfer

Add an S3 connection under **Storage connections**, then create a task. Select the source, destination and execution nodes. Use `SKIP` to retain existing destinations or `OVERWRITE` to replace them; set key mapping and bandwidth limits as needed.

Task details show progress, errors and individual child tasks. You can stop a task and resume it later. Resume keeps completed object results; interrupted objects are transferred again. Download the report after the task stops or finishes.

A task bandwidth limit is shared by its selected nodes. Node limits apply to all transfers on that node.

The console language menu supports English, Chinese and Japanese.

## Use the CLI

Log in with your console account. This Bash example reads the password without echoing it:

```bash
read -r -s -p 'Password: ' OSS_TRANSFER_PASSWORD; printf '\n'
export OSS_TRANSFER_PASSWORD
oss-transfer-cli --host http://HOST:1685 login --username admin --password-env OSS_TRANSFER_PASSWORD
unset OSS_TRANSFER_PASSWORD
```

Use the same Master address for task commands:

```sh
oss-transfer-cli --host http://HOST:1685 task list
oss-transfer-cli --host http://HOST:1685 task show 123
oss-transfer-cli --host http://HOST:1685 task stop 123
oss-transfer-cli --host http://HOST:1685 task resume 123
oss-transfer-cli --host http://HOST:1685 task report 123 --output task-123.zip
```

Run `oss-transfer-cli --help` for task creation, storage connections and node management. Add `--json` for scripts, or choose a language with `--lang en`, `--lang zh` or `--lang ja`.

## Upgrade and back up

Stop active tasks before upgrading. Download the latest installer and run it for each installed role, keeping any custom paths and service names:

```sh
RELEASE_URL=https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest
curl -fSL "$RELEASE_URL/install.sh" -o install.sh
sudo bash install.sh --role master --method native --release-url "$RELEASE_URL" --upgrade --yes
```

Use `--role agent` for an Agent. For a macOS user service, keep `--user` and run without `sudo`. On Windows, download `Install.ps1` again and run it with `-Role master -ReleaseUrl $ReleaseUrl -Upgrade -Yes`, changing the role for an Agent.

The installer backs up the stopped service and its database before replacement. If the new service fails its startup check, it restores that backup. For your own backups, stop the service and copy its configuration and data directories; use the paths printed by the installer.

V4.0.x installations need the [database migration procedure](docs/go-migration.md) before installing the current version.

## Docker Compose (optional)

On Linux, place [docker-compose.yml](docker-compose.yml) and [.env.example](.env.example) in a deployment directory. Create `.env`:

```sh
cp .env.example .env
```

Its default settings are:

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest
TRANSFER_PORT=1685
TRANSFER_LOCAL_ROOT=./transfer-files
```

Start the Master and read the setup token from its log:

```sh
docker compose pull master
docker compose up -d master
docker compose logs master
```

Complete account setup at `http://HOST:1685`. You can then add native Agents using the console, or run an Agent in the same Compose project.

For the Compose Agent, create the directory specified by `TRANSFER_LOCAL_ROOT` with access for container UID/GID `65532`. With the default setting:

```sh
sudo install -d -o 65532 -g 65532 -m 0750 ./transfer-files
```

Create an invitation in **Execution nodes → Add node** and copy its join URL. Enroll the Agent once, then start it:

```bash
read -r -s -p 'Agent invitation URL: ' JOIN_URL; printf '\n'
export OSS_TRANSFER_JOIN_TOKEN="${JOIN_URL##*/}"
docker compose --profile agent run --rm -e OSS_TRANSFER_JOIN_TOKEN agent \
  oss-transfer-agent --master-url http://master:1685 --data-dir /data --enroll-only
unset OSS_TRANSFER_JOIN_TOKEN JOIN_URL
docker compose --profile agent up -d agent
```

The host directory appears at `/files` inside the Agent. A host file `./transfer-files/example.txt` is therefore `/files/example.txt` in a task. Add bind mounts to the Agent service for other directories.

The `master-data` and `agent-data` volumes hold application state. For an upgrade, stop tasks and services, back up both volumes, then run:

```sh
docker compose pull master
docker compose up -d master
```

If you also run the Agent in Compose, update it with `docker compose --profile agent up -d agent`. Keep the existing volumes and mounts. Run the container CLI with `docker compose exec master oss-transfer-cli`.

## Build from source

Install the Go version in `go.mod`, Node.js 24 and Python 3, then run in Bash:

```sh
./build.sh --test
```

This tests and packages the current platform under `output/go-development/`. Add `--all` to build all supported platforms. [Release instructions](.github/RELEASING.md) describe publishing; the [test guide](.github/acceptance/README.md) lists CI coverage.
