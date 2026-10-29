# Native deployment reference

For the default Linux setup, use the [quickstart](../../README.md). For other systems, custom paths or offline installation, follow the steps below.

## Download the Unix installer

On Linux, macOS or FreeBSD:

```sh
curl -fsSL --connect-timeout 3 --max-time 15 \
  https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh ||
curl -fsSL --connect-timeout 3 --max-time 15 \
  https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/install.sh -o install.sh
```

## Install a service

On Linux, install the Master with:

```sh
sudo bash install.sh --role master --yes
```

The Master serves the WebUI and schedules tasks. Open its URL on port `1685`, then sign in as `admin` with the initial password printed by the installer. The first login requires a password change. Use `--port 1785` during installation if port `1685` is already in use.

On macOS, the same command installs a system service. To run it under your logged-in account instead:

```sh
bash install.sh --role master --user --yes
```

On FreeBSD, install Bash and curl first, then run the installer as root:

```sh
pkg install -y bash curl
bash install.sh --role master --yes
```

### Windows

Download the installer in an Administrator PowerShell window:

```powershell
& {
    try {
        Invoke-WebRequest https://raw.githubusercontent.com/MoonWX/oss-transfer/main/deploy/native/Install.ps1 -OutFile Install.ps1 -TimeoutSec 15 -ErrorAction Stop
    } catch {
        Invoke-WebRequest https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/Install.ps1 -OutFile Install.ps1 -TimeoutSec 15 -ErrorAction Stop
    }
}
```

Install the Master:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -Yes
```

The installer prints the service name, configuration path and log path. For all available arguments, run `bash install.sh --help` or `Get-Help .\Install.ps1`.

## Use a reverse proxy

Serve the console at the domain root, such as `https://transfer.example.com/`. No browser address needs to be saved in the application. For NGINX, preserve the browser's Host header and allow the Agent's long-poll requests to finish:

```nginx
location / {
    proxy_pass http://127.0.0.1:1685;
    proxy_set_header Host $http_host;
    proxy_read_timeout 60s;
}
```

Add this to the domain's existing server block. Hosting the application under a path such as `/transfer/` is not supported.

## Add execution hosts

Open **Execution nodes → Add node** on the Master, select the new host's operating system, and copy its one-line command. Run it on that host to install and join. The Master address defaults to the current browser address; change it to the Master's private URL if the Agent will connect over a private network.

The Agent connects to the Master and appears in the node list. Use the same process when adding hosts to an existing cluster.

Install an Agent on the Master host too if it will transfer files. The two services use separate data directories. Enrollment consumes the invitation once and stores the Agent identity in `agent.db`; restarts and upgrades retain that identity. Keep the Master and Agents on the same release.

## Make local files accessible

Task paths refer to the Agent host. Use absolute paths, such as `/mnt/storage/import` or `D:\transfer\import`. Its service account needs read access for uploads and write access for downloads.

Create the directories and configure mounts and permissions on the host. For a local-file task assigned to several Agents, each must access the same shared filesystem at the same absolute path. The tool checks shared access before distributing work.

Configure network filesystems to mount after a restart. A containerized Master can use native Agents; only the Agents need access to transfer directories. See [Docker Compose deployment](../../docs/docker-compose.md) for container setup.

## Change paths and startup settings

Defaults for new Linux installations:

| Item | Master | Agent |
| --- | --- | --- |
| Service | `oss-transfer-master` | `oss-transfer-agent` |
| Programs | `/opt/oss-transfer/master` | `/opt/oss-transfer/agent` |
| Configuration | `/etc/oss-transfer/master/service.json` | `/etc/oss-transfer/agent/service.json` |
| Data | `/var/lib/oss-transfer/master` | `/var/lib/oss-transfer/agent` |
| Backups | `/var/backups/oss-transfer/master` | `/var/backups/oss-transfer/agent` |

Use `--install-dir`, `--config-dir`, `--data-dir`, `--backup-dir`, `--service-name` and `--cli-path` to change these locations. PowerShell uses `-InstallDir`, `-ConfigDir`, `-DataDir`, `-BackupDir`, `-ServiceName` and `-CliPath`. Program, configuration, data and backup directories must be separate and must not contain one another.

On macOS, configuration and data live under `/Library/Application Support/OSS Transfer`, or `~/Library/Application Support/OSS Transfer` for a user service. Windows uses `%ProgramFiles%\OSS Transfer` for programs and `%ProgramData%\OSS Transfer` for configuration, data and backups. FreeBSD uses `/usr/local/lib/oss-transfer`, `/usr/local/etc/oss-transfer`, `/var/db/oss-transfer` and `/var/backups/oss-transfer`, respectively.

The Unix installer puts the Master's CLI at `/usr/local/bin/oss-transfer-cli`, or `~/.local/bin/oss-transfer-cli` for a user installation. An Agent's CLI stays beside its binary. If another installation occupies the CLI path, the interactive installer asks before replacing it. For unattended replacement, `--replace-cli` backs up the old executable first.

Edit `service.json` and restart the service to change startup settings. `--listen` and `-Listen` set the Master's listener on first installation. Change manual transfer limits in the console. CLI authentication and commands are covered in [CLI usage](../../docs/cli.md).

To host Agent downloads yourself, set `agentArtifactBase`, `agentArtifactVersion` and `agentDockerImage` in the Master's `service.json`. Use a fixed release version with matching archives and manifests.

## Check and restart services

Linux, using the default Master service:

```sh
sudo systemctl status oss-transfer-master
sudo tail -n 50 /var/lib/oss-transfer/master/logs/service.log
sudo systemctl restart oss-transfer-master
```

For a macOS user service:

```sh
launchctl print "gui/$(id -u)/oss-transfer-master"
tail -n 50 "$HOME/Library/Application Support/OSS Transfer/master/data/logs/service.log"
launchctl kickstart -k "gui/$(id -u)/oss-transfer-master"
```

FreeBSD, as root:

```sh
service oss_transfer_master status
tail -n 50 /var/db/oss-transfer/master/logs/service.log
service oss_transfer_master restart
```

Windows, in Administrator PowerShell:

```powershell
Get-Service OSSTransferMaster
Get-Content "$env:ProgramData\OSS Transfer\master\data\logs\service.log" -Tail 50
Restart-Service OSSTransferMaster
```

Use the names and paths printed during installation if you changed the defaults.

## Reset the administrator password

Stop the Master before resetting a lost password. For the default Linux installation:

```sh
sudo systemctl stop oss-transfer-master
sudo /opt/oss-transfer/master/oss-transfer-master \
  --config /etc/oss-transfer/master/service.json --reset-admin-password
sudo systemctl start oss-transfer-master
```

Use the new password printed by the command, then choose your own password after login. The reset signs out other sessions. On other systems, stop the corresponding service and run its Master binary with the same options.

## Upgrade

Stop active tasks. On Linux, rerun the [install command](../../README.md#install); it detects existing services and offers to upgrade them.

On macOS, [download the current installer](#download-the-unix-installer) and run `bash install.sh --upgrade`. Add `--user` for a user service. On FreeBSD, run the same upgrade command as root.

On Windows, download the current `Install.ps1` using the download block in [Windows installation](#windows), then run:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Upgrade
```

The installer lists discovered services and reuses their paths, including older installations with `oss-transfer-go` names. To upgrade only Agents, add `--role agent` or `-Role agent`. A custom service can be selected with `--service-name` or `-ServiceName`. If its service definition has been removed, pass its existing `--install-dir` or `-InstallDir` so the installer can locate it.

The installer stops the service and backs up its programs, configuration, service definition and database. Accounts, settings and Agent identities remain. If the new service fails its startup check, the installer restores the backup and restarts the previous service if it was running. Older installations that did not record a custom backup directory use the platform default.

After a successful upgrade, it keeps the two most recent upgrade backups and removes backups older than 30 days, always keeping at least one rollback copy. It also checks for eligible backups before the next upgrade's disk-space check. Backups from failed upgrades and older installers are left for manual review.

Stop active tasks before upgrading if they must stay stopped during rollback. Restoring a database does not undo objects or files already written. For a separate backup, stop the service and copy its configuration and data directories, or take a consistent filesystem snapshot.

If a file-list task originally included a missing object, retrying after that object appears saves its source identity in a newer database format. V4.1.8 and earlier cannot open that database. To return to an older release, restore its programs and the matching pre-upgrade database backup on the Master and Agents together.

After a new installation or administrator password reset in V4.1.11, downgrading to V4.1.10 or earlier requires restoring the matching older database backup.

## Install without network access

Use one fixed release from the OSS mirror. For example, [v4.1.6/manifest.tsv](https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/v4.1.6/manifest.tsv) lists that release’s filenames and checksums; download its archives and installers from the same `v4.1.6/` directory. To find a newer version, read the `version` row in the [latest manifest](https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/manifest.tsv) and use the matching `v<version>/` directory. Copy the following files into one directory on the target host:

| Target | Files |
| --- | --- |
| Linux, macOS or FreeBSD | `install.sh`, `native-install.sh`, `manifest.tsv`, platform `.tar.gz` archive |
| Windows | `Install.ps1`, `manifest.tsv`, platform `.zip` archive |

For Unix, run from that directory:

```sh
sudo bash install.sh --package-dir "$PWD"
```

On Windows, run from that directory in Administrator PowerShell:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -PackageDir $PWD.Path
```

Add `--upgrade` or `-Upgrade` to upgrade an installed service with those files. Offline Agent enrollment still requires a reachable Master and its invitation. The installer verifies package checksums before replacing files.

## Register existing data as a service

Use `--adopt-config` to install a service around stopped Go data. V4.0.x data needs [offline conversion](../../docs/development/legacy-migration.md) first.

The service name and data directory must match `service.json`. Program and configuration installation directories must be empty. For a Master converted into `/srv/oss-transfer-import/master`, run:

```sh
sudo bash install.sh --role master \
  --adopt-config /srv/oss-transfer-import/master/service.json \
  --service-name oss-transfer-master \
  --data-dir /srv/oss-transfer-import/master --yes
```

Windows accepts `-AdoptConfig`, `-ServiceName` and `-DataDir`. For each Agent, copy its finalized data directory to the absolute path recorded in its generated configuration, then register that configuration on the Agent host.

The installer checks the stopped database and migration status, then takes a snapshot before starting the service. Accounts and Agent identities remain. If startup fails, it restores the snapshot.

## Build offline packages

From a clean source checkout with Go, Node.js, npm and Python 3 installed:

```sh
./build.sh --all
python3 deploy/native/release-assets.py --packages output/packages \
  --output "output/releases/$(git rev-parse --short HEAD)" \
  --version "$(cat VERSION)" --commit "$(git rev-parse HEAD)"
```

The output directory must not already exist. The assembler verifies each package's hashes, platform, version and commit, and includes installers from that commit. Use the resulting directory with `--package-dir`. See [release steps](../../.github/RELEASING.md) for publication.

## Platform requirements

Linux service installation requires systemd, macOS requires 12 or later, and Windows requires Windows 10 or Server 2016 or later. FreeBSD service installation is tested on 14.4. Linux binaries are built without CGO and do not depend on a particular glibc version.

CPU baselines are amd64 v1, arm64 v8.0, ARMv7 and RISC-V RVA20U64. For openEuler, Kylin or UOS, choose the package by CPU architecture and check the kernel against the [Go platform requirements](https://go.dev/wiki/MinimumRequirements). `loong64` requires LoongArch and Linux 5.19 or later; it does not support older MIPS-based Loongson processors.

ARMv7 has a 32-bit address space, and its metadata database has a roughly 2 GiB mapping limit. Use a 64-bit Master for large task inventories. This limits task metadata, not the size of transferred files. V4.0.x database conversion runs on Linux or macOS; FreeBSD can use data converted on either system.
