package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.executor.ExploreExecutorService;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class OssAutoStartRecoveryTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService db;
    private String path;
    private TaskCreationService creations;

    @Before public void setup() throws Exception { path = temporary.newFolder().getAbsolutePath(); open(); }
    private void open() {
        db = new RocksDBService(); ReflectionTestUtils.setField(db, "dbPath", path); db.init();
        creations = new TaskCreationService(); ReflectionTestUtils.setField(creations, "rocksDBService", db);
    }
    @After public void close() { db.destroy(); }
    private BucketPair request() {
        BucketPair request = new BucketPair(); request.setSourceBucket("source"); request.setDestBucket("target");
        request.setDirection(TransferDirection.OSS_TO_OSS); return request;
    }
    private String creation(boolean automatic, int id) {
        BucketPair request = request(); String key = creations.create(request, automatic);
        creations.enterScope(key, request); creations.recordTaskIds(Collections.singletonList(id)); creations.clearScope();
        return key;
    }

    @Test public void reopenQueuesOnlyExplicitAutomaticUnfinishedScans() {
        String automatic = creation(true, 11), legacy = creation(false, 12), stopped = creation(true, 13);
        String completed = creation(true, 14), failed = creation(true, 15), deleted = creation(true, 16);
        creations.stopByTaskIds(Collections.singletonList(13)); creations.complete(completed);
        creations.fail(failed, "access denied"); creations.markDeletedByTaskIds(Collections.singletonList(16));
        db.destroy(); open(); creations.recoverInterrupted();
        List<BucketPair> pending = creations.recoverableRequests();
        assertEquals(1, pending.size()); assertEquals(automatic, pending.get(0).getCreationId());
        assertEquals(Boolean.TRUE, creations.get(automatic).getAutoStart());
        assertEquals("QUEUED", creations.get(automatic).getStatus());
        assertEquals("FAILED", creations.get(legacy).getStatus());
        assertEquals("STOPPED", creations.get(stopped).getStatus());
        assertEquals("COMPLETED", creations.get(completed).getStatus());
        assertEquals("FAILED", creations.get(failed).getStatus()); assertTrue(creations.get(deleted).isDeleted());
        DistributedConfig config = mock(DistributedConfig.class); when(config.isMaster()).thenReturn(true);
        BufferQueueManager queues = mock(BufferQueueManager.class); when(queues.offerExploreTask(any())).thenReturn(true);
        ExploreExecutorService executor = new ExploreExecutorService();
        ReflectionTestUtils.setField(executor, "distributedConfig", config);
        ReflectionTestUtils.setField(executor, "taskCreationService", creations);
        ReflectionTestUtils.setField(executor, "bufferQueueManager", queues);
        executor.recoverPendingCreations(); verify(queues).offerExploreTask(pending.get(0));
    }

    @Test public void processShutdownPreservesAutomaticScanForNextStartupButLiveFailureDoesNot() {
        String automatic = creations.create(request(), true);
        BucketPair request = creations.get(automatic).getRequest();
        BucketExploreService explorer = mock(BucketExploreService.class);
        doThrow(new IllegalStateException("socket closed")).when(explorer).exploreSingleBucketAndCreateSubTasks(any());
        ExploreExecutorService executor = new ExploreExecutorService();
        ReflectionTestUtils.setField(executor, "taskCreationService", creations);
        ReflectionTestUtils.setField(executor, "endpointNormalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(executor, "bucketExploreService", explorer);
        executor.destroy(); ReflectionTestUtils.invokeMethod(executor, "executeExploreTask", request);
        assertEquals("RUNNING", creations.get(automatic).getStatus());
        db.destroy(); open(); creations.recoverInterrupted();
        assertEquals(automatic, creations.recoverableRequests().get(0).getCreationId());
        ReflectionTestUtils.setField(executor, "taskCreationService", creations);
        ReflectionTestUtils.setField(executor, "shuttingDown", false);
        ReflectionTestUtils.invokeMethod(executor, "executeExploreTask", creations.get(automatic).getRequest());
        assertEquals("FAILED", creations.get(automatic).getStatus()); assertTrue(creations.recoverableRequests().isEmpty());
    }

    @Test public void workerRestartRecoversNewQueuedIntentButNeverLegacyOrStoppedTasks() {
        SubTaskManagerService manager = mock(SubTaskManagerService.class);
        LocalTaskControlService control = mock(LocalTaskControlService.class);
        List<Integer> ids = Arrays.asList(11, 12, 13, 14, 15);
        when(manager.getTaskIdList()).thenReturn(ids);
        for (int id : ids) {
            SubTaskStatus task = new SubTaskStatus(); task.setTaskIndex(id); task.setDirection(TransferDirection.OSS_TO_OSS);
            task.setStatus(id == 13 ? SubTaskStatus.TaskStatus.STOPPED : id == 15
                    ? SubTaskStatus.TaskStatus.COMPLETED : SubTaskStatus.TaskStatus.PENDING);
            if (id != 12) task.setAutoStart(true);
            when(manager.getTaskStatus(id)).thenReturn(task);
        }
        when(control.isStopped(14)).thenReturn(true);
        LocalTransferService service = new LocalTransferService();
        ReflectionTestUtils.setField(service, "rocksDBService", db);
        ReflectionTestUtils.setField(service, "subTaskManagerService", manager);
        ReflectionTestUtils.setField(service, "controlService", control);
        com.transfer.tools.executor.ListExecutionService execution = mock(com.transfer.tools.executor.ListExecutionService.class);
        ReflectionTestUtils.setField(service, "listExecutionService", execution);
        try {
            service.recoverDiscoveries();
            SubTaskStatus queued = manager.getTaskStatus(11);
            verify(manager).recoverAutomaticTask(queued);
            verify(execution).startExecution(null, null, Collections.singletonList(11));
            for (int id : Arrays.asList(12, 13, 14, 15)) {
                SubTaskStatus notQueued = manager.getTaskStatus(id);
                verify(manager, never()).recoverTask(notQueued);
                verify(manager, never()).recoverAutomaticTask(notQueued);
            }
            verify(manager).stopTask(14);
        } finally { service.shutdownDiscovery(); }
    }

    @Test public void assignedIntentSurvivesDatabaseReopenAndDuplicateDispatchPreservesStoppedRecord() {
        SubTaskManagerService manager = new SubTaskManagerService();
        LocalTaskControlService control = new LocalTaskControlService(); ReflectionTestUtils.setField(control, "rocksDBService", db);
        FileReportLedgerService ledger = new FileReportLedgerService(); ReflectionTestUtils.setField(ledger, "rocksDBService", db);
        ReflectionTestUtils.setField(manager, "rocksDBService", db);
        ReflectionTestUtils.setField(manager, "localTaskControlService", control);
        ReflectionTestUtils.setField(manager, "fileReportLedgerService", ledger);
        ReflectionTestUtils.setField(manager, "endpointNormalizationService", new EndpointNormalizationService());
        BufferQueueManager queues = mock(BufferQueueManager.class);
        doThrow(new IllegalStateException("full")).when(queues).offerListTask(any());
        ReflectionTestUtils.setField(manager, "bufferQueueManager", queues);
        ReflectionTestUtils.setField(manager, "taskProgressCache", mock(TaskProgressCache.class));
        TaskImport automatic = new TaskImport(); automatic.setTaskIndex(71); automatic.setSourceBucket("source");
        automatic.setDestBucket("target"); automatic.setDirection(TransferDirection.OSS_TO_OSS);
        automatic.setShardId("oss-creation-0"); automatic.setAutoStart(true);
        try {
            manager.importTasksWithTaskId(Collections.singletonList(automatic));
            assertEquals(Boolean.TRUE, manager.getTaskStatus(71).getAutoStart());
            assertEquals(SubTaskStatus.TaskStatus.PENDING, manager.getTaskStatus(71).getStatus());
            db.destroy(); open();
            ReflectionTestUtils.setField(manager, "rocksDBService", db);
            ReflectionTestUtils.setField(control, "rocksDBService", db);
            ReflectionTestUtils.setField(ledger, "rocksDBService", db);
            SubTaskStatus retained = manager.getTaskStatus(71); assertEquals(Boolean.TRUE, retained.getAutoStart());
            manager.recoverAutomaticTask(retained);
            assertEquals(SubTaskStatus.TaskStatus.PENDING, manager.getTaskStatus(71).getStatus());
            assertEquals(Integer.valueOf(1), manager.getTaskStatus(71).getAttempt());
            assertFalse(control.isStopped(71)); verify(queues, never()).offerListTask(any());
            retained = manager.getTaskStatus(71);
            retained.setStatus(SubTaskStatus.TaskStatus.STOPPED); db.putSync("task:71", retained);
            manager.importTasksWithTaskId(Collections.singletonList(automatic));
            assertEquals(SubTaskStatus.TaskStatus.STOPPED, manager.getTaskStatus(71).getStatus());
            retained.setAutoStart(null); retained.setStatus(SubTaskStatus.TaskStatus.PENDING); db.putSync("task:71", retained);
            manager.importTasksWithTaskId(Collections.singletonList(automatic));
            assertNull(manager.getTaskStatus(71).getAutoStart());
            TaskImport imported = new TaskImport(); imported.setTaskIndex(72); imported.setSourceBucket("source");
            imported.setDestBucket("target"); imported.setDirection(TransferDirection.OSS_TO_OSS); imported.setAutoStart(true);
            manager.importTasksWithTaskId(Collections.singletonList(imported));
            SubTaskStatus completed = manager.getTaskStatus(72); completed.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
            db.putSync("task:72", completed);
            manager.importTasksWithTaskId(Collections.singletonList(imported));
            assertEquals(SubTaskStatus.TaskStatus.COMPLETED, manager.getTaskStatus(72).getStatus());
            imported.setDestBucket("other-target"); imported.setDestinationEndpoint(null);
            try {
                manager.importTasksWithTaskId(Collections.singletonList(imported)); fail("existing ID must not be reassigned");
            } catch (TransferValidationException expected) { assertEquals("TASK_ASSIGNMENT_MISMATCH", expected.getCode()); }
        } finally { manager.shutdownCompletionWriter(); }
    }
}
