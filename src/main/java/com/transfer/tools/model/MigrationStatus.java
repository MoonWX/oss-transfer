package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

import java.util.List;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class MigrationStatus {
    private Integer totalTasks;
    private Integer pendingTasks;
    private Integer runningTasks;
    private Integer completedTasks;
    private Integer partialFailedTasks;
    private Integer failedTasks;
    private Integer listDoneTasks;
    private Long totalListObjectCount;
    private Long totalListObjectSize;
    private Long totalPutObjectCount;
    private Long totalPutObjectSize;
    private Long totalFailedObjectCount;
    private Long totalFailedObjectSize;
    private Long totalSkippedObjectCount;
    private Long totalSkippedObjectSize;
    private List<SubTaskStatus> tasks;
}
