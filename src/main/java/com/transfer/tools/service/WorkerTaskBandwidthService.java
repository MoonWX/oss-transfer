package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.TaskBandwidthPolicy;
import com.transfer.tools.model.RocksDBPage;
import com.transfer.tools.storage.RocksDBService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentMap;

/** One interruptible bucket per logical task on this node; reads never query storage. */
@Service
public class WorkerTaskBandwidthService {
    static final String PREFIX = "task-bandwidth:policy:";
    private static final double MAX_BURST_BYTES = 64 * 1024;
    @Autowired private RocksDBService rocksDBService;
    @Autowired private DistributedConfig distributedConfig;
    @Autowired private LocalTaskControlService control;
    private final ConcurrentMap<String, Bucket> buckets = new ConcurrentHashMap<>();

    @PostConstruct public synchronized void restore() {
        Map<String, Bucket> restored = new LinkedHashMap<>();
        String cursor = null;
        do {
            RocksDBPage<TaskBandwidthPolicy> page = rocksDBService.scanPage(PREFIX, cursor, 500, TaskBandwidthPolicy.class);
            for (Map.Entry<String, TaskBandwidthPolicy> entry : page.getItems().entrySet()) {
                TaskBandwidthPolicy policy = entry.getValue(); validate(policy);
                if (!entry.getKey().equals(key(policy.getScopeId())) || restored.put(policy.getScopeId(), new Bucket(copy(policy))) != null)
                    throw new IllegalStateException("invalid stored task bandwidth scope");
            }
            if (page.isDone()) break;
            cursor = page.getNextCursor();
            if (cursor == null) throw new IllegalStateException("task bandwidth cursor did not advance");
        } while (true);
        buckets.clear(); buckets.putAll(restored);
    }

    public synchronized TaskBandwidthPolicy apply(TaskBandwidthPolicy policy) {
        validate(policy);
        TaskBandwidthPolicy next = copy(policy);
        Bucket current = buckets.get(next.getScopeId());
        if (current != null) {
            TaskBandwidthPolicy previous = current.policy;
            if (next.getRevision() < previous.getRevision()) throw new IllegalArgumentException("任务带宽策略版本已过期");
            if (next.getRevision() == previous.getRevision()) {
                if (!same(previous, next)) throw new IllegalArgumentException("任务带宽策略版本冲突");
                return copy(previous);
            }
        }
        // An unsuccessful durable write must leave the running policy unchanged.
        rocksDBService.putSync(key(next.getScopeId()), next);
        if (current == null) buckets.put(next.getScopeId(), new Bucket(next));
        else current.update(next);
        return copy(next);
    }

    public TaskBandwidthPolicy get(String scopeId) {
        Bucket bucket = buckets.get(scopeId);
        return bucket == null ? null : copy(bucket.policy);
    }

    public boolean hasPolicy(ObjectListMessage message) { return buckets.containsKey(scopeId(message)); }

    public String scopeId(ObjectListMessage message) {
        if (message.getTaskBandwidthScopeId() != null && !message.getTaskBandwidthScopeId().trim().isEmpty())
            return message.getTaskBandwidthScopeId();
        return message.getParentTaskId() == null ? "task:" + message.getTaskIndex() : "local:" + message.getParentTaskId();
    }

    public Scope scope(ObjectListMessage message) {
        return new Scope(scopeId(message), message.getTaskBandwidthScopeId() != null,
                message.getTaskIndex(), message.getParentTaskId());
    }

    public void check(Scope scope) { bucket(scope); checkRunning(scope); }

    public void acquire(Scope scope, int bytes) {
        int remaining = bytes;
        while (remaining > 0) remaining -= reserve(scope, remaining).getBytes();
    }

    /** Acquire a complete bounded read before touching the source. No per-stream credit accumulates. */
    public Reservation reserve(Scope scope, int maximum) {
        if (maximum <= 0) throw new IllegalArgumentException("read size must be positive");
        Bucket bucket = bucket(scope);
        while (true) {
            checkRunning(scope);
            if (bucket == null) return new Reservation(null, 0, maximum);
            synchronized (bucket) {
                verifyMember(bucket.policy);
                if (bucket.policy.getLimitMbps() == null) return new Reservation(null, 0, maximum);
                bucket.refill(System.nanoTime());
                int amount = Math.min(maximum, (int) Math.floor(bucket.capacity()));
                if (bucket.tokens >= amount) {
                    bucket.tokens -= amount;
                    return new Reservation(bucket, bucket.policy.getRevision(), amount);
                }
                long nanos = Math.min(100_000_000L, Math.max(1_000_000L,
                        (long) Math.ceil((amount - bucket.tokens) / bucket.bytesPerSecond * 1_000_000_000.0)));
                try { bucket.wait(nanos / 1_000_000L, (int) (nanos % 1_000_000L)); }
                catch (InterruptedException error) {
                    Thread.currentThread().interrupt();
                    throw new TaskCancelledException("task bandwidth wait interrupted");
                }
            }
        }
    }

    public static final class Reservation {
        private final Bucket bucket;
        private final long revision;
        private final int bytes;
        private boolean completed;
        private Reservation(Bucket bucket, long revision, int bytes) {
            this.bucket = bucket; this.revision = revision; this.bytes = bytes;
        }
        public int getBytes() { return bytes; }
        /** Return only unused bytes of this reservation, never credit from an obsolete policy. */
        public synchronized void complete(int consumed) {
            if (completed) return;
            completed = true;
            int unused = bytes - Math.max(0, Math.min(bytes, consumed));
            if (bucket != null && unused > 0) synchronized (bucket) {
                if (bucket.policy.getRevision() == revision && bucket.policy.getLimitMbps() != null) {
                    bucket.refill(System.nanoTime());
                    bucket.tokens = Math.min(bucket.capacity(), bucket.tokens + unused);
                    bucket.notifyAll();
                }
            }
        }
    }

    private Bucket bucket(Scope scope) {
        Bucket bucket = buckets.get(scope.id);
        if (bucket == null && scope.expectedPolicy)
            throw new TransferValidationException("TASK_BANDWIDTH_POLICY_MISSING", "任务带宽策略尚未同步到执行节点");
        if (bucket != null) verifyMember(bucket.policy);
        return bucket;
    }

    private void verifyMember(TaskBandwidthPolicy policy) {
        if (policy.getLimitMbps() != null && !policy.getMembers().contains(distributedConfig.getLocalIp()))
            throw new TransferValidationException("TASK_BANDWIDTH_MEMBER_MISMATCH", "执行节点不在任务带宽策略的参与节点中");
    }

    private void checkRunning(Scope scope) {
        control.checkRunning(scope.taskId); control.checkRunning(scope.parentId);
    }

    private static void validate(TaskBandwidthPolicy policy) {
        if (policy == null || policy.getScopeId() == null || policy.getScopeId().trim().isEmpty()
                || policy.getScopeId().length() > 200 || policy.getRevision() < 1)
            throw new IllegalArgumentException("任务带宽策略无效");
        TaskBandwidthPolicy.validateLimit(policy.getLimitMbps());
        if (policy.getMembers() == null || policy.getMembers().isEmpty()
                || policy.getMembers().stream().anyMatch(member -> member == null || member.trim().isEmpty())
                || new HashSet<>(policy.getMembers()).size() != policy.getMembers().size())
            throw new IllegalArgumentException("任务带宽策略参与节点无效");
    }

    private static boolean same(TaskBandwidthPolicy left, TaskBandwidthPolicy right) {
        return Objects.equals(left.getLimitMbps(), right.getLimitMbps()) && left.getMembers().equals(right.getMembers());
    }

    private static TaskBandwidthPolicy copy(TaskBandwidthPolicy source) {
        TaskBandwidthPolicy result = new TaskBandwidthPolicy(); result.setScopeId(source.getScopeId());
        result.setLimitMbps(source.getLimitMbps()); result.setMembers(new ArrayList<>(source.getMembers()));
        result.setRevision(source.getRevision()); return result;
    }

    private static String key(String scopeId) {
        return PREFIX + Base64.getUrlEncoder().withoutPadding().encodeToString(scopeId.getBytes(StandardCharsets.UTF_8));
    }

    public static final class Scope {
        final String id;
        final boolean expectedPolicy;
        final Integer taskId, parentId;
        Scope(String id, boolean expectedPolicy, Integer taskId, Integer parentId) {
            this.id = id; this.expectedPolicy = expectedPolicy; this.taskId = taskId; this.parentId = parentId;
        }
    }

    private static final class Bucket {
        volatile TaskBandwidthPolicy policy;
        double tokens, bytesPerSecond;
        long lastRefill = System.nanoTime();
        Bucket(TaskBandwidthPolicy policy) { this.policy = policy; bytesPerSecond = rate(policy); }
        synchronized void update(TaskBandwidthPolicy next) {
            refill(System.nanoTime());
            policy = next; bytesPerSecond = rate(next);
            tokens = Math.min(tokens, capacity());
            notifyAll();
        }
        void refill(long now) {
            if (bytesPerSecond > 0) tokens = Math.min(capacity(), tokens + Math.max(0, now - lastRefill) / 1_000_000_000.0 * bytesPerSecond);
            else tokens = 0;
            lastRefill = now;
        }
        double capacity() { return bytesPerSecond <= 0 ? 0 : Math.max(1, Math.min(MAX_BURST_BYTES, bytesPerSecond * 0.1)); }
        static double rate(TaskBandwidthPolicy policy) {
            return policy.getLimitMbps() == null ? 0 : policy.getLimitMbps() * 1_000_000.0 / 8 / policy.getMembers().size();
        }
    }
}
