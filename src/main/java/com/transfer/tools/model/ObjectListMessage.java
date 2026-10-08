package com.transfer.tools.model;

import com.amazonaws.services.s3.model.S3ObjectSummary;
import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@NoArgsConstructor
public class ObjectListMessage {
    private Integer executionAttempt;
    private S3ObjectSummary objectSummary;
    private Integer taskIndex;
    private Boolean isLastMessage;
    private String sourceBucket;
    private String destBucket;
    private String sourceS3Alias;
    private String destS3Alias;
    private Boolean overwrite;
    private String key;
    private TransferDirection direction;
    private TransferEndpoint sourceEndpoint;
    private TransferEndpoint destinationEndpoint;
    private ConflictPolicy conflictPolicy;
    private String localPath;
    private String executionNodeIp;
    private String targetRelativePath;
    private String reportReasonCode;
    private Long sourceSize;
    private Boolean sourceSizeUnknown;
    private Long sourceLastModified;
    private Integer parentTaskId;
    private String shardId;
    private Integer shardIndex;
    private String storageId;
    private String workerNode;
    private Double listQpsLimit;
    private Double headQpsLimit;
    private Double putQpsLimit;
    private Double bandwidthBitsPerSecond;
    /** Additional logical-task total budget, in decimal Mbps. Null means no additional limit. */
    private Double taskBandwidthLimitMbps;
    private String taskBandwidthScopeId;
    private OssKeyMapping keyMapping;

    public ObjectListMessage(S3ObjectSummary objectSummary, Integer taskIndex, Boolean isLastMessage,
                             String sourceBucket, String destBucket, String sourceS3Alias,
                             String destS3Alias, Boolean overwrite, String key) {
        this.objectSummary = objectSummary;
        this.taskIndex = taskIndex;
        this.isLastMessage = isLastMessage;
        this.sourceBucket = sourceBucket;
        this.destBucket = destBucket;
        this.sourceS3Alias = sourceS3Alias;
        this.destS3Alias = destS3Alias;
        this.overwrite = overwrite;
        this.key = key;
    }
}
