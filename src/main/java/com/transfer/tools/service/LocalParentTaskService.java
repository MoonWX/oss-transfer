package com.transfer.tools.service;

import com.transfer.tools.model.LocalNodeBinding;
import com.transfer.tools.model.LocalExploreCheckpoint;
import com.transfer.tools.model.LocalParentTaskRecord;
import com.transfer.tools.model.LocalTransferRequest;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.model.TransferEndpoint;
import com.transfer.tools.storage.RocksDBService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

@Service
public class LocalParentTaskService {
    private static final String PARENT_KEY_PREFIX = "local:parent:";
    private static final String PARENT_ID_LIST_KEY = "local:parent:id:list";

    @Autowired private RocksDBService rocksDBService;
    @Autowired private SubTaskManagerService subTaskManagerService;

    public synchronized List<LocalParentTaskRecord> create(List<LocalTransferRequest> requests) {
        if (requests == null || requests.isEmpty()) return Collections.emptyList();
        int firstId = subTaskManagerService.allocateTaskIdRange(requests.size());
        List<Integer> ids = getParentIds();
        Map<String, Object> batch = new LinkedHashMap<>();
        List<LocalParentTaskRecord> records = new ArrayList<>();
        long now = System.currentTimeMillis();
        for (int i = 0; i < requests.size(); i++) {
            LocalTransferRequest request = requests.get(i);
            int parentId = firstId + i;
            request.setTaskId(parentId);
            request.setParentTaskId(parentId);
            request.setTaskBandwidthScopeId("local:" + parentId);
            LocalParentTaskRecord record = new LocalParentTaskRecord(parentId, request,
                    SubTaskStatus.TaskStatus.PENDING, null, null, false,
                    now, null, null, now);
            records.add(record);
            if (!ids.contains(parentId)) ids.add(parentId);
            batch.put(parentKey(parentId), record);
        }
        batch.put(PARENT_ID_LIST_KEY, ids);
        rocksDBService.putBatchSync(batch);
        return records;
    }

    public LocalParentTaskRecord get(Integer parentTaskId) {
        return parentTaskId == null ? null : rocksDBService.get(parentKey(parentTaskId), LocalParentTaskRecord.class);
    }

    public List<LocalParentTaskRecord> getAll() {
        List<LocalParentTaskRecord> result = new ArrayList<>();
        for (Integer id : getParentIds()) {
            LocalParentTaskRecord record = get(id);
            if (record != null && !isDeleted(id)) result.add(record);
        }
        return result;
    }

    public boolean isDeleted(Integer parentTaskId) {
        return Boolean.TRUE.equals(rocksDBService.get("local:parent:purged:" + parentTaskId, Boolean.class))
                || rocksDBService.get("local:parent:deleted:" + parentTaskId, SubTaskStatus.class) != null;
    }

    public List<SubTaskStatus> getDeleted() {
        List<SubTaskStatus> result = new ArrayList<>();
        for (Integer id : getParentIds()) {
            SubTaskStatus snapshot = rocksDBService.get("local:parent:deleted:" + id, SubTaskStatus.class);
            if (snapshot != null) result.add(snapshot);
        }
        return result;
    }

    public synchronized void softDelete(Integer parentTaskId, SubTaskStatus snapshot) {
        if (get(parentTaskId) == null || snapshot == null) return;
        rocksDBService.putBatchSync(Collections.singletonMap("local:parent:deleted:" + parentTaskId, snapshot));
    }

    public synchronized void restore(Integer parentTaskId) {
        rocksDBService.delete("local:parent:deleted:" + parentTaskId);
    }

    public synchronized void permanentDelete(Integer parentTaskId) {
        if (!isDeleted(parentTaskId)) return;
        subTaskManagerService.purgeParentMetadata(parentTaskId);
        List<Integer> ids = getParentIds();
        ids.remove(parentTaskId);
        Map<String, Object> puts = new LinkedHashMap<>();
        puts.put(PARENT_ID_LIST_KEY, ids);
        // Workers may retain discovery/checkpoint records. A permanent fence prevents
        // those records or stale queued work from recreating a deleted public task.
        puts.put("local:parent:purged:" + parentTaskId, true);
        rocksDBService.putBatchWithDeletes(puts, java.util.Arrays.asList(parentKey(parentTaskId),
                "local:parent:deleted:" + parentTaskId, "local:explore:" + parentTaskId));
    }

    public List<LocalParentTaskRecord> recoverable() {
        List<LocalParentTaskRecord> result = new ArrayList<>();
        for (LocalParentTaskRecord record : getAll()) {
            if (!Boolean.TRUE.equals(record.getOrchestrationComplete())
                    && (record.getStatus() == SubTaskStatus.TaskStatus.PENDING
                    || record.getStatus() == SubTaskStatus.TaskStatus.RUNNING)) {
                result.add(record);
            }
        }
        return result;
    }

    public synchronized boolean markRunning(Integer parentTaskId) {
        LocalParentTaskRecord record = get(parentTaskId);
        if (isStopped(parentTaskId)) return false;
        update(parentTaskId, SubTaskStatus.TaskStatus.RUNNING, null, null, false);
        return true;
    }

    public synchronized void markOrchestrationComplete(Integer parentTaskId) {
        LocalParentTaskRecord record = get(parentTaskId);
        if (isStopped(parentTaskId)) return;
        update(parentTaskId, SubTaskStatus.TaskStatus.RUNNING, null, null, true);
    }

    public synchronized void deferUnstartedRecovery(Integer parentTaskId) {
        LocalParentTaskRecord record = get(parentTaskId);
        if (record == null || isStopped(parentTaskId) || Boolean.TRUE.equals(record.getOrchestrationComplete())) return;
        record.setStatus(SubTaskStatus.TaskStatus.PENDING);
        record.setErrorCode("AGENT_OFFLINE"); record.setErrorMessage("等待执行节点连接");
        record.setStartTime(null); record.setEndTime(null); record.setUpdatedAt(System.currentTimeMillis());
        rocksDBService.putSync(parentKey(parentTaskId), record);
    }

    public synchronized void markFailed(Integer parentTaskId, String errorCode, String errorMessage) {
        LocalParentTaskRecord record = get(parentTaskId);
        if (isStopped(parentTaskId)) return;
        update(parentTaskId, SubTaskStatus.TaskStatus.FAILED, errorCode, errorMessage, false);
    }

    public synchronized void markStopped(Integer parentTaskId) {
        LocalParentTaskRecord record = get(parentTaskId);
        if (record == null) return;
        update(parentTaskId, SubTaskStatus.TaskStatus.STOPPED, null, null,
                Boolean.TRUE.equals(record.getOrchestrationComplete()));
    }

    public synchronized boolean isStopped(Integer parentTaskId) {
        LocalParentTaskRecord record = get(parentTaskId);
        return record == null || isDeleted(parentTaskId) || record.getStatus() == SubTaskStatus.TaskStatus.STOPPED;
    }

    /** Serialize dispatch against stop so no new shard can be sent after stop is recorded. */
    public synchronized boolean whileActive(Integer parentTaskId, Runnable action) {
        if (isStopped(parentTaskId)) return false;
        action.run();
        return true;
    }

    public LocalExploreCheckpoint exploreCheckpoint(Integer parentTaskId) {
        return rocksDBService.get("local:explore:" + parentTaskId, LocalExploreCheckpoint.class);
    }

    public void saveExploreCheckpoint(Integer parentTaskId, LocalExploreCheckpoint checkpoint) {
        rocksDBService.putBatchSync(Collections.singletonMap("local:explore:" + parentTaskId, checkpoint));
    }

    public synchronized void markResuming(Integer parentTaskId) {
        if (isDeleted(parentTaskId)) return;
        LocalParentTaskRecord record = get(parentTaskId);
        if (record == null) return;
        update(parentTaskId, SubTaskStatus.TaskStatus.RUNNING, null, null,
                Boolean.TRUE.equals(record.getOrchestrationComplete()));
    }

    public void markRetrying(Integer parentTaskId) {
        update(parentTaskId, SubTaskStatus.TaskStatus.PENDING, null, null, false);
    }

    public void markWorkerRetrying(Integer parentTaskId) {
        update(parentTaskId, SubTaskStatus.TaskStatus.RUNNING, null, null, true);
    }

    private synchronized void update(Integer parentTaskId, SubTaskStatus.TaskStatus status,
                                     String errorCode, String errorMessage, boolean orchestrationComplete) {
        LocalParentTaskRecord record = get(parentTaskId);
        if (record == null) return;
        long now = System.currentTimeMillis();
        record.setStatus(status);
        record.setErrorCode(errorCode);
        record.setErrorMessage(errorMessage);
        record.setOrchestrationComplete(orchestrationComplete);
        record.setUpdatedAt(now);
        if (status == SubTaskStatus.TaskStatus.RUNNING && record.getStartTime() == null) record.setStartTime(now);
        if (status == SubTaskStatus.TaskStatus.FAILED || status == SubTaskStatus.TaskStatus.STOPPED) {
            record.setEndTime(now);
        } else {
            record.setEndTime(null);
        }
        rocksDBService.putBatchSync(Collections.singletonMap(parentKey(parentTaskId), record));
    }

    public SubTaskStatus toStatus(LocalParentTaskRecord record) {
        LocalTransferRequest request = record.getRequest();
        SubTaskStatus status = new SubTaskStatus();
        status.setTaskIndex(record.getParentTaskId());
        status.setParentTaskId(record.getParentTaskId());
        status.setDirection(request.getDirection());
        status.setSourceEndpoint(request.getSourceEndpoint());
        status.setDestinationEndpoint(request.getDestinationEndpoint());
        status.setConflictPolicy(request.getConflictPolicy());
        status.setDistributionMode(request.getDistributionMode());
        status.setDateAfter(request.getDateAfter());
        status.setDateBefore(request.getDateBefore());
        status.setTargetRelativePath(request.getTargetRelativePath());
        status.setTaskBandwidthScopeId(request.getTaskBandwidthScopeId());
        status.setTaskBandwidthLimitMbps(request.getTaskBandwidthLimitMbps());
        TransferEndpoint source = request.getSourceEndpoint();
        TransferEndpoint destination = request.getDestinationEndpoint();
        if (source != null) {
            status.setSourceBucket(source.getBucket());
            status.setSourceS3Alias(source.getS3Alias());
            status.setObjectKey(source.getObjectKey());
        }
        if (destination != null) {
            status.setDestBucket(destination.getBucket());
            status.setDestS3Alias(destination.getS3Alias());
        }
        TransferEndpoint local = request.getDirection() == TransferDirection.LOCAL_TO_OSS ? source : destination;
        if (local != null) {
            status.setStorageId(local.getStorageId());
            status.setNodeBindings(local.getNodeBindings());
            List<String> nodes = new ArrayList<>();
            if (local.getNodeBindings() != null) {
                for (LocalNodeBinding binding : local.getNodeBindings()) nodes.add(binding.getExecutionNodeIp());
            }
            status.setEligibleWorkerNodes(nodes);
            if (local.getNodeBindings() != null && local.getNodeBindings().size() == 1) {
                status.setExecutionNodeIp(local.getNodeBindings().get(0).getExecutionNodeIp());
                status.setLocalPath(local.getNodeBindings().get(0).getLocalPath());
            }
        }
        status.setScanComplete(Boolean.TRUE.equals(record.getOrchestrationComplete()));
        status.setStatus(record.getStatus() == null ? SubTaskStatus.TaskStatus.PENDING : record.getStatus());
        status.setErrorCode(record.getErrorCode());
        status.setErrorMessage(record.getErrorMessage());
        status.setCreateTime(record.getCreateTime());
        status.setStartTime(record.getStartTime());
        status.setEndTime(record.getEndTime());
        status.setListObjectCount(0L); status.setListObjectSize(0L);
        status.setPutObjectCount(0L); status.setPutObjectSize(0L);
        status.setSkippedObjectCount(0L); status.setSkippedObjectSize(0L);
        status.setFailedObjectCount(0L);
        status.setFailedObjectSize(0L);
        status.setCurrentSpeedBytesPerSecond(0L); status.setAverageSpeedBytesPerSecond(0L);
        return status;
    }

    @SuppressWarnings("unchecked")
    private List<Integer> getParentIds() {
        List<Integer> ids = rocksDBService.get(PARENT_ID_LIST_KEY, List.class);
        return ids == null ? new ArrayList<>() : new ArrayList<>(ids);
    }

    private String parentKey(Integer parentTaskId) {
        return PARENT_KEY_PREFIX + parentTaskId;
    }
}
