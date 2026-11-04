# OSS Transfer

[English](README.md)

OSS Transfer 是一个分布式数据迁移工具，用于在 S3 兼容对象存储之间迁移数据，也可以把本地文件上传到对象存储，或把对象下载到本地。

一个 Master 提供管理页面、API 和任务调度；多个 Agent 部署在靠近数据的主机上，负责扫描和传输。所有操作都可以在浏览器或命令行中完成。

## 功能

- **三种迁移方向**：对象存储 → 对象存储、本地 → 对象存储、对象存储 → 本地。支持按前缀、单个对象或文件清单迁移，可以保留、添加或替换路径前缀。
- **多节点并行**：一个任务可以分给多个 Agent 执行，默认按剩余数据量均衡分发。大对象会拆成分片，由多个节点并行上传。
- **实时进度**：按实际传输字节显示进度和速度。任务详情中同时显示 LIST、GET、HEAD、PUT 的实时请求速率和生效上限。
- **流量控制**：任务和节点都可以限制带宽及各类请求的 QPS，运行中修改立即生效。节点并发也可以随时调整。
- **故障恢复**：Agent 失联后，未完成的子任务会转给其他健康节点。扫描支持断点续扫，失败文件可以单独重试，任务结束后可以导出报告。
- **对象比对**：比对单个对象，或整个存储桶、前缀下的对象。可以只比较元数据，也可以流式计算 SHA-256 来比较内容。
- **运维**：在线升级 Master 并逐台升级 Agent，升级失败时自动回滚。存储凭证加密保存。界面和 CLI 支持中文、英文和日文。

## 架构

```text
 浏览器 / oss-transfer-cli
           │ HTTP :1685
           ▼
   ┌───────────────┐      长轮询 / 心跳      ┌───────────────┐
   │    Master     │ ◄────────────────────── │    Agent A    │ ──► 源端 / 目标端
   │ 页面 · API    │ ◄────────────────────── │    Agent B    │ ──► 对象存储或本地目录
   │ 调度 · 进度   │                         │      ...      │
   └───────────────┘                         └───────────────┘
```

Agent 主动连接 Master，所以 Master 不需要能访问 Agent。数据只在 Agent 和存储之间传输，不经过 Master。

## 快速开始

### 1. 安装 Master

在使用 systemd 的 Linux 上运行：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash
```

在中国大陆可以使用镜像：

```sh
curl -fsSL https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/install.sh | sudo bash
```

按提示选择 Master 和原生服务，并决定是否同时安装本机 Agent。本机 Agent 安装后会自动加入集群。程序、配置和数据默认放在 `/opt/oss-transfer/`。

macOS、Windows、FreeBSD 和自定义目录的安装方法见[安装选项](docs/install.md)，无法联网的环境见[离线安装](docs/offline-install.zh-CN.md)。

### 2. 登录

打开 `http://<master-ip>:1685`，用户名是 `admin`，密码是安装器输出的随机初始密码。首次登录后必须修改密码。

### 3. 添加 Agent

1. 打开 **执行节点 → 添加节点**。
2. 填写节点名称，以及 Agent 能访问到的 Master 地址。
3. 生成邀请，复制对应系统的命令，在 Agent 主机上运行。

Agent 会从 Master 下载安装器和程序包。如果 Agent 和 Master 在同一内网，填写 Master 的内网地址即可，浏览器可以继续使用公网地址或反向代理。

### 4. 创建迁移任务

1. 在 **存储连接** 页面添加源端和目标端的对象存储。
2. 在 **迁移任务** 页面点击 **创建迁移**，选择方向、源端、目标端和执行节点。
3. 涉及本地文件时，填写 Agent 主机上的路径，并通过目录预检。上传时，Agent 服务账号需要源目录的读权限；下载时需要目标目录的写权限。
4. 点击创建。任务会先扫描，再开始传输。

在 **迁移任务** 页面可以查看进度、调整限速、停止或继续任务、重试失败文件，任务结束后可以下载报告。

## 命令行

在已安装的 Master 或 Agent 主机上，用服务所属账号或管理员身份运行 CLI，会直接连接本机所属的集群，不需要登录：

```sh
oss-transfer-cli task list
oss-transfer-cli task progress <任务ID> --watch 5
oss-transfer-cli task limits <任务ID> --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli node list
```

CLI 可以完成页面上的全部操作，并支持 `--json` 输出，方便写脚本。远程登录、创建任务等用法见 [CLI 文档](docs/cli.md)。

## Docker Compose

已安装 Docker Engine 和 Compose 插件时运行：

```sh
mkdir oss-transfer && cd oss-transfer
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml
docker compose up -d master
docker compose logs master
```

在日志中找到 `admin` 的初始密码，然后登录。添加 Agent 时，在 **执行节点 → 添加节点** 中选择 **Docker**。镜像地址是 `moonwx/osstransfer`，目录挂载和升级方法见 [Docker Compose](docs/docker-compose.md)。

## 升级

建议在没有运行中的任务时升级。

1. 在 **运维工具 → 版本更新** 中更新 Master。
2. 在 **执行节点 → 版本更新** 中点击 **更新全部节点**，Agent 会逐台更新。

升级前会自动备份，新版本通过健康检查后删除备份；启动失败时会自动回滚。也可以重新运行安装命令来升级。

需要重装时，保留各角色的 `config` 目录和完整的 `data` 目录，包括 `.db.key` 密钥文件，安装器会继续使用原来的账号、任务和节点身份。计划降级时，应在升级前另外备份。详见[运维说明](docs/operations.md)。

## 注意事项

- **`SKIP` 不比较内容**：目标端已有同名对象或文件时直接跳过。需要确认内容一致时，使用[对象比对](docs/operations.md#compare-objects)。
- **本地路径是 Agent 主机上的路径**：多个节点处理同一个本地目录时，这个目录必须是共享目录。各节点的挂载路径可以不同，预检会写入一个小文件，确认它们指向同一个目录。
- **续扫不是持续同步**：断点续扫只是继续同一轮扫描，不保证发现断点之前的数据变化。需要迁移固定数据集时，应保持源端不变或使用快照。

## 支持的平台

| 系统 | 架构 |
| --- | --- |
| Linux | amd64、arm64、armv7、riscv64、loong64 |
| macOS | amd64、arm64 |
| Windows | amd64、arm64 |
| FreeBSD | amd64、arm64 |

每个版本都会发布到 [GitHub Releases](https://github.com/MoonWX/oss-transfer/releases)、Docker Hub `moonwx/osstransfer` 和国内 OSS 镜像。

## 文档

| 文档 | 内容 |
| --- | --- |
| [安装选项](docs/install.md) | 各系统的安装方法、目录、端口、重装和反向代理 |
| [离线安装](docs/offline-install.zh-CN.md) | 在无法联网的环境中安装 |
| [Docker Compose](docs/docker-compose.md) | 容器部署、目录挂载和升级 |
| [CLI](docs/cli.md) | 命令行的全部用法 |
| [运维说明](docs/operations.md) | 并发、对象比对、升级、故障转移、断点续扫、重置密码 |
| [数据库维护](docs/database-maintenance.md) | 回收数据库文件空间 |
| [从 V4.0.x 升级](docs/migration.md) | 旧版本数据的离线转换 |

## 从源码构建

需要 Go 1.26、Node.js 22.12 或更高版本，以及 Python 3。

```sh
./build.sh          # 构建当前系统和架构
./build.sh --all    # 构建全部平台
./build.sh --test   # 先运行测试再打包
```

安装包和校验文件输出到 `output/packages/`。代码结构如下：

| 目录 | 内容 |
| --- | --- |
| `cmd/` | Master 和 Agent 的入口 |
| `internal/` | 调度、传输引擎、存储适配和 Agent 通信 |
| `cli/` | `oss-transfer-cli` |
| `frontend/` | 管理页面（React + MUI） |
| `deploy/` | 安装脚本、打包和发布 |
