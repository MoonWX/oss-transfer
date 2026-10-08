package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class LocalTransferRequest {
    /** Assigned by Master for Worker dispatch; omitted by external callers. */
    private Integer taskId;
    /** Stable logical task id shared by every Worker shard. */
    private Integer parentTaskId;
    private TransferDirection direction;
    private TransferEndpoint sourceEndpoint;
    private TransferEndpoint destinationEndpoint;
    private ConflictPolicy conflictPolicy;
    private String dateAfter;
    private String dateBefore;
    /** Optional rename for an exact OSS object at a local destination. */
    private String targetRelativePath;
    private String distributionMode;
    /** Master-calculated cluster budget shares; internal dispatch fields. */
    private Double listQpsLimit;
    private Double headQpsLimit;
    private Double putQpsLimit;
    private Double bandwidthBitsPerSecond;
    /** Additional logical-task total budget, in decimal Mbps. Null means no additional limit. */
    private Double taskBandwidthLimitMbps;
    private String taskBandwidthScopeId;
}
