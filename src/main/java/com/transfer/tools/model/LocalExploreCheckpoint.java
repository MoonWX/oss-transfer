package com.transfer.tools.model;

import lombok.Data;
import lombok.NoArgsConstructor;
import java.util.List;

/** Durable dispatch boundary; pending ranges keep their original worker assignment on replay. */
@Data
@NoArgsConstructor
public class LocalExploreCheckpoint {
    private String marker = "";
    private int nextShardIndex;
    private boolean complete;
    private List<ListSubTask> pendingTasks;
}
