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

## 调整任务的执行节点

查看执行任务的 Agent 及其状态、扫描节点、待完成的子任务、各节点分得的任务限速份额、已移除的节点以及可以添加的 Agent。只有正在扫描（或正在接管扫描）的节点标记为扫描节点；扫描结束后改为提示扫描已完成。正在移出的离线节点会注明在执行租约到期后离开；没有租约隔离的任务则注明在重新连接后离开：

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK"
```

任务存在期间可以添加或移除 Agent。`--add` 和 `--remove` 可以重复使用，也可以用逗号分隔多个值。不加 `--yes` 时，命令只打印计划的调整（本地传输还会打印预检结果），不提交任何更改：

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK" --add 10.0.0.3 --yes
oss-transfer-cli task nodes "$TRANSFER_TASK" --remove 10.0.0.2 --yes
oss-transfer-cli task nodes "$TRANSFER_TASK" --remove 10.0.0.2 --now --yes
```

- 默认以排空方式移除：节点不再接收新工作，其排队的工作转给其他节点，正在运行的工作完成后离开。
- `--now` 撤销该节点正在运行的工作，由其余节点继续执行。部分任务不支持此方式，`task nodes` 会显示原因。
- 正在添加的节点显示为“加入中”，通过握手和共享目录检查后才会接收工作。

上传和下载任务需要为每个新增节点指定一个共享目录。CLI 提交前会运行 Master 的共享目录预检，任一绑定失败都会停止：

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK" --add 10.0.0.3 --binding 10.0.0.3=/mnt/share --yes
```

同一时间只能有一个调整在进行。有节点正在加入或排空时，Master 只接受以下调整：移除加入中的节点（取消加入）、在节点离开前重新添加排空中或正在立即移除的节点（取消移除；本地传输沿用原共享目录，不要指定 `--binding`）、用 `--now` 移除排空中的节点、移除工作已被接管的失联节点。其他调整会被拒绝并给出原因。至少要保留一个能接收工作的节点，且加入中的节点不计入：请先添加替换节点，等它加入后再移除旧节点。每次调整都带有 CLI 读取到的成员版本；如果期间有人修改了节点，Master 会拒绝本次调整，请重新运行 `task nodes` 后再提交。已停止或失败的任务可以调整，恢复时生效。已完成的任务以及单对象、单文件任务不能调整。

连接在结果返回前中断时，CLI 会打印请求 ID 和成员版本。请先用 `task nodes` 核对。需要重试时，保持原有参数并加上打印出的 `--request-id` 和 `--expected-revision`；Master 会返回原来的结果，不会重复执行调整。带 `--request-id` 重试时会跳过本地预检，因为 Master 已检查过原请求。

`task show` 也会列出节点成员，标出加入中和排空中的节点，并显示每个节点实际分得的份额。隔离、带宽重新分配和限制条件见[调整任务的执行节点](operations.zh-CN.md#调整任务的执行节点)。

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

用 `update master --version VERSION --yes` 更新 Master，再运行 `update agents --all --yes` 把 Agent 更新到它的版本。只更新一个 Agent 时，使用 `update agent --worker NODE --yes`。用 `update status --watch 5` 跟踪结果；请求被受理并不代表升级已经完成。在线升级需要托管的原生安装。

`update check` 只报告正式版。安装 `4.1.29-beta.1` 这类预发布版本时，需要指定精确版本并明确确认：

```sh
oss-transfer-cli update master --version 4.1.29-beta.1 --allow-prerelease --yes
```

不加 `--allow-prerelease` 时，预发布版本会在发送任何请求之前被拒绝，退出码为 2。为 `update agent` 或 `update agents` 指定预发布的 `--version` 时同样如此。不指定 `--version` 时，Agent 跟随 Master 的当前版本（包括预发布版本），无需该标志。见[预发布版本更新](operations.zh-CN.md#试用预发布版本)。

`backup list` 显示升级备份。`backup prune master --yes` 或 `backup prune agent --worker NODE --yes` 只删除服务认定可清理的备份。

## 语言和脚本

CLI 跟随系统语言，无法识别时使用英文。用 `config set language en`、`zh`、`ja` 或 `auto` 保存偏好；`--lang` 可为单条命令临时覆盖该偏好。

使用 `--json` 获取结构化输出。退出码：0 表示成功，1 表示操作失败，2 表示参数无效，3 表示结果不完整或不确定，130 表示被中断。只读命令支持 `--watch 5`。

账号登录时输入的密码不会显示。修改账号密码需要登录该账号；本机服务访问不会修改网页控制台的密码。`logout` 关闭账号会话，不会移除已安装服务的本机访问。

脚本可以用 `--password-env`、`--password-file` 或 `--password-stdin` 登录。`change-password` 接受 `--old-password-env` 和 `--new-password-env`，或对应的文件选项。
