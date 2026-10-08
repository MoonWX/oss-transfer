package com.transfer.tools.service;

import com.transfer.tools.config.RateLimitConfig;
import com.transfer.tools.model.RateLimitAdjustment;
import com.transfer.tools.model.RateLimitRequest;
import com.transfer.tools.storage.RocksDBService;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.List;
import java.util.Map;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;

import static org.junit.Assert.*;

public class RateLimiterTransparencyTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService store;
    private RateLimiterManager manager;
    private RateLimitConfig config;

    @Before
    public void setUp() throws Exception {
        store = new RocksDBService();
        ReflectionTestUtils.setField(store, "dbPath", temporary.newFolder("rocksdb").getAbsolutePath());
        store.init();
        config = new RateLimitConfig();
        RateLimitConfig.SrcRateLimitConfig src = new RateLimitConfig.SrcRateLimitConfig();
        RateLimitConfig.SrcRateLimitConfig.QpsConfig sq = new RateLimitConfig.SrcRateLimitConfig.QpsConfig();
        sq.setList(100); src.setQps(sq); config.setSrc(src);
        RateLimitConfig.DestRateLimitConfig dest = new RateLimitConfig.DestRateLimitConfig();
        RateLimitConfig.DestRateLimitConfig.QpsConfig dq = new RateLimitConfig.DestRateLimitConfig.QpsConfig();
        dq.setPut(1000); dq.setHead(2000); dest.setQps(dq); config.setDest(dest);
        config.setBandwidth(2000000L);
        config.getAutoAdjust().setConsecutive503Threshold(2);
        config.getAutoAdjust().setReduceFactor(0.5);
        config.getAutoAdjust().setMinPutQps(200);
        config.getAutoAdjust().setIntervalMs(600000L);
        start();
    }

    @After
    public void tearDown() {
        if (manager != null) manager.destroy();
        if (store != null) store.destroy();
    }

    @Test
    public void reportsObserved503AndThenActualReductionAndEveryRecoveryStep() {
        assertEquals("CONFIG", rate("put").get("limitSource"));
        assertEquals("NORMAL", rate("put").get("state"));
        manager.onOss503SlowDown();
        assertEquals(1, auto().get("consecutive503Count"));
        assertNotNull(auto().get("last503At"));
        assertEquals(0L, auto().get("sequence"));
        manager.onOss503SlowDown();
        assertEquals(500.0, (Double) rate("put").get("limitQps"), 0.0);
        assertEquals(1000.0, (Double) rate("put").get("manualLimitQps"), 0.0);
        assertEquals("REDUCED", rate("put").get("state"));
        RateLimitAdjustment reduction = last("put");
        assertEquals("OSS_503", reduction.getReasonCode());
        assertEquals(Integer.valueOf(2), reduction.getConsecutive503Count());
        assertEquals(Integer.valueOf(2), reduction.getConsecutive503Threshold());
        assertEquals(1000.0, reduction.getPreviousValue(), 0.0);
        assertEquals(1000.0, reduction.getManualTarget(), 0.0);
        recover();
        assertEquals("RECOVERING", rate("put").get("state"));
        assertEquals(600.0, (Double) rate("put").get("limitQps"), 0.0);
        assertEquals("RECOVERY_STEP", last("put").getReasonCode());
        assertEquals(500.0, last("put").getPreviousValue(), 0.0);
        assertTrue(((Number) auto().get("nextRecoveryAt")).longValue() > ((Number) auto().get("observedAt")).longValue());
        for (int i = 0; i < 10; i++) recover();
        assertEquals("RECOVERED", rate("put").get("state"));
        assertEquals(1000.0, (Double) rate("put").get("limitQps"), 0.0);
        assertEquals("RECOVERED", last("put").getReasonCode());
        long sequence = ((Number) auto().get("sequence")).longValue();
        recover();
        assertEquals(sequence, auto().get("sequence"));
        // Recovery does not require the counter to reset and leaves LIST/bandwidth intact.
        assertEquals(2, auto().get("consecutive503Count"));
        assertEquals(100.0, (Double) rate("list").get("limitQps"), 0.0);
        assertEquals(2000000.0, (Double) rate("bandwidth").get("limitBps"), 0.0);
    }

    @Test
    public void doesNotClaimReductionWhenUnlimitedOrBelowMinimumAndDoesNotRepeatNoopUpdates() {
        manager.updatePutRateLimit(0);
        manager.updateHeadRateLimit(100);
        long before = ((Number) auto().get("sequence")).longValue();
        for (int i = 0; i < 20; i++) manager.onOss503SlowDown();
        recover();
        manager.updatePutRateLimit(0);
        manager.updateHeadRateLimit(100);
        assertEquals(before, auto().get("sequence"));
        assertEquals("UNLIMITED", rate("put").get("state"));
        assertEquals(-1.0, ((Number) rate("put").get("limitQps")).doubleValue(), 0.0);
        assertEquals("NORMAL", rate("head").get("state"));
        assertEquals(20, auto().get("consecutive503Count"));
        manager.resetConsecutive503Count();
        assertEquals(0, auto().get("consecutive503Count"));
        assertNotNull(auto().get("last503At"));
    }

    @Test
    public void reachingMinimumDoesNotGenerateFurtherReductionEvents() {
        for (int i = 0; i < 20; i++) manager.onOss503SlowDown();
        long before = ((Number) auto().get("sequence")).longValue();
        manager.onOss503SlowDown();
        assertEquals(before, auto().get("sequence"));
        assertEquals(200.0, (Double) rate("put").get("limitQps"), 0.0);
        assertEquals(200.0, (Double) rate("head").get("limitQps"), 0.0);
        assertEquals("REDUCED", rate("put").get("state"));
    }

    @Test
    public void manualUpdateAtCurrentEffectiveLimitRecordsChangedTargetAndPersistsSource() {
        manager.onOss503SlowDown(); manager.onOss503SlowDown();
        manager.updatePutRateLimit(500);
        assertEquals("NORMAL", rate("put").get("state"));
        assertEquals("PERSISTED_MANUAL", rate("put").get("limitSource"));
        assertEquals("MANUAL_SET", last("put").getReasonCode());
        assertEquals(500.0, last("put").getPreviousValue(), 0.0);
        assertEquals(1000.0, last("put").getPreviousManualTarget(), 0.0);
        assertEquals(500.0, last("put").getManualTarget(), 0.0);
        assertEquals(500.0, store.get("rate_limit:manual:put", Double.class), 0.0);
    }

    @Test
    public void restartKeepsHistoryButStartsNewProcessWithoutReplayingEvents() {
        manager.updatePutRateLimit(1200);
        manager.onOss503SlowDown(); manager.onOss503SlowDown();
        String process = (String) auto().get("processId");
        int historyCount = manager.getAdjustmentHistory(100).size();
        manager.destroy(); start();
        assertNotEquals(process, auto().get("processId"));
        assertEquals(0L, auto().get("sequence"));
        assertTrue(events().isEmpty());
        assertNull(last("put"));
        assertNull(auto().get("last503At"));
        assertEquals(0, auto().get("consecutive503Count"));
        assertEquals("NORMAL", rate("put").get("state"));
        assertEquals("PERSISTED_MANUAL", rate("put").get("limitSource"));
        assertEquals(1200.0, (Double) rate("put").get("limitQps"), 0.0);
        assertEquals(historyCount, manager.getAdjustmentHistory(100).size());
        assertEquals(process, manager.getAdjustmentHistory(100).get(0).getProcessId());
    }

    @Test
    public void legacyHistoryDoesNotInventReasonAndNewEventsAreBoundedAndOrdered() {
        store.put("rate_adjustment:1", new RateLimitAdjustment("put", 500.0, false, 1L));
        store.put("rate_adjustment:count", 1L);
        assertNull(manager.getAdjustmentHistory(1).get(0).getReasonCode());
        assertEquals(Boolean.FALSE, manager.getAdjustmentHistory(1).get(0).getDetailsAvailable());
        assertNull(manager.getAdjustmentHistory(1).get(0).getPreviousValue());
        for (int i = 0; i < 120; i++) manager.updatePutRateLimit(1001 + i);
        assertEquals(100, events().size());
        assertEquals(Long.valueOf(21), events().get(0).getSequence());
        assertEquals(Long.valueOf(120), events().get(99).getSequence());
        assertEquals(121, manager.getAdjustmentHistory(200).size());
    }

    @Test
    public void concurrent503SnapshotsKeepBothLimitsAndReasonCountsConsistent() throws Exception {
        ExecutorService pool = Executors.newFixedThreadPool(4);
        try {
            for (int i = 0; i < 50; i++) pool.submit(() -> manager.onOss503SlowDown());
            for (int i = 0; i < 50; i++) {
                Map<String, Object> snapshot = manager.getRateLimitStatus();
                Map<?, ?> put = (Map<?, ?>) snapshot.get("put");
                Map<?, ?> head = (Map<?, ?>) snapshot.get("head");
                Map<?, ?> auto = (Map<?, ?>) snapshot.get("autoAdjust");
                if (((Number) auto.get("consecutive503Count")).intValue() >= 2) {
                    assertTrue(((Number) put.get("limitQps")).doubleValue() <= 500);
                    assertTrue(((Number) head.get("limitQps")).doubleValue() <= 1000);
                    assertTrue(((RateLimitAdjustment) put.get("lastAdjustment")).getConsecutive503Count()
                            <= ((Number) auto.get("consecutive503Count")).intValue());
                }
            }
        } finally {
            pool.shutdown();
            assertTrue(pool.awaitTermination(10, TimeUnit.SECONDS));
        }
        assertEquals(50, auto().get("consecutive503Count"));
    }

    @Test
    public void unlimitedRequestsStillContributeToMeasuredThroughput() {
        manager.updateListRateLimit(0);
        manager.updatePutRateLimit(0);
        manager.updateHeadRateLimit(0);
        manager.updateBandwidthRateLimit(0);
        manager.acquireList(); manager.acquirePut(); manager.acquireHead(); manager.acquireBandwidth(1024);
        assertEquals(1, ((java.util.concurrent.atomic.AtomicLong) ReflectionTestUtils.getField(manager, "listRequestCount")).get());
        assertEquals(1, ((java.util.concurrent.atomic.AtomicLong) ReflectionTestUtils.getField(manager, "putRequestCount")).get());
        assertEquals(1, ((java.util.concurrent.atomic.AtomicLong) ReflectionTestUtils.getField(manager, "headRequestCount")).get());
        assertEquals(8192, ((java.util.concurrent.atomic.AtomicLong) ReflectionTestUtils.getField(manager, "bandwidthBitsCount")).get());
        assertNull(manager.getPutRateLimiter());
    }

    @Test
    public void historyReadFailureIsReportedWhileCurrentRateStatusRemainsAvailable() {
        RocksDBService failed = org.mockito.Mockito.mock(RocksDBService.class);
        org.mockito.Mockito.when(failed.get("rate_adjustment:count", Long.class))
                .thenThrow(new IllegalStateException("read failed"));
        ReflectionTestUtils.setField(manager, "rocksDBService", failed);
        try {
            manager.getAdjustmentHistory(20);
            fail("A database failure must not look like an empty history");
        } catch (IllegalStateException expected) {
            assertTrue(expected.getMessage().contains("历史读取失败"));
        }
        assertEquals("NORMAL", rate("put").get("state"));
        com.transfer.tools.controller.WorkerController worker = new com.transfer.tools.controller.WorkerController();
        ReflectionTestUtils.setField(worker, "rateLimiterManager", manager);
        assertEquals(Boolean.FALSE, worker.getRateLimitHistory(20).get("success"));
        ReflectionTestUtils.setField(worker, "bufferQueueManager", org.mockito.Mockito.mock(com.transfer.tools.executor.BufferQueueManager.class));
        SubTaskManagerService tasks = org.mockito.Mockito.mock(SubTaskManagerService.class);
        org.mockito.Mockito.when(tasks.getMigrationStatus(null, null)).thenReturn(new com.transfer.tools.model.MigrationStatus());
        ReflectionTestUtils.setField(worker, "subTaskManagerService", tasks);
        Map<String, Object> status = worker.getWorkerStatus();
        assertEquals(Boolean.TRUE, status.get("success"));
        assertEquals(Boolean.FALSE, status.get("rateLimitHistoryComplete"));
        assertNotNull(status.get("rateLimit"));
    }

    @Test
    public void savedTargetEqualToYamlIsPresentedAsConfigurationTarget() {
        manager.updatePutRateLimit(1000);
        assertEquals("CONFIG", rate("put").get("limitSource"));
        assertEquals(1000.0, (Double) rate("put").get("configuredLimitQps"), 0.0);
        manager.updatePutRateLimit(1500);
        assertEquals("PERSISTED_MANUAL", rate("put").get("limitSource"));
        manager.updatePutRateLimit(1000);
        manager.destroy(); start();
        assertEquals("CONFIG", rate("put").get("limitSource"));
        assertEquals(1000.0, (Double) rate("put").get("manualLimitQps"), 0.0);
    }

    @Test
    public void incidentDetailsSurviveRingEvictionAndDatabaseReopenWithTheirPolicySnapshot() {
        RateLimitRequest request = request("original-request");
        manager.onOss503SlowDown(request); manager.onOss503SlowDown(request);
        RateLimitAdjustment first = last("put");
        String id = first.getId();
        assertEquals(first.getIncidentId(), last("head").getIncidentId());
        for (int i = 0; i < 10; i++) recover();
        assertEquals(first.getIncidentId(), last("put").getIncidentId());
        assertNull(last("put").getTriggerRequest());
        for (int i = 0; i < 110; i++) manager.updateListRateLimit(101 + i);
        Map<String, Object> detail = manager.getAdjustmentDetail(id, null);
        assertEquals(Boolean.FALSE, detail.get("incidentActive"));
        assertEquals(Boolean.FALSE, detail.get("processRestarted"));
        assertEquals("original-request", ((RateLimitAdjustment) detail.get("triggerEvent")).getTriggerRequest().getRequestId());
        assertEquals(600000L, first.getIntervalMs().longValue());
        assertEquals(1.2, first.getIncreaseFactor(), 0.0);
        String path = (String) ReflectionTestUtils.getField(store, "dbPath");
        manager.destroy(); store.destroy();
        store = new RocksDBService(); ReflectionTestUtils.setField(store, "dbPath", path); store.init();
        config.getAutoAdjust().setIntervalMs(900000L);
        config.getAutoAdjust().setIncreaseFactor(1.5);
        start();
        detail = manager.getAdjustmentDetail(id, null);
        assertEquals(Boolean.TRUE, detail.get("processRestarted"));
        assertEquals(Boolean.FALSE, detail.get("incidentActive"));
        RateLimitAdjustment recorded = (RateLimitAdjustment) detail.get("event");
        assertEquals(600000L, recorded.getIntervalMs().longValue());
        assertEquals(1.2, recorded.getIncreaseFactor(), 0.0);
        assertEquals("original-request", recorded.getTriggerRequest().getRequestId());
    }

    @Test
    public void eachReductionKeepsItsOwnTriggerAndManualChangeClosesItsIncidentMembership() {
        manager.onOss503SlowDown(request("first")); manager.onOss503SlowDown(request("first"));
        RateLimitAdjustment first = last("put");
        manager.onOss503SlowDown(request("second"));
        RateLimitAdjustment second = last("put");
        assertEquals(first.getIncidentId(), second.getIncidentId());
        assertEquals("first", first.getTriggerRequest().getRequestId());
        assertEquals("second", second.getTriggerRequest().getRequestId());
        assertEquals(first.getId(), ((RateLimitAdjustment) manager.getAdjustmentDetail(second.getId(), null).get("triggerEvent")).getId());
        manager.updatePutRateLimit(300);
        assertEquals(first.getIncidentId(), last("put").getIncidentId());
        assertEquals("MANUAL_SET", last("put").getReasonCode());
        assertEquals(Boolean.TRUE, manager.getAdjustmentDetail(first.getId(), null).get("incidentActive"));
        manager.updateHeadRateLimit(600);
        assertEquals(Boolean.FALSE, manager.getAdjustmentDetail(first.getId(), null).get("incidentActive"));
        manager.onOss503SlowDown(request("third"));
        assertNotEquals(first.getIncidentId(), last("put").getIncidentId());
    }

    @Test
    public void incidentTimelineIsPagedAndRejectsACursorFromAnotherIncident() {
        config.getAutoAdjust().setConsecutive503Threshold(1);
        config.getAutoAdjust().setReduceFactor(0.999);
        for (int i = 0; i < 110; i++) manager.onOss503SlowDown(request("request-" + i));
        String id = last("put").getId();
        Map<String, Object> page1 = manager.getAdjustmentDetail(id, null);
        assertEquals(200, ((List<?>) page1.get("events")).size());
        assertEquals(Boolean.FALSE, page1.get("done"));
        String cursor = (String) page1.get("nextCursor");
        Map<String, Object> page2 = manager.getAdjustmentDetail(id, cursor);
        assertEquals(20, ((List<?>) page2.get("events")).size());
        assertEquals(Boolean.TRUE, page2.get("done"));
        manager.updatePutRateLimit(1000); manager.updateHeadRateLimit(2000);
        manager.onOss503SlowDown(request("another-incident"));
        try {
            manager.getAdjustmentDetail(last("put").getId(), cursor);
            fail("Cursor must be scoped to its own incident");
        } catch (IllegalArgumentException expected) {
            assertTrue(expected.getMessage().contains("different scope"));
        }
    }

    @Test
    public void historyWithoutADetailIndexIsExplicitlyUnavailable() {
        String legacyId = "00000000-0000-0000-0000-000000000000:1";
        RateLimitAdjustment legacy = new RateLimitAdjustment("put", 500.0, false, 1L);
        legacy.setId(legacyId);
        store.put("rate_adjustment:1", legacy); store.put("rate_adjustment:count", 1L);
        try {
            manager.getAdjustmentDetail(legacyId, null);
            fail("Legacy history must not be rewritten with current request evidence");
        } catch (IllegalArgumentException expected) {
            assertTrue(expected.getMessage().contains("未记录"));
        }
    }

    @Test
    public void failedTriggerPersistenceNeverTurnsARecoveryIntoTheTriggerEvent() {
        RocksDBService failing = org.mockito.Mockito.spy(store);
        org.mockito.Mockito.doThrow(new IllegalStateException("first trigger write failed"))
                .doThrow(new IllegalStateException("second trigger write failed"))
                .doCallRealMethod().when(failing).putBatchSync(org.mockito.ArgumentMatchers.anyMap());
        ReflectionTestUtils.setField(manager, "rocksDBService", failing);
        manager.onOss503SlowDown(request("lost")); manager.onOss503SlowDown(request("lost"));
        assertEquals(Boolean.FALSE, last("put").getDetailsAvailable());
        recover();
        Map<String, Object> details = manager.getAdjustmentDetail(last("put").getId(), null);
        assertNull(details.get("triggerEvent"));
        assertEquals(Boolean.FALSE, details.get("historyComplete"));
        assertEquals("RECOVERY_STEP", ((RateLimitAdjustment) details.get("event")).getReasonCode());
    }

    private RateLimitRequest request(String id) {
        RateLimitRequest request = new RateLimitRequest();
        request.setRequestId(id); request.setStatusCode(503); request.setErrorCode("ServiceUnavailable");
        request.setOperation("PUT_OBJECT"); request.setMethod("PUT"); request.setBucket("test-bucket");
        return request;
    }

    private void start() {
        manager = new RateLimiterManager();
        ReflectionTestUtils.setField(manager, "rateLimitConfig", config);
        ReflectionTestUtils.setField(manager, "rocksDBService", store);
        manager.init();
    }
    private void recover() { ReflectionTestUtils.invokeMethod(manager, "increaseCurrentRates"); }
    @SuppressWarnings("unchecked") private Map<String, Object> rate(String type) { return (Map<String, Object>) manager.getRateLimitStatus().get(type); }
    @SuppressWarnings("unchecked") private Map<String, Object> auto() { return (Map<String, Object>) manager.getRateLimitStatus().get("autoAdjust"); }
    @SuppressWarnings("unchecked") private List<RateLimitAdjustment> events() { return (List<RateLimitAdjustment>) auto().get("events"); }
    private RateLimitAdjustment last(String type) { return (RateLimitAdjustment) rate(type).get("lastAdjustment"); }
}
