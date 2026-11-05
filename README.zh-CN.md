# OSS Transfer

[English](README.md)

OSS Transfer 是一个分布式数据迁移工具，用于在 S3 兼容对象存储之间迁移数据，也可以把本地文件上传到对象存储，或把对象下载到本地。

一个 Master 提供管理页面、API 和任务调度；多个 Agent 部署在靠近数据的主机上，负责扫描和传输。所有操作都可以在浏览器或命令行中完成。

![迁移任务页面：一个任务正在两个节点上执行，另外两个已完成](docs/images/overview.zh-CN.png)

## 功能

- **三种迁移方向**：对象存储 → 对象存储、本地 → 对象存储、对象存储 → 本地。支持按前缀、单个对象或文件清单迁移，可以保留、添加或替换路径前缀。
- **多节点并行**：一个任务可以分给多个 Agent 执行，默认按剩余数据量均衡分发。每个对象只由一个节点传输；128 MiB 以上的大对象会分片，在该节点内并行上传，节点空闲时会借用更多并发。
- **运行中调整节点**：可以给运行中的任务添加 Agent，或以排空、立即转移两种方式移出 Agent。带宽和 QPS 份额随之重新分配，不会超过任务上限。详见[调整任务的执行节点](docs/operations.zh-CN.md#调整任务的执行节点)。
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

打开 **执行节点** 页面，点击右上角的 **添加节点**。

![执行节点页面，红框标出“添加节点”按钮](docs/images/add-node.zh-CN.png)

填写节点名称，以及 Agent 能访问到的 Master 地址，然后点击 **生成加入指令**。窗口中会显示 Linux / macOS / FreeBSD、Windows 和 Docker 三种命令：

1. 当前所选系统的加入命令。Agent 主机是 Windows 或要用 Docker 运行时，先切换到对应标签。
2. 点击 **复制指令**，在 Agent 主机上运行。

<p align="center"><img src="docs/images/join-command.zh-CN.png" width="720" alt="添加节点窗口，红框标出加入命令和“复制指令”按钮"></p>

原生安装默认从 Master 下载安装器和程序包，点击 **改用公网下载** 可以改为从公网下载；Docker 命令会直接拉取 Agent 镜像。加入凭证只能使用一次，并有有效期。如果 Agent 和 Master 在同一内网，填写 Master 的内网地址即可，浏览器可以继续使用公网地址或反向代理。

### 4. 创建迁移任务

先在 **存储连接** 页面点击 **添加连接**，添加源端和目标端的对象存储。默认会在保存时检查连接是否可用。

![存储连接页面，红框标出“添加连接”按钮](docs/images/storage.zh-CN.png)

然后点击左上角的 **创建迁移**，按图中编号填写：

1. **迁移方向**：对象存储 → 对象存储、对象存储 → 本地，或本地 → 对象存储。
2. **迁移源和迁移目标**：选择存储连接和存储桶，可以只迁移某个前缀或单个对象，也可以修改目标端的对象名称。涉及本地文件时，这里填写 Agent 主机上的路径，并需要通过目录预检。
3. **迁移选项**：选择执行节点、目标端已有同名对象时的处理方式，以及节点分发方式（默认均衡分发；本地上传时由绑定的节点数决定）。下方还可以设置时间范围和任务限速。
4. 点击 **创建迁移任务**。任务会先扫描，再开始传输。

![创建迁移页面，红框按顺序标出迁移方向、源和目标、迁移选项、创建按钮](docs/images/create.zh-CN.png)

上传本地文件时，Agent 服务账号需要源目录的读权限；下载到本地时，需要目标目录的写权限。

### 5. 查看进度

在 **迁移任务** 页面点击任务，右侧会打开任务详情：

1. 停止或继续任务；点击 **调整**（未设置带宽上限时显示为 **限速**）可以在运行中修改带宽和请求上限。
2. **传输统计**：已扫描、已迁移、已跳过和失败的对象数及数据量，按实际传输的字节实时更新。
3. **请求速率**：LIST、GET、HEAD、PUT 每秒的实际请求数，以及任务上限和最终生效的上限。

<p align="center"><img src="docs/images/task-detail.zh-CN.png" width="440" alt="任务详情，红框标出停止和调整限速、传输统计、请求速率"></p>

在 **异常记录** 页面可以重试任务中失败的对象，已停止的任务可以继续传输，已完成的对象不会重复传输。任务停止或结束后，可以导出任务报告。

## 命令行

在已安装的 Master 或 Agent 主机上，用服务所属账号或管理员身份运行 CLI，会直接连接本机所属的集群，不需要登录：

```sh
oss-transfer-cli task list
oss-transfer-cli task progress <任务ID> --watch 5
oss-transfer-cli task limits <任务ID> --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli node list
```

CLI 可以完成页面上的全部操作，并支持 `--json` 输出，方便写脚本。远程登录、创建任务等用法见 [CLI 文档](docs/cli.zh-CN.md)。

## Docker Compose

已安装 Docker Engine 和 Compose 插件时运行：

```sh
mkdir oss-transfer && cd oss-transfer
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml
docker compose up -d master
docker compose logs master
```

在日志中找到 `admin` 的初始密码，然后登录。添加 Agent 时，在 **执行节点 → 添加节点** 中选择 **Docker**。镜像地址是 `moonwx/osstransfer`，目录挂载和升级方法见 [Docker Compose](docs/docker-compose.zh-CN.md)。

## 升级

建议在没有运行中的任务时升级。

1. 在 **运维工具 → 版本更新** 中更新 Master。
2. 在 **执行节点 → 版本更新** 中点击 **更新全部节点**，Agent 会逐台更新。

升级前会自动备份，新版本通过健康检查后删除备份；启动失败时会自动回滚。也可以重新运行安装命令来升级。

需要重装时，保留各角色的 `config` 目录和完整的 `data` 目录，包括 `.db.key` 密钥文件，安装器会继续使用原来的账号、任务和节点身份。计划降级时，应在升级前另外备份。详见[运维说明](docs/operations.zh-CN.md)。

## 注意事项

- **`SKIP` 不比较内容**：目标端已有同名对象或文件时直接跳过。需要确认内容一致时，使用[对象比对](docs/operations.zh-CN.md#对象比对)。
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
| [Docker Compose](docs/docker-compose.zh-CN.md) | 容器部署、目录挂载和升级 |
| [CLI](docs/cli.zh-CN.md) | 命令行的全部用法 |
| [运维说明](docs/operations.zh-CN.md) | 并发、对象比对、升级、故障转移、调整执行节点、断点续扫、重置密码 |
| [数据库维护](docs/database-maintenance.zh-CN.md) | 回收数据库文件空间 |
| [从 V4.0.x 升级](docs/migration.zh-CN.md) | 旧版本数据的离线转换 |

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
