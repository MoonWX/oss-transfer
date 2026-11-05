# Docker Compose

[English](docker-compose.md)

如果事先知道每个 Agent 要访问哪些目录，可以使用 Compose。Agent 需要访问经常变化的主机路径时，[原生安装](../README.zh-CN.md)更方便。Master 提供控制台，Agent 执行传输。

## 启动 Master

在已安装 Docker Engine 和 Compose 插件的 Linux 主机上，下载 Compose 文件：

```sh
mkdir oss-transfer && cd oss-transfer &&
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml &&
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/.env.example -o .env &&
docker compose pull master &&
docker compose up -d master &&
docker compose logs master
```

在浏览器中打开这台主机的 `1685` 端口（本机运行时为 `http://localhost:1685`）。使用 `admin` 和日志中的初始密码登录，然后设置自己的密码。

`.env` 文件用于设置镜像和对外端口：

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest
TRANSFER_PORT=1685
```

## 添加 Agent

在 **执行节点 → 添加节点** 中填写节点名称，选择 **Docker**，将生成的单行命令复制到 Agent 主机上执行。安装器会创建 Agent 的 Compose 服务并加入集群。如果 Agent 通过内网连接，请在生成命令前修改 Master 地址。

Agent 的 Compose 文件保存在 `/opt/oss-transfer-compose/agent/docker-compose.yml`。要让 Agent 访问主机目录，在 Agent 的 `volumes` 下添加绑定挂载，然后重建容器。容器内外使用相同的绝对路径，例如 `/mnt/storage:/mnt/storage`。容器以 UID/GID `65532` 运行，需要对要传输的文件有访问权限。

## 升级与备份

先在控制台中停止正在运行的迁移任务，再停止容器。下面的命令会将 Master 的数据卷备份到 `backups/master-data.tar.gz`：

```sh
docker compose stop
mkdir -p backups
docker run --rm --volumes-from "$(docker compose ps -aq master)" \
  -v "$PWD/backups:/backup" busybox:1.37 \
  tar -czf /backup/master-data.tar.gz -C /data .
```

在每台 Agent 主机上，停止它的 Compose 服务，并将 `/var/lib/oss-transfer-compose/agent` 复制到备份位置。它的 Compose 文件位于 `/opt/oss-transfer-compose/agent`，请与备份一起保存。如果修改过默认路径，以安装器输出的路径为准。

Master 的 `docker-compose.yml` 和 `.env` 也要与备份一起保存。已传输的文件需要另外备份。归档命令会覆盖之前同名的归档文件。

重试文件清单任务时，如果找到了之前缺失的源对象，数据库会以 V4.1.8 及更早版本无法读取的格式记录该对象的标识。此后降级时，除了换回旧镜像，还需要恢复对应的升级前 Master 和 Agent 数据备份。

拉取新镜像并启动 Master：

```sh
docker compose pull master
docker compose up -d master
```

对每个 Agent，将其 Compose 文件中的镜像改为与 Master 一致，然后运行：

```sh
sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml pull
sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml up -d
```

升级时保留现有数据卷。`docker compose down -v` 会删除它们。
