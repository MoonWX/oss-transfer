# Database space

The database stores object names, scan checkpoints, assignments and transfer results. Object contents are streamed between endpoints. With millions of small files, the number of objects matters more to database size than their total size.

An unfinished scan keeps an index for deduplication and source-change detection during recovery. After scanning completes, the Agent removes this index in small background transactions. Scan batches remain available for delivery to the Master. Permanently deleting a task removes its scan and execution records; stopping a task or moving it to the recycle bin retains them.

New writes use compact object records and compress larger values when compression saves space. Existing plain records remain readable. Cleanup makes pages reusable, but does not reduce the database file's size on disk.

## Reclaim disk space

Stop the service before compacting its database. Compaction writes a separate file, packs its pages, converts old Agent scan-index records and compresses eligible values. It verifies every record before publishing the new file. The original is unchanged, and an existing output file is never overwritten. Progress is printed every 10 seconds.

For an Agent installed at the default Linux paths:

```sh
sudo systemctl stop oss-transfer-agent
sudo /opt/oss-transfer/agent/oss-transfer-agent \
  --config /etc/oss-transfer/agent/service.json \
  --compact-state /var/lib/oss-transfer/agent/agent.compact.db
```

If compaction succeeds, keep the service stopped and install the verified copy. These commands preserve the original's owner and permissions and refuse to overwrite an existing `agent.before-compact.db`:

```sh
sudo cp --attributes-only --preserve=mode,ownership /var/lib/oss-transfer/agent/agent.db /var/lib/oss-transfer/agent/agent.compact.db &&
sudo mv -nT /var/lib/oss-transfer/agent/agent.db /var/lib/oss-transfer/agent/agent.before-compact.db &&
sudo test ! -e /var/lib/oss-transfer/agent/agent.db &&
sudo mv -nT /var/lib/oss-transfer/agent/agent.compact.db /var/lib/oss-transfer/agent/agent.db &&
sudo systemctl start oss-transfer-agent
```

Check that the node reconnects and its tasks and reports are present before deleting the retained original. For a Master, substitute `master` for `agent` in these commands. Use the installation's actual paths and service name if they were customized.

The destination needs room for the compacted copy. The tool checks free space before each write transaction and reserves at least 256 MiB plus room for the next database growth. If it fails, it removes its temporary output and leaves the original intact; restart the service with the original. Other programs can still consume space during maintenance.

The compact storage format uses database schema 4; automatic task takeover requires schema 5. Older binaries cannot open it. To downgrade, restore the matching database backup made before upgrading along with the old binary.

## 中文说明

数据库保存对象名称、扫描进度、子任务和传输结果，不保存对象内容。小文件越多，所需的元数据空间越大。

未结束的扫描需要保留去重索引，用于恢复和检测源文件变化。扫描完成后，Agent 在后台分批清理索引，但仍保留待下发的扫描批次。永久删除任务时才清理扫描和执行记录；停止任务、移入回收站不会删除这些数据。

新写入会减少重复字段，并对较大的记录进行无损压缩。旧记录仍可读取。清理后，数据库可以复用空闲页，但文件本身不会自动缩小。

要把空间归还给文件系统，先停止对应服务，再执行上面的 `--compact-state` 命令。它生成并逐条验证一个新文件，不改动原库；成功后才执行第二组命令替换文件、启动服务。默认 Linux 安装可以直接使用示例，Master 将其中的 `agent` 替换为 `master`；自定义安装使用实际路径和服务名。

操作期间需要额外空间容纳副本。程序每批写入前检查剩余空间，至少保留 256 MiB 和下一次扩展所需空间；失败时清理临时副本，原库不变。启动后确认节点、任务和报告正常，再删除保留的原库。

压缩存储使用数据库 schema 4，自动接管任务使用 schema 5。旧程序不能直接读取新格式。降级时需要同时恢复升级前的数据库备份和对应程序。
