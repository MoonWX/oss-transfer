package com.transfer.tools.model;

import lombok.Data;

@Data
public class FileReportRecord {
    private Integer reportTaskId;
    private Integer taskId;
    private Integer parentTaskId;
    private String shardId;
    private TransferDirection direction;
    private String logicalKey;
    private String sourceS3Alias;
    private String sourceBucket;
    private String sourceKey;
    private String sourcePath;
    private String destinationS3Alias;
    private String destinationBucket;
    private String destinationKey;
    private String destinationPath;
    private Long size;
    private String node;
    private String result;
    private String reasonCode;
    private String errorMessage;
    private Long discoveredAt;
    private Long completedAt;
}
