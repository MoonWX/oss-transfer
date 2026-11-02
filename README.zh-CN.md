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

安装时可选择同时安装本机 Agent，安装完成后自动加入集群。新安装默认放在 `/opt/oss-transfer/`，可用 `--root-dir` 指定其他目录。脚本会自动选择 GitHub 或 OSS 镜像下载程序包。其他系统和自定义路径见[安装选项](docs/install.md)，无法访问互联网的主机见[离线安装](docs/offline-install.zh-CN.md)。

打开 `http://<master-ip>:1685`，使用 `admin` 和安装器输出的随机初始密码登录；首次登录必须修改密码。

## 添加 Agent

打开 **执行节点 → 添加节点**，选择主机操作系统，将生成的命令复制到对应主机上运行。Agent 默认从 Master 下载安装包；如果安装 Master 时已勾选本机 Agent，就可以直接开始迁移。

添加节点时，Master 地址默认使用当前浏览器地址。Agent 走内网时，将它改为 Master 的内网 URL。

## 开始迁移

1. 在管理页面添加存储连接。
2. 创建任务，选择源端、目标端和执行任务的 Agent。
3. 涉及本地文件时，填写 Agent 主机上的路径，并通过目录预检。Agent 服务账号上传时需要读权限，下载时需要写权限。
4. 启动任务，查看进度；结束后检查失败记录或下载报告。

在已安装的 Master 或 Agent 主机上，以服务所属账号或系统管理员身份运行 CLI，会直接连接本机所属集群，无需填写 `--host` 或登录：

```sh
oss-transfer-cli task list
oss-transfer-cli node list
```

任务控制、远程登录和脚本用法见 [CLI 文档](docs/cli.md)。

## 更新原生部署

先在 **运维工具 → 版本更新** 更新 Master，再到 **执行节点 → 版本更新** 点击“更新全部节点”来更新 Agent。更新 Master 不会自动触发 Agent 更新。尚无这些入口的旧版本需要先通过安装脚本升级一次；也可以重新运行安装命令。

重装时会识别并保留原配置、数据库和节点身份。旧安装继续使用原路径。升级成功后会删除本次临时备份。计划降级时，升级前要另做备份；需要保留的文件和数据库密钥见[备份与恢复说明](docs/operations.md)。

## Docker Compose

已安装 Docker Engine 和 Compose 插件时，运行：

```sh
mkdir oss-transfer && cd oss-transfer
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/docker-compose.yml -o docker-compose.yml
docker compose up -d master
docker compose logs master
```

打开 `http://<master-ip>:1685`，使用日志中的 `admin` 初始密码登录。在 **执行节点 → 添加节点** 中选择 **Docker** 添加 Agent。目录挂载和升级步骤见 [Docker Compose](docs/docker-compose.md)。

## 迁移时要知道

- `SKIP` 会保留目标端已有的同名对象或文件，不会比较内容。需要校验对象存储中的内容时，使用[对象比对](docs/operations.md#compare-objects)。
- 本地路径指 Agent 主机上的路径。多个 Agent 的挂载路径可以不同，但共享目录任务必须访问同一共享目录，并通过预检。
- 断点续扫继续的是同一轮扫描，不是持续同步，也不能保证发现断点之前的数据变化。需要迁移固定数据集时，应保持源端不变或使用快照。
