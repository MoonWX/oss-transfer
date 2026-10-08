package com.transfer.tools.service;

public class TransferValidationException extends RuntimeException {
    private final String code;

    public TransferValidationException(String code, String message) {
        super(message);
        this.code = code;
    }

    public String getCode() {
        return code;
    }
}
