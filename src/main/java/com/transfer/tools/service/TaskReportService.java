package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import lombok.Getter;
import org.springframework.beans.BeanUtils;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpStatus;
import org.springframework.http.client.SimpleClientHttpRequestFactory;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;
import org.springframework.web.server.ResponseStatusException;

import java.io.*;
import java.net.URLEncoder;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.nio.file.*;
import java.util.*;
import java.util.zip.ZipEntry;
import java.util.zip.ZipOutputStream;

/** Streams a logical task report from its physical worker ledgers using bounded pages. */
@Service
public class TaskReportService {
    private static final int PAGE_SIZE = 500;
    private static final List<String> RESULTS = Arrays.asList("SUCCESS", "SKIPPED", "FAILED", "UNPROCESSED");
    private static final String FILE_HEADER = "parentTaskId,childTaskId,direction,logicalKey,sourceS3Alias,sourceBucket,sourceKey,sourcePath,destinationS3Alias,destinationBucket,destinationKey,destinationPath,size,node,result,reasonCode,errorMessage,discoveredAt,completedAt\r\n";
    private static final String ERROR_HEADER = "parentTaskId,childTaskId,direction,logicalKey,size,node,reasonCode,errorMessage,timestamp\r\n";
    @Autowired private DistributedConfig distributedConfig;
    @Autowired private LocalParentTaskService parentTasks;
    @Autowired private TaskCreationService taskCreations;
    private final ObjectMapper mapper = new ObjectMapper();
    private final RestTemplate restTemplate;

    public TaskReportService() {
        SimpleClientHttpRequestFactory factory = new SimpleClientHttpRequestFactory();
        factory.setConnectTimeout(3000);
        factory.setReadTimeout(60000);
        restTemplate = new RestTemplate(factory);
        restTemplate.getInterceptors().add((request, body, execution) -> {
            String token = distributedConfig.getInternalToken();
            if (token != null && !token.isEmpty()) request.getHeaders().set("X-Internal-Token", token);
            return execution.execute(request, body);
        });
    }

    /** Resolve before response headers so unknown tasks produce a normal 404. */
    public ReportScope resolveScope(Integer taskId) {
        if (taskId == null || taskId <= 0) throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "任务 ID 无效");
        TaskCreationService.Creation creation = taskCreations == null ? null : taskCreations.findByTaskId(taskId);
        ReportScope scope = new ReportScope(creation == null ? taskId : creation.getPublicTaskId());
        scope.creation = creation;
        scope.creationFingerprint = creationFingerprint(creation);
        if (creation != null && creation.getTaskIds() != null) scope.physicalTaskIds.addAll(creation.getTaskIds());
        if (scope.physicalTaskIds.isEmpty()) scope.physicalTaskIds.add(scope.taskId);
        scope.parent = creation == null ? parentTasks.get(taskId) : null;
        if (scope.parent != null) {
            LocalTransferRequest request = scope.parent.getRequest();
            TransferEndpoint local = request.getDirection() == TransferDirection.LOCAL_TO_OSS
                    ? request.getSourceEndpoint() : request.getDestinationEndpoint();
            if (local != null) {
                if (local.getNodeBindings() != null) for (LocalNodeBinding binding : local.getNodeBindings()) {
                    addNode(scope.requiredNodes, binding.getExecutionNodeIp());
                }
                addNode(scope.requiredNodes, local.getExecutionNodeIp());
            }
        }
        // Keep historical participants even after they are removed from configuration.
        if (creation != null && creation.getParticipatingWorkerNodes() != null) {
            for (String node : creation.getParticipatingWorkerNodes()) addNode(scope.requiredNodes, node);
        }
        // Older OSS tasks have no durable owner directory; probe all configured workers too.
        if ((creation != null || scope.requiredNodes.isEmpty()) && distributedConfig.getWorkerIps() != null) {
            for (String node : distributedConfig.getWorkerIps()) addNode(scope.requiredNodes, node);
        }
        for (String node : scope.requiredNodes) for (Integer physicalId : scope.physicalTaskIds) {
            ReportTarget target = new ReportTarget(node, physicalId);
            String key = creation == null ? node : node + "/" + physicalId;
            scope.targets.put(key, target);
            try {
                scope.initial.put(key, checkedTarget(fetchScope(node, physicalId), target, scope));
            } catch (RuntimeException error) {
                scope.unreachableNodes.add(node);
                scope.unreachableTargets.add(key);
            }
        }
        scope.missingTaskIds.addAll(missingTaskIds(scope, scope.initial));
        boolean found = scope.parent != null || scope.initial.values().stream().anyMatch(TaskReportWorkerSnapshot::isFound);
        if (!found) {
            if (!scope.unreachableNodes.isEmpty()) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE,
                    "执行节点不可达，无法确认任务是否存在");
            throw new ResponseStatusException(HttpStatus.NOT_FOUND, "任务不存在或已永久删除");
        }
        scope.resolvedAt = System.currentTimeMillis();
        scope.summary = summarize(scope, scope.initial, scope.parent, scope.creation);
        return scope;
    }

    /** Public downloads require completed or fully stopped execution on every node. */
    public ReportScope resolveExportableScope(Integer taskId) {
        ReportScope scope = resolveScope(taskId);
        if (!scope.unreachableNodes.isEmpty()) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE,
                "执行节点不可达，无法确认任务已结束或停止");
        if (!scope.missingTaskIds.isEmpty()) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE,
                "执行子任务记录不完整，无法导出整个任务报告");
        if (scope.creation != null && !creationEnded(scope.creation)) throw unfinished();
        if (!isExportableStatus(scope.summary.getStatus())) throw unfinished();
        boolean stopped = stoppedSnapshot(scope.summary, scope.initial);
        for (TaskReportWorkerSnapshot snapshot : scope.initial.values()) {
            if (snapshot.isScopeMissing()) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE,
                    "执行记录不完整，无法确认任务已结束或停止");
            if (Boolean.TRUE.equals(snapshot.getExecutionActive())) throw unfinished();
            if (stopped && snapshot.getExecutionActive() == null) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE,
                    "执行节点未提供活动状态，无法确认任务已完全停止；请同步升级节点");
            for (SubTaskStatus task : snapshot.getTasks()) {
                if (!isExportableStatus(task.getStatus())) throw unfinished();
            }
            String discovery = snapshot.getDiscoveryStatus();
            if (discovery != null && !"COMPLETED".equals(discovery) && !"FAILED".equals(discovery)
                    && !"STOPPED".equals(discovery)) throw unfinished();
            if (discovery == null && Boolean.FALSE.equals(snapshot.getDiscoveryComplete())) throw unfinished();
        }
        return scope;
    }

    public static boolean isExportableStatus(SubTaskStatus.TaskStatus status) {
        return status == SubTaskStatus.TaskStatus.COMPLETED || status == SubTaskStatus.TaskStatus.FAILED
                || status == SubTaskStatus.TaskStatus.PARTIAL_FAILED || status == SubTaskStatus.TaskStatus.STOPPED;
    }

    private static ResponseStatusException unfinished() {
        return new ResponseStatusException(HttpStatus.CONFLICT, "任务尚未结束或仍在停止中，请完全停止后再导出报告");
    }

    public void write(Integer taskId, OutputStream output) throws IOException {
        write(resolveScope(taskId), output);
    }

    public void write(ReportScope scope, OutputStream output) throws IOException {
        ExportState state = new ExportState(scope);
        ReportChildTaskIds childIds = childIds(scope);
        Path exceptions = Files.createTempFile("oss-task-report-", ".csv");
        try {
            try (BufferedWriter errors = Files.newBufferedWriter(exceptions, StandardCharsets.UTF_8);
                 ZipOutputStream zip = new ZipOutputStream(output, StandardCharsets.UTF_8)) {
                errors.write(ERROR_HEADER);
                writeSummary(zip, scope, childIds);
                zip.putNextEntry(new ZipEntry("file-results.csv"));
                text(zip, FILE_HEADER);
                PriorityQueue<NodeCursor> cursors = new PriorityQueue<>(Comparator.comparing(c -> key(c.current)));
                for (Map.Entry<String, TaskReportWorkerSnapshot> entry : scope.initial.entrySet()) {
                    if (!entry.getValue().isFound() && !entry.getValue().isScopeMissing()) continue;
                    NodeCursor cursor = new NodeCursor(scope.taskId, scope.targets.get(entry.getKey()), entry.getKey(), scope.creation != null, state);
                    if (cursor.advance()) cursors.add(cursor);
                }
                while (!cursors.isEmpty()) {
                    NodeCursor cursor = cursors.poll();
                    String logical = key(cursor.current);
                    FileReportRecord selected = cursor.current;
                    if (cursor.advance()) cursors.add(cursor);
                    while (!cursors.isEmpty() && logical.equals(key(cursors.peek().current))) {
                        NodeCursor duplicate = cursors.poll();
                        selected = finalRecord(selected, duplicate.current);
                        state.duplicateRows++;
                        if (duplicate.advance()) cursors.add(duplicate);
                    }
                    // Output failures are not worker failures: propagate an IOException
                    // immediately on disconnect instead of trying to finish another ZIP.
                    fileRow(zip, scope.taskId, childIds, selected);
                    state.count(selected);
                    if ("FAILED".equals(selected.getResult())) {
                        errors.write(row(scope.taskId, childIds.childId(selected.getNode(), selected.getTaskId(), selected.getShardId()),
                                selected.getDirection(), selected.getLogicalKey(), selected.getSize(), selected.getNode(),
                                selected.getReasonCode(), selected.getErrorMessage(), selected.getCompletedAt()));
                    }
                }
                zip.closeEntry();
                Map<String, TaskReportWorkerSnapshot> end = new LinkedHashMap<>();
                for (Map.Entry<String, ReportTarget> entry : scope.targets.entrySet()) {
                    String key = entry.getKey(); ReportTarget target = entry.getValue();
                    try {
                        TaskReportWorkerSnapshot snapshot = checkedTarget(fetchScope(target.node, target.taskId), target, scope);
                        end.put(key, snapshot);
                        if (snapshot.isScopeMissing()) state.scopeMissing(key);
                        TaskReportWorkerSnapshot start = scope.initial.get(key);
                        if (start == null || !fingerprint(start).equals(fingerprint(snapshot))) state.changedNodes.add(key);
                    } catch (RuntimeException error) {
                        state.fail(key, "WORKER_UNAVAILABLE");
                    }
                }
                TaskCreationService.Creation endCreation = scope.creation == null ? null : taskCreations.get(scope.creation.getId());
                boolean creationChanged = !scope.creationFingerprint.equals(creationFingerprint(endCreation));
                LocalParentTaskRecord endParent = scope.parent == null ? null : parentTasks.get(scope.taskId);
                boolean parentChanged = !parentFingerprint(scope.parent).equals(parentFingerprint(endParent));
                Map<String, Object> metadata = metadata(scope, state, end, endParent, parentChanged, endCreation, creationChanged);
                if (scope.parent != null && scope.parent.getErrorCode() != null) {
                    errors.write(row(scope.taskId, null,
                            scope.parent.getRequest().getDirection(), null, null, null,
                            scope.parent.getErrorCode(), scope.parent.getErrorMessage(), scope.parent.getUpdatedAt()));
                }
                for (Map.Entry<String, TaskReportWorkerSnapshot> entry : scope.initial.entrySet()) {
                    if (entry.getValue().getDiscoveryError() != null) errors.write(row(scope.taskId,
                            null, null, null, null, scope.targets.get(entry.getKey()).node, "SOURCE_DISCOVERY_FAILED",
                            entry.getValue().getDiscoveryError(), entry.getValue().getObservedAt()));
                }
                for (Map.Entry<String, String> issue : state.issues.entrySet()) {
                    ReportTarget target = scope.targets.get(issue.getKey());
                    errors.write(row(scope.taskId, null, null, null, null,
                            target == null ? issue.getKey() : target.node, issue.getValue(),
                            "Node report could not be read completely", System.currentTimeMillis()));
                }
                errors.flush();
                zip.putNextEntry(new ZipEntry("exception-details.csv"));
                Files.copy(exceptions, zip);
                zip.closeEntry();
                zip.putNextEntry(new ZipEntry("report-metadata.json"));
                zip.write(mapper.writerWithDefaultPrettyPrinter().writeValueAsBytes(metadata));
                zip.closeEntry();
                zip.finish();
            }
        } finally {
            Files.deleteIfExists(exceptions);
        }
    }

    protected TaskReportWorkerSnapshot fetchScope(String node, Integer taskId) {
        return read(node, "/api/worker/task-reports/" + taskId + "/scope", TaskReportWorkerSnapshot.class);
    }

    protected FileReportPage fetchPage(String node, Integer taskId, String cursor) {
        String path = "/api/worker/task-reports/" + taskId + "/files?limit=" + PAGE_SIZE;
        if (cursor != null) {
            try { path += "&cursor=" + URLEncoder.encode(cursor, "UTF-8"); }
            catch (UnsupportedEncodingException impossible) { throw new IllegalStateException(impossible); }
        }
        return read(node, path, FileReportPage.class);
    }

    private <T> T read(String node, String path, Class<T> type) {
        // fetchPage has already encoded the opaque cursor. The String overload
        // would escape '%' a second time before sending the request to Worker.
        Map response = restTemplate.getForObject(URI.create("http://" + node + ":" + distributedConfig.getPort() + path), Map.class);
        if (response == null || !Boolean.TRUE.equals(response.get("success")) || response.get("data") == null) {
            throw new IllegalStateException("invalid worker report response");
        }
        return mapper.convertValue(response.get("data"), type);
    }

    private TaskReportWorkerSnapshot checkedScope(TaskReportWorkerSnapshot snapshot, Integer taskId) {
        if (snapshot == null || !taskId.equals(snapshot.getReportTaskId()) || snapshot.getTasks() == null) {
            throw new IllegalStateException("invalid worker report scope");
        }
        return snapshot;
    }

    private Map<String, Object> metadata(ReportScope scope, ExportState state,
            Map<String, TaskReportWorkerSnapshot> end, LocalParentTaskRecord endParent, boolean parentChanged,
            TaskCreationService.Creation endCreation, boolean creationChanged) {
        Map<String, Object> result = new LinkedHashMap<>();
        SubTaskStatus endSummary = summarize(scope, end, endParent, endCreation);
        boolean running = activeSnapshot(scope.summary, scope.initial) || activeSnapshot(endSummary, end)
                || (endCreation != null && !creationEnded(endCreation));
        boolean stopped = stoppedSnapshot(scope.summary, scope.initial) || stoppedSnapshot(endSummary, end)
                || (endCreation != null && "STOPPED".equals(endCreation.getStatus()));
        boolean missingLocalParent = scope.parent == null && scope.summary.getDirection() != null
                && scope.summary.getDirection() != TransferDirection.OSS_TO_OSS;
        boolean scanComplete = !missingLocalParent && (scope.parent == null || Boolean.TRUE.equals(scope.parent.getOrchestrationComplete()));
        scanComplete &= endParent == null ? scope.parent == null : Boolean.TRUE.equals(endParent.getOrchestrationComplete());
        if (scope.creation != null) scanComplete &= Boolean.TRUE.equals(scope.creation.getScanComplete())
                && endCreation != null && Boolean.TRUE.equals(endCreation.getScanComplete());
        Set<Integer> missingIds = missingTaskIds(scope, end);
        missingIds.addAll(scope.missingTaskIds);
        boolean tracked = missingIds.isEmpty();
        int participatingNodes = 0;
        Map<String, Object> nodeCoverage = new LinkedHashMap<>();
        for (String node : scope.targets.keySet()) {
            TaskReportWorkerSnapshot before = scope.initial.get(node), after = end.get(node);
            if (before == null || after == null) { scanComplete = false; tracked = false; continue; }
            Map<String, Object> coverage = new LinkedHashMap<>();
            coverage.put("node", scope.targets.get(node).node);
            coverage.put("taskId", scope.targets.get(node).taskId);
            coverage.put("start", before.getCoverage()); coverage.put("end", after.getCoverage());
            nodeCoverage.put(node, coverage);
            if (before.isFound() || after.isFound() || before.isScopeMissing() || after.isScopeMissing()) {
                participatingNodes++;
                ReportCoverage first = before.getCoverage(), last = after.getCoverage();
                tracked &= !before.isScopeMissing() && !after.isScopeMissing()
                        && first != null && last != null && "TRACKED".equals(first.getLedgerCoverage())
                        && "TRACKED".equals(last.getLedgerCoverage());
                scanComplete &= first != null && last != null && first.isScanComplete() && last.isScanComplete();
                if (Boolean.FALSE.equals(before.getDiscoveryComplete()) || Boolean.FALSE.equals(after.getDiscoveryComplete())) scanComplete = false;
            }
        }
        scanComplete &= participatingNodes > 0 && state.issues.isEmpty() && missingIds.isEmpty();
        boolean countsMatch = countsMatch(scope.summary, state) && countsMatch(endSummary, state);
        boolean stable = !running && !parentChanged && !creationChanged && missingIds.isEmpty()
                && state.changedNodes.isEmpty() && state.issues.isEmpty();
        boolean complete = !stopped && tracked && scanComplete && stable && countsMatch && state.issues.isEmpty();
        List<String> reasons = new ArrayList<>();
        if (stopped) reasons.add("TASK_STOPPED");
        if (!tracked) reasons.add("HISTORICAL_OR_PARTIAL_LEDGER");
        if (missingLocalParent) reasons.add("LOCAL_PARENT_SCOPE_UNKNOWN");
        if (!scanComplete) reasons.add("SCAN_NOT_CONFIRMED_COMPLETE");
        if (!state.issues.isEmpty()) reasons.add("NODE_REPORT_INCOMPLETE");
        if (!state.scopeMissingNodes.isEmpty()) reasons.add("NODE_SCOPE_MISSING");
        if (running) reasons.add("RUNNING_SNAPSHOT");
        if (parentChanged || creationChanged || !state.changedNodes.isEmpty()) reasons.add("STATE_CHANGED_DURING_EXPORT");
        if (!missingIds.isEmpty()) reasons.add("TASK_SHARD_MISSING");
        if (!countsMatch) reasons.add("COUNTS_DO_NOT_RECONCILE");
        if (state.unknownFileSizeCount > 0) reasons.add("FILE_SIZE_UNKNOWN");
        result.put("schemaVersion", 2);
        Map<String, Object> identifiers = new LinkedHashMap<>();
        identifiers.put("parentTaskId", "Public task ID shared by every CSV row in this report.");
        identifiers.put("childTaskId", "Display ID in parentTaskId-sequence form, matching task details. Empty for task summaries, discovery/orchestration, node issues, or records with no retained child identity.");
        identifiers.put("diagnosticIds", "Internal task IDs in this JSON are diagnostic identifiers, not CSV child IDs.");
        result.put("identifiers", identifiers);
        result.put("taskId", scope.taskId);
        result.put("scope", scope.creation != null ? "OSS_CREATION_WITH_ALL_SHARDS" : scope.parent == null ? "TASK" : "LOCAL_PARENT_WITH_ALL_SHARDS");
        result.put("physicalTaskIds", scope.physicalTaskIds);
        result.put("missingTaskIds", missingIds);
        if (scope.creation != null) {
            result.put("creationId", scope.creation.getId());
            result.put("creationStatus", scope.creation.getStatus());
            result.put("creationStatusAtEnd", endCreation == null ? null : endCreation.getStatus());
        }
        result.put("scopeResolvedAt", scope.resolvedAt);
        result.put("generatedAt", System.currentTimeMillis());
        result.put("complete", complete);
        result.put("coverage", complete ? "COMPLETE" : tracked ? "PARTIAL" : "LEGACY_OR_PARTIAL");
        result.put("scanComplete", scanComplete);
        result.put("runningSnapshot", running);
        result.put("stoppedSnapshot", stopped);
        result.put("stableSnapshot", stable);
        result.put("countsReconciled", countsMatch);
        result.put("requiredNodes", scope.requiredNodes);
        Set<String> responded = new LinkedHashSet<>(scope.initial.keySet());
        responded.retainAll(end.keySet()); responded.removeAll(state.issues.keySet());
        result.put("respondedNodes", targetNodes(scope, responded));
        Set<String> unreadable = new LinkedHashSet<>();
        for (Map.Entry<String, String> issue : state.issues.entrySet()) if (!"NODE_SCOPE_MISSING".equals(issue.getValue())) unreadable.add(issue.getKey());
        result.put("unreachableNodes", targetNodes(scope, unreadable));
        result.put("scopeMissingNodes", targetNodes(scope, state.scopeMissingNodes));
        result.put("changedNodes", targetNodes(scope, state.changedNodes));
        result.put("nodeIssues", state.issues);
        result.put("partialReasons", reasons);
        result.put("fileRows", state.rows);
        result.put("fileBytes", state.bytes);
        result.put("unknownFileSizeCount", state.unknownFileSizeCount);
        result.put("fileBytesMeaning", "Sum of known file sizes; excludes records with unknown size.");
        result.put("resultCounts", state.counts);
        result.put("resultBytes", state.resultBytes);
        result.put("unprocessedCount", state.counts.get("UNPROCESSED"));
        result.put("duplicateRowsMerged", state.duplicateRows);
        result.put("statusSnapshot", statistics(scope.summary));
        result.put("statusSnapshotAtEnd", statistics(endSummary));
        result.put("nodeCoverage", nodeCoverage);
        result.put("contentVerified", false);
        result.put("csvStringEscaping", "APOSTROPHE_PREFIX_V1: string values beginning with apostrophe, =, +, -, @, TAB or CR gain one leading apostrophe. After CSV parsing, remove exactly one leading apostrophe from string columns to restore the original value.");
        result.put("completenessMeaning", "Complete covers the task's discovered items, not an independent inventory or checksum verification.");
        result.put("historicalLimitations", "Retained historical successes/failures may be included; missing skips and deleted failure history cannot be reconstructed.");
        return result;
    }

    private boolean countsMatch(SubTaskStatus summary, ExportState state) {
        long accounted = value(summary.getPutObjectCount()) + value(summary.getSkippedObjectCount()) + value(summary.getFailedObjectCount());
        return state.unknownFileSizeCount == 0 && state.rows == value(summary.getListObjectCount()) && state.bytes == value(summary.getListObjectSize())
                && state.counts.get("SUCCESS") == value(summary.getPutObjectCount())
                && state.counts.get("SKIPPED") == value(summary.getSkippedObjectCount())
                && state.counts.get("FAILED") == value(summary.getFailedObjectCount())
                && state.counts.get("UNPROCESSED") == Math.max(0, value(summary.getListObjectCount()) - accounted)
                && state.resultBytes.get("SUCCESS") == value(summary.getPutObjectSize())
                && state.resultBytes.get("SKIPPED") == value(summary.getSkippedObjectSize())
                && state.resultBytes.get("FAILED") == value(summary.getFailedObjectSize());
    }

    private SubTaskStatus summarize(ReportScope scope, Map<String, TaskReportWorkerSnapshot> snapshots,
            LocalParentTaskRecord parent, TaskCreationService.Creation creation) {
        SubTaskStatus result = parent == null ? new SubTaskStatus() : parentTasks.toStatus(parent);
        if (result == null) result = new SubTaskStatus();
        List<SubTaskStatus> shards = new ArrayList<>();
        for (TaskReportWorkerSnapshot snapshot : snapshots.values()) shards.addAll(snapshot.getTasks());
        if (parent == null && !shards.isEmpty()) BeanUtils.copyProperties(shards.get(0), result);
        result.setTaskIndex(scope.taskId); result.setLocalFileEntries(null);
        long listed = 0, listedBytes = 0, success = 0, successBytes = 0, skipped = 0, skippedBytes = 0, failed = 0, failedBytes = 0;
        Long start = null, end = null;
        SubTaskStatus.TaskStatus status = SubTaskStatus.TaskStatus.COMPLETED;
        for (SubTaskStatus shard : shards) {
            listed += value(shard.getListObjectCount()); listedBytes += value(shard.getListObjectSize());
            success += value(shard.getPutObjectCount()); successBytes += value(shard.getPutObjectSize());
            skipped += value(shard.getSkippedObjectCount()); skippedBytes += value(shard.getSkippedObjectSize());
            failed += value(shard.getFailedObjectCount()); failedBytes += value(shard.getFailedObjectSize());
            if (shard.getStartTime() != null) start = start == null ? shard.getStartTime() : Math.min(start, shard.getStartTime());
            if (shard.getEndTime() != null) end = end == null ? shard.getEndTime() : Math.max(end, shard.getEndTime());
            if (rank(shard.getStatus()) > rank(status)) status = shard.getStatus();
        }
        if (parent != null && !Boolean.TRUE.equals(parent.getOrchestrationComplete())) status = parent.getStatus();
        else if (shards.isEmpty()) status = result.getStatus();
        else if (parent != null && parent.getStatus() == SubTaskStatus.TaskStatus.STOPPED) status = parent.getStatus();
        else if (parent != null && parent.getStatus() == SubTaskStatus.TaskStatus.FAILED) status = success > 0 || skipped > 0
                || status == SubTaskStatus.TaskStatus.COMPLETED ? SubTaskStatus.TaskStatus.PARTIAL_FAILED : SubTaskStatus.TaskStatus.FAILED;
        if (creation != null) {
            result.setDirection(TransferDirection.OSS_TO_OSS); result.setParentTaskId(null); result.setShardId(null);
            result.setStartKey(null); result.setEndKey(null);
            if (!creationEnded(creation)) status = SubTaskStatus.TaskStatus.RUNNING;
            else if (!active(status) && "STOPPED".equals(creation.getStatus())) status = SubTaskStatus.TaskStatus.STOPPED;
            else if (!active(status) && "FAILED".equals(creation.getStatus())) status = success > 0 || skipped > 0
                    ? SubTaskStatus.TaskStatus.PARTIAL_FAILED : SubTaskStatus.TaskStatus.FAILED;
            if (creation.getRequest() != null) {
                BucketPair request = creation.getRequest();
                result.setSourceS3Alias(request.getSourceS3Alias()); result.setSourceBucket(request.getSourceBucket());
                result.setDestS3Alias(request.getDestS3Alias()); result.setDestBucket(request.getDestBucket());
                if (request.getSourceEndpoint() != null) result.setSourceEndpoint(request.getSourceEndpoint());
                if (request.getDestinationEndpoint() != null) result.setDestinationEndpoint(request.getDestinationEndpoint());
                result.setDateAfter(request.getDateAfter()); result.setDateBefore(request.getDateBefore());
                result.setKeyMapping(request.getKeyMapping()); result.setObjectKey(request.getObjectKey());
            }
        }
        result.setStatus(status);
        if (start != null) result.setStartTime(start);
        if (active(status) || (parent != null && !Boolean.TRUE.equals(parent.getOrchestrationComplete()))) result.setEndTime(null);
        else if (end != null) result.setEndTime(end);
        result.setListObjectCount(listed); result.setListObjectSize(listedBytes);
        result.setPutObjectCount(success); result.setPutObjectSize(successBytes);
        result.setSkippedObjectCount(skipped); result.setSkippedObjectSize(skippedBytes);
        result.setFailedObjectCount(failed); result.setFailedObjectSize(failedBytes);
        return result;
    }

    private ReportChildTaskIds childIds(ReportScope scope) {
        ReportChildTaskIds ids = new ReportChildTaskIds(scope.taskId,
                scope.creation == null ? Collections.emptyList() : scope.creation.getTaskIds());
        for (Map.Entry<String, TaskReportWorkerSnapshot> entry : scope.initial.entrySet()) {
            String node = scope.targets.get(entry.getKey()).node;
            for (SubTaskStatus task : entry.getValue().getTasks()) ids.add(node, task);
        }
        ids.build();
        return ids;
    }

    private void writeSummary(ZipOutputStream zip, ReportScope scope, ReportChildTaskIds childIds) throws IOException {
        zip.putNextEntry(new ZipEntry("task-summary.csv"));
        text(zip, "rowType,parentTaskId,childTaskId,direction,node,status,sourceEndpoint,destinationEndpoint,sourcePrefix,startKey,endKey,objectKey,targetRelativePath,dateAfter,dateBefore,overwrite,conflictPolicy,storageId,distributionMode,scannedCount,scannedBytes,successCount,successBytes,skippedCount,skippedBytes,failedCount,failedBytes,createTime,startTime,endTime,keyMappingMode,keyMappingSourcePrefix,keyMappingTargetPrefix,keyMappingTargetKey\r\n");
        summaryRow(zip, "TASK", scope, scope.summary, null, null);
        for (Map.Entry<String, TaskReportWorkerSnapshot> entry : scope.initial.entrySet()) {
            String node = scope.targets.get(entry.getKey()).node;
            for (SubTaskStatus task : entry.getValue().getTasks()) summaryRow(zip, "CHILD_TASK", scope, task, node,
                    childIds.childId(node, task.getTaskIndex(), task.getShardId()));
        }
        zip.closeEntry();
    }

    private void summaryRow(OutputStream out, String rowType, ReportScope scope, SubTaskStatus task, String node, String childId) throws IOException {
        text(out, row(rowType, scope.taskId, childId, task.getDirection(), node,
                task.getStatus(), endpoint(task, true), endpoint(task, false), task.getSourceEndpoint() == null ? null : task.getSourceEndpoint().getPrefix(),
                task.getStartKey(), task.getEndKey(), task.getObjectKey(), task.getTargetRelativePath(), task.getDateAfter(), task.getDateBefore(), task.getOverwrite(),
                task.getConflictPolicy(), task.getStorageId(), task.getDistributionMode(), task.getListObjectCount(), task.getListObjectSize(), task.getPutObjectCount(),
                task.getPutObjectSize(), task.getSkippedObjectCount(), task.getSkippedObjectSize(), task.getFailedObjectCount(), task.getFailedObjectSize(),
                task.getCreateTime(), task.getStartTime(), task.getEndTime(),
                task.getKeyMapping() == null ? null : task.getKeyMapping().getMode(),
                task.getKeyMapping() == null ? null : task.getKeyMapping().getSourcePrefix(),
                task.getKeyMapping() == null ? null : task.getKeyMapping().getTargetPrefix(),
                task.getKeyMapping() == null ? null : task.getKeyMapping().getTargetKey()));
    }

    private String endpoint(SubTaskStatus task, boolean source) throws IOException {
        TransferEndpoint endpoint = source ? task.getSourceEndpoint() : task.getDestinationEndpoint();
        if (endpoint != null) return mapper.writeValueAsString(endpoint);
        return "OSS:" + (source ? task.getSourceS3Alias() + "/" + task.getSourceBucket() : task.getDestS3Alias() + "/" + task.getDestBucket());
    }

    private static void fileRow(OutputStream out, Integer parentId, ReportChildTaskIds childIds, FileReportRecord r) throws IOException {
        text(out, row(parentId, childIds.childId(r.getNode(), r.getTaskId(), r.getShardId()), r.getDirection(), r.getLogicalKey(),
                r.getSourceS3Alias(), r.getSourceBucket(), r.getSourceKey(), r.getSourcePath(), r.getDestinationS3Alias(),
                r.getDestinationBucket(), r.getDestinationKey(), r.getDestinationPath(), r.getSize(), r.getNode(), r.getResult(),
                r.getReasonCode(), r.getErrorMessage(), r.getDiscoveredAt(), r.getCompletedAt()));
    }

    static String row(Object... values) {
        StringBuilder result = new StringBuilder();
        for (int i = 0; i < values.length; i++) {
            if (i > 0) result.append(',');
            String value = values[i] == null ? "" : String.valueOf(values[i]);
            if (values[i] instanceof String && !value.isEmpty() && "'=+-@\t\r".indexOf(value.charAt(0)) >= 0) value = "'" + value;
            result.append('"').append(value.replace("\"", "\"\"")).append('"');
        }
        return result.append("\r\n").toString();
    }

    private static void text(OutputStream out, String text) throws IOException { out.write(text.getBytes(StandardCharsets.UTF_8)); }
    private static long value(Long value) { return value == null ? 0 : value; }
    private static void addNode(Set<String> nodes, String node) { if (node != null && !node.trim().isEmpty()) nodes.add(node); }
    private static String key(FileReportRecord record) { return Base64.getUrlEncoder().withoutPadding().encodeToString(record.getLogicalKey().getBytes(StandardCharsets.UTF_8)); }
    private static boolean active(SubTaskStatus.TaskStatus status) { return status == null || status == SubTaskStatus.TaskStatus.PENDING || status == SubTaskStatus.TaskStatus.RUNNING || status == SubTaskStatus.TaskStatus.LIST_DONE; }
    private static boolean activeSnapshot(SubTaskStatus summary, Map<String, TaskReportWorkerSnapshot> snapshots) {
        if (active(summary.getStatus())) return true;
        for (TaskReportWorkerSnapshot snapshot : snapshots.values()) {
            if (Boolean.TRUE.equals(snapshot.getExecutionActive())) return true;
            for (SubTaskStatus task : snapshot.getTasks()) if (active(task.getStatus())) return true;
            String discovery = snapshot.getDiscoveryStatus();
            if (discovery != null && !"COMPLETED".equals(discovery) && !"FAILED".equals(discovery)
                    && !"STOPPED".equals(discovery)) return true;
        }
        return false;
    }
    private static boolean stoppedSnapshot(SubTaskStatus summary, Map<String, TaskReportWorkerSnapshot> snapshots) {
        if (summary.getStatus() == SubTaskStatus.TaskStatus.STOPPED) return true;
        for (TaskReportWorkerSnapshot snapshot : snapshots.values()) {
            if ("STOPPED".equals(snapshot.getDiscoveryStatus())) return true;
            for (SubTaskStatus task : snapshot.getTasks()) if (task.getStatus() == SubTaskStatus.TaskStatus.STOPPED) return true;
        }
        return false;
    }
    private static int rank(SubTaskStatus.TaskStatus status) {
        if (active(status)) return status == SubTaskStatus.TaskStatus.PENDING ? 6 : 7;
        if (status == SubTaskStatus.TaskStatus.STOPPED) return 5;
        if (status == SubTaskStatus.TaskStatus.FAILED || status == SubTaskStatus.TaskStatus.PARTIAL_FAILED) return 4;
        return 0;
    }
    private static FileReportRecord finalRecord(FileReportRecord left, FileReportRecord right) {
        int a = "SUCCESS".equals(left.getResult()) ? 2 : "UNPROCESSED".equals(left.getResult()) ? 0 : 1;
        int b = "SUCCESS".equals(right.getResult()) ? 2 : "UNPROCESSED".equals(right.getResult()) ? 0 : 1;
        if (a != b) return a > b ? left : right;
        return value(left.getCompletedAt()) >= value(right.getCompletedAt()) ? left : right;
    }
    private Map<String, Object> statistics(SubTaskStatus task) {
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("status", task.getStatus()); result.put("scannedCount", value(task.getListObjectCount()));
        result.put("scannedBytes", value(task.getListObjectSize())); result.put("successCount", value(task.getPutObjectCount()));
        result.put("successBytes", value(task.getPutObjectSize())); result.put("skippedCount", value(task.getSkippedObjectCount()));
        result.put("skippedBytes", value(task.getSkippedObjectSize())); result.put("failedCount", value(task.getFailedObjectCount()));
        result.put("failedBytes", value(task.getFailedObjectSize()));
        return result;
    }
    private String fingerprint(TaskReportWorkerSnapshot snapshot) {
        List<Object> entries = new ArrayList<>();
        entries.add(snapshot.isFound()); entries.add(snapshot.isScopeMissing()); entries.add(snapshot.getExecutionActive());
        entries.add(snapshot.getDiscoveryComplete()); entries.add(snapshot.getDiscoveryStatus());
        entries.add(snapshot.getCoverage() == null ? null : mapper.convertValue(snapshot.getCoverage(), Map.class));
        for (SubTaskStatus task : snapshot.getTasks()) entries.add(Arrays.asList(task.getTaskIndex(), task.getAttempt(),
                statistics(task), task.getStartTime(), task.getEndTime()));
        return entries.toString();
    }
    private String parentFingerprint(LocalParentTaskRecord parent) {
        return parent == null ? "null" : Arrays.asList(parent.getStatus(), parent.getOrchestrationComplete(), parent.getUpdatedAt(), parent.getErrorCode()).toString();
    }

    private TaskReportWorkerSnapshot checkedTarget(TaskReportWorkerSnapshot snapshot, ReportTarget target, ReportScope scope) {
        checkedScope(snapshot, target.taskId);
        if (scope.creation != null) for (SubTaskStatus task : snapshot.getTasks()) {
            if (!target.taskId.equals(task.getTaskIndex())) throw new IllegalStateException("unexpected physical task scope");
        }
        return snapshot;
    }

    private static Set<Integer> missingTaskIds(ReportScope scope, Map<String, TaskReportWorkerSnapshot> snapshots) {
        Set<Integer> missing = new LinkedHashSet<>();
        if (scope.creation == null) return missing;
        missing.addAll(scope.physicalTaskIds);
        for (TaskReportWorkerSnapshot snapshot : snapshots.values()) if (snapshot.isFound()) {
            for (SubTaskStatus task : snapshot.getTasks()) missing.remove(task.getTaskIndex());
        }
        return missing;
    }

    private static boolean creationEnded(TaskCreationService.Creation creation) {
        return "COMPLETED".equals(creation.getStatus()) || "FAILED".equals(creation.getStatus())
                || "STOPPED".equals(creation.getStatus());
    }

    private String creationFingerprint(TaskCreationService.Creation creation) {
        return creation == null ? "null" : mapper.convertValue(creation, Map.class).toString();
    }

    private static Set<String> targetNodes(ReportScope scope, Collection<String> targets) {
        Set<String> result = new LinkedHashSet<>();
        for (String key : targets) result.add(scope.targets.containsKey(key) ? scope.targets.get(key).node : key);
        return result;
    }

    private static class ReportTarget {
        final String node;
        final Integer taskId;
        ReportTarget(String node, Integer taskId) { this.node = node; this.taskId = taskId; }
    }

    @Getter
    public static class ReportScope {
        public boolean isStoppedSnapshot() { return stoppedSnapshot(summary, initial); }
        private final Integer taskId;
        private long resolvedAt;
        private LocalParentTaskRecord parent;
        private TaskCreationService.Creation creation;
        private String creationFingerprint;
        private final Set<Integer> physicalTaskIds = new LinkedHashSet<>();
        private final Set<Integer> missingTaskIds = new LinkedHashSet<>();
        private final Map<String, ReportTarget> targets = new LinkedHashMap<>();
        private final Set<String> unreachableTargets = new LinkedHashSet<>();
        private SubTaskStatus summary;
        private final Set<String> requiredNodes = new LinkedHashSet<>();
        private final Set<String> unreachableNodes = new LinkedHashSet<>();
        private final Map<String, TaskReportWorkerSnapshot> initial = new LinkedHashMap<>();
        private ReportScope(Integer taskId) { this.taskId = taskId; }
    }

    private static class ExportState {
        private long rows, bytes, duplicateRows, unknownFileSizeCount;
        private final Map<String, Long> counts = new LinkedHashMap<>(), resultBytes = new LinkedHashMap<>();
        private final Map<String, String> issues = new LinkedHashMap<>();
        private final Set<String> changedNodes = new LinkedHashSet<>();
        private final Set<String> scopeMissingNodes = new LinkedHashSet<>();
        private final Map<String, Long> revisions = new HashMap<>();
        ExportState(ReportScope scope) {
            for (String result : RESULTS) { counts.put(result, 0L); resultBytes.put(result, 0L); }
            for (String node : scope.unreachableTargets) fail(node, "WORKER_UNAVAILABLE");
            for (Map.Entry<String, TaskReportWorkerSnapshot> item : scope.initial.entrySet()) {
                if (item.getValue().isScopeMissing()) scopeMissing(item.getKey());
                if (item.getValue().getCoverage() != null) revisions.put(item.getKey(), item.getValue().getCoverage().getRevision());
            }
        }
        void fail(String node, String code) { issues.put(node, code); }
        void scopeMissing(String node) { scopeMissingNodes.add(node); issues.putIfAbsent(node, "NODE_SCOPE_MISSING"); }
        void count(FileReportRecord record) {
            rows++; bytes += value(record.getSize());
            if (record.getSize() == null) unknownFileSizeCount++;
            counts.put(record.getResult(), counts.get(record.getResult()) + 1);
            resultBytes.put(record.getResult(), resultBytes.get(record.getResult()) + value(record.getSize()));
        }
    }

    private class NodeCursor {
        final Integer taskId, reportTaskId;
        final String node, targetKey;
        final boolean physicalScope;
        final ExportState state;
        List<FileReportRecord> items = Collections.emptyList();
        int offset;
        String next, previousKey;
        boolean done, stopped;
        FileReportRecord current;
        NodeCursor(Integer reportTaskId, ReportTarget target, String targetKey, boolean physicalScope, ExportState state) {
            this.taskId = target.taskId; this.reportTaskId = reportTaskId; this.node = target.node;
            this.targetKey = targetKey; this.physicalScope = physicalScope; this.state = state;
        }
        boolean advance() {
            if (stopped) return false;
            try {
                while (offset >= items.size()) {
                    if (done) return false;
                    FileReportPage page = fetchPage(node, taskId, next);
                    if (page == null || page.getItems() == null || page.getItems().size() > 1000) throw new IllegalStateException("invalid page");
                    if (!page.isDone() && (page.getNextCursor() == null || page.getNextCursor().equals(next))) throw new IllegalStateException("cursor did not advance");
                    if (page.getCoverage() != null) {
                        Long revision = state.revisions.get(targetKey);
                        if (revision != null && revision != page.getCoverage().getRevision()) state.changedNodes.add(targetKey);
                    }
                    items = page.getItems(); offset = 0; next = page.getNextCursor(); done = page.isDone();
                }
                current = items.get(offset++);
                if (current == null || !taskId.equals(current.getReportTaskId()) || current.getLogicalKey() == null
                        || (physicalScope && !taskId.equals(current.getTaskId()))
                        || !RESULTS.contains(current.getResult()) || value(current.getSize()) < 0) throw new IllegalStateException("invalid record");
                String ordered = key(current);
                if (previousKey != null && ordered.compareTo(previousKey) < 0) throw new IllegalStateException("unordered record");
                previousKey = ordered;
                FileReportRecord copy = new FileReportRecord();
                BeanUtils.copyProperties(current, copy);
                copy.setReportTaskId(reportTaskId); copy.setNode(node); current = copy;
                return true;
            } catch (RuntimeException error) {
                stopped = true; state.fail(targetKey, "WORKER_REPORT_INCOMPLETE"); return false;
            }
        }
    }
}
