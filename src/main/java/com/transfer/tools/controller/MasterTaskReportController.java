package com.transfer.tools.controller;

import com.transfer.tools.service.TaskReportService;
import com.transfer.tools.model.ApiResponse;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.*;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.servlet.mvc.method.annotation.StreamingResponseBody;
import org.springframework.web.server.ResponseStatusException;

@RestController
@RequestMapping("/api/master/task-reports")
public class MasterTaskReportController {
    @Autowired private TaskReportService reports;

    @GetMapping(value = "/{taskId}", produces = "application/zip")
    public ResponseEntity<StreamingResponseBody> report(@PathVariable Integer taskId) {
        TaskReportService.ReportScope scope = reports.resolveExportableScope(taskId);
        return ResponseEntity.ok().contentType(MediaType.parseMediaType("application/zip"))
                .header(HttpHeaders.CONTENT_DISPOSITION, "attachment; filename=task-" + scope.getTaskId() + "-report.zip")
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .body(output -> reports.write(scope, output));
    }

    @ExceptionHandler(ResponseStatusException.class)
    public ResponseEntity<ApiResponse<Object>> statusError(ResponseStatusException error) {
        return ResponseEntity.status(error.getStatus()).contentType(MediaType.APPLICATION_JSON)
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .body(ApiResponse.error("REPORT_UNAVAILABLE", error.getReason()));
    }
}
