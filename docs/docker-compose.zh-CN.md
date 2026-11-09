# Docker Compose

[English](docker-compose.md)

用 Docker Compose 运行 Master，用 Docker 容器运行执行节点（Agent）。Linux 上的 Docker Engine 以及 macOS、Windows 上的 Docker Desktop 都可以使用，Docker Desktop 下不需要 root。镜像是 `moonwx/osstransfer`。

执行节点需要访问很多或经常变化的主机目录时，[原生安装](../README.zh-CN.md#1-安装-master)更简单：容器只能访问挂载进去的目录。大规模迁移前，也请阅读[规划迁移](planning.zh-CN.md)和[安全说明](security.zh-CN.md)。

## 启动 Master

### Linux

在已安装 Docker Engine 和 Compose 插件的主机上运行：

```sh
mkdir oss-transfer && cd oss-transfer
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml
curl -fSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/.env.example -o .env
docker compose pull master
docker compose up -d master
docker compose logs master
```

如果当前用户没有使用 Docker 的权限，请在 `docker` 命令前加 `sudo`。

### macOS 和 Windows 上的 Docker Desktop

安装并启动 Docker Desktop，不需要 root 或管理员权限。在 macOS 上，在终端中运行上面的 Linux 命令。在 Windows 上，在 PowerShell 中运行：

```powershell
mkdir oss-transfer; cd oss-transfer
Invoke-WebRequest https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -OutFile docker-compose.yml
Invoke-WebRequest https://raw.githubusercontent.com/MoonWX/oss-transfer/main/.env.example -OutFile .env
docker compose pull master
docker compose up -d master
docker compose logs master
```

### 确认启动成功

日志中包含：

```text
Master listening on [::]:1685
Initial administrator: admin
Initial password: <随机密码>
```

在浏览器中打开这台主机的 `1685` 端口（同一台电脑上为 `http://localhost:1685`），用 `admin` 和初始密码登录，然后设置自己的密码（至少 8 个字符，不能是常见或容易猜到的密码，不能与用户名相同）。修改后，日志中会记录初始密码已失效。

如果页面打不开，运行 `docker compose ps` 确认 `master` 服务正在运行，并查看 `docker compose logs master`。也可能是 1685 端口已被其他程序占用，请在 `.env` 中换一个端口。

`.env` 用于设置镜像和对外端口：

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:latest
TRANSFER_PORT=1685
```

`latest` 跟随正式版。想自己决定何时升级，请固定版本，例如 `OSS_TRANSFER_IMAGE=moonwx/osstransfer:4.2.0`。

## 添加执行节点

1. 在管理页面打开 **执行节点**，点击 **添加节点**。
2. 填写节点名称和 **Master 地址（Agent 需能访问）**。在容器中，`localhost` 指容器自己，所以地址是回环地址时窗口会给出警告：
   - 执行节点与 Master 在同一台电脑上时（Docker Desktop 下很常见），点击 **host.docker.internal（Docker 与 Master 在同一台主机）**，地址会变为 `http://host.docker.internal:1685`。
   - 执行节点在其他主机上时，填写该主机能访问到的 Master 主机地址，例如内网 IP。（原生安装的 Master 还会把自己的局域网地址列为按钮；容器中的 Master 无法获取这些地址。）
3. 点击 **生成加入指令**，打开 **Docker** 标签页，选择对应的方式：

   | 方式 | 适用环境 | 说明 |
   | --- | --- | --- |
   | **macOS / Linux** | macOS 上的 Docker Desktop、Linux 上的 Docker Engine | 普通的 `docker run` 命令，不需要 root（Linux 上当前用户需能运行 `docker`） |
   | **Windows PowerShell** | Windows 上的 Docker Desktop | 同样的命令，PowerShell 写法 |
   | **Linux Docker Compose** | 有 root 权限的 Linux | Compose 安装器（`curl … \| sudo bash`），见[下文](#linux-compose-安装器) |

4. 点击 **复制指令**，在 30 分钟内到执行节点主机上运行。指令只能使用一次。

`docker run` 方式包含两条命令。第一条在临时容器中加入集群，并把执行节点的身份保存到 Docker 数据卷中；只有它成功后，第二条才会启动执行节点：

- 容器名为 `oss-transfer-agent-<id>`，数据卷为 `oss-transfer-agent-<id>-data`，`<id>` 是邀请 ID 的前 6 个字符，所以同一台主机上的多个执行节点不会冲突；
- 容器为只读，以 UID 65532 运行，不带任何 capability，并会自动重启（`--restart unless-stopped`）；
- 地址为 `host.docker.internal` 时，会加上 `--add-host host.docker.internal:host-gateway`，让这个名称在 Linux 上也能解析。

**成功时**，`docker ps` 中能看到容器 `oss-transfer-agent-<id>`，**执行节点** 页面中该节点显示为 **在线**。

**失败时：**

| 提示 | 原因和处理 |
| --- | --- |
| 连接被拒绝，或无法访问 Master | Master 地址不对或被防火墙拦截。启动 Master、修正地址或防火墙后，重新运行同一条命令：加入失败时不会创建执行节点容器。 |
| HTTP 403，邀请无效、已过期或已被使用 | 重新生成加入指令。 |
| Permission denied | 数据卷或挂载的目录不允许 UID 65532 写入。 |

### Linux Compose 安装器

**Linux Docker Compose** 方式以 root 运行安装器（`curl … | sudo bash`）。它把 Compose 项目写到 `/opt/oss-transfer-compose/agent/docker-compose.yml`，数据放在 `/var/lib/oss-transfer-compose/agent`；在 Debian 和 Ubuntu 上，如果没有安装 Docker Engine，还会询问是否安装。安装失败时会清理本次创建的内容，解决问题后重新运行同一条命令即可。它只能在 Linux 上运行：在 macOS 上会停止，并提示改用 Docker 指令。

## 为本地迁移挂载主机目录

容器中的执行节点只能读写挂载进容器的主机目录。在任务中填写容器内看到的路径。请在两侧使用相同的绝对路径，例如 `/mnt/storage:/mnt/storage`，这样主机上和管理页面中的路径一致。

- **`docker run` 方式：** 运行复制的指令之前，在第二条 `docker run` 的镜像名前加上 `-v /mnt/storage:/mnt/storage`。已有的执行节点需要带上这个参数重建容器（方法同[更新容器中的执行节点](#容器中的执行节点)），身份保存在数据卷中，不受影响。
- **Windows 上的 Docker Desktop：** 把 Windows 文件夹挂载到 Linux 路径，例如 `-v C:\transfer:/mnt/transfer`，在任务中填写 `/mnt/transfer`。
- **Linux Compose 安装器：** 加上 `--files-dir`，例如 `curl -fsSL '<加入链接>' | sudo bash -s -- --files-dir /mnt/storage`。该目录必须已经存在。

执行节点以 UID 65532 运行。在 Linux 上请给这个用户授权，例如下载目标目录可以用 `sudo chown -R 65532:65532 /mnt/storage`。下载任务开始前会检查目标目录是否可写；不可写时以 `LOCAL_PATH_NOT_WRITABLE` 失败，并给出处理提示。

`docker-compose.yml` 中的 `agent` profile 用于从已经加入集群的 `agent-data` 数据卷启动执行节点，并把 `TRANSFER_LOCAL_ROOT` 挂载到 `/files`。通过管理页面添加执行节点时不需要它。

## 查看日志

```sh
docker compose logs -f master               # Master，在 Master 所在目录中运行
docker logs -f oss-transfer-agent-<id>      # 用 docker run 添加的执行节点
sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml logs -f
```

`docker ps -a --filter name=oss-transfer-agent` 会列出主机上的执行节点容器。

## 在容器中使用 CLI

镜像中包含 `oss-transfer-cli`。在 Master 容器中使用不需要登录：

```sh
docker compose exec master oss-transfer-cli task list
docker compose exec master oss-transfer-cli node list
```

从容器中取出文件：

- **任务报告：** 把 ZIP 写到标准输出。请加 `-T`，让 Docker 原样传递字节：

  ```sh
  docker compose exec -T master oss-transfer-cli task report 12 --output - > task-12-report.zip
  ```

  报告不完整时，ZIP 仍会完整写出，原因输出到标准错误，退出状态为 3。
- **其他导出文件**（对象比对、异常记录）：先写入数据卷，再复制出来，最后删除：

  ```sh
  docker compose exec master oss-transfer-cli object export <比对ID> --output /data/comparison.zip
  docker compose cp master:/data/comparison.zip .
  docker run --rm --volumes-from "$(docker compose ps -q master)" busybox:1.37 rm /data/comparison.zip
  ```

管理页面上的下载按钮（**导出任务报告**、**导出全部差异（ZIP）**）会直接下载到浏览器。

## 设置与管理员密码

容器没有 `service.json`。节点限速和并发请在 **执行节点** 页面或用 CLI 修改，它们保存在执行节点的数据卷中。对象比对结果预算使用默认值（见[结果的保存和清理](operations.zh-CN.md#结果的保存和清理)）。

忘记管理员密码时，先停止 Master，再在一次性容器中重置：

```sh
docker compose stop master
docker compose run --rm master oss-transfer-master --data-dir /data/master --reset-admin-password
docker compose start master
```

用输出的密码登录，然后设置新密码。

## 备份

先在管理页面中停止正在运行的迁移任务，再停止容器。下面的命令会将 Master 的数据卷备份到 `backups/master-data.tar.gz`：

```sh
docker compose stop
mkdir -p backups
docker run --rm --volumes-from "$(docker compose ps -aq master)" \
  -v "$PWD/backups:/backup" busybox:1.37 \
  tar -czf /backup/master-data.tar.gz -C /data .
```

用 `docker run` 添加的执行节点，先停止它，再用同样的方法打包它的数据卷：

```sh
docker stop oss-transfer-agent-<id>
docker run --rm -v oss-transfer-agent-<id>-data:/data \
  -v "$PWD/backups:/backup" busybox:1.37 \
  tar -czf /backup/agent-<id>-data.tar.gz -C /data .
docker start oss-transfer-agent-<id>
```

用 Linux Compose 安装器安装的执行节点，停止它的 Compose 服务，将 `/var/lib/oss-transfer-compose/agent` 复制到备份位置，并一起保存 `/opt/oss-transfer-compose/agent` 下的 Compose 文件。如果修改过默认路径，以安装器输出的路径为准。

Master 的 `docker-compose.yml` 和 `.env` 也要与备份一起保存。已传输的文件需要另外备份。归档命令会覆盖之前同名的归档文件。

## 更新

先更新 Master，再更新执行节点，最好在没有运行中的任务时进行。更新前请先备份。

新版本可能会升级数据库 schema，此后旧镜像无法再打开这些数据（见[数据库空间](database-maintenance.zh-CN.md)）。因此降级时，除了换回旧镜像，还需要恢复对应的升级前 Master 和执行节点数据备份。

### Master

如果 `.env` 固定了版本，先修改镜像标签，然后运行：

```sh
docker compose pull master
docker compose up -d master
```

升级时保留现有数据卷。`docker compose down -v` 会删除它们。

### 容器中的执行节点

容器中的执行节点不能在线更新，需要用新镜像重建容器；身份和设置保存在数据卷中，会保留。正在执行的子任务会中断，执行节点重新连接后自动恢复。

1. 打开 **执行节点 → 版本更新**。Docker 执行节点所在的行有一个 **更新命令** 按钮。
2. 窗口中会显示该执行节点的命令，使用 Master 对应的镜像：
   - **用 docker run 加入的节点：** `docker pull <镜像>`、`docker rm -f <容器>`，以及重新启动它的 `docker run` 命令。
   - **用安装脚本以 Docker Compose 部署的节点：** 先把 `/opt/oss-transfer-compose/agent/docker-compose.yml` 中的 `image` 改为新镜像，再运行：

     ```sh
     sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml pull
     sudo docker compose -p oss-transfer-agent -f /opt/oss-transfer-compose/agent/docker-compose.yml up -d
     ```

3. 如果创建容器时加了其他参数（例如目录挂载），请在 `docker run` 中一并加上。在 Linux 上，通过 `host.docker.internal` 访问 Master 的执行节点还需要再加上 `--add-host host.docker.internal:host-gateway`。
4. 该节点显示为 **在线**，并显示新版本。

## 删除安装

**用 `docker run` 添加的执行节点：**

1. 在 **执行节点** 页面移除该节点，或运行 `oss-transfer-cli agent remove <节点> --yes`。执行节点随即停止工作并保持空闲。
2. 在它的主机上删除容器和数据卷。数据卷中保存着执行节点的身份和对象比对结果。

   ```sh
   docker rm -f oss-transfer-agent-<id>
   docker volume rm oss-transfer-agent-<id>-data
   ```

   v4.2.0 之前的版本添加的执行节点，名称为 `oss-transfer-agent` 和 `oss-transfer-agent-data`。

**用 Linux Compose 安装器安装的执行节点：** 按上面的方法移除节点，然后在它的主机上运行：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash -s -- --uninstall --method compose --role agent
```

它会列出要删除的内容并请求确认（`--yes` 跳过确认）。它会停止 Compose 项目，删除 Compose 文件，并在未加 `--keep-data` 时删除 `/var/lib/oss-transfer-compose/agent`。它只处理安装脚本生成的 Compose 项目，并保留 Docker 镜像。

**Master：** 在它所在的目录中，`docker compose down` 会删除容器，保留 `master-data` 数据卷。`docker compose down -v` 还会删除数据卷，其中的所有任务、账号和存储连接都会丢失。不再需要镜像时，用 `docker image rm moonwx/osstransfer:<标签>` 删除。
