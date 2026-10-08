package com.transfer.tools.service;

import org.junit.Test;

import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.atomic.AtomicLong;

import static org.junit.Assert.assertEquals;

public class TransferSpeedServiceTest {
    @Test public void firstSampleNeverDividesByMillisecondsSinceCompletion() {
        AtomicLong clock = new AtomicLong(1000L);
        TransferSpeedService speeds = new TransferSpeedService(clock::get);
        speeds.record(1, 1024L * 1024L);
        assertEquals(1024L * 1024L / 5L, speeds.currentBytesPerSecond(1));
        clock.addAndGet(7L);
        assertEquals(1024L * 1024L / 5L, speeds.currentBytesPerSecond(1));
    }

    @Test public void differentlyStartedShardsUseOneComparableWindow() {
        AtomicLong clock = new AtomicLong(0L);
        TransferSpeedService speeds = new TransferSpeedService(clock::get);
        speeds.record(1, 5_000_000L);
        speeds.record(99, 5_000_000L);
        clock.set(4000L);
        speeds.record(2, 10_000_000L);
        speeds.record(99, 10_000_000L);
        clock.set(4001L);
        assertEquals(3_000_000L, speeds.currentBytesPerSecond(1) + speeds.currentBytesPerSecond(2));
        assertEquals(speeds.currentBytesPerSecond(99),
                speeds.currentBytesPerSecond(1) + speeds.currentBytesPerSecond(2));
    }

    @Test public void sustainedStreamUsesBytesBeforeFileCompletion() {
        AtomicLong clock = new AtomicLong(0L);
        TransferSpeedService speeds = new TransferSpeedService(clock::get);
        // The 4 GiB file is still open: count only the 1 MiB/s actually read so far.
        for (int i = 0; i < 50; i++) {
            clock.set(i * 100L);
            speeds.record(1, 104_855L);
        }
        assertEquals(1_048_550L, speeds.currentBytesPerSecond(1));
    }

    @Test public void idleWindowDropsOldBucketsWithoutResettingOnNewBytes() {
        AtomicLong clock = new AtomicLong(0L);
        TransferSpeedService speeds = new TransferSpeedService(clock::get);
        speeds.record(1, 5_000L);
        clock.set(1000L);
        speeds.record(1, 10_000L);
        clock.set(4999L);
        assertEquals(3_000L, speeds.currentBytesPerSecond(1));
        clock.set(5000L);
        assertEquals(2_000L, speeds.currentBytesPerSecond(1));
        clock.set(6000L);
        assertEquals(0L, speeds.currentBytesPerSecond(1));
        clock.set(9000L);
        speeds.record(1, 5_000L);
        assertEquals(1_000L, speeds.currentBytesPerSecond(1));
    }

    @Test public void clearRemovesStoppedTaskSamplesAndAllowsNewAttempt() {
        AtomicLong clock = new AtomicLong(1000L);
        TransferSpeedService speeds = new TransferSpeedService(clock::get);
        speeds.record(1, 10_000L);
        speeds.record(2, 20_000L);
        speeds.clear(1);
        assertEquals(0L, speeds.currentBytesPerSecond(1));
        assertEquals(4_000L, speeds.currentBytesPerSecond(2));
        speeds.record(1, 5_000L);
        assertEquals(1_000L, speeds.currentBytesPerSecond(1));
    }

    @Test public void invalidSamplesDoNotCreateTraffic() {
        TransferSpeedService speeds = new TransferSpeedService(() -> 1000L);
        speeds.record(null, 100L);
        speeds.record(1, 0L);
        speeds.record(1, -100L);
        speeds.clear(null);
        assertEquals(0L, speeds.currentBytesPerSecond(null));
        assertEquals(0L, speeds.currentBytesPerSecond(1));
    }

    @Test public void concurrentStreamsDoNotLoseBytes() throws Exception {
        TransferSpeedService speeds = new TransferSpeedService(() -> 1000L);
        ExecutorService executor = Executors.newFixedThreadPool(8);
        try {
            List<Future<?>> writers = new ArrayList<>();
            for (int i = 0; i < 8; i++) writers.add(executor.submit(() -> {
                for (int n = 0; n < 1000; n++) {
                    speeds.record(1, 5L);
                    speeds.currentBytesPerSecond(1);
                }
            }));
            for (Future<?> writer : writers) writer.get();
            assertEquals(8_000L, speeds.currentBytesPerSecond(1));
        } finally { executor.shutdownNow(); }
    }
}
