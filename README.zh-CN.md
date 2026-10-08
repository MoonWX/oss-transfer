# OSS Transfer

[English](README.md)

OSS Transfer V4 用于 S3 兼容对象存储与本地文件系统之间的数据迁移。Master 提供 WebUI 和 CLI API，Agent 执行传输并主动连接 Master，无需开放入站服务端口。

支持对象存储到对象存储、对象存储到本地、本地到对象存储。任务支持选择节点、限速、停止和续传，以及文件级报告。

## 部署 Master

### Docker

镜像内含 Java，支持 Linux `amd64` 和 `arm64`，名称为 `moonwx/osstransfer`。

```sh
docker run -d --name oss-transfer-master --restart unless-stopped \
  -p 1685:1685 \
  -e TRANSFER_MASTER_WEB_URL=http://master.example.com:1685 \
  -v oss-transfer-master-data:/app/data \
  moonwx/osstransfer:4.0.0
```

将 URL 改为浏览器和 Agent 均能访问的 Master 地址。打开该地址，使用 `admin` 登录。初始密码随机生成，可通过以下命令读取：

```sh
docker exec oss-transfer-master cat /app/data/secrets/admin_password
```

登录后修改密码。连接配置、节点凭据和任务断点保存在 `/app/data`。更换容器时保留该卷。

迁移本地文件时，还需挂载相应目录，例如 `-v /srv/transfer-files:/data`。容器以 UID/GID `10001` 运行，挂载目录必须赋予其相应读写权限。创建任务时使用容器内路径。

Compose 示例位于 `deploy/docker/compose.yml`。

### 传统部署

需要 Linux、systemd、Java 8 或更高版本，以及 `curl`、`openssl`、`tar`、`timeout`、`ss`。安装包不含 Java；安装脚本会先检查 Java，再修改服务。

从 [Releases](https://github.com/MoonWX/oss-transfer/releases) 下载 `oss-transfer.tar.gz` 和校验文件。私有仓库的 Release 文件需要通过 GitHub 认证下载。

```sh
sha256sum -c oss-transfer.tar.gz.sha256
tar -xzf oss-transfer.tar.gz
cd oss-transfer
sudo ./deploy/install-master.sh --web-url http://master.example.com:1685
```

服务名为 `oss-transfer`。配置位于 `/etc/oss-transfer/application.yml`，初始密码位于 `/etc/oss-transfer/admin-password`，数据位于 `/var/lib/oss-transfer`。再次执行安装脚本会保留现有配置和数据。

```sh
sudo cat /etc/oss-transfer/admin-password
sudo systemctl status oss-transfer
sudo journalctl -u oss-transfer -f
```

使用 `--port` 设置服务端口，使用 `--java-bin` 指定 Java。完整参数见 `./deploy/install-master.sh --help`。

## 添加 Agent

先部署 Master，在**执行节点**中选择**添加节点**。Master 会生成临时、一次性的加入链接，并提供 Docker 和传统部署指令。在新节点执行所选指令后，Agent 自动加入集群，Master 无需通过 SSH 访问该节点。

Docker 使用同一镜像，设置 `TRANSFER_ROLE=agent`，挂载持久化的 `/app/data` 卷，无需发布端口。按需挂载 Agent 使用的本地或共享存储。

传统指令从 Master 下载并校验安装包，安装 `oss-transfer-agent` 服务。配置位于 `/etc/oss-transfer-agent/application.yml`，数据位于 `/var/lib/oss-transfer-agent`。执行前先安装 Java。

每个 Agent 使用独立凭据，重启后保留。不要向 Agent 复制 Master 的内部密钥。加入链接应妥善保管。Agent 必须能够访问 Master URL；经过不可信网络时应使用 HTTPS。

多节点执行本地文件任务时，需将同一共享文件系统挂载到相同路径。单个节点的本地磁盘不会自动共享给其他节点。

## 使用

1. 在**存储连接**中添加源和目标连接。
2. 创建任务，选择方向、范围及执行节点，按需设置冲突策略和限速。
3. 在**任务详情**中查看进度与子任务。调整执行或删除任务前先停止任务。
4. 续传会保留已记录的完成结果。任务停止或结束后可导出报告。

节点限速约束该节点上的全部任务；任务限速约束对应任务，同时受节点上限约束。对象存储返回限流响应时，自动调节可能暂时降低请求上限，节点详情提供触发请求和恢复状态。

CLI 使用同一 Master API：

```sh
oss-transfer-cli --help
oss-transfer-cli worker-status
oss-transfer-cli task list
```

Docker 中在命令前加 `docker exec oss-transfer-master`。使用 `--host` 连接其他 Master。通过登录提示或受保护的配置文件提供凭据，避免将密码写入命令历史。

## 升级与备份

升级前停止运行中的任务。停止服务后备份配置和数据目录，或使用一致性文件系统快照。不要直接复制正在使用的 RocksDB 目录作为备份。

Docker 拉取新版本并使用原有卷和挂载重建容器。传统部署解压新版本后再次执行安装脚本。Agent 升级同样保留配置和数据，已加入的节点无需重新生成邀请。

## 构建与发布

构建需要 JDK 8+、Maven、Node.js 22.12+ 和 Go 1.22+。

```sh
./build.sh
```

输出为 `output/oss-transfer.tar.gz`、SHA-256 校验文件及 `output/oss-transfer/`。构建元数据包含 Git 提交和构建时间，WebUI 据此显示版本信息。

GitHub Actions 在 Pull Request 和推送 `main` 时检查服务端、前端、CLI 和部署脚本。推送与工程版本一致的标签，例如 `v4.0.0`，会运行检查、发布 Release 安装包，并上传两种架构的 Docker 镜像。仓库需配置 `DOCKER_USERNAME` 和 `DOCKER_PAT` 两项 Secret；镜像名为 `moonwx/osstransfer`。
