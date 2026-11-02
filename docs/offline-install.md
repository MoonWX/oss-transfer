# Offline installation

[中文](offline-install.zh-CN.md)

Download one release on a connected machine, then copy it to the offline Master. Agents still need access to the Master and the storage endpoints used by their tasks.

## 1. Prepare the files

Run this in Bash on a connected machine. It downloads the latest release for the **target host**; this example uses Linux x86-64:

```bash
(
base=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer
version=$(curl -fsSL "$base/latest/manifest.tsv" | awk -F '\t' '$1 == "version" {print "v" $2}')
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || exit 1
archive=oss-transfer-linux-amd64.tar.gz
bundle="oss-transfer-$version-offline"
base="$base/$version"
mkdir -p "$bundle"
for file in install.sh native-install.sh Install.ps1 manifest.tsv "$archive"; do
  curl -fL --retry 2 "$base/$file" -o "$bundle/$file" || exit 1
done
)
```

For another platform, change `archive`:

| Target | Archive |
| --- | --- |
| Linux arm64 | `oss-transfer-linux-arm64.tar.gz` |
| macOS | `oss-transfer-darwin-amd64.tar.gz` or `oss-transfer-darwin-arm64.tar.gz` |
| FreeBSD | `oss-transfer-freebsd-amd64.tar.gz` or `oss-transfer-freebsd-arm64.tar.gz` |
| Windows | `oss-transfer-windows-amd64.zip` or `oss-transfer-windows-arm64.zip` |

`amd64` means x86-64; Apple Silicon uses `arm64`. Linux also has `armv7`, `riscv64` and `loong64` archives. Download additional platform archives from the same version directory if your hosts differ.

## 2. Copy and install the Master

Copy the whole folder to the offline host. Keep the archives compressed. On Linux, open a terminal in that folder and run:

```bash
sudo bash install.sh --role master --package-dir "$PWD"
```

Open `http://<master-ip>:1685`, sign in as `admin` with the password printed by the installer, and choose a new password.

macOS and FreeBSD use the same command. For a macOS user service, omit `sudo` and add `--user`; keep that option when upgrading. On FreeBSD, run as root if `sudo` is unavailable. On Windows, open Administrator PowerShell in the copied folder:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -PackageDir $PWD.Path -Yes
```

## 3. Add Agents

During Master installation, you can also install a local Agent; it joins automatically. For other hosts, open **Execution nodes → Add node**, enter a Master address reachable from the Agent, and run the generated command on that host.

Agents download their installer and package from the Master. If the Master is offline, include the archives for every Agent platform in the folder from step 1 before installing it. The installer keeps those packages in the Master’s `data/packages` directory.

An online Master can fetch matching packages for other platforms as needed. Choose **Download from public sources** in the invitation dialog to use the original download path. Docker still uses the container registry.

## 4. Upgrade

Prepare and copy a new version folder using step 1. Stop active tasks, update the Master, then update Agents one at a time. Run the command for the host's role from the new folder:

```bash
sudo bash native-install.sh --role master --upgrade --package-dir "$PWD" --yes
# On each Agent host:
sudo bash native-install.sh --role agent --upgrade --package-dir "$PWD" --yes
```

Windows uses `-Upgrade` with `-Role master` or `-Role agent` and `-PackageDir`. Upgrades retain configuration, task data and Agent identities; no new invitation is needed. Check that each service is available at the new version before resuming tasks. See [updates](operations.md#update-installed-services) for backup and recovery behavior.

## Things to check

- Keep all files from the same fixed release. The installer verifies package checksums; an extracted binary folder is not a valid `--package-dir`.
- Unix hosts need Bash, tar and a SHA-256 tool; Linux also needs systemd. Install missing prerequisites before disconnecting.
- Invitations expire and work once. New invitations can reuse the packages held by the Master. Prepare new release files before an offline update.
