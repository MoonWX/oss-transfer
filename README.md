# OSS Transfer

[中文](README.zh-CN.md)

OSS Transfer is a distributed data migration tool. It copies data between S3-compatible object stores, uploads local files to object storage, and downloads objects to local directories.

One Master serves the web console and API and schedules tasks. Agents (execution nodes) run on hosts close to the data and do the scanning and transfers. Everything can be done in a browser or from the command line.

![Transfer tasks page: one task running on two Agents, two completed](docs/images/overview.en.png)

## Features

- **Three directions:** object storage to object storage, local to object storage, and object storage to local, by bucket, prefix, single object or key list. Keep, add or replace key prefixes.
- **Several Agents per task:** work is balanced across Agents, and large objects are split into parts transferred in parallel ([large objects](docs/operations.md#large-objects)). Add or remove Agents while a task runs ([execution node changes](docs/operations.md#change-a-tasks-execution-nodes)).
- **Rate control:** limit bandwidth and LIST, GET, HEAD and PUT requests per task and per node, also while tasks run. Agents slow down by themselves when the storage service throttles ([task limits](docs/operations.md#task-limits-across-nodes), [storage throttling](docs/operations.md#storage-throttling)).
- **Conflict policies:** skip existing objects, overwrite them, or overwrite only objects that changed, for incremental rounds before a cut-over ([planning](docs/planning.md#incremental-migration-and-cut-over)).
- **Fault recovery:** unfinished work of a lost Agent moves to healthy Agents, scans resume from checkpoints, and failed objects can be retried. Each task has a downloadable report.
- **Object comparison:** compare buckets or prefixes by metadata or by SHA-256 of the contents, and export the differences with a key list for transferring them again ([object comparison](docs/operations.md#compare-objects)).
- **Operations:** online updates with automatic rollback, encrypted storage credentials, and a console and CLI in Chinese, English and Japanese.

## Architecture

```text
 Browser / oss-transfer-cli
           │ HTTP :1685
           ▼
   ┌────────────────┐   long poll / heartbeat   ┌───────────────┐
   │     Master     │ ◄──────────────────────── │    Agent A    │ ──► source / destination
   │ console · API  │ ◄──────────────────────── │    Agent B    │ ──► object storage or
   │ scheduling     │                           │      ...      │     local directories
   └────────────────┘                           └───────────────┘
```

Agents connect to the Master, so the Master never needs to reach them. Data flows only between Agents and storage, never through the Master.

## Before you start

Read [planning](docs/planning.md) before a real migration: where to run the Agents, which ports to open, which storage permissions the keys need, and how to plan the verification and the cut-over. [Security](docs/security.md) covers HTTPS, credentials and passwords.

## Quick start

There are two ways to install:

- **Native services** on Linux with systemd (CentOS 7 / RHEL 7 or later): the one-line installer below. [Installation options](docs/install.md) covers macOS, Windows, FreeBSD and custom paths, and [offline installation](docs/offline-install.md) hosts without internet access.
- **Docker:** the Master with Docker Compose and Agents as containers, on Linux or with Docker Desktop on macOS and Windows. Follow [Docker Compose](docs/docker-compose.md) for steps 1 to 3, then continue here at step 4.

### 1. Install the Master

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash
```

From mainland China, use the mirror:

```sh
curl -fsSL https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/install.sh | sudo bash
```

Choose Master and native deployment, and whether to install an Agent on the same host. Programs, configuration and data go under `/opt/oss-transfer/` by default. To pass options through the pipe, add `-s --`, for example a fixed version without questions:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --role master --with-agent --version 4.2.0 --yes
```

**When it worked,** the output ends like this:

```text
=== Master and local Agent installed successfully ===
Local Agent joined the cluster.
Web UI: http://10.0.0.5:1685
Initial administrator: admin
Initial password: <random password>
```

Without the local Agent, the first line is `=== master 4.2.0 · Installed successfully ===`.

**When it did not,** the installer prints the reason and the log path. Check `sudo systemctl status oss-transfer-master`, fix the cause and run the same command again: it repairs an incomplete installation.

### 2. Sign in

Open the **Web UI** address and sign in as `admin` with the initial password. Choose a new password: at least 8 characters, not a common or easily guessed password, different from the user name and from the initial password.

**When it worked,** the console opens. **Instructions** (and the empty overview) show a checklist: add execution nodes, add storage connections, create a transfer task, run it and view the results.

If the page does not load, open port 1685 in the host's firewall ([network](docs/planning.md#network-and-firewall)). If you lost the password, [reset it](docs/operations.md#reset-the-administrator-password).

### 3. Add Agents

Skip this step if you installed the local Agent and it is enough. Otherwise, open the **Execution nodes** page and choose **Add node** in the top right.

![Execution nodes page with the Add node button outlined in red](docs/images/add-node.en.png)

Enter a node name and the **Master address (reachable from the Agent)**: a private address works within the same network, while the browser can keep using a public address or a reverse proxy. Choose **Generate join commands**. The dialog offers commands for Linux / macOS / FreeBSD, Windows and Docker:

1. The join command for the selected system. Switch tabs first if the Agent host runs Windows or you want to use Docker.
2. Choose **Copy command** and run it on the Agent host.

<p align="center"><img src="docs/images/join-command.en.png" width="720" alt="Add node dialog with the join command and the Copy command button outlined in red"></p>

The native command runs `curl … | sudo bash`; on Windows, run its command in Administrator PowerShell. The dialog shows where the Agent downloads its package from: by default the Master itself, or the public sources after **Download from public sources**. The command works once and expires after 30 minutes.

**When it worked,** the installer prints `=== agent 4.2.0 · Installed successfully ===`, and the node appears as **Online** on the **Execution nodes** page (or in `oss-transfer-cli node list`).

**When it did not:** check that the Agent host can open the Master address (for example `curl http://10.0.0.5:1685/`). If the command has expired or was used, generate a new one.

### 4. Add storage connections

On the **Storage connections** page, choose **Add connection** for the source and for the target. Add an Agent first: the connection check runs on an Agent.

![Storage connections page with the Add connection button outlined in red](docs/images/storage.en.png)

| Field | What to enter |
| --- | --- |
| Connection name | A short name such as `source`; the CLI writes buckets as `source/bucket` |
| Endpoint | `http://` or `https://`, the host and an optional port, no path: `https://s3.example.com` or `http://10.0.0.5:9000` |
| Access Key, Secret Key | Keys with the [permissions](docs/planning.md#storage-permissions) for this migration |
| Addressing style | **Automatic (recommended)** |
| Signature version | **Default (Signature V4)**; choose V2 only for storage that requires it |
| Validate access when saving | Leave it on |

**Automatic addressing** uses path-style (`http://endpoint/bucket/key`) when the endpoint has a port, or its host is an IP address, `localhost`, a name without a dot, or ends with `.internal`, `.local` or `.lan`, as with most self-hosted S3 (MinIO, Ceph, SeaweedFS). Otherwise it uses virtual-hosted style (`https://bucket.endpoint/key`), as with AWS and other cloud domains. The form says which one applies. The console has no region field; if your storage needs a signing region, set it with `oss-transfer-cli storage update <name> --region <region>`.

**When it worked,** the connection saves without a warning and its card shows the addressing, such as **Automatic · Path-style**. **Test connection** reports that it listed objects in a bucket.

**When it did not,** the form shows the cause and an action, with the original error behind **Show original error**. When a bucket name does not resolve, choose **Use path-style**. Keys limited to one bucket save with a warning; check that bucket with **Test connection → Check a bucket** ([bucket-scoped keys](docs/planning.md#bucket-scoped-keys-and-the-connection-check)).

### 5. Create a transfer task

Choose **Create transfer** in the top left and fill in the numbered areas:

1. **Transfer direction:** object storage to object storage, object storage to local, or local to object storage.
2. **Source and destination:** select the storage connections and buckets. You can limit the transfer to a prefix or a single object and change the destination key names. For local files, enter paths on the Agent hosts and pass the directory precheck.
3. **Transfer options:** select the Agents, the conflict policy (**If the destination exists**: **Skip existing objects or files**, **Overwrite changed objects** or **Overwrite existing objects or files**) and the distribution mode. Below are date filters (in the server's time zone), **Ranged download** (downloads only) and task limits.
4. Choose **Create a transfer task**. The task scans first, then starts transferring.

![Create transfer page with the direction, source and destination, transfer options and create button outlined in order](docs/images/create.en.png)

For uploads, the Agent needs read access to the source directory; for downloads, write access to the destination directory. The precheck reports a destination that is not writable.

### 6. Follow progress

On the **Transfer tasks** page, choose a task to open its details on the right:

1. Stop or resume the task. **Adjust** (shown as **Set limit** when no bandwidth limit is set) changes the bandwidth and request limits while it runs.
2. **Transfer statistics:** scanned, transferred, skipped and failed objects with their sizes, updated live.
3. **Request rate:** actual LIST, GET, HEAD and PUT requests per second, with the task limits and the limits in effect.

<p align="center"><img src="docs/images/task-detail.en.png" width="440" alt="Task details with stop and limit controls, transfer statistics and request rates outlined in red"></p>

**When it worked,** the task ends as **Completed** with no failed objects. When objects fail, the details show the main cause; retry them from the **Exceptions** page. Objects already transferred are not copied again. **Export task report** downloads a ZIP with every object's result.

### 7. Verify the result

In the task details, choose **Compare source and target** (object storage to object storage tasks). It opens **Tools → Compare objects** with the task's buckets and prefixes filled in. Choose **Compare**.

**When it worked,** the comparison completes with **No differences found.** Otherwise, export the differences: the ZIP includes a key list for transferring the missing and different objects again ([object comparison](docs/operations.md#compare-objects), [verifying a large migration](docs/planning.md#verify-a-large-migration)).

## Docker Compose

The Master runs from `docker-compose.yml`, and the console's **Docker** join command starts Agents as containers without root, including on Docker Desktop for macOS and Windows:

```sh
mkdir oss-transfer && cd oss-transfer
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/.env.example -o .env
docker compose pull master
docker compose up -d master
docker compose logs master
```

The log shows the initial `admin` password. See [Docker Compose](docs/docker-compose.md) for Docker Desktop, how Agents in containers reach the Master (`host.docker.internal`), directory mounts, logs, the CLI in the container, updates and removal.

## Command line

On an installed Master or Agent host, run the CLI as root (or the service owner). It connects to the local cluster without a login:

```sh
oss-transfer-cli task list
oss-transfer-cli task progress <task-id> --watch 5
oss-transfer-cli task limits <task-id> --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli node list
```

In a Docker installation, run it in the Master container: `docker compose exec master oss-transfer-cli task list`. The CLI covers everything the console does and offers `--json` output for scripts. See [CLI usage](docs/cli.md) for remote login, task creation and exit codes.

## Updating

Update when no tasks are running, if possible.

1. Update the Master under **Tools → Software updates**, or with `oss-transfer-cli update master --yes`.
2. Under **Execution nodes → Software updates**, choose **Update all nodes**. The Agents update one at a time. Docker Agents are updated by recreating their containers; their **Update commands** button shows how ([Docker Compose](docs/docker-compose.md#container-agents)).

Each update takes a backup first and deletes it after the new version passes its health check. If startup fails, the previous version is restored. You can also update by rerunning the install command. Update checks only offer stable releases; see [trying a prerelease](docs/operations.md#try-a-prerelease) for betas.

To reinstall, keep each role's `config` directory and its entire `data` directory, including its key file (`master.db.key` or `agent.db.key`). To remove a native installation, run the installer with `--uninstall` ([installation options](docs/install.md)); for Docker, see [removing an installation](docs/docker-compose.md#remove-an-installation). See [operations](docs/operations.md) for details.

## Things to know

- **Skip existing objects or files compares names only.** An existing destination object or file with the same name is left in place. Use **Overwrite changed objects** for later rounds, and [object comparison](docs/operations.md#compare-objects) to verify.
- **Deletions are not copied.** Objects deleted at the source stay on the target; a comparison lists them as **Only on target** ([deletions at the source](docs/planning.md#deletions-at-the-source)).
- **Local paths belong to the Agent hosts.** When several nodes work on one local directory, it must be a shared directory. Mount paths may differ between nodes; the precheck writes a small file to confirm they point to the same directory.
- **A task copies a moving dataset once.** A scan does not see changes behind it. For a dataset that keeps changing, copy it in rounds and freeze writes for the last one ([cut-over](docs/planning.md#incremental-migration-and-cut-over)).

## Supported platforms

| OS | Architectures |
| --- | --- |
| Linux | amd64, arm64, armv7, riscv64, loong64 |
| macOS | amd64, arm64 |
| Windows | amd64, arm64 |
| FreeBSD | amd64, arm64 |

Native Linux services need systemd 219 or later; CentOS 7 / RHEL 7 (systemd 219, Bash 4.2, curl 7.29) is the oldest supported release. Online updates have two limits on older systemd. Below 236, such as on CentOS 7, a failed update leaves its `oss-transfer-update-*` helper unit listed as failed until you run `systemctl reset-failed`. Below 240, an update helper that cannot start is not reported at once; the update is marked failed only when it times out.

Each release is published to [GitHub Releases](https://github.com/MoonWX/oss-transfer/releases), Docker Hub as `moonwx/osstransfer`, and an OSS mirror in mainland China. GitHub Releases and the mirror both hold every file an offline installation needs (the archives, `manifest.tsv` and the three installers `install.sh`, `native-install.sh` and `Install.ps1`), plus `SHA256SUMS`. The mirror's `latest/` directory always holds the newest stable release.

## Documentation

| Document | Covers |
| --- | --- |
| [Planning](docs/planning.md) | Topology, ports and firewall, storage permissions, speed and sizing, verification, incremental migration and cut-over |
| [Security](docs/security.md) | Network exposure, HTTPS, storage credentials, passwords, sessions, join commands and service accounts |
| [Installation options](docs/install.md) | Other systems, specific or prerelease versions, paths, ports, reinstallation and reverse proxies |
| [Offline installation](docs/offline-install.md) | Installing without internet access |
| [Docker Compose](docs/docker-compose.md) | Container deployment on Linux and Docker Desktop, Agents in containers, mounts, logs, CLI, updates and removal |
| [CLI](docs/cli.md) | All command-line usage |
| [Operations](docs/operations.md) | Concurrency, node limits, task limits across nodes, storage throttling, large objects, comparison, updates, prereleases, failover, execution node changes, scan resume and password reset |
| [Database maintenance](docs/database-maintenance.md) | Reclaiming database file space |
| [Upgrading from V4.0.x](docs/migration.md) | Offline conversion of older data |

## Building from source

Requires Go 1.26, Node.js 22.12 or later, and Python 3.

```sh
./build.sh          # current OS and architecture
./build.sh --all    # every supported platform
./build.sh --test   # run the tests before packaging
```

Archives and checksums are written to `output/packages/`. For the CI tests and the release process, see [Acceptance tests](.github/acceptance/README.md) and [Releases](.github/RELEASING.md). The source is organized as follows:

| Directory | Contents |
| --- | --- |
| `cmd/` | Master and Agent entry points |
| `internal/` | Scheduling, transfer engine, storage adapters and Agent communication |
| `cli/` | `oss-transfer-cli` |
| `frontend/` | Web console (React and MUI) |
| `deploy/` | Installers, packaging and release |
