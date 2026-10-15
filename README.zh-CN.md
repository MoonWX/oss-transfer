# OSS Transfer

[English](README.md)

OSS Transfer 用于 S3 兼容存储与本地文件之间的数据迁移，也支持存储桶之间迁移。Master 提供控制台并调度任务，Agent 在数据所在或可访问的主机上执行传输。

## 安装 Master

预编译包支持以下平台：

| 系统 | 架构 |
| --- | --- |
| Linux | amd64、arm64、ARMv7、riscv64、loong64 |
| macOS、Windows、FreeBSD | amd64、arm64 |

Linux 或 macOS，国内使用京东云 OSS：

```sh
curl -fSL https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/install.sh -o install.sh
sudo bash install.sh --lang zh
```

国外使用 GitHub 仓库：

```sh
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
sudo bash install.sh --lang zh
```

安装器按本机公网出口 IP 选择下载源：中国大陆使用京东云 OSS，其他地区使用 GitHub。可加 `--source github` 或 `--source jdoss` 手动选择，加 `--version 4.1.5` 安装指定版本。

按提示设置监听端口。安装器在 Linux 上注册 systemd 服务，在 macOS 上注册 launchd 服务。macOS 如需安装为当前用户的服务，执行 `bash install.sh --role master --method native --user`，不加 `sudo`。

FreeBSD 以 root 执行 `pkg install bash curl`，然后下载同一脚本并运行 `bash install.sh`，服务由 rc.d 管理。CPU 和系统版本要求见[平台兼容性](deploy/go/README.md#platform-requirements)。

Windows 使用管理员 PowerShell。国内下载地址为：

```powershell
Invoke-WebRequest 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/Install.ps1' -OutFile Install.ps1
.\Install.ps1
```

国外将下载地址换为 `https://raw.githubusercontent.com/MoonWX/oss-transfer/main/deploy/go/Install.ps1`。下载源和版本参数分别为 `-Source github` / `-Source jdoss` 和 `-Version 4.1.5`。

打开 `http://主机地址:1685`，使用自定义端口时替换 `1685`。输入服务日志中的 setup token，创建管理员账号，并把 Master URL 设置为浏览器和 Agent 主机均可访问的地址。

安装结束时会输出日志路径。Linux 默认路径下的查看命令为：

```sh
sudo cat /var/lib/oss-transfer-go/master/logs/service.log
```

自定义路径、服务管理命令和离线安装见[原生部署文档](deploy/go/README.md)。各平台压缩包也可从 [GitHub](https://github.com/MoonWX/oss-transfer/releases) 下载。

## 添加 Agent

单机部署也需要在同一台主机上安装 Agent。初次添加和后期扩容使用相同步骤：

1. 在 **执行节点 → 添加节点** 创建邀请。
2. 复制 **Linux / macOS / FreeBSD** 或 **Windows** 命令，在执行主机上以管理员权限运行。
3. 等待节点显示在线，再为其分配任务。

Agent 主动连接 Master URL，并在本地保存加入凭据。Master 和 Agent 保持相同版本。

本地传输任务填写 Agent 主机上的路径，例如 `/mnt/storage/import` 或 `D:\transfer\import`。服务账号需要有源文件的读取权限或目标目录的写入权限。使用网络存储时，先在主机上完成挂载。

多个 Agent 处理同一本地文件任务时，各主机应在相同的绝对路径挂载同一共享文件系统。工具会在分发前检查共享访问。

## 迁移数据

先在 **存储连接** 中添加 S3 连接，再创建任务并选择来源、目标和执行节点。`SKIP` 保留已有目标，`OVERWRITE` 覆盖已有目标；目标命名和带宽限制按需设置。

任务详情中可查看进度、错误和各个子任务。任务可以停止后继续，恢复时保留已完成对象的结果，未完成的对象重新传输。停止或结束后可下载任务报告。

任务带宽上限由所选节点共同使用。节点限速则作用于该节点上的全部传输。

控制台的语言菜单支持中文、英文和日文。

## 使用 CLI

CLI 使用控制台账号登录。以下 Bash 命令会隐藏输入的密码：

```bash
read -r -s -p 'Password: ' OSS_TRANSFER_PASSWORD; printf '\n'
export OSS_TRANSFER_PASSWORD
oss-transfer-cli --host http://主机地址:1685 login --username admin --password-env OSS_TRANSFER_PASSWORD
unset OSS_TRANSFER_PASSWORD
```

后续任务命令使用相同的 Master 地址：

```sh
oss-transfer-cli --host http://主机地址:1685 task list
oss-transfer-cli --host http://主机地址:1685 task show 123
oss-transfer-cli --host http://主机地址:1685 task stop 123
oss-transfer-cli --host http://主机地址:1685 task resume 123
oss-transfer-cli --host http://主机地址:1685 task report 123 --output task-123.zip
```

创建任务、管理存储连接和节点的命令见 `oss-transfer-cli --help`，脚本中可加 `--json`。

CLI 默认跟随系统语言，支持中、英、日语，无法匹配时使用英文。执行一次 `oss-transfer-cli config set language zh` 即可固定为中文，设为 `auto` 则恢复跟随系统。`--lang` 仅覆盖当前命令。

## 升级与备份

升级前停止正在运行的任务，使用[安装部分](#安装-master)的国内或国外地址重新下载脚本，再执行：

```sh
sudo bash install.sh --upgrade
```

安装器会找到本机已安装的 Master 和 Agent，列出供确认后沿用原来的目录和服务设置升级。只升级 Agent 时加 `--role agent`。macOS 用户服务使用 `bash install.sh --upgrade --user`。Windows 重新下载 `Install.ps1`，在管理员 PowerShell 中执行 `.\Install.ps1 -Upgrade`。

安装器会在替换前停止服务并备份程序、配置和数据库，新服务启动检查失败时恢复备份。自行备份时，先停止服务，再复制配置目录和数据目录，路径以安装器输出为准。

V4.0.x 安装需要先按[数据库迁移步骤](docs/go-migration.md)处理，再安装当前版本。

## Docker Compose（可选）

Linux 上也可以使用 Compose。把 [docker-compose.yml](docker-compose.yml) 和 [.env.example](.env.example) 放到部署目录，创建 `.env`：

```sh
cp .env.example .env
```

默认配置为：

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest
TRANSFER_PORT=1685
TRANSFER_LOCAL_ROOT=./transfer-files
```

启动 Master，从日志中获取 setup token：

```sh
docker compose pull master
docker compose up -d master
docker compose logs master
```

在 `http://主机地址:1685` 完成账号设置。随后可以通过控制台添加原生 Agent，也可以在同一 Compose 项目内运行 Agent。

使用 Compose Agent 时，创建 `TRANSFER_LOCAL_ROOT` 指定的目录，并授予容器 UID/GID `65532` 访问权限。默认目录的命令为：

```sh
sudo install -d -o 65532 -g 65532 -m 0750 ./transfer-files
```

在 **执行节点 → 添加节点** 创建邀请，复制加入链接。完成一次注册后启动 Agent：

```bash
read -r -s -p 'Agent invitation URL: ' JOIN_URL; printf '\n'
export OSS_TRANSFER_JOIN_TOKEN="${JOIN_URL##*/}"
docker compose --profile agent run --rm -e OSS_TRANSFER_JOIN_TOKEN agent \
  oss-transfer-agent --master-url http://master:1685 --data-dir /data --enroll-only
unset OSS_TRANSFER_JOIN_TOKEN JOIN_URL
docker compose --profile agent up -d agent
```

宿主机目录挂载到 Agent 容器的 `/files`。例如宿主机的 `./transfer-files/example.txt`，在任务中填写 `/files/example.txt`。其他目录需要在 Agent 服务下添加 bind mount。

应用数据保存在 `master-data` 和 `agent-data` 卷中。升级时先停止任务和服务、备份这两个卷，然后执行：

```sh
docker compose pull master
docker compose up -d master
```

Agent 也使用 Compose 部署时，再运行 `docker compose --profile agent up -d agent` 更新它。保留原有数据卷和目录挂载。容器内 CLI 使用 `docker compose exec master oss-transfer-cli` 执行。

## 从源码编译

安装 `go.mod` 指定的 Go 版本、Node.js 24 和 Python 3，在 Bash 中运行：

```sh
./build.sh --test
```

该命令测试并打包当前平台，输出目录为 `output/go-development/`。加 `--all` 可构建全部支持的平台。[发版文档](.github/RELEASING.md)说明发布步骤，[测试文档](.github/acceptance/README.md)列出 CI 覆盖范围。
