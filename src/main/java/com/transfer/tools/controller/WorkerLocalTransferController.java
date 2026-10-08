package com.transfer.tools.controller;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.service.*;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.*;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.servlet.mvc.method.annotation.StreamingResponseBody;
import org.springframework.web.server.ResponseStatusException;

import java.util.*;

@RestController
@RequestMapping("/api/worker")
public class WorkerLocalTransferController {
    @Autowired private LocalTransferService localTransferService;
    @Autowired private LocalTransferQueryService queryService;
    @Autowired private LocalReportService reportService;
    @Autowired private TaskReportService taskReportService;
    @Autowired private DistributedConfig distributedConfig;
    @Autowired private SubTaskManagerService subTaskManagerService;

    @GetMapping("/local-capabilities")
    public ApiResponse<Map<String, Object>> capabilities() {
        return ApiResponse.success(localTransferService.capabilities());
    }

    @PostMapping("/local-transfers/validate")
    public ApiResponse<Map<String, Object>> validate(@RequestBody LocalTransferRequest request) {
        return ApiResponse.success(localTransferService.validateOnWorker(request));
    }

    @PostMapping("/local-transfers")
    public ApiResponse<Map<String, Object>> create(@RequestBody List<LocalTransferRequest> requests) {
        if (requests != null) requests.forEach(this::guardTaskBandwidth);
        List<Integer> ids = localTransferService.accept(requests);
        Map<String, Object> data = new LinkedHashMap<>();
        data.put("acceptedCount", ids.size());
        data.put("rejectedCount", 0);
        data.put("taskIds", ids);
        return ApiResponse.success(data);
    }

    @PostMapping("/local-transfers/discover")
    public ApiResponse<Map<String, Object>> discover(@RequestBody LocalTransferRequest request) {
        guardTaskBandwidth(request);
        return ApiResponse.success(localTransferService.startDiscovery(request));
    }

    @PostMapping("/local-transfers/batches")
    public ApiResponse<Map<String, Object>> acceptBatch(@RequestBody LocalFileBatch batch) {
        if (batch != null) guardTaskBandwidth(batch.getRequest());
        Integer taskId = localTransferService.acceptBatch(batch);
        Map<String, Object> data = new LinkedHashMap<>();
        data.put("taskId", taskId);
        data.put("parentTaskId", batch.getParentTaskId());
        data.put("shardId", batch.getShardId());
        return ApiResponse.success(data);
    }

    @GetMapping("/local-transfers/status")
    public ApiResponse<Map<String, Object>> status(
            @RequestParam(required = false) Integer taskId,
            @RequestParam(required = false) TransferDirection direction,
            @RequestParam(required = false) String executionNodeIp,
            @RequestParam(required = false) String status,
            @RequestParam(required = false) String endpoint,
            @RequestParam(required = false) String storageId) {
        return ApiResponse.success(queryService.status(taskId, direction, executionNodeIp, status, endpoint, storageId));
    }

    @GetMapping("/local-transfers/failures")
    public ApiResponse<Map<String, Object>> failures(
            @RequestParam(required = false) Integer taskId,
            @RequestParam(required = false) TransferDirection direction,
            @RequestParam(required = false) String executionNodeIp,
            @RequestParam(required = false) String endpoint,
            @RequestParam(required = false) String storageId,
            @RequestParam(defaultValue = "0") int offset,
            @RequestParam(defaultValue = "100") int limit) {
        return ApiResponse.success(queryService.failures(taskId, direction, executionNodeIp, endpoint, storageId, offset, limit));
    }

    @PostMapping("/local-transfers/stop")
    public ApiResponse<Map<String, Object>> stop(@RequestBody(required = false) Map<String, Object> body) {
        return ApiResponse.success(localTransferService.stop(taskIds(body)));
    }

    @PostMapping("/local-transfers/resume")
    public ApiResponse<Map<String, Object>> resume(@RequestBody(required = false) Map<String, Object> body) {
        return ApiResponse.success(localTransferService.resume(taskIds(body), body != null && Boolean.TRUE.equals(body.get("deferDiscovery"))));
    }

    @PostMapping("/local-transfers/retry-failures")
    public ApiResponse<Map<String, Object>> retry(@RequestBody(required = false) Map<String, Object> body) {
        return ApiResponse.success(localTransferService.retry(taskIds(body), itemKeys(body)));
    }

    @GetMapping(value = "/local-transfers/report", produces = "application/zip")
    public ResponseEntity<StreamingResponseBody> report() {
        if (!distributedConfig.isMaster()) throw new ResponseStatusException(HttpStatus.FORBIDDEN,
                "请从主节点导出任务报告");
        List<SubTaskStatus> tasks = localTransferService.localTasks();
        Set<Integer> reportTaskIds = new LinkedHashSet<>();
        for (SubTaskStatus task : tasks) {
            reportTaskIds.add(task.getParentTaskId() == null ? task.getTaskIndex() : task.getParentTaskId());
        }
        // This legacy endpoint includes node-local rows, but export eligibility
        // must include the parent, discovery and shards on all participating nodes.
        Set<Integer> stoppedScopes = new LinkedHashSet<>();
        for (Integer taskId : reportTaskIds) {
            TaskReportService.ReportScope scope = taskReportService.resolveExportableScope(taskId);
            if (scope != null && scope.isStoppedSnapshot()) stoppedScopes.add(taskId);
        }
        StreamingResponseBody body = output -> reportService.write(output, tasks,
                queryService.localFailures(), Collections.emptyList(), stoppedScopes);
        return ResponseEntity.ok()
                .contentType(MediaType.parseMediaType("application/zip"))
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .header(HttpHeaders.CONTENT_DISPOSITION, "attachment; filename=local-transfer-report.zip")
                .body(body);
    }

    @ExceptionHandler(ResponseStatusException.class)
    public ResponseEntity<ApiResponse<Object>> reportError(ResponseStatusException error) {
        return ResponseEntity.status(error.getStatus()).contentType(MediaType.parseMediaType("application/json;charset=UTF-8"))
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .body(ApiResponse.error("任务带宽设置请通过主节点提交".equals(error.getReason())
                        ? "INTERNAL_AUTH_REQUIRED" : "REPORT_UNAVAILABLE", error.getReason()));
    }

    private void guardTaskBandwidth(LocalTransferRequest request) {
        com.transfer.tools.util.TaskBandwidthRequestGuard.requireInternal(distributedConfig, request != null
                && (request.getTaskBandwidthScopeId() != null || request.getTaskBandwidthLimitMbps() != null));
    }

    private List<Integer> taskIds(Map<String, Object> body) {
        if (body == null || !(body.get("taskIds") instanceof Collection)) return Collections.emptyList();
        List<Integer> ids = new ArrayList<>();
        for (Object item : (Collection<?>) body.get("taskIds")) {
            if (item instanceof Number) ids.add(((Number) item).intValue());
            else ids.add(Integer.valueOf(String.valueOf(item)));
        }
        return ids;
    }

    private List<String> itemKeys(Map<String, Object> body) {
        if (body == null || !(body.get("itemKeys") instanceof Collection)) return Collections.emptyList();
        List<String> keys = new ArrayList<>();
        for (Object item : (Collection<?>) body.get("itemKeys")) keys.add(String.valueOf(item));
        return keys;
    }
}
