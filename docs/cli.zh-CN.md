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

对运行中的任务，`task retry <ID>` 只重试已结束子任务中的失败对象，不停止任务。这些子任务在原节点上按节点并发上限重新执行，已成功或已跳过的对象不会重复传输。输出会显示重试的子任务数和对象数。仍在运行的子任务在结束前保留其失败对象；`task retry --all --yes` 仍只选择失败和部分失败的任务。节点上的 Agent 需要升级到本版本；对旧版 Agent 或 Master 会报告原因，且不在其上重试。详见[运行中任务重试失败项](operations.zh-CN.md#运行中任务重试失败项)。

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

本地传输执行与控制台相同的预检。`--validate-only` 只检查配置，不创建任务。`--bandwidth-mbps`（0.1–1,000,000 Mbps）以及 `--list-qps`、`--head-qps`、`--put-qps` 在创建任务时设置限速；与下文的限速一样，它们是任务所有节点的合计值。

下载到本地目录（`oss-to-local`）时可使用 `--ranged-download auto|off`。默认值 `auto` 让每个 Agent 把 128 MiB 及以上的对象按 8 MiB 字节范围并发下载（每个对象 4 个，节点有空闲能力时最多 16 个），并严格按顺序写入文件，几乎所有任务都应保持默认。限速会约束分段：每个对象同时持有的分段最多相当于该任务在此节点上约一秒的带宽份额（节点带宽上限更低时按节点上限计算）；设有 LIST / GET 限制时，还不超过该任务在此节点上约一秒的 GET 份额，因为每个分段都是一次 GET。一秒的份额不足两个分段时，改为单流下载（见[大对象](operations.zh-CN.md#大对象)）。`off` 对每个对象使用单个顺序流下载；仅在目标存储或网络不适合并发分段下载（Agent 内存紧张、存储会限制大量并发 GET）或技术支持要求时使用。该设置在任务整个生命周期内固定，续传、重试和调整节点时保持不变。`task show` 会为目标为本地的任务显示该设置。其他方向不接受此参数。`task import local` 的 JSON 中对应字段为 `"rangedDownload": "auto"` 或 `"off"`，省略即为 `auto`。早于此选项的 Agent 无论如何设置，都按单个流下载每个对象（见[分段下载选项](operations.zh-CN.md#分段下载选项)）。

```sh
oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download --ranged-download off
```

调整运行中任务的限速：

```sh
oss-transfer-cli task limits "$TRANSFER_TASK" --list-qps 100 --head-qps 500 --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli task limits "$TRANSFER_TASK" --put-qps unlimited
```

未指定的字段保持原值；`unlimited` 解除对应的限制（QPS 选项也可以写 0）；不带新值运行 `task limits ID` 或 `task bandwidth ID` 会显示任务当前的限速：带宽上限、请求限速范围、LIST / GET、HEAD 和 PUT 上限、是否所有节点都已生效，以及未生效节点的原因。`task bandwidth ID MBPS|unlimited` 只修改带宽限制。QPS 是平均速率，可以是小数，范围为 0.000001–1,000,000,000：`--list-qps 0.5` 表示整个任务每 2 秒发出一个列举或 GET 请求。LIST 限制同时适用于 GET 读取（含区间读取）；PUT 包括分片上传请求。已停止或已结束的任务不能修改限速。

`task show ID` 显示当前限速及其适用范围，并把实时请求速率与任务上限、生效上限对照列出。*请求速率*、*任务请求上限* 和 *生效请求上限* 各行保留一位小数，低于 0.05 的上限显示为 0；*LIST / GET QPS*、*HEAD QPS*、*PUT QPS* 各行显示精确值。`task nodes ID` 显示每个节点的份额。任务限速是总量：Master 按需求把它分给活动的 Agent，有工作的节点平分绝大部分限速，空闲节点只保留很小的下限（见[任务限速在节点间的分配](operations.zh-CN.md#任务限速在节点间的分配)）。节点限速（见[设置节点限速](#设置节点限速)）仍然生效，请求必须同时满足两者。

## 调整任务的执行节点

查看执行任务的 Agent 及其状态、扫描节点、待完成的子任务、各节点的需求和当前分得的任务限速份额、已移除的节点以及可以添加的 Agent。需求列显示 *按需（忙）* 或 *空闲（已释放）*；节点尚未确认降额时附加 *调整中*。尚未分得份额或已不再持有份额的节点（加入中或已断开）在需求列和份额列显示 `—`。旧版 Master 按平分计算，所有节点的需求列都显示 `—`。只有正在扫描（或正在接管扫描）的节点标记为扫描节点。表格下方会提示扫描已完成、扫描正在移交给新的扫描节点，或原扫描节点已退出、将由其他节点接管；移交后的扫描从 Master 保存的断点继续。正在移出的离线节点会注明在执行租约到期后离开；没有租约隔离的任务则注明在重新连接后离开：

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
- 正在添加的节点显示为“加入中”，直到通过握手和共享目录检查；任务设有限速时，还要等其他节点确认降低后的份额。之后它才会接收工作。

变更受理后，CLI 会打印调整后的成员。仍有节点在加入或排空时，变更在后台继续完成，可用 `task nodes "$TRANSFER_TASK" --watch 5` 跟踪。变更过程中的各个阶段（例如离开的节点等待租约到期、扫描移交给其他节点）显示在这个视图中，不作为任务错误报告。

上传和下载任务需要为每个新增节点指定一个共享目录。CLI 提交前会运行 Master 的共享目录预检，任一绑定失败都会停止：

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK" --add 10.0.0.3 --binding 10.0.0.3=/mnt/share --yes
```

同一时间只能有一个调整在进行。有节点正在加入或排空时，Master 只接受以下调整：移除加入中的节点（取消加入）、在节点离开前重新添加排空中或正在立即移除的节点（取消移除；本地传输沿用原共享目录，不要指定 `--binding`）、用 `--now` 移除排空中的节点、移除工作已被接管的失联节点。其他调整会被拒绝并给出原因。至少要保留一个能接收工作的节点，且加入中的节点不计入：请先添加替换节点，等它加入后再移除旧节点。每次调整都带有 CLI 读取到的成员版本；如果期间有人修改了节点，Master 会拒绝本次调整，请重新运行 `task nodes` 后再提交。已停止或失败的任务可以调整，恢复时生效。已完成的任务以及单对象、单文件任务不能调整。

连接在结果返回前中断时，CLI 会打印请求 ID 和成员版本。请先用 `task nodes` 核对。需要重试时，保持原有参数并加上打印出的 `--request-id` 和 `--expected-revision`；Master 会返回原来的结果，不会重复执行调整。带 `--request-id` 重试时会跳过本地预检，因为 Master 已检查过原请求。

`task show` 也会列出节点成员，标出加入中和排空中的节点，显示每个节点的需求和实际分得的份额，并给出同样的扫描提示。隔离、带宽重新分配和限制条件见[调整任务的执行节点](operations.zh-CN.md#调整任务的执行节点)。

## 管理节点和异常

```sh
oss-transfer-cli node list
oss-transfer-cli agent invite --label storage-01
```

在新节点上运行打印出的加入命令。邀请包含一个 ID，可用于 `agent invite-status`。如果 Agent 需要通过私有地址访问 Master，创建邀请时设置 `--master-url`。`node update` 支持在线修改 `--child-concurrency`（1–256，默认 32）和 `--object-concurrency`（每个子任务的对象并发，1–64，默认 4），两者乘积不能超过 1024，见[节点并发](operations.zh-CN.md#节点并发)。

```sh
oss-transfer-cli exceptions list failed --task-id "$TRANSFER_TASK"
oss-transfer-cli task retry "$TRANSFER_TASK"
oss-transfer-cli exceptions clear failed --task-id "$TRANSFER_TASK" --yes
```

清理只删除已通过重试成功或跳过而解决的记录，未解决的失败继续显示。本地传输的失败使用 `local`，查看回退记录使用 `fallback`。

## 设置节点限速

节点限速限制单个 Agent 每秒的 LIST、PUT、HEAD 请求数和带宽，由该节点上的所有任务共用（见[节点限速](operations.zh-CN.md#节点限速)）：

```sh
oss-transfer-cli limit list
oss-transfer-cli limit set put 500 --all
oss-transfer-cli limit set bandwidth 200 --worker 10.0.0.2
oss-transfer-cli limit set head unlimited --worker 10.0.0.2
```

`limit list` 为每个节点的每种类型（`list`、`put`、`head`、`bandwidth`）显示一行：目标上限及其来源（等于 Agent `service.json` 中 `nodeControl` 的值或内置默认值时为 *配置目标*，否则为 *已保存目标*）、运行上限、实测速率和状态，状态为 *正常*、*已降速*、*恢复中*、*已恢复* 或 *不限速*（见[限速状态](operations.zh-CN.md#限速状态)）。数值按精确值输出。没有响应的节点不会列出，命令以退出码 3 结束。加 `--watch 5` 可以跟踪自动降速的过程。

`limit set 类型 数值` 需要指定 `--worker 节点` 或 `--all` 之一；`--all` 为每个节点设置相同的值。请求上限的单位为次/秒，可以是小数，范围为 0.01–1,000,000。带宽默认以 Mbps 为单位，可用 `--unit bps|Kbps|Mbps|Gbps` 指定其他单位。数值必须大于 0；解除限制请使用 `unlimited`。该值成为节点的已保存目标：立即生效，结束正在进行的自动降速，重启后仍然保留。如果该上限从未保存过且正按配置值运行，设置同一个值不会被记录，之后修改 `service.json` 仍会对它生效。有 Agent 未确认时，输出会列出该节点，命令以退出码 1 结束。

```sh
oss-transfer-cli limit history --limit 100
oss-transfer-cli limit adjustment --worker 10.0.0.2 --event-id EVENT_ID
```

`limit history` 按时间倒序列出所有节点最近的手动和自动调整；`--limit` 接受 1–200（默认 50）。`limit adjustment` 显示某次调整、触发它的请求以及该轮调节的每一步：传入节点和历史记录中的 `id`；要查看后续 100 步，再加上 `--cursor` 和输出中的 `nextCursor`。这两个命令输出原始字段，时间为自 1970 年起的毫秒数，带宽单位为 bit/s；脚本中请使用 `--json`。另见[存储限流](operations.zh-CN.md#存储限流)。

V4 的命令 `rate-limit`、`list-rate-limit`、`rate-limit-history [N]` 和 `set-rate-limit 类型 数值 [IP]` 仍然可用。`set-rate-limit` 把不带 K、M、G 后缀的带宽值视为 bit/s，把 0 视为不限速；不指定 IP 时应用到所有节点。

## 比对存储的对象

`object compare` 启动一次比对：用 `--scope object` 比对单个对象，用 `--scope prefix` 比对存储桶或前缀；比对由 Master 安排在某个 Agent 上执行。`--mode metadata` 通过 HEAD 请求比较大小和元数据；`--mode content` 以流式方式对两端对象计算 SHA-256，不在磁盘上暂存。检查方式和结果见[对象比对说明](operations.zh-CN.md#对象比对)，参数见 `help object`。

```sh
oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --scope prefix --source-prefix 2026/ --dest-prefix 2026/ --mode metadata
oss-transfer-cli object list
```

`--source-bucket` 和 `--dest-bucket` 使用 `连接名称/存储桶` 格式。前缀比对使用 `--source-prefix` 和 `--dest-prefix`（为空表示整个存储桶）；单对象比对使用 `--source-key` 和 `--dest-key`，指定了 Key 时默认按单对象比对。默认方式为 `metadata`。`object compare` 打印新比对的 ID 和跟踪它的命令；加 `--json` 时输出比对记录。`object list` 按开始时间倒序列出所有节点上的比对：ID、状态、方式、范围、源、目标、已检查的对象数、差异数、开始时间和节点。有节点未响应时，输出会列出这些节点，命令以退出码 3 结束。

从列表中选择一个 ID：

```bash
read -r -p 'Comparison ID: ' TRANSFER_COMPARISON
oss-transfer-cli object status "$TRANSFER_COMPARISON"
oss-transfer-cli object status "$TRANSFER_COMPARISON" --status MISSING_TARGET,DIFFERENT --limit 1000
oss-transfer-cli object export "$TRANSFER_COMPARISON" --output "comparison-$TRANSFER_COMPARISON.zip"
```

`object status` 显示比对的状态、方式、范围、源和目标、各项计数、已保存的差异、已用时间和速率、已读取的字节数（内容模式）、检查点、是否可继续、停止保存差异的原因和最后保存的 Key，以及错误和错误详情。之后列出一页已保存的结果行：编号、结果、源 Key、目标 Key、两端大小和说明（不同的属性，或错误及其详情）。`--status` 只保留指定的结果，多个值用逗号分隔：`DIFFERENT`、`ETAG_MISMATCH`、`MISSING_TARGET`、`EXTRA_TARGET`、`CHANGED_DURING_COMPARE`、`ERROR` 和 `BOTH_MISSING`。`--limit` 设置每页行数，范围 1–1,000（默认 50），`--after N` 从第 N 行之后开始；后面还有结果行时，命令会打印下一页使用的 `--after` 值。比对已停止保存差异时，命令以退出码 3 结束。

`object export ID --output 文件` 保存一个 ZIP，包含 `differences.csv`、`retry-source-keys.incr` 和 `summary.json`（见[导出结果](operations.zh-CN.md#导出结果)）；加 `--format jsonl` 时改为每行保存一条结果。`--status` 与上文相同，用于限制导出的结果行；重传 Key 清单始终包含所有 `MISSING_TARGET`、`DIFFERENT` 和 `ETAG_MISMATCH` 行。输出文件不能已存在：CLI 先下载到同一目录下的临时文件，下载完整并通过校验后才以指定的文件名发布。导出 ZIP 后，命令打印各项计数、导出是否完整以及重传 Key 的数量；导出不完整时以退出码 3 结束。

```sh
oss-transfer-cli object resume "$TRANSFER_COMPARISON"
oss-transfer-cli object cancel "$TRANSFER_COMPARISON"
oss-transfer-cli object delete "$TRANSFER_COMPARISON"
```

`object resume` 从检查点继续已中断、失败或已取消的前缀比对（见[继续比对](operations.zh-CN.md#继续比对)）。`object cancel` 停止正在运行的比对，已取消的前缀比对之后仍可继续。`object delete` 删除未在运行的比对的结果。4.1.40 及更早版本的 Master 不支持 `object list`、`object export`、`object resume` 和 `--status`，此时 CLI 会提示先升级 Master。

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
