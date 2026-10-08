package com.transfer.tools.service;

import com.transfer.tools.config.RateLimitConfig;
import com.transfer.tools.controller.WorkerController;
import com.transfer.tools.model.RateLimitAdjustment;
import com.transfer.tools.storage.RocksDBService;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.rocksdb.RocksDB;
import org.springframework.test.util.ReflectionTestUtils;

import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class RateLimiterPersistenceTest {
    private static final String PREFIX = "rate_limit:manual:";
    private static final List<String> TYPES = Arrays.asList("list", "put", "head", "bandwidth");
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private String dbPath;
    private RocksDBService store;
    private RateLimiterManager manager;

    @Before
    public void openDatabase() throws Exception {
        dbPath = temporary.newFolder("rocksdb").getAbsolutePath();
        store = openStore();
        startManager(config(10, 20, 30, 4000));
    }

    @After
    public void closeDatabase() {
        stopManager();
        if (store != null) store.destroy();
    }

    @Test
    public void restoresAllManualTargetsAfterClosingAndReopeningDatabase() {
        double[] targets = {11.5, 22.25, 33.75, 987654321};
        for (int i = 0; i < TYPES.size(); i++) update(TYPES.get(i), targets[i], true);

        restart(config(100, 200, 300, 400000));

        for (int i = 0; i < TYPES.size(); i++) {
            assertEquals(targets[i], current(TYPES.get(i)), 0.001);
            assertEquals(targets[i], store.get(PREFIX + TYPES.get(i), Double.class), 0.001);
        }
        assertEquals(4, manager.getAdjustmentHistory(100).size());
    }

    @Test
    public void missingOverridesUseYamlPerTypeAndDoNotReplayOldHistory() {
        store.put("rate_adjustment:1", new RateLimitAdjustment("put", 9999.0, true, 1L));
        store.put("rate_adjustment:count", 1L);
        update("head", 12.5, true);

        restart(config(101, 202, 303, 404000));

        assertEquals(101, current("list"), 0.001);
        assertEquals(202, current("put"), 0.001);
        assertEquals(12.5, current("head"), 0.001);
        assertEquals(404000, current("bandwidth"), 0.001);
        assertFalse(store.exists(PREFIX + "put"));
    }

    @Test
    public void persistsUnlimitedForAllTypesIncludingLegacyNegativeInput() {
        for (String type : TYPES) update(type, "put".equals(type) ? -1 : 0, true);
        for (RateLimitAdjustment adjustment : manager.getAdjustmentHistory(100)) {
            assertEquals(Boolean.TRUE, adjustment.getManual());
            assertEquals(0, adjustment.getValue(), 0.0);
        }

        restart(config(101, 202, 303, 404000));

        for (String type : TYPES) {
            assertEquals(-1, current(type), 0.0); // Existing status API represents unlimited as -1.
            assertEquals(0, store.get(PREFIX + type, Double.class), 0.0);
            update(type, 100, false); // A stale automatic update cannot re-enable a disabled limit.
            assertEquals(-1, current(type), 0.0);
        }
        ReflectionTestUtils.invokeMethod(manager, "increaseCurrentRates");
        for (String type : TYPES) assertEquals(-1, current(type), 0.0);
    }

    @Test
    public void automaticReductionAndRecoveryDoNotReplaceSavedTargets() {
        double[] targets = {100, 200, 300, 400000};
        for (int i = 0; i < TYPES.size(); i++) {
            update(TYPES.get(i), targets[i], true);
            update(TYPES.get(i), targets[i] / 2, false);
        }
        ReflectionTestUtils.invokeMethod(manager, "increaseCurrentRates");
        for (int i = 0; i < TYPES.size(); i++) {
            assertEquals(targets[i] * 0.6, current(TYPES.get(i)), 0.001);
            assertEquals(targets[i], store.get(PREFIX + TYPES.get(i), Double.class), 0.001);
        }

        restart(config(1, 2, 3, 4));

        for (int i = 0; i < TYPES.size(); i++) assertEquals(targets[i], current(TYPES.get(i)), 0.001);
    }

    @Test
    public void staleAutomaticUpdateCannotExceedNewManualTarget() {
        update("put", 500, true);
        update("put", 50, true);
        update("put", 450, false);
        assertEquals(50, current("put"), 0.001);
        assertEquals(50, store.get(PREFIX + "put", Double.class), 0.001);
    }

    @Test
    public void failedPersistenceReturnsWorkerFailureWithoutChangingAnyLimit() {
        double[] targets = {12, 23, 34, 4500};
        for (int i = 0; i < TYPES.size(); i++) update(TYPES.get(i), targets[i], true);
        RocksDBService failing = spy(store);
        doThrow(new IllegalStateException("simulated disk write failure"))
                .when(failing).putSync(startsWith(PREFIX), any());
        ReflectionTestUtils.setField(manager, "rocksDBService", failing);
        WorkerController worker = new WorkerController();
        ReflectionTestUtils.setField(worker, "rateLimiterManager", manager);
        for (int i = 0; i < TYPES.size(); i++) {
            String type = TYPES.get(i);
            for (double requested : new double[]{0, 999}) {
                Map<String, Object> response = worker.updateRateLimit(type, Collections.singletonMap("qps", requested));
                assertEquals(Boolean.FALSE, response.get("success"));
                assertTrue(String.valueOf(response.get("message")).contains("原限速未改变"));
                assertEquals(targets[i], current(type), 0.001);
                assertEquals(targets[i], store.get(PREFIX + type, Double.class), 0.001);
            }
        }
        restart(config(1, 2, 3, 4));
        for (int i = 0; i < TYPES.size(); i++) assertEquals(targets[i], current(TYPES.get(i)), 0.001);
    }

    @Test
    public void corruptRecordsAbortStartupBeforeBackgroundThreadsStart() throws Exception {
        stopManager();
        RocksDB database = (RocksDB) ReflectionTestUtils.getField(store, "rocksDB");
        for (String badJson : Arrays.asList("-2", "null", "\"not-a-number\"", "{broken")) {
            database.put((PREFIX + "put").getBytes(StandardCharsets.UTF_8), badJson.getBytes(StandardCharsets.UTF_8));
            manager = newManager(config(100, 200, 300, 400000));
            try {
                manager.init();
                fail("corrupt saved limit must not silently fall back to YAML");
            } catch (IllegalStateException expected) {
                assertTrue(expected.getMessage().contains("put"));
                assertNull(ReflectionTestUtils.getField(manager, "statThread"));
                assertNull(ReflectionTestUtils.getField(manager, "autoAdjustThread"));
            }
        }
    }

    @Test
    public void readFailureAbortsStartupInsteadOfUsingYaml() {
        stopManager();
        RocksDBService failing = mock(RocksDBService.class);
        when(failing.get(PREFIX + "list", Double.class)).thenThrow(new IllegalStateException("simulated read failure"));
        manager = newManager(config(100, 200, 300, 400000));
        ReflectionTestUtils.setField(manager, "rocksDBService", failing);
        try {
            manager.init();
            fail("unreadable saved limit must abort startup");
        } catch (IllegalStateException expected) {
            assertTrue(expected.getMessage().contains("list"));
            assertNull(ReflectionTestUtils.getField(manager, "statThread"));
            assertNull(ReflectionTestUtils.getField(manager, "autoAdjustThread"));
        }
    }

    @Test
    public void concurrentUpdatesKeepEveryTypeAndItsLastAcknowledgedValue() throws Exception {
        ExecutorService executor = Executors.newFixedThreadPool(12);
        CountDownLatch ready = new CountDownLatch(12);
        CountDownLatch start = new CountDownLatch(1);
        List<Future<?>> updates = new ArrayList<>();
        try {
            for (int i = 0; i < 12; i++) {
                final String type = TYPES.get(i % TYPES.size());
                final double value = 100 + i;
                updates.add(executor.submit(() -> {
                    ready.countDown();
                    try {
                        start.await();
                    } catch (InterruptedException error) {
                        Thread.currentThread().interrupt();
                        throw new RuntimeException(error);
                    }
                    update(type, value, true);
                }));
            }
            assertTrue(ready.await(5, TimeUnit.SECONDS));
            start.countDown();
            for (Future<?> update : updates) update.get(10, TimeUnit.SECONDS);
        } finally {
            start.countDown();
            executor.shutdownNow();
            assertTrue(executor.awaitTermination(5, TimeUnit.SECONDS));
        }
        double[] acknowledged = new double[4];
        for (int i = 0; i < TYPES.size(); i++) {
            acknowledged[i] = current(TYPES.get(i));
            assertEquals(acknowledged[i], store.get(PREFIX + TYPES.get(i), Double.class), 0.001);
        }
        assertEquals(12, manager.getAdjustmentHistory(100).size());

        restart(config(1, 2, 3, 4));

        for (int i = 0; i < TYPES.size(); i++) assertEquals(acknowledged[i], current(TYPES.get(i)), 0.001);
    }

    @Test
    public void rejectsNonfiniteRatesWithoutChangingMemoryOrDatabase() {
        update("put", 25, true);
        for (double value : new double[]{Double.NaN, Double.POSITIVE_INFINITY, Double.NEGATIVE_INFINITY}) {
            try {
                update("put", value, true);
                fail("nonfinite rate must be rejected");
            } catch (IllegalArgumentException expected) {
                assertEquals(25, current("put"), 0.001);
                assertEquals(25, store.get(PREFIX + "put", Double.class), 0.001);
            }
        }
    }

    private void update(String type, double rate, boolean manual) {
        boolean success;
        switch (type) {
            case "list": success = manager.updateListRateLimit(rate, manual); break;
            case "put": success = manager.updatePutRateLimit(rate, manual); break;
            case "head": success = manager.updateHeadRateLimit(rate, manual); break;
            case "bandwidth": success = manager.updateBandwidthRateLimit(rate, manual); break;
            default: throw new AssertionError(type);
        }
        assertTrue(success);
    }

    private double current(String type) {
        Map<?, ?> status = (Map<?, ?>) manager.getRateLimitStatus().get(type);
        return ((Number) status.get("bandwidth".equals(type) ? "limitBps" : "limitQps")).doubleValue();
    }

    private RocksDBService openStore() {
        RocksDBService result = new RocksDBService();
        ReflectionTestUtils.setField(result, "dbPath", dbPath);
        result.init();
        return result;
    }

    private RateLimiterManager newManager(RateLimitConfig config) {
        RateLimiterManager result = new RateLimiterManager();
        ReflectionTestUtils.setField(result, "rateLimitConfig", config);
        ReflectionTestUtils.setField(result, "rocksDBService", store);
        return result;
    }

    private void startManager(RateLimitConfig config) {
        manager = newManager(config);
        manager.init();
    }

    private void stopManager() {
        if (manager == null) return;
        Thread stat = (Thread) ReflectionTestUtils.getField(manager, "statThread");
        Thread auto = (Thread) ReflectionTestUtils.getField(manager, "autoAdjustThread");
        manager.destroy();
        assertTrue(stat == null || !stat.isAlive());
        assertTrue(auto == null || !auto.isAlive());
        manager = null;
    }

    private void restart(RateLimitConfig config) {
        stopManager();
        store.destroy();
        store = openStore();
        startManager(config);
    }

    private RateLimitConfig config(int list, int put, int head, long bandwidth) {
        RateLimitConfig result = new RateLimitConfig();
        RateLimitConfig.SrcRateLimitConfig source = new RateLimitConfig.SrcRateLimitConfig();
        RateLimitConfig.SrcRateLimitConfig.QpsConfig sourceQps = new RateLimitConfig.SrcRateLimitConfig.QpsConfig();
        sourceQps.setList(list);
        source.setQps(sourceQps);
        result.setSrc(source);
        RateLimitConfig.DestRateLimitConfig destination = new RateLimitConfig.DestRateLimitConfig();
        RateLimitConfig.DestRateLimitConfig.QpsConfig destinationQps = new RateLimitConfig.DestRateLimitConfig.QpsConfig();
        destinationQps.setPut(put);
        destinationQps.setHead(head);
        destination.setQps(destinationQps);
        result.setDest(destination);
        result.setBandwidth(bandwidth);
        result.getAutoAdjust().setIntervalMs(600000L);
        return result;
    }
}
