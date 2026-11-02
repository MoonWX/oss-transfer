# Offline installation

[中文](offline-install.zh-CN.md)

Prepare the files on a connected machine and copy them to the Master. Other Agents download their packages from the Master; they need access to it and the storage endpoints used by their tasks.

## 1. Prepare the files

Run this in Bash on a connected machine. It downloads the latest stable release for a Master and Agents all running Linux amd64 (x86-64):

```bash
(
set -euo pipefail
base=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer
version=$(curl -fsSL "$base/latest/manifest.tsv" | awk -F '\t' '$1 == "version" {print "v" $2}')
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || exit 1
archives=(oss-transfer-linux-amd64.tar.gz)
bundle="oss-transfer-$version-offline"
base="$base/$version"
mkdir -p "$bundle"
for file in install.sh native-install.sh Install.ps1 manifest.tsv "${archives[@]}"; do
  curl -fL --retry 2 "$base/$file" -o "$bundle/$file" || exit 1
done
)
```

If the hosts use different platforms, include their package names in `archives=(...)`, for example `archives=(oss-transfer-linux-amd64.tar.gz oss-transfer-linux-arm64.tar.gz)`. Download each platform once.

| Target | Archive |
| --- | --- |
| Linux arm64 | `oss-transfer-linux-arm64.tar.gz` |
| macOS | `oss-transfer-darwin-amd64.tar.gz` or `oss-transfer-darwin-arm64.tar.gz` |
| FreeBSD | `oss-transfer-freebsd-amd64.tar.gz` or `oss-transfer-freebsd-arm64.tar.gz` |
| Windows | `oss-transfer-windows-amd64.zip` or `oss-transfer-windows-arm64.zip` |

Apple Silicon uses `arm64`. Linux also has `armv7`, `riscv64` and `loong64` packages with the same naming pattern.

## 2. Copy and install the Master

Copy the whole folder to the offline host. Keep the archives compressed. On Linux, open a terminal in that folder and run:

```bash
sudo bash install.sh --role master --package-dir "$PWD"
```

Choose whether to install an Agent on the Master host; it joins automatically. Open `http://<master-ip>:1685`, sign in as `admin` with the password printed by the installer, and choose a new password.

macOS and FreeBSD use the same command. For a macOS user service, omit `sudo` and add `--user`; keep that option when upgrading. On FreeBSD, run as root if `sudo` is unavailable. On Windows, open Administrator PowerShell in the copied folder:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -PackageDir $PWD.Path
```

## 3. Add Agents

Open **Execution nodes → Add node**, name the node and enter a Master address it can reach. Generate the invitation, then copy the command for its operating system and run it on the Agent host.

The command downloads the installer and package from the Master, starts the Agent and joins the cluster. Once the node appears online, you can select it for a transfer.

An online Master can fetch packages for other platforms as needed. Choose **Download from public sources** in the invitation dialog to let the Agent download from the public mirrors instead.

## Upgrade and reinstall

Prepare a new version folder using step 1 and copy it to each host being upgraded. Stop active tasks, update the Master, then update Agents one at a time. Run the command for the host's role from the new folder:

```bash
sudo bash native-install.sh --role master --upgrade --package-dir "$PWD" --yes
# On each Agent host:
sudo bash native-install.sh --role agent --upgrade --package-dir "$PWD" --yes
```

On Windows, add `-Upgrade` to the installation command and use `-Role agent` for Agents. Upgrades keep configuration, tasks and node identities. Check that all nodes are online at the new version before resuming tasks.

To reinstall after removing the programs, keep the original configuration and entire data directory, including the `.db.key` file. Rerun the installer with the original paths if you customized them; see [reinstallation](install.md#reinstall-without-losing-data).

## Things to check

- Prepare Bash, curl, tar and a SHA-256 tool on Unix hosts; Linux uses systemd. On FreeBSD, run `pkg install -y bash curl` before disconnecting. Run installation commands as root if `sudo` is unavailable.
- Keep all packages from the same release. If an offline Master is missing an Agent platform, add that package to the original installation folder and run the Master upgrade command above to import it. This restarts the Master.
- Invitations expire and work once. Generate a new one if needed.
- Successful upgrades delete their temporary backup. Failed upgrades attempt recovery and retain the backup. Make a separate backup before a planned downgrade; see [backup and recovery](operations.md#update-installed-services).
- This guide installs native services. Offline Docker deployment also requires the container images to be prepared in advance.
