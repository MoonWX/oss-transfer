# Install on other systems

[中文](install.zh-CN.md)

For Linux with systemd 219 or later (CentOS 7 / RHEL 7 and newer), use the [installation command](../README.md#1-install-the-master). The steps below cover other systems and custom paths. For hosts without internet access, use [offline installation](offline-install.md).

## CentOS 7 / RHEL 7

These are the oldest supported Linux releases. Install with the same command as on other Linux systems: the installers and online updates work with the systemd 219, Bash 4.2 and curl 7.29 they ship. Older systemd changes two details of online updates:

- Before systemd 236, a failed update leaves its `oss-transfer-update-*` helper unit listed as failed until you run `systemctl reset-failed`.
- Before systemd 240, an update helper that cannot start is not reported at once; the update fails only when it times out.

## macOS

Install a service for the current user:

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
bash install.sh --role master --user
```

For a system service, use `sudo bash install.sh --role master` instead. The installer prints the service name and paths.

## Windows

Run in Administrator PowerShell:

```powershell
Invoke-WebRequest https://raw.githubusercontent.com/MoonWX/oss-transfer/main/deploy/native/Install.ps1 -OutFile Install.ps1
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master
```

In mainland China, the script is also available at `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/Install.ps1`.

## FreeBSD

Run as root:

```sh
pkg install -y bash curl
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
bash install.sh --role master
```

For all systems, sign in with the `admin` password printed by the installer. The native Master installer offers to install a local Agent too. Add other Agents from **Execution nodes → Add node**.

## Download sources

The installers download from GitHub, or from the JD Cloud OSS mirror when the host's public IP is in mainland China, and try the other source if the first cannot be reached. Pass `--source github` or `--source jdoss` (`-Source` for `Install.ps1`) to choose one. On the mirror, `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/` always holds every file of the latest stable release, including `install.sh` and `Install.ps1`; each version, prereleases included, also has its own directory, such as `v4.1.36/`.

An installer saved as a file (not piped into the shell) looks for `manifest.tsv` or this host's platform archive in its own folder, then in the current directory. If either is there, it installs from the files in that folder without downloading anything, and `--version`, if given, must match them; `--source github` or `--source jdoss` (`-Source` for `Install.ps1`) forces a download instead. See [offline installation](offline-install.md).

## Install a specific or prerelease version

Installers use the latest stable release by default. To install a fixed version, including a prerelease such as `4.1.29-beta.1`, pass it explicitly:

```sh
bash install.sh --role master --version 4.1.29-beta.1
```

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -Version 4.1.29-beta.1
```

The installer verifies a prerelease exactly like a stable release: the GitHub release digest for that tag or the versioned OSS manifest, never `latest`. Rerunning the installer with `--upgrade` and `--version` upgrades an existing installation to that version, including from a prerelease to its stable release. Agents added from the console download the Master's own version.

For Docker Compose, set the exact image tag in `.env`; `latest` and `X.Y` only follow stable releases:

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:4.1.29-beta.1
```

Return to a stable image by setting the stable tag (or `latest`) once it is newer than the prerelease. Prereleases are experimental; see [trying a prerelease](operations.md#try-a-prerelease) for online updates and returning to stable.

## Paths and ports

New native installations keep their files under one root. Linux defaults to `/opt/oss-transfer`, macOS to `/Library/Application Support/OSS Transfer` (the user Library for `--user`), FreeBSD to `/usr/local/lib/oss-transfer`, and Windows to `%ProgramData%\OSS Transfer`. Set `--root-dir` or `-RootDir` to use another disk. OS service registration remains outside this directory; Unix also keeps a global CLI entry point.

Default Linux locations:

| | Master | Agent |
| --- | --- | --- |
| Service | `oss-transfer-master` | `oss-transfer-agent` |
| Programs | `/opt/oss-transfer/master/bin` | `/opt/oss-transfer/agent/bin` |
| Configuration | `/opt/oss-transfer/master/config/service.json` | `/opt/oss-transfer/agent/config/service.json` |
| Data | `/opt/oss-transfer/master/data` | `/opt/oss-transfer/agent/data` |
| Upgrade backups | `/opt/oss-transfer/master/backups` | `/opt/oss-transfer/agent/backups` |

The default Master port is `1685`. Change it with `--port 1785` in `install.sh`, or `-Listen '0.0.0.0:1785'` in `Install.ps1`. Use `--install-dir`, `--config-dir` or `--data-dir` for custom locations; these directories must not contain one another. Run `bash install.sh --help` for the options, or `Get-Help .\Install.ps1` on Windows. Upgrades reuse the installed paths.

On Linux, check a service and its log with:

```sh
sudo systemctl status oss-transfer-master
sudo tail -n 50 /opt/oss-transfer/master/data/logs/service.log
```

Replace `master` with `agent` to inspect an Agent. Other systems print their log paths during installation.

## Reinstall without losing data

Run the installer again, using the same `--root-dir` if you changed it. It recognizes retained service configuration and data, restores missing programs or service definitions, and reuses the Agent identity. Existing installations in the older layout keep their saved paths.

Keep each role’s `config` and entire `data` directory, including its key file (`master.db.key` or `agent.db.key`). The key must match the database. Without the configuration or key, the installer stops instead of creating an empty replacement. Source files, destination files and shared mounts are separate from these application directories.

Use `--with-agent` or `--without-agent` for an unattended native Master installation. Windows accepts `-WithAgent` or `-WithoutAgent`. An existing local Agent is reused only if it belongs to that Master.

## Reverse proxy

Serve the application at the domain root, for example `https://transfer.example.com/`. Add this location to the domain's NGINX server block:

```nginx
location / {
    proxy_pass http://127.0.0.1:1685;
    proxy_set_header Host $http_host;
    proxy_read_timeout 60s;
}
```

Subpaths such as `/transfer/` are unsupported. When adding an Agent, choose a Master URL that the Agent can reach; it may use a private IP while the browser uses the proxy.
