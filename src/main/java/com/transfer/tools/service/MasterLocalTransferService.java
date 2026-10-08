package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.amazonaws.services.s3.AmazonS3;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.config.RateLimitConfig;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.agent.AgentRegistryService;
import com.transfer.tools.agent.AgentUnavailableException;
import com.transfer.tools.model.*;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.*;
import org.springframework.http.client.ClientHttpRequestInterceptor;
import org.springframework.http.client.SimpleClientHttpRequestFactory;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;
import org.springframework.web.client.HttpStatusCodeException;
import org.springframework.boot.context.event.ApplicationReadyEvent;
import org.springframework.context.event.EventListener;

import java.io.UnsupportedEncodingException;
import java.net.URLEncoder;
import java.util.*;

@Service
public class MasterLocalTransferService {
    @Autowired private DistributedConfig distributedConfig;
    @Autowired private NodeConfigService nodeConfigService;
    @Autowired private EndpointNormalizationService normalizationService;
    @Autowired private SubTaskManagerService subTaskManagerService;
    @Autowired private S3ClientManager s3ClientManager;
    @Autowired private BucketExploreService bucketExploreService;
    @Autowired private RateLimitConfig rateLimitConfig;
    @Autowired private BufferQueueManager bufferQueueManager;
    @Autowired private LocalParentTaskService parentTaskService;
    @Autowired private MasterTaskBandwidthService taskBandwidth;
    @Autowired private AgentRegistryService agentRegistry;
    private final Set<Integer> deferredRecoveries = java.util.concurrent.ConcurrentHashMap.newKeySet();
    private final Set<Integer> dispatchedRecoveries = java.util.concurrent.ConcurrentHashMap.newKeySet();

    private final ObjectMapper objectMapper = new ObjectMapper();
    private final RestTemplate restTemplate;

    public MasterLocalTransferService() {
        SimpleClientHttpRequestFactory factory = new SimpleClientHttpRequestFactory();
        factory.setConnectTimeout(3000);
        factory.setReadTimeout(10000);
        restTemplate = new RestTemplate(factory);
        // 附加节点间内部认证头，使主节点调用 worker 接口时通过 AuthInterceptor 校验
        restTemplate.getInterceptors().add((ClientHttpRequestInterceptor) (request, body, execution) -> {
            String token = distributedConfig.getInternalToken();
            if (token != null && !token.isEmpty()) {
                request.getHeaders().set("X-Internal-Token", token);
            }
            return execution.execute(request, body);
        });
    }

    public Map<String, Object> nodes() {
        List<Map<String, Object>> nodes = new ArrayList<>();
        for (String worker : workers()) {
            Map<String, Object> node = new LinkedHashMap<>();
            node.put("executionNodeIp", worker);
            try {
                Map response = restTemplate.getForObject(url(worker, "/api/worker/local-capabilities"), Map.class);
                node.put("reachable", true);
                node.put("localTransferSupported", true);
                Object data = response == null ? null : response.get("data");
                if (data instanceof Map) {
                    node.putAll((Map<String, Object>) data);
                }
            } catch (Exception e) {
                node.put("reachable", false);
                node.put("localTransferSupported", false);
                node.put("advertisedLocalRoots", Collections.emptyList());
            }
            nodes.add(node);
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("nodes", nodes);
        return result;
    }

    public Map<String, Object> create(List<LocalTransferRequest> requests) {
        if (nodeConfigService == null) return createWithMembership(requests);
        return nodeConfigService.withMembershipReadLock(() -> createWithMembership(requests));
    }

    private Map<String, Object> createWithMembership(List<LocalTransferRequest> requests) {
        if (requests == null || requests.isEmpty()) {
            throw new TransferValidationException("INVALID_ENDPOINT", "at least one transfer request is required");
        }
        for (LocalTransferRequest request : requests) {
            if (request != null && request.getTaskBandwidthScopeId() != null)
                throw new TransferValidationException("INVALID_TASK_BANDWIDTH", "带宽范围由服务端分配");
            validateRequest(request);
        }
        List<LocalParentTaskRecord> parentRecords = parentTaskService.create(requests);
        List<Integer> ids = new ArrayList<>();
        List<Map<String, Object>> acceptedRequests = new ArrayList<>();
        RuntimeException firstDispatchFailure = null;
        for (LocalParentTaskRecord parentRecord : parentRecords) {
            LocalTransferRequest request = parentRecord.getRequest();
            ids.add(request.getTaskId());
            try {
                dispatchParent(request);
            } catch (RuntimeException e) {
                parentTaskService.markFailed(request.getParentTaskId(), errorCode(e), e.getMessage());
                if (firstDispatchFailure == null) firstDispatchFailure = e;
            }
            Map<String, Object> accepted = new LinkedHashMap<>();
            accepted.put("parentTaskId", request.getParentTaskId());
            accepted.put("storageId", localEndpoint(request).getStorageId());
            accepted.put("distributionMode", request.getDistributionMode());
            accepted.put("nodeBindings", localEndpoint(request).getNodeBindings());
            acceptedRequests.add(accepted);
        }
        if (firstDispatchFailure != null) throw firstDispatchFailure;
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("acceptedCount", ids.size());
        result.put("rejectedCount", 0);
        result.put("taskIds", ids);
        result.put("parentTaskIds", ids);
        result.put("acceptedNodeBindings", acceptedRequests);
        result.put("rejectedNodeBindings", Collections.emptyList());
        result.put("validationErrors", Collections.emptyList());
        return result;
    }

    @EventListener(ApplicationReadyEvent.class)
    public synchronized void recoverPendingParents() {
        if (!distributedConfig.isMaster()) return;
        for (LocalParentTaskRecord record : parentTaskService.recoverable()) {
            recoverParent(record);
        }
    }

    /** Retry only startup work proven to have been deferred before sending a task command. */
    public synchronized void retryDeferredParents() {
        if (!distributedConfig.isMaster()) return;
        for (Integer id : new ArrayList<>(deferredRecoveries)) {
            LocalParentTaskRecord record = parentTaskService.get(id);
            if (record == null || parentTaskService.isDeleted(id) || parentTaskService.isStopped(id)
                    || Boolean.TRUE.equals(record.getOrchestrationComplete())
                    || !(record.getStatus() == SubTaskStatus.TaskStatus.PENDING || record.getStatus() == SubTaskStatus.TaskStatus.RUNNING)) {
                deferredRecoveries.remove(id);
                continue;
            }
            recoverParent(record);
        }
    }

    private void recoverParent(LocalParentTaskRecord record) {
        Integer id = record.getParentTaskId();
        if (dispatchedRecoveries.contains(id)) { deferredRecoveries.remove(id); return; }
        if (!recoveryAgentsReady(record.getRequest())) { deferredRecoveries.add(id); return; }
        dispatchedRecoveries.add(id);
        try {
            dispatchParent(record.getRequest());
            deferredRecoveries.remove(id);
        } catch (RuntimeException error) {
            if (beforeAgentDelivery(error) || (error instanceof TransferValidationException
                    && "TASK_BANDWIDTH_NOT_READY".equals(((TransferValidationException)error).getCode())
                    && !recoveryAgentsReady(record.getRequest()))) {
                dispatchedRecoveries.remove(id);
                deferredRecoveries.add(id);
                if (record.getStatus() == SubTaskStatus.TaskStatus.PENDING) parentTaskService.deferUnstartedRecovery(id);
            } else {
                deferredRecoveries.remove(id);
                parentTaskService.markFailed(id, errorCode(error), error.getMessage());
            }
        }
    }

    private boolean recoveryAgentsReady(LocalTransferRequest request) {
        if (agentRegistry == null || request == null) return true;
        TransferEndpoint local = localEndpoint(request);
        if (local == null || local.getNodeBindings() == null) return true;
        for (LocalNodeBinding binding : local.getNodeBindings())
            if (agentRegistry.isAgent(binding.getExecutionNodeIp()) && !agentRegistry.online(binding.getExecutionNodeIp())) return false;
        return true;
    }

    private static boolean beforeAgentDelivery(Throwable error) {
        for (Throwable cause = error; cause != null; cause = cause.getCause())
            if (cause instanceof AgentUnavailableException) return true;
        return false;
    }

    private void dispatchParent(LocalTransferRequest request) {
        if (taskBandwidth != null) taskBandwidth.prepareLocal(request);
        if (request.getDirection() == TransferDirection.OSS_TO_LOCAL) {
            prepareLegacyCheckpoint(parentTaskService.get(request.getParentTaskId()));
            BucketPair pair = toBucketPair(request);
            if (request.getSourceEndpoint().getObjectKey() != null
                    && !request.getSourceEndpoint().getObjectKey().isEmpty()) {
                if (!parentTaskService.markRunning(request.getParentTaskId())) return;
                bucketExploreService.distributeExactLocalObject(pair);
                parentTaskService.markOrchestrationComplete(request.getParentTaskId());
            } else if (!bufferQueueManager.offerExploreTask(pair)) {
                throw new DownstreamWorkerException("QUEUE_FULL", "local transfer explore queue is full", null);
            }
        } else {
            if (!parentTaskService.markRunning(request.getParentTaskId())) return;
            dispatchDiscovery(request);
            parentTaskService.markOrchestrationComplete(request.getParentTaskId());
        }
    }

    /** Import old acknowledged ranges once instead of reusing their shard IDs for a new scan. */
    void prepareLegacyCheckpoint(LocalParentTaskRecord record) {
        if (record == null || Boolean.TRUE.equals(record.getOrchestrationComplete())
                || record.getStartTime() == null || parentTaskService.exploreCheckpoint(record.getParentTaskId()) != null)
            return;
        LocalTransferRequest request = record.getRequest();
        if (request.getDirection() != TransferDirection.OSS_TO_LOCAL) return;
        Map<Integer, SubTaskStatus> ranges = new TreeMap<>();
        Set<String> queried = new LinkedHashSet<>();
        for (LocalNodeBinding binding : localEndpoint(request).getNodeBindings()) queried.add(binding.getExecutionNodeIp());
        for (String worker : queried) {
            try {
                Map response = restTemplate.getForObject(url(worker,
                        "/api/worker/local-transfers/status?taskId=" + record.getParentTaskId()), Map.class);
                Object raw = response == null ? null : response.get("data");
                if (response == null || !Boolean.TRUE.equals(response.get("success")) || !(raw instanceof Map)
                        || !(((Map) raw).get("tasks") instanceof Collection))
                    throw new IllegalStateException("worker did not return its saved ranges");
                for (Object item : (Collection<?>) ((Map) raw).get("tasks")) {
                    SubTaskStatus shard = objectMapper.convertValue(item, SubTaskStatus.class);
                    if (!record.getParentTaskId().equals(shard.getParentTaskId())) continue;
                    if (shard.getShardIndex() == null || shard.getShardIndex() < 0
                            || ranges.put(shard.getShardIndex(), shard) != null)
                        throw new TransferValidationException("INVALID_RESUME_CHECKPOINT", "已保存子任务编号不唯一，无法安全恢复扫描");
                }
            } catch (TransferValidationException e) { throw e; }
            catch (Exception e) {
                throw new DownstreamWorkerException("WORKER_UNAVAILABLE",
                        "无法读取节点 " + worker + " 的扫描检查点，尚未恢复扫描", e);
            }
        }
        if (ranges.isEmpty()) return;
        String objectKey = request.getSourceEndpoint().getObjectKey();
        String prefix = request.getSourceEndpoint().getPrefix();
        String boundary = objectKey != null && !objectKey.isEmpty() ? "" : prefix == null ? "" : prefix.trim();
        int next = 0;
        boolean complete = false;
        for (Map.Entry<Integer, SubTaskStatus> entry : ranges.entrySet()) {
            SubTaskStatus shard = entry.getValue();
            String start = shard.getStartKey() == null ? "" : shard.getStartKey();
            String end = shard.getEndKey() == null ? "" : shard.getEndKey();
            if (entry.getKey() != next || complete || !start.equals(boundary)
                    || (!end.isEmpty() && end.compareTo(start) <= 0))
                throw new TransferValidationException("INVALID_RESUME_CHECKPOINT", "已保存子任务范围不连续，无法安全恢复扫描");
            boundary = end;
            complete = end.isEmpty();
            next++;
        }
        LocalExploreCheckpoint checkpoint = new LocalExploreCheckpoint();
        checkpoint.setMarker(boundary); checkpoint.setNextShardIndex(next); checkpoint.setComplete(complete);
        if (parentTaskService.isDeleted(record.getParentTaskId()))
            throw new java.util.concurrent.CancellationException("parent transfer has been deleted");
        parentTaskService.saveExploreCheckpoint(record.getParentTaskId(), checkpoint);
    }

    private String errorCode(RuntimeException error) {
        if (error instanceof TransferValidationException) return ((TransferValidationException) error).getCode();
        if (error instanceof DownstreamWorkerException) return ((DownstreamWorkerException) error).getCode();
        return "INTERNAL_ERROR";
    }

    public Map<String, Object> validate(List<LocalTransferRequest> requests) {
        if (requests == null || requests.isEmpty()) {
            throw new TransferValidationException("INVALID_ENDPOINT", "at least one transfer request is required");
        }
        List<Map<String, Object>> results = new ArrayList<>();
        boolean valid = true;
        for (LocalTransferRequest request : requests) {
            validateBase(request);
            String sourceKind = null;
            for (LocalNodeBinding binding : localEndpoint(request).getNodeBindings()) {
                Map<String, Object> item = new LinkedHashMap<>();
                item.put("executionNodeIp", binding.getExecutionNodeIp());
                item.put("localPath", binding.getLocalPath());
                item.put("storageId", localEndpoint(request).getStorageId());
                item.put("distributionMode", request.getDistributionMode());
                try {
                    requireWorker(binding.getExecutionNodeIp());
                    Map<String, Object> workerResult = callValidate(binding.getExecutionNodeIp(), request);
                    item.putAll(workerResult);
                    item.put("valid", true);
                    if (request.getDirection() == TransferDirection.LOCAL_TO_OSS) {
                        String kind = validatedUploadSourceKind(request, workerResult);
                        if (sourceKind == null) {
                            sourceKind = kind;
                        } else if (!sourceKind.equals(kind)) {
                            item.put("valid", false);
                            item.put("code", "INVALID_ENDPOINT");
                            item.put("message", "LOCAL bindings do not expose the same source kind");
                            valid = false;
                        }
                    }
                } catch (TransferValidationException e) {
                    item.put("valid", false);
                    item.put("code", e.getCode());
                    item.put("message", e.getMessage());
                    valid = false;
                } catch (DownstreamWorkerException e) {
                    item.put("valid", false);
                    item.put("code", e.getCode());
                    item.put("message", e.getMessage());
                    valid = false;
                }
                results.add(item);
            }
        }
        Map<String, Object> data = new LinkedHashMap<>();
        data.put("valid", valid);
        data.put("nodeBindings", results);
        return data;
    }

    private void validateRequest(LocalTransferRequest request) {
        validateBase(request);
        String sourceKind = null;
        for (LocalNodeBinding binding : localEndpoint(request).getNodeBindings()) {
            requireWorker(binding.getExecutionNodeIp());
            Map<String, Object> result = callValidate(binding.getExecutionNodeIp(), request);
            if (request.getDirection() == TransferDirection.LOCAL_TO_OSS) {
                String kind = validatedUploadSourceKind(request, result);
                if (sourceKind == null) sourceKind = kind;
                else if (!sourceKind.equals(kind)) {
                    throw new TransferValidationException("INVALID_ENDPOINT",
                            "LOCAL bindings do not expose the same source kind");
                }
            }
        }
    }

    private String validatedUploadSourceKind(LocalTransferRequest request, Map<String,Object> workerResult) {
        String kind = String.valueOf(workerResult.get("pathType"));
        if (!"FILE".equals(kind) && !"DIRECTORY".equals(kind))
            throw new TransferValidationException("SOURCE_TYPE_UNAVAILABLE", "无法确认源路径是文件还是目录");
        if ("FILE".equals(kind) && request.getSourceEndpoint().getNodeBindings().size() != 1)
            throw new TransferValidationException("SINGLE_FILE_REQUIRES_ONE_NODE", "单文件上传请只保留一个执行节点");
        return kind;
    }

    private void validateBase(LocalTransferRequest request) {
        MasterTaskBandwidthService.validateLimit(request == null ? null : request.getTaskBandwidthLimitMbps());
        normalizationService.normalizeAndValidate(request);
        TransferEndpoint local = localEndpoint(request);
        // This method is only used for new-task preflight/create. Saved task recovery
        // retains its original distribution mode and immutable member set.
        if (request.getDirection() == TransferDirection.LOCAL_TO_OSS) {
            request.setDistributionMode(local.getNodeBindings().size() == 1
                    ? DistributionMode.AFFINITY.getValue() : DistributionMode.BALANCED.getValue());
        }
        applyClusterRateShares(request, local.getNodeBindings().size());
        TransferEndpoint oss = request.getDirection() == TransferDirection.OSS_TO_LOCAL
                ? request.getSourceEndpoint() : request.getDestinationEndpoint();
        AmazonS3 s3 = s3ClientManager.getClient(oss.getS3Alias());
        if (s3 == null) {
            throw new TransferValidationException("S3_CONFIG_NOT_FOUND", "S3 alias is not configured: " + oss.getS3Alias());
        }
        try {
            if (!s3.doesBucketExistV2(oss.getBucket())) {
                throw new TransferValidationException("S3_BUCKET_UNAVAILABLE", "S3 bucket is unavailable: " + oss.getBucket());
            }
        } catch (TransferValidationException e) {
            throw e;
        } catch (Exception e) {
            throw new TransferValidationException("S3_BUCKET_UNAVAILABLE", "cannot access S3 bucket: " + oss.getBucket());
        }
    }

    private void applyClusterRateShares(LocalTransferRequest request, int workerCount) {
        int count = Math.max(1, workerCount);
        if (rateLimitConfig.getSrc() != null && rateLimitConfig.getSrc().getQps() != null
                && rateLimitConfig.getSrc().getQps().getList() != null) {
            request.setListQpsLimit(Math.max(0.1, rateLimitConfig.getSrc().getQps().getList().doubleValue() / count));
        }
        if (rateLimitConfig.getDest() != null && rateLimitConfig.getDest().getQps() != null) {
            if (rateLimitConfig.getDest().getQps().getHead() != null) {
                request.setHeadQpsLimit(Math.max(0.1, rateLimitConfig.getDest().getQps().getHead().doubleValue() / count));
            }
            if (rateLimitConfig.getDest().getQps().getPut() != null) {
                request.setPutQpsLimit(Math.max(0.1, rateLimitConfig.getDest().getQps().getPut().doubleValue() / count));
            }
        }
        if (rateLimitConfig.getBandwidth() != null && rateLimitConfig.getBandwidth() > 0) {
            request.setBandwidthBitsPerSecond(Math.max(1.0, rateLimitConfig.getBandwidth().doubleValue() / count));
        }
    }

    private BucketPair toBucketPair(LocalTransferRequest request) {
        TransferEndpoint source = request.getSourceEndpoint();
        TransferEndpoint local = request.getDestinationEndpoint();
        BucketPair pair = new BucketPair();
        pair.setSourceBucket(source.getBucket());
        pair.setSourceS3Alias(source.getS3Alias());
        pair.setPrefix(source.getPrefix());
        pair.setObjectKey(source.getObjectKey());
        pair.setDateAfter(request.getDateAfter());
        pair.setDateBefore(request.getDateBefore());
        pair.setOverwrite(request.getConflictPolicy() == ConflictPolicy.OVERWRITE);
        pair.setConflictPolicy(request.getConflictPolicy());
        pair.setDirection(request.getDirection());
        pair.setDistributionMode(request.getDistributionMode());
        pair.setSourceEndpoint(source);
        pair.setDestinationEndpoint(local);
        pair.setTargetRelativePath(request.getTargetRelativePath());
        pair.setParentTaskId(request.getParentTaskId());
        pair.setStorageId(local.getStorageId());
        pair.setNodeBindings(local.getNodeBindings());
        List<String> nodes = new ArrayList<>();
        for (LocalNodeBinding binding : local.getNodeBindings()) nodes.add(binding.getExecutionNodeIp());
        pair.setEligibleWorkerNodes(nodes);
        pair.setListQpsLimit(request.getListQpsLimit());
        pair.setHeadQpsLimit(request.getHeadQpsLimit());
        pair.setPutQpsLimit(request.getPutQpsLimit());
        pair.setBandwidthBitsPerSecond(request.getBandwidthBitsPerSecond());
        pair.setTaskBandwidthLimitMbps(request.getTaskBandwidthLimitMbps());
        pair.setTaskBandwidthScopeId(request.getTaskBandwidthScopeId());
        return pair;
    }

    private void dispatchDiscovery(LocalTransferRequest request) {
        List<LocalNodeBinding> bindings = localEndpoint(request).getNodeBindings();
        LocalNodeBinding discovery = bindings.get(Math.floorMod(request.getParentTaskId(), bindings.size()));
        try {
            ResponseEntity<Map> response = restTemplate.postForEntity(
                    url(discovery.getExecutionNodeIp(), "/api/worker/local-transfers/discover"), request, Map.class);
            Map envelope = response.getBody();
            Object data = envelope == null ? null : envelope.get("data");
            if (!response.getStatusCode().is2xxSuccessful() || envelope == null
                    || !Boolean.TRUE.equals(envelope.get("success")) || !(data instanceof Map)
                    || !Boolean.TRUE.equals(((Map) data).get("accepted")))
                throw new IllegalStateException("worker rejected discovery");
        } catch (Exception e) {
            throw new DownstreamWorkerException("WORKER_UNAVAILABLE",
                    "failed to start local discovery on " + discovery.getExecutionNodeIp(), e);
        }
    }

    public Map<String, Object> aggregateStatus(Integer taskId, TransferDirection direction,
                                               String executionNodeIp, String status, String endpoint) {
        return aggregateStatus(taskId, direction, executionNodeIp, status, endpoint, null);
    }

    public Map<String, Object> aggregateStatus(Integer taskId, TransferDirection direction,
                                               String executionNodeIp, String status, String endpoint,
                                               String storageId) {
        List<SubTaskStatus> allShards = new ArrayList<>();
        List<String> unreachable = new ArrayList<>();
        Map<String, Object> byWorker = new LinkedHashMap<>();
        for (String worker : selectedWorkers(executionNodeIp)) {
            try {
                // Parent status is derived from every shard, so status filtering is applied only after aggregation.
                String path = "/api/worker/local-transfers/status" + query(taskId, direction, executionNodeIp, null, endpoint, storageId, 0, 0);
                Map response = restTemplate.getForObject(url(worker, path), Map.class);
                Map data = response == null || !(response.get("data") instanceof Map) ? null : (Map) response.get("data");
                if (response == null || !Boolean.TRUE.equals(response.get("success")) || data == null
                        || !(data.get("tasks") instanceof Collection))
                    throw new IllegalStateException("worker did not return a complete task snapshot");
                byWorker.put(worker, data);
                if (data.get("tasks") instanceof Collection) {
                    for (Object item : (Collection<?>) data.get("tasks")) {
                        SubTaskStatus task = objectMapper.convertValue(item, SubTaskStatus.class);
                        task.setWorkerNode(worker);
                        if (!parentTaskService.isDeleted(task.getParentTaskId())) allShards.add(task);
                    }
                }
            } catch (Exception e) {
                unreachable.add(worker);
            }
        }
        Map<Integer, SubTaskStatus> parentsById = new LinkedHashMap<>();
        Map<Integer, Integer> shardCounts = new HashMap<>();
        Set<Integer> activeParentIds = new HashSet<>();
        for (SubTaskStatus derived : aggregateParents(allShards)) {
            parentsById.put(derived.getParentTaskId(), derived);
        }
        for (SubTaskStatus shard : allShards) {
            Integer id = shard.getParentTaskId() == null ? shard.getTaskIndex() : shard.getParentTaskId();
            shardCounts.put(id, shardCounts.getOrDefault(id, 0) + 1);
            if (shard.getStatus() == SubTaskStatus.TaskStatus.RUNNING
                    || shard.getStatus() == SubTaskStatus.TaskStatus.LIST_DONE) activeParentIds.add(id);
        }
        for (LocalParentTaskRecord record : parentTaskService.getAll()) {
            SubTaskStatus persisted = parentTaskService.toStatus(record);
            SubTaskStatus derived = parentsById.get(record.getParentTaskId());
            parentsById.put(record.getParentTaskId(), mergePersistedParent(
                    persisted, derived, shardCounts.getOrDefault(record.getParentTaskId(), 0)));
        }
        List<SubTaskStatus> parents = new ArrayList<>();
        Set<Integer> visibleParentIds = new HashSet<>();
        for (SubTaskStatus parent : parentsById.values()) {
            boolean activeChild = activeParentIds.contains(parent.getTaskIndex());
            if (parent.getStatus() == SubTaskStatus.TaskStatus.COMPLETED
                    || parent.getStatus() == SubTaskStatus.TaskStatus.STOPPED
                    || (!activeChild && (parent.getStatus() == SubTaskStatus.TaskStatus.FAILED
                    || parent.getStatus() == SubTaskStatus.TaskStatus.PARTIAL_FAILED))) {
                parent.setCurrentSpeedBytesPerSecond(0L);
            }
            if (matchesParent(parent, taskId, direction, executionNodeIp, status, endpoint, storageId)) {
                parents.add(parent);
                visibleParentIds.add(parent.getParentTaskId());
            }
        }
        parents.sort((left, right) -> Integer.compare(right.getTaskIndex(), left.getTaskIndex()));
        List<SubTaskStatus> tasks = new ArrayList<>();
        for (SubTaskStatus shard : allShards) {
            Integer id = shard.getParentTaskId() == null ? shard.getTaskIndex() : shard.getParentTaskId();
            if (visibleParentIds.contains(id)) tasks.add(shard);
        }
        Map<String, Object> result = totals(tasks);
        result.put("totalTasks", parents.size());
        result.put("tasks", parents);
        result.put("shards", tasks);
        result.put("totalParentTasks", parents.size());
        result.put("workers", byWorker);
        result.put("partial", !unreachable.isEmpty());
        result.put("unreachableWorkers", unreachable);
        return result;
    }

    private SubTaskStatus mergePersistedParent(SubTaskStatus persisted, SubTaskStatus derived, int shardCount) {
        if (derived == null) {
            if (persisted.getStatus() != SubTaskStatus.TaskStatus.STOPPED
                    && persisted.getStatus() != SubTaskStatus.TaskStatus.FAILED)
                persisted.setScanComplete(false);
            return persisted;
        }
        persisted.setScanComplete(Boolean.TRUE.equals(persisted.getScanComplete())
                && Boolean.TRUE.equals(derived.getScanComplete()));
        persisted.setParticipatingWorkerNodes(derived.getParticipatingWorkerNodes());
        persisted.setListObjectCount(derived.getListObjectCount());
        persisted.setListObjectSize(derived.getListObjectSize());
        persisted.setPutObjectCount(derived.getPutObjectCount());
        persisted.setPutObjectSize(derived.getPutObjectSize());
        persisted.setSkippedObjectCount(derived.getSkippedObjectCount());
        persisted.setSkippedObjectSize(derived.getSkippedObjectSize());
        persisted.setFailedObjectCount(derived.getFailedObjectCount());
        persisted.setFailedObjectSize(derived.getFailedObjectSize());
        persisted.setCurrentSpeedBytesPerSecond(derived.getCurrentSpeedBytesPerSecond());
        persisted.setAverageSpeedBytesPerSecond(derived.getAverageSpeedBytesPerSecond());
        if (derived.getStartTime() != null) persisted.setStartTime(derived.getStartTime());
        if (derived.getEndTime() != null) persisted.setEndTime(derived.getEndTime());
        if (persisted.getStatus() == SubTaskStatus.TaskStatus.FAILED && shardCount > 0) {
            boolean completedWork = value(derived.getPutObjectCount()) > 0
                    || value(derived.getSkippedObjectCount()) > 0
                    || derived.getStatus() == SubTaskStatus.TaskStatus.COMPLETED;
            persisted.setStatus(completedWork
                    ? SubTaskStatus.TaskStatus.PARTIAL_FAILED : SubTaskStatus.TaskStatus.FAILED);
        } else if (persisted.getStatus() != SubTaskStatus.TaskStatus.STOPPED) {
            persisted.setStatus(derived.getStatus());
            if (!Boolean.TRUE.equals(persisted.getScanComplete())
                    && persisted.getStatus() == SubTaskStatus.TaskStatus.COMPLETED) {
                persisted.setStatus(SubTaskStatus.TaskStatus.RUNNING);
                persisted.setEndTime(null);
            }
        }
        return persisted;
    }

    private boolean matchesParent(SubTaskStatus task, Integer taskId, TransferDirection direction,
                                  String executionNodeIp, String status, String endpoint, String storageId) {
        if (taskId != null && !taskId.equals(task.getParentTaskId()) && !taskId.equals(task.getTaskIndex())) return false;
        if (direction != null && direction != task.getDirection()) return false;
        if (notBlank(status) && (task.getStatus() == null || !status.equalsIgnoreCase(task.getStatus().name()))) return false;
        if (notBlank(storageId) && !storageId.equals(task.getStorageId())) return false;
        if (notBlank(executionNodeIp) && !executionNodeIp.equals(task.getExecutionNodeIp())
                && (task.getEligibleWorkerNodes() == null || !task.getEligibleWorkerNodes().contains(executionNodeIp))) return false;
        return !notBlank(endpoint) || endpointContains(task.getSourceEndpoint(), endpoint)
                || endpointContains(task.getDestinationEndpoint(), endpoint);
    }

    private boolean endpointContains(TransferEndpoint endpoint, String value) {
        if (endpoint == null) return false;
        if (value.equals(endpoint.getS3Alias()) || value.equals(endpoint.getBucket())
                || value.equals(endpoint.getExecutionNodeIp()) || value.equals(endpoint.getLocalPath())
                || value.equals(endpoint.getStorageId())) return true;
        if (endpoint.getNodeBindings() != null) {
            for (LocalNodeBinding binding : endpoint.getNodeBindings()) {
                if (value.equals(binding.getExecutionNodeIp()) || value.equals(binding.getLocalPath())) return true;
            }
        }
        return false;
    }

    private boolean notBlank(String value) {
        return value != null && !value.isEmpty();
    }

    public Map<String, Object> aggregateFailures(Integer taskId, TransferDirection direction,
                                                 String executionNodeIp, String endpoint,
                                                 int offset, int limit) {
        return aggregateFailures(taskId, direction, executionNodeIp, endpoint, null, offset, limit);
    }

    public Map<String, Object> aggregateFailures(Integer taskId, TransferDirection direction,
                                                 String executionNodeIp, String endpoint, String storageId,
                                                 int offset, int limit) {
        List<FailedKey> failures = new ArrayList<>();
        List<String> unreachable = new ArrayList<>();
        Map<String, Object> byWorker = new LinkedHashMap<>();
        int total = 0;
        int boundedOffset = Math.max(0, offset);
        int boundedLimit = Math.max(1, Math.min(1000, limit));
        for (LocalParentTaskRecord record : parentTaskService.getAll()) {
            if (record.getStatus() != SubTaskStatus.TaskStatus.FAILED || record.getErrorCode() == null) continue;
            SubTaskStatus parent = parentTaskService.toStatus(record);
            if (!matchesParent(parent, taskId, direction, executionNodeIp, null, endpoint, storageId)) continue;
            failures.add(parentFailure(record, parent));
            total++;
        }
        for (String worker : selectedWorkers(executionNodeIp)) {
            try {
                String path = "/api/worker/local-transfers/failures" + query(taskId, direction, executionNodeIp,
                        null, endpoint, storageId, 0, 1000);
                Map response = restTemplate.getForObject(url(worker, path), Map.class);
                Map data = response == null ? null : (Map) response.get("data");
                byWorker.put(worker, data);
                if (data != null) total += number(data.get("total"));
                if (data != null && data.get("items") instanceof Collection) {
                    for (Object item : (Collection<?>) data.get("items")) {
                        FailedKey failure = objectMapper.convertValue(item, FailedKey.class);
                        failure.setWorkerNode(worker);
                        failures.add(failure);
                    }
                }
            } catch (Exception e) {
                unreachable.add(worker);
            }
        }
        Map<String, Object> result = new LinkedHashMap<>();
        int from = Math.min(boundedOffset, failures.size());
        int to = Math.min(from + boundedLimit, failures.size());
        result.put("total", total);
        result.put("offset", boundedOffset);
        result.put("limit", boundedLimit);
        result.put("items", new ArrayList<>(failures.subList(from, to)));
        result.put("workers", byWorker);
        result.put("partial", !unreachable.isEmpty());
        result.put("unreachableWorkers", unreachable);
        return result;
    }

    private FailedKey parentFailure(LocalParentTaskRecord record, SubTaskStatus parent) {
        FailedKey failure = new FailedKey();
        failure.setTaskIndex(record.getParentTaskId());
        failure.setParentTaskId(record.getParentTaskId());
        failure.setShardId("orchestration");
        failure.setAttempt(0);
        failure.setDirection(parent.getDirection());
        failure.setSourceBucket(parent.getSourceBucket());
        failure.setDestBucket(parent.getDestBucket());
        failure.setSourceEndpoint(parent.getSourceEndpoint());
        failure.setDestinationEndpoint(parent.getDestinationEndpoint());
        failure.setStorageId(parent.getStorageId());
        failure.setEligibleWorkerNodes(parent.getEligibleWorkerNodes());
        failure.setLocalPath(parent.getLocalPath());
        failure.setExecutionNodeIp(parent.getExecutionNodeIp());
        failure.setKey(parent.getObjectKey() != null ? parent.getObjectKey()
                : (parent.getSourceEndpoint() != null && parent.getSourceEndpoint().getPrefix() != null
                ? parent.getSourceEndpoint().getPrefix() : parent.getSourceBucket()));
        failure.setObjectSize(0L);
        failure.setStage("SOURCE_DISCOVERY_FAILED".equals(record.getErrorCode())
                ? FailureStage.SCAN : FailureStage.DISPATCH);
        failure.setErrorCode(record.getErrorCode());
        failure.setErrorMessage(record.getErrorMessage());
        failure.setRetryCount(0);
        failure.setTimestamp(record.getUpdatedAt());
        return failure;
    }

    public Map<String, Object> control(String operation, List<Integer> taskIds) {
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("taskIds", taskIds == null ? Collections.emptyList() : taskIds);
        return control(operation, body);
    }

    public Map<String, Object> control(String operation, Map<String, Object> body) {
        synchronized (parentTaskService) {
            return controlLocked(operation, body);
        }
    }

    private Map<String, Object> controlLocked(String operation, Map<String, Object> body) {
        int accepted = 0, ignored = 0, failed = 0;
        List<String> unreachable = new ArrayList<>();
        if (body == null) body = Collections.emptyMap();
        List<Integer> parentIds = taskIds(body);
        if (parentIds.isEmpty()) {
            for (LocalParentTaskRecord record : parentTaskService.getAll()) parentIds.add(record.getParentTaskId());
        }
        if (!"stop".equals(operation)) {
            parentIds.removeIf(parentTaskService::isDeleted);
            body = new LinkedHashMap<>(body);
            body.put("taskIds", parentIds);
            if ("resume".equals(operation)) body.put("deferDiscovery", true);
            if (parentIds.isEmpty()) {
                Map<String, Object> noOp = new LinkedHashMap<>();
                noOp.put("accepted", 0); noOp.put("ignored", 0); noOp.put("failed", 0);
                noOp.put("partial", false); noOp.put("unreachableWorkers", Collections.emptyList());
                return noOp;
            }
        }
        if ("resume".equals(operation)) {
            try {
                for (Integer parentId : parentIds) {
                    LocalParentTaskRecord record = parentTaskService.get(parentId);
                    if (record != null && taskBandwidth != null) taskBandwidth.prepareLocal(record.getRequest());
                    prepareLegacyCheckpoint(record);
                }
            } catch (RuntimeException e) {
                Map<String, Object> rejected = new LinkedHashMap<>();
                rejected.put("accepted", 0); rejected.put("ignored", 0); rejected.put("failed", parentIds.size());
                rejected.put("partial", false); rejected.put("unreachableWorkers", Collections.emptyList());
                rejected.put("message", e.getMessage()); rejected.put("code", errorCode(e));
                return rejected;
            }
        }
        // Stop scheduling before asking workers to cancel; a scanner cannot publish another batch afterwards.
        if ("stop".equals(operation)) for (Integer parentId : parentIds) parentTaskService.markStopped(parentId);
        for (String worker : workers()) {
            try {
                ResponseEntity<Map> response = restTemplate.postForEntity(
                        url(worker, "/api/worker/local-transfers/" + operation), body, Map.class);
                Map envelope = response.getBody();
                Map data = envelope == null ? null : (Map) envelope.get("data");
                if (envelope == null || !Boolean.TRUE.equals(envelope.get("success")) || data == null)
                    throw new IllegalStateException("worker control response was not successful");
                if (data != null) {
                    accepted += number(data.get("accepted"));
                    ignored += number(data.get("ignored"));
                    failed += number(data.get("failed"));
                }
            } catch (Exception e) {
                unreachable.add(worker);
            }
        }
        if ("resume".equals(operation) && (!unreachable.isEmpty() || failed > 0)) {
            // A partial resume must not leave some nodes transferring behind a stopped parent.
            for (Integer parentId : parentIds) parentTaskService.markStopped(parentId);
            for (String worker : workers()) {
                try { restTemplate.postForEntity(url(worker, "/api/worker/local-transfers/stop"), body, Map.class); }
                catch (Exception ignoredStopFailure) { if (!unreachable.contains(worker)) unreachable.add(worker); }
            }
            accepted = 0;
        }
        for (Integer parentId : parentIds) {
            LocalParentTaskRecord record = parentTaskService.get(parentId);
            if (record == null || parentTaskService.isDeleted(parentId)) continue;
            if ("stop".equals(operation)) {
                // Parent cancellation is already durable even if one node is temporarily unavailable.
            } else if ("resume".equals(operation)) {
                if (!unreachable.isEmpty() || failed > 0) continue;
                try {
                    parentTaskService.markResuming(parentId);
                    if (record.getRequest().getDirection() == TransferDirection.LOCAL_TO_OSS) {
                        // All workers have cleared the parent stop marker before discovery can
                        // replay a pending batch to another node.
                        dispatchDiscovery(record.getRequest());
                        parentTaskService.markOrchestrationComplete(parentId);
                    } else if (!Boolean.TRUE.equals(record.getOrchestrationComplete())) {
                        dispatchParent(record.getRequest());
                    }
                    accepted++;
                } catch (RuntimeException e) {
                    parentTaskService.markStopped(parentId);
                    for (String worker : workers()) {
                        try { restTemplate.postForEntity(url(worker, "/api/worker/local-transfers/stop"),
                                Collections.singletonMap("taskIds", Collections.singletonList(parentId)), Map.class); }
                        catch (Exception stopFailure) { if (!unreachable.contains(worker)) unreachable.add(worker); }
                    }
                    failed++;
                }
            } else if ("retry-failures".equals(operation)) {
                if (!Boolean.TRUE.equals(record.getOrchestrationComplete())) {
                    try {
                        parentTaskService.markRetrying(parentId);
                        dispatchParent(record.getRequest());
                        accepted++;
                    } catch (RuntimeException e) {
                        parentTaskService.markFailed(parentId, errorCode(e), e.getMessage());
                        failed++;
                    }
                } else {
                    parentTaskService.markWorkerRetrying(parentId);
                }
            }
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("accepted", accepted);
        result.put("ignored", ignored);
        result.put("failed", failed);
        result.put("partial", !unreachable.isEmpty());
        result.put("unreachableWorkers", unreachable);
        return result;
    }

    private List<Integer> taskIds(Map<String, Object> body) {
        Object values = body.get("taskIds");
        if (!(values instanceof Collection)) return new ArrayList<>();
        List<Integer> ids = new ArrayList<>();
        for (Object value : (Collection<?>) values) {
            if (value instanceof Number) ids.add(((Number) value).intValue());
            else ids.add(Integer.valueOf(String.valueOf(value)));
        }
        return ids;
    }

    public List<SubTaskStatus> reportTasks(List<String> unreachable) {
        Map<String, Object> status = aggregateStatus(null, null, null, null, null, null);
        unreachable.addAll((Collection<String>) status.get("unreachableWorkers"));
        return (List<SubTaskStatus>) status.get("tasks");
    }

    public List<FailedKey> reportFailures(List<String> unreachable) {
        List<FailedKey> all = new ArrayList<>();
        for (LocalParentTaskRecord record : parentTaskService.getAll()) {
            if (record.getStatus() == SubTaskStatus.TaskStatus.FAILED && record.getErrorCode() != null) {
                all.add(parentFailure(record, parentTaskService.toStatus(record)));
            }
        }
        for (String worker : workers()) {
            int offset = 0;
            try {
                while (true) {
                    Map response = restTemplate.getForObject(url(worker,
                            "/api/worker/local-transfers/failures?offset=" + offset + "&limit=1000"), Map.class);
                    Map data = response == null ? null : (Map) response.get("data");
                    Collection items = data == null ? Collections.emptyList() : (Collection) data.get("items");
                    if (items == null || items.isEmpty()) break;
                    for (Object item : items) {
                        FailedKey failure = objectMapper.convertValue(item, FailedKey.class);
                        failure.setWorkerNode(worker);
                        all.add(failure);
                    }
                    offset += items.size();
                    if (items.size() < 1000) break;
                }
            } catch (Exception e) {
                if (!unreachable.contains(worker)) unreachable.add(worker);
            }
        }
        return all;
    }

    private Map<String, Object> callValidate(String worker, LocalTransferRequest request) {
        try {
            ResponseEntity<Map> response = restTemplate.postForEntity(
                    url(worker, "/api/worker/local-transfers/validate"), request, Map.class);
            if (!response.getStatusCode().is2xxSuccessful()) throw new IllegalStateException("preflight rejected");
            Map envelope = response.getBody();
            Object data = envelope == null ? null : envelope.get("data");
            return data instanceof Map ? (Map<String, Object>) data : Collections.emptyMap();
        } catch (HttpStatusCodeException e) {
            try {
                Map body = objectMapper.readValue(e.getResponseBodyAsString(), Map.class);
                String code = String.valueOf(body.get("code"));
                String message = String.valueOf(body.get("message"));
                throw new TransferValidationException(code, message);
            } catch (TransferValidationException parsed) {
                throw parsed;
            } catch (Exception ignored) {
                throw new TransferValidationException("INVALID_ENDPOINT", "worker rejected local path preflight");
            }
        } catch (Exception e) {
            throw new DownstreamWorkerException("WORKER_UNAVAILABLE",
                    "worker path preflight failed for " + worker, e);
        }
    }

    private TransferEndpoint localEndpoint(LocalTransferRequest request) {
        return request.getDirection() == TransferDirection.LOCAL_TO_OSS
                ? request.getSourceEndpoint() : request.getDestinationEndpoint();
    }

    private void requireWorker(String worker) {
        if (!workers().contains(worker)) {
            throw new TransferValidationException("WORKER_NOT_CONFIGURED", "worker is not configured: " + worker);
        }
    }

    private List<String> workers() {
        return distributedConfig.getWorkerIps() == null ? Collections.emptyList() : distributedConfig.getWorkerIps();
    }

    private List<String> selectedWorkers(String worker) {
        if (worker == null || worker.isEmpty()) return workers();
        requireWorker(worker);
        return Collections.singletonList(worker);
    }

    private String url(String worker, String path) {
        return "http://" + worker + ":" + distributedConfig.getPort() + path;
    }

    private String query(Integer taskId, TransferDirection direction, String executionNodeIp,
                         String status, String endpoint, String storageId, int offset, int limit) {
        List<String> values = new ArrayList<>();
        if (taskId != null) values.add("taskId=" + taskId);
        if (direction != null) values.add("direction=" + direction.name());
        if (executionNodeIp != null) values.add("executionNodeIp=" + encodeQueryValue(executionNodeIp));
        if (status != null) values.add("status=" + encodeQueryValue(status));
        if (endpoint != null) values.add("endpoint=" + encodeQueryValue(endpoint));
        if (storageId != null) values.add("storageId=" + encodeQueryValue(storageId));
        if (limit > 0) {
            values.add("offset=" + offset);
            values.add("limit=" + limit);
        }
        return values.isEmpty() ? "" : "?" + String.join("&", values);
    }

    private Map<String, Object> totals(List<SubTaskStatus> tasks) {
        long scannedCount = 0, scannedBytes = 0, successCount = 0, successBytes = 0;
        long skippedCount = 0, skippedBytes = 0, failedCount = 0, failedBytes = 0;
        for (SubTaskStatus task : tasks) {
            scannedCount += value(task.getListObjectCount()); scannedBytes += value(task.getListObjectSize());
            successCount += value(task.getPutObjectCount()); successBytes += value(task.getPutObjectSize());
            skippedCount += value(task.getSkippedObjectCount()); skippedBytes += value(task.getSkippedObjectSize());
            failedCount += value(task.getFailedObjectCount()); failedBytes += value(task.getFailedObjectSize());
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("totalTasks", tasks.size()); result.put("scannedCount", scannedCount);
        result.put("scannedBytes", scannedBytes); result.put("successCount", successCount);
        result.put("successBytes", successBytes); result.put("skippedCount", skippedCount);
        result.put("skippedBytes", skippedBytes); result.put("failedCount", failedCount);
        result.put("failedBytes", failedBytes);
        return result;
    }

    private List<SubTaskStatus> aggregateParents(List<SubTaskStatus> shards) {
        Map<Integer, SubTaskStatus> parents = new LinkedHashMap<>();
        for (SubTaskStatus shard : shards) {
            Integer id = shard.getParentTaskId() == null ? shard.getTaskIndex() : shard.getParentTaskId();
            SubTaskStatus parent = parents.get(id);
            if (parent == null) {
                parent = new SubTaskStatus();
                parent.setTaskIndex(id);
                parent.setParentTaskId(id);
                parent.setDirection(shard.getDirection());
                parent.setSourceBucket(shard.getSourceBucket());
                parent.setDestBucket(shard.getDestBucket());
                parent.setSourceS3Alias(shard.getSourceS3Alias());
                parent.setDestS3Alias(shard.getDestS3Alias());
                parent.setSourceEndpoint(shard.getSourceEndpoint());
                parent.setDestinationEndpoint(shard.getDestinationEndpoint());
                parent.setConflictPolicy(shard.getConflictPolicy());
                parent.setDistributionMode(shard.getDistributionMode());
                parent.setStorageId(shard.getStorageId());
                parent.setEligibleWorkerNodes(shard.getEligibleWorkerNodes());
                parent.setParticipatingWorkerNodes(new ArrayList<>());
                parent.setNodeBindings(shard.getNodeBindings());
                parent.setCreateTime(shard.getCreateTime());
                parent.setStartTime(shard.getStartTime());
                parent.setEndTime(shard.getEndTime());
                parent.setStatus(shard.getStatus());
                parent.setScanComplete(true);
                parent.setListObjectCount(0L); parent.setListObjectSize(0L);
                parent.setPutObjectCount(0L); parent.setPutObjectSize(0L);
                parent.setSkippedObjectCount(0L); parent.setSkippedObjectSize(0L);
                parent.setFailedObjectCount(0L); parent.setFailedObjectSize(0L);
                parent.setCurrentSpeedBytesPerSecond(0L); parent.setAverageSpeedBytesPerSecond(0L);
                parents.put(id, parent);
            }
            boolean shardScanComplete = Boolean.TRUE.equals(shard.getScanComplete())
                    || (shard.getScanComplete() == null && (shard.getStatus() == SubTaskStatus.TaskStatus.LIST_DONE
                    || shard.getStatus() == SubTaskStatus.TaskStatus.COMPLETED));
            parent.setScanComplete(Boolean.TRUE.equals(parent.getScanComplete()) && shardScanComplete);
            parent.setListObjectCount(value(parent.getListObjectCount()) + value(shard.getListObjectCount()));
            parent.setListObjectSize(value(parent.getListObjectSize()) + value(shard.getListObjectSize()));
            parent.setPutObjectCount(value(parent.getPutObjectCount()) + value(shard.getPutObjectCount()));
            parent.setPutObjectSize(value(parent.getPutObjectSize()) + value(shard.getPutObjectSize()));
            parent.setSkippedObjectCount(value(parent.getSkippedObjectCount()) + value(shard.getSkippedObjectCount()));
            parent.setSkippedObjectSize(value(parent.getSkippedObjectSize()) + value(shard.getSkippedObjectSize()));
            parent.setFailedObjectCount(value(parent.getFailedObjectCount()) + value(shard.getFailedObjectCount()));
            parent.setFailedObjectSize(value(parent.getFailedObjectSize()) + value(shard.getFailedObjectSize()));
            parent.setCurrentSpeedBytesPerSecond(value(parent.getCurrentSpeedBytesPerSecond()) + value(shard.getCurrentSpeedBytesPerSecond()));
            parent.setAverageSpeedBytesPerSecond(value(parent.getAverageSpeedBytesPerSecond()) + value(shard.getAverageSpeedBytesPerSecond()));
            if (shard.getWorkerNode() != null && !parent.getParticipatingWorkerNodes().contains(shard.getWorkerNode())) {
                parent.getParticipatingWorkerNodes().add(shard.getWorkerNode());
            }
            if (shard.getCreateTime() != null && (parent.getCreateTime() == null
                    || shard.getCreateTime() < parent.getCreateTime())) parent.setCreateTime(shard.getCreateTime());
            if (shard.getStartTime() != null && (parent.getStartTime() == null || shard.getStartTime() < parent.getStartTime())) parent.setStartTime(shard.getStartTime());
            if (shard.getEndTime() != null) parent.setEndTime(parent.getEndTime() == null
                    ? shard.getEndTime() : Math.max(parent.getEndTime(), shard.getEndTime()));
            parent.setStatus(mergeStatus(parent.getStatus(), shard.getStatus()));
        }
        for (SubTaskStatus parent : parents.values()) {
            if (parent.getStatus() == SubTaskStatus.TaskStatus.PENDING
                    || parent.getStatus() == SubTaskStatus.TaskStatus.RUNNING
                    || parent.getStatus() == SubTaskStatus.TaskStatus.LIST_DONE) parent.setEndTime(null);
            long durationMillis = parent.getStartTime() == null ? 0L : Math.max(1L,
                    (parent.getEndTime() == null ? System.currentTimeMillis() : parent.getEndTime())
                            - parent.getStartTime());
            parent.setAverageSpeedBytesPerSecond(durationMillis == 0L ? 0L
                    : value(parent.getPutObjectSize()) * 1000L / durationMillis);
        }
        return new ArrayList<>(parents.values());
    }

    private String encodeQueryValue(String value) {
        try {
            return URLEncoder.encode(value, "UTF-8");
        } catch (UnsupportedEncodingException impossible) {
            throw new IllegalStateException("UTF-8 is not available", impossible);
        }
    }

    private SubTaskStatus.TaskStatus mergeStatus(SubTaskStatus.TaskStatus left, SubTaskStatus.TaskStatus right) {
        if (left == null) return right;
        if (right == null || left == right) return left;
        if (left == SubTaskStatus.TaskStatus.FAILED || right == SubTaskStatus.TaskStatus.FAILED
                || left == SubTaskStatus.TaskStatus.PARTIAL_FAILED || right == SubTaskStatus.TaskStatus.PARTIAL_FAILED) {
            return SubTaskStatus.TaskStatus.PARTIAL_FAILED;
        }
        if (left == SubTaskStatus.TaskStatus.STOPPED || right == SubTaskStatus.TaskStatus.STOPPED) return SubTaskStatus.TaskStatus.STOPPED;
        if (left == SubTaskStatus.TaskStatus.RUNNING || right == SubTaskStatus.TaskStatus.RUNNING
                || left == SubTaskStatus.TaskStatus.LIST_DONE || right == SubTaskStatus.TaskStatus.LIST_DONE) return SubTaskStatus.TaskStatus.RUNNING;
        if (left == SubTaskStatus.TaskStatus.PENDING || right == SubTaskStatus.TaskStatus.PENDING) return SubTaskStatus.TaskStatus.PENDING;
        return SubTaskStatus.TaskStatus.COMPLETED;
    }

    private long value(Long value) { return value == null ? 0 : value; }
    private int number(Object value) { return value instanceof Number ? ((Number) value).intValue() : 0; }
}
