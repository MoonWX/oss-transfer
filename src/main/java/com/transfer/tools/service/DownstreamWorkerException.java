package com.transfer.tools.service;

public class DownstreamWorkerException extends RuntimeException {
    private final String code;

    public DownstreamWorkerException(String code, String message, Throwable cause) {
        super(message, cause);
        this.code = code;
    }

    public String getCode() {
        return code;
    }
}
