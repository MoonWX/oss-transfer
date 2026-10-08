package com.transfer.tools.service;

import com.amazonaws.event.ProgressEvent;
import com.amazonaws.event.ProgressEventType;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.TaskBandwidthPolicy;
import com.transfer.tools.storage.RocksDBService;
import com.transfer.tools.util.TaskBandwidthInputStream;
import org.junit.*;
import org.springframework.test.util.ReflectionTestUtils;
import java.io.*;
import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.AtomicLong;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class TaskBandwidthInputStreamTest {
    private WorkerTaskBandwidthService service;
    private WorkerTaskBandwidthService.Scope scope;
    private LocalTaskControlService control;
    private TransferSpeedService speed;
    private AtomicLong measured;

    @Before public void setup() {
        service = spy(new WorkerTaskBandwidthService());
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getLocalIp()).thenReturn("one");
        ReflectionTestUtils.setField(service, "rocksDBService", mock(RocksDBService.class));
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        control = new LocalTaskControlService();
        ReflectionTestUtils.setField(service, "control", control);
        ObjectListMessage message = new ObjectListMessage(); message.setTaskIndex(1);
        message.setTaskBandwidthScopeId("task:1"); scope = service.scope(message);
        apply(1, 100.0);
        speed = mock(TransferSpeedService.class); measured = new AtomicLong();
        doAnswer(call -> { measured.addAndGet(call.<Long>getArgument(1)); return null; }).when(speed).record(eq(1), anyLong());
    }

    @Test(timeout = 4000) public void concurrentReadsWaitForSharedQuotaBeforeTouchingSource() throws Exception {
        apply(2, 0.1); // 12,500 B/s, shared by all four streams.
        service.reserve(scope, 1250).complete(1250); // Consume any allowed initial burst credit.
        ExecutorService pool = Executors.newFixedThreadPool(4);
        CountDownLatch start = new CountDownLatch(1);
        AtomicLong firstRead = new AtomicLong(Long.MAX_VALUE);
        long begin = System.nanoTime();
        try {
            List<Future<Integer>> reads = new ArrayList<>();
            for (int i = 0; i < 4; i++) reads.add(pool.submit(() -> {
                start.await();
                InputStream input = new ByteArrayInputStream(new byte[1000]) {
                    @Override public synchronized int read(byte[] b, int off, int len) {
                        firstRead.accumulateAndGet(System.nanoTime(), Math::min);
                        return super.read(b, off, len);
                    }
                };
                return stream(input, false).read(new byte[65536]);
            }));
            start.countDown();
            for (Future<Integer> read : reads) assertEquals(Integer.valueOf(1000), read.get());
            long elapsed = TimeUnit.NANOSECONDS.toMillis(System.nanoTime() - begin);
            assertTrue("concurrent streams read ahead of quota: " + elapsed, elapsed >= 240);
            assertTrue("source touched before reservation", TimeUnit.NANOSECONDS.toMillis(firstRead.get() - begin) >= 50);
            assertEquals(4000L, measured.get());
        } finally { pool.shutdownNow(); }
    }

    @Test public void shortReadAndEofRefundUnusedQuotaAndOnlyCountPayload() throws Exception {
        TaskBandwidthInputStream input = stream(new ByteArrayInputStream(new byte[7]), false);
        assertEquals(7, input.read(new byte[65536]));
        assertTrue(tokens() >= 65536 - 7);
        assertEquals(-1, input.read(new byte[65536]));
        assertTrue(tokens() >= 65536 - 7);
        assertEquals(7L, measured.get());
    }

    @Test public void failedReadRefundsReservationWithoutRecordingTraffic() throws Exception {
        InputStream broken = new InputStream() { @Override public int read() throws IOException { throw new IOException("fixture"); } };
        try { stream(broken, false).read(new byte[65536]); fail(); } catch (IOException expected) { }
        assertEquals(65536.0, tokens(), 0.001);
        assertEquals(0L, measured.get());
    }

    @Test public void invalidAndEmptyReadsDoNotAcquireQuota() throws Exception {
        TaskBandwidthInputStream input = stream(new ByteArrayInputStream(new byte[1]), false);
        clearInvocations(service);
        assertEquals(0, input.read(new byte[0]));
        try { input.read(null, 0, 1); fail(); } catch (NullPointerException expected) { }
        try { input.read(new byte[1], 2, 0); fail(); } catch (IndexOutOfBoundsException expected) { }
        verify(service, never()).reserve(any(), anyInt()); assertEquals(0L, measured.get());
    }

    @Test public void singleByteReadDoesNotFabricateDataWhenBulkReadReturnsZero() throws Exception {
        InputStream unusual = new InputStream() {
            @Override public int read() { return 211; }
            @Override public int read(byte[] b, int o, int n) { return 0; }
        };
        assertEquals(211, stream(unusual, false).read()); assertEquals(1L, measured.get());
    }

    @Test public void sdkHashingIsExcludedButBothHttpAttemptsAreCounted() throws Exception {
        TaskBandwidthInputStream input = stream(new ByteArrayInputStream(new byte[31]), true);
        input.mark(31); assertEquals(31, input.read(new byte[31])); assertEquals(0L, measured.get());
        input.reset(); input.uploadListener().progressChanged(new ProgressEvent(ProgressEventType.HTTP_REQUEST_STARTED_EVENT));
        assertEquals(31, input.read(new byte[31]));
        input.uploadListener().progressChanged(new ProgressEvent(ProgressEventType.CLIENT_REQUEST_RETRY_EVENT));
        input.reset(); assertEquals(31, input.read(new byte[31])); assertEquals(31L, measured.get());
        input.reset(); input.uploadListener().progressChanged(new ProgressEvent(ProgressEventType.HTTP_REQUEST_STARTED_EVENT));
        assertEquals(31, input.read(new byte[31])); assertEquals(62L, measured.get());
    }

    @Test public void stoppedReservationCannotReadSource() throws Exception {
        InputStream source = mock(InputStream.class);
        doAnswer(call -> {
            Object reservation = call.callRealMethod(); control.stop(1); return reservation;
        }).when(service).reserve(any(), anyInt());
        try { stream(source, false).read(new byte[65536]); fail(); } catch (TaskCancelledException expected) { }
        verifyZeroInteractions(source); assertEquals(65536.0, tokens(), 0.001);
    }

    @Test public void reservationRefundIsIdempotentAndCannotCreditAnotherPolicyRevision() {
        WorkerTaskBandwidthService.Reservation reservation = service.reserve(scope, 65536);
        reservation.complete(10); double credit = tokens(); reservation.complete(0);
        assertEquals(credit, tokens(), 0.001);
        WorkerTaskBandwidthService.Reservation old = service.reserve(scope, 65536);
        apply(2, 0.1); double afterUpdate = tokens(); old.complete(0);
        assertEquals(afterUpdate, tokens(), 0.001);
    }

    private TaskBandwidthInputStream stream(InputStream input, boolean upload) {
        return new TaskBandwidthInputStream(input, service, scope, upload, speed, 1);
    }
    private void apply(long revision, Double limit) {
        TaskBandwidthPolicy policy = new TaskBandwidthPolicy(); policy.setScopeId("task:1");
        policy.setRevision(revision); policy.setLimitMbps(limit); policy.setMembers(Collections.singletonList("one")); service.apply(policy);
    }
    private double tokens() {
        Object bucket = ((Map<?, ?>) ReflectionTestUtils.getField(service, "buckets")).get("task:1");
        return (Double) ReflectionTestUtils.getField(bucket, "tokens");
    }
}
