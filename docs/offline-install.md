# Offline installation

[中文](offline-install.zh-CN.md)

When the target hosts cannot reach the public internet, download the release files on a connected machine and copy them to the target hosts before installing. This page describes the full process: preparing the files, installing the Master, adding Agents (execution nodes), and upgrading or reinstalling afterwards. For the network requirements between hosts, see [Planning](planning.md). Agents need access to the Master and to the storage endpoints used by their tasks.

## 1. Prepare the files

On a connected machine, download the release files into one folder. Two sources carry identical content. One is JD Cloud OSS, with a fixed URL for the latest stable release: `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/<file>`. To download a specific version, replace `latest` with `v<version>`. The other is the assets attached to a [GitHub Release](https://github.com/MoonWX/oss-transfer/releases).

Download the following files: `install.sh`, `native-install.sh`, `manifest.tsv`, `Install.ps1` for Windows, and the archive for each target platform. For example, when both the Master and the Agents run Linux amd64 (x86-64):

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

If you have machines on other platforms, download their archives as well. Each platform needs only one copy.

| Target | Archive |
| --- | --- |
| Linux amd64 | `oss-transfer-linux-amd64.tar.gz` |
| Linux arm64 | `oss-transfer-linux-arm64.tar.gz` |
| macOS | `oss-transfer-darwin-amd64.tar.gz` or `oss-transfer-darwin-arm64.tar.gz` |
| FreeBSD | `oss-transfer-freebsd-amd64.tar.gz` or `oss-transfer-freebsd-arm64.tar.gz` |
| Windows | `oss-transfer-windows-amd64.zip` or `oss-transfer-windows-arm64.zip` |

Apple Silicon corresponds to `arm64`. Linux also has `armv7`, `riscv64` and `loong64` packages with the same naming pattern.

If you have only this platform's archive and the install scripts, for example from an older GitHub Release, offline installation still works. The installer verifies the archive with the checksums embedded in it and generates `manifest.tsv` locally. `install.sh` carries its own copy of `native-install.sh` for this scenario.

The complete set is still recommended. `manifest.tsv` also verifies the archive files themselves, and it lets the Master serve packages to Agents on other platforms. With only the archive, the Master can serve Agent packages only for its own platform. To enable that, copy `install.sh`, `native-install.sh` and `Install.ps1` alongside the archive.

## 2. Copy and install the Master

Copy the whole folder to the offline host. The archives do not need to be extracted. On Linux, open a terminal in that folder and run:

```bash
sudo bash install.sh --role master
```

When the installer finds release files in its own directory (or the current directory), it uses them directly, prints `Using local release files (offline): <folder>`, and does not access the network. If you pass `--version`, it must match the version of the files in the folder. Use `--package-dir DIR` to point to a different folder, or `--source github|jdoss` to download instead.

During installation the script asks whether to install an Agent on the same host. If you choose to install one, it joins the cluster automatically with no further configuration. The installer finishes with `=== master <version> · Installed successfully ===`, the web console address and the initial password.

Open `http://<master-ip>:1685`, sign in as `admin` with the initial password, and set a new password of at least 8 characters. For other items to verify, see [check the installation](install.md#check-the-installation).

macOS and FreeBSD use the same command. On macOS, to install as a user service, omit `sudo` and add `--user`; keep that flag when upgrading. On FreeBSD without `sudo`, run as root. On Windows, open Administrator PowerShell in the copied folder:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master
```

## 3. Add Agents

Open **Execution nodes → Add node** in the console, enter a node name and the Master address the node can reach, and choose **Generate join commands**. Copy the command for the target operating system and run it on the Agent host. On Linux, macOS and FreeBSD the command has the form `curl -fsSL '<link>' | bash`; run it as root or as a user with `sudo`. The dialog shows *Download source: this Master. The new node needs no internet access.*

The command downloads the installer and package from the Master, starts the Agent and joins the cluster. Once the node appears as online on the **Execution nodes** page, you can use it for transfers. Docker Agents require the image to be loaded on the host first; see the last note below.

When the Master is online, it fetches packages for other platforms as needed. To have the Agent download from the public internet instead, select **Public (automatic)** under **Download source** in the Add node dialog.

## Upgrade and reinstall

Offline hosts cannot use **Software updates** in the console, because it downloads from GitHub or JD Cloud OSS. The upgrade procedure is similar to the initial installation: prepare a folder with the new version following step 1 and copy it to each host that needs upgrading.

Stop running tasks before you begin. Upgrade the Master first, then each Agent one at a time. In the new folder, run:

```bash
sudo bash native-install.sh --role master --upgrade --yes
# On each Agent host:
sudo bash native-install.sh --role agent --upgrade --yes
```

The `native-install.sh` used here is the native installer itself. `install.sh` adds interactive prompts and Docker Compose support on top of it, but calls the same script internally. Installers from older releases do not detect the folder automatically; add `--package-dir "$PWD"` for those (Windows: `-PackageDir $PWD.Path`). On Windows, add `-Upgrade` to the original installation command and use `-Role agent` for Agents.

Upgrades preserve the existing configuration, tasks and node identities. After upgrading, confirm that all nodes are online and running the same version before resuming tasks. Several v4.1.42 features require the Agents to be at v4.1.42 as well; see [upgrading to v4.1.42](operations.md#upgrading-to-v4142). Running the same version again on a healthy installation changes nothing unless you add `--force`.

To reinstall after removing the software, keep the original configuration and the entire data directory, including the `.db.key` file. Rerun the installer with the original paths if you used custom directories; see [reinstall without losing data](install.md#reinstall-without-losing-data). Uninstalling works offline: `sudo bash install.sh --uninstall --role agent` does not download anything (see [uninstall](install.md#uninstall)).

## Notes

- Install Bash, curl, tar and a SHA-256 tool on Unix hosts ahead of time. Linux requires systemd 219, Bash 4.2 and curl 7.29 or later. On FreeBSD, run `pkg install -y bash curl` before disconnecting. Run the installation commands as root if `sudo` is unavailable.
- All packages must come from the same release. When an offline Master is missing a package for an Agent platform, add that archive to the original installation folder and run the Master upgrade command above to import it. This restarts the Master.
- Invitations expire and can only be used once. Generate a new one when needed.
- A successful upgrade deletes its temporary backup automatically. A failed upgrade attempts to restore the previous version and retains the backup. If you plan to downgrade, make a separate backup beforehand; see [backup and restore](operations.md#backup-and-restore).
- This page covers native service installation. Offline Docker deployment also requires the `moonwx/osstransfer` container image to be prepared in advance. Export it with `docker save` on a connected machine and import it with `docker load` on the offline host; see [Docker Compose](docker-compose.md) for details.
