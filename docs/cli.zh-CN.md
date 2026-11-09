# 使用 CLI

[English](cli.md)

命令行工具 `oss-transfer-cli` 可以管理与网页控制台相同的任务和设置。本页先介绍如何连接和一次完整的迁移，再逐一说明各组命令。`oss-transfer-cli --help` 列出全部命令，`oss-transfer-cli help 命令组`（例如 `help task create`、`help storage`）给出该组的说明和示例。

## 连接 Master

**在 Master 或执行节点（Agent）主机上。** 安装脚本把 CLI 安装到 `/usr/local/bin/oss-transfer-cli`（Windows 为安装目录中的 `bin\oss-transfer-cli.exe`）。以 root 或服务所有者身份运行时，它直接使用本机集群，无需登录或指定地址。

```sh
sudo oss-transfer-cli task list
sudo oss-transfer-cli node list
```

已加入集群的执行节点主机的管理员也可以通过这种本机访问管理集群。其他系统用户使用网页控制台账号登录。

**Docker Compose 部署** 在 Master 容器中运行：`docker compose exec master oss-transfer-cli task list`。

**在其他电脑上**，先[安装 CLI](install.zh-CN.md#在其他电脑上安装-cli)，再保存 Master 地址并登录：

```sh
oss-transfer-cli config set master http://10.0.0.5:1685
oss-transfer-cli login
oss-transfer-cli whoami
```

`login` 默认以 `admin` 登录（其他账号加 `--username`），以不回显的方式询问密码，成功后显示 `已登录 http://10.0.0.5:1685（用户 admin）`。`whoami` 显示 Master、用户和角色。`logout` 结束会话（`已注销 …`）。首次登录后，用 `change-password` 修改初始密码；新密码至少 8 个字符，不能是常见或容易猜到的密码（如 `12345678`），且不能与用户名或当前密码相同。会话属于某个 Master 地址，连接另一个 Master 需要单独登录。

`--host URL` 为单条命令指定 Master。否则地址依次来自 `OSS_TRANSFER_MASTER_SERVER`、已保存的设置（`config set master`）和本机安装。`config get master` 显示当前地址，并用文字说明其来源，例如 *来源：自动识别本机安装*。`config set master auto` 恢复使用本机安装的集群。以上都没有时，CLI 使用 `http://localhost:1685`；该地址无法连接时，CLI 会说明原因，并建议运行 `config set master URL` 或使用 `--host`。

## 一次完整的迁移

本示例把一个对象存储上存储桶 `photos` 中前缀 `2026/` 下的对象复制到另一个对象存储的存储桶 `archive`，然后核对结果。在 Master 主机上运行，或先 `login`。

1. 确认执行节点在线。需要时添加一个：`agent invite` 会打印需要在新主机上运行的加入命令（见[添加和移除执行节点](#添加和移除执行节点)）。

   ```sh
   oss-transfer-cli node list
   ```

2. 添加两个存储连接。凭据来自环境变量或文件，从不写在命令行上。保存时会由一个执行节点检查连接（见[存储连接](#存储连接)）。

   ```sh
   export SRC_AK=... SRC_SK=... DST_AK=... DST_SK=...
   oss-transfer-cli storage add source --endpoint https://s3.example.com --access-key-env SRC_AK --secret-key-env SRC_SK
   oss-transfer-cli storage add target --endpoint http://10.0.0.5:9000 --access-key-env DST_AK --secret-key-env DST_SK
   oss-transfer-cli storage test target --bucket archive
   ```

3. 创建任务。`--conflict changed` 让之后的增量轮次只覆盖有变化的对象（见[冲突策略](operations.zh-CN.md#冲突策略)）：

   ```sh
   oss-transfer-cli task create oss-to-oss source/photos target/archive --prefix 2026/ --conflict changed
   ```

   输出 `已创建 1 个任务：#12` 和 `查看进度：oss-transfer-cli task show 12 --watch 5`。创建成功只表示任务已被受理。

4. 跟踪任务，直到状态变为 *已完成*（或 *部分失败*）。按 Ctrl-C 结束跟踪。

   ```sh
   oss-transfer-cli task show 12 --watch 5
   ```

5. 有对象失败时，查看原因并重试（见[异常记录和重试](#异常记录和重试)）：

   ```sh
   oss-transfer-cli exceptions list failed --task-id 12
   oss-transfer-cli task retry 12
   ```

6. 保存报告。每个对象都有最终结果时，输出以 *报告完整：是* 结尾（见[任务报告](#任务报告)）：

   ```sh
   oss-transfer-cli task report 12 --output task-12.zip
   ```

7. 比对源端和目标端，然后查看差异（见[比对存储的对象](#比对存储的对象)）：

   ```sh
   oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --source-prefix 2026/ --dest-prefix 2026/
   oss-transfer-cli object status 比对ID
   oss-transfer-cli object export 比对ID --output comparison.zip
   ```

   已完成且没有差异的比对显示 *未发现差异*。

## 输出、语言和退出码

**面向人的输出。** CLI 显示翻译后的状态、策略和原因，数字完整显示，大小易读，时间为本地时间并带 UTC 偏移，例如 `2026-11-10 08:30:00 +08:00`。写操作只打印简短的确认，例如 `#12  已请求停止`。错误显示为 `<说明>（HTTP n）`，有提示时一并给出，例如 `提示: 请运行 oss-transfer-cli login 登录。`；Master 建议使用 Path-style 时，还会给出 `storage update 连接名 --path-style true` 命令。

**`--json`** 输出 Master 返回的结构化结果，保留原始代码（`RUNNING`、`OVERWRITE_CHANGED`、`SOURCE_CHANGED` 等），不随语言变化，适合脚本使用。与 `--watch` 一起使用时，每次刷新输出一行 JSON。

**`--verbose`** 把请求路径和 HTTP 状态码写到标准错误，并在翻译后的存储错误之外显示原始错误（`详情: …`）。不加该选项而存在原始错误时，CLI 提示 `提示: 使用 --verbose 查看原始错误信息。`

**`--watch N`** 每 N 秒（1–3600）刷新一次只读命令，例如 `task list`、`task show`、`node list`、`limit list` 或 `update status`。

**`--version`** 显示 `OSS Transfer CLI v4.2.0`，以及包含提交、分支和构建时间的一行。`--version --json` 以字段形式输出同样的信息，供脚本使用。

**退出码：**

| 退出码 | 含义 |
| --- | --- |
| 0 | 成功 |
| 1 | 操作失败：Master 或存储拒绝或执行失败，或会话缺失、无权限（HTTP 401 或 403） |
| 2 | 发送任何请求之前发现的命令行错误：未知命令或选项、参数缺失或无效、缺少 `--yes`、`--output` 文件已存在 |
| 3 | 结果不完整：列表不完整（部分节点未响应）、报告或比对导出不完整，或只创建了部分任务 |
| 130 | 被中断（Ctrl-C） |

**语言。** CLI 跟随系统语言，无法识别时使用英文。用 `config set language en`、`zh`、`ja` 或 `auto` 保存偏好；`--lang`（或 `OSS_TRANSFER_LANG`）可为单条命令临时覆盖。JSON 的键和代码在各种语言下都相同。

## 存储连接

```sh
oss-transfer-cli storage list
oss-transfer-cli storage add 连接名 --endpoint URL --access-key-env 变量名 --secret-key-env 变量名 [选项]
oss-transfer-cli storage update 连接名 [选项]
oss-transfer-cli storage test 连接名 [--bucket 存储桶]
oss-transfer-cli storage buckets 连接名 [--bucket 存储桶]
oss-transfer-cli storage remove 连接名 --yes
```

| 选项 | 说明 |
| --- | --- |
| `--endpoint` | `http://` 或 `https://`、主机和可选端口，不含路径，例如 `https://s3.example.com` 或 `http://10.0.0.5:9000` |
| `--region` | 签名区域；省略时为 `us-east-1` |
| `--access-key-env`、`--secret-key-env`（或 `-file`） | 保存密钥的环境变量或文件 |
| `--session-token-env`（或 `-file`）、`--clear-session-token` | 临时凭据 |
| `--path-style auto\|true\|false` | 寻址方式。`auto`（默认）在 Endpoint 带端口、使用 IP 地址、`localhost`、不含点的主机名，或以 `.internal`、`.local`、`.lan` 结尾时使用 Path-style，否则使用 Virtual-hosted。`true` 总是使用 Path-style，`false` 总是使用 Virtual-hosted |
| `--v2-signer default\|true\|false` | 签名版本：`default` 为 V4；`true` 为需要 V2 的服务使用 V2 |
| `--validate true\|false` | 保存前检查连接；默认 `true` |

`storage list` 显示每个连接（凭据已遮盖）、签名版本和寻址方式，例如 *自动（Path-style）*。`storage test` 先列举存储桶，再在 `--bucket` 指定的存储桶（或第一个存储桶）中列举一个对象，显示 *连接测试通过：已列举存储桶，并用当前寻址方式访问了一个存储桶*、寻址方式、存储桶级检查结果以及警告。只授权到单个存储桶的密钥无法列举存储桶：保存时只会警告，可用 `storage test 连接名 --bucket 存储桶` 检查该存储桶。失败时会显示原因和处理方法，见[存储连接检查失败](operations.zh-CN.md#存储连接检查失败)。连接的修改会同步到每个节点，命令会列出仍在等待同步的节点。

## 查看和控制任务

列出全部任务，包括第一页之后的任务：

```sh
oss-transfer-cli task list --all
oss-transfer-cli task list --status PARTIAL_FAILED
```

从列表中选择一个 ID：

```bash
read -r -p 'Task ID: ' TRANSFER_TASK
oss-transfer-cli task show "$TRANSFER_TASK"
oss-transfer-cli task children "$TRANSFER_TASK" --active
oss-transfer-cli task progress "$TRANSFER_TASK" --watch 5
```

`task show` 显示状态、方向、冲突策略、计数、限速和节点；有需要关注的情况时，增加一行 *需要关注* 说明原因（见[任务详情](operations.zh-CN.md#任务详情需要关注与失败摘要)）。遇到常见的存储错误时，会在原始失败原因上方显示翻译后的原因说明。使用 *跳过同名对象或文件* 且有对象被跳过的任务，会提示这些对象只按名称判断。

子任务按控制台的状态优先级排序。还有下一页时，命令会打印获取下一页的命令。`--order id` 按编号列出。`task progress` 显示正在传输的文件字节数；加 `--child ID-N` 查看某个子任务。

```sh
oss-transfer-cli task stop "$TRANSFER_TASK"
oss-transfer-cli task resume "$TRANSFER_TASK"
```

每个任务输出一行，例如 `#12  已请求停止` 或 `无需处理（当前状态：…）`。继续会接着处理未完成的工作，并保留已完成的进度。重试失败的任务时，把它们的 ID 传给 `task retry`，或用 `task retry --all --yes` 重试所有失败和部分失败的任务。请求超时时，CLI 会报告结果未知的任务 ID，并停止发送后续批次。重试前请先检查这些任务。各种操作的区别见[重试、继续与恢复](operations.zh-CN.md#重试继续与恢复)。

对运行中的任务，`task retry <ID>` 只重试已结束子任务中的失败对象，不停止任务。这些子任务在原节点上按节点并发上限重新执行，已成功或已跳过的对象不会重复传输。输出会显示重试的子任务数和对象数。仍在运行的子任务在结束前保留其失败对象；`task retry --all --yes` 仍只选择失败和部分失败的任务。详见[运行中任务重试失败项](operations.zh-CN.md#运行中任务重试失败项)。

`task delete` 把记录移到回收站，`task restore` 恢复它们。`task purge ID --yes` 开始永久清理记录；用 `task cleanup-status ID --watch 5` 跟踪进度。这些操作不会删除已迁移的文件。

## 创建任务和设置限速

CLI 支持对象存储之间的迁移（`oss-to-oss`）、下载（`oss-to-local`）和上传（`local-to-oss`）。`help task create` 列出全部选项，[迁移选项](operations.zh-CN.md#迁移选项)解释了它们的含义。

```sh
oss-transfer-cli task create oss-to-oss source/photos target/archive --prefix 2026/ --mapping replace-prefix --target-prefix backup/2026/ --conflict changed
oss-transfer-cli task create oss-to-oss source/photos target/archive --node 10.0.0.2,10.0.0.3 --date-after 2026-11-01T00:00:00
oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download
oss-transfer-cli task create local-to-oss target/archive --binding 10.0.0.2=/data/upload --target-prefix upload/
```

- `--conflict skip|overwrite|changed` 选择[冲突策略](operations.zh-CN.md#冲突策略)，默认为 `skip`。`--overwrite` 等同于 `--conflict overwrite`，不能与其他 `--conflict` 值同时使用。`changed` 需要 v4.2.0 及以上版本的执行节点。
- `--date-after` 和 `--date-before` 接受 `YYYY-MM-DDTHH:mm:ss`（按 Master 的时区解释），或带时区偏移的 RFC 3339 格式。
- `--distribution balanced|affinity` 选择子任务的分发方式。
- `import buckets|tasks|local 文件` 从文件批量创建任务，见 `help task import`。

成功时输出 `已创建 N 个任务：#ID` 和跟踪进度的命令。Master 拒绝全部行时不会创建任何任务：CLI 显示 `未创建任务：…` 并以 1 退出（`--json` 中 `submissionStatus` 为 `rejected`）。只拒绝部分行时，列出已创建的任务，显示 `N 个任务未创建：…` 并以 3 退出。连接在结果返回前中断时，CLI 会打印请求 ID：先查看 `task list`，再用相同的参数加 `--request-id ID` 重试，这样不会重复创建任务。

本地传输执行与控制台相同的预检，包括[下载写入探测](operations.zh-CN.md#下载预检失败)。`--validate-only` 只检查配置，不创建任务。`--bandwidth-mbps`（0.1–1,000,000 Mbps）以及 `--list-qps`、`--head-qps`、`--put-qps` 在创建任务时设置限速；与下文的限速一样，它们是任务所有节点的合计值。

下载到本地目录（`oss-to-local`）时可使用 `--ranged-download auto|off`。默认值 `auto` 让每个执行节点把 128 MiB 及以上的对象按 8 MiB 字节范围并发下载（每个对象 4 个，节点有空闲能力时最多 16 个），并严格按顺序写入文件，几乎所有任务都应保持默认。限速会约束分段：每个对象同时持有的分段最多相当于该任务在此节点上约一秒的带宽份额（节点带宽上限更低时按节点上限计算）；设有 LIST / GET 限制时，还不超过该任务在此节点上约一秒的 GET 份额，因为每个分段都是一次 GET。一秒的份额不足两个分段时，改为单流下载（见[大对象](operations.zh-CN.md#大对象)）。`off` 对每个对象使用单个顺序流下载；仅在目标存储或网络不适合并发分段下载（执行节点内存紧张、存储会限制大量并发 GET）或技术支持要求时使用。该设置在任务整个生命周期内固定，续传、重试和调整节点时保持不变。`task show` 会为目标为本地的任务显示该设置。其他方向不接受此参数。`task import local` 的 JSON 中对应字段为 `"rangedDownload": "auto"` 或 `"off"`，省略即为 `auto`。早于此选项的执行节点无论如何设置，都按单个流下载每个对象（见[分段下载选项](operations.zh-CN.md#分段下载选项)）。

```sh
oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download --ranged-download off
```

调整运行中任务的限速：

```sh
oss-transfer-cli task limits "$TRANSFER_TASK" --list-qps 100 --head-qps 500 --put-qps 200 --bandwidth-mbps 100
oss-transfer-cli task limits "$TRANSFER_TASK" --put-qps unlimited
```

未指定的字段保持原值；`unlimited` 解除对应的限制（QPS 选项也可以写 0）；不带新值运行 `task limits ID` 或 `task bandwidth ID` 会显示任务当前的限速：带宽上限、请求限速范围、LIST / GET、HEAD 和 PUT 上限、是否所有节点都已生效，以及未生效节点的原因。`task bandwidth ID MBPS|unlimited` 只修改带宽限制。QPS 是平均速率，可以是小数，范围为 0.000001–1,000,000,000：`--list-qps 0.5` 表示整个任务每 2 秒发出一个列举或 GET 请求。LIST 限制同时适用于 GET 读取（含区间读取）；PUT 包括分片上传请求。已停止或已结束的任务不能修改限速。

`task show ID` 显示当前限速及其适用范围，并把实时请求速率与任务上限、生效上限对照列出。*请求速率*、*任务请求上限* 和 *生效请求上限* 各行保留一位小数，低于 0.05 的上限显示为 0；*LIST / GET QPS*、*HEAD QPS*、*PUT QPS* 各行显示精确值。`task nodes ID` 显示每个节点的份额。任务限速是总量：Master 按需求把它分给活动的执行节点，有工作的节点平分绝大部分限速，空闲节点只保留很小的下限（见[任务限速在节点间的分配](operations.zh-CN.md#任务限速在节点间的分配)）。节点限速（见[设置节点限速](#设置节点限速)）仍然生效，请求必须同时满足两者。

## 异常记录和重试

```sh
oss-transfer-cli exceptions list failed --task-id "$TRANSFER_TASK"
oss-transfer-cli exceptions list local --task-id "$TRANSFER_TASK"
oss-transfer-cli task retry "$TRANSFER_TASK"
oss-transfer-cli exceptions export failed --task-id "$TRANSFER_TASK" --output failures.csv
oss-transfer-cli exceptions clear failed --task-id "$TRANSFER_TASK" --yes
```

`exceptions list` 以表格显示：任务、Key、大小、节点、简短原因（例如拒绝访问、DNS / Path-style、连接被拒绝、超时、TLS、存储桶不存在、签名、限流、源对象已删除或已修改）、重试结果（*未重试*、*重试中*、*重试未完成*、*已恢复*、*再次失败*、*已跳过*）和本地时间。表尾显示记录数；对 `local`，还会给出下一页的命令。完整的错误信息在 `--json` 输出和 `exceptions export` 导出的 CSV 中，CSV 另有 `reasonCode` 和 `retryReasonCode` 两列（例如 `SOURCE_CHANGED`、`SOURCE_DELETED`）。CLI 同时列出已解决的记录；控制台默认只显示未解决的记录。

用 `task retry` 重试失败的对象。清理只删除已通过重试成功或跳过而解决的记录（`已清理 N 条已处理的记录`），未解决的失败继续保留。上传和下载的失败使用 `local`。`fallback` 列出旧版引擎的降级迁移记录，当前版本已不再产生这类记录。

## 任务报告

导出已停止或已结束任务的报告：

```sh
oss-transfer-cli task report "$TRANSFER_TASK" --output "task-$TRANSFER_TASK.zip"
```

不指定 `--output` 时，文件为当前目录下的 `task-ID-report.zip`；已存在的文件不会被覆盖。保存后，CLI 显示保存位置、*报告完整*（是或否）、对象结果（*成功 N，跳过 N，失败 N，未处理 N，结果未知 N*），报告不完整时还会逐条说明原因，并列出无法读取的节点。报告不完整时以 3 退出，ZIP 仍会保留。`--json` 增加 `complete`、`incompleteReasons`（原因代码）、`resultCounts` 和 `unreachableNodes`。报告中的文件和代码见[任务报告](operations.zh-CN.md#任务报告)。

`--output -` 把 ZIP 写到标准输出，例如从容器中取出报告：`docker compose exec master oss-transfer-cli task report 12 --output - > task-12.zip`。这时完整性信息写到标准错误。它不能与 `--json` 同时使用（退出码 2）。

`exceptions export local --output 文件.zip` 保存所有本地迁移的报告，同样检查完整性。

## 调整任务的执行节点

查看执行任务的执行节点及其状态、扫描节点、待完成的子任务、各节点的需求和当前分得的任务限速份额、已移除的节点以及可以添加的执行节点。需求列显示 *按需（忙）* 或 *空闲（已释放）*；节点尚未确认降额时附加 *调整中*。尚未分得份额或已不再持有份额的节点（加入中或已断开）在需求列和份额列显示 `—`。旧版 Master 按平分计算，所有节点的需求列都显示 `—`。只有正在扫描（或正在接管扫描）的节点标记为扫描节点。表格下方会提示扫描已完成、扫描正在移交给新的扫描节点，或原扫描节点已退出、将由其他节点接管；移交后的扫描从 Master 保存的断点继续。正在移出的离线节点会注明在执行租约到期后离开；没有租约隔离的任务则注明在重新连接后离开。已停止任务的成员显示为 *已停止*，已结束任务的成员显示为 *已结束*：

```sh
oss-transfer-cli task nodes "$TRANSFER_TASK"
```

任务存在期间可以添加或移除执行节点。`--add` 和 `--remove` 可以重复使用，也可以用逗号分隔多个值。不加 `--yes` 时，命令只打印计划的调整（本地传输还会打印预检结果），不提交任何更改：

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

## 添加和移除执行节点

`agent` 和 `node` 两组命令管理的是同一批执行节点。用 `agent invite` 添加执行节点，用 `node list` 和 `node show` 查看节点，用 `agent update` 修改节点名称、并发或本地根目录，用 `agent remove`（或 `node remove`）移除节点。`node add` 和 `node invite` 同样会打印加入命令。

```sh
oss-transfer-cli node list
oss-transfer-cli agent invite --label storage-01
oss-transfer-cli agent invite-status 邀请ID
```

`agent invite` 打印加入命令，以本地时间显示有效期（*加入凭证仅可使用一次，有效至 …*；邀请有效 30 分钟），并显示下载来源：

- *原生部署（Linux / macOS / FreeBSD，需要 sudo）*：`curl -fsSL '<链接>' | sudo bash`；
- *原生部署（Windows PowerShell）*；
- *Docker（macOS / Linux，无需 root）* 和 *Docker（Windows PowerShell）*：普通的 `docker run` 命令，创建容器 `oss-transfer-agent-<6 个字符>` 及其数据卷；
- *Docker Compose（Linux，需要 sudo）*：安装脚本的 Compose 方式。

在新主机上运行与其对应的命令。执行节点通过命令中的 Master 地址连接 Master，默认使用 CLI 当前连接的地址。如果该地址是 `localhost` 或其他回环地址，CLI 会给出警告：请用 `--master-url` 指定执行节点能访问的地址，例如局域网地址；与 Master 在同一台主机上的 Docker 执行节点可用 `http://host.docker.internal:1685`。`agent invite-status` 显示执行节点是否已加入。

```sh
oss-transfer-cli agent update storage-01 --child-concurrency 32 --object-concurrency 4
oss-transfer-cli agent update storage-01 --local-root /mnt/data --local-root /mnt/archive
oss-transfer-cli agent remove storage-01 --yes
```

`--child-concurrency`（1–256，默认 32）和 `--object-concurrency`（每个子任务的对象并发，1–64，默认 4）立即生效，两者乘积不能超过 1024（见[节点并发](operations.zh-CN.md#节点并发)）。`--local-root` 替换该节点可用于本地迁移的目录列表。

执行节点上还有活动任务时，`agent remove` 会被拒绝。移除后，命令在标准错误中显示 *已移除执行节点 节点。该主机上的 Agent 服务会自行停止且不再重启；如需删除其程序和数据，请在该主机运行安装脚本并加 --uninstall --role agent*（见[移除执行节点](operations.zh-CN.md#移除执行节点)）。

## 设置节点限速

节点限速限制单个执行节点每秒的 LIST、PUT、HEAD 请求数和带宽，由该节点上的所有任务共用（见[节点限速](operations.zh-CN.md#节点限速)）：

```sh
oss-transfer-cli limit list
oss-transfer-cli limit set put 500 --all
oss-transfer-cli limit set bandwidth 200 --worker 10.0.0.2
oss-transfer-cli limit set head unlimited --worker 10.0.0.2
```

`limit list` 为每个节点的每种类型（`list`、`put`、`head`、`bandwidth`）显示一行：目标上限及其来源（等于执行节点 `service.json` 中 `nodeControl` 的值或内置默认值时为 *配置目标*，否则为 *已保存目标*）、运行上限、实测速率和状态，状态为 *正常*、*已降速*、*恢复中*、*已恢复* 或 *不限速*（见[限速状态](operations.zh-CN.md#限速状态)）。数值按精确值输出。没有响应的节点不会列出，命令以退出码 3 结束。加 `--watch 5` 可以跟踪自动降速的过程。

`limit set 类型 数值` 需要指定 `--worker 节点` 或 `--all` 之一；`--all` 为每个节点设置相同的值。请求上限的单位为次/秒，可以是小数，范围为 0.01–1,000,000。带宽默认以 Mbps 为单位，可用 `--unit bps|Kbps|Mbps|Gbps` 指定其他单位。数值必须大于 0；解除限制请使用 `unlimited`。该值成为节点的已保存目标：立即生效，结束正在进行的自动降速，重启后仍然保留。如果该上限从未保存过且正按配置值运行，设置同一个值不会被记录，之后修改 `service.json` 仍会对它生效。有执行节点未确认时，输出会列出该节点，命令以退出码 1 结束。

```sh
oss-transfer-cli limit history --limit 100
oss-transfer-cli limit adjustment --worker 10.0.0.2 --event-id EVENT_ID
```

`limit history` 按时间倒序列出所有节点最近的手动和自动调整；`--limit` 接受 1–200（默认 50）。`limit adjustment` 显示某次调整、触发它的请求以及该轮调节的每一步：传入节点和历史记录中的 `id`；要查看后续 100 步，再加上 `--cursor` 和输出中的 `nextCursor`。这两个命令输出原始字段，时间为自 1970 年起的毫秒数，带宽单位为 bit/s；脚本中请使用 `--json`。另见[存储限流](operations.zh-CN.md#存储限流)。

V4 的命令 `rate-limit`、`list-rate-limit`、`rate-limit-history [N]` 和 `set-rate-limit 类型 数值 [IP]` 仍然可用。`set-rate-limit` 把不带 K、M、G 后缀的带宽值视为 bit/s，把 0 视为不限速；不指定 IP 时应用到所有节点。

## 比对存储的对象

`object compare` 启动一次比对：用 `--scope object` 比对单个对象，用 `--scope prefix` 比对存储桶或前缀；比对由 Master 安排在某个执行节点上执行。`--mode metadata` 通过 HEAD 请求比较大小和元数据；`--mode content` 以流式方式对两端对象计算 SHA-256，不在磁盘上暂存。检查方式和结果见[对象比对说明](operations.zh-CN.md#对象比对)，参数见 `help object`。

```sh
oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --scope prefix --source-prefix 2026/ --dest-prefix 2026/ --mode metadata
oss-transfer-cli object list
```

`--source-bucket` 和 `--dest-bucket` 使用 `连接名称/存储桶` 格式。每次运行 `object compare` 都会启动一次比对。指定 `--source-key` 和 `--dest-key` 时比对单个对象；指定 `--source-prefix` 和 `--dest-prefix` 时比对前缀；既不指定 Key 也不指定前缀时比对整个存储桶。默认方式为 `metadata`。前缀比对打印新比对的 ID 和跟踪它的命令。单对象比对最多等待 15 秒，在此之前完成时按 `object status` 的格式打印结果，否则打印 ID 和跟踪它的命令。加 `--json` 时，已完成的比对输出最终的状态页，否则输出比对记录。`object list` 按开始时间倒序列出所有节点上的比对：ID、状态、方式、范围、源、目标、已检查的对象数、差异数、开始时间和节点。有节点未响应时，输出会列出这些节点，命令以退出码 3 结束。

从列表中选择一个 ID：

```bash
read -r -p 'Comparison ID: ' TRANSFER_COMPARISON
oss-transfer-cli object status "$TRANSFER_COMPARISON"
oss-transfer-cli object status --status MISSING_TARGET,DIFFERENT --limit 1000 "$TRANSFER_COMPARISON"
oss-transfer-cli object export --output "comparison-$TRANSFER_COMPARISON.zip" "$TRANSFER_COMPARISON"
```

选项放在 ID 之前。新比对的 ID 不会以 `-` 开头；旧版本生成的 ID 可能以 `-` 开头。这样的 ID 是完整的 ID（43 个字母、数字、`-` 或 `_`）时，CLI 可以直接接受；在 ID 前加 `--` 也总是可以，例如 `object status --limit 100 -- "$TRANSFER_COMPARISON"`。

`object status` 显示比对的状态、方式、范围、源和目标、各项计数、已保存的差异、已用时间和速率、已读取的字节数和以字节/秒计的读取速率（内容模式；单个对象只显示读取速率）、检查点（单对象比对和已完成的比对不显示）、是否可继续、停止保存差异的原因和最后保存的 Key，以及错误和错误详情。之后列出一页已保存的结果行：编号、结果、源 Key、目标 Key、两端大小和说明（不同的属性，或错误及其详情）。已完成且没有差异的比对显示 *未发现差异*。`--status` 只保留指定的结果，多个值用逗号分隔：`DIFFERENT`、`ETAG_MISMATCH`、`MISSING_TARGET`、`EXTRA_TARGET`、`CHANGED_DURING_COMPARE`、`ERROR` 和 `BOTH_MISSING`。`--limit` 设置每页行数，范围 1–1,000（默认 50），`--after N` 从第 N 行之后开始；后面还有结果行时，命令会打印下一页使用的 `--after` 值。比对已停止保存差异时，命令以退出码 3 结束。

`object export ID --output 文件` 保存一个 ZIP，包含 `differences.csv`、`retry-source-keys.incr` 和 `summary.json`（见[导出结果](operations.zh-CN.md#导出结果)）；加 `--format jsonl` 时改为每行保存一条结果。`--status` 与上文相同，用于限制导出的结果行；重传 Key 清单始终包含所有 `MISSING_TARGET`、`DIFFERENT` 和 `ETAG_MISMATCH` 行。输出文件不能已存在：CLI 先下载到同一目录下的临时文件，下载完整并通过校验后才以指定的文件名发布。导出 ZIP 后，命令打印各项计数、导出是否完整（附 `summary.json` 中的原因，例如 `RESULTS_UNAVAILABLE` 或 `RETRY_KEYS_MISMATCH`）以及重传 Key 的数量；有 Key 未写入 `retry-source-keys.incr` 时，提示用 `object export --format jsonl` 获取它们的准确值。导出不完整时以退出码 3 结束。JSON Lines 导出按与 ZIP 中 `summary.json` 相同的规则检查：比对未完成、未保存全部差异，或者对于保存了全部差异的结果，导出的行数与所选结果的计数之和不符（`ROW_COUNT_MISMATCH`，例如节点上已没有结果文件）时，CLI 报告导出不完整（见[导出结果](operations.zh-CN.md#导出结果)）。

```sh
oss-transfer-cli object resume "$TRANSFER_COMPARISON"
oss-transfer-cli object cancel "$TRANSFER_COMPARISON"
oss-transfer-cli object delete "$TRANSFER_COMPARISON"
```

`object resume` 从检查点继续已中断、失败或已取消的前缀比对（见[继续比对](operations.zh-CN.md#继续比对)）。只有比对重新运行时，命令才提示已继续比对；比对已在运行时，命令说明无需继续并打印跟踪它的命令；其他情况下与 `object cancel` 一样打印比对当前的状态。正在取消的比对要等它停止后才能继续。`object cancel` 停止正在运行的比对，已取消的前缀比对之后仍可继续。`object delete` 删除未在运行的比对的结果。比对所在节点离线时，这些命令以及 `object status`、`object export` 立即失败并提示 *节点离线*；已从集群移除的节点上的比对视为不存在，`object delete` 会把它从列表中移除。4.1.40 及更早版本的 Master 不支持 `object list`、`object export`、`object resume` 和 `--status`，此时 CLI 会提示先升级 Master。

## 更新集群

```sh
oss-transfer-cli update check
oss-transfer-cli update master --yes
oss-transfer-cli update agents --all --yes
oss-transfer-cli update status --watch 5
```

`update check` 显示 *当前版本 v4.1.41 → 可升级到 v4.2.0*、发布说明链接和接下来要运行的两条命令，或说明当前已是最新正式版。`update master --yes` 自行检查并安装最新正式版；Master 已是最新版本时会说明并以 0 退出。`update agents --all --yes` 把所有可更新的执行节点逐台更新到 Master 的版本；`update agent --worker 节点 --yes` 只更新一个。请求被受理并不代表升级已经完成：用 `update status --watch 5` 跟踪，它显示版本、翻译后的阶段、每个节点是否与 Master 一致，以及无法在线更新的节点的原因（见[版本更新](operations.zh-CN.md#版本更新)）。

在线更新只安装最新正式版：`update master --version X` 指定其他正式版时会被拒绝并给出说明，请改为在 Master 主机上带 `--version` 运行安装脚本。安装 `4.2.1-beta.1` 这类预发布版本时，需要指定精确版本并明确确认：

```sh
oss-transfer-cli update master --version 4.2.1-beta.1 --allow-prerelease --yes
```

不加 `--allow-prerelease` 时，预发布版本会在发送任何请求之前被拒绝，退出码为 2。为 `update agent` 或 `update agents` 指定预发布的 `--version` 时同样如此。不指定 `--version` 时，执行节点跟随 Master 的当前版本（包括预发布版本），无需该标志。缺少 `--yes` 等用法错误同样以 2 退出。见[预发布版本更新](operations.zh-CN.md#试用预发布版本)。

`backup list` 显示升级备份。`backup prune master --yes` 或 `backup prune agent --worker 节点 --yes` 只删除服务认定可清理的备份。

## 脚本

- 使用 `--json` 并检查[退出码](#输出语言和退出码)。删除或清理类的命令需要 `--yes`。
- 不经交互登录时，使用 `--password-env 变量名`、`--password-file 文件` 或 `--password-stdin`。`change-password` 接受 `--old-password-env` 和 `--new-password-env`，或对应的文件选项。`OSS_TRANSFER_AUTH_TOKEN` 提供登录令牌。
- `OSS_TRANSFER_CLI_CONFIG_DIR` 设置保存偏好和会话的目录（绝对路径）。默认为 Linux 上的 `~/.config/oss-transfer-cli`、macOS 上的 `~/Library/Application Support/oss-transfer-cli` 和 Windows 上的 `%APPDATA%\oss-transfer-cli`。
- 账号登录时输入的密码不会显示。修改账号密码需要登录该账号；本机服务访问不会修改网页控制台的密码。`logout` 关闭账号会话，不会移除已安装服务的本机访问。
- 明确指定任务 ID，例如 `224,225` 或 `224-226`；命令从不默认作用于全部任务。
