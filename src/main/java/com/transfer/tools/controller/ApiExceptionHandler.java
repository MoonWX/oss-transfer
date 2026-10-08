package com.transfer.tools.controller;

import com.transfer.tools.model.ApiResponse;
import com.transfer.tools.service.TaskCancelledException;
import com.transfer.tools.service.TransferValidationException;
import com.transfer.tools.service.DownstreamWorkerException;
import org.springframework.http.HttpStatus;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.ExceptionHandler;
import org.springframework.web.bind.annotation.RestControllerAdvice;
import org.springframework.http.converter.HttpMessageNotReadableException;
import org.springframework.web.method.annotation.MethodArgumentTypeMismatchException;

@RestControllerAdvice(assignableTypes = {WorkerLocalTransferController.class, MasterLocalTransferController.class})
public class ApiExceptionHandler {
    @ExceptionHandler({HttpMessageNotReadableException.class, MethodArgumentTypeMismatchException.class})
    public ResponseEntity<ApiResponse<Object>> malformed(Exception error) {
        return ResponseEntity.status(HttpStatus.BAD_REQUEST)
                .body(ApiResponse.error("INVALID_DIRECTION", "invalid request value"));
    }

    @ExceptionHandler(TransferValidationException.class)
    public ResponseEntity<ApiResponse<Object>> validation(TransferValidationException error) {
        return ResponseEntity.status(HttpStatus.BAD_REQUEST)
                .body(ApiResponse.error(error.getCode(), error.getMessage()));
    }

    @ExceptionHandler(TaskCancelledException.class)
    public ResponseEntity<ApiResponse<Object>> conflict(TaskCancelledException error) {
        return ResponseEntity.status(HttpStatus.CONFLICT)
                .body(ApiResponse.error("TASK_STOPPED", error.getMessage()));
    }

    @ExceptionHandler(DownstreamWorkerException.class)
    public ResponseEntity<ApiResponse<Object>> downstream(DownstreamWorkerException error) {
        return ResponseEntity.status(HttpStatus.BAD_GATEWAY)
                .body(ApiResponse.error(error.getCode(), error.getMessage()));
    }

    @ExceptionHandler(Exception.class)
    public ResponseEntity<ApiResponse<Object>> unexpected(Exception error) {
        return ResponseEntity.status(HttpStatus.INTERNAL_SERVER_ERROR)
                .body(ApiResponse.error("INTERNAL_ERROR", error.getMessage()));
    }
}
