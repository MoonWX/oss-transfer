# Offline installation

[中文](offline-install.zh-CN.md)

Prepare the files on a connected machine and copy them to the Master. Other Agents (execution nodes) download their packages from the Master; they need access to it and to the storage endpoints used by their tasks. [Planning](planning.md) lists the connections each host needs.

## 1. Prepare the files

On a connected machine, download the release files into one folder. Two sources hold the same complete set:

- JD Cloud OSS, a fixed URL for the latest stable release: `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/<file>`. For a specific version, replace `latest` with the tag, for example `v4.1.42`.
- The assets of a [GitHub Release](https://github.com/MoonWX/oss-transfer/releases).

Download `install.sh`, `native-install.sh`, `manifest.tsv`, `Install.ps1` for Windows, and the archive of each platform you install. For example, for a Master and Agents all running Linux amd64 (x86-64):

```bash
(
set -euo pipefail
base=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest
mkdir -p oss-transfer-offline
for file in install.sh native-install.sh Install.ps1 manifest.tsv oss-transfer-linux-amd64.tar.gz; do
  curl -fL --retry 2 "$base/$file" -o "oss-transfer-offline/$file"
done
)
```

Add an archive for each other platform you use. Download each platform once.

| Target | Archive |
| --- | --- |
| Linux amd64 | `oss-transfer-linux-amd64.tar.gz` |
| Linux arm64 | `oss-transfer-linux-arm64.tar.gz` |
| macOS | `oss-transfer-darwin-amd64.tar.gz` or `oss-transfer-darwin-arm64.tar.gz` |
| FreeBSD | `oss-transfer-freebsd-amd64.tar.gz` or `oss-transfer-freebsd-arm64.tar.gz` |
| Windows | `oss-transfer-windows-amd64.zip` or `oss-transfer-windows-arm64.zip` |

Apple Silicon uses `arm64`. Linux also has `armv7`, `riscv64` and `loong64` packages with the same naming pattern.

If you only have this host's archive and the installer, for example from an older GitHub Release, the installer still works offline: it verifies the archive with the checksums inside it and builds `manifest.tsv` locally (`install.sh` carries its own copy of `native-install.sh` for this case). The complete set is still preferred, because `manifest.tsv` also verifies the archive file itself and lets the Master serve Agents on other platforms. From an archive alone, the Master can serve Agent packages only for its own platform; copy `install.sh`, `native-install.sh` and `Install.ps1` along with the archive so that it can.

## 2. Copy and install the Master

Copy the whole folder to the offline host. Keep the archives compressed. On Linux, open a terminal in that folder and run:

```bash
sudo bash install.sh --role master
```

The installer uses the release files in its own folder (or the current folder), prints `Using local release files (offline): <folder>` and does not access the network. If you pass `--version`, it must match the version of these files. Use `--package-dir DIR` to choose another folder, or `--source github|jdoss` to download instead.

Choose whether to install an Agent on the Master host; it joins automatically. The installer ends with `=== master 4.1.42 · Installed successfully ===`, the web console address and the initial password. Open `http://<master-ip>:1685`, sign in as `admin` with that password, and choose a new password of at least 8 characters. See [check the installation](install.md#check-the-installation) for what else to check.

macOS and FreeBSD use the same command. For a macOS user service, omit `sudo` and add `--user`; keep that option when upgrading. On FreeBSD, run as root if `sudo` is unavailable. On Windows, open Administrator PowerShell in the copied folder:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master
```

## 3. Add Agents

Open **Execution nodes → Add node**, name the node and enter a Master address it can reach. Choose **Generate join commands**, then copy the command for its operating system and run it on the Agent host. On Linux, macOS and FreeBSD it has the form `curl -fsSL '<link>' | bash` (run as root or as a user with `sudo`), and the dialog shows *Download source: this Master. The new node needs no internet access.*

The command downloads the installer and package from the Master, starts the Agent and joins the cluster. Once the node appears online on **Execution nodes**, you can select it for a transfer. For Docker Agents, load the image on the host first (see the last point below).

An online Master can fetch packages for other platforms as needed. Choose **Download from public sources** in the invitation dialog to let the Agent download from the public mirrors instead.

## Upgrade and reinstall

Online updates under **Software updates** download from GitHub or the OSS mirror, so upgrade offline hosts with the installer. Prepare a new version folder using step 1 and copy it to each host being upgraded. Stop active tasks, update the Master, then update Agents one at a time. Run the command for the host's role from the new folder. `native-install.sh` is the native installer itself; `install.sh` adds the interactive questions and Docker Compose, and runs the same native installer.

```bash
sudo bash native-install.sh --role master --upgrade --yes
# On each Agent host:
sudo bash native-install.sh --role agent --upgrade --yes
```

Installers from older releases do not detect the folder automatically; add `--package-dir "$PWD"` (Windows: `-PackageDir $PWD.Path`) for them. On Windows, add `-Upgrade` to the installation command and use `-Role agent` for Agents. Upgrades keep configuration, tasks and node identities. Check that all nodes are online at the new version before resuming tasks; several v4.1.42 features need the Agents at v4.1.42 ([upgrading to v4.1.42](operations.md#upgrading-to-v420)). Rerunning the same version on a healthy installation changes nothing unless you add `--force`.

To reinstall after removing the programs, keep the original configuration and entire data directory, including the `.db.key` file. Rerun the installer with the original paths if you customized them; see [reinstallation](install.md#reinstall-without-losing-data). Uninstalling works offline too: `sudo bash install.sh --uninstall --role agent` downloads nothing ([uninstall](install.md#uninstall)).

## Things to check

- Prepare Bash, curl, tar and a SHA-256 tool on Unix hosts. Linux needs systemd 219, Bash 4.2 and curl 7.29 or later. On FreeBSD, run `pkg install -y bash curl` before disconnecting. Run installation commands as root if `sudo` is unavailable.
- Keep all packages from the same release. If an offline Master is missing an Agent platform, add that package to the original installation folder and run the Master upgrade command above to import it. This restarts the Master.
- Invitations expire and work once. Generate a new one if needed.
- Successful upgrades delete their temporary backup. Failed upgrades attempt recovery and retain the backup. Make a separate backup before a planned downgrade; see [backup and restore](operations.md#backup-and-restore).
- This guide installs native services. Offline Docker deployment also requires the container image `moonwx/osstransfer` to be prepared in advance, for example with `docker save` on a connected machine and `docker load` on the offline host; see [Docker Compose](docker-compose.md).
