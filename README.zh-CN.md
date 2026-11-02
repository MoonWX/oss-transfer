# OSS Transfer

[English](README.md)

在 S3 兼容对象存储之间迁移数据，或上传、下载本地文件。Master 提供管理页面并调度任务，Agent 负责扫描和传输数据。

## 安装 Master

在 Linux 上运行：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash
```

中国大陆：

```sh
curl -fsSL https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/install.sh | sudo bash
```

按提示选择 Master、原生服务，以及是否同时安装本机 Agent。本机 Agent 安装后会自动加入集群。脚本自动选择 GitHub 或 OSS 镜像下载程序包，程序、配置和数据默认放在 `/opt/oss-transfer/`。

其他系统和自定义目录见[安装选项](docs/install.md)，内网环境见[离线安装](docs/offline-install.zh-CN.md)。

打开 `http://<master-ip>:1685`，使用 `admin` 和安装器输出的随机初始密码登录；首次登录必须修改密码。

## 添加 Agent

打开 **执行节点 → 添加节点**，填写节点名称和 Agent 能访问的 Master 地址。生成邀请后，复制对应系统的命令，在 Agent 主机上执行。Agent 会从 Master 下载安装器和程序包。同机安装的 Agent 已经加入，无需再次添加。

两台主机在同一内网时，填写 Master 的内网地址即可；浏览器可以使用另一个地址或反向代理。需要从公网下载安装包时，可以在加入窗口中切换。

## 开始迁移

1. 在管理页面添加存储连接。
2. 创建任务，选择源端、目标端和执行任务的 Agent。
3. 涉及本地文件时，填写 Agent 主机上的路径，并通过目录预检。Agent 服务账号上传时需要读权限，下载时需要写权限。
4. 点击创建，任务会自动开始扫描和传输。之后可查看进度、重试失败文件，结束后下载报告。

在已安装的 Master 或 Agent 主机上，以服务所属账号或系统管理员身份运行 CLI，会直接连接本机所属集群，无需填写 `--host` 或登录：

```sh
oss-transfer-cli task list
oss-transfer-cli node list
```

任务控制、远程登录和脚本用法见 [CLI 文档](docs/cli.md)。

## 更新原生部署

尽量等任务完成后升级。先在 **运维工具 → 版本更新** 更新 Master，再到 **执行节点 → 版本更新** 点击“更新全部节点”，逐台更新 Agent。也可以重新运行安装命令。

重装时，保留各角色的配置和完整数据目录，包括 `.db.key` 密钥文件。安装器会恢复程序，继续使用原账号、任务和节点身份。旧安装沿用原路径，自定义目录的操作见[重装说明](docs/install.md#reinstall-without-losing-data)。

## Docker Compose

已安装 Docker Engine 和 Compose 插件时，运行：

```sh
mkdir oss-transfer && cd oss-transfer
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml
docker compose up -d master
docker compose logs master
```

打开 `http://<master-ip>:1685`，使用日志中的 `admin` 初始密码登录。在 **执行节点 → 添加节点** 中选择 **Docker** 添加 Agent。目录挂载和升级步骤见 [Docker Compose](docs/docker-compose.md)。

## 使用和升级时要知道

- `SKIP` 会保留目标端已有的同名对象或文件，不会比较内容。需要校验对象存储中的内容时，使用[对象比对](docs/operations.md#compare-objects)。
- 本地路径指 Agent 主机上的路径。共享目录预检会写入小文件：上传时至少一个节点需要对源目录有写权限，下载时每个选中节点都需要对目标目录有写权限。挂载路径可以不同，但必须指向同一共享目录。
- 断点续扫继续的是同一轮扫描，不是持续同步，也不能保证发现断点之前的数据变化。需要迁移固定数据集时，应保持源端不变或使用快照。

升级成功后会删除本次临时备份。计划降级时，应提前另存配置、数据和对应版本的程序，具体见[备份与恢复说明](docs/operations.md)。
