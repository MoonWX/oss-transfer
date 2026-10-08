package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.executor.ListExecutionService;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TaskImport;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.service.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class WorkerOssStopFenceTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService db;
    private String path;
    private SubTaskManagerService tasks;
    private LocalTaskControlService control;
    private WorkerController controller;
    private ListExecutionService execution;
    private BufferQueueManager queues;
    private final ObjectMapper mapper = new ObjectMapper();

    @Before public void setup() throws Exception {
        path = temporary.newFolder().getAbsolutePath();
        tasks = new SubTaskManagerService(); controller = new WorkerController();
        execution = mock(ListExecutionService.class); queues = mock(BufferQueueManager.class);
        ReflectionTestUtils.setField(tasks, "endpointNormalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(tasks, "fileReportLedgerService", mock(FileReportLedgerService.class));
        ReflectionTestUtils.setField(tasks, "bufferQueueManager", queues);
        ReflectionTestUtils.setField(tasks, "taskProgressCache", mock(TaskProgressCache.class));
        ReflectionTestUtils.setField(controller, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(controller, "listExecutionService", execution);
        ReflectionTestUtils.setField(controller, "objectMapper", mapper);
        ReflectionTestUtils.setField(controller, "distributedConfig", mock(DistributedConfig.class));
        open();
    }

    private void open() {
        db = new RocksDBService(); ReflectionTestUtils.setField(db, "dbPath", path); db.init();
        control = new LocalTaskControlService(); ReflectionTestUtils.setField(control, "rocksDBService", db); control.loadStops();
        ReflectionTestUtils.setField(tasks, "rocksDBService", db);
        ReflectionTestUtils.setField(tasks, "localTaskControlService", control);
        ReflectionTestUtils.setField(controller, "localTaskControlService", control);
    }

    @After public void close() { tasks.shutdownCompletionWriter(); db.destroy(); }
    private Map<String,Object> ids() { return Collections.singletonMap("taskIds", Collections.singletonList(71)); }
    private List<Map<String,Object>> payload() {
        TaskImport task = new TaskImport(); task.setTaskIndex(71); task.setDirection(TransferDirection.OSS_TO_OSS);
        task.setSourceBucket("source"); task.setDestBucket("target"); task.setAutoStart(true);
        task.setShardId("oss-creation-0");
        return Collections.singletonList(mapper.convertValue(task, Map.class));
    }

    @Test public void stopBeforeLateReceiveSurvivesRestartAndDoesNotExecuteUntilResumed() {
        Map<String,Object> stopped = controller.stopTasks(ids());
        assertEquals(true, stopped.get("success")); assertEquals(1, stopped.get("accepted"));
        assertNull(tasks.getTaskStatus(71)); assertTrue(control.isStopped(71));
        db.destroy(); open();
        assertTrue(control.isStopped(71));
        assertEquals(true, controller.receiveTasks(payload()).get("success"));
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, tasks.getTaskStatus(71).getStatus());
        verify(execution, never()).startExecution(any(), any(), anyList());
        verify(queues, never()).offerListTask(any());
        assertEquals(true, controller.resumeTasks(ids()).get("success"));
        assertFalse(control.isStopped(71));
        assertEquals(SubTaskStatus.TaskStatus.PENDING, tasks.getTaskStatus(71).getStatus());
        verify(queues).offerListTask(any(SubTaskStatus.class));
    }

    @Test public void resumeBeforeLateReceiveClearsFenceAndAllowsAutomaticReplay() {
        controller.stopTasks(ids());
        assertEquals(1, controller.resumeTasks(ids()).get("accepted"));
        assertFalse(control.isStopped(71)); assertNull(tasks.getTaskStatus(71));
        assertEquals(true, controller.receiveTasks(payload()).get("success"));
        assertEquals(SubTaskStatus.TaskStatus.PENDING, tasks.getTaskStatus(71).getStatus());
        verify(execution).startExecution(null, null, Collections.singletonList(71));
    }

    @Test public void repeatedStopIsIdempotentAndOnlyFencesRequestedIds() {
        assertEquals(1, controller.stopTasks(ids()).get("accepted"));
        Map<String,Object> second = controller.stopTasks(ids());
        assertEquals(true, second.get("success")); assertEquals(0, second.get("accepted"));
        assertEquals(1, second.get("ignored")); assertFalse(control.isStopped(72));
    }
}
