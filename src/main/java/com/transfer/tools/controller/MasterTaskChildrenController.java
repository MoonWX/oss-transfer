package com.transfer.tools.controller;

import com.transfer.tools.model.ApiResponse;
import com.transfer.tools.service.MasterTaskChildrenService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.HttpStatus;
import org.springframework.web.bind.annotation.*;
import org.springframework.web.server.ResponseStatusException;
import java.util.*;

@RestController
@RequestMapping("/api/master/tasks")
public class MasterTaskChildrenController {
    @Autowired private MasterTaskChildrenService query;
    @GetMapping("/{parentId}/children")
    public ApiResponse<Map<String, Object>> children(@PathVariable int parentId,
            @RequestParam(defaultValue = "0") int offset, @RequestParam(defaultValue = "25") int limit,
            @RequestParam(required = false) String executionNodeIp) {
        return ApiResponse.success(query.query(parentId, offset, limit, executionNodeIp));
    }
    @GetMapping("/{parentId}/children/{number}")
    public ApiResponse<Map<String, Object>> child(@PathVariable int parentId, @PathVariable int number,
            @RequestParam(required = false) String executionNodeIp) {
        if (number < 1) throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "Invalid child task number");
        return ApiResponse.success(query.query(parentId, number - 1, 1, executionNodeIp));
    }
}
