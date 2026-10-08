package com.transfer.tools.agent;

import lombok.Data;
import lombok.AllArgsConstructor;
import lombok.NoArgsConstructor;
import java.util.LinkedHashMap;
import java.util.Map;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class RpcResponse {
    private int status;
    private byte[] body;
    private Map<String, String> headers = new LinkedHashMap<>();
}
