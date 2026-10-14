# OSS Transfer

[中文](README.zh-CN.md)

OSS Transfer copies data between S3-compatible buckets and local directories. The Master provides the WebUI, API and scheduling; Agents scan and transfer data. Master, Agent and CLI are separate Go executables for Linux, macOS and Windows, on amd64 and arm64. Java is not required to run them.

## Install the Master

On Linux or macOS, download the installer from JD Cloud OSS:

```sh
RELEASE_URL=https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest
curl -fSL "$RELEASE_URL/install.sh" -o install.sh
sudo bash install.sh --lang en
```

Choose Master and a deployment method. Linux supports a native systemd service or Docker Compose; macOS uses launchd. Native installation needs no language runtime. Use `--help` for custom paths and ports, or `--user` on macOS for a service owned by your logged-in user.

On Windows, run PowerShell as Administrator:

```powershell
$ReleaseUrl = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest'
Invoke-WebRequest "$ReleaseUrl/Install.ps1" -OutFile Install.ps1
.\Install.ps1 -Role master -ReleaseUrl $ReleaseUrl -Yes
```

Open `http://HOST:1685`. Use the setup token in the service log to create your administrator account and set the Master URL that browsers and Agents will use. There is no default password. The installer prints the service name, configuration path and log location. For the default Linux installation:

```sh
sudo cat /var/lib/oss-transfer-go/master/logs/service.log
```

[GitHub Releases](https://github.com/MoonWX/oss-transfer/releases) provides the same binaries and checksums. See the [native deployment guide](deploy/go/README.md) for local release files, upgrades and rollback.

## Docker Compose

Copy `docker-compose.yml` and `.env.example` from the repository into a deployment directory:

```sh
cp .env.example .env
```

Compose uses `moonwx/osstransfer:latest`. Adjust the port and local directory in `.env` as needed:

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest
TRANSFER_PORT=1685
TRANSFER_LOCAL_ROOT=./transfer-files
```

Start the Master and read its setup token:

```sh
docker compose pull master
docker compose up -d master
docker compose logs master
```

Complete setup in the browser. The `master-data` volume stores the Master's database. `docker compose down` keeps it; `docker compose down -v` deletes it.

The Master does not transfer files itself. Add an Agent even for a single-host installation. You can use the console's generated command, or the optional `agent` profile in this Compose file. For the profile, create the host directory and grant container UID/GID `65532` access:

```sh
sudo install -d -o 65532 -g 65532 -m 0750 ./transfer-files
```

Create an invitation in **Execution nodes → Add node**. In Bash, enter its token and enroll the local Agent once:

```bash
read -r -s -p 'Invitation token: ' OSS_TRANSFER_JOIN_TOKEN; printf '\n'
export OSS_TRANSFER_JOIN_TOKEN
docker compose --profile agent run --rm -e OSS_TRANSFER_JOIN_TOKEN agent \
  oss-transfer-agent --master-url http://master:1685 --data-dir /data --enroll-only
unset OSS_TRANSFER_JOIN_TOKEN
docker compose --profile agent up -d agent
```

The profile mounts `TRANSFER_LOCAL_ROOT` at `/files`; use `/files` in transfer tasks. Its identity and checkpoints live in the separate `agent-data` volume. To access more host directories, add bind mounts to the Agent service. A native Agent can access permitted host paths directly.

## Add execution nodes

In **Execution nodes → Add node**, create a single-use invitation and run the generated native or Docker command on the new host. The Agent connects outbound to the Master and retains its identity across restarts. It needs no public administration port. Use the same release for Master and Agents.

For a shared local-file task, mount the same filesystem at the same path on every selected Agent. Configure the available roots in node settings. The tool verifies shared access before assigning that task to multiple Agents.

Keep invitation links private. Removing a node revokes its credential. Rejoining a removed node requires a new invitation and a fresh Agent data directory.

## Transfer data

The WebUI supports Chinese, English and Japanese. It follows your browser language on first use; the language menu saves your choice without leaving the page. The CLI accepts `--lang en`, `--lang zh` or `--lang ja`, or the `OSS_TRANSFER_LANG` environment variable. For example: `oss-transfer-cli --lang ja task list`. JSON keys, status codes, paths and object keys stay unchanged.

Add storage connections, then create a task with its source, destination and execution nodes. Choose `SKIP` or `OVERWRITE`, key mapping and limits as needed. Task details show progress and clickable child tasks, numbered `#parent-1`, `#parent-2`, and so on.

Stop and resume preserves recorded object results. An unfinished download restarts the file; an unfinished S3 upload is aborted and that object is uploaded again. Completed multipart parts are not reused after a process restart. Reports are available after a task stops or finishes. Node limits cover all work on that node; a task's bandwidth limit is shared across its selected nodes.

The CLI uses the same user accounts as the WebUI. Log in from Bash:

```bash
read -r -s -p 'Password: ' OSS_TRANSFER_PASSWORD; printf '\n'
export OSS_TRANSFER_PASSWORD
oss-transfer-cli --host http://HOST:1685 login --username admin --password-env OSS_TRANSFER_PASSWORD
unset OSS_TRANSFER_PASSWORD
```

Use the same `--host` for subsequent commands:

```sh
oss-transfer-cli --host http://HOST:1685 task list
oss-transfer-cli --host http://HOST:1685 task show 123
oss-transfer-cli --host http://HOST:1685 task children 123 --offset 0 --limit 25
oss-transfer-cli --host http://HOST:1685 task stop 123
oss-transfer-cli --host http://HOST:1685 task resume 123
oss-transfer-cli --host http://HOST:1685 task report 123 --output task-123.zip
```

Use `--help` for creation, connection and node commands; add `--json` for structured output. In Compose, run the CLI with `docker compose exec master oss-transfer-cli`.

## Upgrade and back up

For an existing Go native installation, download the new version's installer and run it with the same paths and `--upgrade` (`-Upgrade` on Windows). It verifies the binaries, backs up the stopped database and service configuration, then checks the replacement service. Failed startup triggers rollback. The CLI is replaced only when owned by that installation or explicitly approved with `--replace-cli`.

For Compose, stop active tasks, back up the stopped services' data volumes, then run `docker compose pull` and `docker compose up -d`. If you pinned an image version, update that tag first. Keep the existing volumes and Agent mounts. Application rollback does not undo files or objects already transferred.

V4.0.x databases require [offline export and conversion](docs/development/legacy-migration.md). Do not point Go at the old RocksDB directory. The [migration guide](docs/go-migration.md) describes preserved state and the first resume after conversion.

## Build

Use the Go version declared in `go.mod`, Node.js 24 and Python 3. A Bash environment is required for the build script:

```sh
./build.sh --test
./build.sh --all
```

The first command tests and builds the current platform; the second builds all six platforms. Archives and checksums are written to `output/go-development/`. Each archive includes the three executables, `VERSION` and `SHA256SUMS`. The WebUI is embedded in the Master binary.

See [release steps](.github/RELEASING.md) and [acceptance checks](.github/acceptance/README.md).
