package com.transfer.tools.model;

import lombok.Data;

@Data
public class RocksDBPage<T> {
    private java.util.Map<String, T> items = new java.util.LinkedHashMap<>();
    private String nextCursor;
    private boolean done;
}
