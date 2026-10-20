# Docker Compose

Use Compose when the directories available to each Agent are known in advance. [Native installation](../README.md) is easier when Agents need access to changing host paths. The Master hosts the console; Agents perform transfers.

## Start the Master

On a Linux host with Docker Engine and the Compose plugin installed, download the Compose files:

```sh
mkdir oss-transfer && cd oss-transfer &&
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml &&
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/.env.example -o .env &&
docker compose pull master &&
docker compose up -d master &&
docker compose logs master
```

Open port `1685` on this host in your browser (`http://localhost:1685` when running locally). Sign in as `admin` with the initial password from the log, then choose your own password.

The `.env` file sets the image, published port and Agent's host directory:

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest
TRANSFER_PORT=1685
TRANSFER_LOCAL_ROOT=./transfer-files
```

## Add an Agent

In **Execution nodes → Add node**, enter a node name and generate an invitation. The Master address defaults to your browser address; change it if the Agent will use a private network. Run the command from the **Docker** tab on the Agent host. It uses the installer to create a Compose service and join the cluster. The native installation command is available in the same dialog.

To manage an Agent in the same Compose project as the Master, create its host directory first. The container runs as UID/GID `65532`; that account needs access to the files being transferred. For the default directory:

```sh
sudo install -d -o 65532 -g 65532 -m 0750 ./transfer-files
```

With the CLI logged in to the Master, run `oss-transfer-cli agent invite --label storage-01 --json`. Enter its `inviteToken` at the prompt below. Enrollment saves the Agent's identity in its data volume:

```bash
read -r -s -p 'Agent invitation token: ' OSS_TRANSFER_JOIN_TOKEN; printf '\n'
export OSS_TRANSFER_JOIN_TOKEN
docker compose --profile agent run --rm -e OSS_TRANSFER_JOIN_TOKEN agent \
  oss-transfer-agent --master-url http://master:1685 --data-dir /data --enroll-only
unset OSS_TRANSFER_JOIN_TOKEN
docker compose --profile agent up -d agent
```

The Agent can access the host directory at `/files`. For example, `./transfer-files/example.txt` on the host is `/files/example.txt` when creating a task. To expose another directory, add a bind mount under the Agent's `volumes` in `docker-compose.yml` and recreate the Agent container.

## Upgrade and back up

Stop active transfer tasks in the console, then stop the containers. The following commands back up the Master's data volume to `backups/master-data.tar.gz`:

```sh
docker compose --profile agent stop
mkdir -p backups
docker run --rm --volumes-from "$(docker compose ps -aq master)" \
  -v "$PWD/backups:/backup" busybox:1.37 \
  tar -czf /backup/master-data.tar.gz -C /data .
```

If you run the Compose Agent, back up its data volume too:

```sh
docker run --rm --volumes-from "$(docker compose --profile agent ps -aq agent)" \
  -v "$PWD/backups:/backup" busybox:1.37 \
  tar -czf /backup/agent-data.tar.gz -C /data .
```

Keep `docker-compose.yml` and `.env` with the backup. These archives contain application state and Agent credentials; files in `/files` require their own backup. The commands overwrite earlier archives with the same names.

When retrying a file-list task resolves a previously missing source object, the database records its identity in a format that V4.1.8 and earlier cannot read. Downgrading then requires restoring the matching pre-upgrade Master and Agent data backups as well as the old image.

Pull the new image and start the Master:

```sh
docker compose pull master
docker compose up -d master
```

If using the Compose Agent, restart it with the same image:

```sh
docker compose --profile agent up -d agent
```

Keep the existing volumes when upgrading. `docker compose down -v` deletes them.
