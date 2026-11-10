# 将 V4.0.x 状态转换为 V4.1.x

[English](legacy-migration.md)

启动 V4.1.x 服务前，使用 `oss-transfer-master migrate` 转换 V4.0.x 数据库。转换会保留账号、存储连接、任务历史、报告覆盖范围和回收站状态。未完成的任务保持停止，需显式恢复后才会继续。

转换过程可在 Linux 或 macOS 上运行，输出写入新目录。在转换后的服务和任务结果核对完成之前，保持原安装处于停止状态，并保留其备份。

## 导出旧数据库

停止旧 Master 和所有执行节点，然后复制它们的数据目录和实际生效的配置。Master 也可能执行过传输，因此同样需要导出。除 `application.yml` 外，还要记录环境变量和命令行覆盖参数。

使用 JDK 8 或更高版本构建导出工具。运行时需要 Java 运行环境和原 V4.0.x JAR；导出工具借助该 JAR 中的 `rocksdbjni` 库读取复制出的数据库。

```sh
bash tools/legacy/build.sh
java -jar output/tools/legacy/legacy-exporter.jar \
  --legacy-jar /backup/master/oss-transfer.jar \
  --expected-jar-sha256 '<SHA-256 of that JAR>' \
  --database /backup/master/data/rocksdb \
  --config /backup/master/application.yml \
  --node 10.0.0.10 \
  --role master \
  --output /backup/exports/master.jsonl
```

`--node` 设为持久化的 `node_config.localIp` 或 `agent:identity.address`。角色可选 `master`、`agent` 或 `worker`；`worker` 表示旧版通过地址直连的执行节点。每个节点各执行一次导出，并使用单独的输出文件。

导出工具以只读方式打开已有的 RocksDB 数据库，并检查锁和文件身份。复制前先停止所有节点，确保各快照对应同一集群状态。导出文件包含凭证和密码哈希，以 `0600` 权限写入；应与受保护的数据库备份一起存放。

检查整套导出文件：

```sh
oss-transfer-master migrate inspect \
  /backup/exports/master.jsonl /backup/exports/worker.jsonl
```

检查会校验导出文件的校验和、节点覆盖情况和身份映射，并报告转换前必须解决的冲突。原配置文件仅作为留存依据；实际生效的值需在下面的计划中显式填写。

## 创建迁移计划

保存一个私有的 `migration-plan.json`。下面的示例对应位于 `10.0.0.10` 的一台 Master/worker 合一节点：

```json
{
  "snapshots": ["/backup/exports/master.jsonl"],
  "destination": "/export/oss-transfer",
  "publicUrl": "https://transfer.example.com",
  "directNodes": {
    "10.0.0.10": {"address": "10.0.0.10", "label": "Master host execution"}
  },
  "legacyTimezone": "Asia/Shanghai",
  "acceptGoRuntimeDefaults": true,
  "masterListen": ":1685",
  "effectiveNodeControl": {
    "10.0.0.10": {
      "rate-limit.src.qps.list": "100",
      "rate-limit.dest.qps.put": "1400",
      "rate-limit.dest.qps.head": "2800",
      "rate-limit.bandwidth": "2000000000",
      "rate-limit.auto-adjust.consecutive-503-threshold": "20",
      "rate-limit.auto-adjust.reduce-factor": "0.9",
      "rate-limit.auto-adjust.increase-factor": "1.2",
      "rate-limit.auto-adjust.min-put-qps": "200",
      "rate-limit.auto-adjust.interval-ms": "10000"
    }
  }
}
```

`destination` 必须是尚不存在的绝对路径。将每个节点的导出文件都加入 `snapshots`，并为每个执行节点提供全部九项 `effectiveNodeControl` 属性。把示例值替换为已停止部署的实际生效设置。带宽单位为 bit/s；请求或带宽限制为零表示不限制。持久化的手动限制优先于这些配置目标。转换工具把这些设置写入各 Agent 的 `service.json` 中的 `nodeControl`：四项限制对应 `listQps`、`putQps`、`headQps` 和 `bandwidthBitsPerSecond`；自动调节设置写入其中的 `autoAdjust` 对象，对应 `consecutive503Threshold`（`throttleThreshold` 的旧名称，现在表示 5 秒窗口内的限流响应数）、`reduceFactor`、`increaseFactor`、`minQps`（现在是 LIST、PUT、HEAD 降速的共同下限）和 `recoveryIntervalMs`（见[调整自动降速参数](../operations.zh-CN.md#调整自动降速参数)）。

`directNodes` 为每个旧版直连 worker（包括旧 Master 上的执行）授权一个新的 Agent 身份。已注册的 Agent 在其私有凭证与 Master 快照匹配后，保留原有身份。

`publicUrl` 设为新 Agent 和浏览器访问的地址。`masterListen` 指定监听地址和端口。`legacyTimezone` 是原 JVM 时区；已保存的日期筛选条件不带时区偏移时必须填写。

设置 `acceptGoRuntimeDefaults: true` 之前，请先阅读下文的执行默认值。转换工具将这一选择记录为配置档 `go-runtime-defaults-1`，最终发布前必须确认。

## 准备与最终发布

```sh
chmod 600 migration-plan.json
oss-transfer-master migrate prepare --plan migration-plan.json
```

`prepare` 会输出一个私有暂存目录并导入任务目录。后续命令使用输出的路径：

```sh
STAGING=/export/.oss-transfer-migration-123456789
oss-transfer-master migrate status --directory "$STAGING"
oss-transfer-master migrate prepare-recovery --directory "$STAGING"
oss-transfer-master migrate status --directory "$STAGING"
oss-transfer-master migrate finalize --directory "$STAGING"
```

`prepare-recovery` 为未完成任务写入保留的对象键清单。之后恢复任务时才开始读取存储。`finalize` 检查节点快照、任务历史、恢复清单、身份、运行时确认和服务配置，然后发布目标目录。

转换失败时，报告中给出的暂存目录会保留以供检查。`convert-tasks --directory "$STAGING" --legacy-timezone Asia/Shanghai` 使用相同输入重试任务转换。如果计划中未确认运行时默认值，核对默认值后，可向 `finalize` 传入 `--accept-go-runtime-defaults`。修改任务的原始范围需要重新准备。

目标目录包含 `master/master.db`、`agents/<instanceId>/agent.db`、各自的 `service.json` 文件、原始导出文件、`identity-map.json` 和 `manifest.json`。

## 安装转换后的服务

每个 Agent 必须在其预定的执行主机上运行。只把它自己的私有目录复制过去，保留 `service.json` 中记录的绝对路径、目录权限 `0700` 和文件权限 `0600`。Master 状态和原始导出文件留在 Master 主机上。确认旧的本地源路径和目标路径仍指向预期的挂载点。

使用安装器的 `--adopt-config` 选项登记转换后的数据。服务名和数据路径必须与生成的配置一致。本例中的 Master：

```sh
RELEASE_URL=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest
curl -fSL "$RELEASE_URL/install.sh" -o install.sh
sudo bash install.sh --role master --method native --release-url "$RELEASE_URL" \
  --adopt-config /export/oss-transfer/master/service.json \
  --service-name oss-transfer-master --data-dir /export/oss-transfer/master --yes
```

Agent 生成的服务名为 `oss-transfer-agent-<instanceId>`。登记 Agent 时，使用它自己的 `service.json`、服务名和数据路径。程序和配置的安装目录必须为空；安装器会在启动前为已停止的数据库创建快照。

如需前台检查，在各自主机上运行对应程序：

```sh
oss-transfer-master --config /export/oss-transfer/master/service.json
oss-transfer-agent --config '/export/oss-transfer/agents/<instanceId>/service.json'
```

使用原账号密码登录，确认 Agent 在线。登录成功后，旧密码哈希会升级为 Argon2id。导入的账号保留角色和显示名称；登录会话会重新建立。

## 首次恢复

恢复导入的未完成任务时，会创建一次新的执行尝试，同时在历史中保留原父任务、子任务、报告记录和断点。恢复会检查所选 Agent 身份、已保存的停止和删除状态，以及任务所需的清单文件。

扫描沿用原来的范围、清单、本地源、筛选条件和目标命名。保留的对象键与新发现的对象键通过持久化唯一索引合并。历史上被 `TIME_FILTER` 排除的对象仍保持排除；`dateAfter` 包含边界，`dateBefore` 不包含。保留的源对象缺失时，生成状态为 `SOURCE_MISSING` 的失败记录。

复用旧的 `SUCCESS` 结果之前，Agent 会完整读取当前的源对象和目标对象并比较 SHA-256，读取前后都会检查两端身份。目标已存在但内容不一致时，按保存的冲突策略处理：`SKIP` 记录为跳过，`OVERWRITE` 允许重新写入。此前的跳过、演练结果和失败在历史中保持原有含义。

恢复会先持久化每个批次和分配，再确认接收。任务代次推进和尝试计数重置只执行一次，因此重试中断的恢复不会重复计入旧的成功结果。校验和传输期间，节点和任务限制均生效。

中断的分片上传对象从对象级别重新开始：V4.0.x 没有保存续传所需的 upload ID 和已完成分片列表。旧的远端上传和本地暂存文件需要按归属关系清理。如果校验后业务又修改了源对象，可能需要再次传输；恢复检查的是当前内容，不会重建已不可用的历史版本。

已完成任务保留历史状态和报告覆盖范围。旧版报告元数据包含 `containsLegacyHistory: true` 和 `contentVerified: false`；这些记录的 `targetCommitted` 和 `ledgerObservedAt` 为空。新的执行尝试单独记录当前结果。

## 执行默认值

转换后，重试、并发和分片设置采用 Go 执行配置档：

| 设置 | 值 |
| --- | --- |
| 请求重试 | AWS SDK 标准策略，含首次请求最多 3 次；被限流的请求（HTTP 429、503 或限流错误码）最多尝试 10 次，每次重试前随机退避（见[存储限流](../operations.zh-CN.md#存储限流)） |
| 对象执行 | 默认每个 Agent 最多 32 个活动子任务，每个子任务 4 个对象工作线程；每个节点可设置 1–256 个子任务、每个子任务 1–64 个对象，子任务数 × 对象数最多 1,024（见[节点并发](../operations.zh-CN.md#节点并发)） |
| 对象发现 | 每个 Agent 最多 2 个扫描；每批最多 128 个对象或描述内容总计 256 MiB；超大对象单独成批 |
| 准入 | 每个 Agent 最多 1,024 个未完成的子任务作业；额度用满时新工作等待 |
| 分片上传 | 分片 8 MiB，每次流式写入最多 4 个并发分片请求和 32 MiB 负载缓冲；较大的请求体可能使用暂存文件 |
| 大对象 | 128 MiB 及以上的对象在单个 Agent 内按字节区间并行传输：每个对象 4 个区间，节点有空闲对象槽位时每个对象最多 16 个，全部计入该 Agent 256 MiB 的传输内存预算。写入对象存储时各区间作为分片发送（1 GiB 及以上的对象使用 16 MiB 分片），但任务设置了任务级带宽、LIST / GET 或 PUT 限制时每个对象按单一数据流传输。下载到本地目录时以 8 MiB 为一段拉取，并按顺序写入；有带宽或 LIST / GET 限制时，每个对象占用的区间最多相当于约一秒的读取量，每秒不足两段时按单一数据流下载（见[大对象](../operations.zh-CN.md#大对象)） |
| 结果持久化 | 每 128 条结果或每 1 秒提交一批；最终结果与子任务完成状态立即一并提交 |

如果在目标写入和结果提交之间发生崩溃，该对象可能会按保存的冲突策略再次处理。映射的节点控制属性和持久化的手动覆盖仍然有效。旧任务的 QPS 和带宽字段保持按节点生效；逻辑任务带宽策略保持集群范围。两者同时存在时都会生效。

## 状态映射参考

V4.0.x 在 RocksDB 默认列族中使用 UTF-8 键和 Fastjson 值。导出工具按原字节保留。参考结构见 [V4.0.6 源码](https://github.com/MoonWX/oss-transfer/tree/v4.0.6/src/main/java)，提交 `835a1d8`。

| V4.0.x 键 | 转换方式 |
| --- | --- |
| `user:<username>` | 在 `users` 中保留密码哈希、角色和显示名称；其余时间戳保留在迁移历史中。 |
| `s3_config_<alias>` | 向 Master 和 Agent 写入一份协调一致的存储快照。保留凭证、签名方式和 path-style 标志；旧的动态签名区域设为 `default`。 |
| `node_config` | 映射成员关系和标签；保留已记录的目录元数据。Master/worker 合一节点拆分为独立角色。监听地址和公开 URL 取自计划。 |
| `agent:member:<address>`, `agent:identity` | 先用 Master 中 base64url 编码的 SHA-256 哈希匹配 Agent 凭证，再写入 Go 的十六进制 SHA-256 索引。保留实例 ID 和注册 ID；已吊销的成员保持吊销。 |
| `agent:invite:<token hash>`, `agent:storage-version` | 保留邀请历史和存储来源信息。新运行时会签发新的邀请和配置版本。 |
| `rate_limit:manual:list`, `:put`, `:head`, `:bandwidth` | 将手动覆盖保留在 Agent 节点控制设置中。带宽单位为 bit/s。 |
| `rate_adjustment:*`, `rate_event:*`, `rate_incident:*`, `rate_incident_event:*`, `rate_incident_gap:*` | 保留历史事件。当前 503 计数和恢复事件随新进程重新开始。 |
| `task_bandwidth:desired:<scope>`, `task_bandwidth:task:<id>`, `task_bandwidth:scope_tasks:<scope>`, `task-bandwidth:policy:<base64url scope>` | 重建逻辑任务范围，并在分配限制前将各 worker 副本与 Master 策略比对。 |
| `task_creation:<UUID>`, `task_creation_ids` | 按 `publicTaskId` 归组执行范围；保留代次、停止/删除状态和待分发项。协调器的 `COMPLETED` 表示扫描完成，传输是否完成以执行结果为准。 |
| `local:parent:<id>`, `local:parent:id:list` | 保留本地文件逻辑父任务和原始请求。`orchestrationComplete` 表示分发完成。 |
| `task:<id>`, `task:id:list`, `task:id:sequence` | 保留任务历史和公开父任务 ID。新分配器从旧分配高水位之后开始。 |
| `task:deleted:<id>`, `task:id:list:deleted`, `local:parent:deleted:<id>` | 保留回收站及其中保留的报告。 |
| `task:purged:<id>`, `local:parent:purged:<id>`, `report:purged:<id>`, `local:stop:<id>` | 保留永久删除和停止记录。worker 上互相冲突的部分清除会阻止恢复。 |
| `local:explore:<parent>`, `local:discovery:<parent>` | 保留扫描标记、待处理范围和待处理批次，用于恢复。 |
| `local:batch-assignment:*`, `local:shard-assignment:*` | 保留原节点分配和内部任务 ID，用于对不确定的分发去重。 |
| `inventory:<UUID>:entry:<base64url key>`, `inventory:<UUID>:ready` | 原样保留清单。缺少条目或就绪标记会阻止恢复。 |
| `completed:<report task id>:<base64url logical key>` | 保留历史完成记录；当前源和目标身份在恢复时确定。 |
| `file-report:<id>:item:<base64url logical key>`, `file-report:<id>:coverage`, `:scan:*`, `:legacy` | 保留源/目标映射、状态、时间戳，以及 `TRACKED`、`PARTIAL` 或 `LEGACY_UNKNOWN` 覆盖范围。 |
| `failed:<sequence>`, `failed:count` | 保留异常历史和分配高水位，包括已清理条目留下的空缺。 |
| `fallback:<sequence>`, `fallback:count` | 保留旧 SDK 回退历史。 |
| `local-temp:<UUID>` | 保留归属元数据，供清理时核对。Java 的文件键字符串不能作为 Go 的 inode 身份。 |

旧执行记录按节点身份和任务 ID 映射，并结合已保存的子任务身份。本地内部 ID 可能为负数。保留公开的 `parent-number` 编号；一个旧范围拆分为多个批次时保留对应关系。旧的逻辑键可能已包含目标前缀；源和目标名称应根据保存的端点和报告字段推导。

历史记录不进入 Agent 可运行队列。Go 作业需要一份不可变、对象描述完整的列表；恢复时根据原始范围生成这些列表。报告中缺失的覆盖范围和未知字段保留在私有原始导出文件中。

## 导出格式与暂存

每个 JSONL 导出文件包含：带节点、角色、时间戳及 JAR/库哈希的文件头；可选的配置附件；按顺序排列的 base64url 键值条目；以及包含计数和前面所有行 SHA-256 校验和的文件尾。未知的数据库键保留原始字节。

检查会校验整个文件，包括顺序、重复字段和文件尾计数。限制为：每个键 1 MiB、每个值 64 MiB、每个配置附件 4 MiB、映射事实 100,000 条。超出限制的文件无法通过检查。

`legacyimport.Prepare` 在映射前将导出文件复制到同级的私有暂存目录。`Prepared.Master`、`Prepared.Agents` 和 `Prepared.Iterate` 用于访问暂存的数据库和原始记录。重新打开暂存目录时会再次检查校验和、权限和迁移身份。

暂存的数据库带有 `migration/state.pending=true`，会阻止服务启动。最终发布时记录数据库的最终绝对路径，并通过原子重命名发布目录；目标已存在时拒绝发布。Linux 使用带 `RENAME_NOREPLACE` 的 `renameat2`，macOS 使用带 `RENAME_EXCL` 的 `renamex_np`。在不支持 `renameat2` 的 Linux 内核上，或文件系统拒绝该标志时，最终发布会先以独占方式创建空的目标目录，再重命名到该目录上；`rename(2)` 只会替换空目录，因此仍不会覆盖已有数据。最终发布中断后可从暂存目录重试。服务启动时会同时检查角色和最终路径。

`MapEffectiveNodeControl` 要求计划中提供全部九项已确定的设置，拒绝占位值和未知属性。`WriteRuntimeConfig` 为每个角色写入一个包含最终路径的 `service.json`。节点连接不一致、身份缺失或清单不完整，都必须在最终发布前解决。

## 测试

```sh
go test -race ./internal/legacyimport
go vet ./internal/legacyimport ./tools/legacy/inspect
python3 tools/legacy/test-export.py \
  --jdk-home /path/to/jdk8 \
  --output output/packages/qa/legacy-export
```

导出工具测试使用真实的 V4.0.6 RocksDB JNI 库。它逐字节比对每个导出的键值，并检查源文件、输出权限、拒绝已有输出，以及在存在活动写入者时的锁定行为。Go 测试覆盖导出文件损坏、节点快照缺失、身份冲突、恢复期间内容变化、最终发布中断，以及不支持 `renameat2` 或 `RENAME_NOREPLACE` 时的发布。

`output/packages/qa/legacy-real-tasks-2` 中的录制夹具由 `tools/legacy/capture-tasks.py` 使用 V4.0.6 和 SeaweedFS 采集，包含六个逻辑任务：四个已完成（其中一个在回收站中）和两个已中断。测试归档固定为 SHA-256 `64a0e80a28d61322fad20be17c8d47c31f77565a76daf5b1f4718590f9c992d4`。

有该夹具时，检查历史转换：

```sh
OSS_TRANSFER_LEGACY_TASK_SNAPSHOT="$PWD/output/packages/qa/legacy-real-tasks-2/master.jsonl" \
  go test -count=1 ./internal/legacyimport ./internal/master -run 'TestRealV4'
```

进行进程级恢复测试时，需提供内嵌 Web 资源的发布版程序：

```sh
OSS_TRANSFER_LEGACY_RESUME_SNAPSHOT="$PWD/output/packages/qa/legacy-real-tasks-2/master.jsonl" \
OSS_TRANSFER_NATIVE_MASTER=/path/to/oss-transfer-master \
OSS_TRANSFER_NATIVE_AGENT=/path/to/oss-transfer-agent \
  go test -count=1 ./internal/legacyimport -run '^TestRealV4FinalizedNativeProcesses$' -v
```

该测试复制存储夹具，启动最终发布后的服务，用旧密码登录，并检查 Agent 身份和保存的 1 Mbps 限制。随后通过管理员 API 调高这一隔离测试环境中的限制，恢复任务 #7 和 #8，并对全部 520 对源/目标对象计算哈希。测试还会检查回收站中的任务以及四个旧成功结果的复用。当前发布检查见[验收指南](../../.github/acceptance/README.zh-CN.md)。
