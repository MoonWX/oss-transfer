package com.transfer.tools.agent;

import lombok.Data;

@Data
public class RpcRelayRequest {
    private String address;
    private String method;
    private String path;
    private String body;
    private String contentType;
}
