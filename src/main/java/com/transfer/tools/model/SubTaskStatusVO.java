package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

import java.text.SimpleDateFormat;
import java.util.Date;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class SubTaskStatusVO {
    private Integer taskIndex;
    private String sourceBucket;
    private String destBucket;
    private String sourceS3Alias;
    private String destS3Alias;
    private String startKey;
    private String endKey;
    private String status;
    private Long listObjectCount;
    private Long listObjectSize;
    private Long putObjectCount;
    private Long putObjectSize;
    private Long failedObjectCount;
    private Long failedObjectSize;
    private Long skippedObjectCount;
    private Long skippedObjectSize;
    private String createTime;
    private String startTime;
    private String endTime;
    private String duration;
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
    private Long currentSpeedBytesPerSecond;
    private Long averageSpeedBytesPerSecond;
    private String distributionMode;
    private Integer parentTaskId;
    private String shardId;
    private Integer shardIndex;
    private Integer attempt;
    private Boolean autoStart;
    private String storageId;
    private java.util.List<String> eligibleWorkerNodes;
    private java.util.List<String> participatingWorkerNodes;
    private java.util.List<LocalNodeBinding> nodeBindings;
    private Double listQpsLimit;
    private Double headQpsLimit;
    private Double putQpsLimit;
    private Double bandwidthBitsPerSecond;
    /** Additional logical-task total budget, in decimal Mbps. Null means no additional limit. */
    private Double taskBandwidthLimitMbps;
    private String taskBandwidthScopeId;
    private OssKeyMapping keyMapping;
    private String objectKey;
    private String targetRelativePath;
    private String errorCode;
    private String errorMessage;

    private static final SimpleDateFormat DATE_FORMAT = new SimpleDateFormat("yyyy-MM-dd HH:mm:ss");

    public static SubTaskStatusVO from(SubTaskStatus status) {
        SubTaskStatusVO vo = new SubTaskStatusVO();
        vo.setTaskIndex(status.getTaskIndex());
        vo.setTargetRelativePath(status.getTargetRelativePath());
        vo.setErrorCode(status.getErrorCode()); vo.setErrorMessage(status.getErrorMessage());
        vo.setSourceBucket(status.getSourceBucket());
        vo.setDestBucket(status.getDestBucket());
        vo.setSourceS3Alias(status.getSourceS3Alias());
        vo.setDestS3Alias(status.getDestS3Alias());
        vo.setStartKey(status.getStartKey());
        vo.setEndKey(status.getEndKey());
        vo.setStatus(status.getStatus().name());
        vo.setListObjectCount(status.getListObjectCount());
        vo.setListObjectSize(status.getListObjectSize());
        vo.setPutObjectCount(status.getPutObjectCount());
        vo.setPutObjectSize(status.getPutObjectSize());
        vo.setFailedObjectCount(status.getFailedObjectCount());
        vo.setFailedObjectSize(status.getFailedObjectSize());
        vo.setSkippedObjectCount(status.getSkippedObjectCount());
        vo.setSkippedObjectSize(status.getSkippedObjectSize());
        vo.setCreateTime(formatTime(status.getCreateTime()));
        vo.setStartTime(formatTime(status.getStartTime()));
        vo.setEndTime(formatTime(status.getEndTime()));
        vo.setDuration(formatDuration(status.getStartTime(), status.getEndTime(), status.getStatus()));
        vo.setMarker(status.getMarker());
        vo.setScanComplete(status.getScanComplete());
        vo.setWorkerNode(status.getWorkerNode());
        vo.setDateAfter(status.getDateAfter());
        vo.setDateBefore(status.getDateBefore());
        vo.setOverwrite(status.getOverwrite());
        vo.setDirection(status.getDirection());
        vo.setSourceEndpoint(status.getSourceEndpoint());
        vo.setDestinationEndpoint(status.getDestinationEndpoint());
        vo.setConflictPolicy(status.getConflictPolicy());
        vo.setLocalPath(status.getLocalPath());
        vo.setExecutionNodeIp(status.getExecutionNodeIp());
        vo.setCurrentSpeedBytesPerSecond(status.getCurrentSpeedBytesPerSecond());
        vo.setAverageSpeedBytesPerSecond(status.getAverageSpeedBytesPerSecond());
        vo.setDistributionMode(status.getDistributionMode());
        vo.setParentTaskId(status.getParentTaskId());
        vo.setShardId(status.getShardId());
        vo.setShardIndex(status.getShardIndex());
        vo.setAttempt(status.getAttempt());
        vo.setAutoStart(status.getAutoStart());
        vo.setStorageId(status.getStorageId());
        vo.setEligibleWorkerNodes(status.getEligibleWorkerNodes());
        vo.setParticipatingWorkerNodes(status.getParticipatingWorkerNodes());
        vo.setNodeBindings(status.getNodeBindings());
        vo.setListQpsLimit(status.getListQpsLimit());
        vo.setHeadQpsLimit(status.getHeadQpsLimit());
        vo.setPutQpsLimit(status.getPutQpsLimit());
        vo.setBandwidthBitsPerSecond(status.getBandwidthBitsPerSecond());
        vo.setTaskBandwidthLimitMbps(status.getTaskBandwidthLimitMbps());
        vo.setTaskBandwidthScopeId(status.getTaskBandwidthScopeId());
        vo.setKeyMapping(status.getKeyMapping());
        vo.setObjectKey(status.getObjectKey());
        return vo;
    }

    private static String formatTime(Long timestamp) {
        if (timestamp == null) {
            return null;
        }
        return DATE_FORMAT.format(new Date(timestamp));
    }

    private static String formatDuration(Long startTime, Long endTime, SubTaskStatus.TaskStatus status) {
        if (startTime == null || status == SubTaskStatus.TaskStatus.PENDING) {
            return "-";
        }
        long end = endTime != null ? endTime : System.currentTimeMillis();
        long durationMs = end - startTime;

        long seconds = durationMs / 1000;
        long minutes = seconds / 60;
        long hours = minutes / 60;

        if (hours > 0) {
            return String.format("%dh %dm %ds", hours, minutes % 60, seconds % 60);
        } else if (minutes > 0) {
            return String.format("%dm %ds", minutes, seconds % 60);
        } else {
            return String.format("%ds", seconds);
        }
    }
}
