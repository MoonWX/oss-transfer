# OSS Transfer

[中文](README.zh-CN.md)

OSS Transfer V4 migrates data between S3-compatible object storage and local filesystems. A master provides the WebUI and CLI API. Agents execute transfers and connect outbound to the master; they do not need an inbound service port.

Supported directions: object storage to object storage, object storage to local files, and local files to object storage. Tasks support node selection, bandwidth limits, stop/resume, and per-file reports.

## Deploy the master

### Docker

Docker includes Java. Linux `amd64` and `arm64` images are published as `moonwx/osstransfer`.

```sh
docker run -d --name oss-transfer-master --restart unless-stopped \
  -p 1685:1685 \
  -e TRANSFER_MASTER_WEB_URL=http://master.example.com:1685 \
  -v oss-transfer-master-data:/app/data \
  moonwx/osstransfer:latest
```

Replace the URL with an address that your browser and agents can reach. Open that URL and log in as `admin`. Read the generated password:

```sh
docker exec oss-transfer-master cat /app/data/secrets/admin_password
```

Change the password after logging in. Application data, storage connections, node credentials, and task checkpoints remain in `/app/data`. Keep the volume when replacing the container.

For local-file transfers, also mount the relevant directory, for example `-v /srv/transfer-files:/data`. The container runs as UID/GID `10001`; mounted directories must grant it the necessary read/write permissions. The task uses the path inside the container.

### Docker Compose

The repository and release archive include `docker-compose.yml` and `.env.example` at the root. Docker Compose v2 is required.

```sh
cp .env.example .env
```

Edit `TRANSFER_MASTER_WEB_URL` in `.env` to an address reachable by browsers and agents. Set `OSS_TRANSFER_FILES_DIR` to the host directory used for local-file transfers. For a new directory using the example path:

```sh
sudo install -d -o 10001 -g 10001 -m 0750 /srv/transfer-files
docker compose pull
docker compose up -d
```

Open the configured URL and log in as `admin`. Read the initial password, check logs, or run the CLI:

```sh
docker compose exec master cat /app/data/secrets/admin_password
docker compose logs -f master
docker compose exec master oss-transfer-cli worker-status
```

The default image is `moonwx/osstransfer:latest`. To use the JD Cloud mirror, set `OSS_TRANSFER_IMAGE=moonwx-cn-north-1.jcr.service.jdcloud.com/osstransfer:latest` in `.env`.

After stopping active tasks, update with:

```sh
docker compose pull
docker compose up -d
```

The `master-data` volume preserves configuration, credentials and task checkpoints. `docker compose down` keeps this volume; adding `-v` deletes it.

### Traditional deployment

Requires Linux with systemd, Java 8 or later, `curl`, `openssl`, `tar`, `timeout`, and `ss`. Java is not included in the archive. The installer checks Java before changing the service.

Download `oss-transfer.tar.gz` and its checksum from [Releases](https://github.com/MoonWX/oss-transfer/releases). Private repositories require GitHub authentication to download release files.

```sh
sha256sum -c oss-transfer.tar.gz.sha256
tar -xzf oss-transfer.tar.gz
cd oss-transfer
sudo ./deploy/install-master.sh --web-url http://master.example.com:1685
```

The service is `oss-transfer`. Configuration is `/etc/oss-transfer/application.yml`, the initial password is `/etc/oss-transfer/admin-password`, and data is `/var/lib/oss-transfer`. Existing configuration and data are preserved when the installer is run again.

```sh
sudo cat /etc/oss-transfer/admin-password
sudo systemctl status oss-transfer
sudo journalctl -u oss-transfer -f
```

Use `--port` to change the service port or `--java-bin` to select a Java executable. `./deploy/install-master.sh --help` lists installation options.

## Add an agent

Deploy the master first. In **Execution nodes**, choose **Add node**. The master creates a temporary, single-use invitation and shows Docker and traditional installation commands. Run the selected command on the new node. The agent joins automatically; no SSH access from the master is required.

The Docker command uses the same image with `TRANSFER_ROLE=agent`. It requires a persistent `/app/data` volume and no published ports. Mount any local or shared storage that the agent will access.

The traditional command downloads a checksum-verified bundle from the master and installs the `oss-transfer-agent` service. It uses `/etc/oss-transfer-agent/application.yml` and `/var/lib/oss-transfer-agent`. Install Java first.

Each agent receives its own credential and retains it across restarts. Do not copy the master's internal token to agents. Keep invitations private. Agents must be able to reach the master's URL; use HTTPS when traffic crosses an untrusted network.

Removing a node revokes its credential. To enroll it again, use a new invitation and a fresh agent data directory or volume.

For local-file tasks across several agents, mount the same shared filesystem at the same path. A node's local disk is not shared with other nodes.

## Use the tool

1. Add source and destination connections in **Storage connections**.
2. Create a task, select its direction and scope, choose execution nodes, and set conflict handling or limits as needed.
3. Review progress and child tasks in **Task details**. Stop a task before changing its execution or deleting it.
4. Resume stopped tasks to retain recorded completed work. Export the task report after it stops or finishes.

Node limits apply to all work on that node. Task limits apply to that task and are also constrained by node limits. Automatic adjustment can temporarily reduce request limits after storage throttling; the node details show the triggering request and recovery status.

The CLI uses the same master API:

```sh
oss-transfer-cli --help
oss-transfer-cli worker-status
oss-transfer-cli task list
oss-transfer-cli task show 123
oss-transfer-cli task children 123 --offset 25 --limit 25
oss-transfer-cli task show 123 --child 123-26 --node 10.0.0.2
oss-transfer-cli task trash --direction oss-to-local
```

Child tasks load in pages of 25. In the WebUI, scroll to the child-task section to load the first page; select a child to view its details. CLI output gives the command for the next page. Use `--json` for structured output.

For Docker, prefix commands with `docker exec oss-transfer-master`. Use `--host` to target another master. Keep credentials out of command history; use the login prompt or a protected configuration file.

## Upgrade and back up

Stop active tasks before an upgrade. Back up the configuration and data directories while the service is stopped, or use a consistent filesystem snapshot. Do not copy a live RocksDB directory as a backup.

For Docker, pull the new version and recreate the container with the same volume and storage mounts. For traditional deployment, extract the new release and run the same installer. Agent upgrades also retain their data and configuration; a new invitation is not needed for an already enrolled agent.

## Build locally

Building requires JDK 8+, Maven, Node.js 22.12+ and Go 1.22+.

```sh
./build.sh
```

Outputs are `output/oss-transfer.tar.gz`, its SHA-256 file, and `output/oss-transfer/`. Build metadata contains the Git commit and build time; the WebUI uses it for the version display.

[Maintainer release guide](https://github.com/MoonWX/oss-transfer/blob/main/.github/RELEASING.md).
