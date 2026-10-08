package com.transfer.tools.executor;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.S3ObjectSummary;
import com.amazonaws.services.s3.model.ObjectMetadata;
import com.transfer.tools.config.ThreadConfig;
import com.transfer.tools.config.LocalTransferConfig;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.service.ObjectMigrationService;
import com.transfer.tools.service.S3ClientManager;
import com.transfer.tools.service.SubTaskManagerService;
import com.transfer.tools.service.TaskProgressCache;
import com.transfer.tools.service.LocalTaskControlService;
import com.transfer.tools.service.TaskCancelledException;
import com.transfer.tools.model.FailureStage;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Qualifier;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import javax.annotation.PreDestroy;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Semaphore;

@Slf4j
@Service
public class PutExecutionService {

    @Autowired
    private BufferQueueManager bufferQueueManager;

    @Autowired
    private ObjectMigrationService objectMigrationService;

    @Autowired
    private ThreadConfig threadConfig;

    @Autowired
    private SubTaskManagerService subTaskManagerService;

    @Autowired
    private TaskProgressCache taskProgressCache;

    @Autowired
    private S3ClientManager s3ClientManager;

    @Autowired
    private LocalTaskControlService localTaskControlService;

    @Autowired
    private LocalTransferConfig localTransferConfig;

    @Autowired
    private com.transfer.tools.service.FileReportLedgerService fileReportLedgerService;

    @Autowired
    private com.transfer.tools.service.RateLimiterManager rateLimiterManager;

    private ExecutorService executorService;
    private Semaphore localTransferPermits;

    @PostConstruct
    public void init() {
        Integer threadCount = threadConfig.getDest().getPut();
        executorService = Executors.newFixedThreadPool(threadCount);
        localTransferPermits = new Semaphore(Math.max(1, localTransferConfig.getSmallFileConcurrency()), true);

        log.info("Put执行器初始化完成，启动 {} 个工作线程", threadCount);

        for (int i = 0; i < threadCount; i++) {
            executorService.submit(this::processMessages);
        }
    }

    private void processMessages() {
        while (!Thread.currentThread().isInterrupted()) {
            try {
                ObjectListMessage message = bufferQueueManager.takePutMessage();

                processMessage(message);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                log.info("Put工作线程被中断");
                break;
            } catch (Exception e) {
                log.error("Put工作线程异常", e);
            }
        }
    }

    private void processMessage(ObjectListMessage message) throws InterruptedException {
        if (localTaskControlService.isStopped(message.getTaskIndex())
                        || localTaskControlService.isStopped(message.getParentTaskId())) return;
        try (LocalTaskControlService.Operation ignored = localTaskControlService.begin(message.getTaskIndex(), message.getParentTaskId())) {
            SubTaskStatus current = subTaskManagerService.getTaskStatus(message.getTaskIndex());
            int expectedAttempt = current == null || current.getAttempt() == null ? 0 : current.getAttempt();
            int messageAttempt = message.getExecutionAttempt() == null ? 0 : message.getExecutionAttempt();
            if (current != null && expectedAttempt != messageAttempt) return;
            if (localTaskControlService.isStopped(message.getTaskIndex())) {
                return;
            }

            if (message.getIsLastMessage() != null && message.getIsLastMessage()) {
                log.info("任务 {} 的最后一个消息已处理完成", message.getTaskIndex());
                return;
            }

            S3ObjectSummary summary = message.getObjectSummary();
            String key = message.getKey();

            if (summary == null && key == null) {
                log.warn("消息中的对象和key都为空，跳过");
                return;
            }

            if (summary == null && key != null) {
                summary = new S3ObjectSummary();
                summary.setKey(key);
                summary.setBucketName(message.getSourceBucket());
                message.setObjectSummary(summary);
            }

            long itemSize = message.getSourceSize() != null
                    ? message.getSourceSize() : summary.getSize();
            long discoveredSize = 0L;
            boolean readingSourceMetadata = false;
            final String logicalKey = summary.getKey();

            Integer completionTaskId = message.getParentTaskId() == null
                    ? message.getTaskIndex() : message.getParentTaskId();
            boolean localPermit = message.getDirection() != null
                    && message.getDirection() != com.transfer.tools.model.TransferDirection.OSS_TO_OSS;
            boolean permitAcquired = false;
            try {
                if (localPermit) {
                    localTransferPermits.acquire();
                    permitAcquired = true;
                }
                localTaskControlService.checkRunning(message.getTaskIndex());
                if (Boolean.TRUE.equals(message.getSourceSizeUnknown())) {
                    // Inventory discovery counts keys only. Resolve the size once before
                    // choosing a transfer strategy, recording results, or comparing dates.
                    Long completedSize = fileReportLedgerService.completedSize(completionTaskId, logicalKey);
                    if (completedSize == null) {
                        readingSourceMetadata = true;
                        rateLimiterManager.acquireHead();
                        localTaskControlService.checkRunning(message.getTaskIndex());
                        ObjectMetadata metadata = getS3Client(message.getSourceS3Alias())
                                .getObjectMetadata(message.getSourceBucket(), logicalKey);
                        localTaskControlService.checkRunning(message.getTaskIndex());
                        summary.setSize(metadata.getContentLength());
                        summary.setLastModified(metadata.getLastModified());
                        summary.setETag(metadata.getETag());
                        if (metadata.getLastModified() != null) message.setSourceLastModified(metadata.getLastModified().getTime());
                        readingSourceMetadata = false;
                    } else {
                        // A durable completion must remain recoverable when the source is
                        // subsequently removed or changed. It does not need another HEAD.
                        summary.setSize(completedSize);
                    }
                    itemSize = summary.getSize();
                    discoveredSize = itemSize;
                    message.setSourceSize(itemSize);
                    message.setSourceSizeUnknown(false);
                    fileReportLedgerService.sourceMetadataResolved(message);
                }
                String durableResult = fileReportLedgerService.completedResult(completionTaskId, logicalKey);
                if ("SKIPPED".equals(durableResult)) {
                    taskProgressCache.addToProgress(message.getTaskIndex(), 0, discoveredSize,
                            0, 0L, 0, 0L, 1, itemSize, logicalKey);
                    return;
                }
                if ("SUCCESS".equals(durableResult) || subTaskManagerService.isItemCompleted(completionTaskId, logicalKey)) {
                    fileReportLedgerService.finished(message, "SUCCESS", "ALREADY_COMPLETED", null);
                    taskProgressCache.addToProgress(message.getTaskIndex(), 0, discoveredSize,
                            1, itemSize, 0, 0L, 0, 0L, logicalKey);
                    return;
                }
                boolean migrated = objectMigrationService.migrate(message);

                if (migrated && "DRY_RUN".equals(message.getReportReasonCode())) {
                    fileReportLedgerService.finished(message, "SKIPPED", "DRY_RUN", null);
                    taskProgressCache.addToProgress(message.getTaskIndex(), 0, discoveredSize,
                            0, 0L, 0, 0L, 1, itemSize, logicalKey);
                } else if (migrated) {
                    fileReportLedgerService.finished(message, "SUCCESS", "TRANSFERRED", null);
                    // The synced ledger is the commit point. Cancellation while waiting for the
                    // legacy completion index must not lose this successful file's progress.
                    taskProgressCache.addToProgress(
                        message.getTaskIndex(),
                        0, discoveredSize,
                        1, itemSize,
                        0, 0L,
                        summary.getKey()
                    );
                    try {
                        subTaskManagerService.saveCompletedItem(completionTaskId, message.getDirection(), logicalKey, itemSize);
                    } catch (RuntimeException indexError) {
                        log.warn("完成索引未及时写入，将使用已持久化文件结果恢复: task={}, key={}",
                                message.getTaskIndex(), logicalKey, indexError);
                    }
                } else {
                    fileReportLedgerService.finished(message, "SKIPPED", "TARGET_EXISTS", null);
                    taskProgressCache.addToProgress(
                        message.getTaskIndex(),
                        0, discoveredSize,
                        0, 0L,
                        0, 0L,
                        1, itemSize,
                        summary.getKey()
                    );
                }
            } catch (InterruptedException e) {
                if (!localTaskControlService.isStopped(message.getTaskIndex())) {
                    Thread.currentThread().interrupt();
                    throw e;
                }
                return;
            } catch (TaskCancelledException e) {
                log.info("任务 {} 已停止，条目 {} 未提交", message.getTaskIndex(), logicalKey);
            } catch (com.transfer.tools.service.FileReportLedgerService.FileReportPersistenceException e) {
                // The object may already have been written. Coverage carries the reporting gap.
                subTaskManagerService.recordFailedKey(message.getTaskIndex(), logicalKey, e.getMessage(),
                        itemSize, FailureStage.WRITE_DESTINATION, "REPORT_PERSISTENCE_FAILED", message.getLocalPath());
                taskProgressCache.addToProgress(message.getTaskIndex(), 0, discoveredSize, 0, 0L, 1, itemSize, logicalKey);
            } catch (Exception e) {
                if (localTaskControlService.isStopped(message.getTaskIndex())
                        || localTaskControlService.isStopped(message.getParentTaskId())) return;
                try {
                    fileReportLedgerService.finished(message, "FAILED", readingSourceMetadata ? "SOURCE_METADATA_FAILED" : "TRANSFER_FAILED", e.getMessage());
                } catch (RuntimeException reportError) {
                    e.addSuppressed(reportError);
                }
                log.error("迁移对象失败: {}", summary.getKey(), e);
                subTaskManagerService.recordFailedKey(
                    message.getTaskIndex(),
                    summary.getKey(),
                    e.getMessage(),
                    itemSize,
                    readingSourceMetadata ? FailureStage.READ_SOURCE : FailureStage.WRITE_DESTINATION,
                    readingSourceMetadata ? "SOURCE_METADATA_FAILED" : "TRANSFER_FAILED",
                    message.getLocalPath()
                );
                taskProgressCache.addToProgress(
                    message.getTaskIndex(),
                    0, discoveredSize,
                    0, 0L,
                    1, itemSize,
                    summary.getKey()
                );
            } finally {
                if (permitAcquired) localTransferPermits.release();
            }
        } catch (TaskCancelledException ignored) { }
    }

    private AmazonS3 getS3Client(String alias) {
        if (alias == null || alias.trim().isEmpty()) {
            throw new RuntimeException("alias is empty");
        }
        AmazonS3 client = s3ClientManager.getClient(alias);
        if (client == null) {
            throw new RuntimeException("get s3 client with alias ["+ alias +"] failed");
        }
        return client;
    }

    public void stopExecution() {
        bufferQueueManager.clearPutQueue();
        log.info("Put执行器已停止");
    }

    @PreDestroy
    public void destroy() {
        bufferQueueManager.clearPutQueue();
        if (executorService != null) {
            executorService.shutdownNow();
        }
        log.info("Put执行器已关闭");
    }
}
