# Install, upgrade and uninstall

[中文](install.zh-CN.md)

This page covers everything about installing OSS Transfer on a host: choosing a version, checking that the installation worked, other operating systems, paths and logs, upgrading, uninstalling, and installing the command-line interface (CLI) on another computer. The [quick start](../README.md#quick-start) shows the shortest path on Linux. For hosts without internet access, use [offline installation](offline-install.md). For containers, see [Docker Compose](docker-compose.md).

Before installing a cluster for a large migration, read [planning](planning.md) (topology, ports, storage permissions, sizing) and [security](security.md) (TLS, credentials, passwords).

## Before you start

- **Linux:** systemd 219 or later (CentOS 7 / RHEL 7 and newer), Bash, curl, tar and a SHA-256 tool. Run the installer as root or through `sudo`.
- **macOS, Windows, FreeBSD:** see [other systems](#other-systems).
- **Ports:** the Master listens on TCP `1685` by default. Browsers, the CLI and every Agent (execution node) must reach it. Agents need no inbound port. See [planning](planning.md) for the full list.
- **What runs as root:** native services installed by the installer run as root on Linux and FreeBSD (the systemd unit or rc.d script sets no other user), as root for a macOS system service, and as LocalSystem on Windows. A macOS `--user` installation runs as the signed-in user. Online updates need these privileges. An Agent started with the console's Docker command runs as UID 65532 inside its container.

## Install on Linux

Run the installer on the Master host:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash
```

From mainland China, use the mirror `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/install.sh` in the same command.

The installer asks for the language, the role (Master or Agent), the deployment method (native or Docker Compose) and, for a native Master, whether to install an Agent on the same host. That local Agent joins the Master automatically.

To pass options to a piped installer, put them after `bash -s --`. For example, an unattended Master with a local Agent:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --role master --with-agent --yes
```

Run `bash install.sh --help` on a saved copy to list every option.

### Check the installation

A successful installation ends like this (paths and addresses differ on your host):

```text
Service: oss-transfer-master
Configuration: /opt/oss-transfer/master/config/service.json
Log: /opt/oss-transfer/master/data/logs/service.log
Installation: /opt/oss-transfer/master/bin
CLI: /usr/local/bin/oss-transfer-cli
Tasks: /usr/local/bin/oss-transfer-cli task list

=== master 4.2.0 · Installed successfully ===
Web UI: http://10.0.0.5:1685

Initial administrator: admin
Initial password: <printed once>
```

Then check:

1. `sudo systemctl status oss-transfer-master` shows `active (running)`. For an Agent, use `oss-transfer-agent`.
2. Open the **Web UI** address in a browser and sign in as `admin` with the initial password. You must choose a new password: at least 8 characters, different from the user name and from the initial password.
3. On the Master host, `sudo oss-transfer-cli node list` lists the local Agent as online, if you installed one.

The initial password is printed only once. If you lost it, [reset the administrator password](operations.md#reset-the-administrator-password).

If the installer stops with an error:

| Message | What to do |
| --- | --- |
| `Cannot listen on …: port 1685 is already in use by …` | Stop the other program, or install with `--port` (Master only, see [paths and ports](#paths-and-ports)). Nothing was changed. |
| `Startup timed out after …s` | Read the log named in the message and `systemctl status`, fix the cause, then run the same command again. |
| `Startup failed: this Agent was removed from the cluster on the Master` | The Agent's enrollment was revoked. Copy a new join command from **Execution nodes → Add node**, or [uninstall](#uninstall) the Agent. |
| `… is already v4.2.0 and running; nothing was changed. Add --force to reinstall it.` | The same version is already installed and healthy. Add `--force` only if you want to reinstall it. |

### Add Agents

Open **Execution nodes → Add node** in the console, enter a node name and a Master address that the new node can reach, and choose **Generate join commands**. Copy the command for the node's system and run it on that host. The native command for Linux, macOS and FreeBSD is `curl -fsSL '<link>' | sudo bash`. The node appears online on **Execution nodes** within a few seconds. From the CLI, `oss-transfer-cli agent invite` prints the same commands ([CLI](cli.md#add-and-remove-agents)). For the Docker commands, see [Docker Compose](docker-compose.md).

## Other systems

### CentOS 7 / RHEL 7

These are the oldest supported Linux releases. Install with the same command as on other Linux systems: the installers and online updates work with the systemd 219, Bash 4.2 and curl 7.29 they ship. Older systemd changes two details of online updates:

- Before systemd 236, a failed update leaves its `oss-transfer-update-*` helper unit listed as failed until you run `systemctl reset-failed`.
- Before systemd 240, an update helper that cannot start is not reported at once; the update fails only when it times out.

### macOS

Install a service for the current user:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
bash install.sh --role master --user
```

For a system service, use `sudo bash install.sh --role master` instead. The installer prints the service name and paths. Docker Compose installation is Linux only; with Docker Desktop, add Agents with the console's Docker command instead.

### Windows

Run in Administrator PowerShell:

```powershell
Invoke-WebRequest https://raw.githubusercontent.com/MoonWX/oss-transfer/main/deploy/native/Install.ps1 -OutFile Install.ps1
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master
```

In mainland China, the script is also available at `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/Install.ps1`. The installer ends with `Installation successful: master 4.2.0`, the web console address, the service name (`OSSTransferMaster` or `OSSTransferAgent`), the paths and the log file. Run `Get-Help .\Install.ps1` for the options.

### FreeBSD

Run as root:

```sh
pkg install -y bash curl
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
bash install.sh --role master
```

On every system, sign in with the `admin` password printed by the installer. The native Master installer offers to install a local Agent too.

## Download sources

The installers download from GitHub, or from the JD Cloud OSS mirror when the host's public IP is in mainland China, and try the other source if the first cannot be reached. To find the location, they ask `https://www.cloudflare.com/cdn-cgi/trace`, then `https://ipinfo.io/country`. Pass `--source github` or `--source jdoss` (`-Source` for `Install.ps1`) to choose a source and skip this lookup. On the mirror, `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/` always holds every file of the latest stable release, including `install.sh` and `Install.ps1`; each version, prereleases included, also has its own directory, such as `v4.2.0/`.

An installer saved as a file (not piped into the shell) looks for `manifest.tsv` or this host's platform archive in its own folder, then in the current directory. If either is there, it installs from the files in that folder without downloading anything, and `--version`, if given, must match them; `--source github` or `--source jdoss` (`-Source` for `Install.ps1`) forces a download instead. See [offline installation](offline-install.md).

Agents added from the console download the installer and package from the Master by default, so they need no internet access. The join dialog shows the download source.

## Install a specific or prerelease version

Installers use the latest stable release by default. To install a fixed version, pass it with `--version` (`-Version` for `Install.ps1`). A leading `v` is optional:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --version 4.2.0
```

```sh
bash install.sh --role master --version 4.2.0
```

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -Version 4.2.0
```

The installer downloads that release from the GitHub release `v4.2.0` or from the mirror directory `oss-transfer/v4.2.0/`. Release notes and all files of a version are on [GitHub Releases](https://github.com/MoonWX/oss-transfer/releases).

Prereleases, such as `4.2.1-beta.1`, are installed the same way:

```sh
bash install.sh --role master --version 4.2.1-beta.1
```

The installer verifies a prerelease exactly like a stable release: the GitHub release digest for that tag or the versioned OSS manifest, never `latest`. Rerunning the installer with `--upgrade` and `--version` upgrades an existing installation to that version, including from a prerelease to its stable release. Agents added from the console download the Master's own version.

For Docker Compose, set the exact image tag in `.env`; `latest` and `X.Y` only follow stable releases:

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:4.2.0
```

Prereleases are experimental; see [trying a prerelease](operations.md#try-a-prerelease) for online updates and returning to stable.

## Paths and ports

New native installations keep their files under one root. Linux defaults to `/opt/oss-transfer`, macOS to `/Library/Application Support/OSS Transfer` (the user Library for `--user`), FreeBSD to `/usr/local/lib/oss-transfer`, and Windows to `%ProgramData%\OSS Transfer`. Set `--root-dir` or `-RootDir` to use another disk. OS service registration remains outside this directory; Unix also keeps a global CLI entry point.

Default Linux locations:

| | Master | Agent |
| --- | --- | --- |
| Service | `oss-transfer-master` | `oss-transfer-agent` |
| Programs | `/opt/oss-transfer/master/bin` | `/opt/oss-transfer/agent/bin` |
| Configuration | `/opt/oss-transfer/master/config/service.json` | `/opt/oss-transfer/agent/config/service.json` |
| Data | `/opt/oss-transfer/master/data` | `/opt/oss-transfer/agent/data` |
| Service log | `/opt/oss-transfer/master/data/logs/service.log` | `/opt/oss-transfer/agent/data/logs/service.log` |
| Upgrade backups | `/opt/oss-transfer/master/backups` | `/opt/oss-transfer/agent/backups` |
| CLI | `/usr/local/bin/oss-transfer-cli` | `/usr/local/bin/oss-transfer-cli` |

On Windows, the services are `OSSTransferMaster` and `OSSTransferAgent`, and each role uses `bin`, `config`, `data` and `backups` under `%ProgramData%\OSS Transfer\<role>`; the CLI is `bin\oss-transfer-cli.exe`.

The default Master port is `1685`. Change it with `--port 1785` in `install.sh`, or `-Listen '0.0.0.0:1785'` in `Install.ps1`. Use `--install-dir`, `--config-dir` or `--data-dir` for custom locations; these directories must not contain one another. Upgrades reuse the installed paths.

## Logs

Each service writes its own log file, `logs/service.log` in its data directory (see the table above; the installer prints it as `Log:`). This is where startup errors, task problems, comparison failures and the message of a removed Agent appear. An Agent also logs each child task attempt's start and end with its counts, the first failed objects of each attempt, storage retries during an outage and changes of its connection to the Master ([monitoring and logs](operations.md#monitoring-and-logs)):

```sh
sudo tail -n 100 /opt/oss-transfer/master/data/logs/service.log
sudo tail -f /opt/oss-transfer/agent/data/logs/service.log
```

systemd records only when the service starts, stops or fails, with its exit status:

```sh
sudo systemctl status oss-transfer-agent
sudo journalctl -u oss-transfer-agent --since today
```

The log file grows until you rotate it; the services do not rotate it themselves. Because they append to it, `logrotate` with `copytruncate` works without restarting them. Online updates log to the same file. See [monitoring and logs](operations.md#monitoring-and-logs) for what to watch.

## Upgrade

The usual way is online, from the console: update the Master under **Tools → Software updates**, then the Agents under **Execution nodes → Software updates** ([software updates](operations.md#software-updates)). Use the installer instead for the first upgrade of an installation that cannot update online, on hosts without internet access ([offline installation](offline-install.md#upgrade-and-reinstall)), or to install a version other than the latest.

To upgrade with the installer, run it on each host, the Master first:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --upgrade --yes
```

`--upgrade` finds the native services installed on the host, backs each one up, installs the new version and checks that it starts. Add `--role master` or `--role agent` to upgrade only one of them, and `--version` to choose the version. Rerunning the one-line command without options on a host that already has an installation also offers to upgrade it. On Windows, add `-Upgrade` to the installation command.

The installer prints `=== master 4.2.0 · Installed successfully ===` (or `agent`) for each upgraded service. If the new version fails to start, the installer restores the previous one and keeps the backup ([recover from a failed upgrade](operations.md#recover-from-a-failed-upgrade)).

Rerunning the same version on an installation that is complete and running changes nothing: interactively the installer asks whether to reinstall it; with `--yes` it prints `… is already v4.2.0 and running; nothing was changed. Add --force to reinstall it.` and exits with status 0. Add `--force` to reinstall it anyway. A missing or damaged part (program, service definition, CLI, Agent packages) is repaired without `--force`. `Install.ps1` has no `-Force`; it always reinstalls.

Read [what changes with v4.2.0](operations.md#upgrading-to-v420) before upgrading an existing cluster.

## Reinstall without losing data

Run the installer again, using the same `--root-dir` if you changed it. It recognizes retained service configuration and data, restores missing programs or service definitions, and reuses the Agent identity. Existing installations in the older layout keep their saved paths.

Keep each role's `config` and entire `data` directory, including its key file (`master.db.key` or `agent.db.key`). The key must match the database. Without the configuration or key, the installer stops instead of creating an empty replacement. Source files, destination files and shared mounts are separate from these application directories. To move an installation to another host, see [backup and restore](operations.md#backup-and-restore).

Use `--with-agent` or `--without-agent` for an unattended native Master installation. Windows accepts `-WithAgent` or `-WithoutAgent`. An existing local Agent is reused only if it belongs to that Master.

## Uninstall

Remove an Agent from the Master before uninstalling it, so that it is no longer offered for tasks: **Execution nodes**, or `oss-transfer-cli agent remove NODE --yes` ([remove a node](operations.md#remove-a-node)). Uninstalling a Master does not remove its Agents; uninstall them on their own hosts.

### Linux, macOS and FreeBSD

Run the installer with `--uninstall`. Nothing is downloaded: `install.sh` carries its own copy of the native installer.

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --uninstall --role agent
```

With a saved copy, run `sudo bash install.sh --uninstall --role agent`. The options:

| Option | Effect |
| --- | --- |
| `--role master` or `--role agent` | Uninstall only that role. Without it, every installation on the host is uninstalled. |
| `--keep-data` | Keep the configuration, the data directory and the upgrade backups, so that you can reinstall later with the same identity, tasks and settings. |
| `--yes` | Do not ask for confirmation. Without a terminal, `--yes` is required. |
| `--service-name`, `--root-dir`, `--install-dir`, `--user` | Select an installation with a custom name or location, or a macOS user service. |

`--uninstall` cannot be combined with installation options, and `--keep-data` needs `--uninstall`.

The installer first lists everything under `Installations to uninstall:`, `Will remove:` and `Will keep:`, and asks `Uninstall? [y/N]`. It stops every service before deleting any file, then prints `=== Uninstalled ===` with the next steps.

It removes:

- the service definition (systemd unit, launchd plist or rc.d script) and the program;
- the installation record (`install.tsv`) and the CLI the installation recorded, such as `/usr/local/bin/oss-transfer-cli`, with its state files;
- without `--keep-data`: `service.json`, the data directory and this service's own upgrade backups;
- when no installation is left on the host (and without `--keep-data`): the CLI logins and preferences of root and of the user who ran `sudo` (`~/.config/oss-transfer-cli`, or `~/Library/Application Support/oss-transfer-cli` on macOS).

It keeps, and says why: a CLI that another installation still uses, other installations' backups, a data directory that contains files it does not recognize or that is a shared or shallow directory, and directories that still contain other files (`Kept (contains other files)`). It refuses and changes nothing if a service definition runs a different program or a service cannot be stopped. With nothing installed, it prints `nothing to uninstall` and exits with status 0. Migrated files in source and destination directories are never touched.

### Docker Compose installations made by the installer

```sh
sudo bash install.sh --uninstall --method compose --role agent
```

This runs `docker compose down` for the installer's project, removes its Compose file under `/opt/oss-transfer-compose/<role>` and, without `--keep-data`, its data under `/var/lib/oss-transfer-compose/<role>`. It acts only on Compose files the installer wrote, and keeps the Docker image (`docker image rm` removes it). A plain `--uninstall` mentions such an installation if one exists.

### Agents started with the console's Docker command

An Agent started with `docker run` from **Add node → Docker** has no installer. After removing the node on the Master, delete its container and volume. Their names are in the join command, for example:

```sh
docker rm -f oss-transfer-agent-1a2b3c
docker volume rm oss-transfer-agent-1a2b3c-data
```

Agents added with this command before v4.2.0 use `oss-transfer-agent` and `oss-transfer-agent-data`. For your own Compose projects, see [Docker Compose](docker-compose.md).

### Windows

`Install.ps1` has no uninstall option yet. In Administrator PowerShell, for an Agent with the default service name and paths (use `OSSTransferMaster` and `master` for a Master):

```powershell
Stop-Service OSSTransferAgent
sc.exe delete OSSTransferAgent
Remove-Item -Recurse -Force "$env:ProgramData\OSS Transfer\agent\bin"
```

To delete the data too, also remove `config`, `data` and `backups`:

```powershell
Remove-Item -Recurse -Force "$env:ProgramData\OSS Transfer\agent\config", "$env:ProgramData\OSS Transfer\agent\data", "$env:ProgramData\OSS Transfer\agent\backups"
```

Each user's CLI logins and preferences are in `%APPDATA%\oss-transfer-cli`; remove that folder when the CLI is no longer used.

## Install the CLI on another computer

Every release archive contains the CLI. To manage a cluster from a workstation, download the archive for that computer, check it, and copy out `oss-transfer-cli`. For Linux amd64 and v4.2.0:

```sh
v=4.2.0 p=linux-amd64
curl -fLO "https://github.com/MoonWX/oss-transfer/releases/download/v$v/oss-transfer-$p.tar.gz"
curl -fLO "https://github.com/MoonWX/oss-transfer/releases/download/v$v/SHA256SUMS"
grep " oss-transfer-$p.tar.gz\$" SHA256SUMS
sha256sum "oss-transfer-$p.tar.gz"
tar -xzf "oss-transfer-$p.tar.gz"
sudo install -m 755 "oss-transfer-$p/oss-transfer-cli" /usr/local/bin/oss-transfer-cli
```

The two checksums must be equal (use `shasum -a 256` on macOS). From mainland China, replace the GitHub address with `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/v$v`. The archive names are listed in [offline installation](offline-install.md#1-prepare-the-files); Apple Silicon uses `darwin-arm64`. On Windows, extract `oss-transfer-cli.exe` from `oss-transfer-windows-amd64.zip`. Use the same version as the Master.

Check it and sign in to the Master:

```sh
oss-transfer-cli --version
oss-transfer-cli config set master http://10.0.0.5:1685
oss-transfer-cli login
oss-transfer-cli task list
```

`--version` prints `OSS Transfer CLI v4.2.0` and the build. `login` signs in as `admin` (add `--username` for another account), asks for the password without showing it, and prints `Signed in to … as admin`. See [CLI](cli.md#connect-to-a-master) for scripts and other options.

## Reverse proxy

Serve the application at the domain root, for example `https://transfer.example.com/`. Add this location to the domain's NGINX server block:

```nginx
client_max_body_size 16m;

location / {
    proxy_pass http://127.0.0.1:1685;
    proxy_set_header Host $http_host;
    proxy_read_timeout 60s;
}
```

`client_max_body_size` must allow about 8 MiB: Agents send up to 8 MiB per request, and NGINX allows 1 MiB by default. `proxy_read_timeout` must be longer than 25 seconds, the time an Agent's poll can stay open. Subpaths such as `/transfer/` are unsupported. When adding an Agent, choose a Master URL that the Agent can reach; it may use a private IP while the browser uses the proxy. For TLS certificates and HTTPS Master URLs, see [security](security.md).
