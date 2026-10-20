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

The `.env` file sets the image and published port:

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest
TRANSFER_PORT=1685
```

## Add an Agent

In **Execution nodes → Add node**, enter a node name, select **Docker**, and copy the one-line command to the Agent host. The installer creates its Compose service and joins the cluster. If the Agent connects over a private network, change the Master address before generating the command.

The Agent's Compose file is saved at `/opt/oss-transfer-compose/agent/docker-compose.yml`. To expose a host directory, add a bind mount under the Agent's `volumes` and recreate the container. Use the same absolute path on both sides, such as `/mnt/storage:/mnt/storage`. The container runs as UID/GID `65532`, which needs access to the files being transferred.

## Upgrade and back up

Stop active transfer tasks in the console, then stop the containers. The following commands back up the Master's data volume to `backups/master-data.tar.gz`:

```sh
docker compose stop
mkdir -p backups
docker run --rm --volumes-from "$(docker compose ps -aq master)" \
  -v "$PWD/backups:/backup" busybox:1.37 \
  tar -czf /backup/master-data.tar.gz -C /data .
```

On each Agent host, stop its Compose service and copy `/var/lib/oss-transfer-compose/agent` to your backup location. Its Compose file is under `/opt/oss-transfer-compose/agent`; keep that file with the backup. Use the paths printed by the installer if you changed the defaults.

Keep the Master's `docker-compose.yml` and `.env` with its backup too. Back up transferred files separately. The archive command overwrites an earlier archive with the same name.

When retrying a file-list task resolves a previously missing source object, the database records its identity in a format that V4.1.8 and earlier cannot read. Downgrading then requires restoring the matching pre-upgrade Master and Agent data backups as well as the old image.

Pull the new image and start the Master:

```sh
docker compose pull master
docker compose up -d master
```

For each Agent, update the image in its Compose file to match the Master, then run:

```sh
sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml pull
sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml up -d
```

Keep the existing volumes when upgrading. `docker compose down -v` deletes them.
