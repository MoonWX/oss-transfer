package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

import java.util.ArrayList;
import java.util.List;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class LocalFileBatch {
    private Integer parentTaskId;
    private String shardId;
    private Integer shardIndex;
    private Integer attempt;
    private String discoveryWorker;
    private LocalTransferRequest request;
    private LocalNodeBinding nodeBinding;
    private List<LocalFileEntry> entries = new ArrayList<>();
    private Boolean lastBatch;
}
