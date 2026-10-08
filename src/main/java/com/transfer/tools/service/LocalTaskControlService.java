package com.transfer.tools.service;

import org.springframework.stereotype.Service;

import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.Map;
import java.util.Collection;
import java.util.HashSet;

@Service
public class LocalTaskControlService {
    @org.springframework.beans.factory.annotation.Autowired(required = false)
    private com.transfer.tools.storage.RocksDBService rocksDBService;
    private static final String STOP_PREFIX = "local:stop:";
    private final Map<Integer, Set<Operation>> operations = new ConcurrentHashMap<>();
    private final Map<Integer, Set<Runnable>> cancellations = new ConcurrentHashMap<>();

    public static class StopRecord {
        public Integer taskId;
        public boolean stopped;
        public StopRecord() { }
        StopRecord(Integer taskId, boolean stopped) { this.taskId = taskId; this.stopped = stopped; }
    }

    @javax.annotation.PostConstruct
    public void loadStops() {
        if (rocksDBService == null) return;
        for (StopRecord record : rocksDBService.scanByPrefix(STOP_PREFIX, StopRecord.class)) {
            if (record.stopped && record.taskId != null) stoppedTasks.add(record.taskId);
        }
    }

    public synchronized Operation begin(Integer taskId) { return begin(taskId, null); }

    public synchronized Operation begin(Integer taskId, Integer parentId) {
        checkRunning(taskId);
        if (parentId != null) checkRunning(parentId);
        Operation operation = new Operation(taskId, parentId, Thread.currentThread());
        operations.computeIfAbsent(taskId, key -> ConcurrentHashMap.newKeySet()).add(operation);
        if (parentId != null && !parentId.equals(taskId)) {
            operations.computeIfAbsent(parentId, key -> ConcurrentHashMap.newKeySet()).add(operation);
        }
        return operation;
    }

    public synchronized boolean isActive(Integer taskId) {
        Set<Operation> active = operations.get(taskId);
        return active != null && !active.isEmpty();
    }

    public synchronized AutoCloseable onCancel(Integer taskId, Runnable cancel) {
        checkRunning(taskId);
        cancellations.computeIfAbsent(taskId, key -> ConcurrentHashMap.newKeySet()).add(cancel);
        return () -> { synchronized (LocalTaskControlService.this) {
            Set<Runnable> actions = cancellations.get(taskId);
            if (actions != null) { actions.remove(cancel); if (actions.isEmpty()) cancellations.remove(taskId); }
        }};
    }

    public final class Operation implements AutoCloseable {
        private final Integer taskId;
        private final Integer parentId;
        private final Thread thread;
        private boolean cancelled;
        private Operation(Integer taskId, Integer parentId, Thread thread) {
            this.taskId = taskId; this.parentId = parentId; this.thread = thread;
        }
        @Override public void close() {
            synchronized (LocalTaskControlService.this) {
                Set<Operation> active = operations.get(taskId);
                if (active != null) { active.remove(this); if (active.isEmpty()) operations.remove(taskId); }
                if (parentId != null && !parentId.equals(taskId)) {
                    Set<Operation> parent = operations.get(parentId);
                    if (parent != null) { parent.remove(this); if (parent.isEmpty()) operations.remove(parentId); }
                }
                // A task cancellation must not retire a shared executor worker.
                if (cancelled && thread == Thread.currentThread()) Thread.interrupted();
            }
        }
    }

    private final Set<Integer> stoppedTasks = ConcurrentHashMap.newKeySet();
    private final Map<Integer, Set<String>> retrySelections = new ConcurrentHashMap<>();

    public void stop(Integer taskId) {
        Set<Runnable> actions;
        synchronized (this) {
            if (rocksDBService != null) rocksDBService.putSync(STOP_PREFIX + taskId, new StopRecord(taskId, true));
            stoppedTasks.add(taskId);
            actions = new HashSet<>(cancellations.getOrDefault(taskId, java.util.Collections.emptySet()));
            Set<Operation> active = operations.get(taskId);
            if (active != null) for (Operation operation : active) {
                operation.cancelled = true;
                operation.thread.interrupt();
                actions.addAll(cancellations.getOrDefault(operation.taskId, java.util.Collections.emptySet()));
            }
        }
        for (Runnable action : actions) try { action.run(); } catch (RuntimeException ignored) { }
    }

    public synchronized void resume(Integer taskId) {
        if (isActive(taskId)) throw new IllegalStateException("task is still stopping; retry after active I/O exits");
        if (rocksDBService != null) rocksDBService.putSync(STOP_PREFIX + taskId, new StopRecord(taskId, false));
        stoppedTasks.remove(taskId);
    }

    public void selectItems(Integer taskId, Collection<String> itemKeys) {
        if (itemKeys == null || itemKeys.isEmpty()) {
            retrySelections.remove(taskId);
        } else {
            retrySelections.put(taskId, new HashSet<>(itemKeys));
        }
    }

    public boolean shouldProcess(Integer taskId, String logicalKey) {
        Set<String> selected = retrySelections.get(taskId);
        return selected == null || selected.contains(logicalKey);
    }

    public void clearSelection(Integer taskId) {
        retrySelections.remove(taskId);
    }

    public boolean isStopped(Integer taskId) {
        return taskId != null && stoppedTasks.contains(taskId);
    }

    public void checkRunning(Integer taskId) {
        if (isStopped(taskId) || Thread.currentThread().isInterrupted()) {
            throw new TaskCancelledException("task " + taskId + " is stopped");
        }
    }
}
