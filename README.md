# OSS Transfer

[中文](README.zh-CN.md)

Move objects between S3-compatible storage and local disks, spread across as many servers as you need.

## Quick start

Install the Master:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash
```

From mainland China:

```sh
curl -fsSL https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/install.sh | sudo bash
```

Open `http://<master-ip>:1685` and sign in as `admin` with the initial password printed by the installer. Choose your own password on first login.

The Master only schedules work. Data moves through Agents, so add at least one under **Execution nodes → Add node** and run the command it gives you on that host. The Master host can run an Agent too.

When adding a node, the Master address defaults to the current browser address. Change it to the Master's private URL if that Agent will connect over a private network.

Add a storage connection, create a task, and watch it run.

Run the install command again whenever you want to upgrade.

## Docs

- [Deployment](deploy/native/README.md): Windows, offline install, custom paths
- [Docker Compose](docs/docker-compose.md)
- [CLI](docs/cli.md)

To build from source, run `./build.sh`.
