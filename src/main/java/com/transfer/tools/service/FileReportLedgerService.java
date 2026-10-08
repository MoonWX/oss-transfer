package com.transfer.tools.service;

import com.transfer.tools.model.*;
import com.transfer.tools.util.OssKeyMapper;
import com.transfer.tools.storage.RocksDBService;
import lombok.Data;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import java.nio.charset.StandardCharsets;
import java.nio.file.Paths;
import java.util.*;
import java.util.concurrent.ConcurrentHashMap;

/** Durable, retry-idempotent per-file state. No credentials or connection secrets are stored. */
@Service
public class FileReportLedgerService {
    @Autowired private RocksDBService rocksDBService;
    private final Map<Integer, String> writeFailures = new ConcurrentHashMap<>();
    private static final String PREFIX = "file-report:";
    private static final String LEGACY_WRITE_FAILURE = "Legacy report persistence failed";

    public static Integer reportTaskId(SubTaskStatus task) {
        return task.getParentTaskId() == null ? task.getTaskIndex() : task.getParentTaskId();
    }

    private static String encode(String value) {
        return Base64.getUrlEncoder().withoutPadding().encodeToString(value.getBytes(StandardCharsets.UTF_8));
    }
    private String header(Integer id) { return PREFIX + id + ":coverage"; }
    private String rows(Integer id) { return PREFIX + id + ":item:"; }
    private String scanKey(Integer id, String scan) { return PREFIX + id + ":scan:" + encode(scan); }
    private String scanId(SubTaskStatus task) { return "task:" + task.getTaskIndex() + ":" + task.getShardId(); }

    /** Permanent deletion keeps a fence outside the report namespace for stale callbacks. */
    public synchronized void purge(Integer taskId) {
        synchronized (rocksDBService) {
            rocksDBService.putSync("report:purged:" + taskId, true);
            rocksDBService.deleteByPrefix(PREFIX + taskId + ":");
            writeFailures.remove(taskId);
        }
    }

    public synchronized ReportCoverage coverage(Integer taskId) {
        ReportCoverage value = rocksDBService.get(header(taskId), ReportCoverage.class);
        if (value == null) value = new ReportCoverage();
        if (writeFailures.containsKey(taskId)) {
            value.setLedgerCoverage("PARTIAL");
            value.setLastError(writeFailures.get(taskId));
        }
        value.setScanComplete("TRACKED".equals(value.getLedgerCoverage())
                && value.getRegisteredScans() > 0 && value.getRegisteredScans() == value.getCompletedScans());
        return value;
    }

    public synchronized void registerTask(SubTaskStatus task) {
        changeScan(reportTaskId(task), scanId(task), "PENDING",
                rocksDBService.get("task:" + task.getTaskIndex(), SubTaskStatus.class) == null);
    }
    public synchronized void scanStarted(SubTaskStatus task) {
        changeScan(reportTaskId(task), scanId(task), "RUNNING", false);
    }
    public synchronized void scanCompleted(SubTaskStatus task) {
        changeScan(reportTaskId(task), scanId(task), "COMPLETE", false);
    }
    public synchronized void discoveryRegistered(Integer id) { changeScan(id, "discovery", "PENDING", true); }
    public synchronized void discoveryStarted(Integer id) { changeScan(id, "discovery", "RUNNING", false); }
    public synchronized void discoveryCompleted(Integer id) { changeScan(id, "discovery", "COMPLETE", false); }

    private void changeScan(Integer id, String scan, String state, boolean newlyCreated) {
        String key = scanKey(id, scan);
        String prior = rocksDBService.get(key, String.class);
        if ("PENDING".equals(state) && prior != null) return;
        if (state.equals(prior)) return;
        ReportCoverage value = rocksDBService.get(header(id), ReportCoverage.class);
        if (value == null) {
            value = new ReportCoverage();
            if (newlyCreated) value.setLedgerCoverage("TRACKED");
        }
        if (prior == null) value.setRegisteredScans(value.getRegisteredScans() + 1);
        if ("COMPLETE".equals(prior)) value.setCompletedScans(value.getCompletedScans() - 1);
        if ("COMPLETE".equals(state)) value.setCompletedScans(value.getCompletedScans() + 1);
        if ("RUNNING".equals(state) && value.getScanStartedAt() == null) value.setScanStartedAt(System.currentTimeMillis());
        value.setScanCompletedAt(value.getRegisteredScans() == value.getCompletedScans()
                ? System.currentTimeMillis() : null);
        value.setRevision(value.getRevision() + 1);
        Map<String, Object> updates = new LinkedHashMap<>();
        updates.put(key, state);
        updates.put(header(id), value);
        persist(id, updates);
    }

    /** A scan failure leaves registered-but-incomplete coverage, even if the task has zero files. */
    public synchronized void scanFailed(Integer id, String error) {
        ReportCoverage value = coverage(id);
        value.setLastError(error);
        value.setRevision(value.getRevision() + 1);
        persist(id, Collections.singletonMap(header(id), value));
    }

    public synchronized void markPartial(Integer id, String reason) {
        ReportCoverage value = coverage(id);
        value.setLedgerCoverage("PARTIAL");
        value.setScanComplete(false);
        value.setLastError(reason);
        value.setRevision(value.getRevision() + 1);
        persist(id, Collections.singletonMap(header(id), value));
    }

    public void discovered(SubTaskStatus task, String key, Long size, String sourcePath) {
        store(fromTask(task, key, size, sourcePath), "UNPROCESSED", "DISCOVERED", null);
    }
    public void failed(SubTaskStatus task, String key, Long size, String sourcePath, String reason, String error) {
        store(fromTask(task, key, size, sourcePath), "FAILED", reason, error);
    }
    public void skipped(SubTaskStatus task, String key, Long size, String sourcePath, String reason) {
        store(fromTask(task, key, size, sourcePath), "SKIPPED", reason, null);
    }
    public synchronized boolean hasUnknownSize(Integer taskId, String logicalKey) {
        FileReportRecord row = rocksDBService.get(rows(taskId) + encode(logicalKey), FileReportRecord.class);
        return row != null && row.getSize() == null;
    }

    /** Synchronous per-file results survive a crash between destination commit and legacy completion indexing. */
    public synchronized String completedResult(Integer taskId, String logicalKey) {
        FileReportRecord row = rocksDBService.get(rows(taskId) + encode(logicalKey), FileReportRecord.class);
        if (row == null) return null;
        return "SUCCESS".equals(row.getResult()) || "SKIPPED".equals(row.getResult()) ? row.getResult() : null;
    }

    public synchronized Long completedSize(Integer taskId, String logicalKey) {
        FileReportRecord row = rocksDBService.get(rows(taskId) + encode(logicalKey), FileReportRecord.class);
        return row != null && ("SUCCESS".equals(row.getResult()) || "SKIPPED".equals(row.getResult()))
                ? row.getSize() : null;
    }

    /** Fill inventory metadata without replacing a retained completion or failure result. */
    public synchronized void sourceMetadataResolved(ObjectListMessage message) {
        FileReportRecord resolved = fromMessage(message);
        Integer id = resolved.getReportTaskId();
        String key = rows(id) + encode(resolved.getLogicalKey());
        FileReportRecord row = rocksDBService.get(key, FileReportRecord.class);
        if (row != null && Objects.equals(row.getSize(), resolved.getSize())) return;
        ReportCoverage value = coverage(id);
        if (row == null) {
            row = resolved;
            row.setResult("UNPROCESSED"); row.setReasonCode("DISCOVERED");
            row.setDiscoveredAt(System.currentTimeMillis());
            value.setDiscoveredCount(value.getDiscoveredCount() + 1);
        } else row.setSize(resolved.getSize());
        value.setRevision(value.getRevision() + 1);
        Map<String, Object> updates = new LinkedHashMap<>();
        updates.put(key, row); updates.put(header(id), value);
        persist(id, updates);
    }

    public void finished(ObjectListMessage message, String result, String reason, String error) {
        store(fromMessage(message), result, reason, error);
    }
    public void discoveryItem(LocalTransferRequest request, LocalFileItem item, String node) {
        String sourcePrefix = request.getSourceEndpoint().getPrefix();
        if (sourcePrefix != null && !sourcePrefix.isEmpty() && !item.getRelativeKey().startsWith(sourcePrefix)) return;
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(request.getParentTaskId());
        task.setParentTaskId(request.getParentTaskId());
        task.setShardId("discovery");
        task.setDirection(request.getDirection());
        task.setSourceEndpoint(request.getSourceEndpoint());
        task.setDestinationEndpoint(request.getDestinationEndpoint());
        task.setWorkerNode(node);
        task.setDestBucket(request.getDestinationEndpoint().getBucket());
        task.setDestS3Alias(request.getDestinationEndpoint().getS3Alias());
        String objectKey = request.getDestinationEndpoint().getObjectKey();
        String key = objectKey == null || objectKey.isEmpty()
                ? join(request.getDestinationEndpoint().getPrefix(), item.getRelativeKey()) : objectKey;
        discovered(task, key, item.getSize(), item.getPath().toString());
    }

    private synchronized void store(FileReportRecord row, String result, String reason, String error) {
        if (row.getReportTaskId() == null || row.getLogicalKey() == null) {
            throw new IllegalArgumentException("Report task and logical key are required");
        }
        Integer id = row.getReportTaskId();
        String key = rows(id) + encode(row.getLogicalKey());
        FileReportRecord prior = rocksDBService.get(key, FileReportRecord.class);
        // Rescanning or retrying must not turn a durable success into skipped/failed/unprocessed.
        if (prior != null && ("SUCCESS".equals(prior.getResult())
                || ("UNPROCESSED".equals(result) && !"UNPROCESSED".equals(prior.getResult())))) return;
        if (prior != null) {
            if (row.getSize() == null) row.setSize(prior.getSize());
            if (row.getSourcePath() == null) row.setSourcePath(prior.getSourcePath());
            if (row.getDestinationPath() == null) row.setDestinationPath(prior.getDestinationPath());
        }
        if (prior != null && prior.getCompletedAt() != null && row.getCompletedAt() != null
                && prior.getCompletedAt() > row.getCompletedAt() && !"SUCCESS".equals(result)) return;
        row.setResult(result);
        row.setReasonCode(reason);
        row.setErrorMessage(error);
        boolean legacy = reason != null && reason.startsWith("LEGACY_RETAINED_");
        row.setDiscoveredAt(prior == null ? (legacy ? null : System.currentTimeMillis()) : prior.getDiscoveredAt());
        if (!legacy && !"UNPROCESSED".equals(result) && row.getCompletedAt() == null) row.setCompletedAt(System.currentTimeMillis());
        ReportCoverage value = coverage(id);
        if (prior == null) value.setDiscoveredCount(value.getDiscoveredCount() + 1);
        value.setRevision(value.getRevision() + 1);
        Map<String, Object> updates = new LinkedHashMap<>();
        updates.put(key, row);
        updates.put(header(id), value);
        persist(id, updates);
    }

    private void persist(Integer id, Map<String, Object> updates) {
        persist(id, updates, "File report persistence failed");
    }

    private void persist(Integer id, Map<String, Object> updates, String failureMessage) {
        try {
            synchronized (rocksDBService) {
                if (Boolean.TRUE.equals(rocksDBService.get("report:purged:" + id, Boolean.class))) return;
                rocksDBService.putBatchSync(updates);
            }
        } catch (RuntimeException error) {
            String message = failureMessage;
            writeFailures.put(id, message);
            try {
                ReportCoverage value = rocksDBService.get(header(id), ReportCoverage.class);
                if (value == null) value = new ReportCoverage();
                value.setLedgerCoverage("PARTIAL");
                value.setScanComplete(false);
                value.setLastError(message);
                value.setRevision(value.getRevision() + 1);
                rocksDBService.putSync(header(id), value);
            } catch (RuntimeException markerError) {
                error.addSuppressed(markerError);
            }
            throw new FileReportPersistenceException(message, error);
        }
    }

    public synchronized FileReportPage page(Integer taskId, String cursor, int limit) {
        if (taskId == null) throw new IllegalArgumentException("report task is required");
        validateCursor(taskId, cursor);
        int size = limit <= 0 ? 500 : Math.min(limit, 1000);
        FileReportPage result = new FileReportPage();
        if (Boolean.TRUE.equals(rocksDBService.get("report:purged:" + taskId, Boolean.class))) {
            result.setDone(true);
            result.setCoverage(new ReportCoverage());
            return result;
        }
        if (!backfillLegacyPage(taskId)) {
            result.setNextCursor("legacy:" + taskId + ":" + coverage(taskId).getRevision());
            result.setCoverage(coverage(taskId));
            return result;
        }
        if (cursor != null && cursor.startsWith("legacy:")) {
            if (!cursor.startsWith("legacy:" + taskId + ":")) throw new IllegalArgumentException("Cursor belongs to a different scope");
            cursor = null;
        }
        RocksDBPage<FileReportRecord> data = rocksDBService.scanPage(rows(taskId), cursor, size, FileReportRecord.class);
        result.setItems(new ArrayList<>(data.getItems().values()));
        result.setNextCursor(data.getNextCursor());
        result.setDone(data.isDone());
        result.setCoverage(coverage(taskId));
        return result;
    }

    private void validateCursor(Integer taskId, String cursor) {
        if (cursor == null) return;
        if (cursor.trim().isEmpty()) throw new IllegalArgumentException("Invalid report cursor");
        if (cursor.startsWith("legacy:")) {
            String prefix = "legacy:" + taskId + ":";
            if (!cursor.startsWith(prefix)) throw new IllegalArgumentException("Cursor belongs to a different scope");
            String revision = cursor.substring(prefix.length());
            try {
                if (!revision.matches("[1-9][0-9]*") || Long.parseLong(revision) <= 0) {
                    throw new IllegalArgumentException("Invalid report cursor");
                }
            } catch (NumberFormatException error) {
                throw new IllegalArgumentException("Invalid report cursor", error);
            }
            return;
        }
        try {
            String key = new String(Base64.getUrlDecoder().decode(cursor), StandardCharsets.UTF_8);
            if (!key.startsWith(rows(taskId))) throw new IllegalArgumentException("Cursor belongs to a different scope");
        } catch (IllegalArgumentException error) {
            throw new IllegalArgumentException("Invalid report cursor or task scope", error);
        }
    }

    /** Old retention is incomplete: convert at most 500 entries per call, and never claim full coverage. */
    private boolean backfillLegacyPage(Integer id) {
        String stateKey = PREFIX + id + ":legacy";
        ReportCoverage existing = rocksDBService.get(header(id), ReportCoverage.class);
        LegacyState state = rocksDBService.get(stateKey, LegacyState.class);
        if (state == null && existing != null && !"LEGACY_UNKNOWN".equals(existing.getLedgerCoverage())
                && !LEGACY_WRITE_FAILURE.equals(existing.getLastError())) return true;
        if (state == null) state = new LegacyState();
        if (state.isDone()) return true;
        Map<String, Object> updates = new LinkedHashMap<>();
        ReportCoverage value = coverage(id);
        if (!state.isCompletedDone()) {
            RocksDBPage<CompletedItemRecord> page = rocksDBService.scanPage("completed:" + id + ":",
                    state.getCompletedCursor(), 500, CompletedItemRecord.class);
            SubTaskStatus task = rocksDBService.get("task:" + id, SubTaskStatus.class);
            for (CompletedItemRecord item : page.getItems().values()) {
                FileReportRecord row = task == null ? new FileReportRecord()
                        : fromTask(task, item.getLogicalKey(), item.getSize(), null);
                row.setReportTaskId(id);
                row.setTaskId(item.getTaskId());
                row.setLogicalKey(item.getLogicalKey());
                row.setDirection(item.getDirection());
                row.setSize(item.getSize());
                row.setCompletedAt(item.getCompletedAt());
                stageLegacyRow(row, "SUCCESS", "LEGACY_RETAINED_SUCCESS", null, updates, value);
            }
            state.setCompletedCursor(page.getNextCursor());
            state.setCompletedDone(page.isDone());
        } else {
            Long count = rocksDBService.get("failed:count", Long.class);
            long end = Math.min(count == null ? 0L : count, state.getFailedIndex() + 500L);
            while (state.getFailedIndex() < end) {
                long index = state.getFailedIndex() + 1;
                FailedKey failure = rocksDBService.get("failed:" + index, FailedKey.class);
                if (failure != null && (id.equals(failure.getParentTaskId())
                        || (failure.getParentTaskId() == null && id.equals(failure.getTaskIndex())))
                        && failure.getKey() != null && failure.getStage() != FailureStage.SCAN
                        && !"discovery".equals(failure.getShardId())) {
                    SubTaskStatus task = rocksDBService.get("task:" + failure.getTaskIndex(), SubTaskStatus.class);
                    FileReportRecord row = task == null ? new FileReportRecord()
                            : fromTask(task, failure.getKey(), failure.getObjectSize(), failure.getLocalPath());
                    row.setReportTaskId(id);
                    row.setTaskId(failure.getTaskIndex());
                    row.setParentTaskId(failure.getParentTaskId());
                    row.setShardId(failure.getShardId());
                    row.setLogicalKey(failure.getKey());
                    row.setSize(failure.getObjectSize());
                    row.setDirection(failure.getDirection());
                    row.setNode(failure.getWorkerNode());
                    if (task == null) {
                        TransferDirection direction = failure.getDirection() == null ? TransferDirection.OSS_TO_OSS : failure.getDirection();
                        row.setDirection(direction);
                        if (direction != TransferDirection.LOCAL_TO_OSS) {
                            row.setSourceBucket(failure.getSourceBucket());
                            row.setSourceKey(failure.getKey());
                            if (failure.getSourceEndpoint() != null) row.setSourceS3Alias(failure.getSourceEndpoint().getS3Alias());
                        } else {
                            row.setSourcePath(failure.getLocalPath());
                        }
                        if (direction != TransferDirection.OSS_TO_LOCAL) {
                            row.setDestinationBucket(failure.getDestBucket());
                            row.setDestinationKey(failure.getDestinationKey() == null ? failure.getKey() : failure.getDestinationKey());
                            if (failure.getDestinationEndpoint() != null) row.setDestinationS3Alias(failure.getDestinationEndpoint().getS3Alias());
                        }
                    }
                    row.setCompletedAt(failure.getTimestamp());
                    stageLegacyRow(row, "FAILED", "LEGACY_RETAINED_FAILURE", failure.getErrorMessage(), updates, value);
                }
                state.setFailedIndex(index);
            }
            state.setDone(count == null || state.getFailedIndex() >= count);
        }
        updates.put(stateKey, state);
        value.setRevision(value.getRevision() + 1);
        updates.put(header(id), value);
        // Cursor, records and counters commit together. A failed page resumes from its prior cursor.
        persist(id, updates, LEGACY_WRITE_FAILURE);
        return state.isDone();
    }

    private void stageLegacyRow(FileReportRecord row, String result, String reason, String error,
                                Map<String, Object> updates, ReportCoverage coverage) {
        if (row.getReportTaskId() == null || row.getLogicalKey() == null) {
            throw new IllegalArgumentException("Report task and logical key are required");
        }
        String key = rows(row.getReportTaskId()) + encode(row.getLogicalKey());
        FileReportRecord prior = updates.containsKey(key) ? (FileReportRecord) updates.get(key)
                : rocksDBService.get(key, FileReportRecord.class);
        if (prior != null && "SUCCESS".equals(prior.getResult())) return;
        if (prior != null && prior.getCompletedAt() != null && row.getCompletedAt() != null
                && prior.getCompletedAt() > row.getCompletedAt() && !"SUCCESS".equals(result)) return;
        if (prior != null) {
            if (row.getSize() == null) row.setSize(prior.getSize());
            if (row.getSourcePath() == null) row.setSourcePath(prior.getSourcePath());
            if (row.getDestinationPath() == null) row.setDestinationPath(prior.getDestinationPath());
        }
        row.setResult(result);
        row.setReasonCode(reason);
        row.setErrorMessage(error);
        row.setDiscoveredAt(prior == null ? null : prior.getDiscoveredAt());
        if (prior == null) coverage.setDiscoveredCount(coverage.getDiscoveredCount() + 1);
        coverage.setRevision(coverage.getRevision() + 1);
        updates.put(key, row);
    }

    private FileReportRecord fromMessage(ObjectListMessage message) {
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(message.getTaskIndex());
        task.setParentTaskId(message.getParentTaskId());
        task.setShardId(message.getShardId());
        task.setDirection(message.getDirection());
        task.setSourceEndpoint(message.getSourceEndpoint());
        task.setDestinationEndpoint(message.getDestinationEndpoint());
        task.setSourceBucket(message.getSourceBucket());
        task.setSourceS3Alias(message.getSourceS3Alias());
        task.setDestBucket(message.getDestBucket());
        task.setDestS3Alias(message.getDestS3Alias());
        task.setLocalPath(message.getLocalPath());
        task.setWorkerNode(message.getWorkerNode());
        task.setExecutionNodeIp(message.getExecutionNodeIp());
        task.setTargetRelativePath(message.getTargetRelativePath());
        task.setKeyMapping(message.getKeyMapping());
        String key = message.getObjectSummary() == null ? message.getKey() : message.getObjectSummary().getKey();
        Long size = Boolean.TRUE.equals(message.getSourceSizeUnknown()) ? null
                : message.getSourceSize() != null ? message.getSourceSize()
                : message.getObjectSummary() == null ? null : message.getObjectSummary().getSize();
        return fromTask(task, key, size, message.getLocalPath());
    }

    private FileReportRecord fromTask(SubTaskStatus task, String key, Long size, String sourcePath) {
        FileReportRecord row = new FileReportRecord();
        row.setReportTaskId(reportTaskId(task));
        row.setTaskId(task.getTaskIndex());
        row.setParentTaskId(task.getParentTaskId());
        row.setShardId(task.getShardId());
        TransferDirection direction = task.getDirection() == null ? TransferDirection.OSS_TO_OSS : task.getDirection();
        row.setDirection(direction);
        row.setLogicalKey(key);
        row.setSize(size);
        row.setNode(task.getExecutionNodeIp() == null ? task.getWorkerNode() : task.getExecutionNodeIp());
        if (direction == TransferDirection.LOCAL_TO_OSS) {
            row.setSourcePath(sourcePath);
        } else {
            row.setSourceS3Alias(task.getSourceS3Alias());
            row.setSourceBucket(task.getSourceBucket());
            row.setSourceKey(key);
        }
        if (direction == TransferDirection.OSS_TO_LOCAL) {
            String relative = task.getTargetRelativePath() == null || task.getTargetRelativePath().isEmpty()
                    ? key : task.getTargetRelativePath();
            if (task.getLocalPath() != null && relative != null) {
                try {
                    row.setDestinationPath(Paths.get(task.getLocalPath()).resolve(relative.replace('\\', '/')).normalize().toString());
                } catch (java.nio.file.InvalidPathException error) {
                    // Retain the attempted path; execution performs authoritative path validation.
                    row.setDestinationPath(task.getLocalPath() + "/" + relative);
                }
            }
        } else {
            row.setDestinationS3Alias(task.getDestS3Alias());
            row.setDestinationBucket(task.getDestBucket());
            row.setDestinationKey(direction == TransferDirection.OSS_TO_OSS ? destinationKey(task, key) : key);
        }
        return row;
    }

    private static String destinationKey(SubTaskStatus task, String sourceKey) {
        try {
            return OssKeyMapper.destinationKey(task.getKeyMapping(), sourceKey);
        } catch (TransferValidationException invalidKey) {
            // A mapping failure must still be reportable by source identity.
            // Do not invent a destination for a key that cannot be transferred.
            return null;
        }
    }

    private static String join(String prefix, String key) {
        return prefix == null || prefix.isEmpty() ? key : prefix.endsWith("/") ? prefix + key : prefix + "/" + key;
    }
    @Data
    public static class LegacyState {
        private String completedCursor;
        private boolean completedDone;
        private long failedIndex;
        private boolean done;
    }
    public static class FileReportPersistenceException extends IllegalStateException {
        public FileReportPersistenceException(String message, Throwable cause) { super(message, cause); }
    }
}
