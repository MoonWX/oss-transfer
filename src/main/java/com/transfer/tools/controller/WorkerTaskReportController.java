package com.transfer.tools.controller;

import com.transfer.tools.model.*;
import com.transfer.tools.service.FileReportLedgerService;
import com.transfer.tools.service.TaskReportWorkerService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpStatus;
import org.springframework.http.MediaType;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.server.ResponseStatusException;

@RestController
@RequestMapping("/api/worker/task-reports")
public class WorkerTaskReportController {
    @Autowired private TaskReportWorkerService reports;
    @Autowired private FileReportLedgerService ledger;

    @GetMapping("/{taskId}/scope")
    public ApiResponse<TaskReportWorkerSnapshot> scope(@PathVariable Integer taskId) {
        validate(taskId);
        return ApiResponse.success(reports.scope(taskId));
    }

    @GetMapping("/{taskId}/files")
    public ApiResponse<FileReportPage> files(@PathVariable Integer taskId,
            @RequestParam(required = false) String cursor,
            @RequestParam(defaultValue = "500") int limit) {
        validate(taskId);
        if (limit < 1 || limit > 1000) throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "分页大小必须为 1 至 1000");
        if (cursor == null) {
            TaskReportWorkerSnapshot snapshot = reports.scope(taskId);
            if (!snapshot.isFound() && !snapshot.isScopeMissing()) throw new ResponseStatusException(HttpStatus.NOT_FOUND, "任务不存在或已永久删除");
        }
        try {
            return ApiResponse.success(ledger.page(taskId, cursor, limit));
        } catch (IllegalArgumentException error) {
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "报告游标无效", error);
        }
    }

    @ExceptionHandler(ResponseStatusException.class)
    public ResponseEntity<ApiResponse<Object>> statusError(ResponseStatusException error) {
        return ResponseEntity.status(error.getStatus()).contentType(MediaType.APPLICATION_JSON)
                .body(ApiResponse.error("REPORT_UNAVAILABLE", error.getReason()));
    }

    private void validate(Integer taskId) {
        if (taskId == null || taskId <= 0) throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "任务 ID 无效");
    }
}
