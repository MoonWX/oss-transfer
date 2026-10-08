package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

import java.util.List;
import java.util.stream.Collectors;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class MigrationStatusVO {
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
    private List<SubTaskStatusVO> tasks;

    public static MigrationStatusVO from(MigrationStatus status) {
        MigrationStatusVO vo = new MigrationStatusVO();
        vo.setTotalTasks(status.getTotalTasks());
        vo.setPendingTasks(status.getPendingTasks());
        vo.setRunningTasks(status.getRunningTasks());
        vo.setCompletedTasks(status.getCompletedTasks());
        vo.setPartialFailedTasks(status.getPartialFailedTasks());
        vo.setFailedTasks(status.getFailedTasks());
        vo.setListDoneTasks(countListDoneTasks(status));
        vo.setTotalListObjectCount(status.getTotalListObjectCount());
        vo.setTotalListObjectSize(status.getTotalListObjectSize());
        vo.setTotalPutObjectCount(status.getTotalPutObjectCount());
        vo.setTotalPutObjectSize(status.getTotalPutObjectSize());
        vo.setTotalFailedObjectCount(status.getTotalFailedObjectCount());
        vo.setTotalFailedObjectSize(status.getTotalFailedObjectSize());
        vo.setTotalSkippedObjectCount(status.getTotalSkippedObjectCount());
        vo.setTotalSkippedObjectSize(status.getTotalSkippedObjectSize());
        vo.setTasks(status.getTasks().stream()
                .map(SubTaskStatusVO::from)
                .collect(Collectors.toList()));
        return vo;
    }

    private static Integer countListDoneTasks(MigrationStatus status) {
        return (int) status.getTasks().stream()
                .filter(t -> t.getStatus() == SubTaskStatus.TaskStatus.LIST_DONE)
                .count();
    }
}
