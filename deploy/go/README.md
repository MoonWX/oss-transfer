# Native deployment

Install a Master for the WebUI and scheduling, then an Agent on each host that will transfer files. A single host can run both services with separate data directories. Linux uses systemd, macOS uses launchd, Windows uses Windows Services, and FreeBSD uses rc.d.

## Install

On Linux, macOS or FreeBSD (install `bash` and `curl` first on FreeBSD):

```sh
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
sudo bash install.sh --role master --method native --yes
```

In mainland China, download from `https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/install.sh` instead.

The installer selects JD Cloud OSS for a mainland Chinese public egress IP and GitHub for other locations. Use `--source github` or `--source jdoss` to override it. Add `--version 4.1.5` to select a release. An explicit `--release-url URL` or offline `--package-dir DIR` skips location detection.

The Master listens on port `1685`. Add `--port 1785` to change it. For a macOS user service, run the command without `sudo` and add `--user`.

On Windows, open PowerShell as Administrator:

```powershell
Invoke-WebRequest 'https://raw.githubusercontent.com/MoonWX/oss-transfer/main/deploy/go/Install.ps1' -OutFile Install.ps1
.\Install.ps1 -Role master -Listen '0.0.0.0:1685' -Yes
```

The Windows download URL for mainland China is `https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/Install.ps1`. PowerShell uses `-Source`, `-Version`, `-ReleaseUrl` and `-PackageDir` for the corresponding options.

Open the Master URL and enter the setup token printed in its service log. Create an administrator and set the public URL that browsers and Agents will use. The installer prints the log and configuration paths.

To install offline, put `manifest.tsv`, the installers and your platform's archive in one directory, then use `--package-dir DIR` (`-PackageDir DIR` on Windows). For Linux amd64, download `install.sh`, `native-install.sh`, `manifest.tsv` and `oss-transfer-linux-amd64.tar.gz` from the same version directory. The installer verifies the archive and extracts the selected service and CLI.

## Add an Agent

1. Open **Execution nodes → Add node** on the Master and create an invitation.
2. Copy the native command for the execution host's operating system.
3. Run it on that host as root on Linux, through the chosen launchd mode on macOS, or in Administrator PowerShell on Windows.
4. Confirm the Agent is online and select it when creating a task.

Repeat these steps when adding hosts to an existing cluster. If the Master host will transfer files, install an Agent there too.

Agents connect to the Master's public URL. Enrollment consumes the single-use invitation and saves the identity in `agent.db`; restarts and upgrades reuse it. Keep Master and Agents on the same release.

## Local directories

Enter absolute paths from the execution host, such as `/mnt/storage/import` or `D:\transfer\import`. The Agent's service account needs read permission for uploads and write permission for downloads. Configure network filesystems to mount after a restart, and check that the service account can access them before starting tasks.

**Execution nodes → Edit node** records local roots shown in the console. Create the directories and configure mounts and permissions on the host.

For a local-file task with several Agents, every selected Agent must access the same shared filesystem at the same absolute path. The tool checks shared access before distributing work.

A containerized Master can use native Agents. Set its public URL to an address the Agent hosts can reach. If an Agent also runs in Docker, map its transfer directories as described in [Docker Compose](../../README.md#docker-compose-optional).

## Paths and configuration

Linux defaults:

| Item | Master | Agent |
| --- | --- | --- |
| Service | `oss-transfer-go-master` | `oss-transfer-go-agent` |
| Programs | `/opt/oss-transfer-go/master` | `/opt/oss-transfer-go/agent` |
| Configuration | `/etc/oss-transfer-go/master/service.json` | `/etc/oss-transfer-go/agent/service.json` |
| Data | `/var/lib/oss-transfer-go/master` | `/var/lib/oss-transfer-go/agent` |
| Backups | `/var/backups/oss-transfer-go/master` | `/var/backups/oss-transfer-go/agent` |

Use the paths printed by the installer on macOS and Windows. Override them with `--install-dir`, `--config-dir`, `--data-dir`, `--backup-dir`, `--service-name` and `--cli-path`. PowerShell uses `-InstallDir`, `-ConfigDir`, `-DataDir`, `-BackupDir`, `-ServiceName` and `-CliPath`.

The public Unix installer puts the Master's CLI at `/usr/local/bin/oss-transfer-cli`, or `~/.local/bin/oss-transfer-cli` for a user installation. An Agent's CLI stays beside its binary. If an unrelated CLI occupies the path, choose another location or pass `--replace-cli` to back up and replace it.

Edit `service.json` and restart the service to change its listener or other startup settings. `--listen` applies to first installation. Change local roots and manual limits in the console.

Master release builds include the Agent download location and Docker image version. To host release files elsewhere, set `agentArtifactBase`, `agentArtifactVersion` and `agentDockerImage` in Master configuration.

On Linux, check the service and read its log with:

```sh
sudo systemctl status oss-transfer-go-master
sudo tail -f /var/lib/oss-transfer-go/master/logs/service.log
```

After editing configuration:

```sh
sudo systemctl restart oss-transfer-go-master
```

Include the Master's actual URL and port in CLI `--host`; see [CLI login](../../README.md#use-the-cli).

## Upgrade

Download the installer again from either address in [Install](#install), then run:

```sh
sudo bash install.sh --upgrade
```

```powershell
.\Install.ps1 -Upgrade
```

The installer discovers existing services and reuses their installation, configuration, data and CLI paths. It lists all matching services before upgrading them. Use `--role agent` or `--service-name NAME` to limit the selection (`-Role` or `-ServiceName` on Windows). Keep `--user` and omit `sudo` for a macOS user service. An installation with no remaining service definition needs `--install-dir` (`-InstallDir` on Windows) once. Older installations that did not record a custom backup directory use the platform default backup directory. The installer verifies downloads, stops the service and backs up its binary, configuration, service definition and database. It retains accounts, settings and Agent identities. Startup checks the managed process, version and startup identifier; failure restores the backup and restarts the previous service if it was running.

Stop active tasks before an upgrade if they must remain paused during rollback. Restoring the database cannot undo files or objects already written by a transfer. For manual backups, stop the service before copying its configuration and data directory, or use a consistent filesystem snapshot.

## Adopt converted or manually created Go data

Use `--adopt-config` to register a service for stopped Go data. For V4.0.x, complete [offline conversion](../../docs/development/legacy-migration.md) first.

The service name and data directory must match the imported `service.json`. The program and configuration installation directories must be empty. This example matches the Master configuration produced by the converter:

```sh
sudo bash install.sh --role master --method native \
  --adopt-config /srv/oss-transfer-import/master/service.json \
  --service-name oss-transfer-master --data-dir /srv/oss-transfer-import/master --yes
```

Windows accepts `-AdoptConfig FILE`, `-ServiceName` and `-DataDir`. For each Agent, use its generated configuration and copy its finalized data directory to the recorded absolute path on its execution host.

Adoption checks the stopped database, role and migration status, then takes a snapshot before starting the service. Existing accounts and Agent identities remain. If startup fails, the installer restores the snapshot.

## Prepare local release files

From a clean source commit:

```sh
./build.sh --all
python3 deploy/go/release-assets.py --packages output/go-development \
  --output output/go-release --version "$(cat VERSION)" --commit "$(git rev-parse HEAD)"
```

Choose a new output directory for each run. The assembler checks every listed package's hashes, platform, version and commit, and includes installers from that commit. Use the resulting directory with `--package-dir`. Publication is covered in [release steps](../../.github/RELEASING.md).

## Platform requirements

Packages use baseline CPU instructions (`amd64 v1`, `arm64 v8.0`, `ARMv7`, `riscv64 RVA20U64`) and `CGO_ENABLED=0`. Linux binaries do not require a particular glibc version. The Linux installer requires systemd; macOS requires 12 or later, and Windows requires Windows 10 or Server 2016 or later. FreeBSD service installation is tested on 14.4.

For Chinese distributions such as Kylin and UOS, select the package by CPU architecture and check the kernel version. `loong64` targets LoongArch and requires Linux 5.19 or later; it does not run on older MIPS-based Loongson processors. Other Linux targets follow the [Go platform requirements](https://go.dev/wiki/MinimumRequirements), with a general kernel floor of 3.2 and architecture-specific requirements. These builds are not a vendor compatibility certification. Test an upload, download and interrupted-task recovery on the intended host and storage mount before production use.

ARMv7 has a 32-bit address space. Its embedded metadata database has a roughly 2 GiB mapping limit; use a 64-bit Master for large task inventories. This limit concerns task metadata, not the size of transferred files. V4.0.x offline database conversion runs on Linux or macOS; FreeBSD can use data converted on a supported host.
