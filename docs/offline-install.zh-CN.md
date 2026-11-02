# 离线安装

[English](offline-install.md)

在联网机器上下载一个版本的安装文件，再拷贝到离线 Master。Agent 仍需能访问 Master，以及任务使用的对象存储地址。

## 1. 准备安装文件

在联网机器的 Bash 中执行以下命令，下载最新版本及与**目标主机**匹配的压缩包。下面以 Linux x86-64 为例：

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
sudo bash install.sh --role master --package-dir "$PWD"
```

打开 `http://<master-ip>:1685`，使用 `admin` 和安装器输出的初始密码登录，然后设置自己的密码。

macOS 和 FreeBSD 使用同样的命令。macOS 安装当前用户服务时，去掉 `sudo` 并加上 `--user`，升级时也要保留该参数。FreeBSD 没有 `sudo` 时，切换到 root 执行。Windows 在拷贝后的文件夹中打开管理员 PowerShell：

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -PackageDir $PWD.Path -Yes
```

## 3. 添加 Agent

安装 Master 时可同时安装本机 Agent，它会自动加入集群。其他机器打开**执行节点 → 添加节点**，填写 Agent 能访问的 Master 地址，将生成的命令原样复制到 Agent 主机执行。

Agent 从 Master 获取安装器和程序包，不需要逐台拷贝文件或拆分邀请链接。Master 离线时，先把各台 Agent 所需平台的压缩包一起放进第 1 步的文件夹，再安装 Master；安装器会将这些包保存到 Master 的 `data/packages` 目录。

Master 能联网时，可以按需下载其他平台的同版本安装包。需要使用原来的公网下载方式时，在加入窗口点击“改用公网下载”；Docker 使用镜像仓库。

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
- 邀请有有效期且只能使用一次。新邀请可以继续使用 Master 已保存的安装包。版本更新需要先准备新版本文件。
