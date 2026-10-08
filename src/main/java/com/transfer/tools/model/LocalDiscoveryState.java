package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@NoArgsConstructor
public class LocalDiscoveryState {
    public enum Status { PENDING, RUNNING, COMPLETED, FAILED, STOPPED }

    private Integer parentTaskId;
    private LocalTransferRequest request;
    private String discoveryWorker;
    private String lastCommittedRelativeKey;
    private Integer nextBatchSequence;
    private Status status;
    private String errorMessage;
    private Long updatedAt;
    /** Exact batch persisted before dispatch; replay this snapshot until its acknowledgment is durable. */
    private LocalFileBatch pendingBatch;

    public LocalDiscoveryState(Integer parentTaskId, LocalTransferRequest request, String discoveryWorker,
                               String lastCommittedRelativeKey, Integer nextBatchSequence, Status status,
                               String errorMessage, Long updatedAt) {
        this.parentTaskId = parentTaskId; this.request = request; this.discoveryWorker = discoveryWorker;
        this.lastCommittedRelativeKey = lastCommittedRelativeKey; this.nextBatchSequence = nextBatchSequence;
        this.status = status; this.errorMessage = errorMessage; this.updatedAt = updatedAt;
    }
}
