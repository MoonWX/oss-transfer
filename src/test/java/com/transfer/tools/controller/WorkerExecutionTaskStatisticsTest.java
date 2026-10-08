package com.transfer.tools.controller;

import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.MigrationStatus;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.service.ExecutionTaskStatistics;
import com.transfer.tools.service.RateLimiterManager;
import com.transfer.tools.service.SubTaskManagerService;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class WorkerExecutionTaskStatisticsTest {
    @Test
    public void workerPublishesExecutionCountsFromItsStoredIndexAndKeepsLegacyFields() {
        SubTaskStatus first = task(101, SubTaskStatus.TaskStatus.COMPLETED, "44-local-0");
        first.setParentTaskId(44);
        SubTaskStatus second = task(102, SubTaskStatus.TaskStatus.COMPLETED, "44-local-1");
        second.setParentTaskId(44);
        SubTaskStatus discovery = task(44, SubTaskStatus.TaskStatus.COMPLETED, "discovery");
        discovery.setParentTaskId(44);
        SubTaskStatus legacy = task(99, SubTaskStatus.TaskStatus.COMPLETED, null);
        legacy.setParentTaskId(99); // Older single-node executions are still real work.
        MigrationStatus snapshot = new MigrationStatus();
        snapshot.setCompletedTasks(99); // Legacy field is preserved, not reused as the new count.
        snapshot.setTasks(Arrays.asList(first, second, discovery, legacy));
        SubTaskManagerService tasks = mock(SubTaskManagerService.class);
        when(tasks.getMigrationStatus(null, null)).thenReturn(snapshot);
        WorkerController worker = new WorkerController();
        ReflectionTestUtils.setField(worker, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(worker, "bufferQueueManager", mock(BufferQueueManager.class));
        ReflectionTestUtils.setField(worker, "rateLimiterManager", mock(RateLimiterManager.class));

        Map<String, Object> response = worker.getWorkerStatus();
        assertEquals(Boolean.TRUE, response.get("success"));
        assertEquals(99, ((Map) response.get("taskStats")).get("completedTasks"));
        Map stats = (Map) response.get("executionTaskStats");
        assertEquals("CURRENT_EXECUTION_SUBTASKS", stats.get("scope"));
        assertEquals(3, stats.get("totalTasks"));
        assertEquals(3, stats.get("completedTasks"));
        verify(tasks, times(1)).getMigrationStatus(null, null);
    }

    @Test
    public void stateCountsDescribeCurrentRetainedExecutionsNotLifetimeTotals() {
        List<SubTaskStatus> retained = new ArrayList<>();
        int id = 1;
        for (SubTaskStatus.TaskStatus state : SubTaskStatus.TaskStatus.values()) retained.add(task(id++, state, null));
        retained.add(retained.get(0)); // Duplicate index entries must not inflate counts.
        retained.add(null);
        Map<String, Object> stats = ExecutionTaskStatistics.fromStoredExecutionTasks(retained);
        assertEquals(7, stats.get("totalTasks"));
        for (String key : Arrays.asList("pendingTasks", "runningTasks", "listDoneTasks", "completedTasks", "partialFailedTasks", "failedTasks", "stoppedTasks"))
            assertEquals(key, 1, stats.get(key));
        retained.removeIf(task -> task != null && task.getStatus() == SubTaskStatus.TaskStatus.COMPLETED);
        assertEquals(0, ExecutionTaskStatistics.fromStoredExecutionTasks(retained).get("completedTasks"));
        assertEquals(6, ExecutionTaskStatistics.fromStoredExecutionTasks(retained).get("totalTasks"));
    }

    @Test
    public void missingAndEmptySnapshotsRemainDistinct() {
        assertNull(ExecutionTaskStatistics.fromStoredExecutionTasks(null));
        Map<String, Object> empty = ExecutionTaskStatistics.fromStoredExecutionTasks(Collections.emptyList());
        assertEquals(0, empty.get("totalTasks"));
        assertEquals(0, empty.get("completedTasks"));
        assertEquals(1, ExecutionTaskStatistics.fromStoredExecutionTasks(Collections.singletonList(task(1, null, null))).get("unknownTasks"));
    }

    private SubTaskStatus task(int id, SubTaskStatus.TaskStatus state, String shard) {
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(id); task.setStatus(state); task.setShardId(shard);
        return task;
    }
}
