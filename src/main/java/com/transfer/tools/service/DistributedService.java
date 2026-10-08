package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.ListSubTask;
import com.transfer.tools.model.LocalNodeBinding;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.service.TransferValidationException;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.ResponseEntity;
import org.springframework.http.client.ClientHttpRequestInterceptor;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;

import java.util.ArrayList;
import java.util.Collections;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicInteger;

@Slf4j
@Service
public class DistributedService {

    @Autowired
    private DistributedConfig distributedConfig;

    @Autowired
    private SubTaskManagerService subTaskManagerService;

    @Autowired
    private TaskCreationService taskCreationService;

    @Autowired private BalancedWorkerScheduler balancedWorkerScheduler;
    @Autowired private NodeConfigService nodeConfigService;
    @Autowired private MasterTaskBandwidthService taskBandwidth;
    private final AtomicInteger balancedCursor = new AtomicInteger();

    private RestTemplate restTemplate = boundedRestTemplate();

    private static RestTemplate boundedRestTemplate() {
        org.springframework.http.client.SimpleClientHttpRequestFactory factory =
                new org.springframework.http.client.SimpleClientHttpRequestFactory();
        factory.setConnectTimeout(3000);
        factory.setReadTimeout(10000);
        return new RestTemplate(factory);
    }

    private Map<String, String> bucketWorkerMapping = new ConcurrentHashMap<>();

    {
        // 附加节点间内部认证头，使主节点调用 worker 接口时通过 AuthInterceptor 校验
        restTemplate.getInterceptors().add((ClientHttpRequestInterceptor) (request, body, execution) -> {
            String token = distributedConfig.getInternalToken();
            if (token != null && !token.isEmpty()) {
                request.getHeaders().set("X-Internal-Token", token);
            }
            return execution.execute(request, body);
        });
    }

    private AtomicInteger distributedIdx = new AtomicInteger(0);

    public static final String MODE_BALANCED = "balanced";
    public static final String MODE_AFFINITY = "affinity";

    private String currentMode = MODE_AFFINITY;

    public boolean isMaster() {
        return distributedConfig.isMaster();
    }

    public void setDistributionMode(String mode) {
        if (MODE_BALANCED.equals(mode) || MODE_AFFINITY.equals(mode)) {
            this.currentMode = mode;
            log.info("任务分发模式设置为: {}", mode);
        }
    }

    public String getDistributionMode() {
        return currentMode;
    }

    public void clearBucketWorkerMapping() {
        bucketWorkerMapping.clear();
        distributedIdx.set(0);
        log.info("已清空bucket-worker映射关系");
    }

    public void distributeTasks(List<ListSubTask> allTasks) {
        distributeTasks(allTasks, currentMode);
    }

    public void distributeTasks(List<ListSubTask> allTasks, String mode) {
        String creation = taskCreationService == null ? null : taskCreationService.currentCreationId();
        List<String> workers = creationMembers(allTasks, creation);
        if (taskBandwidth != null && creation != null) {
            com.transfer.tools.model.TaskBandwidthPolicy policy = taskBandwidth.desired("creation:" + creation);
            if (policy == null) throw new TransferValidationException("TASK_BANDWIDTH_NOT_READY", "任务带宽策略未就绪");
            workers = policy.getMembers();
            for (ListSubTask task : allTasks) {
                task.setTaskBandwidthScopeId(policy.getScopeId());
                task.setTaskBandwidthLimitMbps(policy.getLimitMbps());
            }
        }
        workers = MasterTaskBandwidthService.selectMembers(workers, distributedConfig.getWorkerIps());
        for (ListSubTask task : allTasks) task.setEligibleWorkerNodes(new ArrayList<>(workers));
        if (creation != null && taskCreationService.get(creation) != null) {
            final List<String> members = workers;
            taskCreationService.whileActive(creation, () -> dispatchOssCreation(allTasks, mode, members, creation));
        } else distributeTasks(allTasks, mode, workers, null);
    }

    private List<String> creationMembers(List<ListSubTask> tasks, String creation) {
        TaskCreationService.Creation saved = creation == null ? null : taskCreationService.get(creation);
        List<String> selected = saved != null && saved.getRequest() != null
                ? saved.getRequest().getEligibleWorkerNodes() : null;
        for (ListSubTask task : tasks) {
            if (task.getEligibleWorkerNodes() == null) continue;
            List<String> taskMembers = MasterTaskBandwidthService.selectMembers(
                    task.getEligibleWorkerNodes(), distributedConfig.getWorkerIps());
            if (selected == null) selected = taskMembers;
            else if (!new java.util.LinkedHashSet<>(selected).equals(new java.util.LinkedHashSet<>(taskMembers)))
                throw new TransferValidationException("TASK_WORKERS_MISMATCH", "同一迁移任务的执行节点不一致");
        }
        return MasterTaskBandwidthService.selectMembers(selected, distributedConfig.getWorkerIps());
    }

    private String creationAffinityWorker(String creation, List<String> workers) {
        List<String> previous = taskCreationService.get(creation).getParticipatingWorkerNodes();
        if (previous != null && !previous.isEmpty()) {
            String worker = previous.get(0);
            if (!workers.contains(worker))
                throw new TransferValidationException("TASK_WORKERS_MISMATCH", "原执行节点不在当前任务范围内");
            return worker;
        }
        return getOrCreateWorkerForBucket("oss-creation:" + creation, workers);
    }

    /** Plans are durable before send; retries use the same node, ID and idempotency shard token. */
    private void dispatchOssCreation(List<ListSubTask> tasks, String mode, List<String> workers, String creation) {
        if (!taskCreationService.get(creation).getPendingDispatches().isEmpty())
            throw new IllegalStateException("replay pending ranges before discovering additional ranges");
        if (workers == null || workers.isEmpty())
            throw new TransferValidationException("NO_ELIGIBLE_WORKER", "no eligible worker is available");
        Map<String,List<ListSubTask>> groups = new java.util.LinkedHashMap<>();
        int shard = taskCreationService.get(creation).getNextShardIndex();
        for (ListSubTask task : tasks) {
            task.setAutoStart(taskCreationService.get(creation).getAutoStart());
            task.setShardIndex(shard++);
            task.setShardId("oss-" + creation + "-" + task.getShardIndex());
            String worker = MODE_BALANCED.equals(mode) ? selectBalanced(workers, task)
                    : creationAffinityWorker(creation, workers);
            groups.computeIfAbsent(worker, ignored -> new ArrayList<>()).add(task);
        }
        int id = subTaskManagerService.allocateTaskIdRange(tasks.size());
        List<TaskCreationService.Dispatch> pending = new ArrayList<>();
        for (Map.Entry<String,List<ListSubTask>> group : groups.entrySet()) {
            TaskCreationService.Dispatch batch = new TaskCreationService.Dispatch();
            batch.setWorker(group.getKey()); batch.setFirstTaskId(id); batch.setTasks(group.getValue());
            pending.add(batch); id += group.getValue().size();
        }
        taskCreationService.savePending(creation, pending);
        replayOssCreation(creation);
    }

    public void replayOssCreation(String creation) {
        if (creation == null || taskCreationService == null || taskCreationService.get(creation) == null) return;
        taskCreationService.whileActive(creation, () -> {
            List<TaskCreationService.Dispatch> pending = taskCreationService.get(creation).getPendingDispatches();
            if (pending.isEmpty()) return;
            for (TaskCreationService.Dispatch batch : pending) {
                List<String> members = creationMembers(batch.getTasks(), creation);
                if (!members.contains(batch.getWorker()))
                    throw new TransferValidationException("TASK_WORKERS_MISMATCH", "已保存子任务的节点不在任务范围内");
            }
            for (TaskCreationService.Dispatch batch : pending)
                distributeTasksToWorker(batch.getWorker(), batch.getTasks(), batch.getFirstTaskId());
            taskCreationService.acknowledgePending(creation);
        });
    }

    public void distributeTasks(List<ListSubTask> allTasks, String mode, List<String> eligibleWorkers,
                                List<LocalNodeBinding> nodeBindings) {
        List<String> workers = eligibleWorkers == null ? new ArrayList<>() : new ArrayList<>(eligibleWorkers);
        if (workers == null || workers.isEmpty()) {
            throw new TransferValidationException("NO_ELIGIBLE_WORKER", "no eligible worker is available");
        }

        int totalTasks = allTasks.size();
        boolean localTransfer = isLocalTransfer(allTasks);
        Integer startIndex = localTransfer ? 0 : subTaskManagerService.allocateTaskIdRange(totalTasks);

        if (MODE_BALANCED.equals(mode)) {
            distributeTasksBalanced(allTasks, workers, nodeBindings, startIndex);
        } else {
            distributeTasksWithAffinity(allTasks, workers, nodeBindings, startIndex);
        }
    }

    public void prepareLocalAssignments(List<ListSubTask> tasks, String mode, List<String> workers,
                                        List<LocalNodeBinding> bindings) {
        if (workers == null || workers.isEmpty())
            throw new TransferValidationException("NO_ELIGIBLE_WORKER", "no eligible worker is available");
        String affinity = MODE_BALANCED.equals(mode) ? null
                : getOrCreateWorkerForBucket("local-parent:" + tasks.get(0).getParentTaskId(), workers);
        for (int i = 0; i < tasks.size(); i++) {
            ListSubTask task = tasks.get(i);
            if (task.getExecutionNodeIp() != null) continue; // A saved assignment is immutable.
            String worker = affinity == null ? selectBalanced(workers, task) : affinity;
            bind(java.util.Collections.singletonList(tasks.get(i)), worker, bindings);
        }
    }

    public void distributePreparedLocalTasks(List<ListSubTask> tasks) {
        Map<String, List<ListSubTask>> groups = new java.util.LinkedHashMap<>();
        for (ListSubTask task : tasks) {
            if (task.getExecutionNodeIp() == null)
                throw new TransferValidationException("SHARD_ASSIGNMENT_MISMATCH", "missing durable worker assignment");
            groups.computeIfAbsent(task.getExecutionNodeIp(), ignored -> new ArrayList<>()).add(task);
        }
        for (Map.Entry<String, List<ListSubTask>> group : groups.entrySet())
            distributeTasksToWorker(group.getKey(), group.getValue(), 0);
    }

    private void distributeTasksBalanced(List<ListSubTask> allTasks, List<String> workers,
                                         List<LocalNodeBinding> nodeBindings, int startIndex) {
        Map<String, List<ListSubTask>> groups = new java.util.LinkedHashMap<>();
        for (ListSubTask task : allTasks) {
            String worker = selectBalanced(workers, task);
            bind(java.util.Collections.singletonList(task), worker, nodeBindings);
            groups.computeIfAbsent(worker, ignored -> new ArrayList<>()).add(task);
        }
        int taskIndex = startIndex;
        for (Map.Entry<String, List<ListSubTask>> group : groups.entrySet()) {
            distributeTasksToWorker(group.getKey(), group.getValue(), taskIndex);
            taskIndex += group.getValue().size();
        }
    }

    private String selectBalanced(List<String> workers, ListSubTask task) {
        if (balancedWorkerScheduler == null)
            return workers.get(Math.floorMod(balancedCursor.getAndIncrement(), workers.size()));
        return balancedWorkerScheduler.select(workers,
                task.getEstimatedObjectSize() == null ? 0 : task.getEstimatedObjectSize(),
                task.getEstimatedObjectCount() == null ? 1 : task.getEstimatedObjectCount());
    }

    private void distributeTasksWithAffinity(List<ListSubTask> allTasks, List<String> workers,
                                             List<LocalNodeBinding> nodeBindings, int startIndex) {
        Map<String, List<ListSubTask>> bucketTasksMap = new HashMap<>();
        for (ListSubTask task : allTasks) {
            String bucketKey = task.getParentTaskId() == null
                    ? task.getSourceBucket() + "->" + task.getDestBucket()
                    : "local-parent:" + task.getParentTaskId();
            bucketTasksMap.computeIfAbsent(bucketKey, k -> new ArrayList<>()).add(task);
        }

        int taskIndex = startIndex;
        for (Map.Entry<String, List<ListSubTask>> entry : bucketTasksMap.entrySet()) {
            String bucketKey = entry.getKey();
            List<ListSubTask> bucketTasks = entry.getValue();

            String workerIp = getOrCreateWorkerForBucket(bucketKey, workers);
            bind(bucketTasks, workerIp, nodeBindings);

            int workerStartIndex = taskIndex;
            int workerEndIndex = taskIndex + bucketTasks.size() - 1;

            try {
                distributeTasksToWorker(workerIp, bucketTasks, workerStartIndex);
                if (isLocalTransfer(bucketTasks)) {
                    log.info("亲和模式: 分发父任务 {} 的 {} 个本地子任务到工作节点 {}",
                            bucketTasks.get(0).getParentTaskId(), bucketTasks.size(), workerIp);
                } else {
                    log.info("亲和模式: 分发 {} 个任务到工作节点 {} (bucket: {})，taskId范围: {} - {}",
                            bucketTasks.size(), workerIp, bucketKey, workerStartIndex, workerEndIndex);
                }
            } catch (Exception e) {
                log.error("分发任务到工作节点 {} 失败", workerIp, e);
                throw e instanceof RuntimeException ? (RuntimeException) e : new RuntimeException(e);
            }

            taskIndex = workerEndIndex + 1;
        }
    }

    private String getOrCreateWorkerForBucket(String bucketKey, List<String> workers) {
        String existingWorker = bucketWorkerMapping.get(bucketKey);
        if (existingWorker != null && workers.contains(existingWorker)) {
            return existingWorker;
        }

        int workerCount = workers.size();
        int currentIdx = Math.floorMod(distributedIdx.getAndUpdate(idx -> (idx + 1) % workerCount), workerCount);
        String selectedWorker = workers.get(currentIdx);

        bucketWorkerMapping.put(bucketKey, selectedWorker);
        log.info("bucket {} 映射到 worker {} (distributedIdx: {})", bucketKey, selectedWorker, currentIdx);

        return selectedWorker;
    }

    private void bind(List<ListSubTask> tasks, String workerIp, List<LocalNodeBinding> bindings) {
        if (bindings == null || bindings.isEmpty()) return;
        LocalNodeBinding selected = null;
        for (LocalNodeBinding binding : bindings) {
            if (workerIp.equals(binding.getExecutionNodeIp())) {
                selected = binding;
                break;
            }
        }
        if (selected == null) {
            throw new TransferValidationException("SHARD_ASSIGNMENT_MISMATCH",
                    "worker has no LOCAL node binding: " + workerIp);
        }
        for (ListSubTask task : tasks) {
            task.setExecutionNodeIp(workerIp);
            task.setLocalPath(selected.getLocalPath());
        }
    }

    private void distributeTasksToWorker(String workerIp, List<ListSubTask> tasks, int startTaskIndex) {
        if (nodeConfigService == null) { dispatchToWorker(workerIp, tasks, startTaskIndex); return; }
        nodeConfigService.withMembershipReadLock(() -> {
            if (!distributedConfig.getWorkerIps().contains(workerIp))
                throw new TransferValidationException("NO_ELIGIBLE_WORKER", "assigned worker is no longer registered: " + workerIp);
            dispatchToWorker(workerIp, tasks, startTaskIndex);
            return null;
        });
    }

    private void dispatchToWorker(String workerIp, List<ListSubTask> tasks, int startTaskIndex) {
        String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/tasks";

        List<Map<String, Object>> taskList = new ArrayList<>();
        java.util.Set<String> checkedScopes = new java.util.HashSet<>();
        int taskIndex = startTaskIndex;
        for (ListSubTask task : tasks) {
            Map<String, Object> taskMap = new HashMap<>();
            int assignedTaskId = task.getDirection() != null && task.getDirection() != TransferDirection.OSS_TO_OSS
                    ? subTaskManagerService.preferredLocalShardTaskId(
                    task.getDirection(), task.getParentTaskId(), task.getShardId()) : taskIndex;
            if (taskBandwidth != null && task.getTaskBandwidthScopeId() != null) {
                if (checkedScopes.add(task.getTaskBandwidthScopeId()))
                    taskBandwidth.requireAssignment(task.getTaskBandwidthScopeId(), workerIp);
                taskBandwidth.registerTask(task.getParentTaskId() == null ? assignedTaskId : task.getParentTaskId(), task.getTaskBandwidthScopeId());
            }
            taskMap.put("taskIndex", assignedTaskId);
            taskMap.put("sourceBucket", task.getSourceBucket());
            taskMap.put("destBucket", task.getDestBucket());
            taskMap.put("sourceS3Alias", task.getSourceS3Alias());
            taskMap.put("destS3Alias", task.getDestS3Alias());
            taskMap.put("startKey", task.getStartKey());
            taskMap.put("endKey", task.getEndKey());
            taskMap.put("workerNode", workerIp);
            taskMap.put("dateAfter", task.getDateAfter());
            taskMap.put("dateBefore", task.getDateBefore());
            taskMap.put("overwrite", task.getOverwrite());
            taskMap.put("direction", task.getDirection());
            taskMap.put("sourceEndpoint", task.getSourceEndpoint());
            taskMap.put("destinationEndpoint", task.getDestinationEndpoint());
            taskMap.put("conflictPolicy", task.getConflictPolicy());
            taskMap.put("localPath", task.getLocalPath());
            taskMap.put("executionNodeIp", task.getExecutionNodeIp());
            taskMap.put("objectKey", task.getObjectKey());
            taskMap.put("keyMapping", task.getKeyMapping());
            taskMap.put("targetRelativePath", task.getTargetRelativePath());
            taskMap.put("distributionMode", task.getDistributionMode());
            taskMap.put("parentTaskId", task.getParentTaskId());
            taskMap.put("shardId", task.getShardId());
            taskMap.put("shardIndex", task.getShardIndex());
            taskMap.put("attempt", task.getAttempt());
            taskMap.put("autoStart", task.getAutoStart());
            taskMap.put("storageId", task.getStorageId());
            taskMap.put("eligibleWorkerNodes", task.getEligibleWorkerNodes());
            taskMap.put("nodeBindings", task.getNodeBindings());
            taskMap.put("localFileEntries", task.getLocalFileEntries());
            taskMap.put("listQpsLimit", task.getListQpsLimit());
            taskMap.put("headQpsLimit", task.getHeadQpsLimit());
            taskMap.put("putQpsLimit", task.getPutQpsLimit());
            taskMap.put("bandwidthBitsPerSecond", task.getBandwidthBitsPerSecond());
            taskMap.put("taskBandwidthLimitMbps", task.getTaskBandwidthLimitMbps());
            taskMap.put("taskBandwidthScopeId", task.getTaskBandwidthScopeId());
            taskList.add(taskMap);
            taskIndex++;
        }

        ResponseEntity<Map> response = restTemplate.postForEntity(url, taskList, Map.class);
        if (!response.getStatusCode().is2xxSuccessful() || response.getBody() == null
                || Boolean.FALSE.equals(response.getBody().get("success"))) {
            Map body = response.getBody();
            String code = body != null && body.get("code") != null
                    ? String.valueOf(body.get("code")) : "WORKER_UNAVAILABLE";
            String message = body != null && body.get("message") != null
                    ? String.valueOf(body.get("message")) : "worker rejected task batch: " + workerIp;
            throw new DownstreamWorkerException(code, message, null);
        }
        if (taskCreationService != null) {
            List<Integer> createdIds = new ArrayList<>();
            for (Map<String, Object> task : taskList) createdIds.add((Integer) task.get("taskIndex"));
            taskCreationService.recordTaskIds(createdIds);
        }
    }

    private boolean isLocalTransfer(List<ListSubTask> tasks) {
        return tasks != null && !tasks.isEmpty() && tasks.get(0).getDirection() != null
                && tasks.get(0).getDirection() != TransferDirection.OSS_TO_OSS;
    }

    public Map<String, Object> aggregateStatus() {
        List<String> workers = distributedConfig.getWorkerIps();
        Map<String, Object> result = new HashMap<>();

        int totalTasks = 0;
        int pendingTasks = 0;
        int runningTasks = 0;
        int completedTasks = 0;
        int partialFailedTasks = 0;
        int failedTasks = 0;
        long totalListObjectCount = 0;
        long totalListObjectSize = 0;
        long totalPutObjectCount = 0;
        long totalPutObjectSize = 0;
        long totalFailedObjectCount = 0;
        long totalFailedObjectSize = 0;
        List<Object> allTasks = new ArrayList<>();

        for (String workerIp : workers) {
            try {
                String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/status";
                ResponseEntity<Map> response = restTemplate.getForEntity(url, Map.class);
                Map<String, Object> workerStatus = response.getBody();

                if (workerStatus != null) {
                    totalTasks += ((Number) workerStatus.getOrDefault("totalTasks", 0)).intValue();
                    pendingTasks += ((Number) workerStatus.getOrDefault("pendingTasks", 0)).intValue();
                    runningTasks += ((Number) workerStatus.getOrDefault("runningTasks", 0)).intValue();
                    completedTasks += ((Number) workerStatus.getOrDefault("completedTasks", 0)).intValue();
                    partialFailedTasks += ((Number) workerStatus.getOrDefault("partialFailedTasks", 0)).intValue();
                    failedTasks += ((Number) workerStatus.getOrDefault("failedTasks", 0)).intValue();

                    if (workerStatus.containsKey("totalListObjectCount")) {
                        totalListObjectCount += ((Number) workerStatus.get("totalListObjectCount")).longValue();
                    }
                    if (workerStatus.containsKey("totalListObjectSize")) {
                        totalListObjectSize += statusBytes(workerStatus.get("totalListObjectSize"));
                    }
                    if (workerStatus.containsKey("totalPutObjectCount")) {
                        totalPutObjectCount += ((Number) workerStatus.get("totalPutObjectCount")).longValue();
                    }
                    if (workerStatus.containsKey("totalPutObjectSize")) {
                        totalPutObjectSize += statusBytes(workerStatus.get("totalPutObjectSize"));
                    }
                    if (workerStatus.containsKey("totalFailedObjectCount")) {
                        totalFailedObjectCount += ((Number) workerStatus.get("totalFailedObjectCount")).longValue();
                    }
                    if (workerStatus.containsKey("totalFailedObjectSize")) {
                        totalFailedObjectSize += statusBytes(workerStatus.get("totalFailedObjectSize"));
                    }

                    if (workerStatus.containsKey("tasks")) {
                        allTasks.addAll((List<Object>) workerStatus.get("tasks"));
                    }
                }
            } catch (Exception e) {
                log.error("获取工作节点 {} 状态失败", workerIp, e);
            }
        }

        result.put("totalTasks", totalTasks);
        result.put("pendingTasks", pendingTasks);
        result.put("runningTasks", runningTasks);
        result.put("completedTasks", completedTasks);
        result.put("partialFailedTasks", partialFailedTasks);
        result.put("failedTasks", failedTasks);
        result.put("totalListObjectCount", totalListObjectCount);
        result.put("totalListObjectSize", formatSize(totalListObjectSize));
        result.put("totalPutObjectCount", totalPutObjectCount);
        result.put("totalPutObjectSize", formatSize(totalPutObjectSize));
        result.put("totalFailedObjectCount", totalFailedObjectCount);
        result.put("totalFailedObjectSize", formatSize(totalFailedObjectSize));
        result.put("tasks", allTasks);

        return result;
    }

    public void startAllWorkers(String sourceBucket, String destBucket, List<Integer> taskIds) {
        List<String> workers = distributedConfig.getWorkerIps();
        for (String workerIp : workers) {
            try {
                String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/start";
                restTemplate.postForEntity(url, controlBody(sourceBucket, destBucket, taskIds), Map.class);
                log.info("启动工作节点 {} 的迁移任务", workerIp);
            } catch (Exception e) {
                log.error("启动工作节点 {} 失败", workerIp, e);
            }
        }
    }

    public void stopAllWorkers() {
        List<String> workers = distributedConfig.getWorkerIps();
        for (String workerIp : workers) {
            try {
                String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/stop";
                restTemplate.postForEntity(url, Collections.emptyMap(), Map.class);
                log.info("停止工作节点 {} 的迁移任务", workerIp);
            } catch (Exception e) {
                log.error("停止工作节点 {} 失败", workerIp, e);
            }
        }
    }

    public void clearAllWorkers(String sourceBucket, String destBucket, List<Integer> taskIds) {
        List<String> workers = distributedConfig.getWorkerIps();
        for (String workerIp : workers) {
            try {
                String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/clear";
                restTemplate.postForEntity(url, controlBody(sourceBucket, destBucket, taskIds), Map.class);
                log.info("清空工作节点 {} 的任务", workerIp);
            } catch (Exception e) {
                log.error("清空工作节点 {} 失败", workerIp, e);
            }
        }
    }

    private Map<String, Object> controlBody(String sourceBucket, String destBucket, List<Integer> taskIds) {
        Map<String, Object> body = new HashMap<>();
        if (sourceBucket != null) body.put("sourceBucket", sourceBucket);
        if (destBucket != null) body.put("destBucket", destBucket);
        if (taskIds != null && !taskIds.isEmpty()) body.put("taskIds", taskIds);
        return body;
    }

    private long statusBytes(Object value) {
        return value instanceof Number ? ((Number)value).longValue()
                : parseSizeToBytes(value == null ? null : value.toString());
    }

    private long parseSizeToBytes(String sizeStr) {
        if (sizeStr == null || sizeStr.isEmpty()) {
            return 0;
        }
        try {
            String[] parts = sizeStr.split(" ");
            if (parts.length != 2) {
                return 0;
            }
            double value = Double.parseDouble(parts[0]);
            String unit = parts[1];

            switch (unit) {
                case "B": return (long) value;
                case "KB": return (long) (value * 1024);
                case "MB": return (long) (value * 1024 * 1024);
                case "GB": return (long) (value * 1024 * 1024 * 1024);
                case "TB": return (long) (value * 1024 * 1024 * 1024 * 1024);
                case "PB": return (long) (value * 1024 * 1024 * 1024 * 1024 * 1024);
                default: return 0;
            }
        } catch (Exception e) {
            return 0;
        }
    }

    private String formatSize(long bytes) {
        if (bytes == 0) return "0 B";
        int unit = 1024;
        String[] units = {"B", "KB", "MB", "GB", "TB", "PB"};
        int i = (int) (Math.log(bytes) / Math.log(unit));
        return String.format("%.3f %s", bytes / Math.pow(unit, i), units[i]);
    }
}
