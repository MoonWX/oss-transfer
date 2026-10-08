package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import java.util.concurrent.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.mockito.ArgumentMatchers.*;

public class WorkerTaskBandwidthServiceTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService db;
    private WorkerTaskBandwidthService service;
    private LocalTaskControlService control;
    private ExecutorService executor;

    @Before public void setup() throws Exception {
        db = spy(new RocksDBService());
        ReflectionTestUtils.setField(db, "dbPath", temporary.newFolder("db").getAbsolutePath()); db.init();
        control = new LocalTaskControlService(); executor = Executors.newFixedThreadPool(4);
        service = instance();
    }
    @After public void close() { executor.shutdownNow(); db.destroy(); Thread.interrupted(); }

    private WorkerTaskBandwidthService instance() {
        WorkerTaskBandwidthService result = new WorkerTaskBandwidthService();
        DistributedConfig config = mock(DistributedConfig.class); when(config.getLocalIp()).thenReturn("one");
        ReflectionTestUtils.setField(result, "rocksDBService", db); ReflectionTestUtils.setField(result, "distributedConfig", config);
        ReflectionTestUtils.setField(result, "control", control); result.restore(); return result;
    }

    @Test public void appliedAndClearedPoliciesSurviveDatabaseReopenAndReadsAreDefensive() {
        service.apply(policy(1, 8.0));
        TaskBandwidthPolicy read = service.get("local:7"); read.setLimitMbps(99.0); read.getMembers().clear();
        assertEquals(Double.valueOf(8), service.get("local:7").getLimitMbps());
        db.destroy(); db.init(); service = instance();
        assertEquals(Double.valueOf(8), service.get("local:7").getLimitMbps());
        service.apply(policy(2, null)); db.destroy(); db.init(); service = instance();
        assertNull(service.get("local:7").getLimitMbps()); assertEquals(2, service.get("local:7").getRevision());
    }

    @Test public void failedPersistenceAndStaleOrConflictingRevisionCannotChangeRunningPolicy() {
        service.apply(policy(2, 1.0)); service.apply(policy(2, 1.0));
        for (TaskBandwidthPolicy invalid : Arrays.asList(policy(1, 0.5), policy(2, 2.0))) {
            try { service.apply(invalid); fail(); } catch (IllegalArgumentException expected) { }
        }
        doThrow(new IllegalStateException("disk full")).when(db).putSync(anyString(), any());
        try { service.apply(policy(3, null)); fail(); } catch (IllegalStateException expected) { }
        assertEquals(Double.valueOf(1), service.get("local:7").getLimitMbps());
        assertEquals(2, service.get("local:7").getRevision());
    }

    @Test public void explicitUnknownAndNonmemberScopesFailClosedButUntouchedLegacyIsUnlimited() {
        ObjectListMessage message = message();
        try { service.check(service.scope(message)); fail(); }
        catch (TransferValidationException expected) { assertTrue(expected.getMessage().contains("尚未同步")); }
        message.setTaskBandwidthScopeId(null); service.acquire(service.scope(message), 100000);
        TaskBandwidthPolicy foreign = policy(1, 1.0); foreign.setMembers(Collections.singletonList("other")); service.apply(foreign);
        try { service.acquire(service.scope(message), 1); fail(); }
        catch (TransferValidationException expected) { assertTrue(expected.getMessage().contains("参与节点")); }
        foreign.setRevision(2); foreign.setLimitMbps(null); service.apply(foreign); service.acquire(service.scope(message), 100000);
    }

    @Test(timeout = 5000) public void concurrentShardsShareOneNodeFractionOfTheTotalBudget() throws Exception {
        service.apply(policy(1, 1.6)); // two fixed members => this node gets 100,000 bytes/s.
        CountDownLatch ready = new CountDownLatch(4), start = new CountDownLatch(1);
        List<Future<?>> futures = new ArrayList<>();
        for (int i = 0; i < 4; i++) {
            final int id = -1 - i;
            futures.add(executor.submit(() -> {
                ObjectListMessage message = message(); message.setTaskIndex(id); ready.countDown();
                try { start.await(); } catch (InterruptedException error) { throw new RuntimeException(error); }
                service.acquire(service.scope(message), 25000);
            }));
        }
        assertTrue(ready.await(1, TimeUnit.SECONDS)); long begin = System.nanoTime(); start.countDown();
        for (Future<?> future : futures) future.get(3, TimeUnit.SECONDS);
        long elapsed = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - begin);
        assertTrue("aggregate budget was bypassed: " + elapsed, elapsed >= 850);
    }

    @Test(timeout = 5000) public void increasingAndClearingLimitsWakeExistingWaitersWithoutReplacingBucket() throws Exception {
        service.apply(policy(1, 0.1)); Object original = ((Map) ReflectionTestUtils.getField(service, "buckets")).get("local:7");
        Future<?> waiting = executor.submit(() -> service.acquire(service.scope(message()), 100000));
        Thread.sleep(120); assertFalse(waiting.isDone());
        service.apply(policy(2, 100.0)); waiting.get(1, TimeUnit.SECONDS);
        assertSame(original, ((Map) ReflectionTestUtils.getField(service, "buckets")).get("local:7"));
        service.apply(policy(3, 0.1));
        Future<?> cleared = executor.submit(() -> service.acquire(service.scope(message()), 100000));
        Thread.sleep(120); assertFalse(cleared.isDone());
        service.apply(policy(4, null)); cleared.get(1, TimeUnit.SECONDS);
        assertSame(original, ((Map) ReflectionTestUtils.getField(service, "buckets")).get("local:7"));
    }

    @Test(timeout = 5000) public void loweringLimitDoesNotResetCreditAndStoppedLargeReadExitsPromptly() throws Exception {
        service.apply(policy(1, 100.0)); service.apply(policy(2, 0.1));
        CountDownLatch started = new CountDownLatch(1);
        Future<?> waiting = executor.submit(() -> {
            try (LocalTaskControlService.Operation ignored = control.begin(-1, 7)) {
                started.countDown(); service.acquire(service.scope(message()), Integer.MAX_VALUE);
            }
        });
        assertTrue(started.await(1, TimeUnit.SECONDS)); Thread.sleep(120); assertFalse(waiting.isDone());
        long stopped = System.nanoTime(); control.stop(7);
        try { waiting.get(500, TimeUnit.MILLISECONDS); fail(); }
        catch (ExecutionException expected) { assertTrue(expected.getCause() instanceof TaskCancelledException); }
        assertTrue(TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - stopped) < 500);
        assertEquals(Double.valueOf(0.1), service.get("local:7").getLimitMbps());
    }

    @Test public void hotPathNeverReadsDatabaseAndCorruptStoredPolicyFailsRestore() {
        service.apply(policy(1, null)); clearInvocations(db);
        service.acquire(service.scope(message()), 65536); service.check(service.scope(message())); service.get("local:7");
        verifyZeroInteractions(db);
        String key = db.scanPage(WorkerTaskBandwidthService.PREFIX, null, 10, TaskBandwidthPolicy.class).getItems().keySet().iterator().next();
        db.putSync(key, "not a policy");
        try { instance(); fail("corrupt policy must not silently become unlimited"); } catch (RuntimeException expected) { }
    }

    private static TaskBandwidthPolicy policy(long revision, Double limit) {
        TaskBandwidthPolicy policy = new TaskBandwidthPolicy(); policy.setScopeId("local:7");
        policy.setRevision(revision); policy.setLimitMbps(limit); policy.setMembers(Arrays.asList("one", "two")); return policy;
    }
    private static ObjectListMessage message() {
        ObjectListMessage message = new ObjectListMessage(); message.setTaskIndex(-1); message.setParentTaskId(7);
        message.setTaskBandwidthScopeId("local:7"); return message;
    }
}
