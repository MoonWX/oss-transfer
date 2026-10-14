# OSS Transfer

[English](README.md)

OSS Transfer 支持 S3 兼容存储之间，以及对象存储与本地目录之间的数据迁移。Master 提供 WebUI、API 和调度；Agent 负责扫描和传输。Master、Agent、CLI 均为独立 Go 程序，支持 Linux、macOS、Windows 的 amd64 和 arm64，运行时不需要 Java。

默认使用原生服务部署，Agent 直接访问主机目录和已挂载的文件系统。需要容器部署时，也可以使用 Docker Compose。

## 安装 Master

在 Linux 或 macOS 上，从京东云 OSS 下载安装器：

```sh
RELEASE_URL=https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest
curl -fSL "$RELEASE_URL/install.sh" -o install.sh
sudo bash install.sh --lang zh --method native
```

按提示选择主节点。Linux 使用 systemd，macOS 使用 launchd。自定义目录、端口可查看 `--help`；macOS 加 `--user` 可安装为当前登录用户的服务。

Windows 使用管理员 PowerShell：

```powershell
$ReleaseUrl = 'https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest'
Invoke-WebRequest "$ReleaseUrl/Install.ps1" -OutFile Install.ps1
.\Install.ps1 -Role master -ReleaseUrl $ReleaseUrl -Yes
```

打开 `http://主机地址:1685`，使用服务日志中的 setup token 创建管理员账号，并设置浏览器和 Agent 可访问的 Master URL。没有默认密码。安装器会输出服务名、配置路径和日志位置。Linux 默认安装的日志命令为：

```sh
sudo cat /var/lib/oss-transfer-go/master/logs/service.log
```

[GitHub Releases](https://github.com/MoonWX/oss-transfer/releases) 提供相同的程序和校验文件。本地安装文件、升级和回滚见[原生部署文档](deploy/go/README.md)。

## 添加执行节点

Master 不执行文件传输，单机部署也需要安装 Agent。在 **执行节点 → 添加节点** 创建一次性邀请，把生成的原生安装命令放到执行主机运行。后期添加节点也使用这个步骤。Agent 主动连接 Master，重启后保留身份，无须对外开放管理端口。Master 和 Agent 使用相同版本。

本地文件任务填写 Agent 主机上的绝对路径，例如 `/mnt/storage/import`。服务账号需要有源文件的读取权限或目标目录的写入权限。节点设置中的本地根目录用于声明路径，不会自动创建目录或挂载存储。

多节点访问本地文件时，应在各节点的相同路径挂载同一共享文件系统。工具会在分发任务前验证共享访问。

邀请链接不要公开。删除节点会撤销其凭据；重新加入需要新邀请和新的 Agent 数据目录。

## 使用

WebUI 支持中文、英文和日文，首次使用跟随浏览器语言。通过语言菜单切换后会保存选择，当前页面和已填内容保持不变。CLI 可指定 `--lang zh`、`--lang en`、`--lang ja`，或设置环境变量 `OSS_TRANSFER_LANG`，例如 `oss-transfer-cli --lang ja task list`。JSON 字段、状态码、路径和对象 Key 保持原样。

先添加存储连接，再创建任务，选择来源、目标和执行节点，按需设置 `SKIP` 或 `OVERWRITE`、目标命名和限速。任务详情展示进度和子任务；子任务使用 `#主任务ID-1`、`#主任务ID-2` 等编号，点击可查看详情。

停止后恢复会保留已记录的对象结果。未完成的下载从该文件重新开始；未完成的 S3 上传会清理旧 multipart，再重传该对象。进程重启后不会复用已上传的分片。任务停止或结束后可导出报告。节点限速作用于该节点上的全部任务；任务带宽上限由所选节点共同使用。

CLI 使用 WebUI 的同一套账号。在 Bash 中登录：

```bash
read -r -s -p 'Password: ' OSS_TRANSFER_PASSWORD; printf '\n'
export OSS_TRANSFER_PASSWORD
oss-transfer-cli --host http://主机地址:1685 login --username admin --password-env OSS_TRANSFER_PASSWORD
unset OSS_TRANSFER_PASSWORD
```

后续命令使用相同的 `--host`：

```sh
oss-transfer-cli --host http://主机地址:1685 task list
oss-transfer-cli --host http://主机地址:1685 task show 123
oss-transfer-cli --host http://主机地址:1685 task children 123 --offset 0 --limit 25
oss-transfer-cli --host http://主机地址:1685 task stop 123
oss-transfer-cli --host http://主机地址:1685 task resume 123
oss-transfer-cli --host http://主机地址:1685 task report 123 --output task-123.zip
```

创建任务、管理连接和节点的选项见 `--help`，加 `--json` 输出结构化结果。Compose 中使用 `docker compose exec master oss-transfer-cli` 执行 CLI。

## 升级与备份

已有 Go 原生部署，下载新版本安装器，沿用原来的角色、路径和服务名：

```sh
sudo bash install.sh --role master --method native --release-url "$RELEASE_URL" --upgrade --yes
```

Agent 使用 `--role agent`，Windows 安装器使用 `-Upgrade`。安装器验证程序后，备份停止状态下的数据库及服务配置，再检查新服务。启动失败会回滚。仅本安装管理的 CLI 会直接替换，其他已有 CLI 需要明确使用 `--replace-cli` 同意备份后替换。计划升级前先停止任务，并保留备份；应用回滚不会撤销已传输的文件或对象。

V4.0.x 数据库需要[离线导出和转换](docs/development/legacy-migration.md)，不能直接把 Go 指向旧 RocksDB 目录。保留范围及转换后首次恢复的行为见[迁移说明](docs/go-migration.md)。

## Docker Compose 部署（可选）

Linux 也支持 Docker 部署。可在安装器中选择 Docker Compose（`--method compose`），或把仓库中的 `docker-compose.yml` 和 `.env.example` 放到部署目录：

```sh
cp .env.example .env
```

Compose 默认使用 `moonwx/osstransfer:latest`，端口和本地目录可在 `.env` 中修改：

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest
TRANSFER_PORT=1685
TRANSFER_LOCAL_ROOT=./transfer-files
```

启动 Master，查看初始化令牌：

```sh
docker compose pull master
docker compose up -d master
docker compose logs master
```

在浏览器完成首次设置。Master 的数据库保存在 `master-data` 卷中。`docker compose down` 保留该卷；`docker compose down -v` 会删除它。

Master 可以搭配原生或容器 Agent。使用容器 Agent 时，可执行控制台生成的 Docker 命令，或启用 Compose 中的 `agent` profile。使用该 profile 前，创建宿主机目录并授予容器 UID/GID `65532` 访问权限：

```sh
sudo install -d -o 65532 -g 65532 -m 0750 ./transfer-files
```

在 **执行节点 → 添加节点** 创建邀请。在 Bash 中输入令牌，完成一次注册：

```bash
read -r -s -p 'Invitation token: ' OSS_TRANSFER_JOIN_TOKEN; printf '\n'
export OSS_TRANSFER_JOIN_TOKEN
docker compose --profile agent run --rm -e OSS_TRANSFER_JOIN_TOKEN agent \
  oss-transfer-agent --master-url http://master:1685 --data-dir /data --enroll-only
unset OSS_TRANSFER_JOIN_TOKEN
docker compose --profile agent up -d agent
```

该 profile 将 `TRANSFER_LOCAL_ROOT` 挂载到容器的 `/files`，创建任务时填写 `/files`。Agent 身份和断点保存在独立的 `agent-data` 卷中。需要访问更多目录时，为 Agent 增加 bind mount，任务中填写容器内路径。共享存储应先在宿主机完成挂载，再启动 Agent。

Compose 部署先停止运行中的任务，备份停止服务后的数据卷，再执行 `docker compose pull` 和 `docker compose up -d`。如果固定了镜像版本，先更新该标签。保留原有数据卷和 Agent 挂载。应用回滚不会撤销已传输的文件或对象。

## 编译

使用 `go.mod` 指定的 Go 版本、Node.js 24 和 Python 3。构建脚本需要 Bash：

```sh
./build.sh --test
./build.sh --all
```

第一条执行测试并构建当前平台，第二条构建全部六个平台。压缩包和校验文件输出到 `output/go-development/`。每个包包含三个程序、`VERSION` 和 `SHA256SUMS`，WebUI 已嵌入 Master 程序。

另见[发版步骤](.github/RELEASING.md)和[验收项](.github/acceptance/README.md)。
