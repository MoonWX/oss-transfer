package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class FailedKeyMessage {
    private Integer taskIndex;
    private String sourceBucket;
    private String destBucket;
    private String key;
    /** Actual OSS destination; key remains the source identity for retries. */
    private String destinationKey;
    private Long objectSize;
    private String errorMessage;
    private TransferDirection direction;
    private TransferEndpoint sourceEndpoint;
    private TransferEndpoint destinationEndpoint;
    private String localPath;
    private String executionNodeIp;
    private String workerNode;
    private FailureStage stage;
    private String errorCode;
    private Integer retryCount;
    private Integer parentTaskId;
    private String shardId;
    private Integer attempt;
    private String storageId;
    private java.util.List<String> eligibleWorkerNodes;
}
