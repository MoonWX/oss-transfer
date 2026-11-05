# 使用 CLI

[English](cli.md)

命令行工具（CLI）可以管理与网页控制台相同的任务和设置。在已安装 Master 或 Agent 的主机上，以服务所有者或系统管理员身份运行时，它直接使用本机集群，无需登录或指定地址。

```sh
oss-transfer-cli task list
oss-transfer-cli node list
```

Docker Compose 部署使用 `docker compose exec master oss-transfer-cli task list`。已加入集群的 Agent 主机的管理员也可以通过这种本机访问管理集群。其他系统用户使用网页控制台账号执行 `login`。

## 连接其他 Master

单条命令使用 `--host`，或保存连接供后续命令使用：

```bash
read -r -p 'Master URL: ' TRANSFER_MASTER
oss-transfer-cli config set master "$TRANSFER_MASTER"
oss-transfer-cli login
oss-transfer-cli task list
```

`config set master auto` 恢复使用本机安装的集群。`config get master` 显示当前地址及其来源。地址按 `--host`、`OSS_TRANSFER_MASTER_SERVER`、已保存的设置、本机安装的顺序选择。独立使用的 CLI 若以上都没有，使用 `http://localhost:1685`。会话属于某个 Master 地址；连接另一个 Master 需要单独登录。

## 查看和控制任务

列出全部任务，包括第一页之后的任务：

```sh
oss-transfer-cli task list --all
```

从列表中选择一个 ID：

```bash
read -r -p 'Task ID: ' TRANSFER_TASK
oss-transfer-cli task show "$TRANSFER_TASK"
oss-transfer-cli task children "$TRANSFER_TASK" --active
oss-transfer-cli task progress "$TRANSFER_TASK" --watch 5
```

子任务按控制台的状态优先级排序。还有下一页时，命令会打印获取下一页的命令。`--order id` 按编号列出。`task progress` 显示正在传输的文件字节数；加 `--child ID-N` 查看某个子任务。

```sh
oss-transfer-cli task stop "$TRANSFER_TASK"
oss-transfer-cli task resume "$TRANSFER_TASK"
```

恢复会继续未完成的工作，并保留已完成的进度。重试失败的任务时，把它们的 ID 传给 `task retry`，或用 `task retry --all --yes` 重试所有失败和部分失败的任务。请求超时时，CLI 会报告结果未知的任务 ID，并停止发送后续批次。重试前请先检查这些任务。

导出已停止或已结束任务的报告：

```sh
oss-transfer-cli task report "$TRANSFER_TASK" --output "task-$TRANSFER_TASK.zip"
```

`task delete` 把记录移到回收站，`task restore` 恢复它们。`task purge ID --yes` 开始永久清理记录；用 `task cleanup-status ID --watch 5` 跟踪进度。这些操作不会删除已迁移的文件。

## 创建任务和设置限速

CLI 支持对象存储之间的迁移、上传和下载。帮助中列出了每个方向所需的路径、存储连接和节点：

```sh
oss-transfer-cli help task create
oss-transfer-cli help task import
oss-transfer-cli help storage
```

本地传输执行与控制台相同的预检。`--validate-only` 只检查配置，不创建任务。

调整运行中任务的限速：

```sh
oss-transfer-cli task limits "$TRANSFER_TASK" --list-qps 100 --head-qps 500 --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli task limits "$TRANSFER_TASK" --put-qps unlimited
```

未指定的字段保持原值。LIST 限制同时适用于 GET 读取；PUT 包括分片上传请求。任务限速由所选 Agent 共享，节点限速仍然生效。

## 管理节点和异常

```sh
oss-transfer-cli node list
oss-transfer-cli agent invite --label storage-01
```

在新节点上运行打印出的加入命令。邀请包含一个 ID，可用于 `agent invite-status`。如果 Agent 需要通过私有地址访问 Master，创建邀请时设置 `--master-url`。`node update` 支持在线修改 `--child-concurrency` 和 `--object-concurrency`。

```sh
oss-transfer-cli exceptions list failed --task-id "$TRANSFER_TASK"
oss-transfer-cli task retry "$TRANSFER_TASK"
oss-transfer-cli exceptions clear failed --task-id "$TRANSFER_TASK" --yes
```

清理只删除已通过重试成功或跳过而解决的记录，未解决的失败继续显示。本地传输的失败使用 `local`，查看回退记录使用 `fallback`。

## 比对存储的对象

`object compare` 用 `--scope object` 检查单个对象，用 `--scope prefix` 检查存储桶或前缀。`--mode metadata` 比较元数据；`--mode content` 以流式方式对两端对象计算 SHA-256，不在磁盘上暂存。使用返回的比对 ID 执行 `object status`、`object cancel` 和 `object delete`。检查方式见[对象比对说明](operations.zh-CN.md#对象比对)，参数见 `help object`。

## 更新集群

```sh
oss-transfer-cli update check
oss-transfer-cli update status
oss-transfer-cli help update
```

用 `update master --version 版本 --yes` 更新 Master，再运行 `update agents --all --yes` 把 Agent 更新到它的版本。只更新一个 Agent 时，使用 `update agent --worker 节点 --yes`。用 `update status --watch 5` 跟踪结果；请求被受理并不代表升级已经完成。在线升级需要托管的原生安装。

`update check` 只报告正式版。安装 `4.1.29-beta.1` 这类预发布版本时，需要指定精确版本并明确确认：

```sh
oss-transfer-cli update master --version 4.1.29-beta.1 --allow-prerelease --yes
```

不加 `--allow-prerelease` 时，预发布版本会在发送任何请求之前被拒绝，退出码为 2。为 `update agent` 或 `update agents` 指定预发布的 `--version` 时同样如此。不指定 `--version` 时，Agent 跟随 Master 的当前版本（包括预发布版本），无需该标志。见[预发布版本更新](operations.zh-CN.md#试用预发布版本)。

`backup list` 显示升级备份。`backup prune master --yes` 或 `backup prune agent --worker 节点 --yes` 只删除服务认定可清理的备份。

## 语言和脚本

CLI 跟随系统语言，无法识别时使用英文。用 `config set language en`、`zh`、`ja` 或 `auto` 保存偏好；`--lang` 只对单条命令生效。

使用 `--json` 获取结构化输出。退出码：0 表示成功，1 表示操作失败，2 表示参数无效，3 表示结果不完整或不确定，130 表示被中断。只读命令支持 `--watch 5`。

账号登录时输入的密码不会显示。修改账号密码需要登录该账号；本机服务访问不会修改网页控制台的密码。`logout` 关闭账号会话，不会移除已安装服务的本机访问。

脚本可以用 `--password-env`、`--password-file` 或 `--password-stdin` 登录。`change-password` 接受 `--old-password-env` 和 `--new-password-env`，或对应的文件选项。
