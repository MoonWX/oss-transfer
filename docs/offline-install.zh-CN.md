# 离线安装

[English](offline-install.md)

在联网机器上下载一个版本的安装文件，再拷贝到离线主机。Agent 仍需能访问 Master，以及任务使用的对象存储地址。

## 1. 准备安装文件

在联网机器的 Bash 中执行以下命令。选定发布版本和与**目标主机**匹配的压缩包，下面以 Linux x86-64 为例：

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

其他平台修改 `archive` 即可：

| 目标主机 | 压缩包 |
| --- | --- |
| Linux arm64 | `oss-transfer-linux-arm64.tar.gz` |
| macOS | `oss-transfer-darwin-amd64.tar.gz` 或 `oss-transfer-darwin-arm64.tar.gz` |
| FreeBSD | `oss-transfer-freebsd-amd64.tar.gz` 或 `oss-transfer-freebsd-arm64.tar.gz` |
| Windows | `oss-transfer-windows-amd64.zip` 或 `oss-transfer-windows-arm64.zip` |

`amd64` 对应 x86-64，Apple Silicon 使用 `arm64`。Linux 另有 `armv7`、`riscv64` 和 `loong64` 压缩包。如果各台主机的平台不同，从同一版本目录补充下载所需压缩包。

## 2. 拷贝并安装 Master

将整个文件夹拷贝到离线主机，压缩包无需解压。Linux 在该文件夹中打开终端并执行：

```bash
sudo bash native-install.sh --role master --package-dir "$PWD" --yes
```

打开 `http://<master-ip>:1685`，使用 `admin` 和安装器输出的初始密码登录，然后设置自己的密码。

macOS 和 FreeBSD 使用同样的命令。macOS 安装当前用户服务时，去掉 `sudo` 并加上 `--user`，升级时也要保留该参数。FreeBSD 没有 `sudo` 时，切换到 root 执行。Windows 在拷贝后的文件夹中打开管理员 PowerShell：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -PackageDir $PWD.Path -Yes
```

## 3. 添加 Agent

打开**执行节点 → 添加节点**，填写 Agent 能访问的 Master 地址并生成加入指令。在 **Linux / macOS / FreeBSD** 页签中，只复制以 `/api/agents/bootstrap/<token>` 结尾的 URL，不要带引号。

在 Linux Agent 主机上进入拷贝后的文件夹。先执行 `read -r join_url`，粘贴 URL 并回车，然后执行余下命令：

```bash
read -r join_url
sudo bash install.sh --role agent --method native \
  --package-dir "$PWD" --join-url "$join_url" --yes
unset join_url
```

macOS 或 FreeBSD 按第 2 步调整权限和 `--user`。Windows 在管理员 PowerShell 中执行，粘贴同一个邀请 URL：

```powershell
$join = (Read-Host 'Agent invitation URL') -split '/api/agents/bootstrap/', 2
$env:TRANSFER_JOIN_TOKEN = $join[1]
try {
    powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role agent -MasterUrl $join[0] -PackageDir $PWD.Path -Yes
} finally {
    Remove-Item Env:TRANSFER_JOIN_TOKEN
}
```

确认 Agent 在控制台中显示在线。每台 Agent 单独生成邀请；如果 Master 所在主机也要传输数据，在同一主机上再安装一个 Agent。

## 4. 升级

按第 1 步准备并拷贝新版本文件夹。停止正在运行的任务，先升级 Master，再逐台升级 Agent。在新文件夹中，根据主机上的角色执行对应命令：

```bash
sudo bash native-install.sh --role master --upgrade --package-dir "$PWD" --yes
# 在各台 Agent 主机上执行：
sudo bash native-install.sh --role agent --upgrade --package-dir "$PWD" --yes
```

Windows 使用 `-Upgrade`，并指定 `-Role master` 或 `-Role agent` 及 `-PackageDir`。升级保留配置、任务数据和 Agent 身份，无需重新邀请。确认各服务已按新版本恢复可用，再继续任务。备份和恢复行为见[版本更新说明](operations.md#update-installed-services)。

## 注意事项

- 所有文件必须来自同一个固定版本。安装器会校验压缩包，不能把解压后的二进制目录当作 `--package-dir`。
- Unix 主机需要 Bash、tar 和 SHA-256 工具，Linux 还需要 systemd。缺少的工具应提前安装。
- 邀请有有效期且只能使用一次。控制台中的普通加入指令和版本更新按钮会下载发布文件；无法下载时，使用本文的本地命令。
