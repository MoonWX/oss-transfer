package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class BucketPair {
    private String creationId;
    private String sourceBucket;
    private String destBucket;
    private String sourceS3Alias;
    private String destS3Alias;
    private String dateAfter;
    private String dateBefore;
    private String prefix;
    private Boolean overwrite;
    private String distributionMode;
    private TransferDirection direction;
    private TransferEndpoint sourceEndpoint;
    private TransferEndpoint destinationEndpoint;
    private ConflictPolicy conflictPolicy;
    private String localPath;
    private String executionNodeIp;
    private String objectKey;
    private String targetRelativePath;
    private Integer parentTaskId;
    private String storageId;
    private java.util.List<String> eligibleWorkerNodes;
    private java.util.List<LocalNodeBinding> nodeBindings;
    private Double listQpsLimit;
    private Double headQpsLimit;
    private Double putQpsLimit;
    private Double bandwidthBitsPerSecond;
    /** Additional logical-task total budget, in decimal Mbps. Null means no additional limit. */
    private Double taskBandwidthLimitMbps;
    private String taskBandwidthScopeId;
    private OssKeyMapping keyMapping;
}
