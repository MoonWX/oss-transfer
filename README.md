# OSS Transfer

[中文](README.zh-CN.md)

OSS Transfer copies data between S3-compatible buckets and local directories, or between two buckets. A Master serves the WebUI and API. Agents connect to it to run transfers.

## Guided installation

Run the installer on the target Linux host. It supports `amd64` and `arm64`:

```sh
curl -fSL https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/install.sh -o install.sh &&
sudo bash install.sh --lang en
```

Choose Master or Agent, then Docker Compose or systemd. Fill in the settings and confirm to install. Debian and Ubuntu install missing dependencies automatically; on other distributions, install them first.

For an Agent, create an invitation under **Execution nodes → Add node** in the WebUI, then paste its URL into the installer on the new node.

To use an extracted local package, add `--method native --package-dir /path/to/oss-transfer`. The wizard handles new installations and stops if it finds an existing deployment. For updates, follow [Upgrade and back up](#upgrade-and-back-up).

## Deploy the master

### Docker

The image includes Java. Replace `http://master.example.com:1685` with an address your browser and Agents can reach:

```sh
docker run -d --name oss-transfer-master --restart unless-stopped \
  -p 1685:1685 \
  -e TRANSFER_MASTER_WEB_URL=http://master.example.com:1685 \
  -v oss-transfer-master-data:/app/data \
  moonwx/osstransfer:latest
```

`latest` follows stable releases. Use a tag such as `moonwx/osstransfer:4.0.5` to pin a version.

Open the configured URL and log in as `admin`. Read the generated password with:

```sh
docker exec oss-transfer-master cat /app/data/secrets/admin_password
```

Change the password after logging in. Settings and task checkpoints are stored in `/app/data`; keep this volume when replacing the container.

For local-file transfers, mount the directory with an option such as `-v /srv/transfer-files:/data`. The container runs as UID/GID `10001`, which needs read/write access to the directory. Use the container path (`/data` in this example) when creating a task.

### Docker Compose

Use Docker Compose v2 with `docker-compose.yml` and `.env.example` from the repository or [release archive](#traditional-deployment). Run these commands in the directory containing those files:

```sh
cp .env.example .env
```

In `.env`, set `TRANSFER_MASTER_WEB_URL` to the address browsers and Agents will use. Set `OSS_TRANSFER_FILES_DIR` to the host directory for local-file transfers. The default image is `moonwx/osstransfer:latest`.

Create the example directory and start the service:

```sh
sudo install -d -o 10001 -g 10001 -m 0750 /srv/transfer-files
docker compose pull
docker compose up -d
```

Open the configured URL and log in as `admin`. Read the password and view service logs with:

```sh
docker compose exec master cat /app/data/secrets/admin_password
docker compose logs -f master
```

To update, stop active tasks and run:

```sh
docker compose pull
docker compose up -d
```

The `master-data` volume holds settings and task checkpoints. `docker compose down` keeps it; `docker compose down -v` deletes it.

### Traditional deployment

Install Java 8 or later before running the installer. The host also needs Linux with systemd and the commands `curl`, `openssl`, `tar`, `sha256sum`, `timeout`, and `ss`.

Download and extract the latest stable release from JD Cloud OSS:

```sh
OSS_TRANSFER_RELEASE=latest
OSS_TRANSFER_DOWNLOAD_URL="https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/$OSS_TRANSFER_RELEASE"
curl -fSL "$OSS_TRANSFER_DOWNLOAD_URL/oss-transfer.tar.gz" -o oss-transfer.tar.gz &&
curl -fSL "$OSS_TRANSFER_DOWNLOAD_URL/oss-transfer.tar.gz.sha256" -o oss-transfer.tar.gz.sha256 &&
sha256sum -c oss-transfer.tar.gz.sha256 &&
tar -xzf oss-transfer.tar.gz
```

For a fixed version, set `OSS_TRANSFER_RELEASE=v4.0.5`. The archive and checksum are also available from [GitHub Releases](https://github.com/MoonWX/oss-transfer/releases).

Install the Master:

```sh
cd oss-transfer
sudo ./deploy/install-master.sh --web-url http://master.example.com:1685
```

The service is named `oss-transfer`. It reads `/etc/oss-transfer/application.yml` and stores data in `/var/lib/oss-transfer`. Read the initial password and check the service with:

```sh
sudo cat /etc/oss-transfer/admin-password
sudo systemctl status oss-transfer
sudo journalctl -u oss-transfer -f
```

Use `--port` to set the service port or `--java-bin` to select a Java executable. Run `./deploy/install-master.sh --help` for the full options.

## Add an agent

In the Master's **Execution nodes → Add node**, create a single-use invitation. Run the generated Docker or traditional installation command on the new node. The Agent joins the cluster and keeps its credential across restarts.

A Docker Agent uses the same image with `TRANSFER_ROLE=agent` and a persistent `/app/data` volume. Mount the local or shared directories it will access. Agents connect outbound to the Master, so their containers need no published ports.

For traditional deployment, install Java first. The command downloads a bundle and checksum from the Master, then installs the `oss-transfer-agent` service. Configuration is stored in `/etc/oss-transfer-agent/application.yml` and data in `/var/lib/oss-transfer-agent`.

Keep invitation URLs private. Use HTTPS when Agents connect to the Master over a public network. Removing a node revokes its credential; to rejoin, use a new invitation and a fresh Agent data directory or volume.

For local-file tasks across several Agents, mount the same shared filesystem at the same path on each node.

## Use the tool

1. Add source and destination connections in **Storage connections**.
2. Create a task. Select its direction, scope and execution nodes, then set conflict handling and limits as needed.
3. View progress in **Task details**. Click a child task to inspect it.
4. Stop and resume a task to continue from its recorded results. Reports can be exported after a task stops or finishes.

Node limits apply to all tasks on that node. Task limits apply to one task and remain subject to the node limits. When storage throttling triggers automatic adjustment, the node details show the triggering request and recovery status.

CLI examples:

```sh
oss-transfer-cli --help
oss-transfer-cli worker-status
oss-transfer-cli task list
oss-transfer-cli task show 123
oss-transfer-cli task children 123 --offset 25 --limit 25
oss-transfer-cli task show 123 --child 123-26 --node 10.0.0.2
oss-transfer-cli task trash --direction oss-to-local
```

Child tasks load in pages of 25. The WebUI loads the first page when you scroll to that section; the CLI prints a command for the next page. Add `--json` for structured output or `--host` to connect to another Master.

For Docker, prefix CLI commands with `docker exec oss-transfer-master`; for Compose, use `docker compose exec master`.

## Upgrade and back up

Stop active tasks before upgrading. Stop the service before copying its configuration and data directories, or take a consistent filesystem snapshot. Copying a live RocksDB directory can produce an unusable backup.

For Docker, pull the new version and recreate the container with the existing volume and storage mounts. For traditional deployment, extract the new release and run `sudo ./deploy/install-master.sh` on the Master or `sudo ./deploy/install-agent.sh` on an Agent. Pass the directory and service-name options used by the current deployment. These installers preserve configuration and data, including the Agent's enrollment credential.

## Build locally

Building requires JDK 8+, Maven, Node.js 22.12+ and Go 1.22+.

```sh
./build.sh
```

The build writes `output/oss-transfer.tar.gz`, its SHA-256 file, and the extracted package in `output/oss-transfer/`. The WebUI version display reads the Git commit and build time from the package metadata.

See the [maintainer release guide](https://github.com/MoonWX/oss-transfer/blob/main/.github/RELEASING.md) for publishing steps.
