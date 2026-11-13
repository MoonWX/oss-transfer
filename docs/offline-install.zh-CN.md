# 离线安装

[English](offline-install.md)

如果目标主机无法访问公网，需要在一台联网的机器上下载安装文件，拷贝到目标主机后再安装。本页完整说明这一流程：准备文件、安装 Master、添加执行节点（Agent），以及后续的升级和重装。各主机之间的网络要求见[规划](planning.zh-CN.md)。执行节点需要能访问 Master，也需要能访问任务中用到的存储端点。

## 1. 准备安装文件

在一台联网的机器上，将发布文件下载到同一个文件夹。下载地址有两个，内容完全一样。一个是京东云 OSS，最新正式版的固定地址：`https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/<文件名>`，下载特定版本时将 `latest` 替换为 `v<版本号>`。另一个是 [GitHub Release](https://github.com/MoonWX/oss-transfer/releases) 中的附件。

需要下载以下文件：`install.sh`、`native-install.sh`、`manifest.tsv`、Windows 使用的 `Install.ps1`，以及每个目标平台的压缩包。例如 Master 和执行节点均为 Linux amd64（x86-64）时：

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

如果还有其他平台的机器，把对应的压缩包也一起下载。每个平台只需要一份。

| 目标主机 | 压缩包 |
| --- | --- |
| Linux amd64 | `oss-transfer-linux-amd64.tar.gz` |
| Linux arm64 | `oss-transfer-linux-arm64.tar.gz` |
| macOS | `oss-transfer-darwin-amd64.tar.gz` 或 `oss-transfer-darwin-arm64.tar.gz` |
| FreeBSD | `oss-transfer-freebsd-amd64.tar.gz` 或 `oss-transfer-freebsd-arm64.tar.gz` |
| Windows | `oss-transfer-windows-amd64.zip` 或 `oss-transfer-windows-arm64.zip` |

Apple Silicon 对应 `arm64`。Linux 还有 `armv7`、`riscv64` 和 `loong64` 的包，命名方式一样。

如果只有本机平台的压缩包和安装脚本，例如从较早的 GitHub Release 下载的，也可以离线安装。安装脚本会用压缩包内置的校验和验证文件，并在本地生成 `manifest.tsv`。`install.sh` 内置了一份 `native-install.sh`，用于这种场景。

仍建议准备完整文件。`manifest.tsv` 额外校验压缩包本身，Master 也能根据它为其他平台的执行节点提供安装包。如果只有压缩包，Master 最多只能为本机平台的执行节点提供安装包；为此需要将 `install.sh`、`native-install.sh` 和 `Install.ps1` 与压缩包一起拷贝。

## 2. 拷贝并安装 Master

将整个文件夹拷贝到离线主机，压缩包无需解压。Linux 在该文件夹中打开终端，执行：

```bash
sudo bash install.sh --role master
```

安装脚本检测到脚本所在文件夹（或当前文件夹）中有发布文件时，会直接使用它们，输出 `使用本地安装包目录（离线）: <文件夹>`，不访问网络。如果指定了 `--version`，该版本必须与这些文件的版本一致。需要使用其他文件夹时加 `--package-dir DIR`；需要改为在线下载时加 `--source github|jdoss`。

安装过程中脚本会询问是否同时安装本机执行节点。如果选择安装，该节点会自动加入集群，无需额外配置。安装结束时输出 `=== master <版本号> · 安装成功 ===`，以及访问地址和初始密码。

打开 `http://<master-ip>:1685`，用 `admin` 和初始密码登录，设置一个至少 8 个字符的新密码。其他检查项见[检查安装结果](install.zh-CN.md#检查安装结果)。

macOS 和 FreeBSD 使用同样的命令。macOS 上如果要安装为当前用户服务，移除 `sudo` 并加上 `--user`，升级时也需要保留该参数。FreeBSD 没有 `sudo` 时，切换到 root 执行。Windows 在拷贝后的文件夹中打开管理员 PowerShell：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master
```

## 3. 添加执行节点

打开控制台的 **执行节点 → 添加节点**，填写节点名称和该节点能访问的 Master 地址，点击 **生成加入指令**。复制对应系统的命令，在执行节点主机上运行。Linux、macOS 和 FreeBSD 的命令形如 `curl -fsSL '<链接>' | bash`，以 root 或可使用 `sudo` 的用户运行。窗口中显示“下载来源：本 Master，新节点无需访问公网。”

该命令从 Master 获取安装脚本和程序包，启动执行节点并加入集群。节点在 **执行节点** 页面显示为在线后，即可用它创建迁移任务。Docker 执行节点需要先在主机上导入镜像，做法见最后一条注意事项。

Master 能联网时会按需补齐其他平台的包。如果需要执行节点自行从公网下载，在添加节点窗口的 **下载来源** 中选择 **公网（自动选择）**。

## 升级和重装

离线主机无法使用控制台中的 **版本更新**，因为它需要从 GitHub 或京东云 OSS 下载。升级方法与安装时类似：按第 1 步准备新版本的文件夹，拷贝到需要升级的每台主机上。

升级前先停止正在运行的任务。先升级 Master，再逐台升级执行节点。在新文件夹中执行：

```bash
sudo bash native-install.sh --role master --upgrade --yes
# 在各台执行节点主机上执行：
sudo bash native-install.sh --role agent --upgrade --yes
```

这里使用的 `native-install.sh` 就是原生安装脚本本身，`install.sh` 在它基础上增加了交互提问和 Docker Compose 方式，内部调用的是同一个脚本。较早版本的安装脚本不会自动识别文件夹，需要加 `--package-dir "$PWD"`（Windows 为 `-PackageDir $PWD.Path`）。Windows 在原安装命令上加 `-Upgrade`，执行节点使用 `-Role agent`。

升级保留原有的配置、任务和节点身份。升级完成后，确认各节点均在线且版本一致，再继续任务。v4.1.42 有一些功能要求执行节点也升级到 v4.1.42，详见[升级到 v4.1.42](operations.zh-CN.md#升级到-v4142)。对安装完整、运行正常的系统重复安装同一版本，如果不加 `--force`，不会做任何更改。

删除程序后重装，保留原配置和完整数据目录即可，不要遗漏 `.db.key` 文件。重新运行安装命令，使用过自定义目录时填写原路径。详见[重装说明](install.zh-CN.md#重装且不丢失数据)。卸载也可以离线执行：`sudo bash install.sh --uninstall --role agent` 不下载任何内容（见[卸载](install.zh-CN.md#卸载)）。

## 注意事项

- Unix 主机提前安装 Bash、curl、tar 和 SHA-256 工具。Linux 需要 systemd 219、Bash 4.2、curl 7.29 及以上版本。FreeBSD 在断网前执行 `pkg install -y bash curl`。没有 `sudo` 时，以 root 运行安装命令。
- 所有包必须来自同一版本。Master 离线时，缺少的执行节点平台包需要补充到原安装文件夹，再按上面的 Master 升级命令导入，这会重启 Master。
- 邀请有有效期，只能使用一次。过期后重新生成即可。
- 升级成功后，临时备份会自动删除；失败时脚本尝试恢复并保留备份。如果计划降级，应提前另做备份，见[备份与恢复](operations.zh-CN.md#备份与恢复)。
- 本页安装的是原生服务。离线 Docker 部署还需要提前准备容器镜像 `moonwx/osstransfer`。在联网机器上用 `docker save` 导出，在离线主机上用 `docker load` 导入，详见 [Docker Compose](docker-compose.zh-CN.md)。
