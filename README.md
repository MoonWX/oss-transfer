# OSS Transfer

[中文](README.zh-CN.md)

OSS Transfer moves data between Amazon S3 (Simple Storage Service) compatible buckets, or between a bucket and a local directory. It is for operators running migrations on one or more servers, with progress tracking and recovery after interruptions.

## Start on Linux

Run this on a Linux host with systemd, Bash and curl:

```sh
(curl -fsSL --connect-timeout 3 --max-time 15 \
  https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh ||
 curl -fsSL --connect-timeout 3 --max-time 15 \
  https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/install.sh -o install.sh) &&
bash install.sh
```

Accept the defaults below, then confirm installation:

```text
Role: 1 Master / 2 Agent [1]:
Deployment: 1 Native (recommended) / 2 Docker Compose [1]:
Master port [1685]:
```

The installer automatically chooses the package for your system and CPU. Downloads use JD Cloud OSS for mainland Chinese public IPs and GitHub elsewhere, with fallback if the selected source cannot be reached.

Open the Master host’s IP address on port `1685` in your browser, or `http://localhost:1685` when installing locally. Enter the setup token from the service log, create your administrator account, and set the Master URL to an address reachable by your browser and execution hosts. The default Linux log is:

```sh
sudo cat /var/lib/oss-transfer-go/master/logs/service.log
```

The **Master** runs the console and schedules tasks. An **Agent** runs transfers on an execution host; add one even when everything runs on a single machine.

## Add an execution host

1. In **Execution nodes → Add node**, create an invitation.
2. Run the generated installation command on the host that will transfer data. For a single-machine deployment, run it on the Master host.
3. Wait for the node to show as online.

Use the same steps to add hosts later. Agents connect to the Master, so the Master URL must be reachable from each host. Keep Master and Agents on the same release.

## Create a migration

Add your endpoint and credentials under **Storage connections**. In **Create task**, choose bucket-to-bucket, download or upload, then select the source, destination and execution nodes.

For local transfers, paths refer to the Agent host. The Agent service needs permission to read the source or write the destination. If several Agents work on the same local directory, mount the same shared filesystem at the same absolute path on every selected host.

Use `SKIP` to keep existing destination objects or `OVERWRITE` to replace them. A task's bandwidth limit is shared across its selected nodes; node limits also apply. Task details show progress and failures, with stop and resume controls. Reports become available after a task stops or finishes.

For terminal access, see the [CLI guide](docs/cli.md). The console and CLI support English, Chinese and Japanese.

## Other installation methods

Prebuilt packages cover Linux, macOS, Windows and FreeBSD. See the [native deployment guide](deploy/go/README.md) for platform requirements, Windows and macOS commands, custom paths and offline installation.

[Docker Compose deployment](docs/docker-compose.md) is also available. Container Agents can only access host directories mounted into them; native Agents use host paths directly.

## Upgrade

Stop active tasks, then download and run the current installer:

```sh
(curl -fsSL --connect-timeout 3 --max-time 15 \
  https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh ||
 curl -fsSL --connect-timeout 3 --max-time 15 \
  https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/install.sh -o install.sh) &&
bash install.sh --upgrade
```

It finds installed services and reuses their paths and settings. It backs up the stopped services before replacement and restores them if startup checks fail. See the [deployment guide](deploy/go/README.md#upgrade) for Windows, macOS user services and backups. V4.0.x installations require [database migration](docs/go-migration.md) first.

## Before migrating live data

This is a migration tool, not continuous replication. Writes made to the source during a transfer are not guaranteed to reach the destination; plan a final pass with source writes stopped when consistency is required. Task recovery retains completed-object results, but an interrupted object may need to be transferred again. The transfer only covers the objects it reads, not the bucket's complete version history or storage-service configuration.

## Build and contribute

From a repository checkout, install the Go version in [go.mod](https://github.com/MoonWX/oss-transfer/blob/main/go.mod), Node.js 24 and Python 3, then run:

```sh
./build.sh --test
```

Packages are written to `output/go-development/`. See [CI coverage](.github/acceptance/README.md) and [release steps](.github/RELEASING.md). Report bugs through [GitHub Issues](https://github.com/MoonWX/oss-transfer/issues), including the version and relevant logs with credentials removed.

License: TODO.
