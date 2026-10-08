package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class SubTaskStatus {
    private Integer taskIndex;
    private String sourceBucket;
    private String destBucket;
    private String sourceS3Alias;
    private String destS3Alias;
    private String startKey;
    private String endKey;
    private TaskStatus status;
    private Long listObjectCount;
    private Long listObjectSize;
    private Long putObjectCount;
    private Long putObjectSize;
    private Long failedObjectCount;
    private Long failedObjectSize;
    private Long skippedObjectCount;
    private Long skippedObjectSize;
    private Long createTime;
    private Long startTime;
    private Long endTime;
    private String marker;
    private Boolean scanComplete;
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
    private String targetRelativePath;
    private Long currentSpeedBytesPerSecond;
    private Long averageSpeedBytesPerSecond;
    private String distributionMode;
    private Integer parentTaskId;
    private String shardId;
    private Integer shardIndex;
    private Integer attempt;
    /** Durable execution intent; null/false preserves legacy manual-start tasks. */
    private Boolean autoStart;
    private String storageId;
    private java.util.List<String> eligibleWorkerNodes;
    private java.util.List<String> participatingWorkerNodes;
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
    /** Parent-level orchestration failure, if task creation/discovery failed before an object shard existed. */
    private String errorCode;
    private String errorMessage;
    /** Immutable imported object list stored in RocksDB, independent of the original .incr file. */
    private String inventoryManifestId;

    public enum TaskStatus {
        PENDING,
        RUNNING,
        LIST_DONE,
        COMPLETED,
        PARTIAL_FAILED,
        FAILED,
        STOPPED
    }
}
