package com.transfer.tools.model;

import com.fasterxml.jackson.annotation.JsonProperty;
import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

import java.text.SimpleDateFormat;
import java.util.Date;
import java.util.TimeZone;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class FailedKey {
    private Integer taskIndex;
    private String sourceBucket;
    private String destBucket;
    private String key;
    /** Actual OSS destination; key remains the source identity for retries. */
    private String destinationKey;
    private Long timestamp;
    private String errorMessage;
    private Long objectSize;
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

    private static final SimpleDateFormat DATE_FORMAT;

    static {
        DATE_FORMAT = new SimpleDateFormat("yyyy-MM-dd HH:mm:ss");
        DATE_FORMAT.setTimeZone(TimeZone.getTimeZone("Asia/Shanghai"));
    }

    @JsonProperty(access = JsonProperty.Access.READ_ONLY)
    public String getTimestampFormatted() {
        if (timestamp == null) {
            return null;
        }
        synchronized (DATE_FORMAT) {
            return DATE_FORMAT.format(new Date(timestamp));
        }
    }
}
