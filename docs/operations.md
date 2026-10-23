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

Open **Operations → Software updates** to check the latest stable release. Update the Master first, then update each Agent to the Master's installed version. The page shows each node's current version and update status.

Online replacement supports managed native installations. The updater verifies the release archive, downloads it before stopping the service, and runs the native installer in a separate process. The installer backs up the binary, configuration and stopped database, checks the new service, and attempts to restore the backup if startup fails. Agent transfers interrupted by the restart resume after it reconnects.

The first upgrade from a version without online updates still uses the installation script. Containers use [Docker Compose updates](docker-compose.md); replacing a binary inside a running container would be lost when the container is recreated.

## 中文说明

**执行节点 → 编辑节点** 可调整子任务并发和每个子任务的对象并发。默认值分别是 8 和 4，即单节点最多同时传输 32 个对象；已有配置继续保留。保存后直接生效，重启后保留。调低并发时，已发出的请求会先完成，再按新上限继续调度。子任务并发范围为 1–64，对象并发为 1–32，两者乘积不能超过 256。节点和任务的带宽、QPS 限制仍然生效。

**运维工具 → 对象比对** 支持单对象、整个存储桶和前缀比对。前缀按相对路径匹配，例如源 `images/` 下的 `images/a.jpg` 对应目标 `backup/` 下的 `backup/a.jpg`。两侧缺失对象都会记录。

元数据模式通过 HEAD 比较大小和可迁移元数据，不读取内容，不能据此确认内容相同。内容模式从两端流式读取数据，计算 SHA-256，不生成本地对象文件，但会消耗完整读取两端对象所需的网络流量。ETag 不作为跨厂商通用内容哈希；读取时发生变化的对象会单独标记。整个存储桶并不是原子快照，需要确认某一固定数据集时应先停止写入。

每个节点同时执行一次比对，批量保存差异和错误，保留最近 10 次结果，每次保留前 10,000 条差异。达到上限后仍继续统计，页面会提示结果截断；可缩小前缀再次比对，或删除已结束的比对结果。

**运维工具 → 版本更新** 可检测稳定版，先更新 Master，再逐个将 Agent 更新到 Master 的版本。原生安装会先下载并校验安装包，再由独立进程运行安装器，备份、重启并检查服务；启动失败时尝试恢复备份。Agent 重启导致中断的子任务会在重新连接后恢复。尚不支持在线升级的旧版本需先通过安装脚本升级一次；Docker 部署通过 Compose 更新。
