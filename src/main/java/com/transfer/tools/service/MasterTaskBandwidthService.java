package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.*;
import org.springframework.http.client.SimpleClientHttpRequestFactory;
import org.springframework.stereotype.Service;
import java.net.URLEncoder;
import java.util.*;

/** Desired logical-task budgets live on Master; acknowledgments never stand in for worker state. */
@Service
public class MasterTaskBandwidthService {
    private static final String POLICY = "task_bandwidth:desired:";
    private static final String TASK_SCOPE = "task_bandwidth:task:";
    private static final String SCOPE_TASKS = "task_bandwidth:scope_tasks:";
    @Autowired private RocksDBService rocksDBService;
    @Autowired private DistributedConfig config;
    @Autowired private LocalParentTaskService parents;
    @Autowired private WorkerTaskBandwidthService workerPolicies;
    @Autowired private NodeConfigService nodeConfig;
    @Autowired private TaskCreationService creations;
    @org.springframework.context.annotation.Lazy
    @Autowired private MasterLocalTransferService localTransfers;
    private final ObjectMapper mapper = new ObjectMapper();
    private final org.springframework.web.client.RestTemplate restTemplate;

    public MasterTaskBandwidthService() {
        SimpleClientHttpRequestFactory factory = new SimpleClientHttpRequestFactory();
        factory.setConnectTimeout(3000); factory.setReadTimeout(10000);
        restTemplate = new org.springframework.web.client.RestTemplate(factory);
    }

    public static void validateLimit(Double limit) {
        try { TaskBandwidthPolicy.validateLimit(limit); }
        catch (IllegalArgumentException e) { throw new TransferValidationException("INVALID_TASK_BANDWIDTH", e.getMessage()); }
    }

    public synchronized TaskBandwidthPolicy desired(String scope) {
        return scope == null ? null : rocksDBService.get(POLICY + scope, TaskBandwidthPolicy.class);
    }

    /** Called before any dispatch, including an initially unlimited task. */
    public synchronized TaskBandwidthPolicy initialize(String scope, Double limit, List<String> members) {
        validateLimit(limit);
        TaskBandwidthPolicy policy = desired(scope);
        if (policy == null) {
            policy = new TaskBandwidthPolicy(); policy.setScopeId(scope); policy.setLimitMbps(limit);
            policy.setMembers(normalizeMembers(members)); policy.setRevision(1L);
            rocksDBService.putSync(POLICY + scope, policy);
        } else if (!policy.getMembers().equals(normalizeMembers(members))) {
            throw new TransferValidationException("TASK_BANDWIDTH_MEMBERS_CHANGED", "任务带宽的参与节点不能变更");
        }
        requireApplied(policy);
        return policy;
    }

    public synchronized void prepareLocal(LocalTransferRequest request) {
        String scope = request.getTaskBandwidthScopeId();
        if (scope == null) {
            scope = "local:" + request.getParentTaskId();
            // A legacy resume is not an instruction to clear its former internal share.
            // New parents already persist an explicit scope before their first dispatch.
            if (request.getTaskBandwidthLimitMbps() == null && desired(scope) == null) return;
        }
        TransferEndpoint local = request.getDirection() == TransferDirection.LOCAL_TO_OSS
                ? request.getSourceEndpoint() : request.getDestinationEndpoint();
        List<String> members = new ArrayList<>();
        for (LocalNodeBinding binding : local.getNodeBindings()) members.add(binding.getExecutionNodeIp());
        TaskBandwidthPolicy policy = initialize(scope, request.getTaskBandwidthLimitMbps(), members);
        request.setTaskBandwidthScopeId(scope); request.setTaskBandwidthLimitMbps(policy.getLimitMbps());
        registerTask(request.getParentTaskId(), scope);
    }

    public synchronized void prepareCreation(BucketPair pair, String creationId, List<String> members) {
        String scope = "creation:" + creationId;
        TaskBandwidthPolicy policy = initialize(scope, pair.getTaskBandwidthLimitMbps(),
                pair.getEligibleWorkerNodes() == null ? members : pair.getEligibleWorkerNodes());
        pair.setTaskBandwidthScopeId(scope); pair.setTaskBandwidthLimitMbps(policy.getLimitMbps());
        pair.setEligibleWorkerNodes(new ArrayList<>(policy.getMembers()));
    }

    public synchronized void requireAssignment(String scope, String worker) {
        TaskBandwidthPolicy policy = desired(scope);
        if (policy == null || !policy.getMembers().contains(worker))
            throw new TransferValidationException("TASK_BANDWIDTH_NOT_READY", "任务带宽策略未就绪或执行节点不在任务范围内");
        // Recheck the actual worker before each new batch, including after worker DB replacement.
        TaskBandwidthPolicy applied = readWorker(worker, scope);
        if (!same(policy, applied))
            throw new TransferValidationException("TASK_BANDWIDTH_NOT_READY", "执行节点尚未应用任务带宽策略：" + worker);
    }

    public synchronized void registerTask(Integer id, String scope) {
        if (id == null || scope == null) return;
        String existing = rocksDBService.get(TASK_SCOPE + id, String.class);
        if (existing != null && !scope.equals(existing))
            throw new TransferValidationException("TASK_BANDWIDTH_SCOPE_MISMATCH", "任务带宽范围不一致");
        List<Integer> ids = scopeTasks(scope);
        if (!ids.contains(id)) ids.add(id);
        Map<String,Object> writes = new LinkedHashMap<>();
        writes.put(TASK_SCOPE + id, scope); writes.put(SCOPE_TASKS + scope, ids);
        rocksDBService.putBatchSync(writes);
    }

    public Map<String,Object> get(Integer taskId) {
        return nodeConfig.withMembershipReadLock(() -> inspect(taskId));
    }
    private synchronized Map<String,Object> inspect(Integer id) {
        Resolution resolved = resolve(id);
        TaskBandwidthPolicy policy = desired(resolved.scope);
        if (policy == null) return view(resolved, null, Collections.emptyList());
        List<Map<String,Object>> nodes = new ArrayList<>();
        for (String worker : policy.getMembers()) {
            Map<String,Object> node = node(worker);
            try {
                boolean applied = same(policy, readWorker(worker, policy.getScopeId()));
                node.put("applied", applied);
                if (!applied) node.put("message", "尚未应用当前设置");
            } catch (Exception e) { node.put("applied", false); node.put("message", "节点不可达或无法读取设置"); }
            nodes.add(node);
        }
        return view(resolved, policy, nodes);
    }

    public Map<String,Object> update(Integer taskId, Double limit) {
        validateLimit(limit);
        return nodeConfig.withMembershipReadLock(() -> updateLocked(taskId, limit));
    }
    private synchronized Map<String,Object> updateLocked(Integer id, Double limit) {
        Resolution resolved = resolve(id);
        if (resolved.completed)
            throw new TransferValidationException("TASK_COMPLETED", "已完成任务不能调整带宽");
        TaskBandwidthPolicy previous = desired(resolved.scope);
        TaskBandwidthPolicy policy = new TaskBandwidthPolicy(); policy.setScopeId(resolved.scope);
        policy.setMembers(previous == null ? resolved.members : previous.getMembers());
        policy.setLimitMbps(limit); policy.setRevision(previous == null ? 1 : Math.addExact(previous.getRevision(), 1));
        rocksDBService.putSync(POLICY + policy.getScopeId(), policy);
        registerTask(id, policy.getScopeId());
        return view(resolved, policy, synchronize(policy));
    }

    private Map<String,Object> view(Resolution resolved, TaskBandwidthPolicy policy, List<Map<String,Object>> nodes) {
        Map<String,Object> data = new LinkedHashMap<>();
        data.put("limitMbps", policy == null ? null : policy.getLimitMbps());
        data.put("scopeId", resolved.scope);
        data.put("revision", policy == null ? 0 : policy.getRevision());
        data.put("sharedTaskCount", Math.max(1, scopeTasks(resolved.scope).size()));
        data.put("canEdit", !resolved.completed); data.put("nodes", nodes);
        boolean applied = policy == null || (!nodes.isEmpty() && nodes.stream().allMatch(n -> Boolean.TRUE.equals(n.get("applied"))));
        data.put("applied", applied); return data;
    }
    private void requireApplied(TaskBandwidthPolicy policy) {
        for (Map<String,Object> result : synchronize(policy))
            if (!Boolean.TRUE.equals(result.get("applied")))
                throw new TransferValidationException("TASK_BANDWIDTH_NOT_READY", "任务带宽设置未全部生效，尚未分发任务；请检查节点后重试");
    }
    private List<Map<String,Object>> synchronize(TaskBandwidthPolicy policy) {
        List<Map<String,Object>> results = new ArrayList<>();
        for (String address : policy.getMembers()) {
            Map<String,Object> node = node(address);
            try {
                TaskBandwidthPolicy installed;
                if (address.equals(config.getLocalIp())) installed = workerPolicies.apply(policy);
                else installed = responsePolicy(call(address, HttpMethod.PUT, policy, policy.getScopeId()));
                node.put("applied", same(policy, installed));
                if (!Boolean.TRUE.equals(node.get("applied"))) node.put("message", "节点未确认当前设置");
            } catch (Exception e) { node.put("applied", false); node.put("message", "节点未应用当前设置，请检查连接后重试"); }
            results.add(node);
        }
        return results;
    }
    private TaskBandwidthPolicy readWorker(String address, String scope) {
        return address.equals(config.getLocalIp()) ? workerPolicies.get(scope)
                : responsePolicy(call(address, HttpMethod.GET, null, scope));
    }
    protected Map call(String address, HttpMethod method, Object body, String scope) {
        if (!config.getWorkerIps().contains(address)) throw new IllegalStateException("worker is no longer registered");
        HttpHeaders headers = new HttpHeaders(); headers.set("X-Internal-Token", config.getInternalToken());
        headers.setContentType(MediaType.APPLICATION_JSON);
        String query;
        try { query = URLEncoder.encode(scope, "UTF-8"); } catch (Exception e) { throw new IllegalStateException(e); }
        return restTemplate.exchange(java.net.URI.create("http://" + address + ":" + config.getPort()
                + "/api/worker/task-bandwidth-policy?scopeId=" + query), method, new HttpEntity<>(body, headers), Map.class).getBody();
    }
    private TaskBandwidthPolicy responsePolicy(Map response) {
        if (response == null || !Boolean.TRUE.equals(response.get("success"))) throw new IllegalStateException("policy rejected");
        return response.get("data") == null ? null : mapper.convertValue(response.get("data"), TaskBandwidthPolicy.class);
    }
    private static boolean same(TaskBandwidthPolicy a, TaskBandwidthPolicy b) {
        return b != null && a.getRevision() == b.getRevision() && Objects.equals(a.getScopeId(), b.getScopeId())
                && Objects.equals(a.getLimitMbps(), b.getLimitMbps()) && Objects.equals(a.getMembers(), b.getMembers());
    }
    private static Map<String,Object> node(String address) {
        Map<String,Object> node = new LinkedHashMap<>(); node.put("address", address); return node;
    }
    private List<Integer> scopeTasks(String scope) {
        List<Integer> saved = rocksDBService.get(SCOPE_TASKS + scope, List.class);
        return saved == null ? new ArrayList<>() : new ArrayList<>(saved);
    }
    /** Resolve the requested node set before persisting a task or changing any bandwidth policy. */
    public static List<String> selectMembers(List<String> input, List<String> registered) {
        List<String> candidates = input == null ? registered : input;
        if (candidates == null || candidates.isEmpty())
            throw new TransferValidationException("NO_ELIGIBLE_WORKER", "至少选择一个有效的执行节点");
        Set<String> selected = new LinkedHashSet<>();
        for (String node : candidates) {
            if (node == null || node.trim().isEmpty() || registered == null || !registered.contains(node.trim()))
                throw new TransferValidationException("NO_ELIGIBLE_WORKER", "执行节点未注册或无效：" + node);
            selected.add(node.trim());
        }
        return new ArrayList<>(selected);
    }

    private List<String> normalizeMembers(List<String> input) {
        if (input == null)
            throw new TransferValidationException("NO_ELIGIBLE_WORKER", "任务带宽需要有效的执行节点");
        return selectMembers(input, config.getWorkerIps());
    }
    private static class Resolution {
        String scope; List<String> members; boolean completed;
        Resolution(String scope, List<String> members) { this.scope=scope; this.members=members; }
    }
    private boolean localCompleted(Map<String,Object> snapshot, Integer id) {
        boolean parentCompleted = false;
        Object parents = snapshot.get("tasks");
        if (parents instanceof Collection) for (Object raw : (Collection<?>)parents) {
            Map row = mapper.convertValue(raw, Map.class);
            if (id.equals(number(row.get("taskIndex"))) && "COMPLETED".equals(String.valueOf(row.get("status")))
                    && Boolean.TRUE.equals(row.get("scanComplete"))) parentCompleted = true;
        }
        if (!parentCompleted || !(snapshot.get("shards") instanceof Collection)) return false;
        boolean any = false;
        for (Object raw : (Collection<?>)snapshot.get("shards")) {
            Map row = mapper.convertValue(raw, Map.class);
            Integer parentId = number(row.get("parentTaskId"));
            if (!id.equals(parentId == null ? number(row.get("taskIndex")) : parentId)) continue;
            any = true;
            if (!"COMPLETED".equals(String.valueOf(row.get("status")))) return false;
        }
        return any;
    }

    private boolean allCompleted(List<Map> snapshot, Set<Integer> expected) {
        Set<Integer> seen = new HashSet<>();
        for (Map row : snapshot) {
            Integer id = number(row.get("taskIndex"));
            if (!expected.contains(id)) continue;
            if (!seen.add(id)) throw new TransferValidationException("TASK_ID_AMBIGUOUS", "多个节点存在相同任务 ID");
            if (!"COMPLETED".equals(String.valueOf(row.get("status")))) return false;
        }
        return seen.containsAll(expected);
    }

    private Integer number(Object value) { return value instanceof Number ? ((Number)value).intValue() : null; }

    private Resolution resolve(Integer id) {
        if (id == null || id <= 0) throw new TransferValidationException("TASK_NOT_FOUND", "任务不存在");
        LocalParentTaskRecord parent = parents.get(id);
        if (parent != null && parents.isDeleted(id)) throw new TransferValidationException("TASK_NOT_FOUND", "任务不存在或已删除");
        if (parent != null) {
            LocalTransferRequest request = parent.getRequest();
            TransferEndpoint local = request.getDirection() == TransferDirection.LOCAL_TO_OSS
                    ? request.getSourceEndpoint() : request.getDestinationEndpoint();
            List<String> members = new ArrayList<>();
            for (LocalNodeBinding binding : local.getNodeBindings()) members.add(binding.getExecutionNodeIp());
            Resolution result = new Resolution(request.getTaskBandwidthScopeId() == null
                    ? "local:"+id : request.getTaskBandwidthScopeId(), normalizeMembers(members));
            Map<String,Object> snapshot = localTransfers.aggregateStatus(id, null, null, null, null);
            if (Boolean.TRUE.equals(snapshot.get("partial")))
                throw new DownstreamWorkerException("WORKER_UNAVAILABLE", "部分节点不可达，无法确认任务状态", null);
            result.completed = localCompleted(snapshot, id);
            return result;
        }
        String knownScope = rocksDBService.get(TASK_SCOPE + id, String.class);
        boolean unavailable = false;
        List<Resolution> matches = new ArrayList<>();
        List<Map> taskSnapshot = new ArrayList<>();
        for (String address : config.getWorkerIps()) {
            try {
                HttpHeaders headers = new HttpHeaders(); headers.set("X-Internal-Token", config.getInternalToken());
                Map response = restTemplate.exchange("http://"+address+":"+config.getPort()+"/api/worker/status",
                        HttpMethod.GET, new HttpEntity<>(headers), Map.class).getBody();
                if (response == null || Boolean.FALSE.equals(response.get("success"))
                        || !(response.get("tasks") instanceof Collection)) throw new IllegalStateException("invalid status");
                for (Object raw : (Collection<?>)response.get("tasks")) {
                    if (!(raw instanceof Map)) throw new IllegalStateException("invalid task record");
                    Map task = (Map)raw;
                    if (!(task.get("taskIndex") instanceof Number)) throw new IllegalStateException("missing task identity");
                    Number taskId = (Number) task.get("taskIndex");
                    if (taskId.doubleValue() != taskId.intValue()) throw new IllegalStateException("invalid task identity");
                    taskSnapshot.add(task);
                    if (taskId.intValue() != id) continue;
                    String scope = knownScope != null ? knownScope : (String)task.get("taskBandwidthScopeId");
                    if (scope == null) scope="task:"+id;
                    TaskBandwidthPolicy policy = desired(scope);
                    matches.add(new Resolution(scope, policy == null ? Collections.singletonList(address) : policy.getMembers()));
                }
            } catch (Exception e) { unavailable=true; }
        }
        // Legacy direct imports can allocate the same public ID on separate workers.
        // Neither a first match nor a saved scope mapping proves unique ownership.
        if (unavailable) throw new DownstreamWorkerException("WORKER_UNAVAILABLE", "部分节点不可达，无法确认任务带宽设置", null);
        if (matches.size() > 1) throw new TransferValidationException("TASK_ID_AMBIGUOUS", "多个节点存在相同任务 ID，请先处理重复记录再设置任务带宽");
        if (matches.size() == 1) {
            Resolution result = matches.get(0);
            TaskCreationService.Creation creation = creations == null ? null : creations.findByTaskId(id);
            Set<Integer> expected = new LinkedHashSet<>(scopeTasks(result.scope));
            boolean scanComplete = true;
            if (creation != null) {
                if (creation.isDeleted()) throw new TransferValidationException("TASK_NOT_FOUND", "任务不存在或已删除");
                expected.addAll(creation.getTaskIds());
                scanComplete = Boolean.TRUE.equals(creation.getScanComplete()) && !creation.isStopped();
            }
            if (expected.isEmpty()) expected.add(id);
            result.completed = scanComplete && allCompleted(taskSnapshot, expected);
            return result;
        }
        throw new TransferValidationException("TASK_NOT_FOUND", "任务不存在或已删除");
    }
}
