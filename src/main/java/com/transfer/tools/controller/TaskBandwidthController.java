package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.JsonNode;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.ApiResponse;
import com.transfer.tools.model.TaskBandwidthPolicy;
import com.transfer.tools.service.*;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.*;
import org.springframework.web.bind.annotation.*;
import javax.servlet.http.HttpServletRequest;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.Map;

@RestController
public class TaskBandwidthController {
    @Autowired private MasterTaskBandwidthService master;
    @Autowired private WorkerTaskBandwidthService worker;
    @Autowired private DistributedConfig config;

    @GetMapping("/api/master/tasks/{taskId}/bandwidth-limit")
    public ApiResponse<Map<String,Object>> get(@PathVariable Integer taskId) {
        return ApiResponse.success(master.get(taskId));
    }

    @PutMapping("/api/master/tasks/{taskId}/bandwidth-limit")
    public ApiResponse<Map<String,Object>> update(@PathVariable Integer taskId, @RequestBody JsonNode body) {
        if (body == null || !body.isObject() || !body.has("limitMbps"))
            throw new TransferValidationException("INVALID_TASK_BANDWIDTH", "请提供 limitMbps，设为 null 可取消额外限制");
        JsonNode limit = body.get("limitMbps");
        if (!limit.isNull() && !limit.isNumber())
            throw new TransferValidationException("INVALID_TASK_BANDWIDTH", "任务带宽上限必须为数值或 null");
        Double value = limit.isNull() ? null : limit.doubleValue();
        MasterTaskBandwidthService.validateLimit(value);
        return ApiResponse.success(master.update(taskId, value));
    }

    @GetMapping("/api/worker/task-bandwidth-policy")
    public ResponseEntity<ApiResponse<TaskBandwidthPolicy>> workerGet(@RequestParam String scopeId, HttpServletRequest request) {
        if (!internal(request)) return forbidden();
        return ResponseEntity.ok(ApiResponse.success(worker.get(scopeId)));
    }

    @PutMapping("/api/worker/task-bandwidth-policy")
    public ResponseEntity<ApiResponse<TaskBandwidthPolicy>> workerApply(@RequestBody TaskBandwidthPolicy policy, HttpServletRequest request) {
        if (!internal(request)) return forbidden();
        return ResponseEntity.ok(ApiResponse.success(worker.apply(policy)));
    }
    private boolean internal(HttpServletRequest request) {
        String expected = config.getInternalToken();
        String provided = request.getHeader("X-Internal-Token");
        return expected != null && !expected.isEmpty() && provided != null
                && MessageDigest.isEqual(expected.getBytes(StandardCharsets.UTF_8), provided.getBytes(StandardCharsets.UTF_8));
    }
    private ResponseEntity<ApiResponse<TaskBandwidthPolicy>> forbidden() {
        return ResponseEntity.status(HttpStatus.FORBIDDEN).body(ApiResponse.error("INTERNAL_AUTH_REQUIRED", "仅允许节点内部调用"));
    }
    @ExceptionHandler(TransferValidationException.class)
    public ResponseEntity<ApiResponse<Object>> invalid(TransferValidationException error) {
        HttpStatus status = "TASK_NOT_FOUND".equals(error.getCode()) ? HttpStatus.NOT_FOUND : HttpStatus.BAD_REQUEST;
        return ResponseEntity.status(status).body(ApiResponse.error(error.getCode(), error.getMessage()));
    }
    @ExceptionHandler(DownstreamWorkerException.class)
    public ResponseEntity<ApiResponse<Object>> unavailable(DownstreamWorkerException error) {
        return ResponseEntity.status(HttpStatus.SERVICE_UNAVAILABLE).body(ApiResponse.error(error.getCode(), "节点不可达，无法确认任务设置"));
    }
    @ExceptionHandler(IllegalArgumentException.class)
    public ResponseEntity<ApiResponse<Object>> invalidPolicy(IllegalArgumentException error) {
        return ResponseEntity.badRequest().body(ApiResponse.error("INVALID_TASK_BANDWIDTH", "任务带宽策略无效或修订版本冲突"));
    }
    @ExceptionHandler(Exception.class)
    public ResponseEntity<ApiResponse<Object>> failed(Exception error) {
        return ResponseEntity.status(HttpStatus.INTERNAL_SERVER_ERROR).body(ApiResponse.error("TASK_BANDWIDTH_FAILED", "无法保存任务带宽设置，请稍后重试"));
    }

}
