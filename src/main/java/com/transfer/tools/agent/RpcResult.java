package com.transfer.tools.agent;

import lombok.Data;
import java.util.LinkedHashMap;
import java.util.Map;

@Data
public class RpcResult {
    private String id;
    private int status;
    private String body;
    private Map<String, String> headers = new LinkedHashMap<>();
}
