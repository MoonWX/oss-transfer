package com.transfer.tools.service;

import com.amazonaws.services.s3.model.S3ObjectSummary;
import com.transfer.tools.model.FailedKey;
import com.transfer.tools.model.FailedKeyMessage;
import com.transfer.tools.model.FallbackMessage;
import com.transfer.tools.model.FallbackObject;
import com.transfer.tools.model.ListSubTask;
import com.transfer.tools.model.MigrationStatus;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TaskImport;
import com.transfer.tools.model.CompletedItemRecord;
import com.transfer.tools.model.ConflictPolicy;
import com.transfer.tools.model.FailureStage;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.model.TransferEndpoint;
import com.transfer.tools.config.LocalTransferConfig;
import com.transfer.tools.storage.RocksDBService;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Objects;
import java.util.Set;
import java.nio.charset.StandardCharsets;
import java.util.Base64;
import java.util.LinkedHashMap;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import javax.annotation.PreDestroy;

@Slf4j
@Service
public class SubTaskManagerService {

    @Autowired private TaskBandwidthDispatchGuard taskBandwidthGuard;
    @Autowired
    private RocksDBService rocksDBService;

    @Autowired
    private com.transfer.tools.executor.BufferQueueManager bufferQueueManager;

    @Autowired
    private TaskProgressCache taskProgressCache;

    @Autowired
    private EndpointNormalizationService endpointNormalizationService;

    @Autowired
    private LocalTransferConfig localTransferConfig;

    @Autowired
    private FileReportLedgerService fileReportLedgerService;

    @Autowired private LocalTaskControlService localTaskControlService;

    private final Object completionLock = new Object();
    private final List<PendingCompletion> pendingCompletions = new ArrayList<>();
    private boolean completionFlushScheduled;
    private final ScheduledExecutorService completionWriter = Executors.newSingleThreadScheduledExecutor(r -> {
        Thread thread = new Thread(r, "completion-write-batch");
        thread.setDaemon(true);
        return thread;
    });

    private final java.util.concurrent.atomic.AtomicLong identitySequence = new java.util.concurrent.atomic.AtomicLong();
    private final java.util.concurrent.ConcurrentMap<Integer, Long> taskIdentityVersions = new java.util.concurrent.ConcurrentHashMap<>();
    public long taskIdentityVersion(Integer taskId) { return taskIdentityVersions.getOrDefault(taskId, 0L); }
    private void changedTaskIdentities(java.util.Collection<Integer> ids) {
        for (Integer id : ids) taskIdentityVersions.put(id, identitySequence.incrementAndGet());
    }

    private static final String TASK_KEY_PREFIX = "task:";
    private static final String TASK_ID_SEQUENCE_KEY = "task:id:sequence";
    private static final String TASK_ID_LIST_KEY = "task:id:list";
    private static final String DELETED_TASK_KEY_PREFIX = "task:deleted:";
    private static final String DELETED_TASK_ID_LIST_KEY = "task:id:list:deleted";
    private static final String FAILED_KEY_PREFIX = "failed:";
    private static final String FAILED_COUNT_KEY = "failed:count";
    private static final String FALLBACK_KEY_PREFIX = "fallback:";
    private static final String FALLBACK_COUNT_KEY = "fallback:count";
    private static final String COMPLETED_KEY_PREFIX = "completed:";
    private static final String LOCAL_SHARD_ASSIGNMENT_PREFIX = "local:shard-assignment:";

    public TaskProgressCache getTaskProgressCache() {
        return taskProgressCache;
    }

    public synchronized Integer getNextTaskId() {
        Integer currentId = rocksDBService.get(TASK_ID_SEQUENCE_KEY, Integer.class);
        if (currentId == null) {
            currentId = 0;
        }
        Integer nextId = currentId + 1;
        rocksDBService.put(TASK_ID_SEQUENCE_KEY, nextId);
        return nextId;
    }

    public synchronized Integer allocateTaskIdRange(int count) {
        Integer currentId = rocksDBService.get(TASK_ID_SEQUENCE_KEY, Integer.class);
        if (currentId == null) {
            currentId = 0;
        }
        Integer startId = currentId + 1;
        Integer endId = currentId + count;
        rocksDBService.putSync(TASK_ID_SEQUENCE_KEY, endId);
        return startId;
    }

    public synchronized void updateTaskIdSequence(Integer endIndex) {
        rocksDBService.put(TASK_ID_SEQUENCE_KEY, endIndex);
    }

    @SuppressWarnings("unchecked")
    public void addToTaskIdList(Integer taskId) {
        List<Integer> taskIdList = rocksDBService.get(TASK_ID_LIST_KEY, List.class);
        if (taskIdList == null) {
            taskIdList = new ArrayList<>();
        }
        taskIdList.add(taskId);
        rocksDBService.put(TASK_ID_LIST_KEY, taskIdList);
        changedTaskIdentities(java.util.Collections.singletonList(taskId));
    }

    @SuppressWarnings("unchecked")
    public void removeFromTaskIdList(Integer taskId) {
        List<Integer> taskIdList = rocksDBService.get(TASK_ID_LIST_KEY, List.class);
        if (taskIdList != null) {
            taskIdList.remove(taskId);
            rocksDBService.put(TASK_ID_LIST_KEY, taskIdList);
            taskIdentityVersions.remove(taskId);
        }
    }

    @SuppressWarnings("unchecked")
    public List<Integer> getTaskIdList() {
        List<Integer> taskIdList = rocksDBService.get(TASK_ID_LIST_KEY, List.class);
        return taskIdList != null ? taskIdList : new ArrayList<>();
    }

    public void saveSubTasks(List<ListSubTask> subTasks) {
        Integer startIndex = getNextTaskId();
        Integer endIndex = startIndex + subTasks.size() - 1;

        Map<String, Object> batchData = new HashMap<>();
        List<Integer> newTaskIds = new ArrayList<>();

        for (int i = 0; i < subTasks.size(); i++) {
            ListSubTask task = subTasks.get(i);
            Integer taskIndex = startIndex + i;
            SubTaskStatus status = new SubTaskStatus();
            status.setTaskIndex(taskIndex);
            status.setSourceBucket(task.getSourceBucket());
            status.setDestBucket(task.getDestBucket());
            status.setSourceS3Alias(task.getSourceS3Alias());
            status.setDestS3Alias(task.getDestS3Alias());
            status.setStartKey(task.getStartKey());
            status.setEndKey(task.getEndKey());
            status.setDateAfter(task.getDateAfter());
            status.setDateBefore(task.getDateBefore());
            status.setOverwrite(task.getOverwrite());
            copyTransferFields(task, status);
            status.setStatus(SubTaskStatus.TaskStatus.PENDING);
            status.setListObjectCount(0L);
            status.setListObjectSize(0L);
            status.setPutObjectCount(0L);
            status.setPutObjectSize(0L);
            status.setFailedObjectCount(0L);
            status.setFailedObjectSize(0L);
            status.setSkippedObjectCount(0L);
            status.setSkippedObjectSize(0L);
            status.setCreateTime(System.currentTimeMillis());

            String key = TASK_KEY_PREFIX + taskIndex;
            fileReportLedgerService.registerTask(status);
            batchData.put(key, status);
            newTaskIds.add(taskIndex);
        }

        rocksDBService.put(TASK_ID_SEQUENCE_KEY, endIndex);
        rocksDBService.putBatch(batchData);

        @SuppressWarnings("unchecked")
        List<Integer> taskIdList = rocksDBService.get(TASK_ID_LIST_KEY, List.class);
        if (taskIdList == null) {
            taskIdList = new ArrayList<>();
        }
        taskIdList.addAll(newTaskIds);
        rocksDBService.put(TASK_ID_LIST_KEY, taskIdList);
        changedTaskIdentities(newTaskIds);

        log.info("已保存 {} 个子任务，任务ID范围: {} - {}", subTasks.size(), startIndex, endIndex);
    }

    public SubTaskStatus getTaskStatus(Integer taskIndex) {
        String key = TASK_KEY_PREFIX + taskIndex;
        SubTaskStatus status = rocksDBService.get(key, SubTaskStatus.class);
        if (status != null) {
            endpointNormalizationService.normalize(status);
        }
        return status;
    }

    public synchronized void importInventoryTask(SubTaskStatus task) {
        if (task.getInventoryManifestId() == null) throw new IllegalArgumentException("缺少持久化清单");
        List<Integer> ids = new ArrayList<>(getTaskIdList());
        if (!ids.contains(task.getTaskIndex())) ids.add(task.getTaskIndex());
        Map<String,Object> values = new LinkedHashMap<>();
        values.put(TASK_KEY_PREFIX + task.getTaskIndex(), task);
        values.put(TASK_ID_LIST_KEY, ids);
        rocksDBService.putBatchSync(values);
        changedTaskIdentities(java.util.Collections.singletonList(task.getTaskIndex()));
    }

    public synchronized void updateTaskStatus(SubTaskStatus status) {
        if (Boolean.TRUE.equals(rocksDBService.get("task:purged:" + status.getTaskIndex(), Boolean.class))
                || (status.getParentTaskId() != null && Boolean.TRUE.equals(rocksDBService.get(
                        "local:parent:purged:" + status.getParentTaskId(), Boolean.class)))) return;
        if (localTaskControlService != null
                && (localTaskControlService.isStopped(status.getTaskIndex())
                    || localTaskControlService.isStopped(status.getParentTaskId()))
                && status.getStatus() != SubTaskStatus.TaskStatus.STOPPED) return;
        endpointNormalizationService.normalize(status);
        String key = TASK_KEY_PREFIX + status.getTaskIndex();
        rocksDBService.put(key, status);
    }

    /** Scanner snapshots predate progress flushes; changing failure state must preserve current counters. */
    public synchronized void failTask(Integer taskId) {
        SubTaskStatus current = getTaskStatus(taskId);
        if (current == null) return;
        if (localTaskControlService != null && (localTaskControlService.isStopped(taskId)
                || localTaskControlService.isStopped(current.getParentTaskId()))) return;
        current.setStatus(SubTaskStatus.TaskStatus.FAILED);
        current.setEndTime(System.currentTimeMillis());
        updateTaskStatus(current);
    }

    public synchronized void saveAssignedTask(Integer taskId, ListSubTask task) {
        if (taskId == null) {
            taskId = getNextTaskId();
        } else if (taskId > 0) {
            Integer sequence = rocksDBService.get(TASK_ID_SEQUENCE_KEY, Integer.class);
            if (sequence == null || sequence < taskId) {
                rocksDBService.put(TASK_ID_SEQUENCE_KEY, taskId);
            }
        }
        if (Boolean.TRUE.equals(rocksDBService.get("task:purged:" + taskId, Boolean.class))
                || (task.getParentTaskId() != null && Boolean.TRUE.equals(rocksDBService.get(
                        "local:parent:purged:" + task.getParentTaskId(), Boolean.class))))
            throw new IllegalStateException("已永久删除的任务不能重新导入");
        endpointNormalizationService.normalize(task);
        SubTaskStatus status = new SubTaskStatus();
        status.setTaskIndex(taskId);
        status.setSourceBucket(task.getSourceBucket());
        status.setDestBucket(task.getDestBucket());
        status.setSourceS3Alias(task.getSourceS3Alias());
        status.setDestS3Alias(task.getDestS3Alias());
        status.setStartKey(task.getStartKey());
        status.setEndKey(task.getEndKey());
        status.setDateAfter(task.getDateAfter());
        status.setDateBefore(task.getDateBefore());
        status.setOverwrite(task.getOverwrite());
        status.setWorkerNode(task.getExecutionNodeIp());
        copyTransferFields(task, status);
        status.setStatus(SubTaskStatus.TaskStatus.PENDING);
        status.setListObjectCount(0L);
        status.setListObjectSize(0L);
        status.setPutObjectCount(0L);
        status.setPutObjectSize(0L);
        status.setFailedObjectCount(0L);
        status.setFailedObjectSize(0L);
        status.setSkippedObjectCount(0L);
        status.setSkippedObjectSize(0L);
        status.setCreateTime(System.currentTimeMillis());
        fileReportLedgerService.registerTask(status);
        List<Integer> ids = getTaskIdList();
        if (!ids.contains(taskId)) ids.add(taskId);
        Map<String, Object> saved = new LinkedHashMap<>();
        saved.put(TASK_KEY_PREFIX + taskId, status);
        saved.put(TASK_ID_LIST_KEY, ids);
        if (task.getParentTaskId() != null && task.getShardId() != null) {
            saved.put(localShardAssignmentKey(task.getParentTaskId(), task.getShardId()), taskId);
        }
        rocksDBService.putBatchSync(saved);
        changedTaskIdentities(java.util.Collections.singletonList(taskId));
    }

    public synchronized void ensureTaskIndexed(Integer taskId) {
        List<Integer> ids = getTaskIdList();
        if (!ids.contains(taskId)) { ids.add(taskId); rocksDBService.putSync(TASK_ID_LIST_KEY, ids); changedTaskIdentities(java.util.Collections.singletonList(taskId)); }
    }

    public int preferredLocalShardTaskId(TransferDirection direction, Integer parentTaskId, String shardId) {
        int hash = Objects.hash(direction == null ? "LOCAL" : direction.name(), parentTaskId, shardId);
        if (hash == Integer.MIN_VALUE) return Integer.MIN_VALUE + 1;
        int candidate = -Math.abs(hash);
        return candidate == 0 ? -1 : candidate;
    }

    public synchronized Integer saveAssignedLocalTask(ListSubTask task) {
        if (task == null || task.getParentTaskId() == null || task.getShardId() == null) {
            throw new TransferValidationException("INVALID_ENDPOINT", "LOCAL shard requires parentTaskId and shardId");
        }
        String assignmentKey = localShardAssignmentKey(task.getParentTaskId(), task.getShardId());
        Integer assigned = rocksDBService.get(assignmentKey, Integer.class);
        if (assigned != null) {
            SubTaskStatus existing = getTaskStatus(assigned);
            if (existing != null && task.getShardId().equals(existing.getShardId())) {
                ensureTaskIndexed(assigned); return assigned;
            }
        }
        int candidate = assigned == null
                ? preferredLocalShardTaskId(task.getDirection(), task.getParentTaskId(), task.getShardId()) : assigned;
        candidate = findAvailableLocalTaskId(candidate, task.getShardId());
        SubTaskStatus existingTask = getTaskStatus(candidate);
        if (existingTask != null && Objects.equals(existingTask.getParentTaskId(), task.getParentTaskId())
                && Objects.equals(existingTask.getShardId(), task.getShardId())) {
            rocksDBService.putSync(assignmentKey, candidate);
            ensureTaskIndexed(candidate);
            return candidate;
        }
        saveAssignedTask(candidate, task);
        rocksDBService.put(assignmentKey, candidate);
        return candidate;
    }

    private int findAvailableLocalTaskId(int initial, String shardId) {
        int candidate = initial >= 0 ? -Math.max(1, initial) : initial;
        while (true) {
            SubTaskStatus existing = getTaskStatus(candidate);
            if (existing == null || shardId.equals(existing.getShardId())) return candidate;
            candidate = candidate == Integer.MIN_VALUE + 1 ? -1 : candidate - 1;
        }
    }

    private String localShardAssignmentKey(Integer parentTaskId, String shardId) {
        String encoded = Base64.getUrlEncoder().withoutPadding()
                .encodeToString(shardId.getBytes(StandardCharsets.UTF_8));
        return LOCAL_SHARD_ASSIGNMENT_PREFIX + parentTaskId + ":" + encoded;
    }

    private void copyTransferFields(ListSubTask source, SubTaskStatus target) {
        if (taskBandwidthGuard != null) taskBandwidthGuard.requireReady(source.getTaskBandwidthScopeId(), source.getTaskBandwidthLimitMbps());
        target.setDirection(source.getDirection());
        target.setSourceEndpoint(source.getSourceEndpoint());
        target.setDestinationEndpoint(source.getDestinationEndpoint());
        target.setConflictPolicy(source.getConflictPolicy());
        target.setLocalPath(source.getLocalPath());
        target.setExecutionNodeIp(source.getExecutionNodeIp());
        target.setObjectKey(source.getObjectKey());
        target.setTargetRelativePath(source.getTargetRelativePath());
        target.setDistributionMode(source.getDistributionMode());
        target.setParentTaskId(source.getParentTaskId());
        target.setShardId(source.getShardId());
        target.setShardIndex(source.getShardIndex());
        target.setAttempt(source.getAttempt());
        target.setAutoStart(source.getAutoStart());
        target.setStorageId(source.getStorageId());
        target.setEligibleWorkerNodes(source.getEligibleWorkerNodes());
        target.setNodeBindings(source.getNodeBindings());
        target.setLocalFileEntries(source.getLocalFileEntries());
        target.setListQpsLimit(source.getListQpsLimit());
        target.setHeadQpsLimit(source.getHeadQpsLimit());
        target.setPutQpsLimit(source.getPutQpsLimit());
        target.setBandwidthBitsPerSecond(source.getBandwidthBitsPerSecond());
        target.setTaskBandwidthLimitMbps(source.getTaskBandwidthLimitMbps());
        target.setTaskBandwidthScopeId(source.getTaskBandwidthScopeId());
        target.setKeyMapping(source.getKeyMapping());
    }

    private void copyTransferFields(TaskImport source, SubTaskStatus target) {
        if (taskBandwidthGuard != null) taskBandwidthGuard.requireReady(source.getTaskBandwidthScopeId(), source.getTaskBandwidthLimitMbps());
        target.setDirection(source.getDirection());
        target.setSourceEndpoint(source.getSourceEndpoint());
        target.setDestinationEndpoint(source.getDestinationEndpoint());
        target.setConflictPolicy(source.getConflictPolicy());
        target.setLocalPath(source.getLocalPath());
        target.setExecutionNodeIp(source.getExecutionNodeIp());
        target.setObjectKey(source.getObjectKey());
        target.setTargetRelativePath(source.getTargetRelativePath());
        target.setDistributionMode(source.getDistributionMode());
        target.setParentTaskId(source.getParentTaskId());
        target.setShardId(source.getShardId());
        target.setShardIndex(source.getShardIndex());
        target.setAttempt(source.getAttempt());
        target.setAutoStart(source.getAutoStart());
        target.setStorageId(source.getStorageId());
        target.setEligibleWorkerNodes(source.getEligibleWorkerNodes());
        target.setNodeBindings(source.getNodeBindings());
        target.setLocalFileEntries(source.getLocalFileEntries());
        target.setListQpsLimit(source.getListQpsLimit());
        target.setHeadQpsLimit(source.getHeadQpsLimit());
        target.setPutQpsLimit(source.getPutQpsLimit());
        target.setBandwidthBitsPerSecond(source.getBandwidthBitsPerSecond());
        target.setTaskBandwidthLimitMbps(source.getTaskBandwidthLimitMbps());
        target.setTaskBandwidthScopeId(source.getTaskBandwidthScopeId());
        target.setKeyMapping(source.getKeyMapping());
    }

    public MigrationStatus getMigrationStatus() {
        List<Integer> taskIdList = getTaskIdList();
        if (taskIdList.isEmpty()) {
            return new MigrationStatus(0, 0, 0, 0, 0, 0, 0, 0L, 0L, 0L, 0L, 0L, 0L, 0L, 0L, new ArrayList<>());
        }

        List<String> taskKeys = new ArrayList<>();
        for (Integer taskId : taskIdList) {
            taskKeys.add(TASK_KEY_PREFIX + taskId);
        }
        Map<String, SubTaskStatus> taskMap = rocksDBService.multiGet(taskKeys, SubTaskStatus.class);

        List<SubTaskStatus> tasks = new ArrayList<>();
        int pendingCount = 0;
        int runningCount = 0;
        int completedCount = 0;
        int partialFailedCount = 0;
        int failedCount = 0;
        int listDoneCount = 0;
        long totalListObjectCount = 0L;
        long totalListObjectSize = 0L;
        long totalPutObjectCount = 0L;
        long totalPutObjectSize = 0L;
        long totalFailedObjectCount = 0L;
        long totalFailedObjectSize = 0L;
        long totalSkippedObjectCount = 0L;
        long totalSkippedObjectSize = 0L;

        for (Integer taskId : taskIdList) {
            SubTaskStatus task = taskMap.get(TASK_KEY_PREFIX + taskId);
            if (task != null) {
                endpointNormalizationService.normalize(task);
                tasks.add(task);
                switch (task.getStatus()) {
                    case PENDING:
                        pendingCount++;
                        break;
                    case RUNNING:
                        runningCount++;
                        break;
                    case LIST_DONE:
                        listDoneCount++;
                        break;
                    case COMPLETED:
                        completedCount++;
                        break;
                    case PARTIAL_FAILED:
                        partialFailedCount++;
                        break;
                    case FAILED:
                        failedCount++;
                        break;
                    case STOPPED:
                        break;
                }

                if (task.getListObjectCount() != null) {
                    totalListObjectCount += task.getListObjectCount();
                }
                if (task.getListObjectSize() != null) {
                    totalListObjectSize += task.getListObjectSize();
                }
                if (task.getPutObjectCount() != null) {
                    totalPutObjectCount += task.getPutObjectCount();
                }
                if (task.getPutObjectSize() != null) {
                    totalPutObjectSize += task.getPutObjectSize();
                }
                if (task.getFailedObjectCount() != null) {
                    totalFailedObjectCount += task.getFailedObjectCount();
                }
                if (task.getFailedObjectSize() != null) {
                    totalFailedObjectSize += task.getFailedObjectSize();
                }
                if (task.getSkippedObjectCount() != null) {
                    totalSkippedObjectCount += task.getSkippedObjectCount();
                }
                if (task.getSkippedObjectSize() != null) {
                    totalSkippedObjectSize += task.getSkippedObjectSize();
                }
            }
        }

        return new MigrationStatus(taskIdList.size(), pendingCount, runningCount,
                                   completedCount, partialFailedCount, failedCount, listDoneCount, totalListObjectCount,
                                   totalListObjectSize, totalPutObjectCount,
                                   totalPutObjectSize, totalFailedObjectCount,
                                   totalFailedObjectSize, totalSkippedObjectCount,
                                   totalSkippedObjectSize, tasks);
    }

    public MigrationStatus getMigrationStatus(String sourceBucket, String destBucket) {
        return getMigrationStatus(sourceBucket, destBucket, null, null);
    }

    public MigrationStatus getMigrationStatus(String sourceBucket, String destBucket, String status, String workerNode) {
        List<Integer> taskIdList = getTaskIdList();
        if (taskIdList.isEmpty()) {
            return new MigrationStatus(0, 0, 0, 0, 0, 0, 0, 0L, 0L, 0L, 0L, 0L, 0L, 0L, 0L, new ArrayList<>());
        }

        List<String> taskKeys = new ArrayList<>();
        for (Integer taskId : taskIdList) {
            taskKeys.add(TASK_KEY_PREFIX + taskId);
        }
        Map<String, SubTaskStatus> taskMap = rocksDBService.multiGet(taskKeys, SubTaskStatus.class);

        List<SubTaskStatus> tasks = new ArrayList<>();
        int pendingCount = 0;
        int runningCount = 0;
        int completedCount = 0;
        int partialFailedCount = 0;
        int failedCount = 0;
        int listDoneCount = 0;
        long totalListObjectCount = 0L;
        long totalListObjectSize = 0L;
        long totalPutObjectCount = 0L;
        long totalPutObjectSize = 0L;
        long totalFailedObjectCount = 0L;
        long totalFailedObjectSize = 0L;
        long totalSkippedObjectCount = 0L;
        long totalSkippedObjectSize = 0L;

        for (Integer taskId : taskIdList) {
            SubTaskStatus task = taskMap.get(TASK_KEY_PREFIX + taskId);
            if (task != null) {
                endpointNormalizationService.normalize(task);
                boolean match = true;
                if (sourceBucket != null && !sourceBucket.isEmpty() && !sourceBucket.equals(task.getSourceBucket())) {
                    match = false;
                }
                if (destBucket != null && !destBucket.isEmpty() && !destBucket.equals(task.getDestBucket())) {
                    match = false;
                }
                if (status != null && !status.isEmpty() && !status.equals(task.getStatus().name())) {
                    match = false;
                }
                if (workerNode != null && !workerNode.isEmpty() && !workerNode.equals(task.getWorkerNode())) {
                    match = false;
                }

                if (match) {
                    tasks.add(task);
                    switch (task.getStatus()) {
                        case PENDING:
                            pendingCount++;
                            break;
                        case RUNNING:
                            runningCount++;
                            break;
                        case LIST_DONE:
                            listDoneCount++;
                            break;
                        case COMPLETED:
                            completedCount++;
                            break;
                        case PARTIAL_FAILED:
                            partialFailedCount++;
                            break;
                        case FAILED:
                            failedCount++;
                            break;
                        case STOPPED:
                            break;
                    }

                    if (task.getListObjectCount() != null) {
                        totalListObjectCount += task.getListObjectCount();
                    }
                    if (task.getListObjectSize() != null) {
                        totalListObjectSize += task.getListObjectSize();
                    }
                    if (task.getPutObjectCount() != null) {
                        totalPutObjectCount += task.getPutObjectCount();
                    }
                    if (task.getPutObjectSize() != null) {
                        totalPutObjectSize += task.getPutObjectSize();
                    }
                    if (task.getFailedObjectCount() != null) {
                        totalFailedObjectCount += task.getFailedObjectCount();
                    }
                    if (task.getFailedObjectSize() != null) {
                        totalFailedObjectSize += task.getFailedObjectSize();
                    }
                    if (task.getSkippedObjectCount() != null) {
                        totalSkippedObjectCount += task.getSkippedObjectCount();
                    }
                    if (task.getSkippedObjectSize() != null) {
                        totalSkippedObjectSize += task.getSkippedObjectSize();
                    }
                }
            }
        }

        return new MigrationStatus(tasks.size(), pendingCount, runningCount,
                                   completedCount, partialFailedCount, failedCount, listDoneCount, totalListObjectCount,
                                   totalListObjectSize, totalPutObjectCount,
                                   totalPutObjectSize, totalFailedObjectCount,
                                   totalFailedObjectSize, totalSkippedObjectCount,
                                   totalSkippedObjectSize, tasks);
    }

    public List<Integer> getPendingTaskIds(String sourceBucket, String destBucket) {
        List<Integer> taskIdList = getTaskIdList();
        if (taskIdList.isEmpty()) {
            return new ArrayList<>();
        }

        List<String> taskKeys = new ArrayList<>();
        for (Integer taskId : taskIdList) {
            taskKeys.add(TASK_KEY_PREFIX + taskId);
        }
        Map<String, SubTaskStatus> taskMap = rocksDBService.multiGet(taskKeys, SubTaskStatus.class);

        List<Integer> pendingTaskIds = new ArrayList<>();
        for (Integer taskId : taskIdList) {
            SubTaskStatus task = taskMap.get(TASK_KEY_PREFIX + taskId);
            if (task != null && task.getStatus() == SubTaskStatus.TaskStatus.PENDING) {
                boolean match = true;
                if (sourceBucket != null && !sourceBucket.isEmpty() && !sourceBucket.equals(task.getSourceBucket())) {
                    match = false;
                }
                if (destBucket != null && !destBucket.isEmpty() && !destBucket.equals(task.getDestBucket())) {
                    match = false;
                }
                if (match) {
                    pendingTaskIds.add(taskId);
                }
            }
        }

        return pendingTaskIds;
    }

    public List<Integer> getFailedTaskIds() {
        List<Integer> taskIdList = getTaskIdList();
        if (taskIdList.isEmpty()) {
            return new ArrayList<>();
        }

        List<String> taskKeys = new ArrayList<>();
        for (Integer taskId : taskIdList) {
            taskKeys.add(TASK_KEY_PREFIX + taskId);
        }
        Map<String, SubTaskStatus> taskMap = rocksDBService.multiGet(taskKeys, SubTaskStatus.class);

        List<Integer> failedTaskIds = new ArrayList<>();
        for (Integer taskId : taskIdList) {
            SubTaskStatus task = taskMap.get(TASK_KEY_PREFIX + taskId);
            if (task != null && (task.getStatus() == SubTaskStatus.TaskStatus.PARTIAL_FAILED
                               || task.getStatus() == SubTaskStatus.TaskStatus.FAILED)) {
                failedTaskIds.add(taskId);
            }
        }

        return failedTaskIds;
    }

    public boolean hasTasks() {
        List<Integer> taskIdList = getTaskIdList();
        return !taskIdList.isEmpty();
    }

    public boolean hasTasks(String sourceBucket, String destBucket) {
        List<Integer> taskIdList = getTaskIdList();
        if (taskIdList.isEmpty()) {
            return false;
        }

        List<String> taskKeys = new ArrayList<>();
        for (Integer taskId : taskIdList) {
            taskKeys.add(TASK_KEY_PREFIX + taskId);
        }
        Map<String, SubTaskStatus> taskMap = rocksDBService.multiGet(taskKeys, SubTaskStatus.class);

        for (Integer taskId : taskIdList) {
            SubTaskStatus task = taskMap.get(TASK_KEY_PREFIX + taskId);
            if (task != null) {
                boolean match = true;
                if (sourceBucket != null && !sourceBucket.isEmpty() && !sourceBucket.equals(task.getSourceBucket())) {
                    match = false;
                }
                if (destBucket != null && !destBucket.isEmpty() && !destBucket.equals(task.getDestBucket())) {
                    match = false;
                }
                if (match) {
                    return true;
                }
            }
        }
        return false;
    }

    public void clearTasks() {
        clearTasks(null, null, null, false);
    }

    public int clearTasks(String sourceBucket, String destBucket, List<Integer> taskIds) {
        return clearTasks(sourceBucket, destBucket, taskIds, false);
    }

    public synchronized int clearTasks(String sourceBucket, String destBucket, List<Integer> taskIds, boolean permanent) {
        // Keep registration of new I/O atomic with the idle check and persistent deletion fence.
        synchronized (localTaskControlService == null ? this : localTaskControlService) {
            return clearTasksWhileIdle(sourceBucket, destBucket, taskIds, permanent);
        }
    }

    private int clearTasksWhileIdle(String sourceBucket, String destBucket, List<Integer> taskIds, boolean permanent) {
        List<Integer> taskIdList = getTaskIdList();
        if (taskIdList.isEmpty()) {
            return 0;
        }

        boolean hasFilter = (taskIds != null && !taskIds.isEmpty()) ||
                           (sourceBucket != null && !sourceBucket.isEmpty()) ||
                           (destBucket != null && !destBucket.isEmpty());

        List<Integer> targetTaskIds;
        if (taskIds != null && !taskIds.isEmpty()) {
            targetTaskIds = taskIds;
        } else {
            targetTaskIds = taskIdList;
        }

        List<String> taskKeys = new ArrayList<>();
        for (Integer taskId : targetTaskIds) {
            taskKeys.add(TASK_KEY_PREFIX + taskId);
        }
        Map<String, SubTaskStatus> taskMap = rocksDBService.multiGet(taskKeys, SubTaskStatus.class);

        List<Integer> toDeleteTaskIds = new ArrayList<>();
        Map<String, Object> toPutDeleted = new HashMap<>();

        for (Integer taskId : targetTaskIds) {
            String key = TASK_KEY_PREFIX + taskId;
            SubTaskStatus task = taskMap.get(key);
            if (task == null) {
                continue;
            }

            boolean match = true;
            if (sourceBucket != null && !sourceBucket.isEmpty() && !sourceBucket.equals(task.getSourceBucket())) {
                match = false;
            }
            if (destBucket != null && !destBucket.isEmpty() && !destBucket.equals(task.getDestBucket())) {
                match = false;
            }
            if (!hasFilter) {
                if (task.getStatus() != SubTaskStatus.TaskStatus.COMPLETED) {
                    match = false;
                }
            }

            if (match) {
                if (task.getStatus() == SubTaskStatus.TaskStatus.RUNNING || task.getStatus() == SubTaskStatus.TaskStatus.LIST_DONE
                        || (localTaskControlService != null && localTaskControlService.isActive(taskId))) {
                    throw new IllegalStateException("请先停止任务 " + taskId + " 再删除");
                }
                toDeleteTaskIds.add(taskId);
                toPutDeleted.put(DELETED_TASK_KEY_PREFIX + taskId, task);
            }
        }

        if (toDeleteTaskIds.isEmpty()) {
            return 0;
        }

        List<String> deleteKeys = new ArrayList<>();
        for (Integer taskId : toDeleteTaskIds) {
            if (localTaskControlService != null) localTaskControlService.stop(taskId);
            bufferQueueManager.removeTaskMessages(taskId);
            deleteKeys.add(TASK_KEY_PREFIX + taskId);
        }

        @SuppressWarnings("unchecked")
        List<Integer> currentTaskIdList = rocksDBService.get(TASK_ID_LIST_KEY, List.class);
        if (currentTaskIdList == null) {
            currentTaskIdList = new ArrayList<>();
        }
        Set<Integer> toDeleteSet = new HashSet<>(toDeleteTaskIds);
        List<Integer> newTaskIdList = new ArrayList<>();
        for (Integer id : currentTaskIdList) {
            if (!toDeleteSet.contains(id)) {
                newTaskIdList.add(id);
            }
        }

        List<Integer> deletedIdList = new ArrayList<>(getDeletedTaskIdList());
        for (Integer id : toDeleteTaskIds) if (!deletedIdList.contains(id)) deletedIdList.add(id);
        toPutDeleted.put(TASK_ID_LIST_KEY, newTaskIdList);
        toPutDeleted.put(DELETED_TASK_ID_LIST_KEY, deletedIdList);
        rocksDBService.putBatchWithDeletes(toPutDeleted, deleteKeys);
        for (Integer id : toDeleteTaskIds) taskIdentityVersions.remove(id);
        if (permanent) permanentDeleteTasks(toDeleteTaskIds);

        if (!toDeleteTaskIds.isEmpty()) {
            taskProgressCache.clearTaskProgressBatch(toDeleteTaskIds);
        }

        int clearedCount = toDeleteTaskIds.size();
        if (taskIds != null && !taskIds.isEmpty()) {
            log.info("已{} {} 个指定任务", permanent ? "永久删除" : "标记删除", clearedCount);
        } else if (sourceBucket != null || destBucket != null) {
            log.info("已{} {} 个匹配的任务 (sourceBucket: {}, destBucket: {})", permanent ? "永久删除" : "标记删除", clearedCount, sourceBucket, destBucket);
        } else {
            log.info("已{} {} 个已完成任务", permanent ? "永久删除" : "标记删除", clearedCount);
        }

        return clearedCount;
    }

    private void moveTaskToDeleted(Integer taskId, SubTaskStatus task) {
        rocksDBService.put(DELETED_TASK_KEY_PREFIX + taskId, task);
        addToDeletedTaskIdList(taskId);
        rocksDBService.delete(TASK_KEY_PREFIX + taskId);
        removeFromTaskIdList(taskId);
    }

    private void addToDeletedTaskIdList(Integer taskId) {
        List<Integer> deletedIdList = getDeletedTaskIdList();
        if (!deletedIdList.contains(taskId)) {
            deletedIdList.add(taskId);
            rocksDBService.put(DELETED_TASK_ID_LIST_KEY, deletedIdList);
        }
    }

    private void removeFromDeletedTaskIdList(Integer taskId) {
        List<Integer> deletedIdList = getDeletedTaskIdList();
        deletedIdList.remove(Integer.valueOf(taskId));
        rocksDBService.put(DELETED_TASK_ID_LIST_KEY, deletedIdList);
    }

    public List<Integer> getDeletedTaskIdList() {
        List<Integer> idList = rocksDBService.get(DELETED_TASK_ID_LIST_KEY, List.class);
        if (idList == null) {
            idList = new ArrayList<>();
        }
        return idList;
    }

    public SubTaskStatus getDeletedTaskStatus(Integer taskId) {
        return rocksDBService.get(DELETED_TASK_KEY_PREFIX + taskId, SubTaskStatus.class);
    }

    public synchronized int permanentDeleteTasks(List<Integer> taskIds, List<Integer> parentTaskIds) {
        synchronized (localTaskControlService == null ? this : localTaskControlService) {
            Set<Integer> selectedIds = taskIds == null ? java.util.Collections.emptySet() : new HashSet<>(taskIds);
            Set<Integer> eligibleParents = new HashSet<>();
            if (parentTaskIds != null) for (Integer parentId : parentTaskIds) {
                if (parentId == null || parentId <= 0) throw new IllegalArgumentException("无效的父任务 ID");
                boolean known = rocksDBService.exists("local:discovery:" + parentId)
                        || rocksDBService.exists("local:parent:deleted:" + parentId)
                        || Boolean.TRUE.equals(rocksDBService.get("local:parent:purged:" + parentId, Boolean.class));
                for (Integer id : selectedIds) {
                    SubTaskStatus deleted = getDeletedTaskStatus(id);
                    if (deleted != null && parentId.equals(deleted.getParentTaskId())) known = true;
                }
                // Some cluster nodes never participated. Do not turn an unrecognized ID into a purge scope.
                if (!known) continue;
                for (SubTaskStatus retained : retainedTasks(selectedIds)) {
                    if (parentId.equals(retained.getParentTaskId()) || parentId.equals(retained.getTaskIndex()))
                        throw new IllegalStateException("父任务仍有未删除的子任务: " + parentId);
                }
                if (localTaskControlService != null && (!localTaskControlService.isStopped(parentId)
                        || localTaskControlService.isActive(parentId)))
                    throw new IllegalStateException("请先停止父任务 " + parentId);
                eligibleParents.add(parentId);
            }
            int deleted = permanentDeleteTasks(taskIds);
            for (Integer id : eligibleParents) purgeParentMetadata(id);
            return deleted;
        }
    }

    public synchronized int permanentDeleteTasks(List<Integer> taskIds) {
        synchronized (localTaskControlService == null ? this : localTaskControlService) {
            if (taskIds == null || taskIds.isEmpty()) return 0;
            Map<Integer, SubTaskStatus> selected = new LinkedHashMap<>();
            for (Integer id : new java.util.LinkedHashSet<>(taskIds)) {
                SubTaskStatus task = getDeletedTaskStatus(id);
                if (task == null) continue;
                if (localTaskControlService != null && localTaskControlService.isActive(id))
                    throw new IllegalStateException("请先停止任务 " + id + " 再删除");
                selected.put(id, task);
            }
            if (selected.isEmpty()) return 0;
            for (Integer id : selected.keySet()) {
                if (localTaskControlService != null) localTaskControlService.stop(id);
            }
            // Keep recycle-bin rows until all cleanup succeeds, so a failed cleanup can be retried.
            synchronized (rocksDBService) {
                for (Integer id : selected.keySet()) rocksDBService.putSync("task:purged:" + id, true);
                purgeFailureRecords(selected.keySet(), java.util.Collections.emptySet());
                purgeFallbackRecords(selected.keySet());
            }
            Set<Integer> reports = new HashSet<>();
            Set<String> manifests = new HashSet<>();
            Set<Integer> localParents = new HashSet<>();
            for (SubTaskStatus task : selected.values()) {
                reports.add(FileReportLedgerService.reportTaskId(task));
                if (task.getParentTaskId() != null) localParents.add(task.getParentTaskId());
                if (task.getInventoryManifestId() != null) manifests.add(task.getInventoryManifestId());
            }
            for (SubTaskStatus retained : retainedTasks(selected.keySet())) {
                reports.remove(FileReportLedgerService.reportTaskId(retained));
                manifests.remove(retained.getInventoryManifestId());
            }
            for (Integer id : reports) purgeReportMetadata(id, localParents.contains(id));
            for (String manifest : manifests) rocksDBService.deleteByPrefix("inventory:" + manifest + ":");
            List<Integer> remaining = new ArrayList<>(getDeletedTaskIdList());
            remaining.removeAll(selected.keySet());
            List<String> keys = new ArrayList<>();
            for (Integer id : selected.keySet()) keys.add(DELETED_TASK_KEY_PREFIX + id);
            rocksDBService.putBatchWithDeletes(java.util.Collections.singletonMap(DELETED_TASK_ID_LIST_KEY, remaining), keys);
            // Sync the final removal and preserve fences across a host restart.
            Map<String,Object> fences = new LinkedHashMap<>();
            for (Integer id : selected.keySet()) fences.put("task:purged:" + id, true);
            rocksDBService.putBatchSync(fences);
            for (Integer id : selected.keySet()) taskIdentityVersions.remove(id);
            if (taskProgressCache != null) taskProgressCache.clearTaskProgressBatch(new ArrayList<>(selected.keySet()));
            return selected.size();
        }
    }

    private List<SubTaskStatus> retainedTasks(Set<Integer> excluded) {
        List<SubTaskStatus> tasks = new ArrayList<>();
        for (Integer id : getTaskIdList()) {
            if (excluded.contains(id)) continue;
            SubTaskStatus task = rocksDBService.get(TASK_KEY_PREFIX + id, SubTaskStatus.class);
            if (task != null) tasks.add(task);
        }
        for (Integer id : getDeletedTaskIdList()) {
            if (excluded.contains(id)) continue;
            SubTaskStatus task = getDeletedTaskStatus(id);
            if (task != null) tasks.add(task);
        }
        return tasks;
    }

    /** Explicit parent cleanup also covers discovery nodes that never accepted a physical shard. */
    public synchronized void purgeParentMetadata(Integer parentId) {
        for (SubTaskStatus retained : retainedTasks(java.util.Collections.emptySet())) {
            if (parentId.equals(retained.getParentTaskId()) || parentId.equals(retained.getTaskIndex()))
                throw new IllegalStateException("任务仍有未删除的记录: " + parentId);
        }
        if (localTaskControlService != null && localTaskControlService.isActive(parentId))
            throw new IllegalStateException("任务仍在停止: " + parentId);
        purgeReportMetadata(parentId, true);
    }

    private void purgeReportMetadata(Integer reportId, boolean localParent) {
        if (localParent && localTaskControlService != null) localTaskControlService.stop(reportId);
        // Never acquire the ledger monitor while holding the DB monitor: ledger writes use that order.
        if (fileReportLedgerService != null) fileReportLedgerService.purge(reportId);
        synchronized (rocksDBService) {
            rocksDBService.putSync("report:purged:" + reportId, true);
            rocksDBService.deleteByPrefix("completed:" + reportId + ":");
            purgeFailureRecords(java.util.Collections.singleton(reportId), java.util.Collections.singleton(reportId));
            purgeFallbackRecords(java.util.Collections.singleton(reportId));
            if (localParent) {
                rocksDBService.delete("local:discovery:" + reportId);
                rocksDBService.deleteByPrefix(LOCAL_SHARD_ASSIGNMENT_PREFIX + reportId + ":");
                rocksDBService.deleteByPrefix("local:batch-assignment:" + reportId + ":");
                rocksDBService.putSync("local:parent:purged:" + reportId, true);
            }
        }
    }

    private void purgeFailureRecords(Set<Integer> tasks, Set<Integer> parents) {
        Long end = rocksDBService.get(FAILED_COUNT_KEY, Long.class);
        List<String> keys = new ArrayList<>();
        for (long index = 1; end != null && index <= end; index++) {
            String key = FAILED_KEY_PREFIX + index;
            FailedKey failure = rocksDBService.get(key, FailedKey.class);
            if (failure != null && (tasks.contains(failure.getTaskIndex()) || parents.contains(failure.getParentTaskId()))) {
                keys.add(key);
                if (keys.size() == 1000) { rocksDBService.deleteBatch(keys); keys.clear(); }
            }
        }
        rocksDBService.deleteBatch(keys);
    }

    private void purgeFallbackRecords(Set<Integer> tasks) {
        Long end = rocksDBService.get(FALLBACK_COUNT_KEY, Long.class);
        List<String> keys = new ArrayList<>();
        for (long index = 1; end != null && index <= end; index++) {
            String key = FALLBACK_KEY_PREFIX + index;
            FallbackObject fallback = rocksDBService.get(key, FallbackObject.class);
            if (fallback != null && tasks.contains(fallback.getTaskIndex())) {
                keys.add(key);
                if (keys.size() == 1000) { rocksDBService.deleteBatch(keys); keys.clear(); }
            }
        }
        // The task lifecycle only removes metadata; a referenced temporary file is never deleted here.
        rocksDBService.deleteBatch(keys);
    }

    public void recordFailedKey(Integer taskIndex, String key, String errorMessage, Long objectSize) {
        recordFailedKey(taskIndex, key, errorMessage, objectSize, FailureStage.WRITE_DESTINATION, "TRANSFER_FAILED");
    }

    public void recordFailedKey(Integer taskIndex, String key, String errorMessage, Long objectSize,
                                FailureStage stage, String errorCode) {
        recordFailedKey(taskIndex, key, errorMessage, objectSize, stage, errorCode, null);
    }

    public void recordFailedKey(Integer taskIndex, String key, String errorMessage, Long objectSize,
                                FailureStage stage, String errorCode, String itemLocalPath) {
        SubTaskStatus task = getTaskStatus(taskIndex);

        FailedKeyMessage message = new FailedKeyMessage();
        message.setTaskIndex(taskIndex);
        if (task != null) {
            message.setSourceBucket(task.getSourceBucket());
            message.setDestBucket(task.getDestBucket());
        }
        message.setKey(key);
        message.setDestinationKey(mappedDestinationKey(task, key));
        message.setObjectSize(objectSize);
        message.setErrorMessage(errorMessage);
        message.setStage(stage);
        message.setErrorCode(errorCode);
        message.setRetryCount(0);
        if (task != null) {
            message.setDirection(task.getDirection());
            message.setSourceEndpoint(task.getSourceEndpoint());
            message.setDestinationEndpoint(task.getDestinationEndpoint());
            message.setLocalPath(itemLocalPath == null ? task.getLocalPath() : itemLocalPath);
            message.setExecutionNodeIp(task.getExecutionNodeIp());
            message.setWorkerNode(task.getWorkerNode());
            message.setParentTaskId(task.getParentTaskId());
            message.setShardId(task.getShardId());
            message.setAttempt(task.getAttempt());
            message.setStorageId(task.getStorageId());
            message.setEligibleWorkerNodes(task.getEligibleWorkerNodes());
        }

        try {
            bufferQueueManager.putFailedKey(message);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            log.error("放入失败对象队列被中断: taskIndex={}, key={}", taskIndex, key);
        }

        log.error("记录迁移失败对象: taskIndex={}, sourceBucket={}, destBucket={}, key={}, objectSize={}",
                 taskIndex,
                 task != null ? task.getSourceBucket() : null,
                 task != null ? task.getDestBucket() : null,
                 key,
                 objectSize);
    }

    public void saveCompletedItem(Integer taskId, TransferDirection direction, String logicalKey, long size) {
        CompletedItemRecord record = new CompletedItemRecord(taskId, direction, logicalKey, size, System.currentTimeMillis());
        CompletableFuture<Void> durable = new CompletableFuture<>();
        boolean flushNow = false;
        synchronized (completionLock) {
            pendingCompletions.add(new PendingCompletion(completedKey(taskId, logicalKey), record, durable));
            int maxBatch = localTransferConfig == null ? 500 : Math.max(1, localTransferConfig.getCompletionWriteBatch());
            if (pendingCompletions.size() >= maxBatch) {
                flushNow = true;
            } else if (!completionFlushScheduled) {
                completionFlushScheduled = true;
                completionWriter.schedule(this::flushCompletedItems, 25, TimeUnit.MILLISECONDS);
            }
        }
        if (flushNow) flushCompletedItems();
        try {
            durable.get();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new RuntimeException("completion persistence interrupted", e);
        } catch (ExecutionException e) {
            throw new RuntimeException("completion persistence failed", e.getCause());
        }
    }

    private void flushCompletedItems() {
        List<PendingCompletion> pending;
        synchronized (completionLock) {
            if (pendingCompletions.isEmpty()) {
                completionFlushScheduled = false;
                return;
            }
            pending = new ArrayList<>(pendingCompletions);
            pendingCompletions.clear();
            completionFlushScheduled = false;
        }
        Map<String, Object> values = new LinkedHashMap<>();
        for (PendingCompletion item : pending) values.put(item.key, item.record);
        try {
            synchronized (rocksDBService) {
                values.entrySet().removeIf(entry -> Boolean.TRUE.equals(rocksDBService.get(
                        "report:purged:" + ((CompletedItemRecord) entry.getValue()).getTaskId(), Boolean.class)));
                rocksDBService.putBatchSync(values);
            }
            for (PendingCompletion item : pending) item.durable.complete(null);
        } catch (RuntimeException e) {
            for (PendingCompletion item : pending) item.durable.completeExceptionally(e);
        }
    }

    @PreDestroy
    public void shutdownCompletionWriter() {
        flushCompletedItems();
        completionWriter.shutdownNow();
    }

    private static class PendingCompletion {
        private final String key;
        private final CompletedItemRecord record;
        private final CompletableFuture<Void> durable;

        private PendingCompletion(String key, CompletedItemRecord record, CompletableFuture<Void> durable) {
            this.key = key;
            this.record = record;
            this.durable = durable;
        }
    }

    public boolean isItemCompleted(Integer taskId, String logicalKey) {
        return rocksDBService.exists(completedKey(taskId, logicalKey));
    }

    private String completedKey(Integer taskId, String logicalKey) {
        String encoded = Base64.getUrlEncoder().withoutPadding().encodeToString(
                logicalKey.getBytes(StandardCharsets.UTF_8));
        return COMPLETED_KEY_PREFIX + taskId + ":" + encoded;
    }

    public List<FailedKey> getFailedKeys() {
        return getFailedKeys(null, null, null, null);
    }

    public List<FailedKey> getFailedKeys(String sourceBucket, String destBucket) {
        return getFailedKeys(sourceBucket, destBucket, null, null);
    }

    public List<FailedKey> getFailedKeys(String sourceBucket, String destBucket, Integer taskIndex, Integer limit) {
        Long failedCount = rocksDBService.get(FAILED_COUNT_KEY, Long.class);
        List<FailedKey> failedKeys = new ArrayList<>();

        if (failedCount != null) {
            for (long i = 1; i <= failedCount; i++) {
                String key = FAILED_KEY_PREFIX + i;
                FailedKey failedKey = rocksDBService.get(key, FailedKey.class);
                if (failedKey != null) {
                    boolean match = true;
                    if (sourceBucket != null && !sourceBucket.isEmpty() && !sourceBucket.equals(failedKey.getSourceBucket())) {
                        match = false;
                    }
                    if (destBucket != null && !destBucket.isEmpty() && !destBucket.equals(failedKey.getDestBucket())) {
                        match = false;
                    }
                    if (taskIndex != null && !taskIndex.equals(failedKey.getTaskIndex())) {
                        match = false;
                    }
                    if (match) {
                        failedKeys.add(failedKey);
                        if (limit != null && limit > 0 && failedKeys.size() >= limit) {
                            break;
                        }
                    }
                }
            }
        }

        return failedKeys;
    }

    public Long getFailedCount() {
        return getFailedCount(null, null, null);
    }

    public Long getFailedCount(String sourceBucket, String destBucket) {
        return getFailedCount(sourceBucket, destBucket, null);
    }

    public Long getFailedCount(String sourceBucket, String destBucket, Integer taskIndex) {
        Long failedCount = rocksDBService.get(FAILED_COUNT_KEY, Long.class);
        if (failedCount == null) {
            return 0L;
        }

        long count = 0;
        for (long i = 1; i <= failedCount; i++) {
            String key = FAILED_KEY_PREFIX + i;
            FailedKey failedKey = rocksDBService.get(key, FailedKey.class);
            if (failedKey != null) {
                boolean match = true;
                if (sourceBucket != null && !sourceBucket.isEmpty() && !sourceBucket.equals(failedKey.getSourceBucket())) {
                    match = false;
                }
                if (destBucket != null && !destBucket.isEmpty() && !destBucket.equals(failedKey.getDestBucket())) {
                    match = false;
                }
                if (taskIndex != null && !taskIndex.equals(failedKey.getTaskIndex())) {
                    match = false;
                }
                if (match) {
                    count++;
                }
            }
        }

        return count;
    }

    /** Counts every match while retaining only the requested page, in one storage pass. */
    public static class ExceptionQuery<T> {
        private long count;
        private final List<T> items = new ArrayList<>();
        public long getCount() { return count; }
        public List<T> getItems() { return items; }
    }

    public ExceptionQuery<FailedKey> queryFailedKeys(String sourceBucket, String destBucket,
                                                     Integer taskIndex, Set<Integer> taskIds, Integer limit) {
        return queryExceptionRecords(FAILED_COUNT_KEY, FAILED_KEY_PREFIX, FailedKey.class, taskIds, limit,
                item -> matchesException(sourceBucket, destBucket, taskIndex, taskIds,
                        item.getSourceBucket(), item.getDestBucket(), item.getTaskIndex()));
    }

    public ExceptionQuery<FallbackObject> queryFallbackObjects(String sourceBucket, String destBucket,
                                                               Integer taskIndex, Set<Integer> taskIds, Integer limit) {
        return queryExceptionRecords(FALLBACK_COUNT_KEY, FALLBACK_KEY_PREFIX, FallbackObject.class, taskIds, limit,
                item -> matchesException(sourceBucket, destBucket, taskIndex, taskIds,
                        item.getSourceBucket(), item.getDestBucket(), item.getTaskIndex()));
    }

    private <T> ExceptionQuery<T> queryExceptionRecords(String countKey, String keyPrefix, Class<T> type,
                                                       Set<Integer> taskIds, Integer limit,
                                                       java.util.function.Predicate<T> matches) {
        ExceptionQuery<T> result = new ExceptionQuery<>();
        // An explicit empty group must never broaden the query to every task.
        if (taskIds != null && taskIds.isEmpty()) return result;
        Long storedCount = rocksDBService.get(countKey, Long.class);
        if (storedCount == null) return result;
        for (long i = 1; i <= storedCount; i++) {
            T item = rocksDBService.get(keyPrefix + i, type);
            if (item == null || !matches.test(item)) continue;
            result.count++;
            if (limit == null || limit <= 0 || result.items.size() < limit) result.items.add(item);
        }
        return result;
    }

    private boolean matchesException(String sourceBucket, String destBucket, Integer taskIndex,
                                     Set<Integer> taskIds, String source, String destination, Integer id) {
        return (sourceBucket == null || sourceBucket.isEmpty() || sourceBucket.equals(source))
                && (destBucket == null || destBucket.isEmpty() || destBucket.equals(destination))
                && (taskIndex == null || taskIndex.equals(id))
                && (taskIds == null || taskIds.contains(id));
    }

    public void clearFailedKeys() {
        Long failedCount = rocksDBService.get(FAILED_COUNT_KEY, Long.class);

        if (failedCount != null) {
            for (long i = 1; i <= failedCount; i++) {
                String key = FAILED_KEY_PREFIX + i;
                rocksDBService.delete(key);
            }
        }

        rocksDBService.delete(FAILED_COUNT_KEY);
        log.info("已清空所有失败对象记录");
    }

    public void recordFallbackObject(Integer taskIndex, String key, String tmpFilePath, String errorMessage) {
        SubTaskStatus task = getTaskStatus(taskIndex);

        FallbackMessage message = new FallbackMessage();
        message.setTaskIndex(taskIndex);
        if (task != null) {
            message.setSourceBucket(task.getSourceBucket());
            message.setDestBucket(task.getDestBucket());
        }
        message.setKey(key);
        message.setDestinationKey(mappedDestinationKey(task, key));

        try {
            bufferQueueManager.putFallback(message);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            log.error("放入降级对象队列被中断: taskIndex={}, key={}", taskIndex, key);
        }

        log.info("记录降级迁移对象: taskIndex={}, sourceBucket={}, destBucket={}, key={}",
                taskIndex,
                task != null ? task.getSourceBucket() : null,
                task != null ? task.getDestBucket() : null,
                key);
    }

    private String mappedDestinationKey(SubTaskStatus task, String sourceKey) {
        if (task == null || (task.getDirection() != null && task.getDirection() != TransferDirection.OSS_TO_OSS)) return null;
        try {
            return com.transfer.tools.util.OssKeyMapper.destinationKey(task.getKeyMapping(), sourceKey);
        } catch (RuntimeException invalidMapping) {
            // Preserve the source failure even when its destination cannot be represented.
            return null;
        }
    }

    public List<FallbackObject> getFallbackObjects() {
        return getFallbackObjects(null, null, null, null);
    }

    public List<FallbackObject> getFallbackObjects(String sourceBucket, String destBucket) {
        return getFallbackObjects(sourceBucket, destBucket, null, null);
    }

    public List<FallbackObject> getFallbackObjects(String sourceBucket, String destBucket, Integer taskIndex, Integer limit) {
        Long fallbackCount = rocksDBService.get(FALLBACK_COUNT_KEY, Long.class);
        List<FallbackObject> fallbackObjects = new ArrayList<>();

        if (fallbackCount != null) {
            for (long i = 1; i <= fallbackCount; i++) {
                String key = FALLBACK_KEY_PREFIX + i;
                FallbackObject fallbackObject = rocksDBService.get(key, FallbackObject.class);
                if (fallbackObject != null) {
                    boolean match = true;
                    if (sourceBucket != null && !sourceBucket.isEmpty() && !sourceBucket.equals(fallbackObject.getSourceBucket())) {
                        match = false;
                    }
                    if (destBucket != null && !destBucket.isEmpty() && !destBucket.equals(fallbackObject.getDestBucket())) {
                        match = false;
                    }
                    if (taskIndex != null && !taskIndex.equals(fallbackObject.getTaskIndex())) {
                        match = false;
                    }
                    if (match) {
                        fallbackObjects.add(fallbackObject);
                        if (limit != null && limit > 0 && fallbackObjects.size() >= limit) {
                            break;
                        }
                    }
                }
            }
        }

        return fallbackObjects;
    }

    public Long getFallbackCount() {
        return getFallbackCount(null, null, null);
    }

    public Long getFallbackCount(String sourceBucket, String destBucket) {
        return getFallbackCount(sourceBucket, destBucket, null);
    }

    public Long getFallbackCount(String sourceBucket, String destBucket, Integer taskIndex) {
        Long fallbackCount = rocksDBService.get(FALLBACK_COUNT_KEY, Long.class);
        if (fallbackCount == null) {
            return 0L;
        }

        long count = 0;
        for (long i = 1; i <= fallbackCount; i++) {
            String key = FALLBACK_KEY_PREFIX + i;
            FallbackObject fallbackObject = rocksDBService.get(key, FallbackObject.class);
            if (fallbackObject != null) {
                boolean match = true;
                if (sourceBucket != null && !sourceBucket.isEmpty() && !sourceBucket.equals(fallbackObject.getSourceBucket())) {
                    match = false;
                }
                if (destBucket != null && !destBucket.isEmpty() && !destBucket.equals(fallbackObject.getDestBucket())) {
                    match = false;
                }
                if (taskIndex != null && !taskIndex.equals(fallbackObject.getTaskIndex())) {
                    match = false;
                }
                if (match) {
                    count++;
                }
            }
        }

        return count;
    }

    public void clearFallbackObjects() {
        Long fallbackCount = rocksDBService.get(FALLBACK_COUNT_KEY, Long.class);

        if (fallbackCount != null) {
            for (long i = 1; i <= fallbackCount; i++) {
                String key = FALLBACK_KEY_PREFIX + i;
                FallbackObject fallbackObject = rocksDBService.get(key, FallbackObject.class);
                if (fallbackObject != null && fallbackObject.getTmpFilePath() != null) {
                    try {
                        java.io.File tmpFile = new java.io.File(fallbackObject.getTmpFilePath());
                        if (tmpFile.exists()) {
                            tmpFile.delete();
                        }
                    } catch (Exception e) {
                        log.warn("删除临时文件失败: {}", fallbackObject.getTmpFilePath(), e);
                    }
                }
                rocksDBService.delete(key);
            }
        }

        rocksDBService.delete(FALLBACK_COUNT_KEY);
        log.info("已清空所有降级迁移对象记录");
    }

    public synchronized void importTasks(List<TaskImport> taskImports) {
        if (taskImports == null || taskImports.isEmpty()) {
            log.warn("导入任务列表为空");
            return;
        }

        for (TaskImport task : taskImports) new EndpointNormalizationService().normalizeAndValidate(task);

        Integer startIndex = getNextTaskId();
        Integer endIndex = startIndex + taskImports.size() - 1;

        Map<String, Object> batchData = new HashMap<>();
        List<Integer> newTaskIds = new ArrayList<>();

        for (int i = 0; i < taskImports.size(); i++) {
            TaskImport taskImport = taskImports.get(i);
            Integer taskIndex = startIndex + i;
            taskImport.setTaskIndex(taskIndex);

            SubTaskStatus status = new SubTaskStatus();
            status.setTaskIndex(taskIndex);
            status.setSourceBucket(taskImport.getSourceBucket());
            status.setDestBucket(taskImport.getDestBucket());
            status.setSourceS3Alias(taskImport.getSourceS3Alias());
            status.setDestS3Alias(taskImport.getDestS3Alias());
            status.setStartKey(taskImport.getStartKey());
            status.setEndKey(taskImport.getEndKey());
            status.setMarker(taskImport.getMarker());
            status.setWorkerNode(taskImport.getWorkerNode());
            status.setDateAfter(taskImport.getDateAfter());
            status.setDateBefore(taskImport.getDateBefore());
            status.setOverwrite(taskImport.getOverwrite());
            copyTransferFields(taskImport, status);
            status.setStatus(SubTaskStatus.TaskStatus.PENDING);
            status.setListObjectCount(0L);
            status.setListObjectSize(0L);
            status.setPutObjectCount(0L);
            status.setPutObjectSize(0L);
            status.setFailedObjectCount(0L);
            status.setFailedObjectSize(0L);
            status.setSkippedObjectCount(0L);
            status.setSkippedObjectSize(0L);
            status.setCreateTime(System.currentTimeMillis());

            String key = TASK_KEY_PREFIX + taskIndex;
            fileReportLedgerService.registerTask(status);
            batchData.put(key, status);
            newTaskIds.add(taskIndex);
        }

        batchData.put(TASK_ID_SEQUENCE_KEY, endIndex);
        @SuppressWarnings("unchecked")
        List<Integer> taskIdList = rocksDBService.get(TASK_ID_LIST_KEY, List.class);
        if (taskIdList == null) {
            taskIdList = new ArrayList<>();
        }
        taskIdList.addAll(newTaskIds);
        batchData.put(TASK_ID_LIST_KEY, taskIdList);
        rocksDBService.putBatchSync(batchData);
        changedTaskIdentities(newTaskIds);

        log.info("已导入 {} 个任务，任务ID范围: {} - {}", taskImports.size(), startIndex, endIndex);
    }

    public synchronized void importTasksWithTaskId(List<TaskImport> taskImports) {
        synchronized (localTaskControlService == null ? this : localTaskControlService) {
            importAssignedTasksWhileControlled(taskImports);
        }
    }

    private void requireRetainedTask(Integer taskId, Integer parentTaskId) {
        if (Boolean.TRUE.equals(rocksDBService.get("task:purged:" + taskId, Boolean.class))
                || (parentTaskId != null && Boolean.TRUE.equals(rocksDBService.get(
                        "local:parent:purged:" + parentTaskId, Boolean.class))))
            throw new IllegalStateException("已永久删除的任务不能重新导入或继续传输");
    }

    private void importAssignedTasksWhileControlled(List<TaskImport> taskImports) {
        if (taskImports == null || taskImports.isEmpty()) {
            log.warn("导入任务列表为空");
            return;
        }

        Map<String, Object> batchData = new HashMap<>();
        List<Integer> newTaskIds = new ArrayList<>();

        // Validate the entire dispatch before writing even its first report/assignment.
        // Worker receives can be retried after the master already purged the task.
        for (TaskImport taskImport : taskImports) {
            new EndpointNormalizationService().normalizeAndValidate(taskImport);
            if (taskImport.getTaskIndex() == null) {
                log.warn("任务缺少taskIndex，跳过: {}", taskImport);
                continue;
            }

            Integer taskIndex = taskImport.getTaskIndex();
            requireRetainedTask(taskIndex, taskImport.getParentTaskId());
            String assignmentKey = null;
            if (taskImport.getDirection() != null && taskImport.getDirection() != TransferDirection.OSS_TO_OSS
                    && taskImport.getParentTaskId() != null && taskImport.getShardId() != null) {
                assignmentKey = localShardAssignmentKey(taskImport.getParentTaskId(), taskImport.getShardId());
                Integer assigned = rocksDBService.get(assignmentKey, Integer.class);
                if (assigned != null) taskIndex = assigned;
                taskIndex = findAvailableLocalTaskId(taskIndex, taskImport.getShardId());
                requireRetainedTask(taskIndex, taskImport.getParentTaskId());
                taskImport.setTaskIndex(taskIndex);
                batchData.put(assignmentKey, taskIndex);
            }
            SubTaskStatus retainedTask = getTaskStatus(taskIndex);
            if (retainedTask != null && !com.transfer.tools.util.OssKeyMapper.equivalent(
                    retainedTask.getKeyMapping(), taskImport.getKeyMapping()))
                throw new TransferValidationException("TASK_KEY_MAPPING_MISMATCH", "已有任务的对象命名规则不能被导入请求覆盖");
            if (retainedTask != null && retainedTask.getTaskBandwidthScopeId() != null
                    && !retainedTask.getTaskBandwidthScopeId().equals(taskImport.getTaskBandwidthScopeId()))
                throw new TransferValidationException("TASK_BANDWIDTH_SCOPE_MISMATCH", "已有任务的带宽范围不能被导入请求覆盖");
            if (retainedTask != null && (taskImport.getDirection() == null || taskImport.getDirection() == TransferDirection.OSS_TO_OSS)
                    && (!java.util.Objects.equals(retainedTask.getShardId(), taskImport.getShardId())
                        || (taskImport.getShardId() == null && !sameUnshardedOssTask(retainedTask, taskImport))))
                throw new TransferValidationException("TASK_ASSIGNMENT_MISMATCH", "已有任务编号不能用于其他迁移范围");
        }
        for (TaskImport taskImport : taskImports) {
            if (taskImport.getTaskIndex() == null) continue;
            Integer taskIndex = taskImport.getTaskIndex();
            SubTaskStatus existing = getTaskStatus(taskIndex);
            if (existing != null && java.util.Objects.equals(taskImport.getShardId(), existing.getShardId())) {
                newTaskIds.add(taskIndex);
                batchData.put(TASK_KEY_PREFIX + taskIndex, existing);
                log.info("忽略重复子任务导入: taskId={}, shardId={}", taskIndex, taskImport.getShardId());
                continue;
            }

            SubTaskStatus status = new SubTaskStatus();
            status.setTaskIndex(taskIndex);
            status.setSourceBucket(taskImport.getSourceBucket());
            status.setDestBucket(taskImport.getDestBucket());
            status.setSourceS3Alias(taskImport.getSourceS3Alias());
            status.setDestS3Alias(taskImport.getDestS3Alias());
            status.setStartKey(taskImport.getStartKey());
            status.setEndKey(taskImport.getEndKey());
            status.setMarker(taskImport.getMarker());
            status.setWorkerNode(taskImport.getWorkerNode());
            status.setDateAfter(taskImport.getDateAfter());
            status.setDateBefore(taskImport.getDateBefore());
            status.setOverwrite(taskImport.getOverwrite());
            copyTransferFields(taskImport, status);
            boolean stopped = localTaskControlService != null && (localTaskControlService.isStopped(taskIndex)
                    || localTaskControlService.isStopped(status.getParentTaskId()));
            status.setStatus(stopped ? SubTaskStatus.TaskStatus.STOPPED : SubTaskStatus.TaskStatus.PENDING);
            if (stopped) localTaskControlService.stop(taskIndex);
            status.setListObjectCount(0L);
            status.setListObjectSize(0L);
            status.setPutObjectCount(0L);
            status.setPutObjectSize(0L);
            status.setFailedObjectCount(0L);
            status.setFailedObjectSize(0L);
            status.setSkippedObjectCount(0L);
            status.setSkippedObjectSize(0L);
            status.setCreateTime(System.currentTimeMillis());

            String key = TASK_KEY_PREFIX + taskIndex;
            fileReportLedgerService.registerTask(status);
            batchData.put(key, status);
            newTaskIds.add(taskIndex);
        }

        if (batchData.isEmpty()) {
            log.warn("没有有效的任务可导入");
            return;
        }

        @SuppressWarnings("unchecked")
        List<Integer> taskIdList = rocksDBService.get(TASK_ID_LIST_KEY, List.class);
        if (taskIdList == null) {
            taskIdList = new ArrayList<>();
        }
        for (Integer taskId : newTaskIds) {
            if (!taskIdList.contains(taskId)) taskIdList.add(taskId);
        }
        batchData.put(TASK_ID_LIST_KEY, taskIdList);
        rocksDBService.putBatchSync(batchData);
        changedTaskIdentities(newTaskIds);

        if (!newTaskIds.isEmpty()) {
            Integer minTaskId = newTaskIds.get(0);
            Integer maxTaskId = newTaskIds.get(newTaskIds.size() - 1);
            Integer currentSequence = rocksDBService.get(TASK_ID_SEQUENCE_KEY, Integer.class);
            if (maxTaskId > 0 && (currentSequence == null || currentSequence < maxTaskId)) {
                rocksDBService.put(TASK_ID_SEQUENCE_KEY, maxTaskId);
            }
            log.info("已导入 {} 个任务，使用主节点分配的taskId范围: {} - {}", newTaskIds.size(), minTaskId, maxTaskId);
        }
    }

    private boolean sameUnshardedOssTask(SubTaskStatus saved, TaskImport incoming) {
        return saved.getDirection() == (incoming.getDirection() == null ? TransferDirection.OSS_TO_OSS : incoming.getDirection())
                && java.util.Objects.equals(saved.getSourceBucket(), incoming.getSourceBucket())
                && java.util.Objects.equals(saved.getDestBucket(), incoming.getDestBucket())
                && java.util.Objects.equals(saved.getSourceS3Alias(), incoming.getSourceS3Alias())
                && java.util.Objects.equals(saved.getDestS3Alias(), incoming.getDestS3Alias())
                && java.util.Objects.equals(saved.getStartKey(), incoming.getStartKey())
                && java.util.Objects.equals(saved.getEndKey(), incoming.getEndKey())
                && java.util.Objects.equals(saved.getObjectKey(), incoming.getObjectKey())
                && java.util.Objects.equals(ossFilter(saved.getSourceEndpoint()), ossFilter(incoming.getSourceEndpoint()))
                && java.util.Objects.equals(saved.getConflictPolicy(), incoming.getConflictPolicy() == null
                    ? Boolean.TRUE.equals(incoming.getOverwrite()) ? ConflictPolicy.OVERWRITE : ConflictPolicy.SKIP
                    : incoming.getConflictPolicy())
                && java.util.Objects.equals(saved.getDateAfter(), incoming.getDateAfter())
                && java.util.Objects.equals(saved.getDateBefore(), incoming.getDateBefore());
    }

    private String ossFilter(com.transfer.tools.model.TransferEndpoint endpoint) {
        return endpoint != null && Boolean.TRUE.equals(endpoint.getPrefixFilterEnabled())
                && endpoint.getPrefix() != null ? endpoint.getPrefix() : "";
    }

    public List<FailedKey> getFailedKeysByTaskIndex(Integer taskIndex) {
        Long failedCount = rocksDBService.get(FAILED_COUNT_KEY, Long.class);
        List<FailedKey> failedKeys = new ArrayList<>();

        if (failedCount != null) {
            for (long i = 1; i <= failedCount; i++) {
                String key = FAILED_KEY_PREFIX + i;
                FailedKey failedKey = rocksDBService.get(key, FailedKey.class);
                if (failedKey != null && taskIndex.equals(failedKey.getTaskIndex())) {
                    failedKeys.add(failedKey);
                }
            }
        }

        return failedKeys;
    }

    public void removeFailedKeysByTaskIndex(Integer taskIndex) {
        removeFailedKeysByTaskIndex(taskIndex, null);
    }

    public void removeFailedKeysByTaskIndex(Integer taskIndex, Set<String> selectedKeys) {
        Long failedCount = rocksDBService.get(FAILED_COUNT_KEY, Long.class);

        if (failedCount != null) {
            for (long i = 1; i <= failedCount; i++) {
                String key = FAILED_KEY_PREFIX + i;
                FailedKey failedKey = rocksDBService.get(key, FailedKey.class);
                if (failedKey != null && taskIndex.equals(failedKey.getTaskIndex())
                        && (selectedKeys == null || selectedKeys.contains(failedKey.getKey()))) {
                    rocksDBService.delete(key);
                }
            }
        }

        log.info("已删除任务 {} 的所有失败对象记录", taskIndex);
    }

    /** Stop is durable before cancellation; completed files remain available for resume. */
    public synchronized boolean stopTask(Integer taskIndex) {
        SubTaskStatus task = getTaskStatus(taskIndex);
        if (task == null) throw new IllegalArgumentException("任务不存在: " + taskIndex);
        if (task.getStatus() == SubTaskStatus.TaskStatus.COMPLETED) return false;
        localTaskControlService.stop(taskIndex);
        bufferQueueManager.removeTaskMessages(taskIndex);
        boolean changed = task.getStatus() != SubTaskStatus.TaskStatus.STOPPED;
        task.setStatus(SubTaskStatus.TaskStatus.STOPPED);
        if (changed || task.getEndTime() == null) task.setEndTime(System.currentTimeMillis());
        rocksDBService.putSync(TASK_KEY_PREFIX + taskIndex, task);
        return changed;
    }

    /** Re-enumerate the shard, retaining durable per-file results as the resume checkpoint. */
    public synchronized boolean resumeTask(Integer taskIndex) {
        requireRetainedTask(taskIndex, null);
        SubTaskStatus task = getTaskStatus(taskIndex);
        if (task == null) throw new IllegalArgumentException("任务不存在: " + taskIndex);
        requireRetainedTask(taskIndex, task.getParentTaskId());
        if (task.getStatus() == SubTaskStatus.TaskStatus.COMPLETED) return false;
        if (localTaskControlService.isActive(taskIndex)) {
            throw new IllegalStateException("任务正在停止，请等待当前读写退出");
        }
        if (task.getStatus() == SubTaskStatus.TaskStatus.PENDING && !localTaskControlService.isStopped(taskIndex)) return false;
        if ((task.getStatus() == SubTaskStatus.TaskStatus.RUNNING || task.getStatus() == SubTaskStatus.TaskStatus.LIST_DONE)
                && !localTaskControlService.isStopped(taskIndex)) {
            throw new IllegalStateException("任务仍在运行，请先停止");
        }
        prepareResume(task);
        return true;
    }

    public synchronized void recoverTask(SubTaskStatus task) {
        if (localTaskControlService.isStopped(task.getTaskIndex()) || task.getStatus() == SubTaskStatus.TaskStatus.STOPPED) return;
        prepareResume(task);
    }

    /** Startup prepares durable attempts before the executor schedules them with backpressure. */
    public synchronized void recoverAutomaticTask(SubTaskStatus task) {
        SubTaskStatus saved = getTaskStatus(task.getTaskIndex());
        if (saved == null || !Boolean.TRUE.equals(saved.getAutoStart())
                || localTaskControlService.isStopped(saved.getTaskIndex())
                || saved.getStatus() == SubTaskStatus.TaskStatus.STOPPED) return;
        prepareResume(saved, false);
    }

    private void prepareResume(SubTaskStatus task) { prepareResume(task, true); }

    private void prepareResume(SubTaskStatus task, boolean enqueue) {
        requireRetainedTask(task.getTaskIndex(), task.getParentTaskId());
        if (taskBandwidthGuard != null) taskBandwidthGuard.requireReady(task.getTaskBandwidthScopeId(), task.getTaskBandwidthLimitMbps());
        if (task.getInventoryManifestId() == null && (task.getObjectKey() == null || task.getObjectKey().isEmpty())
                && task.getEndKey() == null && task.getStartKey() != null && task.getStartKey().endsWith(".incr")) {
            throw new IllegalStateException("旧清单任务缺少可恢复的原始清单，请重新导入清单");
        }

        synchronized (localTaskControlService) {
            Integer taskIndex = task.getTaskIndex();
            if (localTaskControlService.isActive(taskIndex)) {
                throw new IllegalStateException("任务正在执行或停止，请等待当前读写退出");
            }
            bufferQueueManager.removeTaskMessages(taskIndex);
            taskProgressCache.clearTaskProgress(taskIndex);
            localTaskControlService.resume(taskIndex);
            task.setListObjectCount(0L); task.setListObjectSize(0L);
            task.setPutObjectCount(0L); task.setPutObjectSize(0L);
            task.setFailedObjectCount(0L); task.setFailedObjectSize(0L);
            task.setSkippedObjectCount(0L); task.setSkippedObjectSize(0L);
            task.setMarker(null); task.setScanComplete(false);
            task.setAttempt((task.getAttempt() == null ? 0 : task.getAttempt()) + 1);
            task.setStartTime(null); task.setEndTime(null);
            task.setStatus(SubTaskStatus.TaskStatus.PENDING);
            rocksDBService.putSync(TASK_KEY_PREFIX + taskIndex, task);
            if (!enqueue) return;
            try { bufferQueueManager.offerListTask(task); }
            catch (RuntimeException error) {
                localTaskControlService.stop(taskIndex);
                task.setStatus(SubTaskStatus.TaskStatus.STOPPED);
                rocksDBService.putSync(TASK_KEY_PREFIX + taskIndex, task);
                throw error;
            }
        }
    }

    public synchronized void retryTask(Integer taskIndex) throws Exception {
        SubTaskStatus task = getTaskStatus(taskIndex);
        if (task == null) throw new IllegalArgumentException("任务不存在: " + taskIndex);
        SubTaskStatus.TaskStatus status = task.getStatus();
        if (status != SubTaskStatus.TaskStatus.STOPPED && status != SubTaskStatus.TaskStatus.FAILED
                && status != SubTaskStatus.TaskStatus.PARTIAL_FAILED)
            throw new IllegalStateException("该任务不支持重试，当前状态: " + status);
        if (localTaskControlService.isActive(taskIndex))
            throw new IllegalStateException("任务正在执行或停止，请等待当前读写退出");
        // Preserve legacy failures in the report before the disposable exception cache is cleared.
        for (FailedKey failure : getFailedKeysByTaskIndex(taskIndex)) {
            fileReportLedgerService.failed(task, failure.getKey(), failure.getObjectSize(), failure.getLocalPath(),
                    failure.getErrorCode() == null ? "LEGACY_RETAINED_FAILURE" : failure.getErrorCode(), failure.getErrorMessage());
        }
        // Durable replay uses the original range or inventory manifest, while completed file
        // checkpoints prevent rewriting successes. Acceptance no longer depends on an async
        // producer that could fail after the API had already reported success.
        resumeTask(taskIndex);
        // List workers acquire this same monitor before executing the newly queued attempt.
        removeFailedKeysByTaskIndex(taskIndex);
    }
}
