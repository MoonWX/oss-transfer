package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.client.SimpleClientHttpRequestFactory;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;

import java.util.*;

/** Places new shards only. Persisted assignments are never changed by load observations. */
@Service
public class BalancedWorkerScheduler {
    public static final int MAX_ITEMS = 128;
    public static final long MAX_DATA_BYTES = 256L * 1024 * 1024;
    private static final long CACHE_MILLIS = 5000;
    @Autowired private DistributedConfig distributedConfig;
    private final RestTemplate restTemplate;
    private final Map<String, Snapshot> snapshots = new HashMap<>();
    private final Map<String, Double> reservedWork = new HashMap<>();
    private long nextTie;

    public BalancedWorkerScheduler() {
        SimpleClientHttpRequestFactory factory = new SimpleClientHttpRequestFactory();
        factory.setConnectTimeout(1500); factory.setReadTimeout(2500);
        restTemplate = new RestTemplate(factory);
        restTemplate.getInterceptors().add((request, body, execution) -> {
            String token = distributedConfig.getInternalToken();
            if (token != null && !token.isEmpty()) request.getHeaders().set("X-Internal-Token", token);
            return execution.execute(request, body);
        });
    }

    public synchronized String select(List<String> candidates, long bytes, long items) {
        List<String> workers = new ArrayList<>(new LinkedHashSet<>(candidates));
        if (workers.isEmpty()) throw new TransferValidationException("NO_ELIGIBLE_WORKER", "no eligible worker is available");
        long now = System.currentTimeMillis();
        String selected = null;
        double selectedFinish = Double.POSITIVE_INFINITY;
        int start = (int) Math.floorMod(nextTie++, (long) workers.size());
        Map<String, Snapshot> observations = new HashMap<>();
        boolean completeAvailable = false;
        for (String worker : workers) {
            Snapshot observed = snapshot(worker, now); observations.put(worker, observed);
            completeAvailable |= observed.reachable && observed.complete;
        }
        for (int i = 0; i < workers.size(); i++) {
            String worker = workers.get((start + i) % workers.size());
            Snapshot snapshot = observations.get(worker);
            if (!snapshot.reachable || (completeAvailable && !snapshot.complete)) continue;
            double work = Math.max(Math.max(0L, bytes) / snapshot.bytesPerSecond,
                    Math.max(1L, items) / snapshot.itemsPerSecond);
            double finish = Math.max(snapshot.waitSeconds, reservedWork.getOrDefault(worker, 0.0)) + work;
            if (finish < selectedFinish - 0.000001) { selected = worker; selectedFinish = finish; }
        }
        if (selected == null) throw new DownstreamWorkerException("WORKER_UNAVAILABLE", "no eligible worker returned its queue status", null);
        reservedWork.put(selected, selectedFinish);
        return selected;
    }

    private Snapshot snapshot(String worker, long now) {
        Snapshot saved = snapshots.get(worker);
        if (saved != null && now - saved.observedAt < CACHE_MILLIS) return saved;
        Snapshot value;
        try {
            Map response = restTemplate.getForObject("http://" + worker + ":" + distributedConfig.getPort()
                    + "/api/worker/worker-status", Map.class);
            if (response == null || !Boolean.TRUE.equals(response.get("success"))) throw new IllegalStateException("worker unavailable");
            value = Snapshot.from(response, now);
        } catch (RuntimeException error) { value = new Snapshot(now); }
        snapshots.put(worker, value);
        // PENDING ordinary OSS tasks are not executing yet: elapsed wall time cannot retire
        // their planned work. Only a complete idle observation releases old reservations.
        if (value.reachable && value.complete && value.idle) reservedWork.remove(worker);
        return value;
    }

    static class Snapshot {
        final long observedAt;
        boolean reachable, complete, idle;
        double bytesPerSecond, itemsPerSecond, waitSeconds;
        Snapshot(long observedAt) { this.observedAt = observedAt; }
        static Snapshot from(Map response, long now) {
            Snapshot result = new Snapshot(now);
            Map queue = map(response.get("queueStatus"));
            Map rates = map(response.get("rateLimit"));
            Map tasks = map(response.get("taskStats"));
            result.reachable = true;
            result.complete = finite(queue.get("putQueueSize")) && finite(queue.get("listQueueSize"))
                    && finite(map(rates.get("put")).get("limitQps"))
                    && finite(map(rates.get("bandwidth")).get("limitBps"));
            // Incomplete but reachable nodes are used only when every observation is incomplete.
            // Equal conservative capacities then provide weighted placement, never a claim of idle I/O.
            result.itemsPerSecond = positive(map(rates.get("put")).get("limitQps"), 1000);
            // RateLimiterManager bandwidth units are bits/second despite the legacy Bps name.
            result.bytesPerSecond = positive(map(rates.get("bandwidth")).get("limitBps"), 1000000000) / 8;
            double put = number(queue.get("putQueueSize"));
            double list = number(queue.get("listQueueSize"));
            double active = number(tasks.get("runningTasks")) + number(tasks.get("listDoneTasks"));
            double pending = number(tasks.get("pendingTasks"));
            result.idle = finite(tasks.get("pendingTasks")) && finite(tasks.get("runningTasks"))
                    && finite(tasks.get("listDoneTasks")) && put == 0 && list == 0 && active == 0 && pending == 0;
            // Queue backlog and pending scans are a conservative item-cost floor. New shard
            // reservations add known byte cost; instantaneous measured throughput is not capacity.
            result.waitSeconds = (put + Math.max(list, pending) * MAX_ITEMS) / result.itemsPerSecond + active / result.itemsPerSecond;
            if (!result.complete) result.waitSeconds = Math.max(1, result.waitSeconds);
            return result;
        }
        private static Map map(Object value) { return value instanceof Map ? (Map) value : Collections.emptyMap(); }
        private static boolean finite(Object value) { return value instanceof Number && Double.isFinite(((Number) value).doubleValue()); }
        private static double number(Object value) { return finite(value) ? Math.max(0, ((Number) value).doubleValue()) : 0; }
        private static double positive(Object value, double fallback) {
            double number = number(value); return number > 0 && Double.isFinite(number) ? number : fallback;
        }
    }
}
