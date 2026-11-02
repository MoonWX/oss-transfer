# 离线安装

[English](offline-install.md)

在联网机器上准备安装文件，拷贝到 Master。其他 Agent 从 Master 拉取安装包；它们需要能访问 Master 和任务使用的存储端点。

## 1. 准备安装文件

在联网机器的 Bash 中执行。下面下载最新正式版，适用于 Master 和 Agent 都是 Linux amd64（x86-64）的情况：

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

如果 Master 和 Agent 的平台不同，把所需包名一起写入 `archives=(...)`，例如 `archives=(oss-transfer-linux-amd64.tar.gz oss-transfer-linux-arm64.tar.gz)`。每个平台只需下载一份。

| 目标主机 | 压缩包 |
| --- | --- |
| Linux arm64 | `oss-transfer-linux-arm64.tar.gz` |
| macOS | `oss-transfer-darwin-amd64.tar.gz` 或 `oss-transfer-darwin-arm64.tar.gz` |
| FreeBSD | `oss-transfer-freebsd-amd64.tar.gz` 或 `oss-transfer-freebsd-arm64.tar.gz` |
| Windows | `oss-transfer-windows-amd64.zip` 或 `oss-transfer-windows-arm64.zip` |

Apple Silicon 使用 `arm64`。Linux 另有 `armv7`、`riscv64` 和 `loong64` 包，命名方式相同。

## 2. 拷贝并安装 Master

将整个文件夹拷贝到离线主机，压缩包无需解压。Linux 在该文件夹中打开终端并执行：

```bash
sudo bash install.sh --role master --package-dir "$PWD"
```

按提示选择是否同时安装本机 Agent；选择安装后，它会自动加入集群。打开 `http://<master-ip>:1685`，使用 `admin` 和安装器输出的初始密码登录，再设置自己的密码。

macOS 和 FreeBSD 使用同样的命令。macOS 安装当前用户服务时，去掉 `sudo` 并加上 `--user`，升级时也要保留该参数。FreeBSD 没有 `sudo` 时，切换到 root 执行。Windows 在拷贝后的文件夹中打开管理员 PowerShell：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -PackageDir $PWD.Path
```

## 3. 添加 Agent

打开 **执行节点 → 添加节点**，填写节点名称和 Agent 能访问的 Master 地址。生成邀请后，复制对应系统的命令，在 Agent 主机上执行。

命令会从 Master 获取安装器和程序包，启动 Agent 并加入集群。确认节点显示在线后，即可用它创建迁移任务。

Master 能联网时会按需补齐其他平台的包。需要让 Agent 自行从公网下载时，在加入窗口点击“改用公网下载”。

## 升级和重装

按第 1 步准备新版本文件夹，并拷贝到需要升级的各台主机。停止正在运行的任务，先升级 Master，再逐台升级 Agent。在新文件夹中执行对应命令：

```bash
sudo bash native-install.sh --role master --upgrade --package-dir "$PWD" --yes
# 在各台 Agent 主机上执行：
sudo bash native-install.sh --role agent --upgrade --package-dir "$PWD" --yes
```

Windows 在原安装命令上加 `-Upgrade`，Agent 使用 `-Role agent`。升级保留原配置、任务和节点身份。确认各节点都已在线且版本一致后，再继续任务。

删除程序后重装，要保留原配置和完整数据目录，包括 `.db.key` 文件。重新运行安装命令即可；使用过自定义目录时，填写原路径。具体见[重装说明](install.md#reinstall-without-losing-data)。

## 注意事项

- Unix 主机提前装好 Bash、curl、tar 和 SHA-256 工具；Linux 使用 systemd。FreeBSD 可在断网前执行 `pkg install -y bash curl`。没有 `sudo` 时，以 root 运行安装命令。
- 所有包必须来自同一版本。Master 离线时，缺少的 Agent 平台包需补到原安装文件夹，再按上面的 Master 升级命令导入；这会重启 Master。
- 邀请有有效期且只能使用一次。过期后重新生成即可。
- 升级成功会删除本次临时备份，失败时会尝试恢复并保留备份。计划降级时，应提前另做备份，见[备份与恢复](operations.md#update-installed-services)。
- 本文安装的是原生服务。离线 Docker 部署还需要提前准备容器镜像。
