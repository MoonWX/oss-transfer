# Offline installation

[中文](offline-install.zh-CN.md)

Download one release on a connected machine, then copy it to the offline hosts. Agents still need access to the Master and the storage endpoints used by their tasks.

## 1. Prepare the files

Run this in Bash on a connected machine. Choose a fixed release and the archive for the **target host**; this example uses Linux x86-64:

```bash
(
version=v4.1.21
archive=oss-transfer-linux-amd64.tar.gz
bundle="oss-transfer-$version-offline"
base="https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/$version"
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
sudo bash native-install.sh --role master --package-dir "$PWD" --yes
```

Open `http://<master-ip>:1685`, sign in as `admin` with the password printed by the installer, and choose a new password.

macOS and FreeBSD use the same command. For a macOS user service, omit `sudo` and add `--user`; keep that option when upgrading. On FreeBSD, run as root if `sudo` is unavailable. On Windows, open Administrator PowerShell in the copied folder:

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -PackageDir $PWD.Path -Yes
```

## 3. Add an Agent

Open **Execution nodes → Add node** and enter a Master address the Agent can reach. Generate the join command. From its **Linux / macOS / FreeBSD** tab, copy just the URL ending in `/api/agents/bootstrap/<token>`, without quotes.

On the Linux Agent host, open the copied folder. Run `read -r join_url`, paste the URL and press Enter, then run the remaining commands:

```bash
read -r join_url
sudo bash install.sh --role agent --method native \
  --package-dir "$PWD" --join-url "$join_url" --yes
unset join_url
```

For macOS or FreeBSD, use the same permission and `--user` adjustments as step 2. In Administrator PowerShell on Windows, paste the same invitation URL:

```powershell
$join = (Read-Host 'Agent invitation URL') -split '/api/agents/bootstrap/', 2
$env:TRANSFER_JOIN_TOKEN = $join[1]
try {
    powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role agent -MasterUrl $join[0] -PackageDir $PWD.Path -Yes
} finally {
    Remove-Item Env:TRANSFER_JOIN_TOKEN
}
```

Confirm that the Agent appears online. Each Agent needs its own invitation. Install an Agent on the Master host too if that machine will transfer data.

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
- Invitations expire and work once. The console's usual join command and update buttons download release files; use the local commands above when downloads are unavailable.
