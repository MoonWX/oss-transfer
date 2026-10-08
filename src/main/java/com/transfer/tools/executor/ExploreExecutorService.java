package com.transfer.tools.executor;

import com.transfer.tools.config.ThreadConfig;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.service.BucketExploreService;
import com.transfer.tools.service.EndpointNormalizationService;
import com.transfer.tools.service.LocalParentTaskService;
import com.transfer.tools.service.DownstreamWorkerException;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import javax.annotation.PreDestroy;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

@Slf4j
@Service
public class ExploreExecutorService {

    @Autowired
    private BucketExploreService bucketExploreService;

    @Autowired
    private BufferQueueManager bufferQueueManager;

    @Autowired
    private ThreadConfig threadConfig;

    @Autowired
    private EndpointNormalizationService endpointNormalizationService;

    @Autowired
    private LocalParentTaskService localParentTaskService;

    @Autowired
    private com.transfer.tools.service.TaskCreationService taskCreationService;

    @Autowired private com.transfer.tools.config.DistributedConfig distributedConfig;

    private ExecutorService executorService;
    private volatile boolean shuttingDown;

    @org.springframework.context.event.EventListener(org.springframework.boot.context.event.ApplicationReadyEvent.class)
    @org.springframework.core.annotation.Order(100)
    public void recoverPendingCreations() {
        if (!distributedConfig.isMaster()) return;
        for (BucketPair request : taskCreationService.recoverableRequests()) {
            if (!offerExploreTask(request))
                taskCreationService.fail(request.getCreationId(), "扫描队列已满，请稍后继续任务");
        }
    }

    @PostConstruct
    public void init() {
        Integer threadCount = threadConfig.getSrc().getExplore();
        executorService = Executors.newFixedThreadPool(threadCount);

        for (int i = 0; i < threadCount; i++) {
            executorService.submit(this::exploreWorker);
        }

        log.info("Explore执行器初始化完成，启动 {} 个工作线程", threadCount);
    }

    @PreDestroy
    public void destroy() {
        shuttingDown = true;
        if (executorService != null) {
            executorService.shutdownNow();
            log.info("Explore执行器已关闭");
        }
    }

    private void exploreWorker() {
        while (!Thread.currentThread().isInterrupted()) {
            try {
                BucketPair bucketPair = bufferQueueManager.takeExploreTask();
                executeExploreTask(bucketPair);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                log.info("Explore工作线程被中断");
                break;
            } catch (Exception e) {
                log.error("Explore工作线程异常", e);
            }
        }
    }

    private void executeExploreTask(BucketPair bucketPair) {
        try {
            if (taskCreationService != null && !taskCreationService.enterScope(bucketPair.getCreationId(), bucketPair)) return;
            endpointNormalizationService.normalize(bucketPair);
            log.info("开始探索bucket: {} -> {}, sourceS3Alias: {}, destS3Alias: {}, dateAfter: {}, dateBefore: {}, prefix: {}, overwrite: {}, distributionMode: {}",
                    bucketPair.getSourceBucket(), bucketPair.getDestBucket(),
                    bucketPair.getSourceS3Alias(), bucketPair.getDestS3Alias(),
                    bucketPair.getDateAfter(), bucketPair.getDateBefore(), bucketPair.getPrefix(), bucketPair.getOverwrite(),
                    bucketPair.getDistributionMode());

            if (bucketPair.getDirection() == com.transfer.tools.model.TransferDirection.OSS_TO_LOCAL) {
                if (!localParentTaskService.markRunning(bucketPair.getParentTaskId())) {
                    log.info("本地父任务已停止，跳过探索: {}", bucketPair.getParentTaskId());
                    return;
                }
                bucketExploreService.exploreLocalBucketAndCreateSubTasks(bucketPair);
                localParentTaskService.markOrchestrationComplete(bucketPair.getParentTaskId());
            } else {
                bucketExploreService.exploreSingleBucketAndCreateSubTasks(bucketPair);
            }

            if (taskCreationService != null) taskCreationService.complete(bucketPair.getCreationId());
            log.info("Explore任务完成: {} -> {}", bucketPair.getSourceBucket(), bucketPair.getDestBucket());
        } catch (java.util.concurrent.CancellationException e) {
            log.info("父任务探索已取消: {}", bucketPair.getParentTaskId());
        } catch (Exception e) {
            // Keep the RUNNING checkpoint recoverable when shutdown interrupts I/O.
            // A business/storage failure while the service is live remains explicitly FAILED.
            if (shuttingDown || Thread.currentThread().isInterrupted()) {
                log.info("服务停止，保留扫描检查点: {}", bucketPair.getCreationId());
                return;
            }
            if (taskCreationService != null) taskCreationService.fail(bucketPair.getCreationId(), e.getMessage());
            if (bucketPair.getDirection() == com.transfer.tools.model.TransferDirection.OSS_TO_LOCAL) {
                String code = e instanceof DownstreamWorkerException
                        ? ((DownstreamWorkerException) e).getCode() : "SOURCE_DISCOVERY_FAILED";
                localParentTaskService.markFailed(bucketPair.getParentTaskId(), code, e.getMessage());
            }
            log.error("探索bucket失败: {} -> {}", bucketPair.getSourceBucket(), bucketPair.getDestBucket(), e);
        } finally {
            if (taskCreationService != null) taskCreationService.clearScope();
        }
    }

    public int getExploreQueueSize() {
        return bufferQueueManager.exploreQueueSize();
    }

    public int getExploreQueueRemainingCapacity() {
        return bufferQueueManager.exploreQueueRemainingCapacity();
    }

    public boolean offerExploreTask(BucketPair bucketPair) {
        return bufferQueueManager.offerExploreTask(bucketPair);
    }
}
