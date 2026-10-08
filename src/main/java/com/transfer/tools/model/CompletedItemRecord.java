package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class CompletedItemRecord {
    private Integer taskId;
    private TransferDirection direction;
    private String logicalKey;
    private Long size;
    private Long completedAt;
}
