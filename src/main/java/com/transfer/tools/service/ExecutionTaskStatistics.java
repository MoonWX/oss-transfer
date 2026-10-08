package com.transfer.tools.service;

import com.transfer.tools.model.SubTaskStatus;
import java.util.HashSet;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Set;

/** Counts the current worker execution index, never master parent summaries. */
public final class ExecutionTaskStatistics {
    private ExecutionTaskStatistics() { }

    public static Map<String, Object> fromStoredExecutionTasks(List<SubTaskStatus> tasks) {
        // A missing snapshot is unknown, not a confirmed empty execution index.
        if (tasks == null) return null;
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("scope", "CURRENT_EXECUTION_SUBTASKS");
        for (String key : new String[]{"totalTasks", "pendingTasks", "runningTasks", "listDoneTasks",
                "completedTasks", "partialFailedTasks", "failedTasks", "stoppedTasks", "unknownTasks"}) {
            result.put(key, 0);
        }
        Set<Integer> seen = new HashSet<>();
        for (SubTaskStatus task : tasks) {
            if (task == null || task.getTaskIndex() == null || "discovery".equals(task.getShardId())
                    || !seen.add(task.getTaskIndex())) continue;
            increment(result, "totalTasks");
            if (task.getStatus() == null) { increment(result, "unknownTasks"); continue; }
            switch (task.getStatus()) {
                case PENDING: increment(result, "pendingTasks"); break;
                case RUNNING: increment(result, "runningTasks"); break;
                case LIST_DONE: increment(result, "listDoneTasks"); break;
                case COMPLETED: increment(result, "completedTasks"); break;
                case PARTIAL_FAILED: increment(result, "partialFailedTasks"); break;
                case FAILED: increment(result, "failedTasks"); break;
                case STOPPED: increment(result, "stoppedTasks"); break;
                default: increment(result, "unknownTasks");
            }
        }
        return result;
    }

    private static void increment(Map<String, Object> values, String key) {
        values.put(key, ((Integer) values.get(key)) + 1);
    }
}
