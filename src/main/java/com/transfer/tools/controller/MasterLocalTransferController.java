package com.transfer.tools.controller;

import com.transfer.tools.model.*;
import com.transfer.tools.service.*;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.*;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.servlet.mvc.method.annotation.StreamingResponseBody;
import org.springframework.web.server.ResponseStatusException;

import java.util.*;

@RestController
@RequestMapping("/api/master")
public class MasterLocalTransferController {
    @Autowired private MasterLocalTransferService masterService;
    @Autowired private LocalReportService reportService;
    @Autowired private TaskReportService taskReportService;

    @GetMapping("/local-nodes")
    public ApiResponse<Map<String, Object>> nodes() {
        return ApiResponse.success(masterService.nodes());
    }

    @PostMapping("/local-transfers")
    public ApiResponse<Map<String, Object>> create(@RequestBody List<LocalTransferRequest> requests) {
        return ApiResponse.success(masterService.create(requests));
    }

    @PostMapping("/local-transfers/validate")
    public ApiResponse<Map<String, Object>> validate(@RequestBody List<LocalTransferRequest> requests) {
        return ApiResponse.success(masterService.validate(requests));
    }

    @GetMapping("/local-transfers/status")
    public ApiResponse<Map<String, Object>> status(
            @RequestParam(required = false) Integer taskId,
            @RequestParam(required = false) TransferDirection direction,
            @RequestParam(required = false) String executionNodeIp,
            @RequestParam(required = false) String status,
            @RequestParam(required = false) String endpoint,
            @RequestParam(required = false) String storageId) {
        return ApiResponse.success(masterService.aggregateStatus(taskId, direction, executionNodeIp, status, endpoint, storageId));
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
        return ApiResponse.success(masterService.aggregateFailures(taskId, direction, executionNodeIp,
                endpoint, storageId, offset, limit));
    }

    @PostMapping("/local-transfers/stop")
    public ApiResponse<Map<String, Object>> stop(@RequestBody(required = false) Map<String, Object> body) {
        return ApiResponse.success(masterService.control("stop", body));
    }

    @PostMapping("/local-transfers/resume")
    public ApiResponse<Map<String, Object>> resume(@RequestBody(required = false) Map<String, Object> body) {
        return ApiResponse.success(masterService.control("resume", body));
    }

    @PostMapping("/local-transfers/retry-failures")
    public ApiResponse<Map<String, Object>> retry(@RequestBody(required = false) Map<String, Object> body) {
        return ApiResponse.success(masterService.control("retry-failures", body));
    }

    @GetMapping(value = "/local-transfers/report", produces = "application/zip")
    public ResponseEntity<StreamingResponseBody> report() {
        List<String> unreachable = new ArrayList<>();
        List<SubTaskStatus> tasks = masterService.reportTasks(unreachable);
        if (!unreachable.isEmpty()) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE,
                "执行节点不可达，无法确认任务已结束或停止");
        if (tasks.stream().anyMatch(task -> !TaskReportService.isExportableStatus(task.getStatus()))) {
            throw new ResponseStatusException(HttpStatus.CONFLICT, "存在运行中或待执行任务，请在任务详情中导出已结束或已停止任务的报告");
        }
        Set<Integer> stoppedScopes = new LinkedHashSet<>();
        for (SubTaskStatus task : tasks) {
            TaskReportService.ReportScope scope = taskReportService.resolveExportableScope(task.getTaskIndex());
            if (scope != null && scope.isStoppedSnapshot()) stoppedScopes.add(task.getTaskIndex());
        }
        List<FailedKey> failures = masterService.reportFailures(unreachable);
        if (!unreachable.isEmpty()) throw new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE,
                "执行节点报告读取不完整，请稍后重试");
        StreamingResponseBody body = output -> reportService.write(output, tasks, failures, unreachable, stoppedScopes);
        return ResponseEntity.ok()
                .contentType(MediaType.parseMediaType("application/zip"))
                .header(HttpHeaders.CONTENT_DISPOSITION, "attachment; filename=local-transfer-report.zip")
                .body(body);
    }

    @ExceptionHandler(ResponseStatusException.class)
    public ResponseEntity<ApiResponse<Object>> reportError(ResponseStatusException error) {
        return ResponseEntity.status(error.getStatus()).contentType(MediaType.APPLICATION_JSON).header(HttpHeaders.CACHE_CONTROL, "no-store")
                .body(ApiResponse.error("REPORT_UNAVAILABLE", error.getReason()));
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
}
