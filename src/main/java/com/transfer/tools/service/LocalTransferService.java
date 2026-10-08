package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.config.LocalTransferConfig;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.*;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;
import org.springframework.http.ResponseEntity;
import org.springframework.http.client.ClientHttpRequestInterceptor;
import org.springframework.web.client.RestTemplate;

import javax.annotation.PreDestroy;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Paths;
import java.nio.file.LinkOption;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.ConcurrentHashMap;
import com.transfer.tools.storage.RocksDBService;
import org.springframework.boot.context.event.ApplicationReadyEvent;
import org.springframework.context.event.EventListener;

import java.util.*;

@Service
@lombok.extern.slf4j.Slf4j
public class LocalTransferService {
    @Autowired private TaskBandwidthDispatchGuard taskBandwidthGuard;
    @Autowired private BalancedWorkerScheduler balancedWorkerScheduler;
    @Autowired private NodeConfigService nodeConfigService;
    @Autowired
    private EndpointNormalizationService normalizationService;
    @Autowired
    private LocalFileService localFileService;
    @Autowired
    private DistributedConfig distributedConfig;
    @Autowired
    private S3ClientManager s3ClientManager;
    @Autowired
    private SubTaskManagerService subTaskManagerService;
    @Autowired
    private BufferQueueManager bufferQueueManager;
    @Autowired private com.transfer.tools.executor.ListExecutionService listExecutionService;
    @Autowired
    private LocalTaskControlService controlService;
    @Autowired
    private TransferSpeedService transferSpeedService;
    @Autowired
    private LocalTransferConfig localTransferConfig;
    @Autowired
    private RocksDBService rocksDBService;

    @Autowired
    private FileReportLedgerService fileReportLedgerService;

    private final RestTemplate restTemplate;
    private static final String DISCOVERY_PREFIX = "local:discovery:";
    private static final String BATCH_ASSIGNMENT_PREFIX = "local:batch-assignment:";
    private final Set<Integer> activeDiscoveries = ConcurrentHashMap.newKeySet();
    private final ExecutorService discoveryExecutor = Executors.newSingleThreadExecutor(r -> {
        Thread thread = new Thread(r, "local-file-discovery");
        thread.setDaemon(true);
        return thread;
    });

    public LocalTransferService() {
        restTemplate = new RestTemplate();
        restTemplate.getInterceptors().add((ClientHttpRequestInterceptor) (request, body, execution) -> {
            String token = distributedConfig.getInternalToken();
            if (token != null && !token.isEmpty()) {
                request.getHeaders().set("X-Internal-Token", token);
            }
            return execution.execute(request, body);
        });
    }

    @EventListener(ApplicationReadyEvent.class)
    @org.springframework.core.annotation.Order(-100)
    public void recoverAtStartup() {
        // Agents wait for enrollment and an authenticated control connection before resuming remote batches.
        if (!distributedConfig.isAgent()) recoverDiscoveries();
    }

    public void recoverDiscoveries() {
        List<Integer> automaticIds = new ArrayList<>();
        for (Integer taskId : subTaskManagerService.getTaskIdList()) {
            try {
                SubTaskStatus task = subTaskManagerService.getTaskStatus(taskId);
                if (task != null && controlService.isStopped(taskId)) {
                    subTaskManagerService.stopTask(taskId);
                    continue;
                }
                boolean local = task != null && task.getDirection() != null && task.getDirection() != TransferDirection.OSS_TO_OSS;
                boolean resumedPending = task != null && task.getAttempt() != null && task.getAttempt() > 0;
                if (task != null && ((task.getStatus() == SubTaskStatus.TaskStatus.PENDING
                        && (local || resumedPending || Boolean.TRUE.equals(task.getAutoStart())))
                        || task.getStatus() == SubTaskStatus.TaskStatus.RUNNING
                        || task.getStatus() == SubTaskStatus.TaskStatus.LIST_DONE)) {
                    if (!local && Boolean.TRUE.equals(task.getAutoStart())) {
                        subTaskManagerService.recoverAutomaticTask(task);
                        automaticIds.add(taskId);
                    } else subTaskManagerService.recoverTask(task);
                }
            } catch (RuntimeException failure) {
                // One invalid legacy task must not prevent application startup or other recovery.
                log.error("恢复任务失败: {}", taskId, failure);
                try {
                    SubTaskStatus current = subTaskManagerService.getTaskStatus(taskId);
                    if (current != null && !controlService.isStopped(taskId)
                            && current.getStatus() != SubTaskStatus.TaskStatus.STOPPED) {
                        current.setStatus(SubTaskStatus.TaskStatus.FAILED);
                        current.setErrorCode("TASK_RECOVERY_FAILED");
                        current.setErrorMessage(failure.getMessage());
                        current.setEndTime(System.currentTimeMillis());
                        rocksDBService.putSync("task:" + taskId, current);
                    }
                } catch (RuntimeException persistFailure) {
                    log.error("无法保存恢复失败状态: {}", taskId, persistFailure);
                }
            }
        }
        if (!automaticIds.isEmpty()) listExecutionService.startExecution(null, null, automaticIds);
        for (LocalDiscoveryState state : rocksDBService.scanByPrefix(DISCOVERY_PREFIX, LocalDiscoveryState.class)) {
            if (state.getStatus() == LocalDiscoveryState.Status.PENDING
                    || state.getStatus() == LocalDiscoveryState.Status.RUNNING) {
                scheduleDiscovery(state);
            }
        }
    }

    public Map<String, Object> capabilities() {
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("executionNodeIp", distributedConfig.getLocalIp());
        result.put("localTransferSupported", true);
        result.put("advertisedLocalRoots", distributedConfig.getAdvertisedLocalRoots() == null
                ? Collections.emptyList() : distributedConfig.getAdvertisedLocalRoots());
        List<Object> bindings = new ArrayList<>();
        for (String root : distributedConfig.getAdvertisedLocalRoots() == null
                ? Collections.<String>emptyList() : distributedConfig.getAdvertisedLocalRoots()) {
            Map<String, String> binding = new LinkedHashMap<>();
            binding.put("storageId", null);
            binding.put("localPath", root);
            bindings.add(binding);
        }
        result.put("advertisedLocalBindings", bindings);
        return result;
    }

    public Map<String, Object> validateOnWorker(LocalTransferRequest request) {
        normalizationService.normalizeAndValidate(request);
        TransferEndpoint local = localEndpoint(request);
        LocalNodeBinding binding = normalizationService.bindingFor(local, distributedConfig.getLocalIp());
        if (request.getDirection() == TransferDirection.LOCAL_TO_OSS) {
            localFileService.validateSource(binding.getLocalPath());
        } else {
            localFileService.validateDestination(binding.getLocalPath(), true);
        }
        TransferEndpoint oss = ossEndpoint(request);
        if (s3ClientManager.getClient(oss.getS3Alias()) == null) {
            throw new TransferValidationException("S3_CONFIG_NOT_FOUND", "S3 alias is not configured: " + oss.getS3Alias());
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("valid", true);
        result.put("executionNodeIp", binding.getExecutionNodeIp());
        result.put("localPath", binding.getLocalPath());
        result.put("storageId", local.getStorageId());
        result.put("pathType", request.getDirection() == TransferDirection.LOCAL_TO_OSS
                ? (Files.isRegularFile(Paths.get(binding.getLocalPath()), LinkOption.NOFOLLOW_LINKS) ? "FILE" : "DIRECTORY")
                : "DIRECTORY");
        return result;
    }

    public synchronized List<Integer> accept(List<LocalTransferRequest> requests) {
        if (requests == null || requests.isEmpty()) {
            throw new TransferValidationException("INVALID_ENDPOINT", "at least one transfer request is required");
        }
        List<Integer> ids = new ArrayList<>();
        for (LocalTransferRequest request : requests) {
            validateOnWorker(request);
        }
        for (LocalTransferRequest request : requests) {
            Integer taskId = request.getTaskId() == null ? subTaskManagerService.getNextTaskId() : request.getTaskId();
            ListSubTask task = toSubTask(request, normalizationService.bindingFor(
                    localEndpoint(request), distributedConfig.getLocalIp()));
            SubTaskStatus retained = subTaskManagerService.getTaskStatus(taskId);
            if (retained != null) {
                subTaskManagerService.ensureTaskIndexed(taskId);
                if (retained.getStatus() == SubTaskStatus.TaskStatus.PENDING
                        && !controlService.isActive(taskId) && !controlService.isStopped(taskId)
                        && !controlService.isStopped(retained.getParentTaskId())) {
                    bufferQueueManager.offerListTask(retained);
                }
                ids.add(taskId);
                continue;
            }
            if (controlService.isStopped(task.getParentTaskId())) {
                throw new TaskCancelledException("parent task is stopped");
            }
            subTaskManagerService.saveAssignedTask(taskId, task);
            SubTaskStatus status = subTaskManagerService.getTaskStatus(taskId);
            bufferQueueManager.offerListTask(status);
            ids.add(taskId);
        }
        return ids;
    }

    public ListSubTask toSubTask(LocalTransferRequest request) {
        return toSubTask(request, normalizationService.bindingFor(
                localEndpoint(request), distributedConfig.getLocalIp()));
    }

    private ListSubTask toSubTask(LocalTransferRequest request, LocalNodeBinding binding) {
        if (taskBandwidthGuard != null) taskBandwidthGuard.requireReady(request.getTaskBandwidthScopeId(), request.getTaskBandwidthLimitMbps());
        normalizationService.normalizeAndValidate(request);
        TransferEndpoint source = request.getSourceEndpoint();
        TransferEndpoint destination = request.getDestinationEndpoint();
        TransferEndpoint local = localEndpoint(request);
        ListSubTask task = new ListSubTask();
        task.setDirection(request.getDirection());
        task.setSourceEndpoint(source);
        task.setDestinationEndpoint(destination);
        task.setConflictPolicy(request.getConflictPolicy());
        task.setOverwrite(request.getConflictPolicy() == ConflictPolicy.OVERWRITE);
        task.setExecutionNodeIp(binding.getExecutionNodeIp());
        task.setLocalPath(binding.getLocalPath());
        task.setDateAfter(request.getDateAfter());
        task.setDateBefore(request.getDateBefore());
        task.setTargetRelativePath(request.getTargetRelativePath());
        task.setDistributionMode(request.getDistributionMode());
        task.setParentTaskId(request.getParentTaskId() == null ? request.getTaskId() : request.getParentTaskId());
        task.setStorageId(local.getStorageId());
        task.setNodeBindings(local.getNodeBindings());
        task.setEligibleWorkerNodes(nodeIps(local));
        task.setAttempt(0);
        task.setListQpsLimit(request.getListQpsLimit());
        task.setHeadQpsLimit(request.getHeadQpsLimit());
        task.setPutQpsLimit(request.getPutQpsLimit());
        task.setBandwidthBitsPerSecond(request.getBandwidthBitsPerSecond());
        task.setTaskBandwidthLimitMbps(request.getTaskBandwidthLimitMbps());
        task.setTaskBandwidthScopeId(request.getTaskBandwidthScopeId());
        if (request.getDirection() == TransferDirection.OSS_TO_LOCAL) {
            task.setSourceBucket(source.getBucket());
            task.setSourceS3Alias(source.getS3Alias());
            task.setObjectKey(source.getObjectKey());
            task.setStartKey(source.getPrefix() == null ? "" : source.getPrefix());
            task.setEndKey("");
        } else {
            task.setDestBucket(destination.getBucket());
            task.setDestS3Alias(destination.getS3Alias());
            task.setObjectKey(destination.getObjectKey());
            task.setStartKey(source.getPrefix() == null ? "" : source.getPrefix());
            task.setEndKey("");
        }
        return task;
    }

    public Map<String, Object> startDiscovery(LocalTransferRequest request) {
        if (request != null && taskBandwidthGuard != null) taskBandwidthGuard.requireReady(request.getTaskBandwidthScopeId(), request.getTaskBandwidthLimitMbps());
        validateOnWorker(request);
        if (request.getDirection() != TransferDirection.LOCAL_TO_OSS) {
            throw new TransferValidationException("INVALID_DIRECTION", "discovery only accepts LOCAL_TO_OSS");
        }
        if (request.getParentTaskId() == null) {
            throw new TransferValidationException("INVALID_ENDPOINT", "parentTaskId is required for discovery");
        }
        synchronized (controlService) {
            requireNotPurged(request.getParentTaskId());
            LocalDiscoveryState state = rocksDBService.get(discoveryKey(request.getParentTaskId()), LocalDiscoveryState.class);
            // A stopped parent must be explicitly resumed before accepting discovery again.
            // This also closes the interval between a permanent deletion's stop and purge fences.
            controlService.checkRunning(request.getParentTaskId());
            if (state == null) {
                state = new LocalDiscoveryState(request.getParentTaskId(), request, distributedConfig.getLocalIp(),
                        null, 0, LocalDiscoveryState.Status.PENDING, null, System.currentTimeMillis());
                fileReportLedgerService.discoveryRegistered(request.getParentTaskId());
                rocksDBService.put(discoveryKey(request.getParentTaskId()), state);
            }
            if (state.getStatus() != LocalDiscoveryState.Status.COMPLETED) scheduleDiscovery(state);
            Map<String, Object> result = new LinkedHashMap<>();
            result.put("parentTaskId", request.getParentTaskId());
            result.put("discoveryWorker", distributedConfig.getLocalIp());
            result.put("accepted", true);
            return result;
        }
    }

    private void requireNotPurged(Integer taskId) {
        if (taskId != null && (Boolean.TRUE.equals(rocksDBService.get("local:parent:purged:" + taskId, Boolean.class))
                || Boolean.TRUE.equals(rocksDBService.get("report:purged:" + taskId, Boolean.class))
                || Boolean.TRUE.equals(rocksDBService.get("task:purged:" + taskId, Boolean.class)))) {
            throw new TransferValidationException("TASK_PERMANENTLY_DELETED", "任务已永久删除: " + taskId);
        }
    }

    private void resumeDiscovery(Integer parentId, boolean deferDiscovery) {
        synchronized (controlService) {
            requireNotPurged(parentId);
            controlService.resume(parentId);
            LocalDiscoveryState state = rocksDBService.get(discoveryKey(parentId), LocalDiscoveryState.class);
            if (!deferDiscovery && state != null && state.getStatus() != LocalDiscoveryState.Status.COMPLETED) scheduleDiscovery(state);
        }
    }

    private void scheduleDiscovery(LocalDiscoveryState state) {
        if (state == null || state.getParentTaskId() == null || !activeDiscoveries.add(state.getParentTaskId())) return;
        discoveryExecutor.submit(() -> {
            try {
                try (LocalTaskControlService.Operation ignored = controlService.begin(state.getParentTaskId())) {
                    discoverAndDispatch(state);
                } catch (TaskCancelledException ignored) { }
            } finally {
                activeDiscoveries.remove(state.getParentTaskId());
            }
        });
    }

    private void discoverAndDispatch(LocalDiscoveryState state) {
        LocalTransferRequest request = state.getRequest();
        TransferEndpoint local = localEndpoint(request);
        LocalNodeBinding discoveryBinding = normalizationService.bindingFor(local, distributedConfig.getLocalIp());
        List<LocalNodeBinding> bindings = local.getNodeBindings();
        List<LocalFileEntry> entries = new ArrayList<>();
        AtomicInteger bytes = new AtomicInteger();
        java.util.concurrent.atomic.AtomicLong dataBytes = new java.util.concurrent.atomic.AtomicLong();
        boolean balanced = DistributionMode.BALANCED.getValue().equals(request.getDistributionMode());
        AtomicInteger sequence = new AtomicInteger(state.getNextBatchSequence() == null ? 0 : state.getNextBatchSequence());
        state.setStatus(LocalDiscoveryState.Status.RUNNING);
        state.setErrorMessage(null);
        persistDiscovery(state);
        try {
            fileReportLedgerService.discoveryStarted(request.getParentTaskId());
            if (state.getPendingBatch() != null) {
                boolean finalBatch = Boolean.TRUE.equals(state.getPendingBatch().getLastBatch());
                dispatchPendingBatch(state);
                sequence.set(state.getNextBatchSequence());
                if (finalBatch) {
                    fileReportLedgerService.discoveryCompleted(request.getParentTaskId());
                    state.setStatus(LocalDiscoveryState.Status.COMPLETED);
                    persistDiscovery(state);
                    return;
                }
            }
            localFileService.scanAfter(discoveryBinding.getLocalPath(), state.getLastCommittedRelativeKey(), item -> {
                controlService.checkRunning(request.getParentTaskId());
                fileReportLedgerService.discoveryItem(request, item, distributedConfig.getLocalIp());
                LocalFileEntry entry = new LocalFileEntry(item.getRelativeKey(), item.getSize(), item.getLastModified());
                if (balanced && !entries.isEmpty() && item.getSize() > BalancedWorkerScheduler.MAX_DATA_BYTES - dataBytes.get()) {
                    journalAndDispatch(state, bindings, sequence.getAndIncrement(), new ArrayList<>(entries), false);
                    entries.clear(); bytes.set(0); dataBytes.set(0);
                }
                entries.add(entry);
                dataBytes.addAndGet(Math.max(0, item.getSize()));
                bytes.addAndGet(item.getRelativeKey().getBytes(StandardCharsets.UTF_8).length + 32);
                if (entries.size() >= Math.max(1, balanced
                        ? Math.min(BalancedWorkerScheduler.MAX_ITEMS, localTransferConfig.getBatch().getMaxItems())
                        : localTransferConfig.getBatch().getMaxItems())
                        || (balanced && dataBytes.get() >= BalancedWorkerScheduler.MAX_DATA_BYTES)
                        || bytes.get() >= Math.max(1024, localTransferConfig.getBatch().getMaxBytes())) {
                    int batchSequence = sequence.getAndIncrement();
                    journalAndDispatch(state, bindings, batchSequence, new ArrayList<>(entries), false);
                    entries.clear();
                    bytes.set(0); dataBytes.set(0);
                }
            });
            if (!entries.isEmpty() || sequence.get() == 0) {
                int batchSequence = sequence.getAndIncrement();
                journalAndDispatch(state, bindings, batchSequence, new ArrayList<>(entries), true);
            }
            fileReportLedgerService.discoveryCompleted(request.getParentTaskId());
            state.setStatus(LocalDiscoveryState.Status.COMPLETED);
            persistDiscovery(state);
        } catch (TaskCancelledException e) {
            state.setStatus(LocalDiscoveryState.Status.STOPPED);
            state.setErrorMessage(e.getMessage());
            persistDiscovery(state);
        } catch (Exception e) {
            if (controlService.isStopped(request.getParentTaskId()) || Thread.currentThread().isInterrupted()) {
                state.setStatus(LocalDiscoveryState.Status.STOPPED);
                persistDiscovery(state);
                return;
            }
            recordLedgerScanFailure(request.getParentTaskId(), e);
            state.setStatus(LocalDiscoveryState.Status.FAILED);
            state.setErrorMessage(e.getMessage());
            persistDiscovery(state);
            recordDiscoveryFailure(request, discoveryBinding, e);
        }
    }

    private void recordLedgerScanFailure(Integer taskId, Exception error) {
        try {
            fileReportLedgerService.scanFailed(taskId, error.getMessage());
        } catch (RuntimeException reportError) {
            error.addSuppressed(reportError);
        }
    }

    private void recordDiscoveryFailure(LocalTransferRequest request, LocalNodeBinding binding, Exception error) {
        FailedKeyMessage failure = new FailedKeyMessage();
        failure.setTaskIndex(request.getParentTaskId());
        failure.setParentTaskId(request.getParentTaskId());
        failure.setShardId("discovery");
        failure.setAttempt(0);
        failure.setDirection(request.getDirection());
        failure.setSourceEndpoint(request.getSourceEndpoint());
        failure.setDestinationEndpoint(request.getDestinationEndpoint());
        failure.setStorageId(localEndpoint(request).getStorageId());
        failure.setEligibleWorkerNodes(nodeIps(localEndpoint(request)));
        failure.setLocalPath(binding.getLocalPath());
        failure.setExecutionNodeIp(binding.getExecutionNodeIp());
        failure.setWorkerNode(distributedConfig.getLocalIp());
        failure.setKey(binding.getLocalPath());
        failure.setObjectSize(0L);
        failure.setStage(FailureStage.SCAN);
        failure.setErrorCode("SOURCE_DISCOVERY_FAILED");
        failure.setErrorMessage(error.getMessage());
        failure.setRetryCount(0);
        try {
            bufferQueueManager.putFailedKey(failure);
        } catch (InterruptedException interrupted) {
            Thread.currentThread().interrupt();
        }
    }

    private void persistDiscovery(LocalDiscoveryState state) {
        state.setUpdatedAt(System.currentTimeMillis());
        rocksDBService.putSync(discoveryKey(state.getParentTaskId()), state);
    }

    private String discoveryKey(Integer parentTaskId) {
        return DISCOVERY_PREFIX + parentTaskId;
    }

    private void journalAndDispatch(LocalDiscoveryState state, List<LocalNodeBinding> bindings,
                                    int sequence, List<LocalFileEntry> entries, boolean last) {
        controlService.checkRunning(state.getParentTaskId());
        state.setPendingBatch(createBatch(state.getRequest(), bindings, sequence, entries, last));
        persistDiscovery(state);
        dispatchPendingBatch(state);
    }

    private void dispatchPendingBatch(LocalDiscoveryState state) {
        controlService.checkRunning(state.getParentTaskId());
        LocalFileBatch pending = state.getPendingBatch();
        dispatchExactBatch(pending);
        List<LocalFileEntry> entries = pending.getEntries();
        if (!entries.isEmpty()) state.setLastCommittedRelativeKey(entries.get(entries.size() - 1).getRelativeKey());
        state.setNextBatchSequence(pending.getShardIndex() + 1);
        state.setPendingBatch(null);
        persistDiscovery(state);
    }

    void dispatchBatch(LocalTransferRequest request, List<LocalNodeBinding> bindings,
                       int sequence, List<LocalFileEntry> entries, boolean last) {
        dispatchExactBatch(createBatch(request, bindings, sequence, entries, last));
    }

    private LocalFileBatch createBatch(LocalTransferRequest request, List<LocalNodeBinding> bindings,
                                      int sequence, List<LocalFileEntry> entries, boolean last) {
        int targetIndex = DistributionMode.BALANCED.getValue().equals(request.getDistributionMode())
                ? sequence % bindings.size() : Math.floorMod(request.getParentTaskId(), bindings.size());
        LocalNodeBinding target = bindings.get(targetIndex);
        if (DistributionMode.BALANCED.getValue().equals(request.getDistributionMode()) && balancedWorkerScheduler != null) {
            List<String> workers = new ArrayList<>();
            for (LocalNodeBinding binding : bindings) workers.add(binding.getExecutionNodeIp());
            long dataBytes = 0;
            for (LocalFileEntry entry : entries) dataBytes += entry.getSize() == null ? 0 : Math.max(0, entry.getSize());
            String selected = balancedWorkerScheduler.select(workers, dataBytes, entries.size());
            for (LocalNodeBinding binding : bindings) if (selected.equals(binding.getExecutionNodeIp())) target = binding;
        }
        return new LocalFileBatch(request.getParentTaskId(),
                request.getParentTaskId() + "-local-" + sequence, sequence, 0,
                distributedConfig.getLocalIp(), request, target, entries, last);
    }

    private void dispatchExactBatch(LocalFileBatch batch) {
        if (nodeConfigService == null) { dispatchRegisteredBatch(batch); return; }
        nodeConfigService.withMembershipReadLock(() -> { dispatchRegisteredBatch(batch); return null; });
    }

    private void dispatchRegisteredBatch(LocalFileBatch batch) {
        LocalNodeBinding target = batch.getNodeBinding();
        if (distributedConfig.getLocalIp().equals(target.getExecutionNodeIp())) {
            acceptBatch(batch);
            return;
        }
        try {
            ResponseEntity<Map> response = restTemplate.postForEntity("http://" + target.getExecutionNodeIp()
                    + ":" + distributedConfig.getPort() + "/api/worker/local-transfers/batches", batch, Map.class);
            Map body = response.getBody();
            if (!response.getStatusCode().is2xxSuccessful() || body == null
                    || !Boolean.TRUE.equals(body.get("success"))) {
                String code = body != null && body.get("code") != null
                        ? String.valueOf(body.get("code")) : "WORKER_UNAVAILABLE";
                String message = body != null && body.get("message") != null
                        ? String.valueOf(body.get("message")) : "worker rejected local batch";
                throw new DownstreamWorkerException(code, message, null);
            }
        } catch (DownstreamWorkerException e) {
            throw e;
        } catch (Exception e) {
            throw new DownstreamWorkerException("WORKER_UNAVAILABLE",
                    "failed to dispatch local batch to " + target.getExecutionNodeIp(), e);
        }
    }

    public synchronized Integer acceptBatch(LocalFileBatch batch) {
        if (batch == null || batch.getRequest() == null || batch.getNodeBinding() == null
                || batch.getEntries() == null) {
            throw new TransferValidationException("INVALID_ENDPOINT", "local file batch is incomplete");
        }
        LocalTransferRequest request = batch.getRequest();
        normalizationService.normalizeAndValidate(request);
        String localIp = distributedConfig.getLocalIp();
        if (!localIp.equals(batch.getNodeBinding().getExecutionNodeIp())) {
            throw new TransferValidationException("SHARD_ASSIGNMENT_MISMATCH", "batch is assigned to another worker");
        }
        LocalNodeBinding declared = normalizationService.bindingFor(localEndpoint(request), localIp);
        if (!declared.getLocalPath().equals(batch.getNodeBinding().getLocalPath())) {
            throw new TransferValidationException("SHARD_ASSIGNMENT_MISMATCH", "batch path does not match declared binding");
        }
        String assignmentKey = BATCH_ASSIGNMENT_PREFIX + batch.getParentTaskId() + ":" + batch.getShardId();
        Integer existingTaskId = rocksDBService.get(assignmentKey, Integer.class);
        if (existingTaskId != null) {
            subTaskManagerService.ensureTaskIndexed(existingTaskId);
            SubTaskStatus existing = subTaskManagerService.getTaskStatus(existingTaskId);
            if (existing != null && existing.getStatus() == SubTaskStatus.TaskStatus.PENDING
                    && !controlService.isActive(existingTaskId) && !controlService.isStopped(batch.getParentTaskId())) {
                bufferQueueManager.offerListTask(existing);
            }
            return existingTaskId;
        }
        controlService.checkRunning(batch.getParentTaskId());
        localFileService.validateSource(declared.getLocalPath());
        ListSubTask task = toSubTask(request, declared);
        task.setParentTaskId(batch.getParentTaskId());
        task.setShardId(batch.getShardId());
        task.setShardIndex(batch.getShardIndex());
        task.setAttempt(batch.getAttempt());
        task.setLocalFileEntries(batch.getEntries());
        Integer taskId = subTaskManagerService.saveAssignedLocalTask(task);
        rocksDBService.putSync(assignmentKey, taskId);
        SubTaskStatus status = subTaskManagerService.getTaskStatus(taskId);
        if (status != null && status.getStatus() == SubTaskStatus.TaskStatus.PENDING) {
            bufferQueueManager.offerListTask(status);
        }
        return taskId;
    }

    private List<String> nodeIps(TransferEndpoint endpoint) {
        List<String> result = new ArrayList<>();
        for (LocalNodeBinding binding : endpoint.getNodeBindings()) result.add(binding.getExecutionNodeIp());
        return result;
    }

    @PreDestroy
    public void shutdownDiscovery() {
        discoveryExecutor.shutdownNow();
    }

    public synchronized Map<String, Object> stop(List<Integer> taskIds) {
        int accepted = 0;
        int ignored = 0;
        Set<Integer> parentIds = new LinkedHashSet<>();
        if (taskIds != null) parentIds.addAll(taskIds);
        if (parentIds.isEmpty()) {
            for (SubTaskStatus task : localTasks()) {
                if (task.getParentTaskId() != null) parentIds.add(task.getParentTaskId());
            }
        }
        {
            for (Integer parentId : parentIds) {
                controlService.stop(parentId);
                LocalDiscoveryState state = rocksDBService.get(discoveryKey(parentId), LocalDiscoveryState.class);
                if (state != null && state.getStatus() != LocalDiscoveryState.Status.COMPLETED) {
                    state.setStatus(LocalDiscoveryState.Status.STOPPED);
                    persistDiscovery(state);
                }
            }
        }
        List<Integer> targets = taskIds == null || taskIds.isEmpty()
                ? localTaskIds() : resolveTaskIds(taskIds);
        for (Integer id : targets) {
            SubTaskStatus task = subTaskManagerService.getTaskStatus(id);
            if (task == null || task.getDirection() == TransferDirection.OSS_TO_OSS
                    || task.getStatus() == SubTaskStatus.TaskStatus.COMPLETED) {
                ignored++;
                continue;
            }
            if (subTaskManagerService.stopTask(id)) accepted++;
            else ignored++;
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("accepted", accepted);
        result.put("ignored", ignored);
        result.put("failed", 0);
        return result;
    }

    public Map<String, Object> resume(List<Integer> taskIds) { return resume(taskIds, false); }

    public synchronized Map<String, Object> resume(List<Integer> taskIds, boolean deferDiscovery) {
        int accepted = 0, ignored = 0, failed = 0;
        List<Integer> targets = taskIds == null || taskIds.isEmpty() ? localTaskIds() : resolveTaskIds(taskIds);
        Set<Integer> parents = new LinkedHashSet<>();
        for (Integer id : targets) {
            SubTaskStatus task = subTaskManagerService.getTaskStatus(id);
            if (task != null && task.getParentTaskId() != null) parents.add(task.getParentTaskId());
        }
        if (taskIds != null) parents.addAll(taskIds);
        Set<Integer> blockedParents = new HashSet<>();
        for (Integer parent : parents) {
            try {
                resumeDiscovery(parent, deferDiscovery);
            } catch (RuntimeException error) { failed++; blockedParents.add(parent); }
        }
        // Re-read after clearing parent fences: imports before that point are now included;
        // later imports observe an active parent and are admitted as PENDING.
        targets = taskIds == null || taskIds.isEmpty() ? localTaskIds() : resolveTaskIds(taskIds);
        for (Integer id : targets) {
            try {
                SubTaskStatus task = subTaskManagerService.getTaskStatus(id);
                if (task != null && blockedParents.contains(task.getParentTaskId())) { failed++; continue; }
                if (subTaskManagerService.resumeTask(id)) accepted++; else ignored++;
            } catch (RuntimeException error) { failed++; }
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("accepted", accepted); result.put("ignored", ignored); result.put("failed", failed);
        return result;
    }

    public Map<String, Object> retry(List<Integer> taskIds) {
        return retry(taskIds, Collections.emptyList());
    }

    public Map<String, Object> retry(List<Integer> taskIds, List<String> itemKeys) {
        int accepted = 0;
        int ignored = 0;
        int failed = 0;
        if (taskIds != null) {
            for (Integer parentId : taskIds) {
                resumeDiscovery(parentId, false);
            }
        }
        List<Integer> targets = taskIds == null || taskIds.isEmpty()
                ? localTaskIds() : resolveTaskIds(taskIds);
        for (Integer id : targets) {
            try {
                synchronized (subTaskManagerService) {
                    SubTaskStatus task = subTaskManagerService.getTaskStatus(id);
                    if (task == null || task.getDirection() == TransferDirection.OSS_TO_OSS) {
                        failed++;
                        continue;
                    }
                    synchronized (controlService) {
                        requireNotPurged(task.getParentTaskId());
                        requireNotPurged(id);
                        if (controlService.isActive(id)) throw new IllegalStateException("task is still running");
                        if (task.getStatus() == SubTaskStatus.TaskStatus.COMPLETED
                                || (task.getStatus() == SubTaskStatus.TaskStatus.PENDING && !controlService.isStopped(id))) {
                            ignored++;
                            continue;
                        }
                        controlService.selectItems(id, itemKeys);
                    }
                    // Recovery deliberately skips STOPPED tasks; an explicit retry must use
                    // the resume transition and count only a successfully queued attempt.
                    if (!subTaskManagerService.resumeTask(id)) {
                        ignored++;
                        continue;
                    }
                    // Preserve failures when queue admission fails. List execution takes
                    // this same monitor, so new failures cannot race this cache cleanup.
                    if (itemKeys == null || itemKeys.isEmpty()) {
                        subTaskManagerService.removeFailedKeysByTaskIndex(id);
                    } else {
                        subTaskManagerService.removeFailedKeysByTaskIndex(id, new HashSet<>(itemKeys));
                    }
                    accepted++;
                }
            } catch (Exception e) {
                failed++;
            }
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("accepted", accepted);
        result.put("ignored", ignored);
        result.put("failed", failed);
        return result;
    }

    public List<SubTaskStatus> localTasks() {
        List<SubTaskStatus> result = new ArrayList<>();
        for (Integer id : subTaskManagerService.getTaskIdList()) {
            SubTaskStatus status = subTaskManagerService.getTaskStatus(id);
            if (status != null && status.getDirection() != TransferDirection.OSS_TO_OSS) {
                updateSpeeds(status);
                result.add(status);
            }
        }
        for (LocalDiscoveryState discovery : rocksDBService.scanByPrefix(DISCOVERY_PREFIX, LocalDiscoveryState.class)) {
            result.add(discoveryStatus(discovery));
        }
        return result;
    }

    private SubTaskStatus discoveryStatus(LocalDiscoveryState discovery) {
        LocalTransferRequest request = discovery.getRequest();
        SubTaskStatus status = new SubTaskStatus();
        status.setTaskIndex(discovery.getParentTaskId());
        status.setParentTaskId(discovery.getParentTaskId());
        status.setShardId("discovery");
        status.setDirection(request.getDirection());
        status.setSourceEndpoint(request.getSourceEndpoint());
        status.setDestinationEndpoint(request.getDestinationEndpoint());
        status.setConflictPolicy(request.getConflictPolicy());
        status.setDistributionMode(request.getDistributionMode());
        TransferEndpoint local = localEndpoint(request);
        status.setStorageId(local.getStorageId());
        status.setNodeBindings(local.getNodeBindings());
        status.setEligibleWorkerNodes(nodeIps(local));
        status.setWorkerNode(discovery.getDiscoveryWorker());
        LocalNodeBinding binding = normalizationService.bindingFor(local, discovery.getDiscoveryWorker());
        status.setExecutionNodeIp(binding.getExecutionNodeIp());
        status.setLocalPath(binding.getLocalPath());
        status.setMarker(discovery.getLastCommittedRelativeKey());
        status.setStatus(discoveryTaskStatus(discovery.getStatus()));
        status.setCreateTime(discovery.getUpdatedAt());
        status.setStartTime(discovery.getStatus() == LocalDiscoveryState.Status.PENDING ? null : discovery.getUpdatedAt());
        status.setListObjectCount(0L); status.setListObjectSize(0L);
        status.setPutObjectCount(0L); status.setPutObjectSize(0L);
        status.setSkippedObjectCount(0L); status.setSkippedObjectSize(0L);
        status.setFailedObjectCount(discovery.getStatus() == LocalDiscoveryState.Status.FAILED ? 1L : 0L);
        status.setFailedObjectSize(0L);
        status.setCurrentSpeedBytesPerSecond(0L); status.setAverageSpeedBytesPerSecond(0L);
        status.setEndTime(discovery.getStatus() == LocalDiscoveryState.Status.COMPLETED
                || discovery.getStatus() == LocalDiscoveryState.Status.FAILED
                || discovery.getStatus() == LocalDiscoveryState.Status.STOPPED ? discovery.getUpdatedAt() : null);
        return status;
    }

    private SubTaskStatus.TaskStatus discoveryTaskStatus(LocalDiscoveryState.Status status) {
        if (status == null || status == LocalDiscoveryState.Status.PENDING) return SubTaskStatus.TaskStatus.PENDING;
        if (status == LocalDiscoveryState.Status.RUNNING) return SubTaskStatus.TaskStatus.RUNNING;
        if (status == LocalDiscoveryState.Status.COMPLETED) return SubTaskStatus.TaskStatus.COMPLETED;
        if (status == LocalDiscoveryState.Status.STOPPED) return SubTaskStatus.TaskStatus.STOPPED;
        return SubTaskStatus.TaskStatus.FAILED;
    }

    private List<Integer> localTaskIds() {
        List<Integer> ids = new ArrayList<>();
        for (SubTaskStatus task : localTasks()) {
            if (!"discovery".equals(task.getShardId())) ids.add(task.getTaskIndex());
        }
        return ids;
    }

    private List<Integer> resolveTaskIds(List<Integer> requested) {
        Set<Integer> result = new LinkedHashSet<>();
        for (SubTaskStatus task : localTasks()) {
            if ("discovery".equals(task.getShardId())) continue;
            if (requested.contains(task.getTaskIndex()) || (task.getParentTaskId() != null
                    && requested.contains(task.getParentTaskId()))) {
                result.add(task.getTaskIndex());
            }
        }
        return new ArrayList<>(result);
    }

    private void updateSpeeds(SubTaskStatus status) {
        long bytes = status.getPutObjectSize() == null ? 0 : status.getPutObjectSize();
        long end = status.getEndTime() == null ? System.currentTimeMillis() : status.getEndTime();
        long durationMillis = status.getStartTime() == null ? 0 : Math.max(1, end - status.getStartTime());
        status.setAverageSpeedBytesPerSecond(durationMillis == 0 ? 0 : bytes * 1000 / durationMillis);
        status.setCurrentSpeedBytesPerSecond(transferSpeedService.currentBytesPerSecond(status.getTaskIndex()));
    }

    private TransferEndpoint localEndpoint(LocalTransferRequest request) {
        return request.getDirection() == TransferDirection.LOCAL_TO_OSS
                ? request.getSourceEndpoint() : request.getDestinationEndpoint();
    }

    private TransferEndpoint ossEndpoint(LocalTransferRequest request) {
        return request.getDirection() == TransferDirection.OSS_TO_LOCAL
                ? request.getSourceEndpoint() : request.getDestinationEndpoint();
    }
}
