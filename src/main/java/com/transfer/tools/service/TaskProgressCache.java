package com.transfer.tools.service;

import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.ProgressMessage;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.storage.RocksDBService;
import lombok.Data;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import javax.annotation.PreDestroy;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicBoolean;

@Slf4j
@Service
public class TaskProgressCache {

    @Autowired
    private SubTaskManagerService subTaskManagerService;

    @Autowired
    private BufferQueueManager bufferQueueManager;

    @Autowired
    private RocksDBService rocksDBService;

    private static final int BATCH_SIZE = 10000;
    private static final long FLUSH_INTERVAL_MS = 1000;

    private final Map<Integer, TaskIncrement> progressCache = new HashMap<>();
    private Thread consumerThread;
    private final AtomicBoolean running = new AtomicBoolean(true);

    @Data
    public static class TaskIncrement {
        private long listObjectCount = 0;
        private long listObjectSize = 0;
        private long putObjectCount = 0;
        private long putObjectSize = 0;
        private long failedObjectCount = 0;
        private long failedObjectSize = 0;
        private long skippedObjectCount = 0;
        private long skippedObjectSize = 0;
        private String lastMarker;
        private SubTaskStatus.TaskStatus newStatus;

        public void addListIncrement(long count, long size) {
            listObjectCount += count;
            listObjectSize += size;
        }

        public void addPutIncrement(long count, long size) {
            putObjectCount += count;
            putObjectSize += size;
        }

        public void addFailedIncrement(long count, long size) {
            failedObjectCount += count;
            failedObjectSize += size;
        }

        public void addSkippedIncrement(long count, long size) {
            skippedObjectCount += count;
            skippedObjectSize += size;
        }

        public void setMarker(String marker) {
            this.lastMarker = marker;
        }

        public void setStatus(SubTaskStatus.TaskStatus status) {
            this.newStatus = status;
        }

        public void reset() {
            listObjectCount = 0;
            listObjectSize = 0;
            putObjectCount = 0;
            putObjectSize = 0;
            failedObjectCount = 0;
            failedObjectSize = 0;
            skippedObjectCount = 0;
            skippedObjectSize = 0;
            lastMarker = null;
            newStatus = null;
        }

        public boolean isEmpty() {
            return listObjectCount == 0 && listObjectSize == 0 && putObjectCount == 0 && failedObjectCount == 0
                && skippedObjectCount == 0 && lastMarker == null && newStatus == null;
        }
    }

    @PostConstruct
    public void init() {
        consumerThread = new Thread(this::consumeMessages, "TaskProgressCache-Consumer");
        consumerThread.start();
        log.info("TaskProgressCache消费者线程已启动");
    }

    private void consumeMessages() {
        while (running.get()) {
            try {
                Thread.sleep(FLUSH_INTERVAL_MS);
                synchronized (subTaskManagerService) { flushToRocksDB(); }
            } catch (InterruptedException error) {
                Thread.currentThread().interrupt();
                break;
            } catch (Exception error) { log.error("刷新任务进度失败", error); }
        }
        synchronized (subTaskManagerService) { flushToRocksDB(); }
    }

    private void record(ProgressMessage message) {
        synchronized (subTaskManagerService) {
            TaskIncrement increment = progressCache.computeIfAbsent(message.getTaskIndex(), key -> new TaskIncrement());
            increment.addListIncrement(message.getListCount(), message.getListSize());
            increment.addPutIncrement(message.getPutCount(), message.getPutSize());
            increment.addFailedIncrement(message.getFailedCount(), message.getFailedSize());
            increment.addSkippedIncrement(message.getSkippedCount(), message.getSkippedSize());
            if (message.getMarker() != null) increment.setMarker(message.getMarker());
            if (message.getNewStatus() != null) increment.setStatus(message.getNewStatus());
        }
    }

    public void addToProgress(Integer taskIndex, long listCount, long listSize,
                              long putCount, long putSize,
                              long failedCount, long failedSize, String marker) {
        ProgressMessage message = new ProgressMessage(
            taskIndex, listCount, listSize, putCount, putSize,
            failedCount, failedSize, 0L, 0L, marker, null);
        record(message);
    }

    public void addToProgress(Integer taskIndex, long listCount, long listSize,
                              long putCount, long putSize,
                              long failedCount, long failedSize,
                              long skippedCount, long skippedSize, String marker) {
        ProgressMessage message = new ProgressMessage(
            taskIndex, listCount, listSize, putCount, putSize,
            failedCount, failedSize, skippedCount, skippedSize, marker, null);
        record(message);
    }

    public void updateTaskStatus(Integer taskIndex, SubTaskStatus.TaskStatus status) {
        ProgressMessage message = new ProgressMessage(
            taskIndex, 0L, 0L, 0L, 0L, 0L, 0L, 0L, 0L, null, status);
        record(message);
    }

    private void flushToRocksDB() {
        if (progressCache.isEmpty()) {
            return;
        }

        List<Integer> tasksToFlush = new ArrayList<>(progressCache.keySet());

        List<String> taskKeys = new ArrayList<>();
        for (Integer taskIndex : tasksToFlush) {
            taskKeys.add("task:" + taskIndex);
        }
        Map<String, SubTaskStatus> existingTasks = rocksDBService.multiGet(taskKeys, SubTaskStatus.class);

        Map<String, Object> batchUpdates = new HashMap<>();
        int flushedCount = 0;

        for (Integer taskIndex : tasksToFlush) {
            TaskIncrement increment = progressCache.get(taskIndex);
            if (increment == null || increment.isEmpty()) {
                continue;
            }

            long listCount = increment.getListObjectCount();
            long listSize = increment.getListObjectSize();
            long putCount = increment.getPutObjectCount();
            long putSize = increment.getPutObjectSize();
            long failedCount = increment.getFailedObjectCount();
            long failedSize = increment.getFailedObjectSize();
            long skippedCount = increment.getSkippedObjectCount();
            long skippedSize = increment.getSkippedObjectSize();
            String marker = increment.getLastMarker();
            SubTaskStatus.TaskStatus newStatus = increment.getNewStatus();

            SubTaskStatus task = existingTasks.get("task:" + taskIndex);
            if (task != null) {
                // Inventory keys can be flushed before PUT resolves their source sizes.
                if (listCount > 0 || listSize > 0) {
                    long currentCount = task.getListObjectCount() != null ? task.getListObjectCount() : 0L;
                    task.setListObjectCount(currentCount + listCount);
                    long currentSize = task.getListObjectSize() != null ? task.getListObjectSize() : 0L;
                    task.setListObjectSize(currentSize + listSize);
                }

                if (putCount > 0 || failedCount > 0) {
                    long currentPutCount = task.getPutObjectCount() != null ? task.getPutObjectCount() : 0L;
                    task.setPutObjectCount(currentPutCount + putCount);
                    long currentPutSize = task.getPutObjectSize() != null ? task.getPutObjectSize() : 0L;
                    task.setPutObjectSize(currentPutSize + putSize);

                    long currentFailedCount = task.getFailedObjectCount() != null ? task.getFailedObjectCount() : 0L;
                    task.setFailedObjectCount(currentFailedCount + failedCount);
                    long currentFailedSize = task.getFailedObjectSize() != null ? task.getFailedObjectSize() : 0L;
                    task.setFailedObjectSize(currentFailedSize + failedSize);
                }

                if (skippedCount > 0) {
                    long currentSkippedCount = task.getSkippedObjectCount() != null ? task.getSkippedObjectCount() : 0L;
                    task.setSkippedObjectCount(currentSkippedCount + skippedCount);
                    long currentSkippedSize = task.getSkippedObjectSize() != null ? task.getSkippedObjectSize() : 0L;
                    task.setSkippedObjectSize(currentSkippedSize + skippedSize);
                }

                if (marker != null) {
                    task.setMarker(marker);
                }

                long totalListCount = task.getListObjectCount() != null ? task.getListObjectCount() : 0L;
                long totalPutCount = task.getPutObjectCount() != null ? task.getPutObjectCount() : 0L;
                long totalFailedCount = task.getFailedObjectCount() != null ? task.getFailedObjectCount() : 0L;
                long totalSkippedCount = task.getSkippedObjectCount() != null ? task.getSkippedObjectCount() : 0L;

                if (newStatus == SubTaskStatus.TaskStatus.LIST_DONE
                        && task.getStatus() != SubTaskStatus.TaskStatus.STOPPED) {
                    task.setStatus(newStatus);
                    task.setScanComplete(true);
                }

                if (task.getStatus() == SubTaskStatus.TaskStatus.LIST_DONE) {
                    if (totalPutCount + totalFailedCount + totalSkippedCount >= totalListCount) {
                        if (totalFailedCount == 0) {
                            task.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
                        } else {
                            task.setStatus(SubTaskStatus.TaskStatus.PARTIAL_FAILED);
                        }
                        task.setEndTime(System.currentTimeMillis());
                        log.info("任务 {} 已结束，状态: {}, list: {}, put: {}, failed: {}, skipped: {}",
                                taskIndex, task.getStatus(), totalListCount, totalPutCount, totalFailedCount, totalSkippedCount);
                    }
                }

                batchUpdates.put("task:" + taskIndex, task);
                flushedCount++;
            }
        }

        if (!batchUpdates.isEmpty()) {
            rocksDBService.putBatch(batchUpdates);
            for (Integer taskIndex : tasksToFlush) progressCache.remove(taskIndex);
        }

        if (flushedCount > 0) {
            log.info("已刷新 {} 个任务的进度到RocksDB", flushedCount);
        }
    }

    public void clear() {
        synchronized (subTaskManagerService) {
            bufferQueueManager.clearProgressQueue();
            progressCache.clear();
        }
    }

    public void clearTaskProgress(Integer taskIndex) {
        synchronized (subTaskManagerService) {
            bufferQueueManager.removeTaskProgress(taskIndex);
            progressCache.remove(taskIndex);
        }
    }

    public void clearTaskProgressBatch(List<Integer> taskIndices) {
        if (taskIndices == null) return;
        synchronized (subTaskManagerService) {
            for (Integer taskIndex : taskIndices) clearTaskProgress(taskIndex);
        }
    }

    @PreDestroy
    public void destroy() {
        running.set(false);
        if (consumerThread != null) {
            consumerThread.interrupt();
            try {
                consumerThread.join(5000);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        }
        log.info("TaskProgressCache已关闭");
    }
}
