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

Online replacement supports managed native installations. The updater verifies the release archive, downloads it before stopping the service, and runs the native installer in a separate process. The installer backs up the binary, configuration and stopped database, checks the new service, and attempts to restore the backup if startup fails. Agent transfers interrupted by the restart resume after it reconnects.

Before stopping the service, the installer cleans eligible backups and checks space for the current database and binaries. It keeps up to two completed upgrade backups for 30 days, always retaining the newest rollback copy; failed and unrecognized backups are left alone. It applies the same policy after a successful upgrade. Database backup progress is printed every 10 seconds, including the final disk sync. A large database can take minutes.

The first upgrade from a version without online updates still uses the installation script. Containers use [Docker Compose updates](docker-compose.md); replacing a binary inside a running container would be lost when the container is recreated.

## Disconnected nodes

Multi-node tasks can move unfinished child tasks to another selected node after an Agent loses contact. All participating Agents must support task leases. The Master renews execution permission every 20 seconds; an isolated Agent stops starting work and cancels active transfers when its 60-second lease expires. Reassignment waits another 10 seconds. A returning node stays out of that task, but remains available for new tasks.

Takeover supports OSS-to-OSS and verified shared directories. A small hidden marker stays in each shared directory so another node can verify that it still sees the original volume. A private local directory, a missing marker, or an older Agent prevents automatic reassignment. Completed children stay unchanged; incomplete children keep their numbers and saved file results. The new owner checks already completed objects before continuing. If the scanner fails, the replacement rebuilds its scan index from the Master's saved assignments and rescans metadata; it does not download the dataset for this check.

A request already accepted by object storage may finish after its client is cancelled. Takeover therefore cannot promise exactly-once writes across arbitrary S3 services. Sources should remain stable during a transfer. Enabling task leases raises the database format to schema 5; downgrading requires the matching pre-upgrade database backups for both Master and Agents.

## Database space

Completed scans release their deduplication indexes in the background. Permanently deleted tasks also release their scan and execution records. Freed pages are reused; to shrink an existing database file, follow [database maintenance](database-maintenance.md).

## 中文说明

**执行节点 → 编辑节点** 可调整子任务并发和每个子任务的对象并发。默认值分别是 8 和 4，即单节点最多同时传输 32 个对象；已有配置继续保留。保存后直接生效，重启后保留。调低并发时，已发出的请求会先完成，再按新上限继续调度。子任务并发范围为 1–64，对象并发为 1–32，两者乘积不能超过 256。节点和任务的带宽、QPS 限制仍然生效。

**运维工具 → 对象比对** 支持单对象、整个存储桶和前缀比对。前缀按相对路径匹配，例如源 `images/` 下的 `images/a.jpg` 对应目标 `backup/` 下的 `backup/a.jpg`。两侧缺失对象都会记录。

元数据模式通过 HEAD 比较大小和可迁移元数据，不读取内容，不能据此确认内容相同。内容模式从两端流式读取数据，计算 SHA-256，不生成本地对象文件，但会消耗完整读取两端对象所需的网络流量。ETag 不作为跨厂商通用内容哈希；读取时发生变化的对象会单独标记。整个存储桶并不是原子快照，需要确认某一固定数据集时应先停止写入。

每个节点同时执行一次比对，批量保存差异和错误，保留最近 10 次结果，每次保留前 10,000 条差异。达到上限后仍继续统计，页面会提示结果截断；可缩小前缀再次比对，或删除已结束的比对结果。

**运维工具 → 版本更新** 更新 Master 后，在 **执行节点 → 版本更新** 点击“更新全部节点”，即可逐台更新可升级的 Agent。前一台重新连接、版本一致且安装完成后，才更新下一台；失败时停止队列。队列保存在 Master，关闭页面或重启 Master 后继续，也可以单独更新某个节点。原生安装会先下载并校验安装包，再由独立进程运行安装器，备份、重启并检查服务；启动失败时尝试恢复备份。Agent 重启导致中断的子任务会在重新连接后恢复。尚不支持在线升级的旧版本需先通过安装脚本升级一次；Docker 部署通过 Compose 更新。


安装器在停服务前清理符合条件的旧备份并检查空间，不足时保留运行中的服务。成功升级的备份最多保留 2 份、30 天，但始终保留最新 1 份；失败和无法识别的备份不自动删除。升级成功后也按此规则清理。数据库备份每 10 秒显示进度，落盘同步也会提示；大数据库可能需要几分钟。

多节点任务中的 Agent 失联后，未完成子任务会自动交给原先选定的健康节点。参与节点都需要支持任务租约：Master 每 20 秒续期，失联 Agent 在 60 秒授权到期后停止启动新工作并取消当前传输；Master 再等待 10 秒后重新分配。原节点回来后不再参与这个任务，仍可用于新任务。

接管支持 OSS→OSS 和已验证的共享目录。共享目录保留一个很小的隐藏校验文件，接管前通过它确认候选节点仍能访问原来的卷。独占本地目录、校验文件丢失或旧版 Agent 都不会自动接管。已完成子任务保持不变，未完成子任务保留编号和文件结果，新节点校验已完成对象后继续。扫描节点故障时，新节点根据 Master 保存的已分发清单重建去重索引，再扫描源端元数据，无需下载全部数据。

对象存储已经接收的请求可能在客户端取消后才完成，因此不能保证任意 S3 服务上绝无重复写入。迁移期间应保持源数据稳定。启用租约后数据库格式升级为 schema 5；降级需要同时恢复 Master 和 Agent 升级前的配套数据库备份。

扫描完成后会在后台回收去重索引，永久删除任务时也会清理扫描和执行记录。释放的页面可再次使用；缩小已有数据库文件的方法见[数据库维护](database-maintenance.md)。
