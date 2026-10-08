package com.transfer.tools.service;

import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.FailedKey;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.storage.RocksDBService;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.mockito.ArgumentCaptor;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.Collections;
import java.util.Map;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class LocalTransferRetryRecoveryTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService database;
    private SubTaskManagerService tasks;
    private LocalTransferService local;
    private LocalTaskControlService control;
    private BufferQueueManager queue;

    @Before public void open() throws Exception {
        database = new RocksDBService();
        ReflectionTestUtils.setField(database, "dbPath", temporary.newFolder("db").getAbsolutePath());
        database.init();
        control = new LocalTaskControlService();
        ReflectionTestUtils.setField(control, "rocksDBService", database);
        FileReportLedgerService ledger = new FileReportLedgerService();
        ReflectionTestUtils.setField(ledger, "rocksDBService", database);
        queue = mock(BufferQueueManager.class);
        tasks = new SubTaskManagerService();
        ReflectionTestUtils.setField(tasks, "rocksDBService", database);
        ReflectionTestUtils.setField(tasks, "localTaskControlService", control);
        ReflectionTestUtils.setField(tasks, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(tasks, "taskProgressCache", mock(TaskProgressCache.class));
        ReflectionTestUtils.setField(tasks, "endpointNormalizationService", mock(EndpointNormalizationService.class));
        ReflectionTestUtils.setField(tasks, "fileReportLedgerService", ledger);
        local = new LocalTransferService();
        ReflectionTestUtils.setField(local, "rocksDBService", database);
        ReflectionTestUtils.setField(local, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(local, "controlService", control);
        ReflectionTestUtils.setField(local, "transferSpeedService", mock(TransferSpeedService.class));
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(-11); task.setParentTaskId(11); task.setShardId("11-local-0");
        task.setDirection(TransferDirection.OSS_TO_LOCAL); task.setStatus(SubTaskStatus.TaskStatus.STOPPED);
        database.putSync("task:-11", task);
        database.putSync("task:id:list", Collections.singletonList(-11));
        FailedKey failure = new FailedKey(); failure.setTaskIndex(-11); failure.setKey("failed-key");
        database.putSync("failed:1", failure); database.putSync("failed:count", 1L);
        control.stop(11); control.stop(-11);
    }

    @After public void close() {
        if (local != null) local.shutdownDiscovery();
        if (tasks != null) tasks.shutdownCompletionWriter();
        if (database != null) database.destroy();
    }

    @Test public void stoppedShardRetryCreatesDurableQueuedAttemptAndDuplicateRetryIsIgnored() {
        Map<String,Object> result = local.retry(Collections.singletonList(11), Collections.singletonList("failed-key"));
        assertEquals(1, result.get("accepted")); assertEquals(0, result.get("failed"));
        assertEquals(SubTaskStatus.TaskStatus.PENDING, tasks.getTaskStatus(-11).getStatus());
        assertEquals(Integer.valueOf(1), tasks.getTaskStatus(-11).getAttempt());
        assertFalse(control.isStopped(11)); assertFalse(control.isStopped(-11));
        assertTrue(tasks.getFailedKeysByTaskIndex(-11).isEmpty());
        ArgumentCaptor<SubTaskStatus> queued = ArgumentCaptor.forClass(SubTaskStatus.class);
        verify(queue).offerListTask(queued.capture());
        assertEquals(Integer.valueOf(-11), queued.getValue().getTaskIndex());
        assertEquals(Integer.valueOf(1), queued.getValue().getAttempt());

        result = local.retry(Collections.singletonList(11));
        assertEquals(0, result.get("accepted")); assertEquals(1, result.get("ignored"));
        assertFalse(control.shouldProcess(-11, "outside-original-selection"));
        verify(queue, times(1)).offerListTask(any());
    }

    @Test public void rejectedQueueDoesNotEraseFailureOrClaimAcceptance() {
        doThrow(new IllegalStateException("queue unavailable")).when(queue).offerListTask(any());
        Map<String,Object> result = local.retry(Collections.singletonList(11));
        assertEquals(0, result.get("accepted")); assertEquals(1, result.get("failed"));
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, tasks.getTaskStatus(-11).getStatus());
        assertTrue(control.isStopped(-11));
        assertEquals(1, tasks.getFailedKeysByTaskIndex(-11).size());
    }
}
