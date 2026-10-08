package com.transfer.tools.model;

import com.transfer.tools.model.SubTaskStatus;
import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class ProgressMessage {
    private Integer taskIndex;
    private Long listCount;
    private Long listSize;
    private Long putCount;
    private Long putSize;
    private Long failedCount;
    private Long failedSize;
    private Long skippedCount;
    private Long skippedSize;
    private String marker;
    private SubTaskStatus.TaskStatus newStatus;
}
