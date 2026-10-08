package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.NodeConfig;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.S3ClientConfig;
import com.transfer.tools.model.LocalParentTaskRecord;
import com.transfer.tools.model.TaskBandwidthPolicy;
import lombok.Data;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.*;
import org.springframework.http.client.SimpleClientHttpRequestFactory;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;
import java.io.IOException;
import java.net.HttpURLConnection;
import java.security.SecureRandom;
import java.util.*;

/** Manages membership without exposing cluster credentials to the browser or an unverified address. */
@Service
public class NodeRegistryService {
    @Autowired private NodeConfigService nodeConfig;
    @Autowired private DistributedConfig config;
    @Autowired private MasterLocalTransferService localTransfers;
    @Autowired private S3ClientManager s3Clients;
    @Autowired private LocalParentTaskService parentRecords;
    @Autowired private MasterTaskBandwidthService taskBandwidth;
    @Autowired(required = false) private com.transfer.tools.agent.AgentRegistryService agents;
    private final RestTemplate http;
    @Data public static class Change {
        private String address;
        private String label;
        private List<String> advertisedLocalRoots;
    }
    public NodeRegistryService() {
        SimpleClientHttpRequestFactory factory = new SimpleClientHttpRequestFactory() {
            @Override protected void prepareConnection(HttpURLConnection connection, String method) throws IOException {
                super.prepareConnection(connection, method);
                connection.setInstanceFollowRedirects(false);
            }
        };
        factory.setConnectTimeout(3000); factory.setReadTimeout(5000);
        http = new RestTemplate(factory);
    }
    public Map<String, Object> list() {
        NodeConfig saved = nodeConfig.get();
        Map<String, Object> result = new LinkedHashMap<>();
        List<Map<String, Object>> nodes = new ArrayList<>();
        for (String address : saved.getWorkerIps()) {
            Map<String, Object> node = new LinkedHashMap<>();
            node.put("address", address); node.put("label", saved.getNodeLabels().getOrDefault(address, ""));
            node.put("master", address.equals(config.getMasterIp()));
            boolean agent = agents != null && agents.isAgent(address);
            node.put("agent", agent);
            if (agent) {
                node.put("online", agents.online(address));
                long seen = agents.lastSeen(address);
                node.put("lastSeen", seen == 0 ? null : java.time.Instant.ofEpochMilli(seen).toString());
            }
            nodes.add(node);
        }
        result.put("nodes", nodes); result.put("port", config.getPort());
        result.put("masterWebUrl", config.getMasterWebUrl());
        return result;
    }
    public Map<String, Object> create(Change change) {
        return nodeConfig.withMembershipWriteLock(() -> {
            config.getMasterWebUrl(); // Fail before any remote/configuration mutation.
            validate(change);
            NodeConfig saved = nodeConfig.get();
            if (saved.getWorkerIps().contains(change.address)) throw error("NODE_EXISTS", "节点已存在");
            verifyCandidate(change.address);
            synchronizeStorage(change.address);
            updateRoots(change.address, change.advertisedLocalRoots);
            saved.getWorkerIps().add(change.address);
            saved.getNodeLabels().put(change.address, label(change));
            nodeConfig.replaceMembership(saved.getWorkerIps(), saved.getNodeLabels());
            return list();
        });
    }
    public Map<String, Object> update(String oldAddress, Change change) {
        return nodeConfig.withMembershipWriteLock(() -> {
            config.getMasterWebUrl();
            validate(change);
            NodeConfig saved = requireRegistered(oldAddress);
            boolean moved = !oldAddress.equals(change.address);
            if (moved && agents != null && agents.isAgent(oldAddress))
                throw error("AGENT_IDENTITY_IMMUTABLE", "Agent 地址与加入凭据绑定，请删除后重新加入以更换地址");
            if (moved) {
                requireRemovable(oldAddress);
                if (saved.getWorkerIps().contains(change.address)) throw error("NODE_EXISTS", "目标地址已注册");
                verifyCandidate(change.address);
                synchronizeStorage(change.address);
            }
            updateRoots(change.address, change.advertisedLocalRoots);
            int position = saved.getWorkerIps().indexOf(oldAddress);
            saved.getWorkerIps().set(position, change.address);
            saved.getNodeLabels().remove(oldAddress);
            saved.getNodeLabels().put(change.address, label(change));
            nodeConfig.replaceMembership(saved.getWorkerIps(), saved.getNodeLabels());
            return list();
        });
    }
    public Map<String, Object> delete(String address) {
        return nodeConfig.withMembershipWriteLock(() -> {
            config.getMasterWebUrl();
            NodeConfig saved = requireRegistered(address);
            requireRemovable(address);
            saved.getWorkerIps().remove(address); saved.getNodeLabels().remove(address);
            if (agents != null && agents.isAgent(address))
                agents.revokeMembership(address, saved.getWorkerIps(), saved.getNodeLabels());
            else nodeConfig.replaceMembership(saved.getWorkerIps(), saved.getNodeLabels());
            return list();
        });
    }
    private NodeConfig requireRegistered(String address) {
        NodeConfig saved = nodeConfig.get();
        if (!saved.getWorkerIps().contains(address)) throw error("NODE_NOT_FOUND", "节点未注册");
        return saved;
    }
    private void requireRemovable(String address) {
        if (address.equals(config.getMasterIp())) throw error("MASTER_NODE_REQUIRED", "主节点同时承担集群管理，不可删除或更换地址");
        // A parent may reference a node that never received a shard (e.g. empty discovery).
        for (LocalParentTaskRecord record : parentRecords.getAll()) {
            if (referencesNode(record.getRequest(), address) || referencesNode(parentRecords.exploreCheckpoint(record.getParentTaskId()), address))
                throw error("NODE_HISTORY_REMAINS", "节点仍被父任务的目录绑定或发现记录引用，请先处理任务记录");
        }
        for (SubTaskStatus record : parentRecords.getDeleted()) {
            if (referencesNode(record, address))
                throw error("NODE_HISTORY_REMAINS", "节点仍被回收站父任务引用，请先处理任务记录");
        }
        // Parent discovery may exist before its first worker shard. Its aggregated state is required.
        Map<String, Object> parents = localTransfers.aggregateStatus(null, null, null, null, null);
        if (Boolean.TRUE.equals(parents.get("partial")) || Boolean.FALSE.equals(parents.get("complete"))
                || !(parents.get("tasks") instanceof Collection))
            throw error("NODE_STATE_INCOMPLETE", "部分节点不可达，无法确认任务状态，未修改节点");
        for (Object item : (Collection<?>) parents.get("tasks")) {
            if (referencesNode(item, address))
                throw error("NODE_HISTORY_REMAINS", "节点仍被任务目录绑定引用，请先处理任务记录");
            Object status = item instanceof SubTaskStatus ? ((SubTaskStatus) item).getStatus()
                    : item instanceof Map ? ((Map<?, ?>) item).get("status") : null;
            if (!terminal(status))
                throw error("NODE_TASKS_UNFINISHED", "集群仍有待执行或运行的本地任务，暂不能删除或换址");
        }
        for (String worker : config.getWorkerIps()) {
            Map<String, Object> status = trusted(worker, "/api/worker/status", HttpMethod.GET, null);
            Object tasks = status.get("tasks");
            if (!(tasks instanceof Collection)) throw error("NODE_STATE_INCOMPLETE", "节点未返回完整任务记录");
            for (Object task : (Collection<?>) tasks) {
                if (referencesNode(task, address))
                    throw error("NODE_HISTORY_REMAINS", "节点仍被任务或任务带宽范围引用，请先处理任务记录");
                if (!(task instanceof Map) || !terminal(((Map<?, ?>) task).get("status")))
                    throw error("NODE_TASKS_UNFINISHED", "集群仍有待执行或运行的任务，暂不能删除或换址");
            }
            Map<String, Object> runtime = trusted(worker, "/api/worker/worker-status", HttpMethod.GET, null);
            Object queues = runtime.get("queueStatus");
            if (!(queues instanceof Map)) throw error("NODE_STATE_INCOMPLETE", "节点未返回队列状态");
            for (String name : Arrays.asList("list", "put", "progress", "failedKey", "fallback")) {
                Object value = ((Map<?, ?>) queues).get(name + "QueueSize");
                if (!(value instanceof Number) || ((Number) value).longValue() != 0)
                    throw error("NODE_BUSY", "节点队列尚未清空，暂不能删除或换址");
            }
            Map<String, Object> deleted = trusted(worker, "/api/worker/deleted-tasks", HttpMethod.GET, null);
            if (!(deleted.get("tasks") instanceof Collection)) throw error("NODE_STATE_INCOMPLETE", "节点回收站未完整返回");
            for (Object task : (Collection<?>) deleted.get("tasks")) {
                if (referencesNode(task, address))
                    throw error("NODE_HISTORY_REMAINS", "节点仍被回收站任务或任务带宽范围引用，请先处理任务记录");
            }
            if (worker.equals(address)) {
                if (!((Collection<?>) tasks).isEmpty() || !((Collection<?>) deleted.get("tasks")).isEmpty())
                    throw error("NODE_HISTORY_REMAINS", "节点仍有任务或回收站记录。请先处理这些记录，保留节点以便查询历史和导出报告");
                requireNoLocalHistory(address);
            }
        }
    }
    private boolean terminal(Object status) {
        return Arrays.asList("COMPLETED", "FAILED", "PARTIAL_FAILED", "STOPPED").contains(String.valueOf(status));
    }
    private boolean referencesNode(Object value, String address) {
        if (value == null) return false;
        Object json = new com.fasterxml.jackson.databind.ObjectMapper().convertValue(value, Object.class);
        return referencesJson(json, address);
    }
    private boolean referencesJson(Object value, String address) {
        if (value instanceof Collection) {
            for (Object item : (Collection<?>) value) if (referencesJson(item, address)) return true;
        } else if (value instanceof Map) {
            Map<?, ?> record = (Map<?, ?>) value;
            if (taskBandwidth != null) {
                Object scopeValue = record.get("taskBandwidthScopeId");
                Object taskId = record.containsKey("parentTaskId") && record.get("parentTaskId") != null
                        ? record.get("parentTaskId") : record.get("taskIndex");
                String scope = scopeValue instanceof String ? (String) scopeValue
                        : taskId instanceof Number ? (record.get("parentTaskId") != null ? "local:" : "task:") + taskId : null;
                TaskBandwidthPolicy policy = taskBandwidth.desired(scope);
                if (policy != null && policy.getMembers().contains(address)) return true;
            }
            for (Map.Entry<?, ?> field : record.entrySet()) {
                String key = String.valueOf(field.getKey());
                if (Arrays.asList("executionNodeIp", "workerNode", "discoveryWorker").contains(key)
                        && address.equals(field.getValue())) return true;
                if (Arrays.asList("eligibleWorkerNodes", "participatingWorkerNodes").contains(key)
                        && field.getValue() instanceof Collection && ((Collection<?>) field.getValue()).contains(address)) return true;
                if (referencesJson(field.getValue(), address)) return true;
            }
        }
        return false;
    }
    private void validate(Change change) {
        if (change == null || !validAddress(change.address)) throw error("INVALID_NODE_ADDRESS", "请输入规范的IPv4地址；端口使用集群统一配置");
        if (change.label != null && (change.label.length() > 80 || change.label.indexOf('\n') >= 0 || change.label.indexOf('\r') >= 0))
            throw error("INVALID_NODE_LABEL", "节点名称不能超过80个字符或包含换行");
        if (change.advertisedLocalRoots != null) change.advertisedLocalRoots = NodeConfigService.validateRoots(change.advertisedLocalRoots);
    }
    static boolean validAddress(String address) {
        if (address == null || !address.matches("(?:0|[1-9][0-9]{0,2})(?:\\.(?:0|[1-9][0-9]{0,2})){3}")) return false;
        String[] parts = address.split("\\.");
        for (String part : parts) if (Integer.parseInt(part) > 255) return false;
        int first = Integer.parseInt(parts[0]);
        return first != 0 && first < 224 && !"255.255.255.255".equals(address);
    }
    private String label(Change change) { return change.label == null ? "" : change.label.trim(); }
    private void verifyCandidate(String address) {
        byte[] random = new byte[32]; new SecureRandom().nextBytes(random);
        StringBuilder nonce = new StringBuilder();
        for (byte item : random) nonce.append(String.format("%02x", item & 0xff));
        Map<String, Object> proof = exchange(address, "/api/node-registration/proof?nonce=" + nonce, HttpMethod.GET, null, false);
        if (!NodeIdentityService.valid(proof, nonce.toString(), address, config))
            throw error("NODE_IDENTITY_MISMATCH", "节点身份校验失败：检查集群共享密钥、主节点地址、当前节点地址及Worker角色");
        // Only a valid, address-bound proof allows a request carrying the internal token.
        Map<String, Object> runtime = trusted(address, "/api/worker/worker-status", HttpMethod.GET, null);
        if (!(runtime.get("queueStatus") instanceof Map)) throw error("NODE_STATE_INCOMPLETE", "新节点未返回队列状态");
        for (String name : Arrays.asList("list", "put", "progress", "failedKey", "fallback")) {
            Object value = ((Map<?, ?>) runtime.get("queueStatus")).get(name + "QueueSize");
            if (!(value instanceof Number) || ((Number) value).longValue() != 0)
                throw error("NODE_BUSY", "新节点队列未清空，未加入成员列表");
        }
        for (String path : Arrays.asList("/api/worker/status", "/api/worker/deleted-tasks")) {
            Map<String, Object> status = trusted(address, path, HttpMethod.GET, null);
            if (!(status.get("tasks") instanceof Collection)) throw error("NODE_STATE_INCOMPLETE", "新节点未返回完整任务记录");
            if (!((Collection<?>) status.get("tasks")).isEmpty())
                throw error("NODE_HISTORY_REMAINS", "新节点仍有任务或回收站记录，暂不支持合并已有任务，请先处理记录");
        }
        requireNoLocalHistory(address);
    }
    private void requireNoLocalHistory(String address) {
        Map<String, Object> result = trusted(address, "/api/worker/local-transfers/status", HttpMethod.GET, null);
        Object data = result.get("data");
        if (!(data instanceof Map) || !(((Map<?, ?>) data).get("tasks") instanceof Collection))
            throw error("NODE_STATE_INCOMPLETE", "节点未返回本地发现记录，未修改成员列表");
        if (!((Collection<?>) ((Map<?, ?>) data).get("tasks")).isEmpty())
            throw error("NODE_HISTORY_REMAINS", "节点仍有本地发现或子任务记录，请先处理记录");
    }
    private void updateRoots(String address, List<String> roots) {
        if (roots == null) return;
        NodeConfig change = new NodeConfig(); change.setAdvertisedLocalRoots(roots);
        if (address.equals(config.getLocalIp())) nodeConfig.update(change);
        else trusted(address, "/api/worker/node-config", HttpMethod.PUT, change);
    }
    private void synchronizeStorage(String address) { synchronizeStorage(address, false); }
    private void synchronizeStorage(String address, boolean masterAuthority) {
        if (address.equals(config.getLocalIp())) return;
        Map<String, Object> response = trusted(address, "/api/worker/s3-config", HttpMethod.GET, null);
        if (!(response.get("configs") instanceof Collection))
            throw error("NODE_CONFIG_INCOMPLETE", "新节点未返回完整存储配置，未加入成员列表");
        Map<String, Map<?, ?>> existing = new HashMap<>();
        for (Object value : (Collection<?>) response.get("configs")) {
            if (!(value instanceof Map) || !(((Map<?, ?>) value).get("alias") instanceof String))
                throw error("NODE_CONFIG_INCOMPLETE", "新节点存储配置无效，未加入成员列表");
            Map<?, ?> item = (Map<?, ?>) value;
            existing.put((String) item.get("alias"), item);
        }
        List<Map<String, Object>> missing = new ArrayList<>();
        for (S3ClientConfig item : s3Clients.getAllConfigs()) {
            Map<String, Object> expected = new LinkedHashMap<>();
            expected.put("alias", item.getAlias()); expected.put("endpoint", item.getEndpoint());
            expected.put("accessKey", item.getAccessKey()); expected.put("secretKey", item.getSecretKey());
            expected.put("useV2SignerType", item.getUseV2SignerType()); expected.put("pathStyleEnable", item.getPathStyleEnable());
            Map<?, ?> actual = existing.get(item.getAlias());
            if (actual != null) {
                for (Map.Entry<String, Object> field : expected.entrySet()) {
                    if (!Objects.equals(field.getValue(), actual.get(field.getKey())) && !masterAuthority)
                        throw error("NODE_STORAGE_CONFLICT", "新节点存在同名但不同的存储连接：" + item.getAlias() + "，未覆盖或加入成员列表");
                }
                if (masterAuthority) {
                    boolean different = expected.entrySet().stream().anyMatch(field -> !Objects.equals(field.getValue(), actual.get(field.getKey())));
                    if (different) {
                        expected.put("validate", false);
                        trusted(address, "/api/worker/s3-config/" + item.getAlias(), HttpMethod.PUT, expected);
                    }
                }
            } else missing.add(expected);
        }
        for (Map<String, Object> item : missing) {
            item.put("validate", false); // Existing credentials were already validated; this creates the same client config.
            try { trusted(address, "/api/worker/s3-config", HttpMethod.POST, item); }
            catch (RuntimeException e) {
                throw error("NODE_STORAGE_SYNC_FAILED", "存储连接同步失败，未加入成员列表；新节点已接收的连接会保留，可检查后重试");
            }
        }
        if (masterAuthority) {
            Set<String> wanted = new HashSet<>();
            for (S3ClientConfig item : s3Clients.getAllConfigs()) wanted.add(item.getAlias());
            for (String alias : existing.keySet()) if (!wanted.contains(alias))
                trusted(address, "/api/worker/s3-config/" + alias, HttpMethod.DELETE, null);
        }
    }
    public void synchronizeAgentStorage(String address) { synchronizeStorage(address, true); }
    private Map<String, Object> trusted(String address, String path, HttpMethod method, Object body) {
        return exchange(address, path, method, body, true);
    }
    @SuppressWarnings("unchecked")
    Map<String, Object> exchange(String address, String path, HttpMethod method, Object body, boolean authenticated) {
        try {
            HttpHeaders headers = new HttpHeaders();
            if (authenticated) headers.set("X-Internal-Token", config.getInternalToken());
            if (body != null) headers.setContentType(MediaType.APPLICATION_JSON);
            ResponseEntity<Map> response = http.exchange("http://" + address + ":" + config.getPort() + path,
                    method, new HttpEntity<>(body, headers), Map.class);
            Map<String, Object> result = response.getBody();
            if (!response.getStatusCode().is2xxSuccessful() || result == null
                    || (authenticated && !Boolean.TRUE.equals(result.get("success"))
                        && !legacyTaskSnapshot(path, method, result))
                    || Boolean.FALSE.equals(result.get("success")))
                throw error("NODE_UNAVAILABLE", "节点请求未成功：" + address);
            return result;
        } catch (TransferValidationException e) { throw e; }
        catch (Exception e) { throw error("NODE_UNAVAILABLE", "无法确认节点状态：" + address + "，未修改成员列表"); }
    }
    private boolean legacyTaskSnapshot(String path, HttpMethod method, Map<String, Object> result) {
        // This long-standing read endpoint returns MigrationStatusVO without an envelope.
        if (method != HttpMethod.GET || !"/api/worker/status".equals(path)
                || result.containsKey("success") || !(result.get("tasks") instanceof Collection)) return false;
        for (String key : Arrays.asList("totalTasks", "pendingTasks", "runningTasks", "listDoneTasks",
                "completedTasks", "failedTasks", "partialFailedTasks")) {
            Object value = result.get(key);
            if (!(value instanceof Number) || !Double.isFinite(((Number) value).doubleValue())
                    || ((Number) value).doubleValue() < 0
                    || ((Number) value).doubleValue() != ((Number) value).longValue()) return false;
        }
        return ((Number) result.get("totalTasks")).longValue() == ((Collection<?>) result.get("tasks")).size();
    }
    private static TransferValidationException error(String code, String message) { return new TransferValidationException(code, message); }
}
