package com.transfer.tools.controller;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.HeadBucketRequest;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.ExploreExecutorService;
import com.transfer.tools.executor.ListExecutionService;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.model.RateLimitAdjustment;
import com.transfer.tools.model.S3ClientConfig;
import com.transfer.tools.model.TaskImport;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.SubTaskStatusVO;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.service.BucketExploreService;
import com.transfer.tools.service.DistributedService;
import com.transfer.tools.service.RateLimiterManager;
import com.transfer.tools.service.S3ClientManager;
import com.transfer.tools.service.SubTaskManagerService;
import com.transfer.tools.service.EndpointNormalizationService;
import com.transfer.tools.service.MasterLocalTransferService;
import com.transfer.tools.service.TaskCreationService;
import com.transfer.tools.service.MasterTaskBandwidthService;
import com.transfer.tools.service.TransferValidationException;
import com.transfer.tools.service.TaskLifecycleService;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.web.bind.annotation.*;

import java.util.ArrayList;
import java.util.Collection;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import com.fasterxml.jackson.databind.ObjectMapper;

@Slf4j
@RestController
@RequestMapping("/api/master")
public class MasterController {

    private final org.springframework.web.client.RestTemplate nodeHttp = nodeHttpClient();

    private static org.springframework.web.client.RestTemplate nodeHttpClient() {
        org.springframework.http.client.SimpleClientHttpRequestFactory factory =
                new org.springframework.http.client.SimpleClientHttpRequestFactory();
        factory.setConnectTimeout(5000);
        factory.setReadTimeout(5000);
        return new org.springframework.web.client.RestTemplate(factory);
    }

    @Autowired private MasterTaskBandwidthService taskBandwidth;
    @Autowired
    private DistributedConfig distributedConfig;


    @Autowired
    private DistributedService distributedService;

    @Autowired
    private ExploreExecutorService exploreExecutorService;

    @Autowired
    private SubTaskManagerService subTaskManagerService;

    @Autowired
    private RateLimiterManager rateLimiterManager;

    @Autowired
    private S3ClientManager s3ClientManager;

    @Autowired
    private BucketExploreService bucketExploreService;

    @Autowired
    private ListExecutionService listExecutionService;

    @Autowired
    private EndpointNormalizationService endpointNormalizationService;

    @Autowired
    private MasterLocalTransferService masterLocalTransferService;

    @Autowired private TaskCreationService taskCreationService;
    @Autowired private com.transfer.tools.service.OssTaskGroupService ossTaskGroups;
    @Autowired private TaskLifecycleService taskLifecycleService;
    @Autowired private com.transfer.tools.service.NodeConfigService nodeConfigService;
    @Autowired private com.transfer.tools.service.NodeRegistryService nodeRegistryService;

    @GetMapping("/task-creations/{creationId}")
    public Map<String, Object> taskCreation(@PathVariable String creationId) {
        Map<String, Object> result = new HashMap<>();
        TaskCreationService.Creation creation = taskCreationService.get(creationId);
        result.put("success", creation != null);
        if (creation == null) result.put("message", "未找到此次任务创建记录");
        else result.put("creation", creation);
        return result;
    }

    @PostMapping("/stop")
    public Map<String, Object> stopTasks(@RequestBody Map<String, Object> params) {
        if (hasOssGroup(params)) return controlOssGroups("stop", params);
        return controlWorkerTasks("stop", params);
    }

    @PostMapping("/resume")
    public Map<String, Object> resumeTasks(@RequestBody Map<String, Object> params) {
        if (hasOssGroup(params)) return controlOssGroups("resume", params);
        return controlWorkerTasks("resume", params);
    }

    private Map<String, Object> controlWorkerTasks(String operation, Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        if (!(params.get("taskIds") instanceof Collection) || ((Collection<?>)params.get("taskIds")).isEmpty()) {
            result.put("success", false); result.put("message", "请指定任务 ID"); return result;
        }
        int accepted = 0, failed = 0;
        Set<Integer> matched = new HashSet<>();
        List<String> failedWorkers = new ArrayList<>();
        List<String> errors = new ArrayList<>();
        for (String worker : distributedConfig.getWorkerIps()) {
            try {
                String response = callRemoteApi("http://" + worker + ":" + distributedConfig.getPort() + "/api/worker/" + operation, "POST", params);
                if (response == null) throw new IllegalStateException("节点不可达");
                Map data = new ObjectMapper().readValue(response, Map.class);
                accepted += ((Number)data.getOrDefault("accepted", 0)).intValue();
                failed += ((Number)data.getOrDefault("failed", 0)).intValue();
                Object ids = data.get("matchedTaskIds");
                if (ids instanceof Collection) for (Object id : (Collection<?>)ids) matched.add(((Number)id).intValue());
                if (!Boolean.TRUE.equals(data.get("success"))) errors.add(worker + ": " + data.get("message"));
            } catch (Exception e) { failedWorkers.add(worker); errors.add(worker + ": " + e.getMessage()); }
        }
        result.put("accepted", accepted); result.put("failed", failed);
        result.put("ignored", Math.max(0, matched.size() - accepted - failed));
        result.put("partial", !failedWorkers.isEmpty() || failed > 0);
        result.put("failedWorkers", failedWorkers); result.put("details", errors);
        result.put("success", !matched.isEmpty() && errors.isEmpty() && failed == 0);
        result.put("message", matched.isEmpty() ? "未找到指定任务" : errors.isEmpty() ? "操作已执行" : "部分任务操作失败，请检查节点状态");
        return result;
    }

    @GetMapping("/status")
    public Map<String, Object> statusView(@RequestParam(required = false) String sourceBucket,
                                      @RequestParam(required = false) String destBucket,
                                      @RequestParam(required = false) String status,
                                      @RequestParam(required = false) String workerNode,
                                      @RequestParam(defaultValue = "true") boolean includeSubtasks,
                                      @RequestHeader(value = "X-Task-View", required = false) String view) {
        Map<String, Object> result = status(sourceBucket, destBucket, status, workerNode);
        if (!includeSubtasks || "summary".equals(view)) {
            Object tasks = result.get("tasks");
            if (tasks instanceof Collection) for (Object task : (Collection<?>) tasks)
                if (task instanceof Map) ((Map<?, ?>) task).remove("shards");
        }
        return result;
    }

    public Map<String, Object> status(String sourceBucket, String destBucket, String status, String workerNode) {
        try {
            List<String> workers = distributedConfig.getWorkerIps();
            Map<String, Object> result = new HashMap<>();

            int totalTasks = 0;
            int pendingTasks = 0;
            int runningTasks = 0;
            int completedTasks = 0;
            int partialFailedTasks = 0;
            int failedTasks = 0;
            int listDoneTasks = 0;
            long totalListObjectCount = 0;
            long totalListObjectSize = 0;
            long totalPutObjectCount = 0;
            long totalPutObjectSize = 0;
            long totalFailedObjectCount = 0;
            long totalFailedObjectSize = 0;
            long totalSkippedObjectCount = 0;
            long totalSkippedObjectSize = 0;
            List<Object> allTasks = new ArrayList<>();
            List<String> unavailableWorkers = new ArrayList<>();

            for (String workerIp : workers) {
                try {
                    StringBuilder urlBuilder = new StringBuilder();
                    urlBuilder.append("http://").append(workerIp).append(":").append(distributedConfig.getPort());
                    urlBuilder.append("/api/worker/status");

                    // Group complete worker snapshots before applying filters; filtering shards
                    // first can falsely turn a partly running migration into a completed task.
                    String url = urlBuilder.toString();
                    String response = callRemoteApi(url, "GET", null);
                    if (response != null) {
                        Map<String, Object> workerStatus = new ObjectMapper()
                            .readValue(response, Map.class);

                        List<Object> tasks = (List<Object>) workerStatus.get("tasks");
                        if (tasks == null || Boolean.FALSE.equals(workerStatus.get("success")))
                            throw new IllegalStateException("节点任务列表不完整");
                        appendOssTopLevelTasks(allTasks, tasks);
                    } else throw new IllegalStateException("节点不可达");
                } catch (Exception e) {
                    unavailableWorkers.add(workerIp);
                    log.error("获取工作节点 {} 状态失败", workerIp, e);
                }
            }

            if (ossTaskGroups != null) allTasks = ossTaskGroups.group(allTasks, false, !unavailableWorkers.isEmpty());
            allTasks.removeIf(item -> !matchesTaskFilters(item, sourceBucket, destBucket, status, workerNode));

            Map<String, Object> localStatus = masterLocalTransferService.aggregateStatus(
                    null, null, workerNode, status, null, null);
            Object localTasks = localStatus.get("tasks");
            if (localTasks instanceof Collection) {
                for (Object item : (Collection<?>) localTasks) {
                    SubTaskStatus task = new ObjectMapper().convertValue(item, SubTaskStatus.class);
                    if (sourceBucket != null && !sourceBucket.isEmpty()
                            && !sourceBucket.equals(task.getSourceBucket())) continue;
                    if (destBucket != null && !destBucket.isEmpty()
                            && !destBucket.equals(task.getDestBucket())) continue;
                    allTasks.add(SubTaskStatusVO.from(task));
                }
            }

            for (Object item : allTasks) {
                Map task = new ObjectMapper().convertValue(item, Map.class);
                totalTasks++;
                String taskStatus = String.valueOf(task.get("status"));
                if ("PENDING".equals(taskStatus)) pendingTasks++;
                else if ("RUNNING".equals(taskStatus)) runningTasks++;
                else if ("COMPLETED".equals(taskStatus)) completedTasks++;
                else if ("PARTIAL_FAILED".equals(taskStatus)) partialFailedTasks++;
                else if ("FAILED".equals(taskStatus)) failedTasks++;
                else if ("LIST_DONE".equals(taskStatus)) listDoneTasks++;
                totalListObjectCount += number(task.get("listObjectCount"));
                totalListObjectSize += number(task.get("listObjectSize"));
                totalPutObjectCount += number(task.get("putObjectCount"));
                totalPutObjectSize += number(task.get("putObjectSize"));
                totalFailedObjectCount += number(task.get("failedObjectCount"));
                totalFailedObjectSize += number(task.get("failedObjectSize"));
                totalSkippedObjectCount += number(task.get("skippedObjectCount"));
                totalSkippedObjectSize += number(task.get("skippedObjectSize"));
            }

            result.put("totalTasks", totalTasks);
            result.put("pendingTasks", pendingTasks);
            result.put("runningTasks", runningTasks);
            result.put("completedTasks", completedTasks);
            result.put("partialFailedTasks", partialFailedTasks);
            result.put("failedTasks", failedTasks);
            result.put("listDoneTasks", listDoneTasks);
            result.put("totalListObjectCount", totalListObjectCount);
            result.put("totalListObjectSize", totalListObjectSize);
            result.put("totalPutObjectCount", totalPutObjectCount);
            result.put("totalPutObjectSize", totalPutObjectSize);
            result.put("totalFailedObjectCount", totalFailedObjectCount);
            result.put("totalFailedObjectSize", totalFailedObjectSize);
            result.put("totalSkippedObjectCount", totalSkippedObjectCount);
            result.put("totalSkippedObjectSize", totalSkippedObjectSize);
            result.put("tasks", allTasks);
            Object localUnavailable = localStatus.get("unreachableWorkers");
            if (localUnavailable instanceof Collection) for (Object node : (Collection<?>) localUnavailable)
                if (!unavailableWorkers.contains(String.valueOf(node))) unavailableWorkers.add(String.valueOf(node));
            result.put("partial", !unavailableWorkers.isEmpty() || Boolean.TRUE.equals(localStatus.get("partial")));
            result.put("unreachableWorkers", unavailableWorkers);

            return result;
        } catch (Exception e) {
            log.error("获取分布式状态失败", e);
            Map<String, Object> result = new HashMap<>();
            result.put("success", false);
            result.put("message", "获取状态失败: " + e.getMessage());
            return result;
        }
    }

    private long number(Object value) {
        return value instanceof Number ? ((Number) value).longValue() : 0L;
    }

    private boolean matchesTaskFilters(Object value, String source, String destination, String state, String worker) {
        Map task = new ObjectMapper().convertValue(value, Map.class);
        if (source != null && !source.isEmpty() && !source.equals(task.get("sourceBucket"))) return false;
        if (destination != null && !destination.isEmpty() && !destination.equals(task.get("destBucket"))) return false;
        if (state != null && !state.isEmpty() && !state.equals(String.valueOf(task.get("status")))) return false;
        if (worker == null || worker.isEmpty()) return true;
        Object participants = task.get("participatingWorkerNodes");
        return worker.equals(task.get("workerNode")) || worker.equals(task.get("executionNodeIp"))
                || (participants instanceof Collection && ((Collection<?>) participants).contains(worker));
    }

    private List<Integer> requestedTaskIds(Map<String, Object> params) {
        List<Integer> ids = new ArrayList<>();
        Object raw = params == null ? null : params.get("taskIds");
        if (raw instanceof Collection) for (Object value : (Collection<?>) raw) {
            if (!(value instanceof Number) || ((Number) value).doubleValue() != ((Number) value).intValue())
                throw new IllegalArgumentException("任务 ID 必须为整数");
            if (!ids.contains(((Number) value).intValue())) ids.add(((Number) value).intValue());
        }
        return ids;
    }

    private boolean hasOssGroup(Map<String, Object> params) {
        if (taskCreationService == null) return false;
        try {
            for (Integer id : requestedTaskIds(params))
                if (taskCreationService.findByTaskId(id) != null) return true;
        } catch (IllegalArgumentException ignored) { /* Existing handler returns validation errors. */ }
        return false;
    }

    /** Public OSS IDs address the complete creation, never just its first worker range. */
    private Map<String, Object> controlOssGroups(String operation, Map<String, Object> params) {
        synchronized (taskCreationService) {
            return controlOssGroupsLocked(operation, params);
        }
    }

    private Map<String, Object> controlOssGroupsLocked(String operation, Map<String, Object> params) {
        Map<String, Object> result = new java.util.LinkedHashMap<>();
        List<String> failedWorkers = new ArrayList<>(), details = new ArrayList<>();
        int accepted = 0, failed = 0, ignored = 0, resumedScans = 0;
        boolean scanning = false;
        try {
            List<Integer> requested = requestedTaskIds(params);
            if ("stop".equals(operation)) taskCreationService.stopByTaskIds(requested);
            else if (!"resume".equals(operation)) taskCreationService.ensureReadyToStart(requested);
            List<Integer> ids = taskCreationService.expandTaskIds(requested);
            Map<String, Set<Integer>> plannedByWorker = new java.util.LinkedHashMap<>();
            for (Integer id : requested) {
                TaskCreationService.Creation creation = taskCreationService.findByTaskId(id);
                if (creation == null) continue;
                if ("resume".equals(operation) && creation.isDeleted())
                    throw new IllegalStateException("任务已删除，不能继续扫描");
                for (TaskCreationService.Dispatch batch : creation.getPendingDispatches()) {
                    if (batch.getWorker() == null) continue;
                    Set<Integer> planned = plannedByWorker.computeIfAbsent(batch.getWorker(), key -> new java.util.LinkedHashSet<>());
                    for (int i = 0; i < batch.getTasks().size(); i++) planned.add(batch.getFirstTaskId() + i);
                }
            }
            Map<String, List<Map>> snapshots = new java.util.LinkedHashMap<>();
            Set<Integer> found = new HashSet<>();
            for (String worker : distributedConfig.getWorkerIps()) {
                try {
                    String raw = callRemoteApi("http://" + worker + ":" + distributedConfig.getPort() + "/api/worker/status", "GET", null);
                    Map data = raw == null ? null : new ObjectMapper().readValue(raw, Map.class);
                    if (data == null || Boolean.FALSE.equals(data.get("success")) || !(data.get("tasks") instanceof Collection))
                        throw new IllegalStateException("节点未返回完整任务列表");
                    List<Map> selected = new ArrayList<>();
                    for (Object item : (Collection<?>) data.get("tasks")) {
                        Map row = new ObjectMapper().convertValue(item, Map.class);
                        Object id = row.get("taskIndex");
                        if (id instanceof Number && ids.contains(((Number) id).intValue())) {
                            selected.add(row); found.add(((Number) id).intValue());
                        }
                    }
                    snapshots.put(worker, selected);
                } catch (Exception e) { failedWorkers.add(worker); details.add(worker + ": " + e.getMessage()); }
            }
            if (!"stop".equals(operation) && !failedWorkers.isEmpty())
                throw new IllegalStateException("部分执行节点不可达，尚未执行操作");
            Set<Integer> scanningIds = new HashSet<>();
            if ("resume".equals(operation)) {
                for (Integer requestedId : requested) {
                    TaskCreationService.Creation creation = taskCreationService.findByTaskId(requestedId);
                    if (creation != null && !Boolean.TRUE.equals(creation.getScanComplete())) {
                        // Unacknowledged planned ranges are replayed by discovery, not started here.
                        if (java.util.Collections.disjoint(found, creation.getTaskIds())
                                && creation.getPendingDispatches().isEmpty())
                            throw new IllegalStateException("任务执行记录不存在，无法恢复扫描");
                        if (Boolean.TRUE.equals(creation.getAutoStart())) {
                            // Automatic scans may already have executing/stopped ranges. Resume
                            // those now; only undispatched planned IDs belong to scanner replay.
                            for (TaskCreationService.Dispatch batch : creation.getPendingDispatches())
                                for (int i = 0; i < batch.getTasks().size(); i++) {
                                    int id = batch.getFirstTaskId() + i;
                                    if (!found.contains(id)) scanningIds.add(id);
                                }
                        } else scanningIds.addAll(creation.getTaskIds());
                    }
                }
                scanning = !scanningIds.isEmpty();
            }
            Set<Integer> executionIds = new HashSet<>(ids);
            executionIds.removeAll(scanningIds);
            if (!"stop".equals(operation) && !found.containsAll(executionIds))
                throw new IllegalStateException("执行子任务记录不完整，尚未执行操作");
            if ("stop".equals(operation)) {
                // A lost dispatch response does not prove the worker rejected the request.
                // Fence every durable planned ID, including records absent from its snapshot.
                for (Map.Entry<String, Set<Integer>> entry : plannedByWorker.entrySet()) {
                    List<Map> rows = snapshots.computeIfAbsent(entry.getKey(), key -> new ArrayList<>());
                    Set<Integer> known = new HashSet<>();
                    for (Map row : rows) known.add(((Number) row.get("taskIndex")).intValue());
                    for (Integer id : entry.getValue()) if (!known.contains(id)) {
                        Map<String,Object> absent = new HashMap<>();
                        absent.put("taskIndex", id); absent.put("status", "PENDING"); rows.add(absent);
                    }
                }
            }
            if ("resume".equals(operation)) {
                // Clear a stop fence before replaying a not-yet-visible planned request.
                // If it arrived meanwhile, the worker resumes the retained record instead.
                for (Map.Entry<String, Set<Integer>> entry : plannedByWorker.entrySet()) {
                    List<Integer> missing = new ArrayList<>();
                    for (Integer id : entry.getValue()) if (!found.contains(id)) missing.add(id);
                    if (missing.isEmpty()) continue;
                    try {
                        Map response = checkedWorkerResponse(entry.getKey(), "/resume", "POST",
                                java.util.Collections.singletonMap("taskIds", missing));
                        accepted += response.get("accepted") instanceof Number ? ((Number) response.get("accepted")).intValue() : missing.size();
                    } catch (Exception e) {
                        failed += missing.size(); failedWorkers.add(entry.getKey());
                        throw new IllegalStateException("解除待分发子任务的停止状态失败：" + entry.getKey() + ": " + e.getMessage());
                    }
                }
                for (BucketPair scan : taskCreationService.resumeByTaskIds(requested)) {
                    if (exploreExecutorService.offerExploreTask(scan)) {
                        accepted++; resumedScans++;
                    } else {
                        taskCreationService.fail(scan.getCreationId(), "扫描队列已满，请稍后继续");
                        failed++; details.add("扫描队列已满，请稍后继续");
                    }
                }
            }
            for (Map.Entry<String, List<Map>> entry : snapshots.entrySet()) {
                Map<String, List<Integer>> actions = new java.util.LinkedHashMap<>();
                for (Map row : entry.getValue()) {
                    if (scanningIds.contains(((Number) row.get("taskIndex")).intValue())) continue;
                    String state = String.valueOf(row.get("status"));
                    String action;
                    if ("stop".equals(operation)) action = "COMPLETED".equals(state) ? null : "stop";
                    else if ("retry-task".equals(operation)) action = java.util.Arrays.asList("FAILED", "PARTIAL_FAILED", "STOPPED").contains(state) ? "retry-task" : null;
                    else if ("PENDING".equals(state)) action = "start";
                    else if (java.util.Arrays.asList("STOPPED", "FAILED", "PARTIAL_FAILED").contains(state)) action = "resume";
                    else action = null;
                    if (action == null) { ignored++; continue; }
                    actions.computeIfAbsent(action, key -> new ArrayList<>()).add(((Number) row.get("taskIndex")).intValue());
                }
                for (Map.Entry<String, List<Integer>> action : actions.entrySet()) {
                    try {
                        Map response = checkedWorkerResponse(entry.getKey(), "/" + action.getKey(), "POST",
                                java.util.Collections.singletonMap("taskIds", action.getValue()));
                        accepted += response.get("accepted") instanceof Number ? ((Number) response.get("accepted")).intValue() : action.getValue().size();
                        ignored += response.get("ignored") instanceof Number ? ((Number) response.get("ignored")).intValue() : 0;
                    } catch (Exception e) {
                        failed += action.getValue().size();
                        if (!failedWorkers.contains(entry.getKey())) failedWorkers.add(entry.getKey());
                        details.add(entry.getKey() + ": " + e.getMessage());
                    }
                }
            }
            result.put("success", failedWorkers.isEmpty() && failed == 0
                    && (!found.isEmpty() || resumedScans > 0
                    || ("stop".equals(operation) && (accepted > 0 || ignored > 0))));
            result.put("message", failedWorkers.isEmpty() && failed == 0
                    ? (scanning ? "扫描已排队，任务操作已执行" : "任务操作已执行")
                    : "部分操作未完成，请查看详情后重试");
        } catch (Exception error) {
            result.put("success", false); result.put("message", error.getMessage());
        }
        result.put("accepted", accepted); result.put("failed", failed); result.put("ignored", ignored);
        result.put("resumedScans", resumedScans);
        result.put("partial", !failedWorkers.isEmpty() || failed > 0 || (accepted > 0 && !Boolean.TRUE.equals(result.get("success"))));
        result.put("failedWorkers", failedWorkers); result.put("details", details);
        return result;
    }

    void appendOssTopLevelTasks(List<Object> target, Collection<?> workerTasks) {
        if (workerTasks == null) return;
        ObjectMapper mapper = new ObjectMapper();
        for (Object task : workerTasks) {
            Map taskMap = mapper.convertValue(task, Map.class);
            Object direction = taskMap.get("direction");
            if (direction == null || TransferDirection.OSS_TO_OSS.name().equals(String.valueOf(direction))) {
                target.add(task);
            }
        }
    }

    @GetMapping("/worker-status")
    public Map<String, Object> getWorkerStatus() {
        Map<String, Object> result = new HashMap<>();
        try {
            List<String> workers = distributedConfig.getWorkerIps();
            Map<String, Object> allWorkersStatus = new HashMap<>();

            for (String workerIp : workers) {
                try {
                    String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/worker-status";
                    String response = callRemoteApi(url, "GET", null);
                    Map<String, Object> workerStatus = response == null ? null : new ObjectMapper()
                            .readValue(response, Map.class);
                    if (workerStatus == null || !Boolean.TRUE.equals(workerStatus.get("success"))) {
                        throw new IllegalStateException("未收到有效的节点状态");
                    }
                    allWorkersStatus.put(workerIp, workerStatus);
                } catch (Exception e) {
                    log.error("获取工作节点 {} 状态失败", workerIp, e);
                    Map<String, Object> errorStatus = new HashMap<>();
                    errorStatus.put("success", false);
                    errorStatus.put("message", "获取状态失败: " + e.getMessage());
                    allWorkersStatus.put(workerIp, errorStatus);
                }
            }

            result.put("success", true);
            result.put("workers", allWorkersStatus);
        } catch (Exception e) {
            log.error("获取worker状态失败", e);
            result.put("success", false);
            result.put("message", "获取状态失败: " + e.getMessage());
        }
        return result;
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
            String unit = parts[1].toUpperCase();

            switch (unit) {
                case "B":
                    return (long) value;
                case "KB":
                case "K":
                    return (long) (value * 1024);
                case "MB":
                case "M":
                    return (long) (value * 1024 * 1024);
                case "GB":
                case "G":
                    return (long) (value * 1024 * 1024 * 1024);
                case "TB":
                case "T":
                    return (long) (value * 1024 * 1024 * 1024 * 1024);
                default:
                    return 0;
            }
        } catch (Exception e) {
            return 0;
        }
    }

    private String formatSize(long bytes) {
        if (bytes < 1024) {
            return bytes + " B";
        } else if (bytes < 1024 * 1024) {
            return String.format("%.2f KB", bytes / 1024.0);
        } else if (bytes < 1024 * 1024 * 1024) {
            return String.format("%.2f MB", bytes / (1024.0 * 1024));
        } else if (bytes < 1024L * 1024 * 1024 * 1024) {
            return String.format("%.2f GB", bytes / (1024.0 * 1024 * 1024));
        } else {
            return String.format("%.2f TB", bytes / (1024.0 * 1024 * 1024 * 1024));
        }
    }

    @PostMapping("/start")
    public Map<String, Object> start(@RequestBody Map<String, Object> params) {
        if (hasOssGroup(params)) return controlOssGroups("start", params);
        Map<String, Object> result = new HashMap<>();
        try {
            List<Integer> taskIds = (List<Integer>) params.get("taskIds");
            String sourceBucket = (String) params.get("sourceBucket");
            String destBucket = (String) params.get("destBucket");

            Map<String, List<Integer>> workerTaskMap = new HashMap<>();
            List<String> preCheckErrors = new ArrayList<>();

            for (String workerIp : distributedConfig.getWorkerIps()) {
                try {
                    String url;
                    if (taskIds != null && !taskIds.isEmpty()) {
                        url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/task-status?taskIds=" +
                            taskIds.stream().map(String::valueOf).collect(java.util.stream.Collectors.joining(","));
                    } else {
                        url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/pending-tasks";
                        if (sourceBucket != null && !sourceBucket.isEmpty()) {
                            url += "?sourceBucket=" + sourceBucket;
                            if (destBucket != null && !destBucket.isEmpty()) {
                                url += "&destBucket=" + destBucket;
                            }
                        } else if (destBucket != null && !destBucket.isEmpty()) {
                            url += "?destBucket=" + destBucket;
                        }
                    }

                    String response = callRemoteApi(url, "GET", null);

                    if (response != null) {
                        Map<String, Object> workerResult = new ObjectMapper()
                            .readValue(response, Map.class);

                        if (workerResult != null && Boolean.TRUE.equals(workerResult.get("success"))) {
                            if (taskIds != null && !taskIds.isEmpty()) {
                                Map<String, Object> taskStatuses = (Map<String, Object>) workerResult.get("taskStatuses");
                                if (taskStatuses != null) {
                                    for (Integer taskIndex : taskIds) {
                                        if (taskStatuses.containsKey(String.valueOf(taskIndex))) {
                                            Map<String, Object> taskInfo = (Map<String, Object>) taskStatuses.get(String.valueOf(taskIndex));
                                            String statusStr = (String) taskInfo.get("status");

                                            if (!"PENDING".equals(statusStr)) {
                                                preCheckErrors.add("任务" + taskIndex + "（节点" + workerIp + "）: 不支持启动，当前状态为" + statusStr);
                                                continue;
                                            }

                                            if (taskInfo.containsKey("workerNode")) {
                                                String workerNode = (String) taskInfo.get("workerNode");
                                                workerTaskMap.computeIfAbsent(workerNode, k -> new ArrayList<>()).add(taskIndex);
                                            } else {
                                                workerTaskMap.computeIfAbsent(workerIp, k -> new ArrayList<>()).add(taskIndex);
                                            }
                                        }
                                    }
                                }
                            } else {
                                List<Integer> pendingTaskIds = (List<Integer>) workerResult.get("pendingTaskIds");
                                if (pendingTaskIds != null && !pendingTaskIds.isEmpty()) {
                                    workerTaskMap.computeIfAbsent(workerIp, k -> new ArrayList<>()).addAll(pendingTaskIds);
                                }
                            }
                        }
                    }
                } catch (Exception e) {
                    log.error("获取节点 {} 任务状态失败", workerIp, e);
                }
            }

            if (!preCheckErrors.isEmpty()) {
                result.put("success", false);
                result.put("message", "任务预检失败，部分任务不支持启动");
                result.put("details", preCheckErrors);
                return result;
            }

            if (taskCreationService != null) {
                List<Integer> pending = new ArrayList<>();
                for (List<Integer> ids : workerTaskMap.values()) pending.addAll(ids);
                taskCreationService.ensureReadyToStart(pending);
            }

            if (workerTaskMap.isEmpty()) {
                result.put("success", true);
                result.put("message", "没有需要启动的任务");
                result.put("details", new ArrayList<>());
                return result;
            }

            List<String> results = new ArrayList<>();
            int successCount = 0;
            int failCount = 0;

            for (Map.Entry<String, List<Integer>> entry : workerTaskMap.entrySet()) {
                String workerNode = entry.getKey();
                List<Integer> nodeTaskIds = entry.getValue();

                try {
                    String url = "http://" + workerNode + ":" + distributedConfig.getPort() + "/api/worker/start";
                    Map<String, Object> startRequest = new HashMap<>();
                    startRequest.put("taskIds", nodeTaskIds);

                    String response = callRemoteApi(url, "POST", startRequest);
                    if (response != null) {
                        Map<String, Object> workerResult = new ObjectMapper()
                            .readValue(response, Map.class);

                        if (workerResult != null && Boolean.TRUE.equals(workerResult.get("success"))) {
                            results.add("节点 " + workerNode + ": 启动成功 (" + nodeTaskIds.size() + " 个任务)");
                            successCount += nodeTaskIds.size();
                        } else {
                            results.add("节点 " + workerNode + ": " +
                                (workerResult != null ? workerResult.get("message") : "未知错误"));
                            failCount += nodeTaskIds.size();
                        }
                    }
                } catch (Exception e) {
                    log.error("转发启动请求到节点 {} 失败", workerNode, e);
                    results.add("节点 " + workerNode + ": 转发失败 - " + e.getMessage());
                    failCount += nodeTaskIds.size();
                }
            }

            result.put("success", successCount > 0);
            result.put("message", String.format("启动任务已提交，成功: %d, 失败: %d", successCount, failCount));
            result.put("details", results);
        } catch (Exception e) {
            log.error("启动分布式迁移失败", e);
            result.put("success", false);
            result.put("message", "启动失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/clear")
    public Map<String, Object> clear(@RequestBody Map<String, Object> params) {
        Map<String, Object> moved = taskLifecycleService.clear(params, false);
        if (!Boolean.TRUE.equals(params.get("permanent")) || !Boolean.TRUE.equals(moved.get("success"))) return moved;
        return taskLifecycleService.clear(java.util.Collections.singletonMap("taskIds", moved.get("taskIds")), true);
    }

    @GetMapping("/deleted-tasks")
    public Map<String, Object> getDeletedTasks() {
        return taskLifecycleService.deletedTasks();
    }

    @PostMapping("/permanent-delete")
    public Map<String, Object> permanentDeleteTasks(@RequestBody Map<String, Object> params) {
        return taskLifecycleService.clear(params, true);
    }

    @PostMapping("/failed")
    public Map<String, Object> getFailedKeys(@RequestBody(required = false) Map<String, Object> params) {
        params = expandExceptionTaskFilter(params);
        Map<String, Object> result = new HashMap<>();
        Map<String, Object> records = new java.util.LinkedHashMap<>();
        List<String> successful = new ArrayList<>();
        Map<String, String> errors = new java.util.LinkedHashMap<>();
        for (String worker : distributedConfig.getWorkerIps()) {
            try {
                Map<String, Object> response = checkedWorkerResponse(worker, "/failed", "POST", params);
                if (!(response.get("failedKeys") instanceof List) || !(response.get("count") instanceof Number))
                    throw new IllegalStateException("节点返回的异常记录不完整");
                records.put(worker, response.get("failedKeys"));
                records.put(worker + "Count", response.get("count"));
                successful.add(worker);
            } catch (Exception error) { errors.put(worker, error.getMessage()); }
        }
        result.put("failedKeys", records);
        putFailureCompleteness(result, successful, errors);
        result.put("success", !successful.isEmpty());
        if (successful.isEmpty()) result.put("message", "未能读取任何执行节点的异常记录");
        return result;
    }

    @PostMapping("/failed/clear")
    public Map<String, Object> clearFailedKeys() {
        Map<String, Object> result = new HashMap<>();
        List<String> successful = new ArrayList<>();
        Map<String, String> errors = new java.util.LinkedHashMap<>();
        for (String worker : distributedConfig.getWorkerIps()) {
            try {
                checkedWorkerResponse(worker, "/failed/clear", "POST", null);
                successful.add(worker);
            } catch (Exception error) { errors.put(worker, error.getMessage()); }
        }
        putFailureCompleteness(result, successful, errors);
        boolean success = !successful.isEmpty() && errors.isEmpty();
        result.put("success", success);
        result.put("message", success ? "失败对象记录已清空"
                : "清空失败对象记录未全部完成；成功节点：" + successful + "；失败节点：" + errors);
        return result;
    }

    private Map<String, Object> expandExceptionTaskFilter(Map<String, Object> params) {
        if (params == null || taskCreationService == null || !(params.get("taskIndex") instanceof Number)) return params;
        TaskCreationService.Creation creation = taskCreationService.findByTaskId(((Number) params.get("taskIndex")).intValue());
        if (creation == null) return params;
        Map<String, Object> expanded = new java.util.LinkedHashMap<>(params);
        expanded.remove("taskIndex"); expanded.put("taskIds", creation.getTaskIds());
        return expanded;
    }

    private Map<String, Object> checkedWorkerResponse(String worker, String path, String method, Object params) throws Exception {
        String raw = callRemoteApi("http://" + worker + ":" + distributedConfig.getPort() + "/api/worker" + path, method, params);
        Map<String, Object> response = raw == null ? null : new ObjectMapper().readValue(raw, Map.class);
        if (response == null || !Boolean.TRUE.equals(response.get("success")))
            throw new IllegalStateException(response == null ? "节点未返回有效响应" : String.valueOf(response.getOrDefault("message", "节点拒绝请求")));
        return response;
    }

    private void putFailureCompleteness(Map<String, Object> result, List<String> successful, Map<String, String> errors) {
        result.put("complete", !successful.isEmpty() && errors.isEmpty());
        result.put("partial", !successful.isEmpty() && !errors.isEmpty());
        result.put("successfulWorkers", successful);
        result.put("failedWorkers", new ArrayList<>(errors.keySet()));
        result.put("unreachableWorkers", new ArrayList<>(errors.keySet()));
        result.put("workerErrors", errors);
    }

    @PostMapping("/fallback")
    public Map<String, Object> getFallbackObjects(@RequestBody(required = false) Map<String, Object> params) {
        params = expandExceptionTaskFilter(params);
        Map<String, Object> result = new HashMap<>();
        try {
            Map<String, Object> allNodesFallbackObjects = new HashMap<>();

            for (String workerIp : distributedConfig.getWorkerIps()) {
                try {
                    String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/fallback";
                    String response = callRemoteApi(url, "POST", params);
                    if (response != null) {
                        Map<String, Object> workerResult = new ObjectMapper()
                            .readValue(response, Map.class);
                        if (workerResult != null && Boolean.TRUE.equals(workerResult.get("success"))) {
                            allNodesFallbackObjects.put(workerIp, workerResult.get("fallbackObjects"));
                            allNodesFallbackObjects.put(workerIp + "Count", workerResult.get("count"));
                        }
                    }
                } catch (Exception e) {
                    log.warn("获取工作节点 {} 降级对象失败: {}", workerIp, e.getMessage());
                }
            }

            result.put("success", true);
            result.put("fallbackObjects", allNodesFallbackObjects);
        } catch (Exception e) {
            log.error("查询降级迁移对象失败", e);
            result.put("success", false);
            result.put("message", "查询失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/fallback/clear")
    public Map<String, Object> clearFallbackObjects() {
        Map<String, Object> result = new HashMap<>();
        try {
            for (String workerIp : distributedConfig.getWorkerIps()) {
                try {
                    String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/fallback/clear";
                    callRemoteApi(url, "POST", null);
                } catch (Exception e) {
                    log.warn("清空工作节点 {} 降级对象失败: {}", workerIp, e.getMessage());
                }
            }

            result.put("success", true);
            result.put("message", "降级迁移对象记录已清空");
        } catch (Exception e) {
            log.error("清空降级迁移对象记录失败", e);
            result.put("success", false);
            result.put("message", "清空失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/import")
    public Map<String, Object> importTasks(@RequestBody List<TaskImport> taskImports) {
        if (nodeConfigService == null) return importTasksLocked(taskImports);
        return nodeConfigService.withMembershipReadLock(() -> importTasksLocked(taskImports));
    }

    private Map<String, Object> importTasksLocked(List<TaskImport> taskImports) {
        Map<String, Object> result = new HashMap<>();
        try {
            if (taskImports == null || taskImports.isEmpty()) {
                result.put("success", false);
                result.put("message", "导入任务列表为空");
                return result;
            }

            List<String> workerIps = distributedConfig.getWorkerIps();
            if (workerIps == null || workerIps.isEmpty()) {
                result.put("success", false);
                result.put("message", "未配置worker节点");
                return result;
            }

            for (int i = 0; i < taskImports.size(); i++) {
                TaskImport task = taskImports.get(i);
                endpointNormalizationService.normalizeAndValidate(task);
                MasterTaskBandwidthService.validateLimit(task.getTaskBandwidthLimitMbps());
                if (task.getTaskBandwidthScopeId() != null)
                    throw new TransferValidationException("INVALID_TASK_BANDWIDTH", "带宽范围由服务端分配");

                if (task.getSourceBucket() == null || task.getSourceBucket().trim().isEmpty()) {
                    result.put("success", false);
                    result.put("message", "第 " + (i + 1) + " 个任务：sourceBucket不能为空");
                    return result;
                }

                if (task.getDestBucket() == null || task.getDestBucket().trim().isEmpty()) {
                    result.put("success", false);
                    result.put("message", "第 " + (i + 1) + " 个任务：destBucket不能为空");
                    return result;
                }

                if ((task.getDirection() == null || task.getDirection() == TransferDirection.OSS_TO_OSS)
                        && isSameOssBucket(task.getSourceS3Alias(), task.getSourceBucket(),
                                task.getDestS3Alias(), task.getDestBucket())) {
                    result.put("success", false);
                    result.put("message", "第 " + (i + 1) + " 个任务：源和目标指向同一存储桶，请选择不同的迁移目标。");
                    return result;
                }

                if (task.getDateAfter() != null && !task.getDateAfter().trim().isEmpty()) {
                    if (!isValidDateTimeFormat(task.getDateAfter())) {
                        result.put("success", false);
                        result.put("message", "第 " + (i + 1) + " 个任务：dateAfter格式错误: " + task.getDateAfter() +
                            "，正确格式: YYYY-MM-DDTHH:mm:ss 或 YYYY-MM-DD HH:mm:ss");
                        return result;
                    }
                }

                if (task.getDateBefore() != null && !task.getDateBefore().trim().isEmpty()) {
                    if (!isValidDateTimeFormat(task.getDateBefore())) {
                        result.put("success", false);
                        result.put("message", "第 " + (i + 1) + " 个任务：dateBefore格式错误: " + task.getDateBefore() +
                            "，正确格式: YYYY-MM-DDTHH:mm:ss 或 YYYY-MM-DD HH:mm:ss");
                        return result;
                    }
                }

                if (task.getDateAfter() != null && !task.getDateAfter().trim().isEmpty() &&
                    task.getDateBefore() != null && !task.getDateBefore().trim().isEmpty()) {
                    if (!isDateBeforeValid(task.getDateAfter(), task.getDateBefore())) {
                        result.put("success", false);
                        result.put("message", "第 " + (i + 1) + " 个任务：dateBefore必须在dateAfter之后");
                        return result;
                    }
                }

                if (task.getWorkerNode() != null && !task.getWorkerNode().trim().isEmpty()) {
                    if (!workerIps.contains(task.getWorkerNode().trim())) {
                        result.put("success", false);
                        result.put("message", "第 " + (i + 1) + " 个任务：workerNode '" + task.getWorkerNode() +
                            "' 不在配置的worker节点列表中");
                        return result;
                    }
                }
            }

            Map<String, List<TaskImport>> workerTaskMap = new HashMap<>();
            for (String workerIp : workerIps) {
                workerTaskMap.put(workerIp, new ArrayList<>());
            }

            int workerIndex = 0;
            for (TaskImport task : taskImports) {
                String targetWorker;
                if (task.getWorkerNode() != null && !task.getWorkerNode().trim().isEmpty()) {
                    targetWorker = task.getWorkerNode().trim();
                } else {
                    targetWorker = workerIps.get(workerIndex % workerIps.size());
                    workerIndex++;
                }
                if (taskBandwidth != null) {
                    String scope = "creation:" + java.util.UUID.randomUUID();
                    taskBandwidth.initialize(scope, task.getTaskBandwidthLimitMbps(), java.util.Collections.singletonList(targetWorker));
                    task.setTaskBandwidthScopeId(scope);
                }
                task.setAutoStart(true);
                task.setWorkerNode(targetWorker);
                workerTaskMap.get(targetWorker).add(task);
            }

            // OSS discovery and imports share the master's namespace. Worker-local
            // allocation can collide with ranges that already exist on another node.
            int firstTaskId = subTaskManagerService.allocateTaskIdRange(taskImports.size());
            for (int i = 0; i < taskImports.size(); i++) taskImports.get(i).setTaskIndex(firstTaskId + i);

            List<Integer> importedIds = new ArrayList<>(), unconfirmedIds = new ArrayList<>(), pendingScheduleIds = new ArrayList<>();
            List<String> failedWorkers = new ArrayList<>(), details = new ArrayList<>();
            for (Map.Entry<String, List<TaskImport>> entry : workerTaskMap.entrySet()) {
                String workerIp = entry.getKey();
                List<TaskImport> tasks = entry.getValue();
                if (tasks.isEmpty()) continue;
                List<Integer> assignedIds = new ArrayList<>();
                for (TaskImport task : tasks) assignedIds.add(task.getTaskIndex());
                boolean persistedLocally = false;
                try {
                    if (workerIp.equals(distributedConfig.getLocalIp()) || workerIp.equals("127.0.0.1")) {
                        subTaskManagerService.importTasksWithTaskId(tasks);
                        persistedLocally = true;
                        importedIds.addAll(assignedIds);
                        listExecutionService.startExecution(null, null, assignedIds);
                    } else {
                        String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/tasks";
                        String response = callRemoteApi(url, "POST", tasks);
                        Map<String, Object> workerResult = response == null ? null : new ObjectMapper().readValue(response, Map.class);
                        if (workerResult == null || !Boolean.TRUE.equals(workerResult.get("success")))
                            throw new IllegalStateException(workerResult == null ? "节点未返回接收结果"
                                    : workerResult.get("message") == null ? "节点未确认任务接收"
                                    : String.valueOf(workerResult.get("message")));
                        importedIds.addAll(assignedIds);
                    }
                    log.info("向 worker {} 导入 {} 个任务，编号: {}", workerIp, tasks.size(), assignedIds);
                } catch (Exception error) {
                    failedWorkers.add(workerIp);
                    if (!persistedLocally) unconfirmedIds.addAll(assignedIds);
                    else pendingScheduleIds.addAll(assignedIds);
                    details.add(workerIp + "：任务 " + assignedIds + (persistedLocally
                            ? " 已保存，自动执行排队未确认：" : " 接收结果未确认：") + error.getMessage());
                    log.error("向 worker {} 导入指定编号的任务未完全确认", workerIp, error);
                }
            }
            java.util.Collections.sort(importedIds);
            java.util.Collections.sort(unconfirmedIds);
            result.put("success", failedWorkers.isEmpty() || !importedIds.isEmpty());
            result.put("partial", !failedWorkers.isEmpty());
            result.put("message", failedWorkers.isEmpty() ? "成功导入 " + importedIds.size() + " 个任务"
                    : "已确认保存 " + importedIds.size() + " 个任务；接收未确认编号 " + unconfirmedIds
                    + "，已保存但排队未确认编号 " + pendingScheduleIds + "。请核对任务列表后再重试");
            result.put("importedCount", importedIds.size());
            result.put("taskIds", importedIds);
            result.put("unconfirmedTaskIds", unconfirmedIds);
            result.put("unknownCount", unconfirmedIds.size());
            // A negative/failed worker response may occur after persistence, so it is
            // not evidence that no task was created on that worker.
            result.put("rejectedCount", 0);
            result.put("pendingScheduleTaskIds", pendingScheduleIds);
            result.put("failedWorkers", failedWorkers);
            result.put("details", details);
            result.put("validationErrors", details);
        } catch (Exception e) {
            log.error("导入任务失败", e);
            result.put("success", false);
            result.put("message", "导入失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/add-buckets")
    public Map<String, Object> addBucketPairs(@RequestBody List<BucketPair> bucketPairs) {
        if (nodeConfigService == null) return addBucketPairsLocked(bucketPairs);
        return nodeConfigService.withMembershipReadLock(() -> addBucketPairsLocked(bucketPairs));
    }

    private Map<String, Object> addBucketPairsLocked(List<BucketPair> bucketPairs) {
        Map<String, Object> result = new HashMap<>();
        try {
            if (bucketPairs == null || bucketPairs.isEmpty()) {
                result.put("success", false);
                result.put("message", "bucket对列表不能为空");
                return result;
            }

            for (BucketPair bucketPair : bucketPairs) {
                if (bucketPair != null) {
                    MasterTaskBandwidthService.validateLimit(bucketPair.getTaskBandwidthLimitMbps());
                    if (bucketPair.getTaskBandwidthScopeId() != null)
                        throw new TransferValidationException("INVALID_TASK_BANDWIDTH", "带宽范围由服务端分配");
                    endpointNormalizationService.normalize(bucketPair);
                    bucketPair.setEligibleWorkerNodes(MasterTaskBandwidthService.selectMembers(
                            bucketPair.getEligibleWorkerNodes(), distributedConfig.getWorkerIps()));
                }
                if (bucketPair == null || bucketPair.getSourceBucket() == null || bucketPair.getDestBucket() == null) {
                    result.put("success", false);
                    result.put("message", "源bucket和目标bucket不能为空");
                    return result;
                }

                if (bucketPair.getDirection() == TransferDirection.OSS_TO_OSS
                        && isSameOssBucket(bucketPair.getSourceS3Alias(), bucketPair.getSourceBucket(),
                                bucketPair.getDestS3Alias(), bucketPair.getDestBucket())) {
                    result.put("success", false);
                    result.put("message", "源和目标指向同一存储桶，请选择不同的迁移目标。");
                    return result;
                }

                if (bucketPair.getDateAfter() != null && !bucketPair.getDateAfter().trim().isEmpty()) {
                    if (!isValidDateTimeFormat(bucketPair.getDateAfter())) {
                        result.put("success", false);
                        result.put("message", "dateAfter格式错误: " + bucketPair.getDateAfter() +
                            "，正确格式: YYYY-MM-DDTHH:mm:ss 或 YYYY-MM-DD HH:mm:ss");
                        return result;
                    }
                }

                if (bucketPair.getDateBefore() != null && !bucketPair.getDateBefore().trim().isEmpty()) {
                    if (!isValidDateTimeFormat(bucketPair.getDateBefore())) {
                        result.put("success", false);
                        result.put("message", "dateBefore格式错误: " + bucketPair.getDateBefore() +
                            "，正确格式: YYYY-MM-DDTHH:mm:ss 或 YYYY-MM-DD HH:mm:ss");
                        return result;
                    }
                }

                if (bucketPair.getDateAfter() != null && !bucketPair.getDateAfter().trim().isEmpty() &&
                    bucketPair.getDateBefore() != null && !bucketPair.getDateBefore().trim().isEmpty()) {
                    if (!isDateBeforeValid(bucketPair.getDateAfter(), bucketPair.getDateBefore())) {
                        result.put("success", false);
                        result.put("message", "dateBefore必须在dateAfter之后");
                        return result;
                    }
                }
            }

            Map<String, Set<String>> aliasToSourceBuckets = new HashMap<>();
            Map<String, Set<String>> aliasToDestBuckets = new HashMap<>();
            for (BucketPair bucketPair : bucketPairs) {
                String srcAlias = bucketPair.getSourceS3Alias();
                String destAlias = bucketPair.getDestS3Alias();

                if (srcAlias != null && !srcAlias.isEmpty()) {
                    aliasToSourceBuckets.computeIfAbsent(srcAlias, k -> new HashSet<>()).add(bucketPair.getSourceBucket());
                } else {
                    aliasToSourceBuckets.computeIfAbsent("default", k -> new HashSet<>()).add(bucketPair.getSourceBucket());
                }

                if (destAlias != null && !destAlias.isEmpty()) {
                    aliasToDestBuckets.computeIfAbsent(destAlias, k -> new HashSet<>()).add(bucketPair.getDestBucket());
                } else {
                    aliasToDestBuckets.computeIfAbsent("default", k -> new HashSet<>()).add(bucketPair.getDestBucket());
                }
            }

            for (Map.Entry<String, Set<String>> entry : aliasToSourceBuckets.entrySet()) {
                String alias = entry.getKey();
                Set<String> buckets = entry.getValue();

                AmazonS3 s3Client = s3ClientManager.getClient(alias);
                if (s3Client == null) {
                    result.put("success", false);
                    result.put("message", "源S3配置不存在: " + alias);
                    return result;
                }

                log.info("开始校验源bucket是否存在，alias: {}, buckets: {}", alias, buckets);
                List<String> invalidBuckets = new ArrayList<>();
                for (String bucket : buckets) {
                    try {
                        s3Client.headBucket(new HeadBucketRequest(bucket));
                    } catch (Exception e) {
                        log.warn("源bucket不存在或无List源bucket权限: {}, alias: {}, error: {}", bucket, alias, e.getMessage());
                        invalidBuckets.add(bucket);
                    }
                }

                if (!invalidBuckets.isEmpty()) {
                    String errorMsg = "源bucket不存在或无List源bucket权限 (alias: " + alias + "): " + String.join(", ", invalidBuckets);
                    log.error(errorMsg);
                    result.put("success", false);
                    result.put("message", errorMsg);
                    return result;
                }
            }

            for (Map.Entry<String, Set<String>> entry : aliasToDestBuckets.entrySet()) {
                String alias = entry.getKey();
                Set<String> buckets = entry.getValue();

                AmazonS3 s3Client = s3ClientManager.getClient(alias);
                if (s3Client == null) {
                    result.put("success", false);
                    result.put("message", "目标S3配置不存在: " + alias);
                    return result;
                }

                log.info("开始校验目标bucket是否存在，alias: {}, buckets: {}", alias, buckets);
                List<String> invalidBuckets = new ArrayList<>();
                for (String bucket : buckets) {
                    try {
                        s3Client.headBucket(new HeadBucketRequest(bucket));
                    } catch (Exception e) {
                        log.warn("目标bucket不存在或无List目标bucket权限: {}, alias: {}, error: {}", bucket, alias, e.getMessage());
                        invalidBuckets.add(bucket);
                    }
                }

                if (!invalidBuckets.isEmpty()) {
                    String errorMsg = "目标bucket不存在或无List目标bucket权限 (alias: " + alias + "): " + String.join(", ", invalidBuckets);
                    log.error(errorMsg);
                    result.put("success", false);
                    result.put("message", errorMsg);
                    return result;
                }
            }

            log.info("所有bucket校验通过");

            int remainingCapacity = exploreExecutorService.getExploreQueueRemainingCapacity();
            if (remainingCapacity < bucketPairs.size()) {
                result.put("success", false);
                result.put("message", "Explore队列剩余容量不足，当前剩余: " + remainingCapacity +
                        "，需要: " + bucketPairs.size());
                return result;
            }

            int addedCount = 0;
            List<String> creationIds = new ArrayList<>();
            for (BucketPair bucketPair : bucketPairs) {
                String creationId = taskCreationService.create(bucketPair, true);
                bucketPair.setCreationId(creationId);
                if (taskBandwidth != null) {
                    try { taskBandwidth.prepareCreation(bucketPair, creationId, bucketPair.getEligibleWorkerNodes()); }
                    catch (RuntimeException error) { taskCreationService.fail(creationId, error.getMessage()); throw error; }
                }
                if (exploreExecutorService.offerExploreTask(bucketPair)) {
                    addedCount++;
                    creationIds.add(creationId);
                } else taskCreationService.fail(creationId, "任务队列已满");
            }
            result.put("creationIds", creationIds);

            result.put("success", true);
            result.put("message", "已提交 " + addedCount + " 个迁移任务");
            result.put("addedCount", addedCount);
        } catch (Exception e) {
            log.error("添加迁移任务失败", e);
            result.put("success", false);
            result.put("message", "添加失败: " + e.getMessage());
        }
        return result;
    }

    private boolean isSameOssBucket(String sourceAlias, String sourceBucket,
                                    String destinationAlias, String destinationBucket) {
        String source = sourceAlias == null || sourceAlias.trim().isEmpty() ? "default" : sourceAlias.trim();
        String destination = destinationAlias == null || destinationAlias.trim().isEmpty()
                ? "default" : destinationAlias.trim();
        return sourceBucket != null && destinationBucket != null
                && sourceBucket.trim().equals(destinationBucket.trim()) && source.equals(destination);
    }

    private boolean isValidDateTimeFormat(String dateStr) {
        String[] patterns = {
            "yyyy-MM-dd'T'HH:mm:ss",
            "yyyy-MM-dd HH:mm:ss",
            "yyyy-MM-dd'T'HH:mm:ss.SSS",
            "yyyy-MM-dd HH:mm:ss.SSS"
        };

        for (String pattern : patterns) {
            try {
                java.text.SimpleDateFormat sdf = new java.text.SimpleDateFormat(pattern);
                sdf.setLenient(false);
                sdf.parse(dateStr);
                return true;
            } catch (Exception e) {
                // 继续尝试下一个格式
            }
        }
        return false;
    }

    private boolean isDateBeforeValid(String dateAfterStr, String dateBeforeStr) {
        String[] patterns = {
            "yyyy-MM-dd'T'HH:mm:ss",
            "yyyy-MM-dd HH:mm:ss",
            "yyyy-MM-dd'T'HH:mm:ss.SSS",
            "yyyy-MM-dd HH:mm:ss.SSS"
        };

        java.util.Date dateAfter = null;
        java.util.Date dateBefore = null;

        for (String pattern : patterns) {
            try {
                java.text.SimpleDateFormat sdf = new java.text.SimpleDateFormat(pattern);
                sdf.setLenient(false);
                if (dateAfter == null) {
                    dateAfter = sdf.parse(dateAfterStr);
                }
            } catch (Exception e) {
                // 继续尝试下一个格式
            }
        }

        for (String pattern : patterns) {
            try {
                java.text.SimpleDateFormat sdf = new java.text.SimpleDateFormat(pattern);
                sdf.setLenient(false);
                if (dateBefore == null) {
                    dateBefore = sdf.parse(dateBeforeStr);
                }
            } catch (Exception e) {
                // 继续尝试下一个格式
            }
        }

        if (dateAfter != null && dateBefore != null) {
            return dateBefore.after(dateAfter);
        }
        return false;
    }

    @GetMapping("/rate-limit")
    public Map<String, Object> getRateLimit() {
        Map<String, Object> result = new HashMap<>();
        List<Map<String, Object>> rateLimits = new ArrayList<>();
        List<String> successfulWorkers = new ArrayList<>();
        Map<String, String> errors = new java.util.LinkedHashMap<>();
        List<String> workers = distributedConfig.getWorkerIps();
        for (String workerIp : workers == null ? java.util.Collections.<String>emptyList() : workers) {
            try {
                String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/rate-limit";
                String response = callRemoteApi(url, "GET", null);
                Map<String, Object> workerResult = response == null ? null : new ObjectMapper().readValue(response, Map.class);
                if (workerResult == null || Boolean.FALSE.equals(workerResult.get("success"))
                        || !(workerResult.get("rateLimit") instanceof Map)) {
                    throw new IllegalStateException("未收到有效的限速状态");
                }
                Map<String, Object> workerRateLimit = (Map<String, Object>) workerResult.get("rateLimit");
                Map<String, Object> entry = new HashMap<>();
                entry.put("workerIp", workerIp);
                for (String key : java.util.Arrays.asList("list", "put", "head", "bandwidth", "autoAdjust")) {
                    entry.put(key, workerRateLimit.get(key));
                }
                rateLimits.add(entry);
                successfulWorkers.add(workerIp);
            } catch (Exception e) {
                errors.put(workerIp, e.getMessage());
                log.warn("获取工作节点 {} 限速信息失败: {}", workerIp, e.getMessage());
            }
        }
        result.put("rateLimits", rateLimits);
        putRateQueryCompleteness(result, successfulWorkers, errors);
        return result;
    }

    private void putRateQueryCompleteness(Map<String, Object> result, List<String> successfulWorkers,
                                          Map<String, String> errors) {
        result.put("success", errors.isEmpty() || !successfulWorkers.isEmpty());
        result.put("complete", errors.isEmpty());
        result.put("partial", !errors.isEmpty() && !successfulWorkers.isEmpty());
        result.put("successfulWorkers", successfulWorkers);
        result.put("failedWorkers", new ArrayList<>(errors.keySet()));
        result.put("errors", errors);
        if (!errors.isEmpty()) result.put("message", "部分节点数据未获取：" + String.join("、", errors.keySet()));
    }

    @PostMapping("/rate-limit/{type}")
    public Map<String, Object> updateRateLimit(@PathVariable String type, @RequestBody Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        try {
            if (!params.containsKey("qps")) {
                result.put("success", false);
                result.put("message", "缺少qps参数");
                return result;
            }

            double qps = Double.valueOf(params.get("qps").toString());
            return applyRateLimit(type, params, qps, distributedConfig.getWorkerIps());
        } catch (Exception e) {
            log.error("更新限速配置失败", e);
            result.put("success", false);
            result.put("message", "更新失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/rate-limit/{type}/{workerIp}")
    public Map<String, Object> updateRateLimitForWorker(
            @PathVariable String type,
            @PathVariable String workerIp,
            @RequestBody Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        try {
            if (!params.containsKey("qps")) {
                result.put("success", false);
                result.put("message", "缺少qps参数");
                return result;
            }

            double qps = Double.valueOf(params.get("qps").toString());
            return applyRateLimit(type, params, qps, java.util.Collections.singletonList(workerIp));
        } catch (Exception e) {
            log.error("更新限速配置失败", e);
            result.put("success", false);
            result.put("message", "更新失败: " + e.getMessage());
        }
        return result;
    }

    private Map<String, Object> applyRateLimit(String type, Map<String, Object> params,
                                              double qps, List<String> workers) {
        Map<String, Object> result = new HashMap<>();
        List<String> successfulWorkers = new ArrayList<>();
        List<String> failedWorkers = new ArrayList<>();
        Map<String, String> errors = new java.util.LinkedHashMap<>();
        for (String workerIp : workers == null ? java.util.Collections.<String>emptyList() : workers) {
            try {
                String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/rate-limit/" + type;
                String response = callRemoteApi(url, "POST", params);
                if (response == null || response.trim().isEmpty()) {
                    throw new IllegalStateException("未收到节点成功响应，请检查节点服务和网络");
                }
                Map<String, Object> workerResult = new ObjectMapper().readValue(response, Map.class);
                if (workerResult == null || workerResult.isEmpty()) {
                    throw new IllegalStateException("节点返回空的更新结果");
                }
                if (Boolean.FALSE.equals(workerResult.get("success"))) {
                    throw new IllegalStateException(workerResult.get("message") == null
                            ? "节点拒绝限速更新" : String.valueOf(workerResult.get("message")));
                }
                successfulWorkers.add(workerIp);
            } catch (Exception e) {
                failedWorkers.add(workerIp);
                errors.put(workerIp, e.getMessage());
                log.warn("更新工作节点 {} 限速未确认成功: {}", workerIp, e.getMessage());
            }
        }
        boolean success = !successfulWorkers.isEmpty() && failedWorkers.isEmpty();
        result.put("success", success);
        result.put("partial", !successfulWorkers.isEmpty() && !failedWorkers.isEmpty());
        result.put("successfulWorkers", successfulWorkers);
        result.put("failedWorkers", failedWorkers);
        result.put("errors", errors);
        if (success) {
            result.put("message", type + "限速已更新为: " + formatRateLimit(qps, type)
                    + "；已成功应用节点：" + String.join("、", successfulWorkers));
        } else if (successfulWorkers.isEmpty() && failedWorkers.isEmpty()) {
            result.put("message", "未配置工作节点，限速未更新");
        } else {
            List<String> failureDetails = new ArrayList<>();
            for (String worker : failedWorkers) failureDetails.add(worker + "（" + errors.get(worker) + "）");
            result.put("message", (successfulWorkers.isEmpty() ? "限速更新失败。未确认任何节点已应用"
                    : "限速仅部分更新。已成功应用节点：" + String.join("、", successfulWorkers))
                    + "；失败节点：" + String.join("、", failureDetails));
        }
        return result;
    }

    @GetMapping("/rate-limit/adjustment")
    public Map<String, Object> getRateLimitAdjustment(@RequestParam String workerIp, @RequestParam String eventId,
                                                     @RequestParam(required = false) String cursor) {
        Map<String, Object> result = new HashMap<>();
        result.put("workerIp", workerIp);
        try {
            if (distributedConfig.getWorkerIps() == null || !distributedConfig.getWorkerIps().contains(workerIp)) {
                throw new IllegalArgumentException("节点未配置");
            }
            String url = "http://" + workerIp + ":" + distributedConfig.getPort()
                    + "/api/worker/rate-limit/adjustment?eventId=" + java.net.URLEncoder.encode(eventId, "UTF-8");
            if (cursor != null && !cursor.isEmpty()) url += "&cursor=" + java.net.URLEncoder.encode(cursor, "UTF-8");
            String response = callRemoteApi(url, "GET", null);
            Map<String, Object> details = response == null ? null : new ObjectMapper().readValue(response, Map.class);
            if (details == null) throw new IllegalStateException("无法获取节点调节详情");
            if (!Boolean.TRUE.equals(details.get("success"))) {
                throw new IllegalStateException(details.get("message") == null ? "调节详情未记录" : String.valueOf(details.get("message")));
            }
            result.putAll(details);
            result.put("workerIp", workerIp);
        } catch (Exception e) {
            result.put("success", false);
            result.put("message", e.getMessage());
        }
        return result;
    }

    @GetMapping("/rate-limit/history")
    public Map<String, Object> getRateLimitHistory(@RequestParam(required = false) Integer limit) {
        Map<String, Object> result = new HashMap<>();
        List<Map<String, Object>> allAdjustments = new ArrayList<>();
        List<String> successfulWorkers = new ArrayList<>();
        Map<String, String> errors = new java.util.LinkedHashMap<>();
        List<String> workers = distributedConfig.getWorkerIps();
        for (String workerIp : workers == null ? java.util.Collections.<String>emptyList() : workers) {
            try {
                String url = "http://" + workerIp + ":" + distributedConfig.getPort()
                        + "/api/worker/rate-limit/history?limit=" + (limit != null ? limit : 20);
                String response = callRemoteApi(url, "GET", null);
                Map<String, Object> workerResult = response == null ? null : new ObjectMapper().readValue(response, Map.class);
                if (workerResult == null || Boolean.FALSE.equals(workerResult.get("success"))
                        || !(workerResult.get("adjustments") instanceof List)) {
                    throw new IllegalStateException("未收到有效的限速调整历史");
                }
                List<Map<String, Object>> workerAdjustments = (List<Map<String, Object>>) workerResult.get("adjustments");
                List<Map<String, Object>> entries = new ArrayList<>();
                for (Map<String, Object> adjustment : workerAdjustments) {
                    Map<String, Object> entry = new HashMap<>(adjustment);
                    entry.put("workerIp", workerIp);
                    entries.add(entry);
                }
                allAdjustments.addAll(entries);
                successfulWorkers.add(workerIp);
            } catch (Exception e) {
                errors.put(workerIp, e.getMessage());
                log.warn("获取工作节点 {} 限速调整历史失败: {}", workerIp, e.getMessage());
            }
        }
        allAdjustments.sort((a, b) -> Long.compare(rateAdjustmentTime(b), rateAdjustmentTime(a)));
        result.put("adjustments", allAdjustments);
        result.put("total", allAdjustments.size());
        putRateQueryCompleteness(result, successfulWorkers, errors);
        return result;
    }

    private long rateAdjustmentTime(Map<String, Object> adjustment) {
        Object timestamp = adjustment.get("timestamp");
        if (timestamp instanceof Number) return ((Number) timestamp).longValue();
        try { return timestamp == null ? 0L : Long.parseLong(timestamp.toString()); }
        catch (NumberFormatException ignored) { return 0L; }
    }

    @GetMapping("/object-metadata")
    public Map<String, Object> getObjectMetadata(
            @RequestParam String sourceBucket,
            @RequestParam String sourceKey,
            @RequestParam String destBucket,
            @RequestParam String destKey) {
        Map<String, Object> result = new HashMap<>();
        try {
            if (!validMetadataBucket(sourceBucket) || !validMetadataBucket(destBucket)) {
                result.put("success", false);
                result.put("message", "源和目标 Bucket 均须使用连接名称/Bucket 格式，例如 storage/my-bucket");
                return result;
            }
            sourceBucket = sourceBucket.trim();
            destBucket = destBucket.trim();
            String sourceAlias = null;
            String sourceBucketName = sourceBucket;
            String destAlias = null;
            String destBucketName = destBucket;

            if (sourceBucket != null && sourceBucket.contains("/")) {
                String[] parts = sourceBucket.split("/", 2);
                if (parts.length == 2) {
                    sourceAlias = parts[0];
                    sourceBucketName = parts[1];
                }
            }

            if (destBucket != null && destBucket.contains("/")) {
                String[] parts = destBucket.split("/", 2);
                if (parts.length == 2) {
                    destAlias = parts[0];
                    destBucketName = parts[1];
                }
            }

            AmazonS3 srcS3Client = s3ClientManager.getClient(sourceAlias);
            if (srcS3Client == null) {
                result.put("success", false);
                result.put("message", "源S3配置不存在: " + sourceAlias);
                return result;
            }

            AmazonS3 dstS3Client = s3ClientManager.getClient(destAlias);
            if (dstS3Client == null) {
                result.put("success", false);
                result.put("message", "目标S3配置不存在: " + destAlias);
                return result;
            }

            Map<String, Object> sourceMetadata = readObjectMetadata(srcS3Client, sourceBucketName, sourceKey);
            Map<String, Object> destMetadata = readObjectMetadata(dstS3Client, destBucketName, destKey);
            boolean sourceAvailable = !"UNAVAILABLE".equals(sourceMetadata.get("queryStatus"));
            boolean destAvailable = !"UNAVAILABLE".equals(destMetadata.get("queryStatus"));
            result.put("success", sourceAvailable || destAvailable);
            result.put("partial", sourceAvailable != destAvailable);
            result.put("complete", sourceAvailable && destAvailable);
            if (!sourceAvailable && !destAvailable) result.put("message", "源对象和目标对象均查询失败，无法比对");
            result.put("sourceBucket", sourceBucketName);
            result.put("sourceKey", sourceKey);
            result.put("destBucket", destBucketName);
            result.put("destKey", destKey);
            result.put("sourceMetadata", sourceMetadata);
            result.put("destMetadata", destMetadata);

            if (Boolean.TRUE.equals(sourceMetadata.get("exists")) && Boolean.TRUE.equals(destMetadata.get("exists"))) {
                boolean sizeMatch = sourceMetadata.get("contentLength").equals(destMetadata.get("contentLength"));
                Map<String, Object> verification = new HashMap<>();
                verification.put("sizeMatch", sizeMatch);
                verification.put("sizeMatchDetail", sourceMetadata.get("contentLength") + " vs " + destMetadata.get("contentLength"));
                result.put("verification", verification);
            } else {
                Map<String, Object> verification = new HashMap<>();
                verification.put("sizeMatch", null);
                verification.put("message", !sourceAvailable || !destAvailable
                        ? "源对象或目标对象查询失败，无法验证" : "源对象或目标对象不存在，无法验证");
                result.put("verification", verification);
            }
        } catch (Exception e) {
            log.error("获取对象元数据失败", e);
            result.put("success", false);
            result.put("message", "获取元数据失败: " + e.getMessage());
        }
        return result;
    }

    private boolean validMetadataBucket(String value) {
        if (value == null) return false;
        String trimmed = value.trim();
        int slash = trimmed.indexOf('/');
        return slash > 0 && slash < trimmed.length() - 1 && trimmed.indexOf('/', slash + 1) < 0
                && !trimmed.substring(0, slash).trim().isEmpty() && !trimmed.substring(slash + 1).trim().isEmpty();
    }

    private Map<String, Object> readObjectMetadata(AmazonS3 client, String bucket, String key) {
        Map<String, Object> result = new HashMap<>();
        try {
            com.amazonaws.services.s3.model.ObjectMetadata metadata = client.getObjectMetadata(bucket, key);
            result.put("exists", true);
            result.put("queryStatus", "FOUND");
            result.put("contentLength", metadata.getContentLength());
            result.put("contentType", metadata.getContentType());
            result.put("lastModified", metadata.getLastModified());
            result.put("etag", metadata.getETag());
            result.put("userMetadata", metadata.getUserMetadata());
            result.put("storageClass", metadata.getStorageClass());
            result.put("contentEncoding", metadata.getContentEncoding());
            result.put("contentDisposition", metadata.getContentDisposition());
            result.put("cacheControl", metadata.getCacheControl());
            result.put("expirationTime", metadata.getExpirationTime());
            result.put("httpExpiresDate", metadata.getHttpExpiresDate());
        } catch (com.amazonaws.services.s3.model.AmazonS3Exception error) {
            result.put("exists", error.getStatusCode() == 404 ? false : null);
            result.put("queryStatus", error.getStatusCode() == 404 ? "NOT_FOUND" : "UNAVAILABLE");
            result.put("statusCode", error.getStatusCode());
            result.put("errorCode", error.getErrorCode());
            result.put("error", error.getStatusCode() == 404 ? "对象不存在" : error.getMessage());
        } catch (RuntimeException error) {
            result.put("exists", null);
            result.put("queryStatus", "UNAVAILABLE");
            result.put("error", error.getMessage());
        }
        return result;
    }

    @PostMapping("/retry-task")
    public Map<String, Object> retryTask(@RequestBody Map<String, Object> request) {
        if (hasOssGroup(request)) return controlOssGroups("retry-task", request);
        Map<String, Object> result = new HashMap<>();
        List<String> details = new ArrayList<>();
        List<String> successful = new ArrayList<>();
        Map<String, String> errors = new java.util.LinkedHashMap<>();
        Map<String, List<Integer>> assignments = new java.util.LinkedHashMap<>();
        Set<Integer> ids = new java.util.LinkedHashSet<>(), matched = new HashSet<>();
        try {
            Object supplied = request.get("taskIds");
            if (supplied != null && !(supplied instanceof Collection)) throw new IllegalArgumentException("taskIds 必须为任务 ID 列表");
            if (supplied != null) for (Object id : (Collection<?>) supplied) {
                if (!(id instanceof Number) || ((Number) id).doubleValue() != ((Number) id).intValue())
                    throw new IllegalArgumentException("任务 ID 必须为整数");
                ids.add(((Number) id).intValue());
            }
            for (String worker : distributedConfig.getWorkerIps()) {
                try {
                    List<Integer> assigned = new ArrayList<>();
                    if (ids.isEmpty()) {
                        Map<String, Object> response = checkedWorkerResponse(worker, "/failed-tasks", "GET", null);
                        if (!(response.get("failedTaskIds") instanceof Collection)) throw new IllegalStateException("节点未返回失败任务列表");
                        for (Object id : (Collection<?>) response.get("failedTaskIds")) {
                            if (!(id instanceof Number)) throw new IllegalStateException("节点返回无效任务 ID");
                            assigned.add(((Number) id).intValue());
                        }
                    } else {
                        String query = ids.stream().map(String::valueOf).collect(java.util.stream.Collectors.joining(","));
                        Map<String, Object> response = checkedWorkerResponse(worker, "/task-status?taskIds=" + query, "GET", null);
                        if (!(response.get("taskStatuses") instanceof Map)) throw new IllegalStateException("节点未返回任务状态");
                        Map statuses = (Map) response.get("taskStatuses");
                        for (Integer id : ids) {
                            Object value = statuses.get(String.valueOf(id));
                            if (value == null) continue;
                            if (!(value instanceof Map)) throw new IllegalStateException("节点返回无效任务状态");
                            matched.add(id);
                            Object status = ((Map) value).get("status");
                            if (!"FAILED".equals(status) && !"PARTIAL_FAILED".equals(status) && !"STOPPED".equals(status))
                                details.add("任务 " + id + "（节点 " + worker + "）不支持重试：" + status);
                            else assigned.add(id);
                        }
                    }
                    if (!assigned.isEmpty()) assignments.put(worker, new ArrayList<>(new java.util.LinkedHashSet<>(assigned)));
                } catch (Exception error) { errors.put(worker, error.getMessage()); }
            }
            for (Integer id : ids) if (!matched.contains(id) && errors.isEmpty()) details.add("任务 " + id + " 不存在");
            if (!errors.isEmpty() || !details.isEmpty()) {
                putFailureCompleteness(result, successful, errors);
                result.put("success", false); result.put("accepted", 0); result.put("failed", details.size()); result.put("ignored", 0);
                result.put("details", details);
                result.put("message", "重试预检未通过；" + String.join("；", details) + (errors.isEmpty() ? "" : "；节点查询失败：" + errors));
                return result;
            }
            int accepted = 0, failed = 0, ignored = 0;
            if (taskCreationService != null) {
                List<Integer> retryIds = new ArrayList<>();
                for (List<Integer> assigned : assignments.values()) retryIds.addAll(assigned);
                taskCreationService.ensureReadyToStart(retryIds);
            }
            for (Map.Entry<String, List<Integer>> entry : assignments.entrySet()) {
                String worker = entry.getKey();
                try {
                    String raw = callRemoteApi("http://" + worker + ":" + distributedConfig.getPort() + "/api/worker/retry-task", "POST",
                            java.util.Collections.singletonMap("taskIds", entry.getValue()));
                    Map response = raw == null ? null : new ObjectMapper().readValue(raw, Map.class);
                    if (response == null || !(response.get("accepted") instanceof Number)
                            || !(response.get("failed") instanceof Number) || !(response.get("ignored") instanceof Number))
                        throw new IllegalStateException("节点未确认重试受理数量");
                    int nodeAccepted = ((Number) response.get("accepted")).intValue();
                    int nodeFailed = ((Number) response.get("failed")).intValue();
                    int nodeIgnored = ((Number) response.get("ignored")).intValue();
                    if (nodeAccepted < 0 || nodeFailed < 0 || nodeIgnored < 0
                            || nodeAccepted + nodeFailed + nodeIgnored != entry.getValue().size())
                        throw new IllegalStateException("节点返回的重试数量不一致");
                    accepted += nodeAccepted; failed += nodeFailed; ignored += nodeIgnored;
                    if (nodeAccepted > 0) successful.add(worker);
                    if (nodeFailed > 0 || !Boolean.TRUE.equals(response.get("success")))
                        errors.put(worker, String.valueOf(response.getOrDefault("message", "部分任务未能受理")));
                    details.add("节点 " + worker + "：受理 " + nodeAccepted + "，失败 " + nodeFailed + "，忽略 " + nodeIgnored);
                } catch (Exception error) { failed += entry.getValue().size(); errors.put(worker, error.getMessage()); }
            }
            putFailureCompleteness(result, successful, errors);
            result.put("success", errors.isEmpty() && failed == 0);
            result.put("partial", accepted > 0 && (!errors.isEmpty() || failed > 0));
            result.put("accepted", accepted); result.put("failed", failed); result.put("ignored", ignored);
            result.put("details", details);
            result.put("message", assignments.isEmpty() ? "没有需要重试的任务" : "重试已受理 " + accepted + "，失败 " + failed + "，忽略 " + ignored
                    + (errors.isEmpty() ? "" : "；节点错误：" + errors));
        } catch (Exception error) {
            result.put("success", false); result.put("message", error.getMessage());
        }
        return result;
    }

    private String callRemoteApi(String url, String method, Object body) {
        try {
            org.springframework.http.HttpHeaders headers = new org.springframework.http.HttpHeaders();
            headers.setContentType(org.springframework.http.MediaType.APPLICATION_JSON);
            String internalToken = distributedConfig.getInternalToken();
            if (internalToken != null && !internalToken.isEmpty()) headers.set("X-Internal-Token", internalToken);
            org.springframework.http.ResponseEntity<String> response = nodeHttp.exchange(
                    java.net.URI.create(url), org.springframework.http.HttpMethod.valueOf(method),
                    new org.springframework.http.HttpEntity<>(body, headers), String.class);
            return response.getStatusCodeValue() == 200 ? response.getBody() : null;
        } catch (Exception e) {
            log.warn("调用远程API失败: {}, error: {}", url, e.getMessage());
            return null;
        }
    }

    @PostMapping("/s3-config")
    public Map<String, Object> addS3Config(@RequestBody Map<String, Object> params) {
        return nodeConfigService.withStorageSynchronizationLock(() -> addS3ConfigLocked(params));
    }

    private Map<String, Object> addS3ConfigLocked(Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        try {
            String alias = (String) params.get("alias");
            String endpoint = (String) params.get("endpoint");
            String accessKey = (String) params.get("accessKey");
            String secretKey = (String) params.get("secretKey");
            Boolean validate = params.containsKey("validate") ? (Boolean) params.get("validate") : true;
            Boolean useV2SignerType = params.containsKey("useV2SignerType") ? (Boolean) params.get("useV2SignerType") : null;
            Boolean pathStyleEnable = params.containsKey("pathStyleEnable") ? (Boolean) params.get("pathStyleEnable") : null;

            if (alias == null || alias.trim().isEmpty()) {
                result.put("success", false);
                result.put("message", "配置别名不能为空");
                return result;
            }

            if (endpoint == null || endpoint.trim().isEmpty()) {
                result.put("success", false);
                result.put("message", "endpoint不能为空");
                return result;
            }

            if (accessKey == null || accessKey.trim().isEmpty()) {
                result.put("success", false);
                result.put("message", "accessKey不能为空");
                return result;
            }

            if (secretKey == null || secretKey.trim().isEmpty()) {
                result.put("success", false);
                result.put("message", "secretKey不能为空");
                return result;
            }

            s3ClientManager.addClient(alias, endpoint, accessKey, secretKey, useV2SignerType, pathStyleEnable, validate);

            List<String> workerIps = distributedConfig.getWorkerIps();
            int successCount = 1;
            int failCount = 0;
            List<String> details = new ArrayList<>();
            details.add(distributedConfig.getLocalIp() + ": 添加成功");

            for (String workerIp : workerIps) {
                if (workerIp.equals(distributedConfig.getLocalIp())) {
                    continue;
                }

                try {
                    String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/s3-config";
                    Map<String, Object> workerParams = new HashMap<>();
                    workerParams.put("alias", alias);
                    workerParams.put("endpoint", endpoint);
                    workerParams.put("accessKey", accessKey);
                    workerParams.put("secretKey", secretKey);
                    workerParams.put("validate", validate);
                    workerParams.put("useV2SignerType", useV2SignerType);
                    workerParams.put("pathStyleEnable", pathStyleEnable);

                    String response = callRemoteApi(url, "POST", workerParams);
                    if (successfulSyncResponse(response)) {
                        nodeRegistryService.confirmAgentStorageSynchronized(workerIp);
                        successCount++;
                        details.add(workerIp + ": 添加成功");
                    } else {
                        failCount++;
                        details.add(workerIp + ": 添加失败");
                    }
                } catch (Exception e) {
                    failCount++;
                    details.add(workerIp + ": 添加异常 - " + e.getMessage());
                    log.error("同步S3配置到worker失败: {}", workerIp, e);
                }
            }

            result.put("success", true);
            result.put("message", String.format("添加结果：成功 %d 个节点，失败 %d 个节点", successCount, failCount));
            result.put("details", details);
            result.put("successCount", successCount);
            result.put("failedCount", failCount);
            result.put("partial", failCount > 0);
            log.info("S3配置添加成功: {} -> {}, 同步到 {} 个节点", alias, endpoint, successCount);
        } catch (Exception e) {
            log.error("添加S3配置失败", e);
            result.put("success", false);
            result.put("message", "添加失败: " + e.getMessage());
        }
        return result;
    }

    @GetMapping("/s3-config")
    public Map<String, Object> getAllS3Configs() {
        Map<String, Object> result = new HashMap<>();
        try {
            List<S3ClientConfig> configs = s3ClientManager.getAllConfigs();
            result.put("success", true);
            result.put("configs", configs);
        } catch (Exception e) {
            log.error("获取S3配置失败", e);
            result.put("success", false);
            result.put("message", "获取失败: " + e.getMessage());
        }
        return result;
    }

    @DeleteMapping("/s3-config/{alias}")
    public Map<String, Object> removeS3Config(@PathVariable String alias) {
        return nodeConfigService.withStorageSynchronizationLock(() -> removeS3ConfigLocked(alias));
    }

    private Map<String, Object> removeS3ConfigLocked(String alias) {
        Map<String, Object> result = new HashMap<>();
        try {
            boolean localSuccess = s3ClientManager.removeClient(alias);
            if (!localSuccess) {
                result.put("success", false);
                result.put("message", "S3配置不存在");
                return result;
            }

            List<String> workerIps = distributedConfig.getWorkerIps();
            int successCount = 1;
            int failCount = 0;
            List<String> details = new ArrayList<>();
            details.add(distributedConfig.getLocalIp() + ": 删除成功");

            for (String workerIp : workerIps) {
                if (workerIp.equals(distributedConfig.getLocalIp())) {
                    continue;
                }

                try {
                    String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/s3-config/" + alias;
                    String response = callRemoteApi(url, "DELETE", null);
                    if (successfulSyncResponse(response)) {
                        nodeRegistryService.confirmAgentStorageSynchronized(workerIp);
                        successCount++;
                        details.add(workerIp + ": 删除成功");
                    } else {
                        failCount++;
                        details.add(workerIp + ": 删除失败");
                    }
                } catch (Exception e) {
                    failCount++;
                    details.add(workerIp + ": 删除异常 - " + e.getMessage());
                    log.error("从worker删除S3配置失败: {}", workerIp, e);
                }
            }

            result.put("success", true);
            result.put("message", String.format("删除结果：成功 %d 个节点，失败 %d 个节点", successCount, failCount));
            result.put("details", details);
            result.put("successCount", successCount);
            result.put("failedCount", failCount);
            result.put("partial", failCount > 0);
            log.info("S3配置删除成功: {}, 同步到 {} 个节点", alias, successCount);
        } catch (Exception e) {
            log.error("删除S3配置失败", e);
            result.put("success", false);
            result.put("message", "删除失败: " + e.getMessage());
        }
        return result;
    }

    @PutMapping("/s3-config/{alias}")
    public Map<String, Object> updateS3Config(@PathVariable String alias, @RequestBody Map<String, Object> params) {
        return nodeConfigService.withStorageSynchronizationLock(() -> updateS3ConfigLocked(alias, params));
    }

    private Map<String, Object> updateS3ConfigLocked(String alias, Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        try {
            String endpoint = (String) params.get("endpoint");
            String accessKey = (String) params.get("accessKey");
            String secretKey = (String) params.get("secretKey");
            Boolean validate = params.containsKey("validate") ? (Boolean) params.get("validate") : true;
            Boolean useV2SignerType = params.containsKey("useV2SignerType") ? (Boolean) params.get("useV2SignerType") : null;
            Boolean pathStyleEnable = params.containsKey("pathStyleEnable") ? (Boolean) params.get("pathStyleEnable") : null;

            if (alias == null || alias.trim().isEmpty()) {
                result.put("success", false);
                result.put("message", "配置别名不能为空");
                return result;
            }

            if (endpoint == null || endpoint.trim().isEmpty()) {
                result.put("success", false);
                result.put("message", "endpoint不能为空");
                return result;
            }

            if (accessKey == null || accessKey.trim().isEmpty()) {
                result.put("success", false);
                result.put("message", "accessKey不能为空");
                return result;
            }

            if (secretKey == null || secretKey.trim().isEmpty()) {
                result.put("success", false);
                result.put("message", "secretKey不能为空");
                return result;
            }

            s3ClientManager.updateClient(alias, endpoint, accessKey, secretKey, useV2SignerType, pathStyleEnable, validate);

            List<String> workerIps = distributedConfig.getWorkerIps();
            int successCount = 1;
            int failCount = 0;
            List<String> details = new ArrayList<>();
            details.add(distributedConfig.getLocalIp() + ": 更新成功");

            for (String workerIp : workerIps) {
                if (workerIp.equals(distributedConfig.getLocalIp())) {
                    continue;
                }

                try {
                    String url = "http://" + workerIp + ":" + distributedConfig.getPort() + "/api/worker/s3-config/" + alias;
                    Map<String, Object> workerParams = new HashMap<>();
                    workerParams.put("endpoint", endpoint);
                    workerParams.put("accessKey", accessKey);
                    workerParams.put("secretKey", secretKey);
                    workerParams.put("validate", validate);
                    workerParams.put("useV2SignerType", useV2SignerType);
                    workerParams.put("pathStyleEnable", pathStyleEnable);

                    String response = callRemoteApi(url, "PUT", workerParams);
                    if (successfulSyncResponse(response)) {
                        nodeRegistryService.confirmAgentStorageSynchronized(workerIp);
                        successCount++;
                        details.add(workerIp + ": 更新成功");
                    } else {
                        failCount++;
                        details.add(workerIp + ": 更新失败");
                    }
                } catch (Exception e) {
                    failCount++;
                    details.add(workerIp + ": 更新异常 - " + e.getMessage());
                    log.error("同步S3配置更新到worker失败: {}", workerIp, e);
                }
            }

            result.put("success", true);
            result.put("message", String.format("更新结果：成功 %d 个节点，失败 %d 个节点", successCount, failCount));
            result.put("details", details);
            result.put("successCount", successCount);
            result.put("failedCount", failCount);
            result.put("partial", failCount > 0);
            log.info("S3配置更新成功: {} -> {}, 同步到 {} 个节点", alias, endpoint, successCount);
        } catch (Exception e) {
            log.error("更新S3配置失败", e);
            result.put("success", false);
            result.put("message", "更新失败: " + e.getMessage());
        }
        return result;
    }

    private boolean successfulSyncResponse(String response) {
        if (response == null) return false;
        try {
            com.fasterxml.jackson.databind.JsonNode value = new ObjectMapper().readTree(response);
            return value != null && value.path("success").isBoolean() && value.path("success").booleanValue();
        } catch (Exception ignored) {
            return false;
        }
    }

    private String formatRateLimit(double value, String type) {
        String[] units;
        if ("bandwidth".equalsIgnoreCase(type)) {
            units = new String[]{"bps", "Kbps", "Mbps", "Gbps", "Tbps"};
        } else {
            units = new String[]{"", "K", "M", "G", "T"};
        }

        if (value == 0) {
            return "0" + units[0];
        }

        int unitIndex = 0;
        double displayValue = value;

        while (displayValue >= 1000 && unitIndex < units.length - 1) {
            displayValue /= 1000;
            unitIndex++;
        }

        if (displayValue == (long) displayValue) {
            return String.format("%d%s", (long) displayValue, units[unitIndex]);
        } else {
            return String.format("%.2f%s", displayValue, units[unitIndex]);
        }
    }

    @GetMapping("/s3-config/{alias}/buckets")
    public Map<String, Object> listBuckets(@PathVariable String alias) {
        Map<String, Object> result = new HashMap<>();
        try {
            List<String> buckets = s3ClientManager.listBuckets(alias);
            result.put("success", true);
            result.put("buckets", buckets);
        } catch (Exception e) {
            log.error("列出buckets失败: {}", alias, e);
            result.put("success", false);
            result.put("message", "列出buckets失败: " + e.getMessage());
        }
        return result;
    }
}
