package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.ListExecutionService;
import com.transfer.tools.model.*;
import com.transfer.tools.service.SubTaskManagerService;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class WorkerOssAutoStartTest {
    @Test public void receivedRangesUsePersistedIntentAndNeverRestartRetainedTerminalOrLegacyRecords() {
        WorkerController controller = new WorkerController();
        SubTaskManagerService manager = mock(SubTaskManagerService.class);
        ListExecutionService execution = mock(ListExecutionService.class);
        wire(controller, manager, execution);
        List<Map<String,Object>> payload = new ArrayList<>();
        for (int id = 1; id <= 5; id++) {
            Map<String,Object> item = new HashMap<>(); item.put("taskIndex", id); item.put("direction", "OSS_TO_OSS");
            item.put("autoStart", true); payload.add(item);
            SubTaskStatus retained = new SubTaskStatus(); retained.setTaskIndex(id);
            retained.setAutoStart(id == 2 ? null : true);
            retained.setStatus(id == 3 ? SubTaskStatus.TaskStatus.STOPPED : id == 4
                    ? SubTaskStatus.TaskStatus.COMPLETED : id == 5 ? SubTaskStatus.TaskStatus.RUNNING : SubTaskStatus.TaskStatus.PENDING);
            when(manager.getTaskStatus(id)).thenReturn(retained);
        }
        assertEquals(true, controller.receiveTasks(payload).get("success"));
        verify(execution).startExecution(null, null, Collections.singletonList(1));
        verify(manager).importTasksWithTaskId(anyList());
    }

    @Test public void newJsonImportsPersistIntentBeforeAutomaticQueueing() {
        WorkerController controller = new WorkerController();
        SubTaskManagerService manager = mock(SubTaskManagerService.class);
        ListExecutionService execution = mock(ListExecutionService.class);
        wire(controller, manager, execution);
        TaskImport task = new TaskImport(); task.setDirection(TransferDirection.OSS_TO_OSS);
        task.setSourceBucket("source"); task.setDestBucket("target"); task.setAutoStart(false);
        doAnswer(call -> {
            List<TaskImport> imported = call.getArgument(0);
            assertEquals(Boolean.TRUE, imported.get(0).getAutoStart()); imported.get(0).setTaskIndex(41);
            return null;
        }).when(manager).importTasks(anyList());
        assertEquals(true, controller.importTasks(Collections.singletonList(task)).get("success"));
        org.mockito.InOrder order = inOrder(manager, execution);
        order.verify(manager).importTasks(anyList()); order.verify(execution).startExecution(null, null, Collections.singletonList(41));
    }

    private void wire(WorkerController controller, SubTaskManagerService manager, ListExecutionService execution) {
        DistributedConfig config = mock(DistributedConfig.class); when(config.getLocalIp()).thenReturn("worker");
        ReflectionTestUtils.setField(controller, "distributedConfig", config);
        ReflectionTestUtils.setField(controller, "subTaskManagerService", manager);
        ReflectionTestUtils.setField(controller, "listExecutionService", execution);
        ReflectionTestUtils.setField(controller, "objectMapper", new ObjectMapper());
    }
}
