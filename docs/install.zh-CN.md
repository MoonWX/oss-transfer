# 安装、升级与卸载

[English](install.md)

本页介绍在主机上安装 OSS Transfer 的全部内容：选择版本、确认安装成功、其他操作系统、路径和日志、升级、卸载，以及在其他电脑上安装命令行工具（CLI）。在 Linux 上最快的方式见[快速开始](../README.zh-CN.md#快速开始)。无法联网的主机请使用[离线安装](offline-install.zh-CN.md)，容器部署见 [Docker Compose](docker-compose.zh-CN.md)。

为大规模迁移搭建集群之前，请先阅读[规划](planning.zh-CN.md)（拓扑、端口、存储权限、规格）和[安全](security.zh-CN.md)（TLS、凭据、密码）。

## 开始之前

- **Linux：** systemd 219 及以上版本（CentOS 7 / RHEL 7 及更新版本），以及 Bash、curl、tar 和 SHA-256 工具。以 root 身份或通过 `sudo` 运行安装脚本。
- **macOS、Windows、FreeBSD：** 见[其他系统](#其他系统)。
- **端口：** Master 默认监听 TCP `1685`。浏览器、CLI 和每个执行节点（Agent）都需要能访问它；执行节点不需要开放入站端口。完整清单见[规划](planning.zh-CN.md)。
- **哪些以 root 运行：** 安装脚本安装的原生服务在 Linux 和 FreeBSD 上以 root 运行（systemd 单元和 rc.d 脚本没有指定其他用户），macOS 系统服务以 root 运行，Windows 服务以 LocalSystem 运行；macOS 的 `--user` 安装以当前登录用户运行。在线更新需要这些权限。用控制台 Docker 指令启动的执行节点在容器内以 UID 65532 运行。

## 在 Linux 上安装

在 Master 主机上运行安装脚本：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash
```

在中国大陆，可在同一命令中改用镜像地址 `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/install.sh`。

安装脚本会询问语言、角色（Master 或 Agent）、部署方式（原生或 Docker Compose），安装原生 Master 时还会询问是否在本机同时安装执行节点。本机执行节点会自动加入该 Master。

通过管道运行时，把参数写在 `bash -s --` 之后。例如无人值守地安装 Master 和本机执行节点：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --role master --with-agent --yes
```

保存脚本后运行 `bash install.sh --help` 可查看全部选项。

### 检查安装结果

安装成功时，输出的结尾如下（路径和地址以你的主机为准；安装语言为中文时标签也是中文）：

```text
服务: oss-transfer-master
配置文件: /opt/oss-transfer/master/config/service.json
日志: /opt/oss-transfer/master/data/logs/service.log
Installation: /opt/oss-transfer/master/bin
CLI: /usr/local/bin/oss-transfer-cli
Tasks: /usr/local/bin/oss-transfer-cli task list

=== master 4.2.0 · 安装成功 ===
访问地址: http://10.0.0.5:1685

初始管理员: admin
初始密码: <只显示这一次>
```

然后确认：

1. `sudo systemctl status oss-transfer-master` 显示 `active (running)`。执行节点的服务名为 `oss-transfer-agent`。
2. 用浏览器打开 **访问地址**，以 `admin` 和初始密码登录。首次登录必须设置新密码：至少 8 个字符，不能与用户名或初始密码相同。
3. 如果同时安装了本机执行节点，在 Master 主机上运行 `sudo oss-transfer-cli node list`，它应显示为在线。

初始密码只显示一次。如果遗失，请[重置管理员密码](operations.zh-CN.md#重置管理员密码)。

安装脚本报错停止时：

| 提示 | 处理方法 |
| --- | --- |
| `Cannot listen on …: port 1685 is already in use by …` | 停止占用端口的程序，或用 `--port` 换一个端口（仅 Master，见[路径和端口](#路径和端口)）。安装脚本没有做任何更改。 |
| `启动等待超过 … 秒` | 查看提示中的日志和 `systemctl status`，排除原因后重新运行同一命令。 |
| `启动失败：此 Agent 已在 Master 上被移除` | 该执行节点的加入凭据已被撤销。从 **执行节点 → 添加节点** 复制新的加入命令，或[卸载](#卸载)该执行节点。 |
| `… 已是 v4.2.0 且正在运行，未做任何更改。如需重新安装，请加 --force。` | 相同版本已安装且运行正常。只有确实要重装时才加 `--force`。 |

### 添加执行节点

在控制台打开 **执行节点 → 添加节点**，填写节点名称和新节点能访问的 Master 地址，点击 **生成加入指令**。复制与该节点系统对应的命令，在该主机上运行。Linux、macOS 和 FreeBSD 的原生命令形如 `curl -fsSL '<链接>' | sudo bash`。几秒钟后，该节点会在 **执行节点** 页面显示为在线。使用 CLI 时，`oss-transfer-cli agent invite` 会打印同样的命令（见 [CLI](cli.zh-CN.md#添加和移除执行节点)）。Docker 指令见 [Docker Compose](docker-compose.zh-CN.md)。

## 其他系统

### CentOS 7 / RHEL 7

CentOS 7 和 RHEL 7 是支持的最早的 Linux 版本，安装命令与其他 Linux 相同：安装脚本和在线更新都能使用系统自带的 systemd 219、Bash 4.2 和 curl 7.29。较旧的 systemd 会让在线更新有两处不同：

- systemd 低于 236 时，在线更新失败后，其辅助单元 `oss-transfer-update-*` 会一直显示为失败状态，执行 `systemctl reset-failed` 即可清除。
- systemd 低于 240 时，无法启动的更新辅助进程不会被立即报告，要等更新超时后才显示失败。

### macOS

为当前用户安装服务：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
bash install.sh --role master --user
```

安装为系统服务时，改用 `sudo bash install.sh --role master`。安装脚本会打印服务名称和路径。Docker Compose 安装只支持 Linux；使用 Docker Desktop 时，请改用控制台中的 Docker 指令添加执行节点。

### Windows

在管理员 PowerShell 中运行：

```powershell
Invoke-WebRequest https://raw.githubusercontent.com/MoonWX/oss-transfer/main/deploy/native/Install.ps1 -OutFile Install.ps1
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master
```

在中国大陆，也可以从 `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/Install.ps1` 获取该脚本。安装成功时会显示 `安装成功：master 4.2.0`、访问地址、服务名称（`OSSTransferMaster` 或 `OSSTransferAgent`）、各个路径和日志文件。运行 `Get-Help .\Install.ps1` 可查看选项。

### FreeBSD

以 root 身份运行：

```sh
pkg install -y bash curl
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
bash install.sh --role master
```

在所有系统上，都使用安装脚本打印的 `admin` 密码登录。原生 Master 安装脚本会询问是否同时安装本机执行节点。

## 下载源

安装脚本默认从 GitHub 下载；主机公网 IP 位于中国大陆时，改从京东云 OSS 镜像下载。首选来源无法访问时，会自动尝试另一个。判断所在地时，依次访问 `https://www.cloudflare.com/cdn-cgi/trace` 和 `https://ipinfo.io/country`。使用 `--source github` 或 `--source jdoss`（`Install.ps1` 使用 `-Source`）可直接指定来源并跳过这一步。镜像中的 `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/` 始终包含最新正式版的全部文件，其中也有 `install.sh` 和 `Install.ps1`；每个版本（包括预发布版本）另有自己的目录，例如 `v4.2.0/`。

以文件形式保存的安装脚本（而不是通过管道交给 shell 运行）会依次在自身所在文件夹和当前目录中查找 `manifest.tsv` 或本机平台的压缩包。找到其中之一时，直接用该文件夹中的文件安装，不下载任何内容；如果指定了 `--version`，它必须与这些文件的版本一致。加 `--source github` 或 `--source jdoss`（`Install.ps1` 使用 `-Source`）则改为在线下载。详见[离线安装](offline-install.zh-CN.md)。

从控制台添加的执行节点默认从 Master 下载安装脚本和安装包，因此不需要访问公网；加入对话框会显示下载来源。

## 安装指定版本或预发布版本

安装脚本默认使用最新正式版。需要安装固定版本时，用 `--version`（`Install.ps1` 使用 `-Version`）指定，开头的 `v` 可写可不写：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --version 4.2.0
```

```sh
bash install.sh --role master --version 4.2.0
```

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -Version 4.2.0
```

安装脚本会从 GitHub 发布 `v4.2.0` 或镜像目录 `oss-transfer/v4.2.0/` 下载该版本。每个版本的发布说明和全部文件见 [GitHub Releases](https://github.com/MoonWX/oss-transfer/releases)。

预发布版本（例如 `4.2.1-beta.1`）的安装方式相同：

```sh
bash install.sh --role master --version 4.2.1-beta.1
```

安装脚本校验预发布版本的方式与正式版完全相同：使用该标签的 GitHub 发布摘要或带版本号的 OSS 清单，而不是 `latest`。带 `--upgrade` 和 `--version` 重新运行安装脚本，可以把已有安装升级到该版本，包括从预发布版本升级到对应的正式版。从控制台添加的执行节点会下载与 Master 相同的版本。

Docker Compose 部署在 `.env` 中设置精确的镜像标签；`latest` 和 `X.Y` 只跟随正式版：

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:4.2.0
```

预发布版本属于实验性质；在线更新和回到正式版的方法见[试用预发布版本](operations.zh-CN.md#试用预发布版本)。

## 路径和端口

新的原生安装把文件集中在同一个根目录下。Linux 默认使用 `/opt/oss-transfer`，macOS 默认使用 `/Library/Application Support/OSS Transfer`（使用 `--user` 时位于用户的 Library），FreeBSD 默认使用 `/usr/local/lib/oss-transfer`，Windows 默认使用 `%ProgramData%\OSS Transfer`。需要使用其他磁盘时，设置 `--root-dir` 或 `-RootDir`。操作系统的服务注册仍在该目录之外；Unix 系统还会保留全局 CLI 入口。

Linux 默认位置：

| | Master | 执行节点 |
| --- | --- | --- |
| 服务 | `oss-transfer-master` | `oss-transfer-agent` |
| 程序 | `/opt/oss-transfer/master/bin` | `/opt/oss-transfer/agent/bin` |
| 配置 | `/opt/oss-transfer/master/config/service.json` | `/opt/oss-transfer/agent/config/service.json` |
| 数据 | `/opt/oss-transfer/master/data` | `/opt/oss-transfer/agent/data` |
| 服务日志 | `/opt/oss-transfer/master/data/logs/service.log` | `/opt/oss-transfer/agent/data/logs/service.log` |
| 升级备份 | `/opt/oss-transfer/master/backups` | `/opt/oss-transfer/agent/backups` |
| CLI | `/usr/local/bin/oss-transfer-cli` | `/usr/local/bin/oss-transfer-cli` |

Windows 上的服务名为 `OSSTransferMaster` 和 `OSSTransferAgent`，每个角色在 `%ProgramData%\OSS Transfer\<角色>` 下使用 `bin`、`config`、`data` 和 `backups`；CLI 为 `bin\oss-transfer-cli.exe`。

Master 默认端口为 `1685`。`install.sh` 使用 `--port 1785` 修改，`Install.ps1` 使用 `-Listen '0.0.0.0:1785'`。使用 `--install-dir`、`--config-dir` 或 `--data-dir` 自定义位置，这些目录不能相互包含。升级时沿用已安装的路径。

## 日志

每个服务把日志写入自己数据目录下的 `logs/service.log`（见上表；安装脚本会以 `日志:` 打印该路径）。启动错误、任务问题、比对失败以及执行节点被移除后的说明都写在这里：

```sh
sudo tail -n 100 /opt/oss-transfer/master/data/logs/service.log
sudo tail -f /opt/oss-transfer/agent/data/logs/service.log
```

systemd 只记录服务的启动、停止和失败，以及退出状态：

```sh
sudo systemctl status oss-transfer-agent
sudo journalctl -u oss-transfer-agent --since today
```

日志文件会一直增长，服务不会自行轮转。服务以追加方式写入，因此可以用 `logrotate` 的 `copytruncate` 轮转，无需重启服务。在线更新也写入同一个文件。需要关注哪些内容，见[监控与日志](operations.zh-CN.md#监控与日志)。

## 升级

通常在控制台在线升级：先在 **运维工具 → 版本更新** 中更新 Master，再在 **执行节点 → 版本更新** 中更新执行节点（见[版本更新](operations.zh-CN.md#版本更新)）。以下情况改用安装脚本：无法在线更新的安装第一次升级、无法联网的主机（见[离线安装](offline-install.zh-CN.md#升级和重装)），或需要安装最新版以外的版本。

用安装脚本升级时，在每台主机上运行，先升级 Master：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --upgrade --yes
```

`--upgrade` 会找出本机已安装的原生服务，逐个备份、安装新版本并确认能够启动。加 `--role master` 或 `--role agent` 只升级其中一个，加 `--version` 指定版本。在已有安装的主机上不带参数重新运行一行安装命令，安装脚本也会询问是否升级。Windows 在安装命令后加 `-Upgrade`。

每个升级成功的服务都会显示 `=== master 4.2.0 · 安装成功 ===`（或 `agent`）。新版本无法启动时，安装脚本会恢复旧版本并保留备份（见[从失败的升级中恢复](operations.zh-CN.md#从失败的升级中恢复)）。

对完整且正在运行的安装重新运行相同版本时，不会做任何更改：交互运行时安装脚本会询问是否重装；带 `--yes` 时打印 `… 已是 v4.2.0 且正在运行，未做任何更改。如需重新安装，请加 --force。` 并以状态 0 退出。确实要重装时加 `--force`。缺失或损坏的部分（程序、服务定义、CLI、执行节点安装包）不需要 `--force` 也会修复。`Install.ps1` 没有 `-Force`，总是重新安装。

升级现有集群之前，请先阅读 [v4.2.0 带来的变化](operations.zh-CN.md#升级到-v420)。

## 重装且不丢失数据

再次运行安装脚本；如果修改过 `--root-dir`，请使用相同的值。安装脚本会识别保留下来的服务配置和数据，补回缺失的程序或服务定义，并沿用执行节点的身份。旧目录结构的现有安装继续使用其保存的路径。

保留每个角色的 `config` 目录和完整的 `data` 目录，包括密钥文件（`master.db.key` 或 `agent.db.key`）。密钥必须与数据库匹配。缺少配置或密钥时，安装脚本会停止，而不是创建一个空的替代安装。源文件、目标文件和共享挂载与这些应用目录相互独立。需要把安装迁到另一台主机时，见[备份与恢复](operations.zh-CN.md#备份与恢复)。

无人值守安装原生 Master 时，使用 `--with-agent` 或 `--without-agent`；Windows 使用 `-WithAgent` 或 `-WithoutAgent`。只有属于该 Master 的现有本机执行节点才会被复用。

## 卸载

卸载执行节点之前，先在 Master 上移除它，使它不再出现在任务的可选节点中：在 **执行节点** 页面操作，或运行 `oss-transfer-cli agent remove 节点 --yes`（见[移除执行节点](operations.zh-CN.md#移除执行节点)）。卸载 Master 不会卸载它的执行节点，请在各自主机上分别卸载。

### Linux、macOS 和 FreeBSD

带 `--uninstall` 运行安装脚本。卸载不下载任何内容：`install.sh` 自带原生安装脚本的副本。

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --uninstall --role agent
```

使用已保存的脚本时，运行 `sudo bash install.sh --uninstall --role agent`。选项：

| 选项 | 作用 |
| --- | --- |
| `--role master` 或 `--role agent` | 只卸载该角色。不指定时卸载本机全部安装。 |
| `--keep-data` | 保留配置、数据目录和升级备份，以后可用原来的身份、任务和设置重新安装。 |
| `--yes` | 不再询问确认。没有终端时必须加 `--yes`。 |
| `--service-name`、`--root-dir`、`--install-dir`、`--user` | 选择自定义名称或位置的安装，或 macOS 用户服务。 |

`--uninstall` 不能与安装选项一起使用，`--keep-data` 必须与 `--uninstall` 一起使用。

安装脚本先在 `将卸载以下安装：`、`将删除：` 和 `将保留：` 下列出全部内容，并询问 `确认卸载？[y/N]`。它会先停止所有服务，再删除文件，最后显示 `=== 卸载完成 ===` 和后续步骤。

会删除：

- 服务定义（systemd 单元、launchd plist 或 rc.d 脚本）和程序；
- 安装记录（`install.tsv`）和该安装记录的 CLI（例如 `/usr/local/bin/oss-transfer-cli`）及其状态文件；
- 不加 `--keep-data` 时：`service.json`、数据目录和该服务自己的升级备份；
- 本机已没有任何安装（且不加 `--keep-data`）时：root 和运行 `sudo` 的用户的 CLI 登录会话和偏好设置（`~/.config/oss-transfer-cli`，macOS 为 `~/Library/Application Support/oss-transfer-cli`）。

会保留并说明原因：仍被其他安装使用的 CLI、其他安装的备份、含有无法识别文件或属于共享目录、层级过浅的数据目录，以及仍有其他文件的目录（`已保留（目录中还有其他文件）`）。如果某个服务定义运行的是其他程序，或某个服务无法停止，安装脚本会拒绝卸载，不做任何更改。本机没有安装时，会提示无需卸载并以状态 0 退出。源目录和目标目录中已迁移的文件不会被触碰。

### 安装脚本创建的 Docker Compose 安装

```sh
sudo bash install.sh --uninstall --method compose --role agent
```

该命令对安装脚本创建的项目运行 `docker compose down`，删除 `/opt/oss-transfer-compose/<角色>` 下的 Compose 文件；不加 `--keep-data` 时还会删除 `/var/lib/oss-transfer-compose/<角色>` 下的数据。它只处理安装脚本自己写入的 Compose 文件，并保留 Docker 镜像（可用 `docker image rm` 删除）。存在这类安装时，普通的 `--uninstall` 也会提示。

### 用控制台 Docker 指令启动的执行节点

用 **添加节点 → Docker** 中的 `docker run` 启动的执行节点没有安装脚本。在 Master 上移除该节点后，删除它的容器和数据卷。名称见当初的加入命令，例如：

```sh
docker rm -f oss-transfer-agent-1a2b3c
docker volume rm oss-transfer-agent-1a2b3c-data
```

v4.2.0 之前用此命令添加的执行节点使用 `oss-transfer-agent` 和 `oss-transfer-agent-data`。自行编写的 Compose 项目见 [Docker Compose](docker-compose.zh-CN.md)。

### Windows

`Install.ps1` 暂不提供卸载选项。在管理员 PowerShell 中，对默认服务名和路径的执行节点运行（Master 改用 `OSSTransferMaster` 和 `master`）：

```powershell
Stop-Service OSSTransferAgent
sc.exe delete OSSTransferAgent
Remove-Item -Recurse -Force "$env:ProgramData\OSS Transfer\agent\bin"
```

同时删除数据时，再删除 `config`、`data` 和 `backups`：

```powershell
Remove-Item -Recurse -Force "$env:ProgramData\OSS Transfer\agent\config", "$env:ProgramData\OSS Transfer\agent\data", "$env:ProgramData\OSS Transfer\agent\backups"
```

每个用户的 CLI 登录会话和偏好设置位于 `%APPDATA%\oss-transfer-cli`，不再使用 CLI 时可删除该文件夹。

## 在其他电脑上安装 CLI

每个发布压缩包都包含 CLI。要在工作站上管理集群，下载与该电脑对应的压缩包，校验后取出 `oss-transfer-cli`。以 Linux amd64 和 v4.2.0 为例：

```sh
v=4.2.0 p=linux-amd64
curl -fLO "https://github.com/MoonWX/oss-transfer/releases/download/v$v/oss-transfer-$p.tar.gz"
curl -fLO "https://github.com/MoonWX/oss-transfer/releases/download/v$v/SHA256SUMS"
grep " oss-transfer-$p.tar.gz\$" SHA256SUMS
sha256sum "oss-transfer-$p.tar.gz"
tar -xzf "oss-transfer-$p.tar.gz"
sudo install -m 755 "oss-transfer-$p/oss-transfer-cli" /usr/local/bin/oss-transfer-cli
```

两个校验和必须相同（macOS 上使用 `shasum -a 256`）。在中国大陆，可把 GitHub 地址换成 `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/v$v`。压缩包名称见[离线安装](offline-install.zh-CN.md#1-准备安装文件)；Apple Silicon 使用 `darwin-arm64`。Windows 从 `oss-transfer-windows-amd64.zip` 中取出 `oss-transfer-cli.exe`。CLI 版本请与 Master 保持一致。

检查 CLI 并登录 Master：

```sh
oss-transfer-cli --version
oss-transfer-cli config set master http://10.0.0.5:1685
oss-transfer-cli login
oss-transfer-cli task list
```

`--version` 显示 `OSS Transfer CLI v4.2.0` 和构建信息。`login` 以 `admin` 登录（其他账号加 `--username`），以不回显的方式询问密码，成功后显示 `已登录 …（用户 admin）`。脚本用法和其他选项见 [CLI](cli.zh-CN.md#连接-master)。

## 反向代理

请把应用部署在域名根路径，例如 `https://transfer.example.com/`。在该域名的 NGINX server 配置中添加：

```nginx
location / {
    proxy_pass http://127.0.0.1:1685;
    proxy_set_header Host $http_host;
    proxy_read_timeout 60s;
}
```

不支持 `/transfer/` 这类子路径。添加执行节点时，请选择该节点能访问的 Master 地址；浏览器通过反向代理访问时，执行节点仍可使用内网 IP。TLS 证书和 HTTPS Master 地址见[安全](security.zh-CN.md)。
