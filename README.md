# OSS Transfer

[中文](README.zh-CN.md)

Transfer data between S3-compatible object stores, or upload and download local files. A Master provides the console and schedules tasks; Agents scan and transfer the data.

## Install the Master

On Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash
```

From mainland China:

```sh
curl -fsSL https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/install.sh | sudo bash
```

Choose Master, native deployment, and whether to install an Agent on the same host. That Agent joins the Master automatically. The installer selects GitHub or the OSS mirror for package downloads. Programs, configuration and data go under `/opt/oss-transfer/` by default.

See [installation options](docs/install.md) for other systems and directories, or [offline installation](docs/offline-install.md) for an isolated network.

Open `http://<master-ip>:1685`. Sign in as `admin` with the generated password printed by the installer; the first login requires you to change it.

## Add an Agent

Open **Execution nodes → Add node**, name the node and enter a Master address it can reach. Generate the invitation, then copy the command for its operating system and run it on the Agent host. The Agent downloads its installer and package from the Master. A local Agent installed with the Master is already registered.

For hosts in the same private network, use the Master’s private address. The browser can use a different address or a reverse proxy. The invitation dialog also offers public package downloads.

## Run a transfer

1. Add your storage connections in the console.
2. Create a task, choose the source and destination, and select the Agents that will run it.
3. For local files, enter paths on the Agent hosts and pass the directory preflight check. The Agent service account needs read access for uploads and write access for downloads.
4. Create the task to start scanning and transferring. Follow its progress, retry failed files, or download the report when it finishes.

On an installed Master or Agent, the CLI uses the local cluster when run as the service owner or system administrator. No `--host` or login is needed for that access:

```sh
oss-transfer-cli task list
oss-transfer-cli node list
```

See [CLI usage](docs/cli.md) for task control, remote login and scripting.

## Update native installations

Wait for active tasks to finish when possible. Update the Master under **Operations → Software updates**, then choose **Execution nodes → Software updates → Update all nodes** to update the Agents one at a time. You can also rerun the install command.

To reinstall, keep each role’s configuration and entire data directory, including its `.db.key` file. The installer restores the programs and reuses the accounts, tasks and node identities. Existing installations keep their saved paths; see [reinstallation](docs/install.md#reinstall-without-losing-data) for custom locations.

## Docker Compose

With Docker Engine and the Compose plugin installed:

```sh
mkdir oss-transfer && cd oss-transfer
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml
docker compose up -d master
docker compose logs master
```

Open `http://<master-ip>:1685` and use the initial `admin` password from the log. Add Agents through **Execution nodes → Add node → Docker**. See [Docker Compose](docs/docker-compose.md) for Agent directory mounts and upgrades.

## Before you transfer or upgrade

- `SKIP` leaves an existing destination object or file in place; it does not compare its contents. For object-storage content verification, use [object comparison](docs/operations.md#compare-objects).
- Local paths belong to the Agent hosts. Shared-directory preflight writes a small test file: uploads need at least one node with write access to the source directory; downloads need write access on every selected node. The paths may differ, but they must point to the same shared directory.
- A checkpoint resumes the same scan. It does not provide continuous synchronization or guarantee detection of changes behind the saved position. Keep the source stable or use a snapshot for a fixed dataset.

Successful upgrades remove their temporary backups. Before a planned downgrade, save a separate copy of the configuration, data and matching programs; see [backup and recovery](docs/operations.md).
