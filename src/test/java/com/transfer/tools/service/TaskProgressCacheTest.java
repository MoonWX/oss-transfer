package com.transfer.tools.service;

import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.storage.RocksDBService;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import static org.junit.Assert.*;
import static org.mockito.Mockito.mock;

public class TaskProgressCacheTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService db;
    private TaskProgressCache progress;
    private SubTaskManagerService tasks;

    @Before public void open() throws Exception {
        db = new RocksDBService();
        ReflectionTestUtils.setField(db, "dbPath", temporary.newFolder("db").getAbsolutePath());
        db.init();
        tasks = mock(SubTaskManagerService.class);
        progress = new TaskProgressCache();
        ReflectionTestUtils.setField(progress, "rocksDBService", db);
        ReflectionTestUtils.setField(progress, "subTaskManagerService", tasks);
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(9); task.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        db.putSync("task:9", task);
    }

    @After public void close() { db.destroy(); }

    @Test public void emptyScanCompletesWithoutAnyPutResult() {
        SubTaskStatus created = task(); created.setListObjectCount(0L); created.setPutObjectCount(0L);
        db.putSync("task:9", created);
        progress.updateTaskStatus(9, SubTaskStatus.TaskStatus.LIST_DONE);
        flush();
        assertEquals(SubTaskStatus.TaskStatus.COMPLETED, task().getStatus());
        assertEquals(Boolean.TRUE, task().getScanComplete());
        assertEquals(Long.valueOf(0), task().getListObjectCount());
        assertEquals(Long.valueOf(0), task().getPutObjectCount());
    }

    @Test public void inventoryBytesArrivingAfterTheScanFlushAreNotDropped() {
        // Reproduce the QA ordering: all keys are discovered, but the last metadata
        // reads and completions arrive in a later flush with no new scanned keys.
        progress.addToProgress(9, 254, 0, 0, 0, 0, 0, null);
        progress.addToProgress(9, 0, 16056443, 218, 16056443, 0, 0, "early");
        progress.updateTaskStatus(9, SubTaskStatus.TaskStatus.LIST_DONE);
        flush();
        assertEquals(Long.valueOf(16056443), task().getListObjectSize());
        assertEquals(SubTaskStatus.TaskStatus.LIST_DONE, task().getStatus());

        progress.addToProgress(9, 0, 2490491, 36, 2490491, 0, 0, "late");
        flush();
        assertEquals(Long.valueOf(254), task().getListObjectCount());
        assertEquals(Long.valueOf(254), task().getPutObjectCount());
        assertEquals(Long.valueOf(18546934), task().getListObjectSize());
        assertEquals(task().getPutObjectSize(), task().getListObjectSize());
        assertEquals(SubTaskStatus.TaskStatus.COMPLETED, task().getStatus());
        flush();
        assertEquals(Long.valueOf(18546934), task().getListObjectSize());
    }

    @Test public void bytesOnlyIncrementIsNotEmptyAndIsFlushedOnce() {
        progress.addToProgress(9, 1, 0, 0, 0, 0, 0, null);
        flush();
        progress.addToProgress(9, 0, 17, 0, 0, 0, 0, null);
        flush();
        assertEquals(Long.valueOf(1), task().getListObjectCount());
        assertEquals(Long.valueOf(17), task().getListObjectSize());
        assertEquals(SubTaskStatus.TaskStatus.RUNNING, task().getStatus());
        flush();
        assertEquals(Long.valueOf(17), task().getListObjectSize());
    }

    @Test public void separatelyResolvedSuccessFailureAndSkipBytesRemainReconciled() {
        progress.addToProgress(9, 3, 0, 0, 0, 0, 0, null);
        progress.updateTaskStatus(9, SubTaskStatus.TaskStatus.LIST_DONE);
        flush();
        progress.addToProgress(9, 0, 5, 1, 5, 0, 0, "success");
        flush();
        progress.addToProgress(9, 0, 7, 0, 0, 1, 7, "failed");
        flush();
        progress.addToProgress(9, 0, 11, 0, 0, 0, 0, 1, 11, "skipped");
        flush();
        SubTaskStatus task = task();
        assertEquals(Long.valueOf(3), task.getListObjectCount());
        assertEquals(Long.valueOf(23), task.getListObjectSize());
        assertEquals(Long.valueOf(1), task.getPutObjectCount());
        assertEquals(Long.valueOf(1), task.getFailedObjectCount());
        assertEquals(Long.valueOf(1), task.getSkippedObjectCount());
        assertEquals(23L, task.getPutObjectSize() + task.getFailedObjectSize() + task.getSkippedObjectSize());
        assertEquals(SubTaskStatus.TaskStatus.PARTIAL_FAILED, task.getStatus());
    }

    private SubTaskStatus task() { return db.get("task:9", SubTaskStatus.class); }
    private void flush() {
        synchronized (tasks) { ReflectionTestUtils.invokeMethod(progress, "flushToRocksDB"); }
    }
}
