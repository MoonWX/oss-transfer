package com.transfer.tools.executor;

import com.transfer.tools.model.FailedKey;
import com.transfer.tools.model.FailedKeyMessage;
import com.transfer.tools.model.FallbackMessage;
import com.transfer.tools.model.FallbackObject;
import com.transfer.tools.storage.RocksDBService;
import com.transfer.tools.config.LocalTransferConfig;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import javax.annotation.PreDestroy;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

@Slf4j
@Service
public class FailedKeyConsumerService {

    @Autowired
    private BufferQueueManager bufferQueueManager;

    @Autowired
    private RocksDBService rocksDBService;

    @Autowired
    private LocalTransferConfig localTransferConfig;

    private static final String FAILED_KEY_PREFIX = "failed:";
    private static final String FAILED_COUNT_KEY = "failed:count";
    private static final String FALLBACK_KEY_PREFIX = "fallback:";
    private static final String FALLBACK_COUNT_KEY = "fallback:count";

    private ExecutorService executor;
    private Thread failedKeyConsumerThread;
    private Thread fallbackConsumerThread;
    private final AtomicBoolean running = new AtomicBoolean(true);

    @PostConstruct
    public void init() {
        failedKeyConsumerThread = new Thread(this::consumeFailedKeys, "failed-key-consumer");
        failedKeyConsumerThread.setDaemon(true);
        failedKeyConsumerThread.start();

        fallbackConsumerThread = new Thread(this::consumeFallbacks, "fallback-consumer");
        fallbackConsumerThread.setDaemon(true);
        fallbackConsumerThread.start();

        log.info("FailedKeyConsumerService 已启动");
    }

    @PreDestroy
    public void shutdown() {
        running.set(false);

        if (failedKeyConsumerThread != null) {
            failedKeyConsumerThread.interrupt();
        }
        if (fallbackConsumerThread != null) {
            fallbackConsumerThread.interrupt();
        }

        try {
            if (failedKeyConsumerThread != null) {
                failedKeyConsumerThread.join(5000);
            }
            if (fallbackConsumerThread != null) {
                fallbackConsumerThread.join(5000);
            }
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }

        log.info("FailedKeyConsumerService 已停止");
    }

    private void consumeFailedKeys() {
        while (running.get()) {
            try {
                FailedKeyMessage message = bufferQueueManager.takeFailedKey();
                List<FailedKeyMessage> messages = new ArrayList<>();
                messages.add(message);
                int maxBatch = Math.max(1, localTransferConfig.getCompletionWriteBatch());
                bufferQueueManager.drainFailedKeyQueue(messages, maxBatch - 1);
                synchronized (rocksDBService) {
                    Long failedCount = rocksDBService.get(FAILED_COUNT_KEY, Long.class);
                    if (failedCount == null) {
                        failedCount = 0L;
                    }
                    Map<String, Object> writes = new LinkedHashMap<>();
                    long index = failedCount;
                    for (FailedKeyMessage item : messages) {
                        if (isPurged(item.getTaskIndex(), item.getParentTaskId())) continue;
                        index++;
                        writes.put(FAILED_KEY_PREFIX + index, toFailedKey(item));
                    }
                    writes.put(FAILED_COUNT_KEY, index);
                    rocksDBService.putBatch(writes);
                }

                log.debug("批量记录 {} 个迁移失败对象", messages.size());
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                break;
            } catch (Exception e) {
                log.error("消费失败对象消息异常", e);
            }
        }
    }

    private boolean isPurged(Integer taskId, Integer parentId) {
        return Boolean.TRUE.equals(rocksDBService.get("task:purged:" + taskId, Boolean.class))
                || Boolean.TRUE.equals(rocksDBService.get("report:purged:" + (parentId == null ? taskId : parentId), Boolean.class));
    }

    private FailedKey toFailedKey(FailedKeyMessage message) {
        FailedKey failedKey = new FailedKey();
        failedKey.setTaskIndex(message.getTaskIndex());
        failedKey.setSourceBucket(message.getSourceBucket());
        failedKey.setDestBucket(message.getDestBucket());
        failedKey.setKey(message.getKey());
        failedKey.setDestinationKey(message.getDestinationKey());
        failedKey.setTimestamp(System.currentTimeMillis());
        failedKey.setObjectSize(message.getObjectSize());
        failedKey.setErrorMessage(message.getErrorMessage());
        failedKey.setDirection(message.getDirection());
        failedKey.setSourceEndpoint(message.getSourceEndpoint());
        failedKey.setDestinationEndpoint(message.getDestinationEndpoint());
        failedKey.setLocalPath(message.getLocalPath());
        failedKey.setExecutionNodeIp(message.getExecutionNodeIp());
        failedKey.setWorkerNode(message.getWorkerNode());
        failedKey.setStage(message.getStage());
        failedKey.setErrorCode(message.getErrorCode());
        failedKey.setRetryCount(message.getRetryCount());
        failedKey.setParentTaskId(message.getParentTaskId());
        failedKey.setShardId(message.getShardId());
        failedKey.setAttempt(message.getAttempt());
        failedKey.setStorageId(message.getStorageId());
        failedKey.setEligibleWorkerNodes(message.getEligibleWorkerNodes());
        return failedKey;
    }

    private void consumeFallbacks() {
        while (running.get()) {
            try {
                FallbackMessage message = bufferQueueManager.takeFallback();

                synchronized (rocksDBService) {
                    if (isPurged(message.getTaskIndex(), null)) continue;
                    Long fallbackCount = rocksDBService.get(FALLBACK_COUNT_KEY, Long.class);
                    if (fallbackCount == null) {
                        fallbackCount = 0L;
                    }

                    long newFallbackIndex = fallbackCount + 1;
                    String fallbackKeyStr = FALLBACK_KEY_PREFIX + newFallbackIndex;

                    FallbackObject fallbackObject = new FallbackObject();
                    fallbackObject.setTaskIndex(message.getTaskIndex());
                    fallbackObject.setSourceBucket(message.getSourceBucket());
                    fallbackObject.setDestBucket(message.getDestBucket());
                    fallbackObject.setKey(message.getKey());
                    fallbackObject.setDestinationKey(message.getDestinationKey());
                    fallbackObject.setTimestamp(System.currentTimeMillis());

                    rocksDBService.put(fallbackKeyStr, fallbackObject);
                    rocksDBService.put(FALLBACK_COUNT_KEY, newFallbackIndex);
                }

                log.debug("记录降级迁移对象: taskIndex={}, key={}", message.getTaskIndex(), message.getKey());
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                break;
            } catch (Exception e) {
                log.error("消费降级对象消息异常", e);
            }
        }
    }
}
