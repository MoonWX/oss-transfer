package com.transfer.tools.service;

import com.transfer.tools.model.FailedKey;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import java.util.*;

@Service
public class LocalTransferQueryService {
    @Autowired
    private LocalTransferService localTransferService;
    @Autowired
    private SubTaskManagerService subTaskManagerService;

    public Map<String, Object> status(Integer taskId, TransferDirection direction,
                                      String executionNodeIp, String status, String endpoint) {
        return status(taskId, direction, executionNodeIp, status, endpoint, null);
    }

    public Map<String, Object> status(Integer taskId, TransferDirection direction,
                                      String executionNodeIp, String status, String endpoint, String storageId) {
        List<SubTaskStatus> items = new ArrayList<>();
        for (SubTaskStatus task : localTransferService.localTasks()) {
            if (taskId != null && !taskId.equals(task.getTaskIndex()) && !taskId.equals(task.getParentTaskId())) continue;
            if (direction != null && direction != task.getDirection()) continue;
            if (notBlank(executionNodeIp) && !executionNodeIp.equals(task.getExecutionNodeIp())) continue;
            if (notBlank(status) && !status.equalsIgnoreCase(task.getStatus().name())) continue;
            if (notBlank(endpoint) && !matchesEndpoint(task, endpoint)) continue;
            if (notBlank(storageId) && !storageId.equals(task.getStorageId())) continue;
            items.add(task);
        }
        Map<String, Object> data = aggregate(items);
        data.put("tasks", items);
        data.put("partial", false);
        data.put("unreachableWorkers", Collections.emptyList());
        return data;
    }

    public Map<String, Object> failures(Integer taskId, TransferDirection direction,
                                        String executionNodeIp, String endpoint,
                                        int offset, int limit) {
        return failures(taskId, direction, executionNodeIp, endpoint, null, offset, limit);
    }

    public Map<String, Object> failures(Integer taskId, TransferDirection direction,
                                        String executionNodeIp, String endpoint, String storageId,
                                        int offset, int limit) {
        int boundedOffset = Math.max(0, offset);
        int boundedLimit = Math.max(1, Math.min(1000, limit));
        List<FailedKey> matched = new ArrayList<>();
        for (FailedKey failure : localFailures()) {
            if (taskId != null && !taskId.equals(failure.getTaskIndex()) && !taskId.equals(failure.getParentTaskId())) continue;
            if (direction != null && direction != failure.getDirection()) continue;
            if (notBlank(executionNodeIp) && !executionNodeIp.equals(failure.getExecutionNodeIp())) continue;
            if (notBlank(endpoint) && !matchesEndpoint(failure, endpoint)) continue;
            if (notBlank(storageId) && !storageId.equals(failure.getStorageId())) continue;
            matched.add(failure);
        }
        int from = Math.min(boundedOffset, matched.size());
        int to = Math.min(from + boundedLimit, matched.size());
        Map<String, Object> data = new LinkedHashMap<>();
        data.put("total", matched.size());
        data.put("offset", boundedOffset);
        data.put("limit", boundedLimit);
        data.put("items", new ArrayList<>(matched.subList(from, to)));
        data.put("partial", false);
        data.put("unreachableWorkers", Collections.emptyList());
        return data;
    }

    public List<FailedKey> localFailures() {
        List<FailedKey> result = new ArrayList<>();
        for (FailedKey failure : subTaskManagerService.getFailedKeys()) {
            if (failure.getDirection() != null && failure.getDirection() != TransferDirection.OSS_TO_OSS) {
                result.add(failure);
            }
        }
        return result;
    }

    private Map<String, Object> aggregate(List<SubTaskStatus> tasks) {
        Map<String, Object> result = new LinkedHashMap<>();
        long scannedCount = 0, scannedBytes = 0, successCount = 0, successBytes = 0;
        long skippedCount = 0, skippedBytes = 0, failedCount = 0, failedBytes = 0;
        for (SubTaskStatus task : tasks) {
            scannedCount += value(task.getListObjectCount());
            scannedBytes += value(task.getListObjectSize());
            successCount += value(task.getPutObjectCount());
            successBytes += value(task.getPutObjectSize());
            skippedCount += value(task.getSkippedObjectCount());
            skippedBytes += value(task.getSkippedObjectSize());
            failedCount += value(task.getFailedObjectCount());
            failedBytes += value(task.getFailedObjectSize());
        }
        result.put("totalTasks", tasks.size());
        result.put("scannedCount", scannedCount);
        result.put("scannedBytes", scannedBytes);
        result.put("successCount", successCount);
        result.put("successBytes", successBytes);
        result.put("skippedCount", skippedCount);
        result.put("skippedBytes", skippedBytes);
        result.put("failedCount", failedCount);
        result.put("failedBytes", failedBytes);
        return result;
    }

    private boolean matchesEndpoint(SubTaskStatus task, String value) {
        return endpointContains(task.getSourceEndpoint(), value) || endpointContains(task.getDestinationEndpoint(), value);
    }

    private boolean matchesEndpoint(FailedKey failure, String value) {
        return endpointContains(failure.getSourceEndpoint(), value) || endpointContains(failure.getDestinationEndpoint(), value);
    }

    private boolean endpointContains(com.transfer.tools.model.TransferEndpoint endpoint, String value) {
        if (endpoint == null) return false;
        return value.equals(endpoint.getS3Alias()) || value.equals(endpoint.getBucket())
                || value.equals(endpoint.getExecutionNodeIp()) || value.equals(endpoint.getLocalPath())
                || value.equals(endpoint.getStorageId()) || bindingContains(endpoint, value);
    }

    private boolean bindingContains(com.transfer.tools.model.TransferEndpoint endpoint, String value) {
        if (endpoint.getNodeBindings() == null) return false;
        for (com.transfer.tools.model.LocalNodeBinding binding : endpoint.getNodeBindings()) {
            if (value.equals(binding.getExecutionNodeIp()) || value.equals(binding.getLocalPath())) return true;
        }
        return false;
    }

    private boolean notBlank(String value) {
        return value != null && !value.isEmpty();
    }

    private long value(Long value) {
        return value == null ? 0L : value;
    }
}
