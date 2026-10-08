package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@NoArgsConstructor
public class ListSubTask {
    private String sourceBucket;
    private String destBucket;
    private String sourceS3Alias;
    private String destS3Alias;
    private String startKey;
    private String endKey;
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
    /** Planning weights for new assignments; not progress or a source snapshot. */
    private Long estimatedObjectCount;
    private Long estimatedObjectSize;
    private Double listQpsLimit;
    private Double headQpsLimit;
    private Double putQpsLimit;
    private Double bandwidthBitsPerSecond;
    /** Additional logical-task total budget, in decimal Mbps. Null means no additional limit. */
    private Double taskBandwidthLimitMbps;
    private String taskBandwidthScopeId;
    private OssKeyMapping keyMapping;

    public ListSubTask(String sourceBucket, String destBucket, String sourceS3Alias,
                       String destS3Alias, String startKey, String endKey, String dateAfter,
                       String dateBefore, Boolean overwrite) {
        this.sourceBucket = sourceBucket;
        this.destBucket = destBucket;
        this.sourceS3Alias = sourceS3Alias;
        this.destS3Alias = destS3Alias;
        this.startKey = startKey;
        this.endKey = endKey;
        this.dateAfter = dateAfter;
        this.dateBefore = dateBefore;
        this.overwrite = overwrite;
    }
}
