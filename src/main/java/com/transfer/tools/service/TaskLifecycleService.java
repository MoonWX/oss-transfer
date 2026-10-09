package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.DeserializationFeature;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.SubTaskStatus;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.client.SimpleClientHttpRequestFactory;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;
import java.util.*;

/** Deletes task metadata across workers and retains local transfers as parent rows. */
@Service
public class TaskLifecycleService {
    @Autowired private DistributedConfig distributedConfig;
    @Autowired private LocalParentTaskService parents;
    @Autowired private MasterLocalTransferService localTransfers;
    @Autowired private TaskCreationService creations;
    @Autowired private OssTaskGroupService ossGroups;
    private final ObjectMapper mapper = new ObjectMapper().configure(DeserializationFeature.FAIL_ON_UNKNOWN_PROPERTIES, false);
    private final RestTemplate http;
    public TaskLifecycleService() {
        SimpleClientHttpRequestFactory factory = new SimpleClientHttpRequestFactory();
        factory.setConnectTimeout(3000); factory.setReadTimeout(10000);
        http = new RestTemplate(factory);
        http.getInterceptors().add((request, body, execution) -> {
            String token = distributedConfig.getInternalToken();
            if (token != null && !token.isEmpty()) request.getHeaders().set("X-Internal-Token", token);
            return execution.execute(request, body);
        });
    }
    private String url(String worker, String path) { return "http://" + worker + ":" + distributedConfig.getPort() + "/api/worker/" + path; }
    private List<String> workers() {
        if (distributedConfig.getWorkerIps() == null || distributedConfig.getWorkerIps().isEmpty())
            throw new IllegalStateException("未配置执行节点");
        return distributedConfig.getWorkerIps();
    }
    private List<SubTaskStatus> read(String worker, boolean deleted) {
        Map response = http.getForObject(url(worker, deleted ? "deleted-tasks" : "status"), Map.class);
        if (response == null || Boolean.FALSE.equals(response.get("success")) || !(response.get("tasks") instanceof Collection))
            throw new IllegalStateException("节点未返回完整任务列表");
        List<SubTaskStatus> result = new ArrayList<>();
        for (Object task : (Collection<?>)response.get("tasks")) {
            Map<String,Object> fields = mapper.convertValue(task, Map.class);
            // /status is a display DTO with formatted dates; deletion uses only identity and state.
            // The worker moves its original durable record, preserving the actual timestamps.
            if (!deleted) for (String date : Arrays.asList("createTime", "startTime", "endTime")) fields.remove(date);
            result.add(mapper.convertValue(fields, SubTaskStatus.class));
        }
        return result;
    }
    private void write(String worker, String action, Collection<Integer> ids) {
        write(worker, action, ids, Collections.emptySet());
    }
    private void write(String worker, String action, Collection<Integer> ids, Collection<Integer> parentIds) {
        if (ids.isEmpty() && parentIds.isEmpty()) return;
        Map<String,Object> request = new LinkedHashMap<>();
        request.put("taskIds", ids);
        if (!parentIds.isEmpty()) request.put("parentTaskIds", parentIds);
        Map response = http.postForObject(url(worker, action), request, Map.class);
        if (response == null || !Boolean.TRUE.equals(response.get("success")))
            throw new IllegalStateException(response == null ? "节点未响应" : String.valueOf(response.get("message")));
    }
    public Map<String,Object> deletedTasks() {
        List<String> queriedWorkers = new ArrayList<>(workers());
        Map<String,Object> grouped = new LinkedHashMap<>();
        Set<String> successfulWorkers = new LinkedHashSet<>();
        List<String> failed = new ArrayList<>();
        int count = 0;
        for (String worker : queriedWorkers) {
            try {
                List<SubTaskStatus> rows = new ArrayList<>();
                for (SubTaskStatus task : read(worker, true)) if (task.getParentTaskId() == null) rows.add(task);
                grouped.put(worker, rows); successfulWorkers.add(worker); count += rows.size();
            } catch (RuntimeException e) { failed.add(worker); }
        }
        List<SubTaskStatus> local = parents.getDeleted();
        String localIp = distributedConfig.getLocalIp();
        List<SubTaskStatus> localRows = new ArrayList<>();
        if (grouped.get(localIp) instanceof List) localRows.addAll((List<SubTaskStatus>)grouped.get(localIp));
        localRows.addAll(local);
        if (!localRows.isEmpty() || successfulWorkers.contains(localIp)) grouped.put(localIp, localRows);
        count += local.size();
        if (ossGroups != null) {
            List<Object> all = new ArrayList<>();
            for (Object rows : grouped.values()) if (rows instanceof Collection) all.addAll((Collection<?>) rows);
            List<Object> logical = ossGroups.group(all, true, !failed.isEmpty());
            grouped.clear();
            // An empty list confirms that the worker responded; a missing key remains unknown.
            for (String worker : successfulWorkers) grouped.put(worker, new ArrayList<>());
            for (Object item : logical) {
                Map row = mapper.convertValue(item, Map.class);
                String worker = row.get("workerNode") == null ? localIp : String.valueOf(row.get("workerNode"));
                ((List<Object>) grouped.computeIfAbsent(worker, key -> new ArrayList<>())).add(item);
            }
            count = logical.size();
        }
        Map<String,Object> result = result(true, failed, "");
        result.put("deletedTasks", grouped); result.put("totalCount", count);
        result.put("queriedWorkers", queriedWorkers);
        result.put("successfulWorkers", new ArrayList<>(successfulWorkers));
        result.put("complete", failed.isEmpty());
        return result;
    }
    public Map<String,Object> clear(Map<String,Object> params, boolean permanent) {
        if (creations == null) {
            synchronized (parents) { return clearLocked(params, permanent); }
        }
        // Match master OSS control locking so resume cannot cross deletion preflight or its fence.
        synchronized (creations) {
            synchronized (parents) { return clearLocked(params, permanent); }
        }
    }
    private Map<String,Object> clearLocked(Map<String,Object> params, boolean permanent) {
        Set<Integer> requested = ids(params.get("taskIds"));
        if (permanent && requested.isEmpty()) return result(false, Collections.emptyList(), "请指定回收站任务 ID");
        // A complete preflight is required before mutating any node.
        Map<String,List<SubTaskStatus>> live = new LinkedHashMap<>(), deleted = new LinkedHashMap<>();
        List<String> failed = new ArrayList<>();
        for (String worker : workers()) {
            try {
                live.put(worker, read(worker, false));
                if (permanent) deleted.put(worker, read(worker, true));
            } catch (RuntimeException e) { failed.add(worker); }
        }
        if (!failed.isEmpty()) return result(false, failed, "部分节点不可达，尚未删除任务");
        Set<Integer> selectedOssGroups = new LinkedHashSet<>();
        if (creations != null && ossGroups != null) {
            List<Object> candidates = new ArrayList<>();
            for (List<SubTaskStatus> rows : (permanent ? deleted : live).values()) candidates.addAll(rows);
            for (Object item : ossGroups.group(candidates, permanent, false)) {
                Map fields = mapper.convertValue(item, Map.class);
                if (!Boolean.TRUE.equals(fields.get("ossTaskGroup"))) continue;
                for (String field : Arrays.asList("createTime", "startTime", "endTime")) fields.remove(field);
                SubTaskStatus logical = mapper.convertValue(fields, SubTaskStatus.class);
                TaskCreationService.Creation creation = creations.findByTaskId(logical.getTaskIndex());
                boolean selected = matches(logical, requested, params, permanent);
                if (!requested.isEmpty() && creation != null)
                    for (Integer id : creation.getTaskIds()) selected |= requested.contains(id);
                if (!selected || creation == null) continue;
                if (active(logical)) return result(false, Collections.emptyList(), "任务 #" + logical.getTaskIndex() + " 正在运行，请先停止");
                selectedOssGroups.add(creation.getPublicTaskId());
            }
            if (incompleteOssGroups(live, deleted, permanent, selectedOssGroups))
                return result(false, Collections.emptyList(), "无法确认全部执行子任务，尚未删除任务");
            if (!requested.isEmpty()) requested = new LinkedHashSet<>(creations.expandTaskIds(requested));
        }
        List<SubTaskStatus> parentRows;
        if (permanent) parentRows = parents.getDeleted();
        else {
            Map<String,Object> aggregate = localTransfers.aggregateStatus(null, null, null, null, null, null);
            if (Boolean.TRUE.equals(aggregate.get("partial"))) return result(false,
                    (List<String>)aggregate.getOrDefault("unreachableWorkers", Collections.emptyList()), "无法确认全部子任务状态，尚未删除任务");
            parentRows = new ArrayList<>();
            Object tasks = aggregate.get("tasks");
            if (tasks instanceof Collection) for (Object task : (Collection<?>)tasks) parentRows.add(mapper.convertValue(task, SubTaskStatus.class));
        }
        Map<Integer,SubTaskStatus> selectedParents = new LinkedHashMap<>();
        Set<Integer> logicalIds = new HashSet<>();
        for (SubTaskStatus parent : parentRows) {
            if (parent.getParentTaskId() != null && matches(parent, requested, params, permanent)) {
                if (active(parent)) return result(false, Collections.emptyList(), "任务 #" + parent.getTaskIndex() + " 正在运行，请先停止");
                selectedParents.put(parent.getTaskIndex(), parent); logicalIds.add(parent.getTaskIndex());
            }
        }
        if (!selectedParents.isEmpty()) {
            // Stop discovery too: a pending parent may not have any physical shard yet.
            Map<String,Object> stopped = localTransfers.control("stop",
                    Collections.singletonMap("taskIds", new ArrayList<>(selectedParents.keySet())));
            if (stopped == null || Boolean.TRUE.equals(stopped.get("partial"))
                    || ((Number)stopped.getOrDefault("failed", 0)).intValue() > 0) {
                return result(false, stopped == null ? Collections.emptyList()
                        : (List<String>)stopped.getOrDefault("unreachableWorkers", Collections.emptyList()),
                        "尚未确认所有节点停止，任务记录保留，请重试");
            }
            // Include shards whose import arrived between the initial snapshot and the stop fence.
            for (String worker : workers()) {
                try { live.put(worker, read(worker, false)); }
                catch (RuntimeException e) { failed.add(worker); }
            }
            if (!failed.isEmpty()) return result(false, failed, "停止后无法确认全部子任务，任务记录保留，请重试");
        }
        if (!selectedOssGroups.isEmpty()) {
            for (List<SubTaskStatus> rows : live.values()) for (SubTaskStatus task : rows) {
                TaskCreationService.Creation group = creations.findByTaskId(task.getTaskIndex());
                if (group != null && selectedOssGroups.contains(group.getPublicTaskId()) && active(task))
                    return result(false, Collections.emptyList(), "任务 #" + group.getPublicTaskId() + " 正在运行，请先停止");
            }
            // Fence exploration before the second snapshot, including imports whose ACK was lost.
            creations.stopByTaskIds(selectedOssGroups);
            for (String worker : workers()) {
                try {
                    live.put(worker, read(worker, false));
                    if (permanent) deleted.put(worker, read(worker, true));
                } catch (RuntimeException e) { failed.add(worker); }
            }
            if (!failed.isEmpty()) return result(false, failed, "停止扫描后无法确认全部子任务，任务记录保留，请重试");
            if (incompleteOssGroups(live, deleted, permanent, selectedOssGroups))
                return result(false, Collections.emptyList(), "停止扫描后无法确认全部执行子任务，任务记录保留，请重试");
        }
        Map<String,Set<Integer>> targets = new LinkedHashMap<>();
        for (String worker : workers()) {
            Set<Integer> target = new LinkedHashSet<>();
            List<SubTaskStatus> candidates = new ArrayList<>(permanent ? deleted.get(worker) : live.get(worker));
            // A prior partial soft-delete may leave inactive shards outside the recycle bin.
            if (permanent) for (SubTaskStatus task : live.get(worker))
                if (selectedParents.containsKey(task.getParentTaskId()) || selectedOss(task, selectedOssGroups)) candidates.add(task);
            for (SubTaskStatus task : candidates) {
                boolean child = selectedParents.containsKey(task.getParentTaskId());
                boolean standalone = task.getParentTaskId() == null && matches(task, requested, params, permanent);
                TaskCreationService.Creation oss = creations == null ? null : creations.findByTaskId(task.getTaskIndex());
                if (oss != null) standalone = selectedOssGroups.contains(oss.getPublicTaskId());
                if (!child && !standalone) continue;
                if (active(task)) return result(false, Collections.emptyList(), "任务 #" + task.getTaskIndex() + " 正在运行，请先停止");
                target.add(task.getTaskIndex());
                if (standalone) logicalIds.add(oss == null ? task.getTaskIndex() : oss.getPublicTaskId());
            }
            targets.put(worker, target);
        }
        if (logicalIds.isEmpty()) return result(false, Collections.emptyList(), "未找到可删除的任务");
        // Persist the terminal deletion fence before touching any physical record.
        // It remains set after a partial worker failure, preventing a concurrent or later resume.
        if (!selectedOssGroups.isEmpty()) creations.markDeletedByTaskIds(selectedOssGroups);
        if (!permanent) for (Map.Entry<Integer,SubTaskStatus> entry : selectedParents.entrySet())
            parents.softDelete(entry.getKey(), entry.getValue());
        for (String worker : workers()) {
            try {
                if (permanent) {
                    Set<Integer> remnants = new HashSet<>();
                    for (SubTaskStatus task : live.get(worker)) if (targets.get(worker).contains(task.getTaskIndex())) remnants.add(task.getTaskIndex());
                    write(worker, "clear", remnants);
                }
                write(worker, permanent ? "permanent-delete" : "clear", targets.get(worker),
                        permanent ? selectedParents.keySet() : Collections.emptySet());
            } catch (RuntimeException e) { failed.add(worker); }
        }
        if (permanent && failed.isEmpty()) for (Integer id : selectedParents.keySet()) parents.permanentDelete(id);
        Map<String,Object> result = result(failed.isEmpty(), failed, failed.isEmpty()
                ? permanent ? "任务记录已永久删除" : "任务已移入回收站"
                : "部分节点记录尚未删除，请刷新后重试");
        result.put("accepted", failed.isEmpty() ? logicalIds.size() : 0);
        result.put("taskIds", new ArrayList<>(logicalIds));
        result.put("failed", failed.size());
        return result;
    }
    private boolean incompleteOssGroups(Map<String,List<SubTaskStatus>> live,
                                         Map<String,List<SubTaskStatus>> deleted, boolean permanent,
                                         Set<Integer> selected) {
        if (selected.isEmpty()) return false;
        List<Object> rows = new ArrayList<>();
        for (List<SubTaskStatus> tasks : live.values()) rows.addAll(tasks);
        // A failed soft-delete can split a group between live records and the recycle bin.
        // Permanent deletion is allowed only when this combined snapshot contains every range.
        if (permanent) for (List<SubTaskStatus> tasks : deleted.values()) rows.addAll(tasks);
        Set<Integer> verified = new HashSet<>();
        for (Object item : ossGroups.group(rows, permanent, false)) {
            Map fields = mapper.convertValue(item, Map.class);
            if (!Boolean.TRUE.equals(fields.get("ossTaskGroup")) || !(fields.get("taskIndex") instanceof Number)) continue;
            int id = ((Number) fields.get("taskIndex")).intValue();
            if (!selected.contains(id)) continue;
            Object missing = fields.get("missingTaskIds");
            if (Boolean.TRUE.equals(fields.get("partial")) || Boolean.FALSE.equals(fields.get("snapshotComplete"))
                    || (missing instanceof Collection && !((Collection<?>) missing).isEmpty())) return true;
            verified.add(id);
        }
        return !verified.containsAll(selected);
    }
    private boolean selectedOss(SubTaskStatus task, Set<Integer> selected) {
        TaskCreationService.Creation group = creations == null ? null : creations.findByTaskId(task.getTaskIndex());
        return group != null && selected.contains(group.getPublicTaskId());
    }
    private boolean matches(SubTaskStatus task, Set<Integer> ids, Map<String,Object> params, boolean permanent) {
        if (!ids.isEmpty()) return ids.contains(task.getTaskIndex());
        String source = (String)params.get("sourceBucket"), destination = (String)params.get("destBucket");
        if (source != null && !source.isEmpty() && !source.equals(task.getSourceBucket())) return false;
        if (destination != null && !destination.isEmpty() && !destination.equals(task.getDestBucket())) return false;
        return permanent || source != null || destination != null || task.getStatus() == SubTaskStatus.TaskStatus.COMPLETED;
    }
    private boolean active(SubTaskStatus task) {
        return task.getStatus() == SubTaskStatus.TaskStatus.RUNNING || task.getStatus() == SubTaskStatus.TaskStatus.LIST_DONE;
    }
    private Set<Integer> ids(Object raw) {
        Set<Integer> result = new LinkedHashSet<>();
        if (raw instanceof Collection) for (Object id : (Collection<?>)raw)
            result.add(id instanceof Number ? ((Number)id).intValue() : Integer.valueOf(String.valueOf(id)));
        return result;
    }
    private Map<String,Object> result(boolean success, List<String> failed, String message) {
        Map<String,Object> result = new LinkedHashMap<>();
        result.put("success", success); result.put("partial", !failed.isEmpty());
        result.put("failedWorkers", failed); result.put("message", message);
        return result;
    }
}
