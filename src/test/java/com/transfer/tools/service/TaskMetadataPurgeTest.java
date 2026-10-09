package com.transfer.tools.service;

import com.transfer.tools.config.LocalTransferConfig;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.executor.FailedKeyConsumerService;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class TaskMetadataPurgeTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService db;
    private SubTaskManagerService tasks;
    private LocalTaskControlService control;
    private FileReportLedgerService ledger;

    @Before public void open() throws Exception {
        db = new RocksDBService();
        ReflectionTestUtils.setField(db, "dbPath", temporary.newFolder("db").getAbsolutePath());
        db.init();
        tasks = new SubTaskManagerService();
        ledger = new FileReportLedgerService();
        control = new LocalTaskControlService();
        ReflectionTestUtils.setField(ledger, "rocksDBService", db);
        ReflectionTestUtils.setField(control, "rocksDBService", db);
        ReflectionTestUtils.setField(tasks, "rocksDBService", db);
        ReflectionTestUtils.setField(tasks, "fileReportLedgerService", ledger);
        ReflectionTestUtils.setField(tasks, "localTaskControlService", control);
        ReflectionTestUtils.setField(tasks, "endpointNormalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(tasks, "bufferQueueManager", mock(BufferQueueManager.class));
        ReflectionTestUtils.setField(tasks, "taskProgressCache", mock(TaskProgressCache.class));
    }
    @After public void close() { tasks.shutdownCompletionWriter(); db.destroy(); }

    @Test public void deletedIdentitiesAreEvictedAndReusedIdsRefreshCachedChildMembership() {
        WorkerTaskChildrenService children = new WorkerTaskChildrenService();
        ReflectionTestUtils.setField(children, "tasks", tasks);
        tasks.saveAssignedTask(-101, assigned(7, "7-local-0"));
        long original = tasks.taskIdentityVersion(-101);
        assertTrue(original > 0);
        assertEquals(1, ((List<?>) children.query(7, 0, 25, null).get("items")).size());
        SubTaskStatus first = tasks.getTaskStatus(-101);
        first.setStatus(SubTaskStatus.TaskStatus.STOPPED);
        tasks.updateTaskStatus(first);
        assertEquals(1, tasks.clearTasks(null, null, Collections.singletonList(-101), false));
        assertEquals(0L, tasks.taskIdentityVersion(-101));
        assertTrue(identityVersions().isEmpty());

        // Reuse happens between child queries, so the cache cannot rely on observing an empty index.
        tasks.saveAssignedTask(-101, assigned(8, "8-local-3"));
        assertTrue(tasks.taskIdentityVersion(-101) > original);
        assertTrue(((List<?>) children.query(7, 0, 25, null).get("items")).isEmpty());
        List<?> page = (List<?>) children.query(8, 0, 25, null).get("items");
        assertEquals(1, page.size());
        assertEquals(4, ((Map<?, ?>) page.get(0)).get("subtaskNumber"));
        assertEquals(1, tasks.clearTasks(null, null, Collections.singletonList(-101), true));
        assertEquals(0L, tasks.taskIdentityVersion(-101));
        assertTrue(identityVersions().isEmpty());
        assertTrue(children.query(8, 0, 25, null).get("items") instanceof List);
        assertTrue(((List<?>) children.query(8, 0, 25, null).get("items")).isEmpty());
    }

    @Test public void removingActiveIndexEvictsOnlyItsIdentityAndReindexingGetsANewerVersion() {
        tasks.saveAssignedTask(-102, assigned(9, "9-local-0"));
        tasks.saveAssignedTask(-103, assigned(9, "9-local-1"));
        long original = tasks.taskIdentityVersion(-102), retained = tasks.taskIdentityVersion(-103);
        tasks.removeFromTaskIdList(-102);
        assertEquals(0L, tasks.taskIdentityVersion(-102));
        assertEquals(Collections.singleton(-103), identityVersions().keySet());
        tasks.ensureTaskIndexed(-102);
        assertTrue(tasks.taskIdentityVersion(-102) > original);
        assertEquals(retained, tasks.taskIdentityVersion(-103));
    }

    @SuppressWarnings("unchecked") private Map<Integer, Long> identityVersions() {
        return (Map<Integer, Long>) ReflectionTestUtils.getField(tasks, "taskIdentityVersions");
    }
    private ListSubTask assigned(int parent, String shard) {
        ListSubTask task = new ListSubTask();
        task.setParentTaskId(parent); task.setShardId(shard); task.setDirection(TransferDirection.OSS_TO_LOCAL);
        task.setSourceBucket("source"); task.setDestBucket("destination");
        return task;
    }

    @Test public void removesOnlySelectedMetadataIncludingLargePrefixesAndLeavesRealFilesUntouched() throws Exception {
        SubTaskStatus one = task(1, null), ten = task(10, null);
        one.setInventoryManifestId("manifest-1");
        ten.setInventoryManifestId("manifest-10");
        deleted(one, ten);
        db.put("failed:count", 8L);
        db.put("failed:1", failed(1, null));
        db.put("failed:8", failed(10, null));
        Path actualFile = temporary.newFile("fallback.bin").toPath();
        Files.write(actualFile, new byte[]{1, 2, 3});
        FallbackObject fallback = new FallbackObject();
        fallback.setTaskIndex(1); fallback.setTmpFilePath(actualFile.toString());
        db.put("fallback:count", 3L); db.put("fallback:3", fallback);
        for (int index = 0; index < 1005; index++) {
            db.put("completed:1:" + index, index);
            db.put("file-report:1:item:" + index, index);
            db.put("inventory:manifest-1:entry:" + index, index);
        }
        db.put("file-report:1:coverage", new ReportCoverage());
        db.put("completed:10:keep", true);
        db.put("file-report:10:item:keep", true);
        db.put("inventory:manifest-10:ready", true);
        db.put("inventory:manifest-1:ready", true);
        db.put("node_config", "keep config");
        assertEquals(Long.valueOf(2), tasks.getFailedCount());
        assertEquals(Long.valueOf(1), tasks.getFallbackCount());

        assertEquals(1, tasks.permanentDeleteTasks(Arrays.asList(1, 1, 999)));

        assertEquals(Collections.singletonList(10), tasks.getDeletedTaskIdList());
        assertNull(tasks.getDeletedTaskStatus(1));
        assertNotNull(tasks.getDeletedTaskStatus(10));
        assertEmpty("completed:1:"); assertEmpty("file-report:1:"); assertEmpty("inventory:manifest-1:");
        assertTrue(db.exists("completed:10:keep")); assertTrue(db.exists("file-report:10:item:keep"));
        assertTrue(db.exists("inventory:manifest-10:ready")); assertTrue(db.exists("node_config"));
        assertNull(db.get("failed:1", FailedKey.class));
        assertEquals(Long.valueOf(1), tasks.getFailedCount());
        assertEquals(Long.valueOf(8), db.get("failed:count", Long.class));
        assertEquals(Long.valueOf(0), tasks.getFallbackCount());
        assertArrayEquals(new byte[]{1, 2, 3}, Files.readAllBytes(actualFile));
        assertTrue(control.isStopped(1));
        assertTrue(db.get("task:purged:1", Boolean.class));
    }

    @Test public void sharedParentAndInventorySurviveUntilLastSiblingAndUnrelatedParentSurvives() {
        SubTaskStatus first = task(-1, 7), second = task(-2, 7), unrelated = task(-3, 70);
        first.setInventoryManifestId("shared"); second.setInventoryManifestId("shared");
        deleted(first, second, unrelated);
        db.put("file-report:7:item:shared", true); db.put("completed:7:shared", true);
        db.put("file-report:70:item:keep", true); db.put("completed:70:keep", true);
        db.put("inventory:shared:ready", true);
        db.put("local:discovery:7", new LocalDiscoveryState());
        db.put("local:shard-assignment:7:first", -1); db.put("local:batch-assignment:7:second", -2);
        db.put("failed:count", 3L);
        db.put("failed:1", failed(-1, 7)); db.put("failed:2", failed(-2, 7)); db.put("failed:3", failed(-3, 70));
        tasks.permanentDeleteTasks(Collections.singletonList(-1));
        assertTrue(db.exists("file-report:7:item:shared")); assertTrue(db.exists("completed:7:shared"));
        assertTrue(db.exists("inventory:shared:ready"));
        assertFalse(Boolean.TRUE.equals(db.get("report:purged:7", Boolean.class)));
        assertNotNull(db.get("failed:2", FailedKey.class));

        tasks.permanentDeleteTasks(Collections.singletonList(-2));
        assertEmpty("file-report:7:"); assertEmpty("completed:7:"); assertEmpty("inventory:shared:");
        assertEmpty("local:shard-assignment:7:"); assertEmpty("local:batch-assignment:7:");
        assertFalse(db.exists("local:discovery:7"));
        assertTrue(db.exists("file-report:70:item:keep")); assertTrue(db.exists("completed:70:keep"));
        assertEquals(Long.valueOf(1), tasks.getFailedCount());
        assertTrue(control.isStopped(7)); assertTrue(db.get("local:parent:purged:7", Boolean.class));
    }

    @Test public void lateReportCompletionAndQueuedFailuresCannotRecreatePurgedRecords() throws Exception {
        SubTaskStatus task = task(2, null); deleted(task);
        tasks.permanentDeleteTasks(Collections.singletonList(2));
        ledger.discovered(task, "late", 4L, null);
        ledger.scanCompleted(task);
        tasks.updateTaskStatus(task);
        assertNull(tasks.getTaskStatus(2));
        ListSubTask staleImport = new ListSubTask();
        try { tasks.saveAssignedTask(2, staleImport); fail("permanently deleted task must reject a late import"); }
        catch (IllegalStateException expected) { assertNull(tasks.getTaskStatus(2)); }
        tasks.saveCompletedItem(2, TransferDirection.OSS_TO_OSS, "late", 4L);
        assertTrue(ledger.page(2, null, 500).isDone());
        assertEmpty("file-report:2:"); assertEmpty("completed:2:");
        // Run the real consumer with a queued pre-delete message followed by an unrelated failure.
        BufferQueueManager queue = mock(BufferQueueManager.class);
        FailedKeyMessage stale = new FailedKeyMessage(); stale.setTaskIndex(2); stale.setKey("late");
        FailedKeyMessage keep = new FailedKeyMessage(); keep.setTaskIndex(3); keep.setKey("keep");
        when(queue.takeFailedKey()).thenReturn(stale, keep).thenThrow(new InterruptedException());
        when(queue.takeFallback()).thenThrow(new InterruptedException());
        LocalTransferConfig config = new LocalTransferConfig(); config.setCompletionWriteBatch(1);
        FailedKeyConsumerService consumer = new FailedKeyConsumerService();
        ReflectionTestUtils.setField(consumer, "rocksDBService", db);
        ReflectionTestUtils.setField(consumer, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(consumer, "localTransferConfig", config);
        consumer.init();
        ((Thread) ReflectionTestUtils.getField(consumer, "failedKeyConsumerThread")).join(3000);
        consumer.shutdown();
        assertEquals(Long.valueOf(1), tasks.getFailedCount());
        assertEquals(Integer.valueOf(3), tasks.getFailedKeys().get(0).getTaskIndex());
    }

    @Test public void parentOnlyDiscoveryCanBePurgedButUnknownIdsAndRetainedTasksCannot() {
        LocalDiscoveryState discovery = new LocalDiscoveryState(); discovery.setParentTaskId(6);
        db.put("local:discovery:6", discovery); db.put("file-report:6:coverage", new ReportCoverage());
        db.put("completed:6:old", true); control.stop(6);
        assertEquals(0, tasks.permanentDeleteTasks(Collections.emptyList(), Arrays.asList(6, 99)));
        assertEmpty("file-report:6:"); assertEmpty("completed:6:");
        assertFalse(db.exists("local:discovery:6"));
        assertTrue(db.get("local:parent:purged:6", Boolean.class));
        assertFalse(db.exists("report:purged:99"));
        SubTaskStatus child = task(-8, 8); deleted(child); control.stop(8);
        try {
            tasks.permanentDeleteTasks(Collections.emptyList(), Collections.singletonList(8));
            // No parent identity in the supplied deletion scope: it is intentionally ignored.
            assertNotNull(tasks.getDeletedTaskStatus(-8));
        } catch (IllegalStateException expected) { assertNotNull(tasks.getDeletedTaskStatus(-8)); }
        db.put("local:discovery:8", new LocalDiscoveryState());
        try { tasks.permanentDeleteTasks(Collections.emptyList(), Collections.singletonList(8)); fail("retained child must block purge"); }
        catch (IllegalStateException expected) { assertNotNull(tasks.getDeletedTaskStatus(-8)); }
    }

    @Test public void receivedTaskReplayAfterPurgeAndDatabaseReopenCannotRecreateAnyBatchRows() {
        deleted(task(22, null));
        tasks.permanentDeleteTasks(Collections.singletonList(22));
        db.destroy();
        db.init();
        TaskImport unrelated = imported(23, null, null);
        TaskImport replay = imported(22, null, null);
        try {
            tasks.importTasksWithTaskId(Arrays.asList(unrelated, replay));
            fail("a purged task in a replayed dispatch must reject the whole batch before writes");
        } catch (IllegalStateException expected) {
            assertNull(tasks.getTaskStatus(22));
            assertNull(tasks.getTaskStatus(23));
            assertTrue(tasks.getTaskIdList().isEmpty());
            assertEmpty("file-report:22:");
            assertEmpty("file-report:23:");
            assertTrue(db.get("task:purged:22", Boolean.class));
        }
    }

    @Test public void localReceiveRejectsBothDeletedParentsAndRemappedPurgedShardIds() {
        LocalDiscoveryState discovery = new LocalDiscoveryState(); discovery.setParentTaskId(90);
        db.put("local:discovery:90", discovery); control.stop(90);
        tasks.permanentDeleteTasks(Collections.emptyList(), Collections.singletonList(90));
        try {
            tasks.importTasksWithTaskId(Collections.singletonList(imported(900, 90, "90-oss-0")));
            fail("a new physical id must not bypass the purged parent fence");
        } catch (IllegalStateException expected) {
            assertTrue(tasks.getTaskIdList().isEmpty());
            assertEmpty("local:shard-assignment:90:");
            assertEmpty("file-report:90:");
        }
        deleted(task(-22, 91), task(-23, 91));
        tasks.permanentDeleteTasks(Collections.singletonList(-22));
        assertFalse(Boolean.TRUE.equals(db.get("local:parent:purged:91", Boolean.class)));
        try {
            tasks.importTasksWithTaskId(Collections.singletonList(imported(22, 91, "91-oss-0")));
            fail("the resolved negative shard id must also be checked before import");
        } catch (IllegalStateException expected) {
            assertNull(tasks.getTaskStatus(-22));
            assertNotNull(tasks.getDeletedTaskStatus(-23));
            assertTrue(tasks.getTaskIdList().isEmpty());
            assertEmpty("local:shard-assignment:91:");
        }
    }

    @Test public void resumeAndRecoveryCannotBypassDurableTaskOrParentPurgeFences() {
        deleted(task(24, null));
        tasks.permanentDeleteTasks(Collections.singletonList(24));
        // A leftover row from an older import path must never let resume clear the stop fence.
        SubTaskStatus stale = task(24, null);
        db.put("task:24", stale);
        db.put("task:id:list", Collections.singletonList(24));
        try { tasks.resumeTask(24); fail("resume must reject a permanently deleted id"); }
        catch (IllegalStateException expected) { assertTrue(control.isStopped(24)); }
        control.resume(24);
        stale.setStatus(SubTaskStatus.TaskStatus.PENDING);
        try { tasks.recoverTask(stale); fail("automatic recovery must reject a permanently deleted id"); }
        catch (IllegalStateException expected) { assertEmpty("file-report:24:"); }

        LocalDiscoveryState discovery = new LocalDiscoveryState(); discovery.setParentTaskId(92);
        db.put("local:discovery:92", discovery); control.stop(92);
        tasks.permanentDeleteTasks(Collections.emptyList(), Collections.singletonList(92));
        SubTaskStatus child = task(-25, 92);
        db.put("task:-25", child);
        try { tasks.resumeTask(-25); fail("a purged parent must block a new child id"); }
        catch (IllegalStateException expected) {
            assertTrue(control.isStopped(92));
            assertEquals(SubTaskStatus.TaskStatus.STOPPED, tasks.getTaskStatus(-25).getStatus());
        }
        verifyNoMoreInteractions((BufferQueueManager) ReflectionTestUtils.getField(tasks, "bufferQueueManager"));
    }

    private TaskImport imported(int id, Integer parent, String shard) {
        TaskImport task = new TaskImport();
        task.setTaskIndex(id); task.setParentTaskId(parent); task.setShardId(shard);
        task.setDirection(parent == null ? TransferDirection.OSS_TO_OSS : TransferDirection.OSS_TO_LOCAL);
        task.setSourceBucket("source"); task.setDestBucket("destination");
        return task;
    }

    @Test public void activeSiblingProtectsItsSharedReportAndDirectPermanentClearUsesSameCleanup() {
        SubTaskStatus first = task(-1, 7), running = task(-2, 7);
        running.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        deleted(first);
        db.put("task:id:list", Collections.singletonList(-2)); db.put("task:-2", running);
        db.put("file-report:7:item:shared", true); db.put("completed:7:shared", true);
        tasks.permanentDeleteTasks(Collections.singletonList(-1));
        assertTrue(db.exists("file-report:7:item:shared")); assertTrue(db.exists("completed:7:shared"));
        SubTaskStatus standalone = task(4, null);
        db.put("task:4", standalone); db.put("task:id:list", Arrays.asList(-2, 4));
        db.put("file-report:4:coverage", new ReportCoverage());
        db.put("failed:count", 1L); db.put("failed:1", failed(4, null));
        assertEquals(1, tasks.clearTasks(null, null, Collections.singletonList(4), true));
        assertNull(tasks.getTaskStatus(4)); assertNull(tasks.getDeletedTaskStatus(4));
        assertEmpty("file-report:4:"); assertEquals(Long.valueOf(0), tasks.getFailedCount());
        assertEquals(Collections.singletonList(-2), tasks.getTaskIdList());
    }

    @Test public void cleanupFailureLeavesRecycleBinEntryAvailableForRetry() {
        RocksDBService failing = spy(db);
        ReflectionTestUtils.setField(tasks, "rocksDBService", failing);
        ReflectionTestUtils.setField(ledger, "rocksDBService", failing);
        SubTaskStatus task = task(11, null); deleted(task);
        doThrow(new IllegalStateException("injected cleanup failure")).doCallRealMethod().when(failing).deleteByPrefix("completed:11:");
        try { tasks.permanentDeleteTasks(Collections.singletonList(11)); fail("expected cleanup failure"); }
        catch (IllegalStateException expected) { assertNotNull(tasks.getDeletedTaskStatus(11)); }
        assertEquals(1, tasks.permanentDeleteTasks(Collections.singletonList(11)));
        assertNull(tasks.getDeletedTaskStatus(11));
    }

    @Test public void lateDiscoveryAndResumeCannotRecreatePermanentlyDeletedParent() {
        SubTaskStatus child = task(-77, 77); deleted(child); control.stop(77);
        assertEquals(1, tasks.permanentDeleteTasks(Collections.singletonList(-77)));
        LocalTransferService local = discoveryService();
        try {
            try { local.startDiscovery(discoveryRequest(77)); fail("late discovery must be rejected"); }
            catch (TransferValidationException expected) { assertEquals("TASK_PERMANENTLY_DELETED", expected.getCode()); }
            assertEquals(1, local.resume(Collections.singletonList(77), true).get("failed"));
            try { local.retry(Collections.singletonList(77)); fail("late retry must be rejected"); }
            catch (TransferValidationException expected) { assertEquals("TASK_PERMANENTLY_DELETED", expected.getCode()); }
            assertFalse(db.exists("local:discovery:77"));
            assertEmpty("file-report:77:");
            assertTrue(control.isStopped(77));
            assertTrue(db.get("local:stop:77", LocalTaskControlService.StopRecord.class).stopped);
        } finally { local.shutdownDiscovery(); }
    }

    @Test public void discoveryAdmissionWaitsForConcurrentPermanentDeletionControlFence() throws Exception {
        SubTaskStatus child = task(-88, 88); deleted(child); control.stop(88);
        LocalTransferService local = discoveryService();
        java.util.concurrent.CountDownLatch validated = new java.util.concurrent.CountDownLatch(1);
        doAnswer(call -> { validated.countDown(); return Collections.emptyMap(); })
                .when(local).validateOnWorker(any(LocalTransferRequest.class));
        java.util.concurrent.ExecutorService executor = java.util.concurrent.Executors.newSingleThreadExecutor();
        java.util.concurrent.Future<Map<String,Object>> attempt;
        try {
            synchronized (control) {
                attempt = executor.submit(() -> local.startDiscovery(discoveryRequest(88)));
                assertTrue(validated.await(3, java.util.concurrent.TimeUnit.SECONDS));
                assertEquals(1, tasks.permanentDeleteTasks(Collections.singletonList(-88)));
            }
            try { attempt.get(3, java.util.concurrent.TimeUnit.SECONDS); fail("late admission must fail after purge"); }
            catch (java.util.concurrent.ExecutionException expected) { assertTrue(expected.getCause() instanceof TransferValidationException); }
            assertFalse(db.exists("local:discovery:88")); assertEmpty("file-report:88:");
            assertTrue(control.isStopped(88));
        } finally { executor.shutdownNow(); local.shutdownDiscovery(); }
    }

    @Test public void ordinaryStoppedDiscoveryRequiresExplicitResumeAndRemainsRecoverable() {
        LocalTransferService local = discoveryService();
        LocalTransferRequest request = discoveryRequest(90);
        LocalDiscoveryState state = new LocalDiscoveryState(90, request, "worker-a", null, 0,
                LocalDiscoveryState.Status.COMPLETED, null, 1L);
        db.put("local:discovery:90", state); control.stop(90);
        try {
            try { local.startDiscovery(request); fail("stopped discovery should not be accepted"); }
            catch (TaskCancelledException expected) { assertTrue(control.isStopped(90)); }
            assertEquals(0, local.resume(Collections.singletonList(90), true).get("failed"));
            assertFalse(control.isStopped(90));
            assertEquals(true, local.startDiscovery(request).get("accepted"));
            assertEquals(LocalDiscoveryState.Status.COMPLETED, db.get("local:discovery:90", LocalDiscoveryState.class).getStatus());
            assertFalse(db.exists("local:parent:purged:90"));
        } finally { local.shutdownDiscovery(); }
    }

    private LocalTransferService discoveryService() {
        LocalTransferService local = spy(new LocalTransferService());
        doReturn(Collections.emptyMap()).when(local).validateOnWorker(any(LocalTransferRequest.class));
        com.transfer.tools.config.DistributedConfig config = new com.transfer.tools.config.DistributedConfig();
        config.setLocalIp("worker-a");
        ReflectionTestUtils.setField(local, "distributedConfig", config);
        ReflectionTestUtils.setField(local, "normalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(local, "rocksDBService", db);
        ReflectionTestUtils.setField(local, "controlService", control);
        ReflectionTestUtils.setField(local, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(local, "fileReportLedgerService", ledger);
        ReflectionTestUtils.setField(local, "transferSpeedService", mock(TransferSpeedService.class));
        return local;
    }
    private LocalTransferRequest discoveryRequest(int id) {
        LocalTransferRequest request = new LocalTransferRequest();
        request.setParentTaskId(id); request.setDirection(TransferDirection.LOCAL_TO_OSS);
        TransferEndpoint source = new TransferEndpoint(); source.setType(EndpointType.LOCAL);
        source.setNodeBindings(Collections.singletonList(new LocalNodeBinding("worker-a", "/shared")));
        TransferEndpoint destination = new TransferEndpoint(); destination.setType(EndpointType.OSS);
        destination.setS3Alias("qa"); destination.setBucket("test-bucket");
        request.setSourceEndpoint(source); request.setDestinationEndpoint(destination);
        return request;
    }

    private void assertEmpty(String prefix) { assertTrue(prefix, db.scanPage(prefix, null, 1000, Object.class).getItems().isEmpty()); }
    private void deleted(SubTaskStatus... statuses) {
        List<Integer> ids = new ArrayList<>();
        for (SubTaskStatus status : statuses) { ids.add(status.getTaskIndex()); db.put("task:deleted:" + status.getTaskIndex(), status); }
        db.put("task:id:list:deleted", ids);
    }
    private SubTaskStatus task(int id, Integer parent) {
        SubTaskStatus task = new SubTaskStatus(); task.setTaskIndex(id); task.setParentTaskId(parent);
        task.setStatus(SubTaskStatus.TaskStatus.STOPPED); task.setDirection(parent == null ? TransferDirection.OSS_TO_OSS : TransferDirection.OSS_TO_LOCAL);
        task.setSourceBucket("source"); task.setDestBucket("destination"); task.setShardId("shard-" + id);
        return task;
    }
    private FailedKey failed(int id, Integer parent) {
        FailedKey failed = new FailedKey(); failed.setTaskIndex(id); failed.setParentTaskId(parent); failed.setKey("key-" + id); return failed;
    }
}
