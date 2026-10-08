package com.transfer.tools.service;

import com.transfer.tools.storage.RocksDBService;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.model.ListSubTask;
import lombok.Data;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;
import javax.annotation.PostConstruct;
import java.util.*;
import java.util.concurrent.CancellationException;

/** A durable logical OSS transfer; worker range IDs and file ledgers remain unchanged. */
@Service
public class TaskCreationService {
    private static final String PREFIX = "task_creation:";
    private static final String IDS = "task_creation_ids";
    @Autowired private RocksDBService rocksDBService;
    private final ThreadLocal<String> current = new ThreadLocal<>();
    private final ThreadLocal<Long> currentGeneration = new ThreadLocal<>();
    private final Map<String, Long> active = new HashMap<>();

    @Data public static class Dispatch {
        private String worker;
        private int firstTaskId;
        private List<ListSubTask> tasks = new ArrayList<>();
    }
    @Data public static class Creation {
        private String id;
        private String status;
        private String message;
        private List<Integer> taskIds = new ArrayList<>();
        private long createdAt;
        private Integer publicTaskId;
        private BucketPair request;
        private Boolean scanComplete;
        /** Only new submissions opt in; absent on historical waiting tasks. */
        private Boolean autoStart;
        private boolean stopped;
        private boolean deleted;
        private long generation;
        private String marker = "";
        private int nextShardIndex;
        private List<Dispatch> pendingDispatches = new ArrayList<>();
        private List<String> participatingWorkerNodes = new ArrayList<>();
    }
    @PostConstruct public synchronized void recoverInterrupted() {
        for (Creation creation : getAll()) {
            if (!creation.deleted && !creation.stopped && !Boolean.TRUE.equals(creation.scanComplete)
                    && ("QUEUED".equals(creation.status) || "RUNNING".equals(creation.status))) {
                boolean resume = Boolean.TRUE.equals(creation.autoStart) && creation.request != null;
                creation.status = resume ? "QUEUED" : "FAILED";
                creation.message = resume ? null : "服务重启中断了扫描；已生成的执行子任务与扫描检查点已保留。";
                creation.generation++;
                save(creation);
            }
        }
    }
    public synchronized String create() { return create(null); }
    public synchronized String create(BucketPair request) { return create(request, false); }
    public synchronized String create(BucketPair request, boolean autoStart) {
        Creation creation = new Creation();
        creation.id = UUID.randomUUID().toString(); creation.status = "QUEUED";
        creation.createdAt = System.currentTimeMillis(); creation.scanComplete = false;
        creation.autoStart = autoStart;
        if (request != null) { request.setCreationId(creation.id); creation.request = request; }
        List<String> saved = rocksDBService.get(IDS, List.class);
        List<String> ids = saved == null ? new ArrayList<>() : new ArrayList<>(saved);
        ids.add(creation.id);
        Map<String,Object> batch = new HashMap<>();
        batch.put(PREFIX + creation.id, creation); batch.put(IDS, ids);
        rocksDBService.putBatchSync(batch);
        return creation.id;
    }
    /** Interrupted automatic scans resume from durable range assignments and the saved boundary. */
    public synchronized List<BucketPair> recoverableRequests() {
        List<BucketPair> requests = new ArrayList<>();
        for (Creation c : getAll()) {
            if (!Boolean.TRUE.equals(c.autoStart) || c.deleted || c.stopped
                    || Boolean.TRUE.equals(c.scanComplete) || c.request == null || !"QUEUED".equals(c.status)) continue;
            c.request.setCreationId(c.id);
            requests.add(c.request);
        }
        return requests;
    }
    public synchronized Creation get(String id) {
        if (id == null || !id.matches("[a-fA-F0-9-]{36}")) return null;
        Creation value = rocksDBService.get(PREFIX + id, Creation.class);
        if (value != null) {
            boolean changed = false;
            if (value.taskIds == null) value.taskIds = new ArrayList<>();
            if (value.pendingDispatches == null) value.pendingDispatches = new ArrayList<>();
            if (value.participatingWorkerNodes == null) value.participatingWorkerNodes = new ArrayList<>();
            if (value.scanComplete == null) { value.scanComplete = "COMPLETED".equals(value.status); changed = true; }
            if (value.publicTaskId == null && !value.taskIds.isEmpty()) { value.publicTaskId = value.taskIds.get(0); changed = true; }
            if (changed) save(value);
        }
        return value;
    }
    public synchronized List<Creation> getAll() {
        List<String> ids = rocksDBService.get(IDS, List.class);
        List<Creation> result = new ArrayList<>();
        if (ids != null) for (String id : ids) { Creation c = get(id); if (c != null) result.add(c); }
        return result;
    }
    public synchronized Creation findByTaskId(Integer id) {
        if (id == null) return null;
        for (Creation c : getAll()) if (id.equals(c.publicTaskId) || c.taskIds.contains(id)) return c;
        return null;
    }
    public synchronized List<Integer> expandTaskIds(Collection<Integer> ids) {
        LinkedHashSet<Integer> result = new LinkedHashSet<>();
        if (ids != null) for (Integer id : ids) {
            Creation c = findByTaskId(id);
            if (c == null) result.add(id); else result.addAll(c.taskIds);
        }
        return new ArrayList<>(result);
    }
    public synchronized void ensureReadyToStart(Collection<Integer> ids) {
        if (ids == null) return;
        for (Integer id : ids) {
            Creation c = findByTaskId(id);
            requireNotDeleted(c);
            if (c != null && (!Boolean.TRUE.equals(c.scanComplete) || c.stopped))
                throw new TransferValidationException("OSS_SCAN_NOT_READY", c.stopped
                        ? "任务已停止，请先继续任务" : "源对象仍在扫描，扫描完成后可启动任务");
        }
    }
    /** Serialized with network dispatch so no additional range can be sent beyond the fence. */
    public synchronized List<Integer> stopByTaskIds(Collection<Integer> ids) {
        for (Creation c : selected(ids)) {
            c.stopped = true; c.status = "STOPPED"; c.generation++; save(c);
        }
        return expandTaskIds(ids);
    }
    /** A durable deletion fence prevents stale public IDs and queued scans from recreating ranges. */
    public synchronized List<Integer> markDeletedByTaskIds(Collection<Integer> ids) {
        for (Creation c : selected(ids)) {
            c.deleted = true; c.stopped = true; c.status = "STOPPED"; c.generation++; save(c);
        }
        return expandTaskIds(ids);
    }
    private void requireNotDeleted(Creation c) {
        if (c != null && c.deleted)
            throw new TransferValidationException("OSS_TASK_DELETED", "任务已删除，不能启动或继续扫描");
    }
    /** Caller enqueues returned requests; completed scans need only resume their existing workers. */
    public synchronized List<BucketPair> resumeByTaskIds(Collection<Integer> ids) {
        List<Creation> selected = selected(ids);
        for (Creation c : selected) {
            requireNotDeleted(c);
            if (!scanAlreadyActive(c) && !Boolean.TRUE.equals(c.scanComplete) && c.request == null)
                throw new TransferValidationException("OSS_SCAN_CHECKPOINT_MISSING", "旧任务缺少扫描请求，已生成子任务保留；不能安全重新扫描");
        }
        List<BucketPair> requests = new ArrayList<>();
        for (Creation c : selected) {
            if (scanAlreadyActive(c)) continue;
            c.stopped = false; c.message = null; c.generation++;
            if (Boolean.TRUE.equals(c.scanComplete)) c.status = "COMPLETED";
            else { c.status = "QUEUED"; c.request.setCreationId(c.id); requests.add(c.request); }
            save(c);
        }
        return requests;
    }
    private boolean scanAlreadyActive(Creation c) {
        return !c.stopped && !Boolean.TRUE.equals(c.scanComplete)
                && ("QUEUED".equals(c.status) || "RUNNING".equals(c.status));
    }
    private List<Creation> selected(Collection<Integer> ids) {
        Map<String,Creation> result = new LinkedHashMap<>();
        if (ids != null) for (Integer id : ids) { Creation c = findByTaskId(id); if (c != null) result.put(c.id,c); }
        return new ArrayList<>(result.values());
    }
    public synchronized boolean enterScope(String id, BucketPair request) {
        if (id == null) { clearScope(); return true; }
        Creation c = get(id);
        if (c == null || c.deleted || c.stopped || Boolean.TRUE.equals(c.scanComplete)) return false;
        if (Objects.equals(active.get(id), c.generation)) return false;
        active.put(id,c.generation); current.set(id); currentGeneration.set(c.generation);
        c.status = "RUNNING"; c.request = request; save(c); return true;
    }
    public synchronized void beginScope(String id) {
        if (id == null) { clearScope(); return; }
        Creation c = get(id); current.set(id); currentGeneration.set(c == null ? 0L : c.generation);
        update(id, "RUNNING", null);
    }
    public String currentCreationId() { return current.get(); }
    public synchronized void clearScope() {
        if (current.get() != null && Objects.equals(active.get(current.get()), currentGeneration.get())) active.remove(current.get());
        current.remove(); currentGeneration.remove();
    }
    public synchronized void ensureActive(String id) {
        if (id == null) return;
        Creation c = get(id);
        if (c == null || c.deleted || c.stopped || (id.equals(current.get()) && !Objects.equals(currentGeneration.get(), c.generation)))
            throw new CancellationException("OSS transfer scan has stopped");
    }
    public synchronized void whileActive(String id, Runnable action) { ensureActive(id); action.run(); }
    public synchronized void complete(String id) {
        if (id == null) return; ensureActive(id);
        Creation c = get(id); c.scanComplete = true; c.status = "COMPLETED"; c.message = null; save(c);
    }
    public synchronized void fail(String id, String message) {
        if (id == null) return;
        Creation c = get(id);
        if (c == null || c.deleted || c.stopped || (id.equals(current.get()) && !Objects.equals(currentGeneration.get(), c.generation))) return;
        c.status = "FAILED"; c.message = message; save(c);
    }
    private synchronized void update(String id, String status, String message) {
        Creation creation = get(id); if (creation == null || creation.deleted || creation.stopped) return;
        creation.status = status; creation.message = message; save(creation);
    }
    public synchronized void recordTaskIds(Collection<Integer> added) {
        Creation c = get(current.get()); if (c == null || added == null) return;
        ensureActive(c.id);
        LinkedHashSet<Integer> ids = new LinkedHashSet<>(c.taskIds); ids.addAll(added);
        c.taskIds = new ArrayList<>(ids);
        if (c.publicTaskId == null && !c.taskIds.isEmpty()) c.publicTaskId = c.taskIds.get(0);
        save(c);
    }
    public synchronized void savePending(String id, List<Dispatch> batches) {
        ensureActive(id); Creation c = get(id);
        if (!c.pendingDispatches.isEmpty()) throw new IllegalStateException("unacknowledged dispatch already exists");
        c.pendingDispatches = batches;
        LinkedHashSet<Integer> ids = new LinkedHashSet<>(c.taskIds);
        for (Dispatch batch : batches) {
            if (batch.worker != null && !c.participatingWorkerNodes.contains(batch.worker)) c.participatingWorkerNodes.add(batch.worker);
            for (int i=0;i<batch.tasks.size();i++) ids.add(batch.firstTaskId+i);
        }
        c.taskIds = new ArrayList<>(ids);
        if (c.publicTaskId == null && !c.taskIds.isEmpty()) c.publicTaskId = c.taskIds.get(0);
        save(c);
    }
    public synchronized void acknowledgePending(String id) {
        ensureActive(id); Creation c = get(id);
        // Dispatch groups need not be in source order: use the greatest persisted shard index.
        for (Dispatch batch : c.pendingDispatches) for (ListSubTask task : batch.tasks)
            if (task.getShardIndex() != null && task.getShardIndex() >= c.nextShardIndex) {
                c.nextShardIndex = task.getShardIndex()+1; c.marker = task.getEndKey();
            }
        c.pendingDispatches = new ArrayList<>(); save(c);
    }
    private void save(Creation c) { rocksDBService.putBatchSync(Collections.<String,Object>singletonMap(PREFIX+c.id,c)); }
}
