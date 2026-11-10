# 离线安装

[English](offline-install.md)

在联网机器上准备安装文件，拷贝到 Master。其他执行节点（Agent）从 Master 拉取安装包；它们需要能访问 Master 和任务使用的存储端点。各主机需要的网络连接见[规划](planning.zh-CN.md)。

## 1. 准备安装文件

在联网机器上把发布文件下载到同一个文件夹。以下两个来源的文件完全相同：

- 京东云 OSS，最新正式版的固定地址：`https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/<文件名>`。需要指定版本时，把 `latest` 换成版本号，例如 `v4.1.42`。
- [GitHub Release](https://github.com/MoonWX/oss-transfer/releases) 中的附件。

需要下载 `install.sh`、`native-install.sh`、`manifest.tsv`、Windows 使用的 `Install.ps1`，以及每个目标平台的压缩包。例如 Master 和执行节点都是 Linux amd64（x86-64）时：

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

用到其他平台时，再加上对应的压缩包。每个平台只需下载一份。

| 目标主机 | 压缩包 |
| --- | --- |
| Linux amd64 | `oss-transfer-linux-amd64.tar.gz` |
| Linux arm64 | `oss-transfer-linux-arm64.tar.gz` |
| macOS | `oss-transfer-darwin-amd64.tar.gz` 或 `oss-transfer-darwin-arm64.tar.gz` |
| FreeBSD | `oss-transfer-freebsd-amd64.tar.gz` 或 `oss-transfer-freebsd-arm64.tar.gz` |
| Windows | `oss-transfer-windows-amd64.zip` 或 `oss-transfer-windows-arm64.zip` |

Apple Silicon 使用 `arm64`。Linux 另有 `armv7`、`riscv64` 和 `loong64` 包，命名方式相同。

如果手上只有本机平台的压缩包和安装脚本（例如来自较早的 GitHub Release），也可以离线安装：安装脚本会用压缩包内置的校验和验证文件，并在本地生成 `manifest.tsv`（`install.sh` 内置了一份 `native-install.sh`，专供这种情况使用）。仍建议准备完整文件，因为 `manifest.tsv` 还会校验压缩包本身，Master 也能据此为其他平台的执行节点提供安装包。只有压缩包时，Master 最多只能为本机平台的执行节点提供安装包；为此请把 `install.sh`、`native-install.sh` 和 `Install.ps1` 与压缩包一起拷贝。

## 2. 拷贝并安装 Master

将整个文件夹拷贝到离线主机，压缩包无需解压。Linux 在该文件夹中打开终端并执行：

```bash
sudo bash install.sh --role master
```

安装脚本会使用脚本所在文件夹（或当前文件夹）中的发布文件，输出 `使用本地安装包目录（离线）: <文件夹>`，且不会访问网络。如果指定了 `--version`，它必须与这些文件的版本一致。需要使用其他文件夹时加 `--package-dir DIR`；需要改为在线下载时加 `--source github|jdoss`。

按提示选择是否同时安装本机执行节点；选择安装后，它会自动加入集群。安装脚本最后显示 `=== master 4.1.42 · 安装成功 ===`、访问地址和初始密码。打开 `http://<master-ip>:1685`，使用 `admin` 和该密码登录，再设置至少 8 个字符的新密码。其他检查项见[检查安装结果](install.zh-CN.md#检查安装结果)。

macOS 和 FreeBSD 使用同样的命令。macOS 安装当前用户服务时，去掉 `sudo` 并加上 `--user`，升级时也要保留该参数。FreeBSD 没有 `sudo` 时，切换到 root 执行。Windows 在拷贝后的文件夹中打开管理员 PowerShell：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master
```

## 3. 添加执行节点

打开 **执行节点 → 添加节点**，填写节点名称和该节点能访问的 Master 地址。点击 **生成加入指令** 后，复制对应系统的命令，在执行节点主机上运行。Linux、macOS 和 FreeBSD 的命令形如 `curl -fsSL '<链接>' | sh -c 'if [ "$(id -u)" = 0 ]; then exec bash; else exec sudo bash; fi'`（以 root 或通过 `sudo` 运行），窗口中显示 *下载来源：本 Master，新节点无需访问公网。*

命令会从 Master 获取安装脚本和程序包，启动执行节点并加入集群。确认节点在 **执行节点** 页面显示为在线后，即可用它创建迁移任务。Docker 执行节点需要先在主机上导入镜像（见最后一条注意事项）。

Master 能联网时会按需补齐其他平台的包。需要让执行节点自行从公网下载时，在加入窗口点击 **改用公网下载**。

## 升级和重装

通过 **版本更新** 进行的在线更新需要从 GitHub 或京东云 OSS 镜像下载，因此离线主机要用安装脚本升级。按第 1 步准备新版本文件夹，并拷贝到需要升级的各台主机。停止正在运行的任务，先升级 Master，再逐台升级执行节点。在新文件夹中执行对应角色的命令。`native-install.sh` 就是原生安装脚本本身；`install.sh` 在它之上增加了交互提问和 Docker Compose 方式，并运行同一个原生安装脚本。

```bash
sudo bash native-install.sh --role master --upgrade --yes
# 在各台执行节点主机上执行：
sudo bash native-install.sh --role agent --upgrade --yes
```

较早版本的安装脚本不会自动识别文件夹，需要加 `--package-dir "$PWD"`（Windows 为 `-PackageDir $PWD.Path`）。Windows 在原安装命令上加 `-Upgrade`，执行节点使用 `-Role agent`。升级保留原配置、任务和节点身份。确认各节点都已在线且版本一致后，再继续任务；v4.1.42 的多项功能要求执行节点也升级到 v4.1.42（见[升级到 v4.1.42](operations.zh-CN.md#升级到-v420)）。对运行正常的安装重复安装相同版本时，不加 `--force` 不会做任何更改。

删除程序后重装，要保留原配置和完整数据目录，包括 `.db.key` 文件。重新运行安装命令即可；使用过自定义目录时，填写原路径。具体见[重装说明](install.zh-CN.md#重装且不丢失数据)。卸载同样可以离线进行：`sudo bash install.sh --uninstall --role agent` 不下载任何内容（见[卸载](install.zh-CN.md#卸载)）。

## 注意事项

- Unix 主机提前装好 Bash、curl、tar 和 SHA-256 工具。Linux 需要 systemd 219 及以上版本；CentOS 7 / RHEL 7 使用系统自带的 systemd 219、Bash 4.2 和 curl 7.29 即可。FreeBSD 可在断网前执行 `pkg install -y bash curl`。没有 `sudo` 时，以 root 运行安装命令。
- 所有包必须来自同一版本。Master 离线时，缺少的执行节点平台包需补到原安装文件夹，再按上面的 Master 升级命令导入；这会重启 Master。
- 邀请有有效期且只能使用一次。过期后重新生成即可。
- 升级成功会删除本次临时备份，失败时会尝试恢复并保留备份。计划降级时，应提前另做备份，见[备份与恢复](operations.zh-CN.md#备份与恢复)。
- 本文安装的是原生服务。离线 Docker 部署还需要提前准备容器镜像 `moonwx/osstransfer`，例如在联网机器上用 `docker save` 导出，在离线主机上用 `docker load` 导入，见 [Docker Compose](docker-compose.zh-CN.md)。
