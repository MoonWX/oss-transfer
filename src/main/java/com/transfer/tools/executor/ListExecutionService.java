package com.transfer.tools.executor;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.ListObjectsRequest;
import com.amazonaws.services.s3.model.ObjectListing;
import com.amazonaws.services.s3.model.S3ObjectSummary;
import com.transfer.tools.config.SourceBucketConfig;
import com.transfer.tools.config.ThreadConfig;
import com.transfer.tools.model.MigrationStatus;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.model.LocalFileItem;
import com.transfer.tools.model.FailureStage;
import com.transfer.tools.service.RateLimiterManager;
import com.transfer.tools.service.S3ClientManager;
import com.transfer.tools.service.SubTaskManagerService;
import com.transfer.tools.service.LocalFileService;
import com.transfer.tools.service.LocalTaskControlService;
import com.transfer.tools.service.TaskCancelledException;
import com.transfer.tools.service.LocalTaskRateLimiterService;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import javax.annotation.PreDestroy;
import java.text.SimpleDateFormat;
import java.util.ArrayList;
import java.util.Date;
import java.util.List;
import java.nio.file.Path;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

@Slf4j
@Service
public class ListExecutionService {

    private static final int MAX_RETRY_COUNT = 2;
    private static final long[] RETRY_DELAYS_MS = {500, 1000, 2000, 4000, 8000};

    @Autowired
    private RateLimiterManager rateLimiterManager;

    @Autowired
    private BufferQueueManager bufferQueueManager;

    @Autowired
    private PutExecutionService putExecutionService;

    @Autowired
    private SubTaskManagerService subTaskManagerService;

    @Autowired
    private SourceBucketConfig sourceBucketConfig;

    @Autowired
    private com.transfer.tools.service.TaskProgressCache taskProgressCache;

    @Autowired
    private ThreadConfig threadConfig;

    @Autowired
    private S3ClientManager s3ClientManager;

    @Autowired
    private LocalFileService localFileService;

    @Autowired
    private LocalTaskControlService localTaskControlService;

    @Autowired
    private LocalTaskRateLimiterService localTaskRateLimiterService;

    @Autowired
    private com.transfer.tools.service.FileReportLedgerService fileReportLedgerService;

    @Autowired
    private com.transfer.tools.service.InventoryManifestService inventoryManifestService;

    private ExecutorService executorService;
    private ExecutorService startExecutorService;

    @PostConstruct
    public void init() {
        Integer threadCount = threadConfig.getSrc().getList();
        executorService = Executors.newFixedThreadPool(threadCount);
        startExecutorService = Executors.newSingleThreadExecutor();

        for (int i = 0; i < threadCount; i++) {
            executorService.submit(this::listWorker);
        }

        log.info("List执行器初始化完成，启动 {} 个工作线程，日期筛选将使用任务级别的配置", threadCount);
    }

    private void listWorker() {
        while (!Thread.currentThread().isInterrupted()) {
            try {
                SubTaskStatus task = bufferQueueManager.takeListTask();
                executeListTask(task);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                log.info("List工作线程被中断");
                break;
            } catch (Exception e) {
                log.error("List工作线程异常", e);
            }
        }
    }

    public void startExecution(String sourceBucket, String destBucket, List<Integer> taskIds) {
        startExecutorService.submit(() -> {
            try {
                List<SubTaskStatus> pendingTasks = getPendingTasks(sourceBucket, destBucket, taskIds);

                enqueueStartedTasks(pendingTasks);

                log.info("已提交 {} 个List任务到队列", pendingTasks.size());
            } catch (Exception e) {
                log.error("提交List任务失败", e);
            }
        });
    }

    void enqueueStartedTasks(List<SubTaskStatus> pendingTasks) throws InterruptedException {
        for (SubTaskStatus task : pendingTasks) {
            try (LocalTaskControlService.Operation ignored = localTaskControlService.begin(
                    task.getTaskIndex(), task.getParentTaskId())) {
                // Start may include more tasks than the bounded queue can hold. Backpressure
                // waits for a consumer rather than silently abandoning the rest of the batch.
                while (true) {
                    synchronized (localTaskControlService) {
                        localTaskControlService.checkRunning(task.getTaskIndex());
                        localTaskControlService.checkRunning(task.getParentTaskId());
                        if (bufferQueueManager.tryOfferListTask(task)) break;
                    }
                    // Do not hold the cancellation lock while waiting for capacity.
                    Thread.sleep(50);
                }
            } catch (TaskCancelledException ignored) {
                // A cancelled member must not prevent unrelated selected tasks from starting.
            } catch (InterruptedException interrupted) {
                if (!localTaskControlService.isStopped(task.getTaskIndex())
                        && !localTaskControlService.isStopped(task.getParentTaskId())) throw interrupted;
            }
        }
    }

    private List<SubTaskStatus> getPendingTasks(String sourceBucket, String destBucket, List<Integer> taskIds) {
        List<SubTaskStatus> pendingTasks = new ArrayList<>();
        MigrationStatus status = subTaskManagerService.getMigrationStatus(sourceBucket, destBucket);

        for (SubTaskStatus task : status.getTasks()) {
            if (task != null && task.getStatus() == SubTaskStatus.TaskStatus.PENDING) {
                if (taskIds == null || taskIds.isEmpty() || taskIds.contains(task.getTaskIndex())) {
                    pendingTasks.add(task);
                }
            }
        }

        log.info("找到 {} 个待执行的List任务 (sourceBucket: {}, destBucket: {}, taskIds: {})",
                pendingTasks.size(), sourceBucket, destBucket, taskIds);
        return pendingTasks;
    }

    private void executeListTask(SubTaskStatus task) {
        if (localTaskControlService.isStopped(task.getTaskIndex())) return;
        try (LocalTaskControlService.Operation ignored = localTaskControlService.begin(task.getTaskIndex(), task.getParentTaskId())) {
            synchronized (subTaskManagerService) {
                SubTaskStatus current = subTaskManagerService.getTaskStatus(task.getTaskIndex());
                if (current != null && (current.getStatus() != SubTaskStatus.TaskStatus.PENDING
                        || !java.util.Objects.equals(current.getAttempt(), task.getAttempt()))) return;
                task.setStatus(SubTaskStatus.TaskStatus.RUNNING);
                subTaskManagerService.updateTaskStatus(task);
            }
            executeListTaskWhileRunning(task);
        } catch (TaskCancelledException ignored) { }
    }

    private void executeListTaskWhileRunning(SubTaskStatus task) {
        if (localTaskControlService.isStopped(task.getTaskIndex())) {
            return;
        }
        task.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        task.setStartTime(System.currentTimeMillis());
        task.setMarker(task.getStartKey());
        subTaskManagerService.updateTaskStatus(task);

        Date taskFilterDateAfter = null;
        if (task.getDateAfter() != null && !task.getDateAfter().trim().isEmpty()) {
            try {
                String dateStr = task.getDateAfter().trim();
                SimpleDateFormat sdf;
                if (dateStr.contains("T")) {
                    sdf = new SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss");
                } else {
                    sdf = new SimpleDateFormat("yyyy-MM-dd HH:mm:ss");
                }
                taskFilterDateAfter = sdf.parse(dateStr);
                log.info("任务 {} 使用日期筛选(dateAfter): {}", task.getTaskIndex(), dateStr);
            } catch (Exception e) {
                log.error("解析任务日期筛选失败: {}", task.getDateAfter(), e);
            }
        }

        Date taskFilterDateBefore = null;
        if (task.getDateBefore() != null && !task.getDateBefore().trim().isEmpty()) {
            try {
                String dateStr = task.getDateBefore().trim();
                SimpleDateFormat sdf;
                if (dateStr.contains("T")) {
                    sdf = new SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ss");
                } else {
                    sdf = new SimpleDateFormat("yyyy-MM-dd HH:mm:ss");
                }
                taskFilterDateBefore = sdf.parse(dateStr);
                log.info("任务 {} 使用日期筛选(dateBefore): {}", task.getTaskIndex(), dateStr);
            } catch (Exception e) {
                log.error("解析任务日期筛选失败: {}", task.getDateBefore(), e);
            }
        }

        try {
            fileReportLedgerService.scanStarted(task);
            if (task.getInventoryManifestId() != null) {
                executeInventoryTask(task);
                return;
            }
            if ((task.getObjectKey() == null || task.getObjectKey().isEmpty())
                    && task.getEndKey() == null && task.getStartKey() != null && task.getStartKey().endsWith(".incr")) {
                throw new IllegalStateException("旧清单任务缺少可恢复的原始清单，请重新导入清单");
            }
            if (task.getDirection() == TransferDirection.LOCAL_TO_OSS) {
                executeLocalSourceTask(task, taskFilterDateAfter, taskFilterDateBefore);
                return;
            }
            if (task.getSourceBucket() == null) {
                throw new IllegalStateException("任务的sourceBucket为空");
            }

            String bucket = task.getSourceBucket();
            log.info("开始执行List任务 {}: sourceBucket='{}', destBucket='{}', startKey='{}', endKey='{}', dateAfter='{}', dateBefore='{}', sourceS3Alias='{}'",
                    task.getTaskIndex(), task.getSourceBucket(), task.getDestBucket(),
                    task.getStartKey(), task.getEndKey(), task.getDateAfter(), task.getDateBefore(), task.getSourceS3Alias());

            if (task.getDirection() != TransferDirection.LOCAL_TO_OSS
                    && task.getObjectKey() != null && !task.getObjectKey().isEmpty()) {
                executeExactOssObject(task, taskFilterDateAfter, taskFilterDateBefore);
                return;
            }

            String prefix = task.getSourceEndpoint() != null
                    && (task.getDirection() == TransferDirection.OSS_TO_LOCAL
                        || Boolean.TRUE.equals(task.getSourceEndpoint().getPrefixFilterEnabled()))
                    ? nullToEmpty(task.getSourceEndpoint().getPrefix()) : "";
            String marker = task.getStartKey();

            while (true) {
                localTaskControlService.checkRunning(task.getTaskIndex());
                if (task.getDirection() != null && task.getDirection() != TransferDirection.OSS_TO_OSS) {
                    localTaskRateLimiterService.acquireList(task);
                }
                rateLimiterManager.acquireList();

                ListObjectsRequest request = createListRequest(
                        bucket, prefix, marker, sourceBucketConfig.getList().getMaxKeys());

                ObjectListing listing = listObjectsWithRetry(request, task.getTaskIndex(), task.getSourceS3Alias());
                List<S3ObjectSummary> summaries = listing.getObjectSummaries();
                log.info("ObjectListing, bucket:{}, marker: {}, size: {}", bucket, marker, summaries.size());

                if (summaries.isEmpty()) {
                    break;
                }

                boolean reachEndKey = false;
                long listCount = 0L;
                long listSize = 0L;
                long skippedCount = 0L;
                long skippedSize = 0L;
                for (S3ObjectSummary summary : summaries) {
                    String key = summary.getKey();

                    if (task.getEndKey() != null && !task.getEndKey().isEmpty() && key.compareTo(task.getEndKey()) > 0) {
                        log.info("任务 {} 已到达endKey边界", task.getTaskIndex());
                        reachEndKey = true;
                        break;
                    }
                    fileReportLedgerService.discovered(task, key, summary.getSize(), null);
                    if (!localTaskControlService.shouldProcess(task.getTaskIndex(), key)) {
                        continue;
                    }

                    localTaskControlService.checkRunning(task.getTaskIndex());
                    taskProgressCache.addToProgress(task.getTaskIndex(), 1, summary.getSize(), 0, 0L, 0, 0L, null);
                    Date lastModified = summary.getLastModified();
                    if (taskFilterDateAfter != null && lastModified != null && lastModified.before(taskFilterDateAfter)) {
                        skippedCount++;
                        skippedSize += summary.getSize();
                        fileReportLedgerService.skipped(task, key, summary.getSize(), null, "TIME_FILTER");
                        continue;
                    }
                    if (taskFilterDateBefore != null && lastModified != null && !lastModified.before(taskFilterDateBefore)) {
                        skippedCount++;
                        skippedSize += summary.getSize();
                        fileReportLedgerService.skipped(task, key, summary.getSize(), null, "TIME_FILTER");
                        continue;
                    }

                    ObjectListMessage message = new ObjectListMessage(
                        summary, task.getTaskIndex(), false,
                        task.getSourceBucket(), task.getDestBucket(),
                        task.getSourceS3Alias(), task.getDestS3Alias(),
                        task.getOverwrite(), null);
                    copyTransferFields(task, message);
                    bufferQueueManager.putPutMessage(message);
                }
                marker = listing.getNextMarker();
                taskProgressCache.addToProgress(task.getTaskIndex(), 0, 0L, 0, 0L, 0, 0L, skippedCount, skippedSize, null);

                if (!listing.isTruncated() || reachEndKey) {
                    break;
                }
            }

            fileReportLedgerService.scanCompleted(task);
            taskProgressCache.updateTaskStatus(task.getTaskIndex(), SubTaskStatus.TaskStatus.LIST_DONE);
            ObjectListMessage lastMessage = new ObjectListMessage(null, task.getTaskIndex(), true,
                    task.getSourceBucket(), task.getDestBucket(),
                    task.getSourceS3Alias(), task.getDestS3Alias(),
                    task.getOverwrite(), null);
            copyTransferFields(task, lastMessage);
            bufferQueueManager.putPutMessage(lastMessage);
        } catch (TaskCancelledException e) {
            task.setStatus(SubTaskStatus.TaskStatus.STOPPED);
            subTaskManagerService.stopTask(task.getTaskIndex());
        } catch (Exception e) {
            if (localTaskControlService.isStopped(task.getTaskIndex())
                    || localTaskControlService.isStopped(task.getParentTaskId())
                    || Thread.currentThread().isInterrupted()) {
                return;
            }
            log.error("List任务 {} 执行失败", task.getTaskIndex(), e);
            recordScanFailure(task, e);
            if ((task.getDirection() != null && task.getDirection() != TransferDirection.OSS_TO_OSS)
                    || (task.getObjectKey() != null && !task.getObjectKey().isEmpty())) {
                subTaskManagerService.recordFailedKey(task.getTaskIndex(),
                        task.getObjectKey() != null ? task.getObjectKey() : task.getLocalPath(),
                        e.getMessage(), 0L,
                        task.getDirection() == TransferDirection.LOCAL_TO_OSS ? FailureStage.SCAN : FailureStage.READ_SOURCE,
                        "SOURCE_DISCOVERY_FAILED");
            }
            task.setStatus(SubTaskStatus.TaskStatus.FAILED);
            subTaskManagerService.failTask(task.getTaskIndex());
        }
    }

    private void executeInventoryTask(SubTaskStatus task) throws InterruptedException {
        String cursor = null;
        do {
            localTaskControlService.checkRunning(task.getTaskIndex());
            com.transfer.tools.model.RocksDBPage<com.transfer.tools.service.InventoryManifestService.Entry> page =
                    inventoryManifestService.page(task.getInventoryManifestId(), cursor);
            for (com.transfer.tools.service.InventoryManifestService.Entry entry : page.getItems().values()) {
                localTaskControlService.checkRunning(task.getTaskIndex());
                if (!localTaskControlService.shouldProcess(task.getTaskIndex(), entry.key)) continue;
                fileReportLedgerService.discovered(task, entry.key, null, null);
                taskProgressCache.addToProgress(task.getTaskIndex(), 1, 0L, 0, 0L, 0, 0L, null);
                ObjectListMessage message = new ObjectListMessage(null, task.getTaskIndex(), false,
                        task.getSourceBucket(), task.getDestBucket(), task.getSourceS3Alias(), task.getDestS3Alias(),
                        task.getOverwrite(), entry.key);
                copyTransferFields(task, message);
                message.setSourceSizeUnknown(true);
                bufferQueueManager.putPutMessage(message);
            }
            cursor = page.isDone() ? null : page.getNextCursor();
        } while (cursor != null);
        fileReportLedgerService.scanCompleted(task);
        taskProgressCache.updateTaskStatus(task.getTaskIndex(), SubTaskStatus.TaskStatus.LIST_DONE);
        ObjectListMessage last = new ObjectListMessage(null, task.getTaskIndex(), true,
                task.getSourceBucket(), task.getDestBucket(), task.getSourceS3Alias(), task.getDestS3Alias(), task.getOverwrite(), null);
        copyTransferFields(task, last);
        bufferQueueManager.putPutMessage(last);
    }

    ListObjectsRequest createListRequest(String bucket, String sourcePrefix, String marker, int maxKeys) {
        return new ListObjectsRequest()
                .withBucketName(bucket)
                .withPrefix(nullToEmpty(sourcePrefix))
                .withMarker(marker)
                .withMaxKeys(maxKeys);
    }

    private void executeExactOssObject(SubTaskStatus task, Date after, Date before) throws Exception {
        localTaskControlService.checkRunning(task.getTaskIndex());
        if (!localTaskControlService.shouldProcess(task.getTaskIndex(), task.getObjectKey())) {
            finishList(task);
            return;
        }
        fileReportLedgerService.discovered(task, task.getObjectKey(), null, null);
        AmazonS3 client = s3ClientManager.getClient(task.getSourceS3Alias());
        if (client == null) {
            throw new IllegalStateException("源S3客户端不存在: " + task.getSourceS3Alias());
        }
        if (task.getDirection() == TransferDirection.OSS_TO_LOCAL) localTaskRateLimiterService.acquireList(task);
        else rateLimiterManager.acquireHead();
        com.amazonaws.services.s3.model.ObjectMetadata metadata = client.getObjectMetadata(
                task.getSourceBucket(), task.getObjectKey());
        localTaskControlService.checkRunning(task.getTaskIndex());
        S3ObjectSummary summary = new S3ObjectSummary();
        summary.setBucketName(task.getSourceBucket());
        summary.setKey(task.getObjectKey());
        summary.setSize(metadata.getContentLength());
        summary.setLastModified(metadata.getLastModified());
        fileReportLedgerService.discovered(task, summary.getKey(), summary.getSize(), null);
        taskProgressCache.addToProgress(task.getTaskIndex(), 1, summary.getSize(), 0, 0, 0, 0, null);
        if (matchesDate(metadata.getLastModified(), after, before)) {
            ObjectListMessage message = new ObjectListMessage(summary, task.getTaskIndex(), false,
                    task.getSourceBucket(), task.getDestBucket(), task.getSourceS3Alias(),
                    task.getDestS3Alias(), task.getOverwrite(), null);
            copyTransferFields(task, message);
            bufferQueueManager.putPutMessage(message);
        } else {
            fileReportLedgerService.skipped(task, summary.getKey(), summary.getSize(), null, "TIME_FILTER");
            taskProgressCache.addToProgress(task.getTaskIndex(), 0, 0, 0, 0, 0, 0,
                    1, summary.getSize(), summary.getKey());
        }
        finishList(task);
    }

    private void executeLocalSourceTask(SubTaskStatus task, Date after, Date before) throws Exception {
        if (task.getLocalFileEntries() != null) {
            executeLocalBatchTask(task, after, before);
            return;
        }
        final String filterPrefix = nullToEmpty(task.getStartKey());
        final boolean singleFile = java.nio.file.Files.isRegularFile(
                java.nio.file.Paths.get(task.getLocalPath()), java.nio.file.LinkOption.NOFOLLOW_LINKS);
        try {
            localFileService.scan(task.getLocalPath(), item -> {
                try {
                    localTaskControlService.checkRunning(task.getTaskIndex());
                    if (!filterPrefix.isEmpty() && !item.getRelativeKey().startsWith(filterPrefix)) {
                        return;
                    }
                    String key = singleFile && task.getObjectKey() != null && !task.getObjectKey().isEmpty()
                            ? task.getObjectKey() : joinOssKey(task.getDestinationEndpoint() == null
                            ? null : task.getDestinationEndpoint().getPrefix(), item.getRelativeKey());
                    fileReportLedgerService.discovered(task, key, item.getSize(), item.getPath().toString());
                    if (!localTaskControlService.shouldProcess(task.getTaskIndex(), key)) {
                        return;
                    }
                    taskProgressCache.addToProgress(task.getTaskIndex(), 1, item.getSize(), 0, 0, 0, 0, null);
                    if (!matchesDate(new Date(item.getLastModified()), after, before)) {
                        fileReportLedgerService.skipped(task, key, item.getSize(), item.getPath().toString(), "TIME_FILTER");
                        taskProgressCache.addToProgress(task.getTaskIndex(), 0, 0, 0, 0, 0, 0,
                                1, item.getSize(), item.getRelativeKey());
                        return;
                    }
                    ObjectListMessage message = new ObjectListMessage(null, task.getTaskIndex(), false,
                            null, task.getDestBucket(), null, task.getDestS3Alias(), task.getOverwrite(), key);
                    copyTransferFields(task, message);
                    message.setLocalPath(item.getPath().toString());
                    message.setSourceSize(item.getSize());
                    message.setSourceLastModified(item.getLastModified());
                    bufferQueueManager.putPutMessage(message);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    throw new TaskCancelledException("local scan interrupted");
                }
            });
        } catch (RuntimeException e) {
            if (e instanceof TaskCancelledException) {
                throw e;
            }
            throw e;
        }
        finishList(task);
    }

    private void executeLocalBatchTask(SubTaskStatus task, Date after, Date before) throws Exception {
        String root = task.getLocalPath();
        String filterPrefix = nullToEmpty(task.getStartKey());
        boolean singleFile = java.nio.file.Files.isRegularFile(java.nio.file.Paths.get(root),
                java.nio.file.LinkOption.NOFOLLOW_LINKS);
        for (com.transfer.tools.model.LocalFileEntry item : task.getLocalFileEntries()) {
            localTaskControlService.checkRunning(task.getTaskIndex());
            String relative = item.getRelativeKey();
            if (!filterPrefix.isEmpty() && !relative.startsWith(filterPrefix)) continue;
            String key = singleFile && task.getObjectKey() != null && !task.getObjectKey().isEmpty()
                    ? task.getObjectKey() : joinOssKey(task.getDestinationEndpoint() == null
                    ? null : task.getDestinationEndpoint().getPrefix(), relative);
            long size = item.getSize() == null ? 0L : item.getSize();
            String plannedSource = singleFile ? root : root + (root.endsWith("/") ? "" : "/") + relative;
            fileReportLedgerService.discovered(task, key, size, plannedSource);
            if (!localTaskControlService.shouldProcess(task.getTaskIndex(), key)) continue;
            taskProgressCache.addToProgress(task.getTaskIndex(), 1, size, 0, 0, 0, 0, null);
            Date modified = item.getLastModified() == null ? null : new Date(item.getLastModified());
            if (!matchesDate(modified, after, before)) {
                fileReportLedgerService.skipped(task, key, size, plannedSource, "TIME_FILTER");
                taskProgressCache.addToProgress(task.getTaskIndex(), 0, 0, 0, 0, 0, 0, 1, size, relative);
                continue;
            }
            Path source;
            try {
                source = localFileService.resolveSource(root, relative);
            } catch (RuntimeException error) {
                try {
                    fileReportLedgerService.failed(task, key, size, plannedSource, "READ_SOURCE_FAILED", error.getMessage());
                } catch (RuntimeException reportError) {
                    error.addSuppressed(reportError);
                }
                throw error;
            }
            fileReportLedgerService.discovered(task, key, size, source.toString());
            ObjectListMessage message = new ObjectListMessage(null, task.getTaskIndex(), false,
                    null, task.getDestBucket(), null, task.getDestS3Alias(), task.getOverwrite(), key);
            copyTransferFields(task, message);
            message.setLocalPath(source.toString());
            message.setSourceSize(size);
            message.setSourceLastModified(item.getLastModified());
            bufferQueueManager.putPutMessage(message);
        }
        finishList(task);
    }

    private void recordScanFailure(SubTaskStatus task, Exception error) {
        try {
            fileReportLedgerService.scanFailed(com.transfer.tools.service.FileReportLedgerService.reportTaskId(task), error.getMessage());
            if (task.getDirection() != TransferDirection.LOCAL_TO_OSS && task.getObjectKey() != null
                    && !task.getObjectKey().isEmpty() && !(error instanceof TaskCancelledException)
                    && !(error instanceof com.transfer.tools.service.FileReportLedgerService.FileReportPersistenceException)) {
                fileReportLedgerService.failed(task, task.getObjectKey(), null, null,
                        "READ_SOURCE_FAILED", error.getMessage());
            }
        } catch (RuntimeException reportError) {
            // A reporting failure must not prevent the original task failure/stop from being saved.
            log.error("任务 {} 的报告状态写入失败", task.getTaskIndex(), reportError);
        }
    }

    private void finishList(SubTaskStatus task) throws InterruptedException {
        fileReportLedgerService.scanCompleted(task);
        taskProgressCache.updateTaskStatus(task.getTaskIndex(), SubTaskStatus.TaskStatus.LIST_DONE);
        ObjectListMessage lastMessage = new ObjectListMessage(null, task.getTaskIndex(), true,
                task.getSourceBucket(), task.getDestBucket(), task.getSourceS3Alias(),
                task.getDestS3Alias(), task.getOverwrite(), null);
        copyTransferFields(task, lastMessage);
        bufferQueueManager.putPutMessage(lastMessage);
        localTaskControlService.clearSelection(task.getTaskIndex());
    }

    private void copyTransferFields(SubTaskStatus task, ObjectListMessage message) {
        message.setExecutionAttempt(task.getAttempt());
        message.setDirection(task.getDirection());
        message.setSourceEndpoint(task.getSourceEndpoint());
        message.setDestinationEndpoint(task.getDestinationEndpoint());
        message.setConflictPolicy(task.getConflictPolicy());
        message.setLocalPath(task.getLocalPath());
        message.setExecutionNodeIp(task.getExecutionNodeIp());
        message.setTargetRelativePath(task.getTargetRelativePath());
        message.setParentTaskId(task.getParentTaskId());
        message.setShardId(task.getShardId());
        message.setShardIndex(task.getShardIndex());
        message.setStorageId(task.getStorageId());
        message.setWorkerNode(task.getWorkerNode());
        message.setListQpsLimit(task.getListQpsLimit());
        message.setHeadQpsLimit(task.getHeadQpsLimit());
        message.setPutQpsLimit(task.getPutQpsLimit());
        message.setBandwidthBitsPerSecond(task.getBandwidthBitsPerSecond());
        message.setTaskBandwidthLimitMbps(task.getTaskBandwidthLimitMbps());
        message.setTaskBandwidthScopeId(task.getTaskBandwidthScopeId());
        message.setKeyMapping(task.getKeyMapping());
    }

    private boolean matchesDate(Date value, Date after, Date before) {
        return (after == null || value == null || !value.before(after))
                && (before == null || value == null || value.before(before));
    }

    private String joinOssKey(String prefix, String relative) {
        if (prefix == null || prefix.isEmpty()) {
            return relative;
        }
        return prefix.endsWith("/") ? prefix + relative : prefix + "/" + relative;
    }

    private String nullToEmpty(String value) {
        return value == null ? "" : value;
    }

    private ObjectListing listObjectsWithRetry(ListObjectsRequest request, Integer taskIndex, String srcS3Alias) throws Exception {
        Exception lastException = null;

        for (int attempt = 0; attempt < MAX_RETRY_COUNT; attempt++) {
            try {
                AmazonS3 s3Client = s3ClientManager.getListClient(srcS3Alias);
                if (s3Client == null) {
                    throw new RuntimeException("源S3客户端不存在: " + srcS3Alias);
                }
                return s3Client.listObjects(request);
            } catch (Exception e) {
                lastException = e;
                if (attempt < MAX_RETRY_COUNT - 1) {
                    long delayMs = RETRY_DELAYS_MS[attempt];
                    log.warn("任务 {} listObjects失败，第 {} 次重试，等待 {} ms，错误: {}",
                            taskIndex, attempt + 1, delayMs, e.getMessage());
                    try {
                        Thread.sleep(delayMs);
                    } catch (InterruptedException ie) {
                        Thread.currentThread().interrupt();
                        log.error("任务 {} listObjects重试被中断", taskIndex);
                        throw ie;
                    }
                }
            }
        }

        log.error("任务 {} listObjects失败，已重试 {} 次", taskIndex, MAX_RETRY_COUNT, lastException);
        throw lastException;
    }

    public void stopExecution() {
        bufferQueueManager.clearListQueue();
        putExecutionService.stopExecution();
        log.info("迁移任务已停止");
    }

    @PreDestroy
    public void destroy() {
        bufferQueueManager.clearListQueue();
        if (executorService != null) {
            executorService.shutdownNow();
        }
        if (startExecutorService != null) {
            startExecutorService.shutdownNow();
        }
        log.info("List执行器已关闭");
    }
}
