package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

import java.util.List;

@Data
@NoArgsConstructor
public class TransferEndpoint {
    private EndpointType type;
    private String s3Alias;
    private String bucket;
    private String objectKey;
    private String prefix;
    /** Explicit LIST filter; null preserves legacy OSS range tasks whose prefix was inferred from startKey. */
    private Boolean prefixFilterEnabled;
    private String executionNodeIp;
    private String localPath;
    private String storageId;
    private List<LocalNodeBinding> nodeBindings;

    public TransferEndpoint(EndpointType type, String s3Alias, String bucket, String objectKey,
                            String prefix, String executionNodeIp, String localPath) {
        this.type = type;
        this.s3Alias = s3Alias;
        this.bucket = bucket;
        this.objectKey = objectKey;
        this.prefix = prefix;
        this.executionNodeIp = executionNodeIp;
        this.localPath = localPath;
    }
}
