# Docker Compose

[中文](docker-compose.zh-CN.md)

This page describes how to run OSS Transfer with Docker: the Master runs in Docker Compose and Agents run as separate containers. Docker Engine on Linux and Docker Desktop on macOS and Windows are both supported. Docker Desktop does not require root. The image is `moonwx/osstransfer`.

If Agents need to access many host directories or the directories change frequently, [native installation](../README.md#1-install-the-master) is simpler, because containers can only access the directories you mount into them. Before a large migration, read [planning a migration](planning.md) and [security](security.md).

## Start the Master

### Linux

On a host with Docker Engine and the Compose plugin, run the following commands:

```sh
mkdir oss-transfer && cd oss-transfer
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/.env.example -o .env
docker compose pull master
docker compose up -d master
docker compose logs master
```

If your user does not have Docker permissions, add `sudo` before the `docker` commands.

### Docker Desktop on macOS and Windows

Install Docker Desktop and start it. No root or administrator rights are needed. On macOS, open Terminal and run the Linux commands above. On Windows, run in PowerShell:

```powershell
mkdir oss-transfer; cd oss-transfer
Invoke-WebRequest https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -OutFile docker-compose.yml
Invoke-WebRequest https://raw.githubusercontent.com/MoonWX/oss-transfer/main/.env.example -OutFile .env
docker compose pull master
docker compose up -d master
docker compose logs master
```

### Verify the startup

The log contains the following:

```text
Master listening on [::]:1685
Initial administrator: admin
Initial password: <random password>
```

Open port `1685` of this host in a browser. If you are on the same machine, the address is `http://localhost:1685`. Sign in as `admin` with the initial password from the log and set your own password. The password must be at least 8 characters, must not be a common or easily guessed password, and must not match the username. After the change, the log records that the initial password is no longer valid.

If the page does not open, run `docker compose ps` to verify that the `master` service is running and check `docker compose logs master` for errors. Another possibility is that port 1685 is occupied by another program; change the port in `.env`.

`.env` sets the image and the published port:

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest
TRANSFER_PORT=1685
```

`latest` always follows stable releases. To control when you upgrade, pin a version, for example `OSS_TRANSFER_IMAGE=moonwx/osstransfer:<version>`.

## Add an Agent

1. In the console, open **Execution nodes** and choose **Add node**.
2. Enter a node name and the **Master address (reachable from the Agent)**. Inside a container, `localhost` refers to the container itself, not the host, so the dialog warns when the address is a loopback address. When the Agent and the Master are on the same machine (common with Docker Desktop), choose **host.docker.internal (Docker on the Master's host)** and the address becomes `http://host.docker.internal:1685`. When the Agent is on another host, enter an address of the Master that the Agent host can reach, such as a private IP. A natively installed Master also lists its LAN addresses as buttons; a Master in a container cannot obtain these addresses.
3. Choose **Generate join commands**, open the **Docker** tab and select the variant:

   | Variant | Applicable environment | Description |
   | --- | --- | --- |
   | **macOS / Linux** | Docker Desktop on macOS, Docker Engine on Linux | Plain `docker run` commands; no root needed (on Linux, your user must be able to run `docker`) |
   | **Windows PowerShell** | Docker Desktop on Windows | The same commands in PowerShell syntax |
   | **Linux Docker Compose** | Linux with root | The Compose installer (`curl … \| bash`, run as root or with `sudo`); see [below](#the-linux-compose-installer) |

4. Choose **Copy command** and run it on the Agent host within 30 minutes. The command can be used only once.

<p align="center"><img src="images/docker-join.en.png" width="720" alt="Docker tab of the Add node dialog, with the environment selector and the Docker command outlined"></p>

The `docker run` variant contains two commands. The first joins the cluster in a temporary container and writes the Agent's identity into a Docker volume. Only when the first command succeeds does the second start the Agent container.

The container is named `oss-transfer-agent-<id>` and the volume is `oss-transfer-agent-<id>-data`, where `<id>` is the first 6 characters of the invitation ID, so multiple Agents on the same host do not collide. The container runs in read-only mode as UID 65532, with no capabilities, and restarts automatically on exit (`--restart unless-stopped`). When the address is `host.docker.internal`, the command also includes `--add-host host.docker.internal:host-gateway` so that the name resolves on Linux as well.

After the command completes, `docker ps` shows `oss-transfer-agent-<id>` and the node appears as **Online** on the **Execution nodes** page.

If it did not succeed, consult the following table:

| Message | Cause and action |
| --- | --- |
| Connection refused, or the Master cannot be reached | The Master address is wrong or blocked by a firewall. Start the Master, correct the address or firewall rules, and run the same command again. A failed join does not create an Agent container. |
| HTTP 403, invitation invalid, expired or already used | Generate new join commands. |
| Permission denied | The data volume or a mounted directory does not allow writes by UID 65532. |

### The Linux Compose installer

The **Linux Docker Compose** variant requires running the installer as root (`curl … | bash`). When the current user is not root, the installer re-executes itself through `sudo`. The installer writes a Compose project to `/opt/oss-transfer-compose/agent/docker-compose.yml` with its data in `/var/lib/oss-transfer-compose/agent`. On Debian and Ubuntu, if Docker Engine is not yet installed on the host, the installer asks whether to install it.

If the installation fails, the installer cleans up what it created. Fix the problem and run the same command again. The installer runs on Linux only. On macOS it stops and directs you to use the Docker command instead.

## Mount host directories for local transfers

An Agent in a container can only read and write the host directories mounted into it. When creating a task, enter the path as the container sees it. Use the same absolute path on both sides, for example `/mnt/storage:/mnt/storage`, so that the path looks the same on the host and in the console.

For an Agent joined with `docker run`, before running the copied command, add `-v /mnt/storage:/mnt/storage` before the image name in the second `docker run`. If the Agent is already running, add the option and recreate the container: in **Execution nodes** > **Software updates**, choose **Recreate commands** on its row (the button reads **Update commands** when an update is available), using the same procedure as [updating container Agents](#container-agents). The identity stays in the volume and is not affected by recreation.

On Windows, Docker Desktop requires mounting a Windows folder onto a Linux path, for example `-v C:\transfer:/mnt/transfer`, and then entering `/mnt/transfer` in the task.

For the Linux Compose installer, add `--files-dir`, for example `curl -fsSL '<join link>' | sudo bash -s -- --files-dir /mnt/storage`. The directory must exist beforehand.

Agents in containers run as UID 65532. On Linux, grant that user access, for example `sudo chown -R 65532:65532 /mnt/storage` for a download destination. A download task checks whether the destination directory is writable before it starts. If the directory is not writable, the task fails with `LOCAL_PATH_NOT_WRITABLE` and provides instructions.

The `agent` profile in `docker-compose.yml` starts an Agent from an already enrolled `agent-data` volume with `TRANSFER_LOCAL_ROOT` mounted at `/files`. You do not need it when adding Agents from the console.

## Logs

```sh
docker compose logs -f master               # Master, in the Master's directory
docker logs -f oss-transfer-agent-<id>      # an Agent added with docker run
sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml logs -f
```

To list the Agent containers on a host, run `docker ps -a --filter name=oss-transfer-agent`.

## Use the CLI in the container

The image includes `oss-transfer-cli`. In the Master container it requires no login:

```sh
docker compose exec master oss-transfer-cli task list
docker compose exec master oss-transfer-cli node list
```

There are two ways to get files out of the container.

Task reports can be written directly to standard output. Add `-T` so that Docker passes the bytes unchanged without allocating a terminal:

```sh
docker compose exec -T master oss-transfer-cli task report 12 --output - > task-12-report.zip
```

When the report is incomplete, the ZIP itself is still valid. The reason is written to standard error and the exit status is 3.

For exports such as object comparisons and exception records, write them into the data volume, copy them to the host, and delete them afterward:

```sh
docker compose exec master oss-transfer-cli object export <comparison-id> --output /data/comparison.zip
docker compose cp master:/data/comparison.zip .
docker run --rm --volumes-from "$(docker compose ps -q master)" busybox:1.37 rm /data/comparison.zip
```

If you prefer not to use the command line, the **Export task report** and **Export all differences (ZIP)** buttons in the console download directly to your browser.

## Settings and the administrator password

Containers have no `service.json`. Node rate limits and concurrency are changed on the **Execution nodes** page or with the CLI; they are stored in the Agent's data volume. The object comparison result budget uses its defaults (see [result storage and retention](operations.md#result-storage-and-retention)).

To reset a forgotten administrator password, stop the Master and run the reset in a temporary container:

```sh
docker compose stop master
docker compose run --rm master oss-transfer-master --data-dir /data/master --reset-admin-password
docker compose start master
```

Sign in with the printed password and set a new one.

## Back up

Stop running migration tasks in the console, then stop the containers. The following commands back up the Master's data volume to `backups/master-data.tar.gz`:

```sh
docker compose stop
mkdir -p backups
docker run --rm --volumes-from "$(docker compose ps -aq master)" \
  -v "$PWD/backups:/backup" busybox:1.37 \
  tar -czf /backup/master-data.tar.gz -C /data .
```

For an Agent added with `docker run`, stop the container and archive its volume the same way:

```sh
docker stop oss-transfer-agent-<id>
docker run --rm -v oss-transfer-agent-<id>-data:/data \
  -v "$PWD/backups:/backup" busybox:1.37 \
  tar -czf /backup/agent-<id>-data.tar.gz -C /data .
docker start oss-transfer-agent-<id>
```

For an Agent installed with the Linux Compose installer, stop its Compose service and copy `/var/lib/oss-transfer-compose/agent` to the backup location. Also save the Compose file under `/opt/oss-transfer-compose/agent`. If you changed the default paths during installation, use the paths printed by the installer.

Back up the Master's `docker-compose.yml` and `.env` as well. Transferred files need to be backed up separately. The archive commands overwrite any earlier archive with the same name.

## Update

Update the Master first, then the Agents. Choose a time when no tasks are running. Back up before updating.

A new release may upgrade the database schema, after which older images cannot open the data (see [database space](database-maintenance.md)). To downgrade, switching back to the old image alone is not sufficient; you must also restore the pre-upgrade Master and Agent data backups.

### Master

If `.env` pins a version, change the image tag first. Then:

```sh
docker compose pull master
docker compose up -d master
```

The existing data volumes are preserved. Do not use `docker compose down -v`, which deletes them.

### Container Agents

Container Agents cannot update online. Recreate each container from the new image. The identity and settings are stored in the volume and are not lost. Running child tasks are interrupted and resume automatically when the Agent reconnects.

<p align="center"><img src="images/docker-recreate.en.png" width="720" alt="Recreate commands dialog for a Docker Agent in Software updates, with the docker run commands outlined"></p>

1. Open **Execution nodes** > **Software updates**. A Docker Agent's row has an **Update commands** button. If the Agent already matches the Master's version, the button reads **Recreate commands** and shows the same commands, which can be used for purposes such as adding a directory mount.
2. The dialog shows the commands for that Agent, with the image matching the Master. For a node joined with `docker run`, the commands are `docker pull <image>`, `docker rm -f <container>`, followed by the `docker run` command that starts it again. For a node deployed with the installer's Docker Compose method, change `image` in `/opt/oss-transfer-compose/agent/docker-compose.yml` to the new image and run:

   ```sh
   sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml pull
   sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml up -d
   ```

3. If you created the container with additional options such as directory mounts, add them to the `docker run` command. On Linux, an Agent that reaches the Master through `host.docker.internal` also needs `--add-host host.docker.internal:host-gateway`.
4. After the update completes, the node shows as **Online** and the version number is refreshed.

## Remove an installation

Before removing anything, export the data you need to keep. The per-object results in task reports and the object comparison results are stored in the Agent's data volume. Once a node is removed, these results can no longer be retrieved: task reports become incomplete and comparison records disappear (see [removing a node](operations.md#remove-a-node)).

For an Agent added with `docker run`, first remove it on the **Execution nodes** page (or run `oss-transfer-cli agent remove <node> --yes`). The Agent stops working and remains idle. Then delete the container and its data volume on its host. The data volume holds the Agent's identity, per-object task results and object comparison results:

```sh
docker rm -f oss-transfer-agent-<id>
docker volume rm oss-transfer-agent-<id>-data
```

Agents added before v4.1.42 use the names `oss-transfer-agent` and `oss-transfer-agent-data`, without the ID suffix.

For an Agent installed with the Linux Compose installer, remove the node as described above and then run on its host:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --uninstall --method compose --role agent
```

The command lists what it will remove and waits for confirmation (`--yes` skips the confirmation). It stops the Compose project and deletes the Compose file, and unless `--keep-data` is added, also deletes `/var/lib/oss-transfer-compose/agent`. The command handles only the Compose projects created by the installer. Docker images are kept.

To remove the Master, work in its directory. `docker compose down` removes the containers and keeps the `master-data` volume. `docker compose down -v` also deletes the volume, permanently losing all tasks, accounts and storage connections. Remove the image with `docker image rm moonwx/osstransfer:<tag>` when you no longer need it.
