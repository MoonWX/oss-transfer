package com.transfer.tools.controller;

import com.transfer.tools.model.ApiResponse;
import com.transfer.tools.service.ReportDownloadTicketService;
import com.transfer.tools.service.TaskReportService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpHeaders;
import org.springframework.http.MediaType;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.server.ResponseStatusException;
import org.springframework.web.servlet.mvc.method.annotation.StreamingResponseBody;

import java.util.Collections;
import java.util.Map;

/** Native attachment delivery keeps large ZIPs out of the JavaScript heap. */
@RestController
@RequestMapping("/api/master/task-reports")
public class TaskReportDownloadController {
    @Autowired private TaskReportService reportService;
    @Autowired private ReportDownloadTicketService ticketService;

    @GetMapping("/{taskId}/download-ticket")
    public ResponseEntity<ApiResponse<Map<String, String>>> ticket(@PathVariable Integer taskId,
            @RequestHeader(value = HttpHeaders.AUTHORIZATION, required = false) String authorization) {
        reportService.resolveExportableScope(taskId); // Fail before handing the browser a download request.
        String token = authorization == null ? null : authorization.replaceFirst("^Bearer ", "").trim();
        String ticket = ticketService.issue(taskId, token);
        return ResponseEntity.ok().header(HttpHeaders.CACHE_CONTROL, "no-store")
                .body(ApiResponse.success(Collections.singletonMap("ticket", ticket)));
    }

    @PostMapping(value = "/{taskId}/download", produces = "application/zip")
    public ResponseEntity<StreamingResponseBody> download(@PathVariable Integer taskId) {
        TaskReportService.ReportScope scope = reportService.resolveExportableScope(taskId);
        return ResponseEntity.ok().contentType(MediaType.parseMediaType("application/zip"))
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .header("X-Content-Type-Options", "nosniff")
                .header(HttpHeaders.CONTENT_DISPOSITION, "attachment; filename=task-" + scope.getTaskId() + "-report.zip")
                .body(output -> reportService.write(scope, output));
    }

    @ExceptionHandler(ResponseStatusException.class)
    public ResponseEntity<ApiResponse<Object>> statusError(ResponseStatusException error) {
        return ResponseEntity.status(error.getStatus()).contentType(MediaType.APPLICATION_JSON)
                .header(HttpHeaders.CACHE_CONTROL, "no-store")
                .body(ApiResponse.error("REPORT_UNAVAILABLE", error.getReason()));
    }
}
