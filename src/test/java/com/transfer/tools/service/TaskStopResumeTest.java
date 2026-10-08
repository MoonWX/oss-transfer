package com.transfer.tools.service;

import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import java.io.ByteArrayInputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.Collections;
import java.util.concurrent.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class TaskStopResumeTest {
    @Rule public TemporaryFolder temp = new TemporaryFolder();
    private RocksDBService db;
    private LocalTaskControlService control;
    private SubTaskManagerService tasks;
    private TaskProgressCache progress;
    private FileReportLedgerService ledger;
    private BufferQueueManager queue;

    @Before public void setup() throws Exception {
        db = new RocksDBService();
        ReflectionTestUtils.setField(db, "dbPath", temp.newFolder("db").getAbsolutePath()); db.init();
        control = controls();
        tasks = new SubTaskManagerService(); progress = new TaskProgressCache(); ledger = new FileReportLedgerService();
        queue = mock(BufferQueueManager.class);
        ReflectionTestUtils.setField(ledger, "rocksDBService", db);
        ReflectionTestUtils.setField(tasks, "rocksDBService", db);
        ReflectionTestUtils.setField(tasks, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(tasks, "localTaskControlService", control);
        ReflectionTestUtils.setField(tasks, "taskProgressCache", progress);
        ReflectionTestUtils.setField(tasks, "endpointNormalizationService", mock(EndpointNormalizationService.class));
        ReflectionTestUtils.setField(tasks, "fileReportLedgerService", ledger);
        ReflectionTestUtils.setField(progress, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(progress, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(progress, "rocksDBService", db);
    }
    @After public void close() { tasks.shutdownCompletionWriter(); db.destroy(); Thread.interrupted(); }
    private LocalTaskControlService controls() {
        LocalTaskControlService c = new LocalTaskControlService(); ReflectionTestUtils.setField(c, "rocksDBService", db);
        c.loadStops(); return c;
    }
    private SubTaskStatus task(int id) {
        SubTaskStatus t = new SubTaskStatus(); t.setTaskIndex(id); t.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        t.setDirection(TransferDirection.OSS_TO_LOCAL); t.setListObjectCount(5L); t.setPutObjectCount(2L);
        db.putSync("task:" + id, t); db.putSync("task:id:list", Collections.singletonList(id)); return t;
    }
    @Test public void stopPersistsAcrossRestartAndResumeKeepsDurableCompletedFiles() {
        SubTaskStatus t = task(-101);
        ObjectListMessage message = new ObjectListMessage(null, -101, false, "source", null, null, null, true, "done");
        message.setDirection(TransferDirection.OSS_TO_LOCAL); message.setSourceSize(20L);
        ledger.finished(message, "SUCCESS", "TRANSFERRED", null);
        assertTrue(tasks.stopTask(-101));
        LocalTaskControlService afterRestart = controls(); assertTrue(afterRestart.isStopped(-101));
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, tasks.getTaskStatus(-101).getStatus());
        ReflectionTestUtils.setField(tasks, "localTaskControlService", afterRestart);
        assertTrue(tasks.resumeTask(-101));
        assertFalse(controls().isStopped(-101));
        assertEquals("SUCCESS", ledger.completedResult(-101, "done"));
        assertEquals(SubTaskStatus.TaskStatus.PENDING, tasks.getTaskStatus(-101).getStatus());
        assertEquals(Long.valueOf(0), tasks.getTaskStatus(-101).getPutObjectCount());
        verify(queue).offerListTask(any(SubTaskStatus.class));
    }
    @Test public void activeIoMustExitBeforeResumeAndCancellationDoesNotRetireWorker() throws Exception {
        task(4);
        ExecutorService worker = Executors.newSingleThreadExecutor();
        CountDownLatch started = new CountDownLatch(1), release = new CountDownLatch(1);
        Future<Boolean> work = worker.submit(() -> {
            try (LocalTaskControlService.Operation operation = control.begin(4);
                 AutoCloseable action = control.onCancel(4, release::countDown)) {
                started.countDown();
                try { release.await(); } catch (InterruptedException expected) { }
                assertTrue(control.isStopped(4));
                try { control.resume(4); fail("still active"); } catch (IllegalStateException expected) { }
            }
            return Thread.currentThread().isInterrupted();
        });
        assertTrue(started.await(3, TimeUnit.SECONDS)); control.stop(4);
        assertFalse(work.get(3, TimeUnit.SECONDS));
        assertFalse(control.isActive(4)); control.resume(4);
        assertEquals("usable", worker.submit(() -> "usable").get(3, TimeUnit.SECONDS)); worker.shutdownNow();
    }
    @Test public void recoveryRequeuesListDoneButLeavesStoppedTasksAlone() {
        SubTaskStatus t = task(5); t.setStatus(SubTaskStatus.TaskStatus.LIST_DONE); db.putSync("task:5", t);
        tasks.recoverTask(t); verify(queue).offerListTask(any(SubTaskStatus.class));
        assertFalse(Boolean.TRUE.equals(tasks.getTaskStatus(5).getScanComplete()));
        reset(queue); tasks.stopTask(5); tasks.recoverTask(tasks.getTaskStatus(5));
        verify(queue, never()).offerListTask(any(SubTaskStatus.class));
    }
    @Test public void startupAndRepeatedStopPreserveTheOriginalEndTimeAndCounters() {
        for (TransferDirection direction : TransferDirection.values()) {
            int id = 70 + direction.ordinal();
            SubTaskStatus saved = task(id); saved.setDirection(direction); saved.setAutoStart(true);
            saved.setStatus(SubTaskStatus.TaskStatus.STOPPED); saved.setStartTime(1000L); saved.setEndTime(2000L);
            db.putSync("task:" + id, saved); control.stop(id);
            db.destroy(); db.init();
            control = controls(); ReflectionTestUtils.setField(tasks, "localTaskControlService", control);
            LocalTransferService service = new LocalTransferService();
            ReflectionTestUtils.setField(service, "rocksDBService", db);
            ReflectionTestUtils.setField(service, "subTaskManagerService", tasks);
            ReflectionTestUtils.setField(service, "controlService", control);
            try { service.recoverDiscoveries(); } finally { service.shutdownDiscovery(); }
            assertFalse(tasks.stopTask(id));
            SubTaskStatus after = tasks.getTaskStatus(id);
            assertEquals(SubTaskStatus.TaskStatus.STOPPED, after.getStatus());
            assertEquals(Long.valueOf(1000), after.getStartTime()); assertEquals(Long.valueOf(2000), after.getEndTime());
            assertEquals(Long.valueOf(5), after.getListObjectCount()); assertEquals(Long.valueOf(2), after.getPutObjectCount());
            assertTrue(control.isStopped(id));
        }
        verify(queue, never()).offerListTask(any(SubTaskStatus.class));
    }
    @Test public void legacyStoppedTaskWithoutEndTimeReceivesOneWithoutResettingProgress() {
        SubTaskStatus saved = task(80); saved.setStatus(SubTaskStatus.TaskStatus.STOPPED); saved.setStartTime(1000L);
        db.putSync("task:80", saved);
        long before = System.currentTimeMillis(); assertFalse(tasks.stopTask(80));
        SubTaskStatus after = tasks.getTaskStatus(80);
        assertNotNull(after.getEndTime()); assertTrue(after.getEndTime() >= before);
        assertEquals(Long.valueOf(1000), after.getStartTime()); assertEquals(Long.valueOf(2), after.getPutObjectCount());
        assertTrue(control.isStopped(80));
    }
    @Test public void progressCannotOverwriteStoppedStatusOrApplyBeforeReset() {
        task(6); progress.addToProgress(6, 1, 10, 1, 10, 0, 0, "a");
        tasks.stopTask(6); progress.updateTaskStatus(6, SubTaskStatus.TaskStatus.LIST_DONE);
        ReflectionTestUtils.invokeMethod(progress, "flushToRocksDB");
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, tasks.getTaskStatus(6).getStatus());
        progress.addToProgress(6, 1, 10, 1, 10, 0, 0, "old"); tasks.resumeTask(6);
        progress.addToProgress(6, 1, 10, 0, 0, 0, 0, null);
        ReflectionTestUtils.invokeMethod(progress, "flushToRocksDB");
        assertEquals(Long.valueOf(1), tasks.getTaskStatus(6).getListObjectCount());
        assertEquals(Long.valueOf(0), tasks.getTaskStatus(6).getPutObjectCount());
    }
    @Test public void cancellationBeforeAtomicRenamePreservesExistingDestination() throws Exception {
        LocalFileService files = new LocalFileService(); ReflectionTestUtils.setField(files, "localTaskControlService", control);
        configureTemporaryFiles(files);
        Path root = temp.newFolder("files").toPath(); Files.write(root.resolve("file"), new byte[]{9});
        ByteArrayInputStream input = new ByteArrayInputStream(new byte[]{1, 2, 3}) {
            @Override public synchronized int read(byte[] bytes, int off, int len) {
                int count = super.read(bytes, off, len); if (count == -1) control.stop(7); return count;
            }
        };
        try { files.writeAtomically(input, root.toString(), "file", 7, ConflictPolicy.OVERWRITE); fail(); }
        catch (TaskCancelledException expected) { }
        assertArrayEquals(new byte[]{9}, Files.readAllBytes(root.resolve("file")));
        try (java.util.stream.Stream<Path> paths = Files.list(root)) { assertEquals(1L, paths.count()); }
    }
    @Test public void truncatedDownloadCannotReplaceDestination() throws Exception {
        LocalFileService files = new LocalFileService();
        configureTemporaryFiles(files);
        Path root = temp.newFolder("truncated").toPath(); Files.write(root.resolve("file"), new byte[]{9});
        try { files.writeAtomically(new ByteArrayInputStream(new byte[]{1, 2}), root.toString(), "file", 9, ConflictPolicy.OVERWRITE, 100); fail(); }
        catch (java.io.IOException expected) { assertTrue(expected.getMessage().contains("length mismatch")); }
        assertArrayEquals(new byte[]{9}, Files.readAllBytes(root.resolve("file")));
        try (java.util.stream.Stream<Path> paths = Files.list(root)) { assertEquals(1L, paths.count()); }
    }
    private void configureTemporaryFiles(LocalFileService files) {
        LocalTemporaryFileRegistry registry = new LocalTemporaryFileRegistry();
        ReflectionTestUtils.setField(registry, "rocksDBService", db);
        registry.recoverInterrupted();
        ReflectionTestUtils.setField(files, "temporaryFileRegistry", registry);
    }
    @Test public void pendingResumeIsIdempotentAndFullQueueLeavesRecoverableStoppedTask() {
        SubTaskStatus t = task(10); t.setStatus(SubTaskStatus.TaskStatus.PENDING); db.putSync("task:10", t);
        assertFalse(tasks.resumeTask(10)); verify(queue, never()).offerListTask(any(SubTaskStatus.class));
        tasks.stopTask(10);
        doThrow(new IllegalStateException("full")).when(queue).offerListTask(any(SubTaskStatus.class));
        try { tasks.resumeTask(10); fail(); } catch (IllegalStateException expected) { }
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, tasks.getTaskStatus(10).getStatus());
        assertTrue(control.isStopped(10));
    }

    @Test public void lateShardReceivedAfterParentStopIsDurableButCannotRun() {
        control.stop(100);
        TaskImport incoming = new TaskImport(); incoming.setTaskIndex(-222); incoming.setParentTaskId(100);
        incoming.setShardId("100-range-0"); incoming.setDirection(TransferDirection.OSS_TO_LOCAL);
        tasks.importTasksWithTaskId(Collections.singletonList(incoming));
        Integer actual = incoming.getTaskIndex();
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, tasks.getTaskStatus(actual).getStatus());
        assertTrue(tasks.getTaskIdList().contains(actual));
        try { control.begin(actual, 100); fail("parent stop must fence late worker messages"); }
        catch (TaskCancelledException expected) { }
        // Simulate a legacy task whose index write was lost; duplicate delivery repairs its index.
        db.putSync("task:id:list", Collections.emptyList());
        tasks.importTasksWithTaskId(Collections.singletonList(incoming));
        assertTrue(tasks.getTaskIdList().contains(actual));
        control.resume(100); assertTrue(tasks.resumeTask(actual));
        assertEquals(SubTaskStatus.TaskStatus.PENDING, tasks.getTaskStatus(actual).getStatus());
    }
    @Test public void stoppingParentCancelsRegisteredChildIoEvenBeforeShardEnumeration() throws Exception {
        java.util.concurrent.atomic.AtomicBoolean aborted = new java.util.concurrent.atomic.AtomicBoolean();
        try (LocalTaskControlService.Operation operation = control.begin(-300, 300);
             AutoCloseable action = control.onCancel(-300, () -> aborted.set(true))) {
            control.stop(300);
            assertTrue(aborted.get()); assertTrue(Thread.currentThread().isInterrupted());
            assertTrue(control.isActive(300));
        }
        assertFalse(Thread.currentThread().isInterrupted());
        assertFalse(control.isActive(300)); assertFalse(control.isActive(-300));
        try { control.begin(-301, 300); fail(); } catch (TaskCancelledException expected) { }
    }

    @Test public void stopMarkerDurableBeforeTaskStatusWriteRemainsResumable() {
        SubTaskStatus pending = task(400); pending.setStatus(SubTaskStatus.TaskStatus.PENDING); db.putSync("task:400", pending);
        control.stop(400); // crash after this durable fence but before task status was updated
        assertTrue(tasks.resumeTask(400));
        assertFalse(control.isStopped(400));
        verify(queue).offerListTask(any(SubTaskStatus.class));
    }

    @Test public void scannerFailureTransitionPreservesFlushedAndPendingCounters() {
        SubTaskStatus t = task(500); t.setParentTaskId(50); db.putSync("task:500", t);
        progress.addToProgress(500, 60, 600, 50, 500, 0, 0, "a");
        ReflectionTestUtils.invokeMethod(progress, "flushToRocksDB");
        tasks.failTask(500);
        assertEquals(Long.valueOf(65), tasks.getTaskStatus(500).getListObjectCount());
        assertEquals(Long.valueOf(52), tasks.getTaskStatus(500).getPutObjectCount());
        assertEquals(SubTaskStatus.TaskStatus.FAILED, tasks.getTaskStatus(500).getStatus());
        progress.addToProgress(500, 3, 30, 2, 20, 0, 0, "b");
        ReflectionTestUtils.invokeMethod(progress, "flushToRocksDB");
        assertEquals(Long.valueOf(68), tasks.getTaskStatus(500).getListObjectCount());
        assertEquals(Long.valueOf(54), tasks.getTaskStatus(500).getPutObjectCount());
    }
    @Test public void parentStopFencesStaleScannerStatusBeforeChildStopArrives() {
        SubTaskStatus stale = task(501); stale.setParentTaskId(51); db.putSync("task:501", stale);
        progress.addToProgress(501, 60, 600, 50, 500, 0, 0, "a");
        ReflectionTestUtils.invokeMethod(progress, "flushToRocksDB");
        control.stop(51);
        stale.setStatus(SubTaskStatus.TaskStatus.FAILED);
        tasks.updateTaskStatus(stale); tasks.failTask(501);
        assertEquals(Long.valueOf(65), tasks.getTaskStatus(501).getListObjectCount());
        assertEquals(Long.valueOf(52), tasks.getTaskStatus(501).getPutObjectCount());
        assertEquals(SubTaskStatus.TaskStatus.RUNNING, tasks.getTaskStatus(501).getStatus());
        tasks.stopTask(501);
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, tasks.getTaskStatus(501).getStatus());
        assertEquals(Long.valueOf(65), tasks.getTaskStatus(501).getListObjectCount());
    }

    @Test public void runningAndListDoneTasksCannotBeDeleted() {
        SubTaskStatus t = task(8);
        for (SubTaskStatus.TaskStatus status : new SubTaskStatus.TaskStatus[]{SubTaskStatus.TaskStatus.RUNNING, SubTaskStatus.TaskStatus.LIST_DONE}) {
            t.setStatus(status); db.putSync("task:8", t);
            try { tasks.clearTasks(null, null, Collections.singletonList(8), false); fail(); }
            catch (IllegalStateException expected) { }
            assertNotNull(tasks.getTaskStatus(8));
        }
    }
}
