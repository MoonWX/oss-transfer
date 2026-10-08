package com.transfer.tools.service;

import com.transfer.tools.model.FailedKey;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferEndpoint;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.springframework.stereotype.Service;

import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.zip.ZipEntry;
import java.util.zip.ZipOutputStream;

@Service
public class LocalReportService {
    public void write(OutputStream output, Collection<SubTaskStatus> tasks,
                      Collection<FailedKey> failures, Collection<String> unreachableWorkers) throws IOException {
        write(output, tasks, failures, unreachableWorkers, Collections.emptySet());
    }

    public void write(OutputStream output, Collection<SubTaskStatus> tasks,
                      Collection<FailedKey> failures, Collection<String> unreachableWorkers,
                      Collection<Integer> stoppedScopes) throws IOException {
        Map<Integer, ReportChildTaskIds> identifiers = new LinkedHashMap<>();
        Map<String, Integer> parentsByWorkerTask = new HashMap<>();
        for (SubTaskStatus task : tasks) {
            Integer parentId = parentId(task);
            if (parentId != null) {
                identifiers.computeIfAbsent(parentId,
                        id -> new ReportChildTaskIds(id, Collections.emptyList())).add(node(task), task);
                parentsByWorkerTask.put(node(task) + ":" + task.getTaskIndex(), parentId);
            }
        }
        for (ReportChildTaskIds ids : identifiers.values()) ids.build();
        try (ZipOutputStream zip = new ZipOutputStream(output, StandardCharsets.UTF_8)) {
            zip.putNextEntry(new ZipEntry("task-summary.csv"));
            write(zip, "rowType,parentTaskId,childTaskId,direction,sourceEndpoint,destinationEndpoint,storageId,distributionMode,eligibleWorkerNodes,participatingWorkerNodes,executionNodeIp,workerNode,conflictPolicy,status,currentSpeedBytesPerSecond,averageSpeedBytesPerSecond,scannedCount,scannedBytes,successCount,successBytes,skippedCount,skippedBytes,failedCount,failedBytes,createTime,startTime,endTime\n");
            for (SubTaskStatus task : tasks) {
                Integer parentId = parentId(task);
                boolean child = task.getParentTaskId() != null && !task.getParentTaskId().equals(task.getTaskIndex());
                ReportChildTaskIds ids = identifiers.get(parentId);
                String childId = child && ids != null ? ids.childId(node(task), task.getTaskIndex(), task.getShardId()) : null;
                writeRow(zip, child ? "CHILD_TASK" : "TASK", parentId, childId, task.getDirection(), endpoint(task.getSourceEndpoint()),
                        endpoint(task.getDestinationEndpoint()), task.getStorageId(), task.getDistributionMode(),
                        task.getEligibleWorkerNodes(), task.getParticipatingWorkerNodes(), task.getExecutionNodeIp(), task.getWorkerNode(),
                        task.getConflictPolicy(), task.getStatus(), task.getCurrentSpeedBytesPerSecond(),
                        task.getAverageSpeedBytesPerSecond(), task.getListObjectCount(), task.getListObjectSize(),
                        task.getPutObjectCount(), task.getPutObjectSize(), task.getSkippedObjectCount(),
                        task.getSkippedObjectSize(), task.getFailedObjectCount(), task.getFailedObjectSize(),
                        task.getCreateTime(), task.getStartTime(), task.getEndTime());
            }
            zip.closeEntry();
            zip.putNextEntry(new ZipEntry("exception-details.csv"));
            write(zip, "parentTaskId,childTaskId,direction,storageId,logicalKey,localPath,size,stage,errorCode,errorMessage,retryCount,executionNodeIp,workerNode,timestamp\n");
            for (FailedKey failure : failures) {
                Integer parentId = failure.getParentTaskId();
                String worker = failure.getWorkerNode() == null ? failure.getExecutionNodeIp() : failure.getWorkerNode();
                if (parentId == null) parentId = parentsByWorkerTask.get(worker + ":" + failure.getTaskIndex());
                if (parentId == null && failure.getTaskIndex() != null && failure.getTaskIndex() > 0) parentId = failure.getTaskIndex();
                ReportChildTaskIds ids = parentId == null ? null : identifiers.get(parentId);
                if (ids == null && parentId != null) {
                    ids = new ReportChildTaskIds(parentId, Collections.emptyList()); ids.build();
                }
                String childId = ids == null ? null : ids.childId(worker, failure.getTaskIndex(), failure.getShardId());
                writeRow(zip, parentId, childId, failure.getDirection(),
                        failure.getStorageId(), failure.getKey(),
                        failure.getLocalPath(), failure.getObjectSize(), failure.getStage(), failure.getErrorCode(),
                        failure.getErrorMessage(), failure.getRetryCount(), failure.getExecutionNodeIp(),
                        failure.getWorkerNode(), failure.getTimestamp());
            }
            for (String worker : unreachableWorkers == null ? Collections.<String>emptyList() : unreachableWorkers) {
                writeRow(zip, null, null, null, null, null, null, null, "DISPATCH", "WORKER_UNAVAILABLE",
                        "worker unavailable during report aggregation", 0, worker, worker, System.currentTimeMillis());
            }
            zip.closeEntry();
            Set<Integer> stoppedTasks = new LinkedHashSet<>(stoppedScopes);
            for (SubTaskStatus task : tasks) if (task.getStatus() == SubTaskStatus.TaskStatus.STOPPED) {
                stoppedTasks.add(task.getParentTaskId() == null ? task.getTaskIndex() : task.getParentTaskId());
            }
            List<String> reasons = new ArrayList<>();
            reasons.add("LEGACY_SUMMARY_ONLY");
            if (!stoppedTasks.isEmpty()) reasons.add("TASK_STOPPED");
            if (unreachableWorkers != null && !unreachableWorkers.isEmpty()) reasons.add("NODE_REPORT_INCOMPLETE");
            Map<String, Object> metadata = new LinkedHashMap<>();
            metadata.put("schemaVersion", 2);
            metadata.put("csvTaskIdentifiers", "parentTaskId is the public task ID; childTaskId uses parentTaskId and the one-based child number shown in the console. Parent and unassigned records have no childTaskId. Internal execution IDs are unchanged.");
            metadata.put("scope", "LEGACY_LOCAL_SUMMARY");
            metadata.put("complete", false);
            metadata.put("coverage", "LEGACY_OR_PARTIAL");
            metadata.put("stoppedSnapshot", !stoppedTasks.isEmpty());
            metadata.put("stoppedTaskIds", stoppedTasks);
            metadata.put("partialReasons", reasons);
            metadata.put("unreachableNodes", unreachableWorkers == null ? Collections.emptyList() : unreachableWorkers);
            metadata.put("completenessMeaning", "Legacy summary and retained exceptions only; use a single-task report for file-level coverage.");
            zip.putNextEntry(new ZipEntry("report-metadata.json"));
            zip.write(new ObjectMapper().writerWithDefaultPrettyPrinter().writeValueAsBytes(metadata));
            zip.closeEntry();
            zip.finish();
        }
    }

    private Integer parentId(SubTaskStatus task) {
        Integer id = task.getParentTaskId() == null ? task.getTaskIndex() : task.getParentTaskId();
        return id != null && id > 0 ? id : null;
    }

    private String node(SubTaskStatus task) {
        return task.getWorkerNode() == null ? task.getExecutionNodeIp() : task.getWorkerNode();
    }

    private String endpoint(TransferEndpoint endpoint) {
        if (endpoint == null || endpoint.getType() == null) return "";
        if (endpoint.getType() == com.transfer.tools.model.EndpointType.OSS) {
            return "OSS:" + endpoint.getS3Alias() + "/" + endpoint.getBucket();
        }
        if (endpoint.getNodeBindings() == null || endpoint.getNodeBindings().isEmpty()) {
            return "LOCAL:" + endpoint.getExecutionNodeIp() + ":" + endpoint.getLocalPath();
        }
        return "LOCAL:" + endpoint.getStorageId() + ":" + endpoint.getNodeBindings();
    }

    private void writeRow(OutputStream output, Object... values) throws IOException {
        StringBuilder row = new StringBuilder();
        for (int i = 0; i < values.length; i++) {
            if (i > 0) row.append(',');
            row.append(csv(values[i]));
        }
        row.append('\n');
        write(output, row.toString());
    }

    private String csv(Object value) {
        if (value == null) return "";
        String text = String.valueOf(value);
        if (!text.isEmpty() && "=+-@\t\r\n".indexOf(text.charAt(0)) >= 0) text = "'" + text;
        return "\"" + text.replace("\"", "\"\"") + "\"";
    }

    private void write(OutputStream output, String value) throws IOException {
        output.write(value.getBytes(StandardCharsets.UTF_8));
    }
}
