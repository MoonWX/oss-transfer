package com.transfer.tools.model;

import com.fasterxml.jackson.annotation.JsonProperty;
import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class TaskImport {
    private Integer taskIndex;
    private String sourceBucket;
    private String destBucket;
    private String sourceS3Alias;
    private String destS3Alias;
    private String startKey;
    private String endKey;
    private String marker;
    private String workerNode;
    private String dateAfter;
    private String dateBefore;
    private Boolean overwrite;
    private TransferDirection direction;
    private TransferEndpoint sourceEndpoint;
    private TransferEndpoint destinationEndpoint;
    private ConflictPolicy conflictPolicy;
    private String localPath;
    private String executionNodeIp;
    private String objectKey;
    /** Explicit source filter. Kept separate from exclusive startKey/endKey boundaries. */
    @JsonProperty(access = JsonProperty.Access.WRITE_ONLY)
    private String prefix;
    private String targetRelativePath;
    private String distributionMode;
    private Integer parentTaskId;
    private String shardId;
    private Integer shardIndex;
    private Integer attempt;
    private Boolean autoStart;
    private String storageId;
    private java.util.List<String> eligibleWorkerNodes;
    private java.util.List<LocalNodeBinding> nodeBindings;
    private java.util.List<LocalFileEntry> localFileEntries;
    private Double listQpsLimit;
    private Double headQpsLimit;
    private Double putQpsLimit;
    private Double bandwidthBitsPerSecond;
    /** Additional logical-task total budget, in decimal Mbps. Null means no additional limit. */
    private Double taskBandwidthLimitMbps;
    private String taskBandwidthScopeId;
    private OssKeyMapping keyMapping;
}
