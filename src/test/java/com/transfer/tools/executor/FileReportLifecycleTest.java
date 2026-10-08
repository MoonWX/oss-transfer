package com.transfer.tools.executor;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.*;
import com.transfer.tools.config.SourceBucketConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.service.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.*;
import java.util.concurrent.Semaphore;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class FileReportLifecycleTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService store;
    private FileReportLedgerService ledger;
    private BufferQueueManager queue;
    private SubTaskManagerService tasks;
    private TaskProgressCache progress;
    private LocalTaskControlService control;
    private S3ClientManager clients;
    private LocalFileService files;

    @Before public void setup() throws Exception {
        store = new RocksDBService();
        ReflectionTestUtils.setField(store, "dbPath", temporary.newFolder("db").getAbsolutePath());
        store.init();
        ledger = new FileReportLedgerService();
        ReflectionTestUtils.setField(ledger, "rocksDBService", store);
        queue = mock(BufferQueueManager.class); tasks = mock(SubTaskManagerService.class);
        progress = mock(TaskProgressCache.class); control = mock(LocalTaskControlService.class);
        clients = mock(S3ClientManager.class); files = mock(LocalFileService.class);
        when(control.shouldProcess(anyInt(), anyString())).thenReturn(true);
    }
    @After public void close() { Thread.interrupted(); store.destroy(); }

    @Test public void ossScanPersistsBeforeQueueAndDistinguishesTimeFiltering() throws Exception {
        SubTaskStatus task = task(1, TransferDirection.OSS_TO_OSS);
        task.setDateAfter("2026-01-01T00:00:00");
        ObjectListing listing = new ObjectListing();
        listing.getObjectSummaries().add(summary("old", 3, new Date(0)));
        listing.getObjectSummaries().add(summary("new", 7, new Date(2000000000000L)));
        AmazonS3 s3 = mock(AmazonS3.class);
        when(clients.getListClient("source")).thenReturn(s3);
        when(s3.listObjects(any(ListObjectsRequest.class))).thenReturn(listing);
        doAnswer(call -> {
            ObjectListMessage message = call.getArgument(0);
            if (!Boolean.TRUE.equals(message.getIsLastMessage())) {
                FileReportRecord pending = rows(1).get("new");
                assertNotNull("durable row must precede queue insertion", pending);
                assertEquals("UNPROCESSED", pending.getResult());
            }
            return null;
        }).when(queue).putPutMessage(any(ObjectListMessage.class));
        runList(task, listService(ledger));
        assertEquals("TIME_FILTER", rows(1).get("old").getReasonCode());
        assertEquals("SKIPPED", rows(1).get("old").getResult());
        assertEquals("UNPROCESSED", rows(1).get("new").getResult());
        assertTrue(ledger.coverage(1).isScanComplete());
        verify(queue, times(2)).putPutMessage(any(ObjectListMessage.class));
    }

    @Test public void exactDownloadRecordsHeadSizeAndRenamedDestinationBeforeQueue() throws Exception {
        SubTaskStatus task = task(2, TransferDirection.OSS_TO_LOCAL);
        task.setObjectKey("prefix/source.bin"); task.setTargetRelativePath("renamed/file.bin");
        task.setLocalPath("/downloads");
        AmazonS3 s3 = mock(AmazonS3.class);
        ObjectMetadata metadata = new ObjectMetadata(); metadata.setContentLength(1234);
        when(clients.getClient("source")).thenReturn(s3);
        when(s3.getObjectMetadata("bucket", "prefix/source.bin")).thenReturn(metadata);
        runList(task, listService(ledger));
        FileReportRecord row = rows(2).get("prefix/source.bin");
        assertEquals(Long.valueOf(1234), row.getSize());
        assertEquals("/downloads/renamed/file.bin", row.getDestinationPath());
        assertEquals("UNPROCESSED", row.getResult());
        assertTrue(ledger.coverage(2).isScanComplete());
    }

    @Test public void localBatchResolveFailureLeavesFileFailureAndIncompleteScan() {
        SubTaskStatus task = task(3, TransferDirection.LOCAL_TO_OSS);
        task.setLocalPath("/missing-root");
        TransferEndpoint destination = new TransferEndpoint(); destination.setPrefix("target");
        task.setDestinationEndpoint(destination);
        task.setLocalFileEntries(Collections.singletonList(new LocalFileEntry("file.bin", 17L, 0L)));
        when(files.resolveSource("/missing-root", "file.bin"))
                .thenThrow(new TransferValidationException("LOCAL_PATH_NOT_FOUND", "source disappeared"));
        runList(task, listService(ledger));
        FileReportRecord row = rows(3).get("target/file.bin");
        assertEquals("FAILED", row.getResult());
        assertEquals("READ_SOURCE_FAILED", row.getReasonCode());
        assertEquals("/missing-root/file.bin", row.getSourcePath());
        assertEquals(SubTaskStatus.TaskStatus.FAILED, task.getStatus());
        assertFalse(ledger.coverage(3).isScanComplete());
    }

    @Test public void localBatchPersistsActualFilePathBeforeEnqueue() throws Exception {
        SubTaskStatus task = task(4, TransferDirection.LOCAL_TO_OSS);
        task.setLocalPath("/share");
        TransferEndpoint destination = new TransferEndpoint(); destination.setPrefix("target");
        task.setDestinationEndpoint(destination);
        task.setLocalFileEntries(Collections.singletonList(new LocalFileEntry("file.bin", 17L, 0L)));
        when(files.resolveSource("/share", "file.bin")).thenReturn(java.nio.file.Paths.get("/share/file.bin"));
        doAnswer(call -> {
            ObjectListMessage message = call.getArgument(0);
            if (!Boolean.TRUE.equals(message.getIsLastMessage())) {
                assertEquals("/share/file.bin", rows(4).get("target/file.bin").getSourcePath());
            }
            return null;
        }).when(queue).putPutMessage(any(ObjectListMessage.class));
        runList(task, listService(ledger));
        assertTrue(ledger.coverage(4).isScanComplete());
    }

    @Test public void putLifecycleRecordsSuccessTargetSkipAndFailureForThreeDirections() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage upload = message(5, TransferDirection.LOCAL_TO_OSS, "upload");
        ObjectListMessage download = message(6, TransferDirection.OSS_TO_LOCAL, "download");
        ObjectListMessage copy = message(7, TransferDirection.OSS_TO_OSS, "copy");
        when(migration.migrate(upload)).thenReturn(true);
        when(migration.migrate(download)).thenReturn(false);
        when(migration.migrate(copy)).thenThrow(new IllegalStateException("destination rejected"));
        runPut(putService(ledger, migration), upload, download, copy);
        assertEquals("SUCCESS", rows(5).get("upload").getResult());
        assertEquals("TARGET_EXISTS", rows(6).get("download").getReasonCode());
        assertEquals("SKIPPED", rows(6).get("download").getResult());
        assertEquals("FAILED", rows(7).get("copy").getResult());
        assertEquals("destination rejected", rows(7).get("copy").getErrorMessage());
        verify(tasks).saveCompletedItem(5, TransferDirection.LOCAL_TO_OSS, "upload", 23L);
    }

    @Test public void reportingFailureCannotPreventOriginalTransferFailureAccounting() throws Exception {
        FileReportLedgerService broken = mock(FileReportLedgerService.class);
        doThrow(new FileReportLedgerService.FileReportPersistenceException("disk full", new RuntimeException()))
                .when(broken).finished(any(ObjectListMessage.class), anyString(), anyString(), any());
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage message = message(8, TransferDirection.OSS_TO_OSS, "failed");
        when(migration.migrate(message)).thenThrow(new IllegalStateException("network failed"));
        runPut(putService(broken, migration), message);
        verify(tasks).recordFailedKey(8, "failed", "network failed", 23L,
                FailureStage.WRITE_DESTINATION, "TRANSFER_FAILED", "/files");
        verify(progress).addToProgress(8, 0L, 0L, 0L, 0L, 1L, 23L, "failed");
    }

    @Test public void reportingFailureCannotPreventScanFailureOrStopState() {
        for (boolean stopped : new boolean[]{false, true}) {
            SubTaskStatus task = task(stopped ? 10 : 9, TransferDirection.OSS_TO_OSS);
            FileReportLedgerService broken = mock(FileReportLedgerService.class);
            RuntimeException original = stopped ? new TaskCancelledException("stopped") : new IllegalStateException("scan failed");
            doThrow(original).when(broken).scanStarted(task);
            doThrow(new IllegalStateException("disk unavailable")).when(broken).scanFailed(anyInt(), any());
            ReflectionTestUtils.invokeMethod(listService(broken), "executeListTask", task);
            assertEquals(stopped ? SubTaskStatus.TaskStatus.STOPPED : SubTaskStatus.TaskStatus.FAILED, task.getStatus());
        }
    }

    @Test public void completionDedupRemainsSuccessAndDryRunIsExplicitlySkipped() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage dedup = message(11, TransferDirection.LOCAL_TO_OSS, "already-copied");
        when(tasks.isItemCompleted(11, "already-copied")).thenReturn(true);
        ObjectListMessage dryRun = message(12, TransferDirection.OSS_TO_OSS, "dry-run");
        dryRun.setReportReasonCode("DRY_RUN");
        when(migration.migrate(dryRun)).thenReturn(true);
        runPut(putService(ledger, migration), dedup, dryRun);
        assertEquals("SUCCESS", rows(11).get("already-copied").getResult());
        assertEquals("ALREADY_COMPLETED", rows(11).get("already-copied").getReasonCode());
        assertEquals("SKIPPED", rows(12).get("dry-run").getResult());
        assertEquals("DRY_RUN", rows(12).get("dry-run").getReasonCode());
        verify(migration, never()).migrate(dedup);
        verify(tasks, never()).saveCompletedItem(eq(12), any(), anyString(), anyLong());
    }

    @Test public void keyOnlyInventoryResolvesHeadBeforeMigrationAndCountsBytesExactlyOnce() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage inventory = message(13, TransferDirection.OSS_TO_OSS, "inventory-key");
        inventory.setSourceSize(null);
        inventory.setSourceSizeUnknown(true);
        long size = 1024 * 1024 + 123L;
        Date modified = new Date(1234567L);
        AmazonS3 source = inventorySource("inventory-key", size, modified);
        when(migration.migrate(inventory)).thenAnswer(call -> {
            assertNotNull(inventory.getObjectSummary());
            assertEquals("inventory-key", inventory.getObjectSummary().getKey());
            assertEquals(size, inventory.getObjectSummary().getSize());
            assertEquals(modified, inventory.getObjectSummary().getLastModified());
            assertEquals(Long.valueOf(size), inventory.getSourceSize());
            assertFalse(inventory.getSourceSizeUnknown());
            assertEquals(Long.valueOf(size), rows(13).get("inventory-key").getSize());
            return true;
        });
        runPut(putService(ledger, migration), inventory);
        FileReportRecord row = rows(13).get("inventory-key");
        assertEquals("SUCCESS", row.getResult());
        assertEquals(Long.valueOf(size), row.getSize());
        assertEquals("TRACKED", ledger.coverage(13).getLedgerCoverage());
        verify(source).getObjectMetadata("bucket", "inventory-key");
        verify(progress).addToProgress(13, 0L, size, 1L, size, 0L, 0L, "inventory-key");
        verifyNoMoreInteractions(progress);
        verify(tasks).saveCompletedItem(13, TransferDirection.OSS_TO_OSS, "inventory-key", size);
    }

    @Test public void inventoryActualLastModifiedMakesRealMigrationSkipExistingNewerTarget() throws Exception {
        ObjectListMessage inventory = message(14, TransferDirection.OSS_TO_OSS, "exists");
        inventory.setSourceSize(null); inventory.setSourceSizeUnknown(true);
        AmazonS3 source = inventorySource("exists", 8192L, new Date(1000));
        AmazonS3 destination = mock(AmazonS3.class);
        ObjectMetadata target = new ObjectMetadata(); target.setContentLength(8192); target.setLastModified(new Date(2000));
        when(clients.getClient("target")).thenReturn(destination);
        when(destination.getObjectMetadata(any(GetObjectMetadataRequest.class))).thenReturn(target);
        ObjectMigrationService migration = new ObjectMigrationService();
        RateLimiterManager rates = mock(RateLimiterManager.class);
        when(rates.getAutoAdjustConfig()).thenReturn(new com.transfer.tools.config.RateLimitConfig.AutoAdjustConfig());
        ReflectionTestUtils.setField(migration, "s3ClientManager", clients);
        ReflectionTestUtils.setField(migration, "localTaskControlService", control);
        ReflectionTestUtils.setField(migration, "rateLimiterManager", rates);
        runPut(putService(ledger, migration), inventory);
        assertEquals("SKIPPED", rows(14).get("exists").getResult());
        assertEquals(Long.valueOf(8192), rows(14).get("exists").getSize());
        verify(progress).addToProgress(14, 0L, 8192L, 0L, 0L, 0L, 0L, 1L, 8192L, "exists");
        verify(source, never()).getObject(anyString(), anyString());
        verify(destination, never()).putObject(any(PutObjectRequest.class));
    }

    @Test public void inventoryPutMetadataAndCompletionReconcileAfterScannerProgressAlreadyFlushed() throws Exception {
        TaskProgressCache actualProgress = new TaskProgressCache();
        ReflectionTestUtils.setField(actualProgress, "rocksDBService", store);
        ReflectionTestUtils.setField(actualProgress, "subTaskManagerService", tasks);
        SubTaskStatus task = task(26, TransferDirection.OSS_TO_OSS);
        task.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        ledger.registerTask(task);
        store.putSync("task:26", task);
        actualProgress.addToProgress(26, 1, 0L, 0, 0L, 0, 0L, null);
        actualProgress.updateTaskStatus(26, SubTaskStatus.TaskStatus.LIST_DONE);
        ReflectionTestUtils.invokeMethod(actualProgress, "flushToRocksDB");

        ObjectListMessage inventory = message(26, TransferDirection.OSS_TO_OSS, "delayed-metadata");
        inventory.setSourceSize(null); inventory.setSourceSizeUnknown(true);
        long size = 1024 * 1024 + 123L;
        inventorySource("delayed-metadata", size, new Date(1000));
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        when(migration.migrate(inventory)).thenReturn(true);
        PutExecutionService put = putService(ledger, migration);
        ReflectionTestUtils.setField(put, "taskProgressCache", actualProgress);
        runPut(put, inventory);
        ReflectionTestUtils.invokeMethod(actualProgress, "flushToRocksDB");
        SubTaskStatus completed = store.get("task:26", SubTaskStatus.class);
        assertEquals(SubTaskStatus.TaskStatus.COMPLETED, completed.getStatus());
        assertEquals(Long.valueOf(1), completed.getListObjectCount());
        assertEquals(Long.valueOf(1), completed.getPutObjectCount());
        assertEquals(Long.valueOf(size), completed.getListObjectSize());
        assertEquals(completed.getPutObjectSize(), completed.getListObjectSize());
        assertEquals(rows(26).get("delayed-metadata").getSize(), completed.getListObjectSize());
    }

    @Test public void inventoryReplayRestoresSuccessAndSkipBytesWithoutSourceHeadOrRewrite() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage successful = message(15, TransferDirection.OSS_TO_OSS, "successful");
        ObjectListMessage skipped = message(15, TransferDirection.OSS_TO_OSS, "zero-byte-skip");
        successful.setSourceSize(5243003L); skipped.setSourceSize(0L);
        ledger.registerTask(task(15, TransferDirection.OSS_TO_OSS));
        ledger.finished(successful, "SUCCESS", "TRANSFERRED", null);
        ledger.finished(skipped, "SKIPPED", "TARGET_EXISTS", null);
        successful.setSourceSize(null); successful.setSourceSizeUnknown(true);
        skipped.setSourceSize(null); skipped.setSourceSizeUnknown(true);
        runPut(putService(ledger, migration), successful, skipped);
        verifyZeroInteractions(clients, migration);
        verify(progress).addToProgress(15, 0L, 5243003L, 1L, 5243003L, 0L, 0L, 0L, 0L, "successful");
        verify(progress).addToProgress(15, 0L, 0L, 0L, 0L, 0L, 0L, 1L, 0L, "zero-byte-skip");
        verifyNoMoreInteractions(progress);
        assertEquals(Long.valueOf(5243003L), rows(15).get("successful").getSize());
        assertEquals(Long.valueOf(0L), rows(15).get("zero-byte-skip").getSize());
    }

    @Test public void inventoryHeadFailureRetainsUnknownSizeAndDoesNotTransferOrInventSuccess() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage inventory = message(16, TransferDirection.OSS_TO_OSS, "missing");
        inventory.setSourceSize(null); inventory.setSourceSizeUnknown(true);
        AmazonS3 source = mock(AmazonS3.class); when(clients.getClient("source")).thenReturn(source);
        AmazonS3Exception missing = new AmazonS3Exception("missing source"); missing.setStatusCode(404);
        when(source.getObjectMetadata("bucket", "missing")).thenThrow(missing);
        runPut(putService(ledger, migration), inventory);
        assertEquals("FAILED", rows(16).get("missing").getResult());
        assertEquals("SOURCE_METADATA_FAILED", rows(16).get("missing").getReasonCode());
        assertNull(rows(16).get("missing").getSize());
        verifyZeroInteractions(migration);
        verify(tasks).recordFailedKey(eq(16), eq("missing"), anyString(), eq(0L), eq(FailureStage.READ_SOURCE), eq("SOURCE_METADATA_FAILED"), eq("/files"));
        verify(progress).addToProgress(16, 0L, 0L, 0L, 0L, 1L, 0L, "missing");
    }

    @Test public void stopDuringInventoryHeadDoesNotTransferOrPublishFailure() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage inventory = message(17, TransferDirection.OSS_TO_OSS, "cancelled");
        inventory.setSourceSize(null); inventory.setSourceSizeUnknown(true);
        LocalTaskControlService realControl = new LocalTaskControlService();
        AmazonS3 source = inventorySource("cancelled", 8192L, new Date(1000));
        when(source.getObjectMetadata("bucket", "cancelled")).thenAnswer(call -> {
            realControl.stop(17); ObjectMetadata metadata = new ObjectMetadata(); metadata.setContentLength(8192); return metadata;
        });
        PutExecutionService executor = putService(ledger, migration);
        ReflectionTestUtils.setField(executor, "localTaskControlService", realControl);
        runPut(executor, inventory);
        assertEquals("UNPROCESSED", rows(17).get("cancelled").getResult());
        assertNull(rows(17).get("cancelled").getSize());
        verifyZeroInteractions(migration, progress);
        verify(tasks, never()).recordFailedKey(anyInt(), anyString(), anyString(), anyLong(), any(), anyString(), any());
    }

    @Test public void metadataRepairPreservesLegacySuccessWithoutUnknownReportSize() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage inventory = message(18, TransferDirection.OSS_TO_OSS, "old-success");
        inventory.setSourceSize(null); inventory.setSourceSizeUnknown(true);
        ledger.registerTask(task(18, TransferDirection.OSS_TO_OSS));
        ledger.finished(inventory, "SUCCESS", "TRANSFERRED", null);
        inventorySource("old-success", 4096L, new Date(1000));
        runPut(putService(ledger, migration), inventory);
        assertEquals("SUCCESS", rows(18).get("old-success").getResult());
        assertEquals(Long.valueOf(4096), rows(18).get("old-success").getSize());
        verifyZeroInteractions(migration);
        verify(progress).addToProgress(18, 0L, 4096L, 1L, 4096L, 0L, 0L, 0L, 0L, "old-success");
    }

    @Test public void replayUsesSynchronousLedgerWhenLegacyCompletionIndexIsMissing() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage done = message(21, TransferDirection.OSS_TO_LOCAL, "committed-before-crash");
        ObjectListMessage skipped = message(21, TransferDirection.OSS_TO_LOCAL, "existing-target");
        ObjectListMessage pending = message(21, TransferDirection.OSS_TO_LOCAL, "unfinished");
        ledger.registerTask(task(21, TransferDirection.OSS_TO_LOCAL));
        ledger.finished(done, "SUCCESS", "TRANSFERRED", null);
        ledger.finished(skipped, "SKIPPED", "TARGET_EXISTS", null);
        when(tasks.isItemCompleted(anyInt(), anyString())).thenReturn(false);
        when(migration.migrate(pending)).thenReturn(true);
        runPut(putService(ledger, migration), done, skipped, pending);
        verify(migration, never()).migrate(done);
        verify(migration, never()).migrate(skipped);
        verify(migration).migrate(pending);
        assertEquals("SUCCESS", rows(21).get("committed-before-crash").getResult());
        assertEquals("SKIPPED", rows(21).get("existing-target").getResult());
        assertEquals("SUCCESS", rows(21).get("unfinished").getResult());
    }

    @Test public void largeFileCompletionOnlyAdvancesDurableProgressNotStreamSpeed() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage message = message(25, TransferDirection.OSS_TO_LOCAL, "large-file");
        long fileSize = 4L * 1024L * 1024L * 1024L;
        message.setSourceSize(fileSize);
        TransferSpeedService speeds = new TransferSpeedService();
        when(migration.migrate(message)).thenAnswer(call -> {
            // Simulate the final stream chunk after earlier chunks aged out of the window.
            speeds.record(message.getTaskIndex(), 65_536L);
            return true;
        });
        runPut(putService(ledger, migration), message);
        assertEquals("SUCCESS", rows(25).get("large-file").getResult());
        verify(progress).addToProgress(25, 0L, 0L, 1L, fileSize, 0L, 0L, "large-file");
        assertEquals(65_536L / 5L, speeds.currentBytesPerSecond(25));
    }

    @Test public void staleQueueMessageFromPreviousAttemptDoesNotRunOrAdvanceProgress() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage stale = message(22, TransferDirection.OSS_TO_LOCAL, "stale");
        SubTaskStatus current = task(22, TransferDirection.OSS_TO_LOCAL); current.setAttempt(2);
        when(tasks.getTaskStatus(22)).thenReturn(current);
        stale.setExecutionAttempt(1);
        runPut(putService(ledger, migration), stale);
        verify(migration, never()).migrate(any(ObjectListMessage.class));
        assertEquals("UNPROCESSED", rows(22).get("stale").getResult());
    }

    @Test public void durableSuccessRetainsProgressWhenLegacyCompletionWaitIsInterrupted() throws Exception {
        ObjectMigrationService migration = mock(ObjectMigrationService.class);
        ObjectListMessage message = message(23, TransferDirection.LOCAL_TO_OSS, "committed");
        when(migration.migrate(message)).thenReturn(true);
        doThrow(new RuntimeException("completion persistence interrupted"))
                .when(tasks).saveCompletedItem(23, TransferDirection.LOCAL_TO_OSS, "committed", 23L);
        runPut(putService(ledger, migration), message);
        assertEquals("SUCCESS", rows(23).get("committed").getResult());
        verify(progress).addToProgress(23, 0L, 0L, 1L, 23L, 0L, 0L, "committed");
        verify(progress, never()).addToProgress(23, 0L, 0L, 0L, 0L, 1L, 23L, "committed");
        verify(tasks, never()).recordFailedKey(eq(23), anyString(), anyString(), anyLong(), any(), anyString(), anyString());
    }

    @Test public void interruptedQueuePutAfterParentStopCannotWriteStaleFailedSnapshot() throws Exception {
        SubTaskStatus task = task(24, TransferDirection.LOCAL_TO_OSS); task.setParentTaskId(240);
        task.setLocalPath("/share");
        task.setLocalFileEntries(Collections.singletonList(new LocalFileEntry("file.bin", 17L, 0L)));
        when(files.resolveSource("/share", "file.bin")).thenReturn(java.nio.file.Paths.get("/share/file.bin"));
        doAnswer(call -> {
            when(control.isStopped(240)).thenReturn(true);
            // BlockingQueue.put clears the interrupt flag when it throws InterruptedException.
            Thread.interrupted();
            throw new InterruptedException("put queue cancelled by parent stop");
        }).when(queue).putPutMessage(any(ObjectListMessage.class));
        runList(task, listService(ledger));
        assertNull(ledger.coverage(240).getLastError());
        verify(tasks, never()).failTask(24);
        verify(tasks, never()).recordFailedKey(eq(24), any(), any(), anyLong(), any(), any());
        assertEquals(SubTaskStatus.TaskStatus.RUNNING, task.getStatus());
    }

    private ListExecutionService listService(FileReportLedgerService reports) {
        ListExecutionService service = new ListExecutionService();
        ReflectionTestUtils.setField(service, "fileReportLedgerService", reports);
        ReflectionTestUtils.setField(service, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(service, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(service, "taskProgressCache", progress);
        ReflectionTestUtils.setField(service, "localTaskControlService", control);
        ReflectionTestUtils.setField(service, "s3ClientManager", clients);
        ReflectionTestUtils.setField(service, "localFileService", files);
        ReflectionTestUtils.setField(service, "rateLimiterManager", mock(RateLimiterManager.class));
        ReflectionTestUtils.setField(service, "localTaskRateLimiterService", mock(LocalTaskRateLimiterService.class));
        SourceBucketConfig config = new SourceBucketConfig();
        SourceBucketConfig.ListConfig list = new SourceBucketConfig.ListConfig(); list.setMaxKeys(1000); config.setList(list);
        ReflectionTestUtils.setField(service, "sourceBucketConfig", config);
        return service;
    }
    private PutExecutionService putService(FileReportLedgerService reports, ObjectMigrationService migration) {
        PutExecutionService service = new PutExecutionService();
        ReflectionTestUtils.setField(service, "fileReportLedgerService", reports);
        ReflectionTestUtils.setField(service, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(service, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(service, "taskProgressCache", progress);
        ReflectionTestUtils.setField(service, "localTaskControlService", control);
        ReflectionTestUtils.setField(service, "objectMigrationService", migration);
        ReflectionTestUtils.setField(service, "s3ClientManager", clients);
        ReflectionTestUtils.setField(service, "rateLimiterManager", mock(RateLimiterManager.class));
        ReflectionTestUtils.setField(service, "localTransferPermits", new Semaphore(1));
        return service;
    }
    private void runPut(PutExecutionService service, ObjectListMessage... messages) throws Exception {
        for (ObjectListMessage message : messages) {
            SubTaskStatus task = task(message.getTaskIndex(), message.getDirection());
            ledger.registerTask(task); ledger.discovered(task, message.getKey(), message.getSourceSize(), message.getLocalPath());
        }
        java.util.concurrent.atomic.AtomicInteger index = new java.util.concurrent.atomic.AtomicInteger();
        when(queue.takePutMessage()).thenAnswer(call -> {
            int current = index.getAndIncrement();
            if (current == messages.length) throw new InterruptedException("test finished");
            return messages[current];
        });
        ReflectionTestUtils.invokeMethod(service, "processMessages");
        Thread.interrupted();
    }
    private void runList(SubTaskStatus task, ListExecutionService service) {
        ledger.registerTask(task);
        ReflectionTestUtils.invokeMethod(service, "executeListTask", task);
    }
    private Map<String, FileReportRecord> rows(int id) {
        Map<String, FileReportRecord> rows = new HashMap<>();
        for (FileReportRecord row : ledger.page(id, null, 500).getItems()) rows.put(row.getLogicalKey(), row);
        return rows;
    }
    private SubTaskStatus task(int id, TransferDirection direction) {
        SubTaskStatus task = new SubTaskStatus(); task.setTaskIndex(id); task.setDirection(direction);
        task.setSourceBucket("bucket"); task.setDestBucket("destination"); task.setSourceS3Alias("source");
        task.setDestS3Alias("target"); task.setStartKey(""); task.setEndKey("");
        task.setStatus(SubTaskStatus.TaskStatus.PENDING); return task;
    }
    private ObjectListMessage message(int id, TransferDirection direction, String key) {
        ObjectListMessage message = new ObjectListMessage(null, id, false, "bucket", "destination", "source", "target", false, key);
        message.setDirection(direction); message.setSourceSize(23L); message.setLocalPath("/files"); return message;
    }
    private S3ObjectSummary summary(String key, long size, Date modified) {
        S3ObjectSummary summary = new S3ObjectSummary(); summary.setKey(key); summary.setSize(size);
        summary.setLastModified(modified); return summary;
    }
    private AmazonS3 inventorySource(String key, long size, Date modified) {
        AmazonS3 source = mock(AmazonS3.class);
        ObjectMetadata metadata = new ObjectMetadata(); metadata.setContentLength(size); metadata.setLastModified(modified);
        when(clients.getClient("source")).thenReturn(source);
        when(source.getObjectMetadata("bucket", key)).thenReturn(metadata);
        return source;
    }
}
