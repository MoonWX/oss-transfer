package com.transfer.tools.service;

import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.concurrent.*;

import static org.junit.Assert.*;

public class FileReportLedgerServiceTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private String dbPath;
    private FailingStore store;
    private FileReportLedgerService ledger;

    @Before public void open() throws Exception {
        dbPath = temporary.newFolder("reports").getAbsolutePath();
        reopen();
    }
    private void reopen() {
        if (store != null) store.destroy();
        store = new FailingStore();
        ReflectionTestUtils.setField(store, "dbPath", dbPath);
        store.init();
        ledger = new FileReportLedgerService();
        ReflectionTestUtils.setField(ledger, "rocksDBService", store);
    }
    @After public void close() { if (store != null) store.destroy(); }

    @Test public void persistsResultsCoverageAndExclusiveCursorAcrossDatabaseReopen() {
        SubTaskStatus task = task(1, TransferDirection.LOCAL_TO_OSS);
        task.setParentTaskId(10);
        ledger.registerTask(task);
        ledger.scanStarted(task);
        for (String key : Arrays.asList("prefix/待处理", "prefix/success", "prefix/skipped", "prefix/failed")) {
            ledger.discovered(task, key, 12L, "/share/" + key);
        }
        ledger.finished(message(task, "prefix/success", 12L), "SUCCESS", "TRANSFERRED", null);
        ledger.skipped(task, "prefix/skipped", 12L, "/share/skipped", "TIME_FILTER");
        ledger.finished(message(task, "prefix/failed", 12L), "FAILED", "TRANSFER_FAILED", "network unavailable");
        ledger.scanCompleted(task);
        FileReportPage first = ledger.page(10, null, 2);
        assertEquals(2, first.getItems().size());
        assertFalse(first.isDone());
        String cursor = first.getNextCursor();
        long revision = first.getCoverage().getRevision();
        reopen();
        FileReportPage second = ledger.page(10, cursor, 2);
        assertTrue(second.isDone());
        assertNull(second.getNextCursor());
        List<FileReportRecord> all = new ArrayList<>(first.getItems());
        all.addAll(second.getItems());
        assertEquals(4, all.size());
        assertEquals(4, all.stream().map(FileReportRecord::getLogicalKey).distinct().count());
        Set<String> results = new HashSet<>();
        all.forEach(row -> results.add(row.getResult()));
        assertEquals(new HashSet<>(Arrays.asList("UNPROCESSED", "SUCCESS", "SKIPPED", "FAILED")), results);
        assertTrue(second.getCoverage().isScanComplete());
        assertEquals(4, second.getCoverage().getDiscoveredCount());
        assertEquals(revision, second.getCoverage().getRevision());
        assertNotNull(second.getCoverage().getScanStartedAt());
        assertNotNull(second.getCoverage().getScanCompletedAt());
    }

    @Test public void retryUpdatesOneLogicalRowAndCannotDowngradeSuccess() {
        SubTaskStatus task = task(3, TransferDirection.OSS_TO_OSS);
        ledger.registerTask(task);
        ledger.scanStarted(task);
        ledger.discovered(task, "prefix/a", 21L, null);
        ledger.finished(message(task, "prefix/a", 21L), "FAILED", "TRANSFER_FAILED", "first attempt");
        ledger.finished(message(task, "prefix/a", 21L), "SUCCESS", "TRANSFERRED", null);
        ledger.finished(message(task, "prefix/a", 21L), "FAILED", "TRANSFER_FAILED", "late stale error");
        ledger.skipped(task, "prefix/a", 21L, null, "TARGET_EXISTS");
        ledger.scanCompleted(task);
        ledger.scanStarted(task);
        ledger.discovered(task, "prefix/a", 21L, null);
        ledger.scanCompleted(task);
        FileReportPage page = ledger.page(3, null, 500);
        assertEquals(1, page.getItems().size());
        assertEquals("SUCCESS", page.getItems().get(0).getResult());
        assertNull(page.getItems().get(0).getErrorMessage());
        assertEquals(1, page.getCoverage().getRegisteredScans());
        assertEquals(1, page.getCoverage().getCompletedScans());
    }

    @Test public void retainsExactKeyAndActualPathsForAllDirectionsAndFillsHeadSize() {
        SubTaskStatus download = task(4, TransferDirection.OSS_TO_LOCAL);
        download.setLocalPath("/mount/target");
        download.setTargetRelativePath("renamed/alpha.bin");
        ledger.registerTask(download);
        ledger.discovered(download, "source/prefix/alpha.bin", null, null);
        ledger.discovered(download, "source/prefix/alpha.bin", 42L, null);
        FileReportRecord row = ledger.page(4, null, 1).getItems().get(0);
        assertEquals(Long.valueOf(42), row.getSize());
        assertEquals("source/prefix/alpha.bin", row.getSourceKey());
        assertEquals("/mount/target/renamed/alpha.bin", row.getDestinationPath());
        assertNull(row.getDestinationKey());
        SubTaskStatus upload = task(5, TransferDirection.LOCAL_TO_OSS);
        ledger.registerTask(upload);
        ledger.discovered(upload, "dest-prefix/exact-name", 13L, "/mount/source/real.bin");
        row = ledger.page(5, null, 1).getItems().get(0);
        assertEquals("/mount/source/real.bin", row.getSourcePath());
        assertEquals("dest-prefix/exact-name", row.getDestinationKey());
        SubTaskStatus copy = task(6, TransferDirection.OSS_TO_OSS);
        ledger.registerTask(copy);
        ledger.discovered(copy, "unchanged/key", 19L, null);
        row = ledger.page(6, null, 1).getItems().get(0);
        assertEquals(row.getSourceKey(), row.getDestinationKey());
        assertEquals("source-bucket", row.getSourceBucket());
        assertEquals("destination-bucket", row.getDestinationBucket());
    }

    @Test public void mappedResultsRetainSourceIdentityAcrossRetryAndDatabaseReopen() {
        SubTaskStatus copy = task(60, TransferDirection.OSS_TO_OSS);
        OssKeyMapping mapping = new OssKeyMapping();
        mapping.setMode(OssKeyMapping.Mode.REPLACE_PREFIX);
        mapping.setSourcePrefix("原目录/"); mapping.setTargetPrefix("archive/");
        copy.setKeyMapping(mapping);
        ledger.registerTask(copy); ledger.scanStarted(copy);
        String key = "原目录/子目录//100% 文件.bin";
        ledger.discovered(copy, key, 21L, null);
        FileReportRecord row = ledger.page(60, null, 10).getItems().get(0);
        assertEquals(key, row.getLogicalKey()); assertEquals(key, row.getSourceKey());
        assertEquals("archive/子目录//100% 文件.bin", row.getDestinationKey());
        ledger.finished(message(copy, key, 21L), "FAILED", "TRANSFER_FAILED", "retryable");
        reopen();
        ledger.finished(message(copy, key, 21L), "SUCCESS", "TRANSFERRED", null);
        ledger.skipped(copy, key, 21L, null, "TARGET_EXISTS");
        FileReportPage page = ledger.page(60, null, 10);
        assertEquals(1, page.getItems().size());
        row = page.getItems().get(0);
        assertEquals("SUCCESS", row.getResult()); assertEquals(key, row.getLogicalKey());
        assertEquals("archive/子目录//100% 文件.bin", row.getDestinationKey());
        assertEquals(1, page.getCoverage().getDiscoveredCount());
    }

    @Test public void exactAndAddedPrefixResultsUseActualDestinationWhileInvalidKeyRemainsReportable() {
        SubTaskStatus copy = task(61, TransferDirection.OSS_TO_OSS);
        OssKeyMapping mapping = new OssKeyMapping(); mapping.setMode(OssKeyMapping.Mode.EXACT_KEY);
        mapping.setTargetKey("renamed.dat"); copy.setKeyMapping(mapping);
        ledger.registerTask(copy);
        ledger.finished(message(copy, "source.dat", 8L), "SUCCESS", "TRANSFERRED", null);
        assertEquals("renamed.dat", ledger.page(61, null, 10).getItems().get(0).getDestinationKey());
        copy = task(62, TransferDirection.OSS_TO_OSS);
        mapping = new OssKeyMapping(); mapping.setMode(OssKeyMapping.Mode.ADD_PREFIX);
        mapping.setTargetPrefix("backup"); copy.setKeyMapping(mapping); ledger.registerTask(copy);
        ledger.skipped(copy, "/source//file", 8L, null, "TARGET_EXISTS");
        assertEquals("backup//source//file", ledger.page(62, null, 10).getItems().get(0).getDestinationKey());
        copy = task(63, TransferDirection.OSS_TO_OSS);
        mapping = new OssKeyMapping(); mapping.setMode(OssKeyMapping.Mode.REPLACE_PREFIX);
        mapping.setSourcePrefix("source/"); mapping.setTargetPrefix(""); copy.setKeyMapping(mapping);
        ledger.registerTask(copy);
        ledger.failed(copy, "source/", 0L, null, "INVALID_TARGET_KEY", "empty destination");
        FileReportRecord row = ledger.page(63, null, 10).getItems().get(0);
        assertEquals("FAILED", row.getResult()); assertEquals("source/", row.getSourceKey());
        assertNull(row.getDestinationKey());
    }

    @Test public void legacyRetentionConvertsInBoundedAdvancingPagesWithoutPretendingCompleteness() {
        Map<String, Object> old = new LinkedHashMap<>();
        for (int i = 0; i < 502; i++) {
            String key = "old-" + i;
            old.put("completed:7:" + encode(key), new CompletedItemRecord(7, TransferDirection.OSS_TO_OSS, key, 1L, 123L));
        }
        FailedKey failure = new FailedKey();
        failure.setTaskIndex(7);
        failure.setKey("old-1");
        failure.setTimestamp(90L);
        failure.setDirection(TransferDirection.OSS_TO_OSS);
        old.put("failed:1", failure);
        old.put("failed:count", 1L);
        store.putBatchSync(old);
        FileReportPage page = ledger.page(7, null, 11);
        assertTrue(page.getItems().isEmpty());
        String first = page.getNextCursor();
        assertNotNull(first);
        page = ledger.page(7, first, 11);
        assertTrue(page.getItems().isEmpty());
        assertNotEquals(first, page.getNextCursor());
        reopen();
        Set<String> keys = new HashSet<>();
        String previousOrder = null;
        while (!page.isDone()) {
            page = ledger.page(7, page.getNextCursor(), 11);
            assertTrue(page.getItems().size() <= 11);
            for (FileReportRecord row : page.getItems()) {
                assertEquals("SUCCESS", row.getResult());
                String order = encode(row.getLogicalKey());
                if (previousOrder != null) assertTrue(previousOrder.compareTo(order) < 0);
                previousOrder = order;
                assertTrue(keys.add(row.getLogicalKey()));
            }
        }
        assertEquals(502, keys.size());
        assertEquals("LEGACY_UNKNOWN", page.getCoverage().getLedgerCoverage());
        assertFalse(page.getCoverage().isScanComplete());
    }

    @Test public void retryOfHistoricalTaskCannotUpgradeCoverageToTracked() {
        SubTaskStatus task = task(8, TransferDirection.OSS_TO_OSS);
        store.putSync("task:8", task);
        ledger.registerTask(task);
        ledger.scanStarted(task);
        ledger.discovered(task, "only-selected-failure", 1L, null);
        ledger.scanCompleted(task);
        assertEquals("LEGACY_UNKNOWN", ledger.coverage(8).getLedgerCoverage());
        assertFalse(ledger.coverage(8).isScanComplete());
    }

    @Test public void failedAtomicWriteDoesNotInventRowOrCountAndPersistsCoverageGap() {
        SubTaskStatus task = task(9, TransferDirection.LOCAL_TO_OSS);
        ledger.registerTask(task);
        ledger.scanStarted(task);
        store.failBatch = true;
        try {
            ledger.discovered(task, "not-enqueued", 1L, "/file");
            fail("write should fail");
        } catch (FileReportLedgerService.FileReportPersistenceException expected) {
            assertNotNull(expected.getCause());
        }
        store.failBatch = false;
        assertEquals(0, ledger.coverage(9).getDiscoveredCount());
        assertEquals("PARTIAL", ledger.coverage(9).getLedgerCoverage());
        reopen();
        FileReportPage page = ledger.page(9, null, 100);
        assertTrue(page.getItems().isEmpty());
        assertEquals("PARTIAL", page.getCoverage().getLedgerCoverage());
        assertFalse(page.getCoverage().isScanComplete());
    }

    @Test public void concurrentResultsCannotLoseCountsOrSuccess() throws Exception {
        SubTaskStatus task = task(10, TransferDirection.OSS_TO_OSS);
        ledger.registerTask(task);
        ExecutorService workers = Executors.newFixedThreadPool(4);
        try {
            List<Future<?>> futures = new ArrayList<>();
            for (int i = 0; i < 32; i++) {
                final String key = "key-" + i;
                futures.add(workers.submit(() -> {
                    ledger.discovered(task, key, 1L, null);
                    ledger.finished(message(task, key, 1L), "SUCCESS", "TRANSFERRED", null);
                    ledger.finished(message(task, key, 1L), "FAILED", "STALE", "late");
                }));
            }
            for (Future<?> future : futures) future.get(10, TimeUnit.SECONDS);
        } finally { workers.shutdownNow(); }
        FileReportPage page = ledger.page(10, null, 100);
        assertEquals(32, page.getItems().size());
        assertEquals(32, page.getCoverage().getDiscoveredCount());
        for (FileReportRecord row : page.getItems()) assertEquals("SUCCESS", row.getResult());
    }

    @Test public void cursorCannotCrossTaskScopesAndCorruptRecordsAreNotSilentlyDropped() {
        SubTaskStatus task = task(11, TransferDirection.OSS_TO_OSS);
        ledger.registerTask(task);
        ledger.discovered(task, "a", 1L, null);
        ledger.discovered(task, "b", 1L, null);
        String cursor = ledger.page(11, null, 1).getNextCursor();
        ledger.registerTask(task(12, TransferDirection.OSS_TO_OSS));
        try { ledger.page(12, cursor, 1); fail("wrong scope"); }
        catch (IllegalArgumentException expected) { assertTrue(expected.getMessage().contains("scope")); }
        store.putSync("file-report:11:item:" + encode("a"), "bad record");
        try { ledger.page(11, null, 1); fail("bad record must fail explicitly"); }
        catch (RuntimeException expected) { assertNotNull(expected); }
    }

    @Test public void malformedCursorIsRejectedBeforeAnyLegacyBackfillMutation() {
        for (String cursor : Arrays.asList("", " ", "legacy:99:0", "legacy:99:abc", "legacy:98:1", "%%invalid%%")) {
            try { ledger.page(99, cursor, 100); fail("invalid cursor must fail: " + cursor); }
            catch (IllegalArgumentException expected) { assertNotNull(expected.getMessage()); }
            assertNull(store.get("file-report:99:coverage", ReportCoverage.class));
            assertNull(store.get("file-report:99:legacy", FileReportLedgerService.LegacyState.class));
        }
    }

    @Test public void historicalPageUsesOneSyncBatchForAtMost500RowsAndItsCheckpoint() {
        seedCompleted(20, 501);
        store.batchCalls = 0;
        FileReportPage first = ledger.page(20, null, 50);
        assertTrue(first.getItems().isEmpty());
        assertEquals(1, store.batchCalls);
        assertEquals(502, store.lastBatch.size());
        assertTrue(store.lastBatch.containsKey("file-report:20:coverage"));
        assertTrue(store.lastBatch.containsKey("file-report:20:legacy"));
        assertEquals(500, first.getCoverage().getDiscoveredCount());
        assertEquals(500, store.scanPage("file-report:20:item:", null, 1000, FileReportRecord.class).getItems().size());
        ledger.page(20, first.getNextCursor(), 50);
        assertEquals(2, store.batchCalls);
        assertEquals(3, store.lastBatch.size());
        assertEquals(501, ledger.coverage(20).getDiscoveredCount());
    }

    @Test public void failedHistoricalPageLeavesRowsCountsAndCheckpointUnadvancedAndRestartsSafely() {
        seedCompleted(21, 502);
        store.failBatch = true;
        try { ledger.page(21, null, 1000); fail("first batch must fail atomically"); }
        catch (FileReportLedgerService.FileReportPersistenceException expected) { assertNotNull(expected.getCause()); }
        assertEquals(0, ledger.coverage(21).getDiscoveredCount());
        assertTrue(store.scanPage("file-report:21:item:", null, 1000, FileReportRecord.class).getItems().isEmpty());
        assertNull(store.get("file-report:21:legacy", FileReportLedgerService.LegacyState.class));
        assertEquals("PARTIAL", ledger.coverage(21).getLedgerCoverage());

        reopen();
        FileReportPage first = ledger.page(21, null, 1000);
        assertEquals(500, first.getCoverage().getDiscoveredCount());
        FileReportLedgerService.LegacyState checkpoint = store.get("file-report:21:legacy", FileReportLedgerService.LegacyState.class);
        assertFalse(checkpoint.isCompletedDone());
        store.failBatch = true;
        try { ledger.page(21, first.getNextCursor(), 1000); fail("second batch must fail atomically"); }
        catch (FileReportLedgerService.FileReportPersistenceException expected) { assertNotNull(expected.getCause()); }
        assertEquals(500, ledger.coverage(21).getDiscoveredCount());
        assertEquals(500, store.scanPage("file-report:21:item:", null, 1000, FileReportRecord.class).getItems().size());
        assertEquals(checkpoint.getCompletedCursor(), store.get("file-report:21:legacy",
                FileReportLedgerService.LegacyState.class).getCompletedCursor());

        reopen();
        FileReportPage second = ledger.page(21, first.getNextCursor(), 1000);
        assertEquals(502, second.getCoverage().getDiscoveredCount());
        FileReportPage done = ledger.page(21, second.getNextCursor(), 1000);
        assertTrue(done.isDone());
        assertEquals(502, done.getItems().size());
        assertEquals(502, done.getItems().stream().map(FileReportRecord::getLogicalKey).distinct().count());
        assertEquals("PARTIAL", done.getCoverage().getLedgerCoverage());
        assertFalse(done.getCoverage().isScanComplete());
        long revision = done.getCoverage().getRevision();
        assertEquals(revision, ledger.page(21, null, 1000).getCoverage().getRevision());
    }

    @Test public void historicalBatchMergesSamePageDuplicatesAndPriorSuccessWithoutDoubleCounting() {
        SubTaskStatus task = task(22, TransferDirection.OSS_TO_OSS);
        // Simulate an old-version conversion interrupted after a success row but before its checkpoint.
        ledger.finished(message(task, "success", 12), "SUCCESS", "LEGACY_RETAINED_SUCCESS", null);
        Map<String, Object> old = new LinkedHashMap<>();
        old.put("completed:22:" + encode("success"),
                new CompletedItemRecord(22, TransferDirection.OSS_TO_OSS, "success", 12L, 99L));
        old.put("failed:1", legacyFailure(22, "success", 400L, "must not downgrade", 12L));
        old.put("failed:2", legacyFailure(22, "duplicate", 200L, "first", 7L));
        old.put("failed:3", legacyFailure(22, "duplicate", 100L, "older", 3L));
        old.put("failed:4", legacyFailure(22, "duplicate", 300L, "latest", null));
        old.put("failed:count", 4L);
        store.putBatchSync(old);
        store.batchCalls = 0;
        FileReportPage first = ledger.page(22, null, 100);
        FileReportPage done = ledger.page(22, first.getNextCursor(), 100);
        assertTrue(done.isDone());
        assertEquals(2, store.batchCalls);
        assertEquals(3, store.lastBatch.size()); // one distinct failure, coverage and checkpoint
        assertEquals(2, done.getCoverage().getDiscoveredCount());
        assertEquals(2, done.getItems().size());
        for (FileReportRecord row : done.getItems()) {
            if ("success".equals(row.getLogicalKey())) {
                assertEquals("SUCCESS", row.getResult());
                assertNull(row.getErrorMessage());
            } else {
                assertEquals("FAILED", row.getResult());
                assertEquals("latest", row.getErrorMessage());
                assertEquals(Long.valueOf(300), row.getCompletedAt());
                assertEquals(Long.valueOf(7), row.getSize());
            }
        }
        assertEquals("LEGACY_UNKNOWN", done.getCoverage().getLedgerCoverage());
    }

    private void seedCompleted(int taskId, int count) {
        Map<String, Object> completed = new LinkedHashMap<>();
        for (int i = 0; i < count; i++) {
            String key = "item-" + i;
            completed.put("completed:" + taskId + ":" + encode(key),
                    new CompletedItemRecord(taskId, TransferDirection.OSS_TO_OSS, key, 1L, 123L));
        }
        store.putBatchSync(completed);
    }

    private FailedKey legacyFailure(int taskId, String key, long timestamp, String error, Long size) {
        FailedKey row = new FailedKey();
        row.setTaskIndex(taskId); row.setKey(key); row.setTimestamp(timestamp);
        row.setErrorMessage(error); row.setObjectSize(size); row.setDirection(TransferDirection.OSS_TO_OSS);
        return row;
    }

    private static String encode(String key) {
        return Base64.getUrlEncoder().withoutPadding().encodeToString(key.getBytes(StandardCharsets.UTF_8));
    }
    private SubTaskStatus task(int id, TransferDirection direction) {
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(id); task.setDirection(direction); task.setShardId("shard-" + id);
        task.setSourceBucket("source-bucket"); task.setSourceS3Alias("source-connection");
        task.setDestBucket("destination-bucket"); task.setDestS3Alias("destination-connection");
        task.setExecutionNodeIp("worker-1");
        return task;
    }
    private ObjectListMessage message(SubTaskStatus task, String key, long size) {
        ObjectListMessage message = new ObjectListMessage(null, task.getTaskIndex(), false,
                task.getSourceBucket(), task.getDestBucket(), task.getSourceS3Alias(), task.getDestS3Alias(), false, key);
        message.setDirection(task.getDirection()); message.setParentTaskId(task.getParentTaskId());
        message.setKeyMapping(task.getKeyMapping());
        message.setShardId(task.getShardId()); message.setSourceSize(size);
        message.setLocalPath("/share/" + key); message.setExecutionNodeIp(task.getExecutionNodeIp());
        return message;
    }
    private static class FailingStore extends RocksDBService {
        boolean failBatch;
        int batchCalls;
        Map<String, Object> lastBatch;
        @Override public void putBatchSync(Map<String, Object> values) {
            batchCalls++;
            lastBatch = new LinkedHashMap<>(values);
            if (failBatch) throw new IllegalStateException("simulated disk write failure");
            super.putBatchSync(values);
        }
    }
}
