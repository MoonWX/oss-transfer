# Native deployment

Install the Master and Agent as separate services. Linux uses systemd, macOS uses launchd, and Windows uses Windows Services. Each role has its own executable, configuration and database. A host can run both roles; only the Master exposes the WebUI port.

## Install

Download the latest stable installer from JD Cloud OSS:

```sh
RELEASE_URL=https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest
curl -fSL "$RELEASE_URL/install.sh" -o install.sh
sudo bash install.sh --role master --method native --release-url "$RELEASE_URL" --yes
```

Set `--port 1785` for a different Master port. On macOS, `--user` installs a LaunchAgent for the logged-in user instead of a system LaunchDaemon. Windows requires an Administrator PowerShell:

```powershell
$ReleaseUrl = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest'
Invoke-WebRequest "$ReleaseUrl/Install.ps1" -OutFile Install.ps1
.\Install.ps1 -Role master -ReleaseUrl $ReleaseUrl -Listen '0.0.0.0:1685' -Yes
```

Open the Master URL and enter the setup token from its log. Create the administrator and set the public URL that browsers and Agents can reach. There is no default password. The installer prints the service, configuration, log and CLI paths.

Add an Agent through **Execution nodes → Add node**, then run the generated native command on the execution host. Enrollment consumes the invitation and saves the Agent identity in `agent.db`. Later starts use that identity. An Agent has no HTTP administration listener.

For a downloaded release directory, replace `--release-url URL` with `--package-dir DIR` (`-PackageDir DIR` on Windows). This directory must contain `manifest.tsv` and the named release files; a platform archive containing only binaries is not an installer directory. The installer validates the binary checksum before stopping or replacing a service.

## Paths and configuration

Default Linux locations:

| Item | Master | Agent |
| --- | --- | --- |
| Service | `oss-transfer-go-master` | `oss-transfer-go-agent` |
| Programs | `/opt/oss-transfer-go/master` | `/opt/oss-transfer-go/agent` |
| Configuration | `/etc/oss-transfer-go/master/service.json` | `/etc/oss-transfer-go/agent/service.json` |
| Data | `/var/lib/oss-transfer-go/master` | `/var/lib/oss-transfer-go/agent` |
| Backups | `/var/backups/oss-transfer-go/master` | `/var/backups/oss-transfer-go/agent` |

Use the paths printed by the installer on macOS and Windows. `--install-dir`, `--config-dir`, `--data-dir`, `--backup-dir`, `--service-name` and `--cli-path` override the Unix defaults. Windows has corresponding `-InstallDir`, `-ConfigDir`, `-DataDir`, `-BackupDir`, `-ServiceName` and `-CliPath` parameters.

The public Unix installer places the Master CLI at `/usr/local/bin/oss-transfer-cli`, or `~/.local/bin/oss-transfer-cli` for a user installation. An Agent's CLI stays beside its binary. An unrelated existing CLI is rejected unless you choose another path or pass `--replace-cli` to back it up and replace it.

Edit `service.json` and restart the service to change its listener or other startup settings. The installer uses `--listen` only during first installation. Agent roots and manual limits can be changed in the console. Release builds include a fixed Agent download location and image version; an independently hosted release can override `agentArtifactBase`, `agentArtifactVersion` and `agentDockerImage` in Master configuration.

On Linux:

```sh
sudo systemctl status oss-transfer-go-master
sudo tail -f /var/lib/oss-transfer-go/master/logs/service.log
sudo systemctl restart oss-transfer-go-master
```

CLI sessions belong to a particular Master URL. Include its actual port in `--host`; see the [login example](../../README.md#transfer-data).

## Upgrade

Download the new release's installer and reuse the original role, paths and service name:

```sh
sudo bash install.sh --role master --method native --release-url "$RELEASE_URL" --upgrade --yes
```

```powershell
.\Install.ps1 -Role master -ReleaseUrl $ReleaseUrl -Upgrade -Yes
```

Use `agent` for an Agent upgrade. Saved settings, accounts and enrollment identity remain; a new invitation is not required. The upgrade verifies the downloaded binaries, stops the old service, then backs up its binary, configuration, service definition and database. Startup checks the managed process, release version and a fresh startup identifier. A failure restores the backup and restarts the previous service if it was running. The backup location is printed.

Stop active tasks before a planned upgrade if they must remain paused through rollback. A database backup does not undo objects or files already written by a transfer. For a manual backup, stop the service before copying its configuration and data directory, or use a consistent filesystem snapshot.

## Adopt converted or manually created Go data

`--adopt-config` registers a service for a stopped manual Go installation or finalized V4.0.x conversion. Its data directory and service name must match `service.json`; the program and configuration installation directories must be empty.

```sh
sudo bash install.sh --role master --method native --release-url "$RELEASE_URL" \
  --adopt-config /srv/oss-transfer-import/master/service.json \
  --service-name oss-transfer-go-master --data-dir /srv/oss-transfer-import/master --yes
```

Windows accepts `-AdoptConfig FILE`, `-ServiceName` and `-DataDir`. Use the Agent's own configuration to adopt an Agent. The installer verifies the database role and migration status, takes a snapshot and retains the existing identity. It rejects a running database, unfinished conversion or mismatched path; startup failure restores the snapshot.

V4.0.x RocksDB requires [offline conversion](../../docs/development/legacy-migration.md) first. Copy each finalized Agent directory to the recorded absolute path on its execution host before adoption. The installer must never use the original RocksDB directory as Go data.

## Prepare local release files

From a clean source commit, build all platforms and assemble a new directory:

```sh
./build.sh --all
python3 deploy/go/release-assets.py --packages output/go-development \
  --output output/go-release --version "$(cat VERSION)" --commit "$(git rev-parse HEAD)"
```

The assembler checks archive hashes, internal checksums, platform and a shared version/commit across all six packages. It takes installers and Compose from that commit and refuses an existing output directory. This produces the files required by `--package-dir`; it does not publish them. See [release steps](../../.github/RELEASING.md).
