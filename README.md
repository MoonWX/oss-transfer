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

After downloading, the installer automatically chooses GitHub or the OSS mirror for its packages. For other systems or custom paths, see [installation options](docs/install.md). For hosts without internet access, use the [offline installation guide](docs/offline-install.md).

Open `http://<master-ip>:1685`. Sign in as `admin` with the generated password printed by the installer; the first login requires you to change it.

## Add an Agent

Open **Execution nodes → Add node**, select the host's operating system, and run the generated command on that host. Add at least one Agent before transferring data. The Master host can run an Agent too.

When adding a node, the Master address defaults to the current browser address. Change it to the Master's private URL if that Agent will connect over a private network.

## Run a transfer

1. Add your storage connections in the console.
2. Create a task, choose the source and destination, and select the Agents that will run it.
3. For local files, enter paths on the Agent hosts and pass the directory preflight check. The Agent service account needs read access for uploads and write access for downloads.
4. Start the task, follow its progress, and review failures or download its report when it finishes.

On an installed Master or Agent, the CLI uses the local cluster when run as the service owner or system administrator. No `--host` or login is needed for that access:

```sh
oss-transfer-cli task list
oss-transfer-cli node list
```

See [CLI usage](docs/cli.md) for task control, remote login and scripting.

## Update native installations

Open **Operations → Software updates** to update the Master. Then open **Execution nodes → Software updates** and choose **Update all nodes** to update the Agents. Updating the Master does not start Agent updates. Older versions without these pages need one installer upgrade first; you can also rerun the install command.

Successful upgrades delete their temporary backups. Make a separate backup before a planned downgrade; [backup and recovery](docs/operations.md) covers the files and database keys to keep.

## Docker Compose

With Docker Engine and the Compose plugin installed:

```sh
mkdir oss-transfer && cd oss-transfer
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml
docker compose up -d master
docker compose logs master
```

Open `http://<master-ip>:1685` and use the initial `admin` password from the log. Add Agents through **Execution nodes → Add node → Docker**. See [Docker Compose](docs/docker-compose.md) for Agent directory mounts and upgrades.

## Transfer behavior

- `SKIP` leaves an existing destination object or file in place; it does not compare its contents. For object-storage content verification, use [object comparison](docs/operations.md#compare-objects).
- Local paths belong to the Agent hosts. Several Agents can use different mount paths, but a shared-directory task must point them to the same shared directory and pass preflight.
- A checkpoint resumes the same scan. It does not provide continuous synchronization or guarantee detection of changes behind the saved position. Keep the source stable or use a snapshot for a fixed dataset.
