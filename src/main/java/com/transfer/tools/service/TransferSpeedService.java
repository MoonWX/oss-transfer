package com.transfer.tools.service;

import org.springframework.stereotype.Service;

import java.util.Arrays;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.TimeUnit;
import java.util.function.LongSupplier;

/** Transfer bytes observed at the stream boundary, averaged over the same rolling window. */
@Service
public class TransferSpeedService {
    private static final long WINDOW_MILLIS = 5000L;
    private static final long BUCKET_MILLIS = 100L;
    private static final int BUCKET_COUNT = (int) (WINDOW_MILLIS / BUCKET_MILLIS);
    private final Map<Integer, Sample> samples = new ConcurrentHashMap<>();
    private final LongSupplier clockMillis;

    public TransferSpeedService() {
        this(() -> TimeUnit.NANOSECONDS.toMillis(System.nanoTime()));
    }

    TransferSpeedService(LongSupplier clockMillis) {
        this.clockMillis = clockMillis;
    }

    /** Record actual stream bytes, never the size of a newly completed file. */
    public void record(Integer taskId, long bytes) {
        if (taskId == null || bytes <= 0L) return;
        samples.compute(taskId, (id, sample) -> {
            if (sample == null) sample = new Sample();
            sample.record(clockMillis.getAsLong(), bytes);
            return sample;
        });
    }

    public long currentBytesPerSecond(Integer taskId) {
        if (taskId == null) return 0L;
        long[] result = {0L};
        samples.computeIfPresent(taskId, (id, sample) -> {
            long bytes = sample.bytesWithinWindow(clockMillis.getAsLong());
            // A fixed denominator avoids amplifying the first byte or summing rates
            // computed over different shard lifetimes. It also naturally decays to zero.
            result[0] = bytes / (WINDOW_MILLIS / 1000L);
            return bytes == 0L ? null : sample;
        });
        return result[0];
    }

    public void clear(Integer taskId) {
        if (taskId != null) samples.remove(taskId);
    }

    private static class Sample {
        private final long[] ticks = new long[BUCKET_COUNT];
        private final long[] bytes = new long[BUCKET_COUNT];

        private Sample() { Arrays.fill(ticks, Long.MIN_VALUE); }

        private void record(long now, long count) {
            long tick = Math.floorDiv(now, BUCKET_MILLIS);
            int slot = (int) Math.floorMod(tick, (long) BUCKET_COUNT);
            if (ticks[slot] != tick) {
                ticks[slot] = tick;
                bytes[slot] = 0L;
            }
            bytes[slot] += count;
        }

        private long bytesWithinWindow(long now) {
            long tick = Math.floorDiv(now, BUCKET_MILLIS);
            long total = 0L;
            for (int i = 0; i < BUCKET_COUNT; i++) {
                if (ticks[i] <= tick && ticks[i] > tick - BUCKET_COUNT) total += bytes[i];
            }
            return total;
        }
    }
}
