# OSS Transfer

[English](README.md)

OSS Transfer 支持 S3 兼容存储桶之间的迁移，以及对象存储与本地目录之间的上传、下载。Master 提供 WebUI 和 API，Agent 连接 Master 执行传输。

## 一键安装

在目标 Linux 主机上运行安装脚本，支持 `amd64` 和 `arm64`：

```sh
curl -fSL https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/install.sh -o install.sh &&
sudo bash install.sh --lang zh
```

选择 Master 或 Agent，再选择 Docker Compose 或 systemd。填写配置并确认后开始安装。Debian、Ubuntu 会自动安装缺少的依赖，其他发行版需提前安装。

安装 Agent 时，先在 WebUI 的**执行节点 → 添加节点**中生成邀请，再将 URL 粘贴到新节点的安装脚本中。

使用已解压的本地安装包时，添加 `--method native --package-dir /path/to/oss-transfer`。向导用于新装，检测到已有部署会停止。更新版本请参照[升级与备份](#升级与备份)。

## 部署 Master

### Docker

镜像内含 Java。将 `http://master.example.com:1685` 替换为浏览器和 Agent 能访问的地址：

```sh
docker run -d --name oss-transfer-master --restart unless-stopped \
  -p 1685:1685 \
  -e TRANSFER_MASTER_WEB_URL=http://master.example.com:1685 \
  -v oss-transfer-master-data:/app/data \
  moonwx/osstransfer:latest
```

`latest` 跟随稳定版本。固定版本时，使用 `moonwx/osstransfer:4.0.5` 这样的标签。

打开配置的地址，使用 `admin` 登录。初始密码随机生成，通过以下命令读取：

```sh
docker exec oss-transfer-master cat /app/data/secrets/admin_password
```

登录后修改密码。配置和任务断点保存在 `/app/data`，更换容器时保留该卷。

迁移本地文件时，添加目录挂载，例如 `-v /srv/transfer-files:/data`。容器以 UID/GID `10001` 运行，需要该目录的读写权限。创建任务时填写容器内路径，本例为 `/data`。

### Docker Compose

使用 Docker Compose v2，以及仓库或[安装包](#传统部署)中的 `docker-compose.yml` 和 `.env.example`。在这两个文件所在目录执行：

```sh
cp .env.example .env
```

在 `.env` 中，将 `TRANSFER_MASTER_WEB_URL` 设置为浏览器和 Agent 可访问的地址，`OSS_TRANSFER_FILES_DIR` 设置为迁移文件所在的宿主机目录。默认镜像为 `moonwx/osstransfer:latest`。

按示例创建目录并启动服务：

```sh
sudo install -d -o 10001 -g 10001 -m 0750 /srv/transfer-files
docker compose pull
docker compose up -d
```

打开配置的地址，使用 `admin` 登录。读取初始密码、查看服务日志：

```sh
docker compose exec master cat /app/data/secrets/admin_password
docker compose logs -f master
```

更新时，先停止运行中的任务，再执行：

```sh
docker compose pull
docker compose up -d
```

`master-data` 卷保存配置和任务断点。`docker compose down` 保留该卷，`docker compose down -v` 会将其删除。

### 传统部署

执行安装脚本前，先安装 Java 8 或更高版本。主机还需具备 Linux、systemd，以及 `curl`、`openssl`、`tar`、`sha256sum`、`timeout`、`ss` 命令。

从京东云 OSS 下载并解压最新稳定版：

```sh
OSS_TRANSFER_RELEASE=latest
OSS_TRANSFER_DOWNLOAD_URL="https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/$OSS_TRANSFER_RELEASE"
curl -fSL "$OSS_TRANSFER_DOWNLOAD_URL/oss-transfer.tar.gz" -o oss-transfer.tar.gz &&
curl -fSL "$OSS_TRANSFER_DOWNLOAD_URL/oss-transfer.tar.gz.sha256" -o oss-transfer.tar.gz.sha256 &&
sha256sum -c oss-transfer.tar.gz.sha256 &&
tar -xzf oss-transfer.tar.gz
```

固定版本时，设置 `OSS_TRANSFER_RELEASE=v4.0.5`。安装包和校验文件也可从 [GitHub Releases](https://github.com/MoonWX/oss-transfer/releases) 下载。

安装 Master：

```sh
cd oss-transfer
sudo ./deploy/install-master.sh --web-url http://master.example.com:1685
```

服务名为 `oss-transfer`，配置文件为 `/etc/oss-transfer/application.yml`，数据目录为 `/var/lib/oss-transfer`。读取初始密码并检查服务：

```sh
sudo cat /etc/oss-transfer/admin-password
sudo systemctl status oss-transfer
sudo journalctl -u oss-transfer -f
```

使用 `--port` 设置服务端口，使用 `--java-bin` 指定 Java 路径。完整参数见 `./deploy/install-master.sh --help`。

## 添加 Agent

在 Master 的**执行节点 → 添加节点**中生成一次性邀请，将页面提供的 Docker 或传统部署指令复制到新节点执行。Agent 会加入集群，重启后继续使用已保存的凭据。

Docker Agent 使用同一镜像，设置 `TRANSFER_ROLE=agent`，并挂载持久化的 `/app/data` 卷。按需挂载本地或共享目录。Agent 主动连接 Master，容器无需发布端口。

传统部署需先安装 Java。指令从 Master 下载并校验安装包，然后安装 `oss-transfer-agent` 服务。配置文件为 `/etc/oss-transfer-agent/application.yml`，数据目录为 `/var/lib/oss-transfer-agent`。

不要公开邀请 URL。Agent 通过公网连接 Master 时应使用 HTTPS。删除节点会撤销其凭据；重新加入时，使用新的邀请和新的 Agent 数据目录或卷。

多节点执行本地文件任务时，将同一共享文件系统挂载到各节点的相同路径。

## 使用

1. 在**存储连接**中添加源和目标连接。
2. 创建任务，选择方向、范围和执行节点，按需设置冲突策略和限速。
3. 在**任务详情**中查看进度，点击子任务查看详情。
4. 停止后续传会沿用已记录的结果。任务停止或结束后可导出报告。

节点限速约束该节点上的全部任务。任务限速只作用于对应任务，同时受节点上限约束。对象存储限流触发自动调节时，节点详情会显示触发请求和恢复状态。

CLI 示例：

```sh
oss-transfer-cli --help
oss-transfer-cli worker-status
oss-transfer-cli task list
oss-transfer-cli task show 123
oss-transfer-cli task children 123 --offset 25 --limit 25
oss-transfer-cli task show 123 --child 123-26 --node 10.0.0.2
oss-transfer-cli task trash --direction oss-to-local
```

子任务每页 25 个。WebUI 滚动到子任务区域后加载首页，CLI 会给出下一页命令。使用 `--json` 输出结构化结果，使用 `--host` 连接其他 Master。

Docker 中在 CLI 命令前加 `docker exec oss-transfer-master`；Compose 中使用 `docker compose exec master`。

## 升级与备份

升级前停止运行中的任务。停止服务后再复制配置和数据目录，或使用一致性文件系统快照。直接复制正在使用的 RocksDB 目录可能得到无法恢复的备份。

Docker 拉取新版本，使用原有卷和挂载重建容器。传统部署解压新版本后，在 Master 上执行 `sudo ./deploy/install-master.sh`，在 Agent 上执行 `sudo ./deploy/install-agent.sh`。目录和服务名参数须与当前部署一致。安装脚本会保留配置和数据，包括 Agent 的加入凭据。

## 本地构建

构建需要 JDK 8+、Maven、Node.js 22.12+ 和 Go 1.22+。

```sh
./build.sh
```

构建输出为 `output/oss-transfer.tar.gz`、SHA-256 校验文件，以及解压目录 `output/oss-transfer/`。WebUI 从包内元数据读取 Git 提交和构建时间，用于显示版本。

发布步骤见[维护者发布指南](https://github.com/MoonWX/oss-transfer/blob/main/.github/RELEASING.md)。
