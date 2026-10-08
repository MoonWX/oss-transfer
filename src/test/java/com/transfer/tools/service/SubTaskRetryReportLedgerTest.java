package com.transfer.tools.service;

import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.Collections;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class SubTaskRetryReportLedgerTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService store;
    private FileReportLedgerService ledger;
    private SubTaskManagerService tasks;
    private BufferQueueManager queue;

    @Before public void setup() throws Exception {
        store = new RocksDBService();
        ReflectionTestUtils.setField(store, "dbPath", temporary.newFolder("db").getAbsolutePath()); store.init();
        ledger = new FileReportLedgerService();
        ReflectionTestUtils.setField(ledger, "rocksDBService", store);
        LocalTaskControlService control = new LocalTaskControlService();
        ReflectionTestUtils.setField(control, "rocksDBService", store); control.loadStops();
        tasks = new SubTaskManagerService(); queue = mock(BufferQueueManager.class);
        ReflectionTestUtils.setField(tasks, "fileReportLedgerService", ledger);
        ReflectionTestUtils.setField(tasks, "rocksDBService", store);
        ReflectionTestUtils.setField(tasks, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(tasks, "localTaskControlService", control);
        ReflectionTestUtils.setField(tasks, "taskProgressCache", mock(TaskProgressCache.class));
        ReflectionTestUtils.setField(tasks, "endpointNormalizationService", mock(EndpointNormalizationService.class));
        SubTaskStatus task = new SubTaskStatus(); task.setTaskIndex(1); task.setDirection(TransferDirection.OSS_TO_OSS);
        task.setStatus(SubTaskStatus.TaskStatus.PARTIAL_FAILED); task.setStartKey("a"); task.setEndKey("z");
        task.setPutObjectCount(1L); task.setFailedObjectCount(1L);
        store.putSync("task:1", task); store.putSync("task:id:list", Collections.singletonList(1));
        FailedKey failure = new FailedKey(); failure.setTaskIndex(1); failure.setKey("retry-key"); failure.setObjectSize(7L);
        store.putSync("failed:1", failure); store.putSync("failed:count", 1L);
    }

    @After public void close() {
        if (tasks != null) tasks.shutdownCompletionWriter();
        if (store != null) store.destroy();
    }

    @Test public void failedQueueAcceptanceKeepsExceptionCacheAndDurableFailureForAnotherRetry() throws Exception {
        doThrow(new IllegalStateException("list queue full")).when(queue).offerListTask(any());
        try { tasks.retryTask(1); fail("queue rejection must propagate"); }
        catch (IllegalStateException expected) { assertEquals("list queue full", expected.getMessage()); }
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, tasks.getTaskStatus(1).getStatus());
        assertEquals(1, tasks.getFailedKeysByTaskIndex(1).size());
        FileReportPage page = ledger.page(1, null, 10);
        while (!page.isDone() && page.getItems().isEmpty()) page = ledger.page(1, page.getNextCursor(), 10);
        assertEquals(1, page.getItems().size());
        assertEquals("retry-key", page.getItems().get(0).getLogicalKey());
        assertEquals("FAILED", page.getItems().get(0).getResult());
    }

    @Test public void ledgerWriteFailureLeavesExceptionCacheAndTaskStateUntouched() throws Exception {
        FileReportLedgerService broken = mock(FileReportLedgerService.class);
        doThrow(new IllegalStateException("disk unavailable")).when(broken).failed(any(), anyString(), any(), any(), any(), any());
        ReflectionTestUtils.setField(tasks, "fileReportLedgerService", broken);
        try { tasks.retryTask(1); fail("write must fail before acceptance"); }
        catch (IllegalStateException expected) { assertEquals("disk unavailable", expected.getMessage()); }
        assertEquals(1, tasks.getFailedKeysByTaskIndex(1).size());
        assertEquals(SubTaskStatus.TaskStatus.PARTIAL_FAILED, tasks.getTaskStatus(1).getStatus());
        verify(queue, never()).offerListTask(any());
    }

    @Test public void retryUsesDurableReplayAndKeepsCompletedFileCheckpoints() throws Exception {
        ObjectListMessage completed = new ObjectListMessage(null, 1, false, "source", "destination", null, null, true, "done");
        completed.setSourceSize(8L); ledger.finished(completed, "SUCCESS", "TRANSFERRED", null);
        tasks.retryTask(1);
        assertEquals(SubTaskStatus.TaskStatus.PENDING, tasks.getTaskStatus(1).getStatus());
        assertEquals(Integer.valueOf(1), tasks.getTaskStatus(1).getAttempt());
        assertEquals("a", tasks.getTaskStatus(1).getStartKey());
        assertEquals("z", tasks.getTaskStatus(1).getEndKey());
        assertEquals("SUCCESS", ledger.completedResult(1, "done"));
        assertTrue(tasks.getFailedKeysByTaskIndex(1).isEmpty());
        verify(queue, times(1)).offerListTask(any());
        verify(queue, never()).putPutMessage(any());
        try { tasks.retryTask(1); fail("queued retry must not be accepted again"); }
        catch (IllegalStateException expected) { assertTrue(expected.getMessage().contains("PENDING")); }
        verify(queue, times(1)).offerListTask(any());
    }
}
