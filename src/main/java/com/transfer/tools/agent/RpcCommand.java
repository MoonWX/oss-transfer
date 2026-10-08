package com.transfer.tools.agent;

import lombok.Data;

@Data
public class RpcCommand {
    private String id;
    private String method;
    private String path;
    private String body;
    private String contentType;
    private long deadline;
}
