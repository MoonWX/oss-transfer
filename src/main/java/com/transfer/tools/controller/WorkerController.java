package com.transfer.tools.controller;

import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.executor.ExploreExecutorService;
import com.transfer.tools.executor.ListExecutionService;
import com.transfer.tools.model.MigrationStatus;
import com.transfer.tools.model.MigrationStatusVO;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.RateLimitAdjustment;
import com.transfer.tools.model.S3ClientConfig;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TaskImport;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.model.FailedKeyMessage;
import com.transfer.tools.model.FailureStage;
import com.transfer.tools.service.RateLimiterManager;
import com.transfer.tools.service.S3ClientManager;
import com.transfer.tools.service.SubTaskManagerService;
import com.transfer.tools.service.InventoryManifestService;
import com.transfer.tools.service.NodeConfigService;
import com.transfer.tools.service.TransferValidationException;
import com.transfer.tools.service.LocalFileService;
import com.transfer.tools.service.ExecutionTaskStatistics;
import com.transfer.tools.config.DistributedConfig;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.web.bind.annotation.*;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileReader;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import com.fasterxml.jackson.databind.ObjectMapper;

@Slf4j
@RestController
@RequestMapping("/api/worker")
public class WorkerController {

    @Autowired
    private SubTaskManagerService subTaskManagerService;

    @Autowired
    private RateLimiterManager rateLimiterManager;

    @Autowired
    private ListExecutionService listExecutionService;

    @Autowired
    private ExploreExecutorService exploreExecutorService;

    @Autowired
    private BufferQueueManager bufferQueueManager;

    @Autowired
    private DistributedConfig distributedConfig;

    @Autowired
    private S3ClientManager s3ClientManager;

    @Autowired
    private ObjectMapper objectMapper;

    @Autowired
    private com.transfer.tools.service.TransferSpeedService transferSpeedService;

    @Autowired
private NodeConfigService nodeConfigService;

    @Autowired
    private LocalFileService localFileService;

    @Autowired
    private com.transfer.tools.service.LocalTaskControlService localTaskControlService;

    @Autowired
    private com.transfer.tools.service.FileReportLedgerService fileReportLedgerService;

    @GetMapping("/node-info")
    public Map<String, Object> getNodeInfo() {
        Map<String, Object> result = new HashMap<>();
        result.put("isMaster", distributedConfig.isMaster());
        result.put("masterIp", distributedConfig.getMasterIp());
        result.put("masterPort", distributedConfig.getPort());
        result.put("localIp", distributedConfig.getLocalIp());
        result.put("taskChildren", true);
        return result;
    }

    @GetMapping("/node-config")
    public Map<String, Object> getNodeConfig() {
        Map<String, Object> result = new HashMap<>();
        try {
            result.put("success", true);
            result.put("data", nodeConfigService.get());
        } catch (Exception e) {
            log.error("获取节点配置失败", e);
            result.put("success", false);
            result.put("message", "获取节点配置失败: " + e.getMessage());
        }
        return result;
    }

    @PutMapping("/node-config")
    public Map<String, Object> updateNodeConfig(@RequestBody com.transfer.tools.model.NodeConfig incoming) {
        Map<String, Object> result = new HashMap<>();
        try {
            com.transfer.tools.model.NodeConfig updated = nodeConfigService.update(incoming);
            result.put("success", true);
            result.put("data", updated);
        } catch (Exception e) {
            log.error("更新节点配置失败", e);
            result.put("success", false);
            result.put("message", "更新节点配置失败: " + e.getMessage());
        }
        return result;
    }

    @GetMapping("/worker-status")
    public Map<String, Object> getWorkerStatus() {
        Map<String, Object> result = new HashMap<>();
        try {
            Map<String, Object> queueStatus = new HashMap<>();
            queueStatus.put("listQueueSize", bufferQueueManager.listQueueSize());
            queueStatus.put("listQueueCapacity", bufferQueueManager.listQueueCapacity());
            queueStatus.put("putQueueSize", bufferQueueManager.putQueueSize());
            queueStatus.put("putQueueCapacity", bufferQueueManager.putQueueCapacity());
            queueStatus.put("progressQueueSize", bufferQueueManager.progressQueueSize());
            queueStatus.put("progressQueueCapacity", bufferQueueManager.progressQueueCapacity());
            queueStatus.put("failedKeyQueueSize", bufferQueueManager.failedKeyQueueSize());
            queueStatus.put("failedKeyQueueCapacity", bufferQueueManager.failedKeyQueueCapacity());
            queueStatus.put("fallbackQueueSize", bufferQueueManager.fallbackQueueSize());
            queueStatus.put("fallbackQueueCapacity", bufferQueueManager.fallbackQueueCapacity());

            Map<String, Object> rateLimitStatus = rateLimiterManager.getRateLimitStatus();

            List<RateLimitAdjustment> rateLimitHistory = new ArrayList<>();
            try {
                rateLimitHistory = rateLimiterManager.getAdjustmentHistory(100);
                result.put("rateLimitHistoryComplete", true);
            } catch (Exception e) {
                result.put("rateLimitHistoryComplete", false);
                result.put("rateLimitHistoryError", "限速调整历史读取失败");
                log.warn("节点状态保留当前限速，但调节历史不可读取", e);
            }

            MigrationStatus migrationStatus = subTaskManagerService.getMigrationStatus(null, null);
            Map<String, Object> taskStats = new HashMap<>();
            taskStats.put("pendingTasks", migrationStatus.getPendingTasks());
            taskStats.put("runningTasks", migrationStatus.getRunningTasks());
            taskStats.put("completedTasks", migrationStatus.getCompletedTasks());
            taskStats.put("partialFailedTasks", migrationStatus.getPartialFailedTasks());
            taskStats.put("failedTasks", migrationStatus.getFailedTasks());
            taskStats.put("listDoneTasks", migrationStatus.getListDoneTasks());
            taskStats.put("totalTasks", migrationStatus.getTotalTasks());

            result.put("success", true);
            result.put("queueStatus", queueStatus);
            result.put("rateLimit", rateLimitStatus);
            result.put("rateLimitHistory", rateLimitHistory);
            result.put("taskStats", taskStats);
            // This snapshot comes only from the worker's current execution index.
            // Local master parents and discovery records live in separate stores;
            // do not aggregate localTasks()/master status into execution counts.
            // Legacy standalone executions may have parentTaskId == taskIndex
            // and remain real work, so that equality is not a parent discriminator.
            result.put("executionTaskStats", ExecutionTaskStatistics.fromStoredExecutionTasks(migrationStatus.getTasks()));
        } catch (Exception e) {
            log.error("获取worker状态失败", e);
            result.put("success", false);
            result.put("message", "获取状态失败: " + e.getMessage());
        }
        return result;
    }

    @GetMapping("/task-status")
    public Map<String, Object> getTaskStatuses(@RequestParam String taskIds) {
        Map<String, Object> result = new HashMap<>();
        try {
            Map<String, Object> taskStatuses = new HashMap<>();

            String[] idArray = taskIds.split(",");
            for (String idStr : idArray) {
                try {
                    Integer taskIndex = Integer.parseInt(idStr.trim());
                    SubTaskStatus task = subTaskManagerService.getTaskStatus(taskIndex);

                    if (task != null) {
                        Map<String, Object> taskInfo = new HashMap<>();
                        taskInfo.put("status", task.getStatus().name());
                        taskInfo.put("workerNode", task.getWorkerNode());
                        taskInfo.put("sourceBucket", task.getSourceBucket());
                        taskInfo.put("destBucket", task.getDestBucket());
                        taskStatuses.put(String.valueOf(taskIndex), taskInfo);
                    }
                } catch (NumberFormatException e) {
                    log.warn("无效的任务ID: {}", idStr);
                }
            }

            result.put("success", true);
            result.put("taskStatuses", taskStatuses);
        } catch (Exception e) {
            log.error("获取任务状态失败", e);
            result.put("success", false);
            result.put("message", "获取任务状态失败: " + e.getMessage());
        }
        return result;
    }

    @GetMapping("/pending-tasks")
    public Map<String, Object> getPendingTasks(@RequestParam(required = false) String sourceBucket,
                                                @RequestParam(required = false) String destBucket) {
        Map<String, Object> result = new HashMap<>();
        try {
            List<Integer> pendingTaskIds = subTaskManagerService.getPendingTaskIds(sourceBucket, destBucket);
            result.put("success", true);
            result.put("pendingTaskIds", pendingTaskIds);
        } catch (Exception e) {
            log.error("获取待执行任务失败", e);
            result.put("success", false);
            result.put("message", "获取待执行任务失败: " + e.getMessage());
        }
        return result;
    }

    @GetMapping("/failed-tasks")
    public Map<String, Object> getFailedTasks() {
        Map<String, Object> result = new HashMap<>();
        try {
            List<Integer> failedTaskIds = subTaskManagerService.getFailedTaskIds();
            result.put("success", true);
            result.put("failedTaskIds", failedTaskIds);
        } catch (Exception e) {
            log.error("获取失败任务失败", e);
            result.put("success", false);
            result.put("message", "获取失败任务失败: " + e.getMessage());
        }
        return result;
    }

    @GetMapping("/status")
    public MigrationStatusVO getStatus(@RequestParam(required = false) String sourceBucket,
                                       @RequestParam(required = false) String destBucket,
                                       @RequestParam(required = false) String status,
                                       @RequestParam(required = false) String workerNode) {
        MigrationStatus migrationStatus = subTaskManagerService.getMigrationStatus(sourceBucket, destBucket, status, workerNode);
        if (transferSpeedService != null && migrationStatus.getTasks() != null) {
            for (SubTaskStatus task : migrationStatus.getTasks()) {
                task.setCurrentSpeedBytesPerSecond(transferSpeedService.currentBytesPerSecond(task.getTaskIndex()));
            }
        }
        return MigrationStatusVO.from(migrationStatus);
    }

    @PostMapping("/start")
    public Map<String, Object> startMigration(@RequestBody Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        try {
            String sourceBucket = (String) params.get("sourceBucket");
            String destBucket = (String) params.get("destBucket");
            List<Integer> taskIds = parseTaskIds(params.get("taskIds"));

            if (!taskIds.isEmpty()) {
                List<Integer> pendingTaskIds = new ArrayList<>();
                int ignoredCount = 0;

                for (Integer taskId : taskIds) {
                    SubTaskStatus task = subTaskManagerService.getTaskStatus(taskId);
                    if (task != null && task.getStatus() == SubTaskStatus.TaskStatus.PENDING) {
                        pendingTaskIds.add(taskId);
                    } else {
                        ignoredCount++;
                        if (task != null) {
                            log.info("忽略非PENDING状态的任务: taskId={}, status={}", taskId, task.getStatus());
                        } else {
                            log.info("忽略不存在的任务: taskId={}", taskId);
                        }
                    }
                }

                if (pendingTaskIds.isEmpty()) {
                    result.put("success", false);
                    result.put("message", "所有指定任务都不是PENDING状态，已忽略 " + ignoredCount + " 个任务");
                    return result;
                }

                taskIds = pendingTaskIds;
                String message = "已启动 " + pendingTaskIds.size() + " 个PENDING状态的任务";
                if (ignoredCount > 0) {
                    message += "，忽略 " + ignoredCount + " 个非PENDING状态的任务";
                }
                result.put("message", message);
            }

            if (!subTaskManagerService.hasTasks(sourceBucket, destBucket)) {
                log.info("没有可执行的任务");
                result.put("success", false);
                result.put("message", "没有可执行的任务");
                return result;
            }

            listExecutionService.startExecution(sourceBucket, destBucket, taskIds);

            if (!result.containsKey("message")) {
                String message = "迁移任务已启动";
                if (taskIds != null && !taskIds.isEmpty()) {
                    message = "已启动指定的 " + taskIds.size() + " 个迁移任务";
                }
                result.put("message", message);
            }

            result.put("success", true);
        } catch (Exception e) {
            log.error("启动迁移任务失败", e);
            result.put("success", false);
            result.put("message", "启动失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/stop")
    public Map<String, Object> stopTasks(@RequestBody Map<String, Object> params) {
        return controlTasks(params, true);
    }

    @PostMapping("/resume")
    public Map<String, Object> resumeTasks(@RequestBody Map<String, Object> params) {
        return controlTasks(params, false);
    }

    private Map<String, Object> controlTasks(Map<String, Object> params, boolean stop) {
        Map<String, Object> result = new HashMap<>();
        List<Integer> ids = parseTaskIds(params.get("taskIds"));
        List<Integer> matched = new ArrayList<>();
        List<String> errors = new ArrayList<>();
        int accepted = 0, failed = 0;
        for (Integer id : ids) {
            try {
                // Serialize the fence with assigned-task import. A timed-out dispatch can
                // arrive after the master has stopped its durable, not-yet-visible ID.
                synchronized (subTaskManagerService) {
                    if (subTaskManagerService.getTaskStatus(id) == null) {
                        if (stop) {
                            boolean alreadyStopped = localTaskControlService.isStopped(id);
                            matched.add(id);
                            localTaskControlService.stop(id);
                            if (!alreadyStopped) accepted++;
                        } else if (localTaskControlService.isStopped(id)) {
                            matched.add(id);
                            localTaskControlService.resume(id);
                            accepted++;
                        }
                    } else {
                        matched.add(id);
                        if (stop ? subTaskManagerService.stopTask(id) : subTaskManagerService.resumeTask(id)) accepted++;
                    }
                }
            } catch (Exception e) { failed++; errors.add("任务 " + id + ": " + e.getMessage()); }
        }
        result.put("success", !ids.isEmpty() && failed == 0);
        result.put("accepted", accepted); result.put("failed", failed);
        result.put("ignored", matched.size() - accepted - failed);
        result.put("matchedTaskIds", matched); result.put("details", errors);
        result.put("message", ids.isEmpty() ? "请指定任务 ID" : errors.isEmpty() ? "操作已执行" : String.join("；", errors));
        return result;
    }

    @PostMapping("/clear")
    public Map<String, Object> clearTasks(@RequestBody Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        try {
            String sourceBucket = (String) params.get("sourceBucket");
            String destBucket = (String) params.get("destBucket");
            Boolean permanent = (Boolean) params.get("permanent");

            List<Integer> taskIds = parseTaskIds(params.get("taskIds"));

            boolean isPermanent = permanent != null && permanent;
            int clearedCount = subTaskManagerService.clearTasks(sourceBucket, destBucket, taskIds, isPermanent);

            String action = isPermanent ? "永久删除" : "标记删除";
            if (!taskIds.isEmpty()) {
                result.put("success", true);
                result.put("message", "已" + action + " " + clearedCount + " 个指定任务");
            } else if (sourceBucket != null || destBucket != null) {
                result.put("success", true);
                result.put("message", "已" + action + " " + clearedCount + " 个匹配的任务");
            } else {
                result.put("success", true);
                result.put("message", "已" + action + " " + clearedCount + " 个已完成任务");
            }
        } catch (Exception e) {
            log.error("清空任务失败", e);
            result.put("success", false);
            result.put("message", "清空失败: " + e.getMessage());
        }
        return result;
    }

    @GetMapping("/deleted-tasks")
    public Map<String, Object> getDeletedTasks() {
        Map<String, Object> result = new HashMap<>();
        try {
            List<Integer> deletedTaskIds = subTaskManagerService.getDeletedTaskIdList();
            List<SubTaskStatus> deletedTasks = new ArrayList<>();

            for (Integer taskId : deletedTaskIds) {
                SubTaskStatus task = subTaskManagerService.getDeletedTaskStatus(taskId);
                if (task != null) {
                    deletedTasks.add(task);
                }
            }

            result.put("success", true);
            result.put("tasks", deletedTasks);
            result.put("totalTasks", deletedTasks.size());
        } catch (Exception e) {
            log.error("获取标删任务列表失败", e);
            result.put("success", false);
            result.put("message", "获取失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/permanent-delete")
    public Map<String, Object> permanentDeleteTasks(@RequestBody Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        try {
            List<Integer> taskIds = parseTaskIds(params.get("taskIds"));

            List<Integer> parentTaskIds = parseTaskIds(params.get("parentTaskIds"));
            if (taskIds.isEmpty() && parentTaskIds.isEmpty()) {
                result.put("success", false);
                result.put("message", "请指定要永久删除的任务ID");
                return result;
            }

            int deletedCount = subTaskManagerService.permanentDeleteTasks(taskIds, parentTaskIds);

            result.put("success", true);
            result.put("message", "已永久删除 " + deletedCount + " 个标删任务");
        } catch (Exception e) {
            log.error("永久删除任务失败", e);
            result.put("success", false);
            result.put("message", "删除失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/retry-task")
    public Map<String, Object> retryTask(@RequestBody Map<String, Object> request) {
        Map<String, Object> result = new HashMap<>();
        List<String> details = new ArrayList<>();
        List<Integer> acceptedIds = new ArrayList<>();
        int failed = 0;
        List<Integer> ids = new ArrayList<>(new java.util.LinkedHashSet<>(parseTaskIds(request.get("taskIds"))));
        for (Integer id : ids) {
            try {
                subTaskManagerService.retryTask(id);
                acceptedIds.add(id);
            } catch (Exception error) {
                failed++;
                details.add("任务 " + id + ": " + error.getMessage());
                if (error instanceof InterruptedException) Thread.currentThread().interrupt();
            }
        }
        result.put("success", !ids.isEmpty() && failed == 0);
        result.put("partial", !acceptedIds.isEmpty() && failed > 0);
        result.put("accepted", acceptedIds.size());
        result.put("failed", failed);
        result.put("ignored", 0);
        result.put("acceptedTaskIds", acceptedIds);
        result.put("details", details);
        result.put("message", ids.isEmpty() ? "请指定任务 ID" : "重试已受理 " + acceptedIds.size() + "，失败 " + failed
                + (details.isEmpty() ? "" : "；" + String.join("；", details)));
        return result;
    }

    @PostMapping("/tasks")
    public Map<String, Object> receiveTasks(@RequestBody List<Map<String, Object>> tasks) {
        com.transfer.tools.util.TaskBandwidthRequestGuard.requireInternal(distributedConfig,
                tasks != null && tasks.stream().anyMatch(task -> task != null
                        && (task.get("taskBandwidthScopeId") != null || task.get("taskBandwidthLimitMbps") != null)));
        Map<String, Object> result = new HashMap<>();
        try {
            if (tasks == null || tasks.isEmpty()) {
                result.put("success", false);
                result.put("message", "任务列表为空");
                return result;
            }

            List<TaskImport> taskImports = new ArrayList<>();
            for (Map<String, Object> task : tasks) {
                TaskImport imported = objectMapper.convertValue(task, TaskImport.class);
                taskImports.add(imported);
                if (imported.getDirection() != null && imported.getDirection() != TransferDirection.OSS_TO_OSS) {
                    String localIp = distributedConfig.getLocalIp();
                    if (!localIp.equals(imported.getExecutionNodeIp())
                            || imported.getEligibleWorkerNodes() == null
                            || !imported.getEligibleWorkerNodes().contains(localIp)) {
                        throw new TransferValidationException("SHARD_ASSIGNMENT_MISMATCH",
                                "LOCAL shard is not assigned to this worker");
                    }
                    if (imported.getDirection() == TransferDirection.OSS_TO_LOCAL) {
                        localFileService.validateDestination(imported.getLocalPath(), true);
                    } else {
                        localFileService.validateSource(imported.getLocalPath());
                    }
                }
            }

            subTaskManagerService.importTasksWithTaskId(taskImports);
            List<Integer> automaticIds = new ArrayList<>();
            for (TaskImport imported : taskImports) {
                if (imported.getDirection() != null && imported.getDirection() != TransferDirection.OSS_TO_OSS) {
                    SubTaskStatus status = subTaskManagerService.getTaskStatus(imported.getTaskIndex());
                    if (status != null && status.getStatus() == SubTaskStatus.TaskStatus.PENDING) {
                        bufferQueueManager.offerListTask(status);
                    }
                } else {
                    // Read the retained record: a replay cannot change a stopped, finished or
                    // historical manual-start task into a newly accepted automatic task.
                    SubTaskStatus status = subTaskManagerService.getTaskStatus(imported.getTaskIndex());
                    if (status != null && status.getStatus() == SubTaskStatus.TaskStatus.PENDING
                            && Boolean.TRUE.equals(status.getAutoStart())) automaticIds.add(status.getTaskIndex());
                }
            }
            if (!automaticIds.isEmpty()) listExecutionService.startExecution(null, null, automaticIds);
            result.put("success", true);
            result.put("message", "成功接收 " + tasks.size() + " 个任务");
            log.info("从主节点接收到 {} 个任务", tasks.size());
        } catch (Exception e) {
            if (tasks != null) {
                for (Map<String, Object> task : tasks) {
                    try {
                        recordReceiveFailure(objectMapper.convertValue(task, TaskImport.class), e);
                    } catch (Exception recordError) {
                        log.warn("记录Worker任务接收失败明细失败", recordError);
                    }
                }
            }
            log.error("接收任务失败", e);
            result.put("success", false);
            result.put("message", "接收失败: " + e.getMessage());
            result.put("code", e instanceof TransferValidationException
                    ? ((TransferValidationException) e).getCode() : "WORKER_TASK_ACCEPT_FAILED");
        }
        return result;
    }

    private void recordReceiveFailure(TaskImport task, Exception error) {
        if (task == null || task.getDirection() == null || task.getDirection() == TransferDirection.OSS_TO_OSS) return;
        FailedKeyMessage failure = new FailedKeyMessage();
        failure.setTaskIndex(task.getTaskIndex());
        failure.setParentTaskId(task.getParentTaskId());
        failure.setShardId(task.getShardId());
        failure.setAttempt(task.getAttempt());
        failure.setDirection(task.getDirection());
        failure.setSourceBucket(task.getSourceBucket());
        failure.setDestBucket(task.getDestBucket());
        failure.setSourceEndpoint(task.getSourceEndpoint());
        failure.setDestinationEndpoint(task.getDestinationEndpoint());
        failure.setStorageId(task.getStorageId());
        failure.setEligibleWorkerNodes(task.getEligibleWorkerNodes());
        failure.setLocalPath(task.getLocalPath());
        failure.setExecutionNodeIp(task.getExecutionNodeIp());
        failure.setWorkerNode(distributedConfig.getLocalIp());
        failure.setKey(task.getObjectKey() != null ? task.getObjectKey()
                : (task.getStartKey() != null ? task.getStartKey() : task.getLocalPath()));
        failure.setObjectSize(0L);
        failure.setStage(error instanceof TransferValidationException ? FailureStage.VALIDATE : FailureStage.DISPATCH);
        failure.setErrorCode(error instanceof TransferValidationException
                ? ((TransferValidationException) error).getCode() : "WORKER_TASK_ACCEPT_FAILED");
        failure.setErrorMessage(error.getMessage());
        failure.setRetryCount(task.getAttempt() == null ? 0 : task.getAttempt());
        if (!bufferQueueManager.offerFailedKey(failure)) {
            log.error("失败明细队列已满: parentTaskId={}, shardId={}", task.getParentTaskId(), task.getShardId());
        }
    }

    @PostMapping("/failed")
    public Map<String, Object> getFailedKeys(@RequestBody(required = false) Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        try {
            String sourceBucket = params != null ? (String) params.get("sourceBucket") : null;
            String destBucket = params != null ? (String) params.get("destBucket") : null;
            Integer taskIndex = params != null && params.get("taskIndex") != null ?
                ((Number) params.get("taskIndex")).intValue() : null;
            Integer limit = params != null && params.get("limit") != null ?
                ((Number) params.get("limit")).intValue() : 100;

            SubTaskManagerService.ExceptionQuery<com.transfer.tools.model.FailedKey> queried =
                    subTaskManagerService.queryFailedKeys(sourceBucket, destBucket, taskIndex, exceptionTaskIds(params), limit);
            result.put("success", true);
            result.put("count", queried.getCount());
            result.put("failedKeys", queried.getItems());
        } catch (Exception e) {
            log.error("查询失败对象失败", e);
            result.put("success", false);
            result.put("message", "查询失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/failed/clear")
    public Map<String, Object> clearFailedKeys() {
        Map<String, Object> result = new HashMap<>();
        try {
            subTaskManagerService.clearFailedKeys();
            result.put("success", true);
            result.put("message", "失败对象记录已清空");
        } catch (Exception e) {
            log.error("清空失败对象记录失败", e);
            result.put("success", false);
            result.put("message", "清空失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/fallback")
    public Map<String, Object> getFallbackObjects(@RequestBody(required = false) Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();
        try {
            String sourceBucket = params != null ? (String) params.get("sourceBucket") : null;
            String destBucket = params != null ? (String) params.get("destBucket") : null;
            Integer taskIndex = params != null && params.get("taskIndex") != null ?
                ((Number) params.get("taskIndex")).intValue() : null;
            Integer limit = params != null && params.get("limit") != null ?
                ((Number) params.get("limit")).intValue() : 100;

            SubTaskManagerService.ExceptionQuery<com.transfer.tools.model.FallbackObject> queried =
                    subTaskManagerService.queryFallbackObjects(sourceBucket, destBucket, taskIndex, exceptionTaskIds(params), limit);
            result.put("success", true);
            result.put("count", queried.getCount());
            result.put("fallbackObjects", queried.getItems());
        } catch (Exception e) {
            log.error("查询降级迁移对象失败", e);
            result.put("success", false);
            result.put("message", "查询失败: " + e.getMessage());
        }
        return result;
    }

    private Set<Integer> exceptionTaskIds(Map<String, Object> params) {
        if (params == null || params.get("taskIds") == null) return null;
        Object value = params.get("taskIds");
        if (!(value instanceof java.util.Collection)) throw new IllegalArgumentException("taskIds 必须为任务 ID 数组");
        Set<Integer> ids = new HashSet<>();
        for (Object item : (java.util.Collection<?>) value) {
            if (!(item instanceof Number)) throw new IllegalArgumentException("taskIds 必须包含整数任务 ID");
            Number number = (Number) item;
            int id = number.intValue();
            if (number.doubleValue() != id) throw new IllegalArgumentException("taskIds 包含无效任务 ID");
            ids.add(id);
        }
        return ids;
    }

    @PostMapping("/fallback/clear")
    public Map<String, Object> clearFallbackObjects() {
        Map<String, Object> result = new HashMap<>();
        try {
            subTaskManagerService.clearFallbackObjects();
            result.put("success", true);
            result.put("message", "降级迁移对象记录已清空");
        } catch (Exception e) {
            log.error("清空降级迁移对象记录失败", e);
            result.put("success", false);
            result.put("message", "清空失败: " + e.getMessage());
        }
        return result;
    }

    @GetMapping("/rate-limit")
    public Map<String, Object> getRateLimit() {
        Map<String, Object> result = new HashMap<>();
        try {
            result.put("success", true);
            result.put("rateLimit", rateLimiterManager.getRateLimitStatus());
        } catch (Exception e) {
            log.error("查询限速配置失败", e);
            result.put("success", false);
            result.put("message", "查询失败: " + e.getMessage());
        }
        return result;
    }

    @GetMapping("/rate-limit/adjustment")
    public Map<String, Object> getRateLimitAdjustment(@RequestParam String eventId,
                                                     @RequestParam(required = false) String cursor) {
        Map<String, Object> result = new HashMap<>();
        try {
            result.putAll(rateLimiterManager.getAdjustmentDetail(eventId, cursor));
            result.put("success", true);
        } catch (Exception e) {
            result.put("success", false);
            result.put("message", e.getMessage());
        }
        return result;
    }

    @GetMapping("/rate-limit/history")
    public Map<String, Object> getRateLimitHistory(@RequestParam(required = false) Integer limit) {
        Map<String, Object> result = new HashMap<>();
        try {
            result.put("success", true);
            result.put("adjustments", rateLimiterManager.getAdjustmentHistory(limit));
        } catch (Exception e) {
            log.error("查询限速调整历史失败", e);
            result.put("success", false);
            result.put("message", "查询失败: " + e.getMessage());
        }
        return result;
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
            boolean success = false;

            switch (type.toLowerCase()) {
                case "list":
                    success = rateLimiterManager.updateListRateLimit(qps);
                    break;
                case "put":
                    success = rateLimiterManager.updatePutRateLimit(qps);
                    break;
                case "head":
                    success = rateLimiterManager.updateHeadRateLimit(qps);
                    break;
                case "bandwidth":
                    success = rateLimiterManager.updateBandwidthRateLimit(qps);
                    break;
                default:
                    result.put("success", false);
                    result.put("message", "不支持的类型: " + type);
                    return result;
            }

            if (success) {
                result.put("success", true);
                result.put("message", type + "限速已更新为: " + formatRateLimit(qps, type));
            } else {
                result.put("success", false);
                result.put("message", "更新失败");
            }
        } catch (Exception e) {
            log.error("更新限速配置失败", e);
            result.put("success", false);
            result.put("message", "更新失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/import")
    public Map<String, Object> importTasks(@RequestBody List<TaskImport> taskImports) {
        com.transfer.tools.util.TaskBandwidthRequestGuard.requireInternal(distributedConfig,
                taskImports != null && taskImports.stream().anyMatch(task -> task != null
                        && (task.getTaskBandwidthScopeId() != null || task.getTaskBandwidthLimitMbps() != null)));
        Map<String, Object> result = new HashMap<>();
        try {
            if (taskImports == null || taskImports.isEmpty()) {
                result.put("success", false);
                result.put("message", "导入任务列表为空");
                return result;
            }

            String localIp = distributedConfig.getLocalIp();
            for (TaskImport task : taskImports) {
                task.setWorkerNode(localIp);
                task.setAutoStart(true);
            }

            subTaskManagerService.importTasks(taskImports);
            result.put("success", true);
            result.put("message", "成功导入 " + taskImports.size() + " 个任务");
            result.put("importedCount", taskImports.size());
            List<Integer> taskIds = new ArrayList<>();
            for (TaskImport task : taskImports) taskIds.add(task.getTaskIndex());
            listExecutionService.startExecution(null, null, taskIds);
            result.put("taskIds", taskIds);
        } catch (Exception e) {
            log.error("导入任务失败", e);
            result.put("success", false);
            result.put("message", "导入失败: " + e.getMessage());
        }
        return result;
    }

    @PostMapping("/s3-config")
    public Map<String, Object> addS3Config(@RequestBody Map<String, Object> params) {
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
            result.put("success", true);
            result.put("message", "S3配置添加成功");
            log.info("S3配置添加成功: {} -> {} (校验: {}, v2签名: {}, pathStyle: {})",
                    alias, endpoint, validate, useV2SignerType, pathStyleEnable);
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
        Map<String, Object> result = new HashMap<>();
        try {
            boolean success = s3ClientManager.removeClient(alias);
            if (success) {
                result.put("success", true);
                result.put("message", "S3配置删除成功");
                log.info("S3配置删除成功: {}", alias);
            } else {
                result.put("success", false);
                result.put("message", "S3配置不存在");
            }
        } catch (Exception e) {
            log.error("删除S3配置失败", e);
            result.put("success", false);
            result.put("message", "删除失败: " + e.getMessage());
        }
        return result;
    }

    @PutMapping("/s3-config/{alias}")
    public Map<String, Object> updateS3Config(@PathVariable String alias, @RequestBody Map<String, Object> params) {
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
            result.put("success", true);
            result.put("message", "S3配置更新成功");
            log.info("S3配置更新成功: {} -> {} (校验: {}, v2签名: {}, pathStyle: {})",
                    alias, endpoint, validate, useV2SignerType, pathStyleEnable);
        } catch (Exception e) {
            log.error("更新S3配置失败", e);
            result.put("success", false);
            result.put("message", "更新失败: " + e.getMessage());
        }
        return result;
    }

    /**
     * 解析任务ID列表，支持格式：
     * - 逗号分隔: "1,2,3,4"
     * - 范围: "9200-9300"
     * - 混合: "1,2,9200-9300,50"
     *
     * @param taskIdsObj 任务ID参数对象
     * @return 解析后的任务ID列表
     */
    private List<Integer> parseTaskIds(Object taskIdsObj) {
        Set<Integer> taskIdSet = new HashSet<>();

        if (taskIdsObj == null) {
            return new ArrayList<>(taskIdSet);
        }

        if (taskIdsObj instanceof List) {
            for (Object id : (List<?>) taskIdsObj) {
                if (id instanceof Integer) {
                    taskIdSet.add((Integer) id);
                } else if (id instanceof Number) {
                    taskIdSet.add(((Number) id).intValue());
                } else if (id instanceof String) {
                    parseTaskIdString((String) id, taskIdSet);
                }
            }
        } else if (taskIdsObj instanceof String) {
            parseTaskIdString((String) taskIdsObj, taskIdSet);
        }

        return new ArrayList<>(taskIdSet);
    }

    /**
     * 解析任务ID字符串，支持逗号分隔和范围格式
     */
    private void parseTaskIdString(String taskIdStr, Set<Integer> taskIdSet) {
        if (taskIdStr == null || taskIdStr.trim().isEmpty()) {
            return;
        }

        String[] parts = taskIdStr.split(",");
        for (String part : parts) {
            part = part.trim();
            if (part.contains("-")) {
                String[] range = part.split("-");
                if (range.length == 2) {
                    try {
                        int start = Integer.parseInt(range[0].trim());
                        int end = Integer.parseInt(range[1].trim());
                        for (int i = start; i <= end; i++) {
                            taskIdSet.add(i);
                        }
                    } catch (NumberFormatException e) {
                        log.warn("无效的任务ID范围: {}", part);
                    }
                }
            } else {
                try {
                    taskIdSet.add(Integer.parseInt(part));
                } catch (NumberFormatException e) {
                    log.warn("无效的任务ID: {}", part);
                }
            }
        }
    }

    @Autowired
    private InventoryManifestService inventoryManifestService;

    @PostMapping("/import-inventory")
    public Map<String, Object> importInventory(@RequestBody Map<String, Object> params) {
        Map<String, Object> result = new HashMap<>();

        String srcAliasBucket = (String) params.get("srcAliasBucket");
        String destAliasBucket = (String) params.get("destAliasBucket");
        String inventoryDir = (String) params.get("inventoryDir");
        Boolean overwrite = params.containsKey("overwrite") ? (Boolean) params.get("overwrite") : false;

        if (srcAliasBucket == null || destAliasBucket == null || inventoryDir == null) {
            result.put("success", false);
            result.put("message", "缺少必要参数: srcAliasBucket, destAliasBucket, inventoryDir");
            return result;
        }

        String[] srcParts = srcAliasBucket.split("/", 2);
        if (srcParts.length != 2) {
            result.put("success", false);
            result.put("message", "srcAliasBucket格式错误，应为: alias/bucket");
            return result;
        }
        String srcAlias = srcParts[0];
        String srcBucket = srcParts[1];

        String[] destParts = destAliasBucket.split("/", 2);
        if (destParts.length != 2) {
            result.put("success", false);
            result.put("message", "destAliasBucket格式错误，应为: alias/bucket");
            return result;
        }
        String destAlias = destParts[0];
        String destBucket = destParts[1];

        File dir = new File(inventoryDir);
        if (!dir.exists() || !dir.isDirectory()) {
            result.put("success", false);
            result.put("message", "增量清单目录不存在或不是目录: " + inventoryDir);
            return result;
        }

        File[] files = dir.listFiles((d, name) -> name.endsWith(".incr"));
        if (files == null || files.length == 0) {
            result.put("success", false);
            result.put("message", "增量清单目录中没有.incr文件");
            return result;
        }

        String workerNode = distributedConfig.getLocalIp();

        List<Integer> taskIndices = new ArrayList<>();
        List<String> failedFiles = new ArrayList<>();
        for (File file : files) {
            Integer taskIndex = subTaskManagerService.getNextTaskId();
            SubTaskStatus task = new SubTaskStatus();
            task.setTaskIndex(taskIndex);
            task.setSourceBucket(srcBucket); task.setDestBucket(destBucket);
            task.setSourceS3Alias(srcAlias); task.setDestS3Alias(destAlias);
            task.setWorkerNode(workerNode); task.setOverwrite(overwrite);
            task.setDirection(TransferDirection.OSS_TO_OSS);
            task.setStatus(SubTaskStatus.TaskStatus.PENDING);
            task.setListObjectCount(0L); task.setListObjectSize(0L);
            task.setPutObjectCount(0L); task.setPutObjectSize(0L);
            task.setFailedObjectCount(0L); task.setFailedObjectSize(0L);
            task.setSkippedObjectCount(0L); task.setSkippedObjectSize(0L);
            task.setCreateTime(System.currentTimeMillis());
            task.setStartKey(file.getName()); task.setEndKey("");
            task.setAttempt(1); // Accepted imports resume after a crash before queue consumption.
            try {
                task.setInventoryManifestId(inventoryManifestService.create(file.toPath()));
                fileReportLedgerService.registerTask(task);
                subTaskManagerService.importInventoryTask(task);
                taskIndices.add(taskIndex);
                bufferQueueManager.offerListTask(task);
            } catch (Exception e) {
                failedFiles.add(file.getName());
                log.error("导入增量清单失败: {}", file.getName(), e);
                if (taskIndices.contains(taskIndex)) subTaskManagerService.stopTask(taskIndex);
            }
        }

        result.put("success", true);
        result.put("success", failedFiles.isEmpty());
        result.put("partial", !failedFiles.isEmpty() && !taskIndices.isEmpty());
        result.put("failedFiles", failedFiles);
        result.put("taskIds", taskIndices);
        result.put("message", failedFiles.isEmpty() ? "增量清单导入已启动" : "部分清单未能导入，请检查失败文件");
        result.put("fileCount", files.length);
        result.put("taskCount", taskIndices.size());
        result.put("inventoryDir", inventoryDir);

        return result;
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
}
