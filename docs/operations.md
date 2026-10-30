# Concurrency, comparison and updates

## Node concurrency

Open **Execution nodes → Edit node** to change child-task concurrency and the object concurrency within each child. The defaults are 8 children and 4 objects per child, allowing up to 32 simultaneous object transfers on a node. Existing saved settings are retained.

Changes apply to running work and survive a restart. Reducing a limit lets the current requests finish before admitting more. Increasing it can help when the network is underused; node and task bandwidth/QPS limits still apply. Child concurrency accepts 1–64, object concurrency 1–32, with a combined maximum of 256.

## Compare objects

Under **Operations → Object comparison**, choose a single object or a bucket/prefix on each side. For prefix comparison, the source prefix is replaced with the target prefix: `images/a.jpg` under `images/` matches `backup/a.jpg` under `backup/`. An empty prefix covers the whole bucket. Results include objects missing from either side.

**Metadata** compares size and transferable metadata through HEAD requests. It does not read object contents or prove byte equality. An ETag is not treated as a portable content hash: multipart uploads and different storage providers can produce different ETags for identical content.

**Content** reads both objects through streams and computes SHA-256. It does not create local object files, but it does transfer the full contents across the network and may incur storage read charges. Objects that change while being read are reported separately. A bucket-wide comparison is not an atomic snapshot; pause writes if you need a fixed dataset verified.

One comparison runs per Agent. Results are saved in batches; only differences and errors are retained. Each node keeps the latest 10 comparisons and the first 10,000 differences per comparison. Counts continue after that limit, and the page marks truncated results. Narrow the prefix to inspect remaining differences, or delete completed comparison results.

## Update installed services

Open **Operations → Software updates** to update the Master. Then open **Execution nodes → Software updates** and choose **Update all nodes** to bring eligible Agents to the Master's version. Each node must reconnect at the target version and finish its update before the next starts. A failed update stops the queue. The Master saves the queue, so closing the page or restarting the Master does not discard it. You can also update one node.

Wait for active tasks to finish when possible. If you upgrade with tasks running, update the Master first, then one Agent at a time. The native installer asks the Master to reserve that Agent’s assignments during the upgrade before stopping it. The reservation ends after verified startup or a successful rollback and expires after 30 minutes if the installer cannot finish. An older Master must be upgraded before the Agent installer can use this protection.

Online replacement supports managed native installations. The updater verifies the release archive, downloads it before stopping the service, and runs the native installer in a separate process. The installer takes a temporary backup of the programs, configuration, service definition and stopped database. It deletes that backup after the new service passes its health check and confirms the installation. If startup fails, it attempts to restore the previous installation and keeps the recovery files. Agent transfers interrupted by the restart resume after it reconnects.

Backups use the host's [default backup directory](../deploy/native/README.md#change-paths-and-startup-settings), even if an earlier installation saved a shared-storage path. An explicit `--backup-dir` or PowerShell `-BackupDir` selects another directory. Before stopping the service, the installer cleans eligible backups and checks available space. **Upgrade backups → Clean up backups** removes completed upgrade backups; failed backups and older backups without a completion marker are kept. Database backup progress is printed every 10 seconds, including the final disk sync.

For a planned downgrade, make a separate backup of each service's programs, configuration and stopped database before upgrading. Successful upgrades do not leave a rollback copy. See [manual recovery](../deploy/native/README.md#recover-from-a-failed-upgrade) if automatic rollback could not finish.

The first upgrade from a version without online updates still uses the installation script. Containers use [Docker Compose updates](docker-compose.md); replacing a binary inside a running container would be lost when the container is recreated.

## Disconnected nodes

Multi-node tasks can move unfinished child tasks to another selected node after an Agent loses contact. All participating Agents must support task leases. The Master renews execution permission every 20 seconds; an isolated Agent stops starting work and cancels active transfers when its 60-second lease expires. Reassignment waits another 10 seconds. A returning node can rejoin when both sides support recovery fencing and the original shared directory is still accessible. Its old execution permission is revoked before it receives a new ownership epoch. Older Agents remain excluded from that task.

The recovered node can receive new assignments and queued children from healthy nodes, even after scanning has finished. Redistribution uses the task's existing placement policy and its load estimate of remaining bytes plus 64 KiB per object. Only children that have never started can move: the current owner must atomically revoke their execution permission before the Master changes ownership. Running and interrupted transfers stay with their current owner. Rebalancing runs in bounded batches after recovery; it does not continuously reshuffle the queue.

Takeover supports OSS-to-OSS and verified shared directories. A small hidden marker stays in each shared directory so another node can verify that it still sees the original volume. A private local directory, a missing marker, or an older Agent prevents automatic reassignment. Completed children stay unchanged; incomplete children keep their numbers and saved file results. The new owner checks already completed objects before continuing. If the scanner fails, the replacement rebuilds its scan index from the Master’s saved assignments and resumes from the last source-page checkpoint covered by those assignments. Scan-ahead results that never reached the Master must be discovered again; the dataset contents are not downloaded for this check.

A request already accepted by object storage may finish after its client is cancelled. Takeover therefore cannot promise exactly-once writes across arbitrary S3 services. Sources should remain stable during a transfer. Enabling task leases raises the database format to schema 5; safely rejoining a retired node requires schema 8, credential encryption requires schema 9, and queue handoff requires schema 10. Downgrading requires the matching pre-upgrade database backups for both Master and Agents.

## Database space

Agents release scan batches after the Master durably saves, acknowledges and authorizes their release. Completed scans also release their deduplication indexes in the background. Permanently deleted tasks also release their scan and execution records. Freed pages are reused; to shrink an existing database file, follow [database maintenance](database-maintenance.md).

## Resume a scan

Stopping a task or restarting its scanning Agent preserves the scan position in the Agent database. OSS scans continue with the saved listing token. Local scans reopen the active directory path and skip completed subdirectories and previously processed files. A very large active directory still needs its names read and sorted on restart; files before the saved position are not individually inspected. File-list tasks also preserve their position.

Checkpoints are committed with the preceding scan results. A restart may replay a small uncommitted portion, which is deduplicated before creating child tasks. Checkpoints add bounded traversal state per task, not a second copy of every file name. Upgrade the scanning Agent to enable this behavior. Unfinished scans from older versions without a checkpoint need one compatibility rescan to establish their first checkpoint. The Master also retains checkpoints covered by saved assignments. A replacement scanner uses that boundary, which can be behind the original Agent’s private scan position. Tasks without a transferable checkpoint still need a compatibility rescan. Shared local paths may differ between nodes, but the original volume and active directory identities must match.

This continues the same traversal; changes behind its saved position may not be discovered. Keep the source stable or use a snapshot when a fixed dataset is required. A rejected OSS token or a replaced active local directory stops the scan with an error rather than silently restarting or skipping data. An already completed scan is not repeated when its transfers resume.

## 中文说明

**执行节点 → 编辑节点** 可调整子任务并发和每个子任务的对象并发。默认值分别是 8 和 4，即单节点最多同时传输 32 个对象；已有配置继续保留。保存后直接生效，重启后保留。调低并发时，已发出的请求会先完成，再按新上限继续调度。子任务并发范围为 1–64，对象并发为 1–32，两者乘积不能超过 256。节点和任务的带宽、QPS 限制仍然生效。

**运维工具 → 对象比对** 支持单对象、整个存储桶和前缀比对。前缀按相对路径匹配，例如源 `images/` 下的 `images/a.jpg` 对应目标 `backup/` 下的 `backup/a.jpg`。两侧缺失对象都会记录。

元数据模式通过 HEAD 比较大小和可迁移元数据，不读取内容，不能据此确认内容相同。内容模式从两端流式读取数据，计算 SHA-256，不生成本地对象文件，但会消耗完整读取两端对象所需的网络流量。ETag 不作为跨厂商通用内容哈希；读取时发生变化的对象会单独标记。整个存储桶并不是原子快照，需要确认某一固定数据集时应先停止写入。

每个节点同时执行一次比对，批量保存差异和错误，保留最近 10 次结果，每次保留前 10,000 条差异。达到上限后仍继续统计，页面会提示结果截断；可缩小前缀再次比对，或删除已结束的比对结果。

尽量等任务完成后升级。需要带任务升级时，先更新 Master，再逐台更新 Agent。原生安装器会先请 Master 暂时保留该 Agent 的任务，再停机备份；新服务验收或成功回滚后解除保留，安装中断则最多保留 30 分钟。旧 Master 不支持此流程时，需要先升级 Master。

**运维工具 → 版本更新** 更新 Master 后，在 **执行节点 → 版本更新** 点击“更新全部节点”，即可逐台更新可升级的 Agent。前一台重新连接、版本一致且安装完成后，才更新下一台；失败时停止队列。队列保存在 Master，关闭页面或重启 Master 后继续，也可以单独更新某个节点。原生安装会先下载并校验安装包，再由独立进程运行安装器，临时备份程序、配置、服务定义和已停止的数据库。新服务通过健康检查并确认安装成功后，删除本次备份；启动失败时尝试自动恢复旧安装，并保留恢复文件。Agent 重启导致中断的子任务会在重新连接后恢复。尚不支持在线升级的旧版本需先通过安装脚本升级一次；Docker 部署通过 Compose 更新。

备份默认写入[本机的默认备份目录](../deploy/native/README.md#change-paths-and-startup-settings)，不再沿用历史安装保存的共享存储路径；需要其他位置时，显式传入 `--backup-dir`，PowerShell 使用 `-BackupDir`。安装器在停服务前清理符合条件的旧备份并检查空间，不足时保留运行中的服务。页面的“升级备份 → 清理备份”可删除成功升级留下的备份；失败备份和缺少完成标记的旧备份继续保留。数据库备份每 10 秒显示进度，包括最后的落盘同步。

如果计划降级，升级前应另行备份各服务的程序、配置和已停止的数据库；成功升级后不会留下回退副本。自动回滚未完成时，按[手动恢复步骤](../deploy/native/README.md#recover-from-a-failed-upgrade)处理。

多节点任务中的 Agent 失联后，未完成子任务会自动交给原先选定的健康节点。参与节点都需要支持任务租约：Master 每 20 秒续期，失联 Agent 在 60 秒授权到期后停止启动新工作并取消当前传输；Master 再等待 10 秒后重新分配。节点回来后，双方都支持恢复隔离机制且共享目录检查通过，才会重新加入。程序先撤销旧执行授权，再发放新一轮授权。旧版 Agent 仍不重新加入该任务。

恢复节点既能接收新子任务，也会分担健康节点上尚未开始的队列，扫描已经结束时也适用。分配沿用任务的分发策略，以剩余字节数加每个对象 64 KiB 的权重估算负载。移交前必须由原节点原子撤销执行权；已经开始或中断过的传输继续由当前节点处理。再均衡在恢复后分批进行，不会持续来回搬动队列。

接管支持 OSS→OSS 和已验证的共享目录。共享目录保留一个很小的隐藏校验文件，接管前通过它确认候选节点仍能访问原来的卷。独占本地目录、校验文件丢失或旧版 Agent 都不会自动接管。已完成子任务保持不变，未完成子任务保留编号和文件结果，新节点校验已完成对象后继续。扫描节点故障时，新节点根据 Master 保存的已分发清单重建去重索引，再从这些清单覆盖的源端分页断点继续扫描。原节点尚未交给 Master 的扫描结果仍需重新发现，这个过程不下载对象内容。

对象存储已经接收的请求可能在客户端取消后才完成，因此不能保证任意 S3 服务上绝无重复写入。迁移期间应保持源数据稳定。启用租约后数据库格式升级为 schema 5，节点重新加入使用 schema 8，凭证加密使用 schema 9，排队任务移交使用 schema 10。降级需要同时恢复 Master 和 Agent 升级前的配套数据库备份。

Master 持久保存、确认接收并授权释放扫描批次后，Agent 会在后台释放这些批次；扫描完成后也会回收去重索引，永久删除任务时也会清理扫描和执行记录。释放的页面可再次使用；缩小已有数据库文件的方法见[数据库维护](database-maintenance.md)。

停止任务或重启扫描节点后，扫描位置保存在 Agent 数据库中。OSS 从保存的分页游标继续；本地扫描重新打开尚未完成的目录，跳过已完成的子目录和已处理文件。若当前单个目录特别大，恢复时仍需读取和排序该目录的文件名，但不会逐个检查断点之前的文件属性。文件清单任务也保存处理位置。

断点与此前的扫描结果按顺序提交，重启后可能回放一小段尚未落盘的内容，去重后再生成子任务。每个任务只增加有限的遍历状态，不额外保存一整份文件名清单。升级负责扫描的 Agent 后生效；旧版本尚未完成且没有断点的扫描，首次恢复需要兼容重扫来建立断点。Master 同时保存已分发清单覆盖的扫描断点，其他节点接管时从这个位置继续。它可能落后于原节点的私有扫描位置；没有可迁移断点的旧任务仍需要兼容重扫。共享目录的挂载路径可以不同，但必须确认原共享卷和当前遍历目录的身份一致。

续扫延续同一轮遍历，不能保证发现断点之前的新增或修改。需要固定数据集时，应保持源端不变或使用快照。OSS 拒绝旧游标、或尚未扫描完的本地目录被替换时，会明确报错，不会静默从头扫描或跳过数据。已完成扫描的任务恢复传输时，不再重新扫描。
