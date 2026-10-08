package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class LocalParentTaskRecord {
    private Integer parentTaskId;
    private LocalTransferRequest request;
    private SubTaskStatus.TaskStatus status;
    private String errorCode;
    private String errorMessage;
    private Boolean orchestrationComplete;
    private Long createTime;
    private Long startTime;
    private Long endTime;
    private Long updatedAt;
}
