package com.transfer.tools.controller;

import com.transfer.tools.model.ApiResponse;
import com.transfer.tools.service.WorkerTaskChildrenService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpStatus;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.server.ResponseStatusException;
import java.util.*;

@RestController
@RequestMapping("/api/worker")
public class WorkerTaskChildrenController {
    @Autowired private WorkerTaskChildrenService query;
    @GetMapping("/task-children")
    public ApiResponse<Map<String, Object>> children(@RequestParam int parentTaskId,
            @RequestParam(defaultValue = "0") int offset, @RequestParam(defaultValue = "25") int limit,
            @RequestParam(required = false) List<Integer> taskIds,
            @RequestParam(required = false) String creationId, @RequestParam(defaultValue = "false") boolean includeIndex) {
        if (parentTaskId < 1 || offset < 0 || limit < 1 || limit > 200 || (taskIds != null && taskIds.size() > 200)
                || (creationId != null && !creationId.matches("[A-Za-z0-9_-]{1,128}")))
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "Invalid child task window");
        return ApiResponse.success(query.query(parentTaskId, offset, limit, taskIds, creationId, includeIndex));
    }
}
