package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.agent.AgentRegistryService;
import com.transfer.tools.config.DistributedConfig;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.*;
import org.springframework.http.client.SimpleClientHttpRequestFactory;
import org.springframework.stereotype.Service;
import org.springframework.web.client.*;
import org.springframework.web.server.ResponseStatusException;
import org.springframework.web.util.UriComponentsBuilder;
import javax.annotation.PreDestroy;
import java.net.*;
import java.time.Instant;
import java.util.*;
import java.util.concurrent.*;

@Service
public class MasterTaskChildrenService {
    @Autowired private DistributedConfig config;
    @Autowired private TaskCreationService creations;
    @Autowired private LocalParentTaskService parents;
    @Autowired(required = false) private AgentRegistryService agents;
    private final RestTemplate http = new RestTemplate(factory());
    private final ExecutorService queries = Executors.newFixedThreadPool(6, runnable -> {
        Thread thread = new Thread(runnable, "child-task-query"); thread.setDaemon(true); return thread;
    });
    private final Map<String, Snapshot> legacy = bounded(64);
    private final Map<String, Snapshot> capabilities = bounded(128);
    private final Map<String, Map<Integer, Integer>> ordinalIndexes = bounded(64);
    private final Map<String, Boolean> legacyScopes = bounded(128);
    private final ObjectMapper mapper = new ObjectMapper();
    private static final class Snapshot { volatile Map<String, Object> data; volatile long at; }
    private static <T> Map<String, T> bounded(final int limit) {
        return Collections.synchronizedMap(new LinkedHashMap<String, T>(16, .75f, true) {
            @Override protected boolean removeEldestEntry(Map.Entry<String, T> entry) { return size() > limit; }
        });
    }
    private static SimpleClientHttpRequestFactory factory() {
        SimpleClientHttpRequestFactory value = new SimpleClientHttpRequestFactory() {
            @Override protected void prepareConnection(HttpURLConnection connection, String method) throws java.io.IOException {
                super.prepareConnection(connection, method); connection.setInstanceFollowRedirects(false);
            }
        };
        value.setConnectTimeout(3000); value.setReadTimeout(18000); return value;
    }
    @PreDestroy public void close() { queries.shutdownNow(); }
    public Map<String, Object> query(int parent, int offset, int limit, String node) {
        if (parent < 1 || offset < 0 || limit < 1 || limit > 200)
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "Invalid child task window");
        TaskCreationService.Creation creation = creations.findByTaskId(parent);
        if ((creation != null && creation.isDeleted()) || parents.isDeleted(parent))
            throw new ResponseStatusException(HttpStatus.NOT_FOUND, "Task not found");
        if (creation != null && !Objects.equals(parent, creation.getPublicTaskId()))
            throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "Use the parent task ID");
        List<Integer> ids = creation == null ? null : new ArrayList<>(creation.getTaskIds());
        boolean singleNode = node != null && !node.isEmpty();
        List<String> workers = new ArrayList<>(config.getWorkerIps());
        if (node != null && !node.isEmpty()) {
            if (!workers.contains(node)) throw new ResponseStatusException(HttpStatus.BAD_REQUEST, "Node is not registered");
            workers = Collections.singletonList(node);
        }
        String indexKey = creation == null ? null : creation.getId() + ":" + ids.hashCode() + ":"
                + String.join(",", workers.stream().map(this::identity).toArray(String[]::new));
        Map<Integer, Integer> known = indexKey == null ? null : ordinalIndexes.get(indexKey);
        boolean includeIndex = creation != null && known == null;
        Map<Integer, Integer> numbers = known == null ? new LinkedHashMap<>() : new LinkedHashMap<>(known);
        List<Future<Map<String, Object>>> futures = new ArrayList<>();
        for (String worker : workers) futures.add(queries.submit(() -> worker(worker, parent, offset, limit,
                ids, creation == null ? null : creation.getId(), includeIndex)));
        List<Map<String, Object>> items = new ArrayList<>();
        List<String> unavailable = new ArrayList<>();
        Map<String, Object> snapshots = new LinkedHashMap<>();
        int total = ids == null ? 0 : ids.size(); long oldest = Long.MAX_VALUE; boolean cached = false;
        boolean indexConflict = false;
        Set<Integer> duplicateIds = new LinkedHashSet<>(), seen = new HashSet<>();
        for (int i = 0; i < workers.size(); i++) {
            try {
                Map<String, Object> data = futures.get(i).get(22, TimeUnit.SECONDS);
                long at = ((Number) data.get("snapshotAtMillis")).longValue();
                oldest = Math.min(oldest, at); cached |= Boolean.TRUE.equals(data.get("cached"));
                Map<String, Object> snapshot = new LinkedHashMap<>();
                snapshot.put("snapshotAt", Instant.ofEpochMilli(at).toString()); snapshot.put("cached", data.get("cached"));
                snapshots.put(workers.get(i), snapshot);
                total = Math.max(total, ((Number) data.get("totalCount")).intValue());
                if (data.get("taskNumbers") instanceof Map) for (Object entry : ((Map) data.get("taskNumbers")).entrySet()) {
                    Map.Entry value = (Map.Entry) entry;
                    int id = Integer.parseInt(String.valueOf(value.getKey()));
                    if (ids != null && !ids.contains(id)) continue;
                    int number = ((Number)value.getValue()).intValue();
                    Integer old = numbers.put(id, number);
                    if (old != null && old != number) indexConflict = true;
                }
                for (Object value : (Collection<?>) data.get("items")) {
                    Map<String, Object> child = new LinkedHashMap<>((Map<String, Object>) value);
                    int id = ((Number) child.get("taskIndex")).intValue();
                    if (ids != null && !ids.contains(id)) continue;
                    int number = child.get("subtaskNumber") instanceof Number ? ((Number) child.get("subtaskNumber")).intValue()
                            : numbers.getOrDefault(id, ids == null ? 0 : ids.indexOf(id) + 1);
                    if (number <= offset || number > (long) offset + limit) continue;
                    numbers.put(id, number);
                    if (!seen.add(id)) duplicateIds.add(id);
                    child.put("subtaskNumber", number); child.put("childTaskId", parent + "-" + number);
                    child.put("workerNode", workers.get(i)); child.put("executionNodeIp", workers.get(i)); items.add(child);
                }
            } catch (Exception failure) { futures.get(i).cancel(true); unavailable.add(workers.get(i)); }
        }
        List<Integer> missing = new ArrayList<>();
        if (ids != null) {
            for (Integer id : ids) {
                if (singleNode && !numbers.containsKey(id)) continue;
                int ordinal = numbers.getOrDefault(id, ids.indexOf(id) + 1);
                if (ordinal > offset && ordinal <= (long)offset + limit && !seen.contains(id)) missing.add(id);
            }
            if (indexKey != null && unavailable.isEmpty() && !indexConflict && (singleNode || numbers.keySet().containsAll(ids)))
                ordinalIndexes.put(indexKey, new LinkedHashMap<>(numbers));
        }
        items.sort(Comparator.comparingInt((Map<String, Object> value) -> ((Number) value.get("subtaskNumber")).intValue())
                .thenComparing(value -> String.valueOf(value.get("workerNode")))
                .thenComparing(value -> String.valueOf(value.get("taskIndex"))));
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("parentTaskId", parent); result.put("items", items); result.put("totalCount", total);
        result.put("offset", offset); result.put("limit", limit); result.put("hasMore", total > (long) offset + limit);
        result.put("partial", !unavailable.isEmpty() || !missing.isEmpty() || !duplicateIds.isEmpty() || indexConflict);
        result.put("missingTaskIds", missing); result.put("duplicateTaskIds", duplicateIds);
        result.put("snapshotComplete", !Boolean.TRUE.equals(result.get("partial"))); result.put("unreachableWorkers", unavailable);
        result.put("cached", cached); result.put("workerSnapshots", snapshots);
        result.put("snapshotAt", oldest == Long.MAX_VALUE ? null : Instant.ofEpochMilli(oldest).toString());
        return result;
    }
    private Map<String, Object> worker(String address, int parent, int offset, int limit, List<Integer> ids,
                                       String creationId, boolean includeIndex) {
        long now = System.currentTimeMillis();
        if (ids != null && ids.isEmpty()) return result(Collections.emptyList(), 0, now, false);
        String identity = identity(address);
        String recordScope = identity + ":creation:" + creationId;
        if (!Boolean.TRUE.equals(legacyScopes.get(recordScope)) && paged(address, identity)) {
            UriComponentsBuilder path = UriComponentsBuilder.fromPath("/api/worker/task-children")
                    .queryParam("parentTaskId", parent).queryParam("offset", offset).queryParam("limit", limit);
            if (creationId != null) path.queryParam("creationId", creationId).queryParam("includeIndex", includeIndex);
            try {
                Map<String, Object> value = envelope(get(address, path.build().toUriString()));
                if (!(value.get("items") instanceof Collection) || !(value.get("totalCount") instanceof Number))
                    throw new IllegalStateException("Incomplete child task response");
                if (creationId == null || !includeIndex || (value.get("taskNumbers") instanceof Map && !((Map)value.get("taskNumbers")).isEmpty())) {
                    Map<String, Object> result = result((Collection<?>) value.get("items"), ((Number) value.get("totalCount")).intValue(), System.currentTimeMillis(), false);
                    if (includeIndex) result.put("taskNumbers", value.get("taskNumbers"));
                    return result;
                }
                // Pre-creation legacy OSS records do not carry the creation UUID.

            } catch (HttpClientErrorException failure) {
                if (failure.getStatusCode() != HttpStatus.NOT_FOUND) throw failure;
                capabilities.remove(identity);
            }
        }
        String cacheKey = identity + (ids == null ? ":local:" + parent : ":oss");
        Snapshot snapshot;
        synchronized (legacy) { snapshot = legacy.computeIfAbsent(cacheKey, key -> new Snapshot()); }
        boolean reused; Map<String, Object> legacyData; long legacyAt;
        synchronized (snapshot) {
            reused = snapshot.data != null && now - snapshot.at < 10000;
            if (!reused) {
                Map<String, Object> response = get(address, ids == null ? "/api/worker/local-transfers/status?taskId=" + parent
                        : "/api/worker/status");
                Map<String, Object> value = ids == null ? envelope(response) : response;
                if (Boolean.FALSE.equals(value.get("success")) || !(value.get("tasks") instanceof Collection))
                    throw new IllegalStateException("Incomplete legacy child task response");
                snapshot.data = value; snapshot.at = System.currentTimeMillis();
            }
            legacyData = snapshot.data; legacyAt = snapshot.at;
        }
        List<Map<String, Object>> items = new ArrayList<>(); Map<Integer, Integer> taskNumbers = new LinkedHashMap<>(); int total = 0;
        for (Object row : (Collection<?>) legacyData.get("tasks")) {
            Map<String, Object> child = new LinkedHashMap<>(mapper.convertValue(row, Map.class));
            if ("discovery".equals(child.get("shardId"))) continue;
            if (ids != null) {
                int id = ((Number)child.get("taskIndex")).intValue();
                if (!ids.contains(id)) continue;
                Integer index = WorkerTaskChildrenService.index(child.get("shardIndex") instanceof Number
                        ? ((Number)child.get("shardIndex")).intValue() : null, (String)child.get("shardId"));
                int ordinal = index == null ? ids.indexOf(id) + 1 : index + 1;
                taskNumbers.put(id, ordinal); total = Math.max(total,ordinal); child.put("subtaskNumber",ordinal);
                if (ordinal <= offset || ordinal > (long)offset + limit) continue;
            } else {
                Object parentId = child.get("parentTaskId") == null ? child.get("taskIndex") : child.get("parentTaskId");
                if (!(parentId instanceof Number) || ((Number) parentId).intValue() != parent) continue;
                Integer index = WorkerTaskChildrenService.index(child.get("shardIndex") instanceof Number
                        ? ((Number) child.get("shardIndex")).intValue() : null, (String) child.get("shardId"));
                int number = index == null ? ++total : index + 1;
                total = Math.max(total, number); child.put("subtaskNumber", number);
                if (number <= offset || number > (long) offset + limit) continue;
                // Old local status contains execution payloads; only publish display fields.
                child.remove("localFileEntries"); child.remove("sourceKeyMap"); child.remove("retryKeys");
            }
            items.add(child);
        }
        Map<String, Object> result = result(items, total, legacyAt, reused);
        if (creationId != null && !taskNumbers.isEmpty()) legacyScopes.put(recordScope, true);
        if (includeIndex) result.put("taskNumbers", taskNumbers);
        return result;
    }
    private boolean paged(String address, String identity) {
        if (Objects.equals(address, config.getLocalIp())) return true;
        boolean agent = agents != null && agents.isAgent(address);
        Snapshot snapshot = null;
        if (agent) synchronized (capabilities) { snapshot = capabilities.computeIfAbsent(identity, key -> new Snapshot()); }
        if (snapshot == null) snapshot = new Snapshot();
        synchronized (snapshot) {
            if (snapshot.data == null || System.currentTimeMillis() - snapshot.at >= 60000) {
                snapshot.data = get(address, "/api/worker/node-info"); snapshot.at = System.currentTimeMillis();
            }
            return Boolean.TRUE.equals(snapshot.data.get("taskChildren"));
        }
    }
    private String identity(String address) {
        String enrollment = agents == null ? null : agents.currentEnrollment(address);
        return address + ":" + (enrollment == null ? "traditional" : enrollment);
    }
    private Map<String, Object> get(String address, String path) {
        HttpHeaders headers = new HttpHeaders(); headers.set("X-Internal-Token", config.getInternalToken());
        ResponseEntity<Map> response = http.exchange(URI.create("http://" + address + ":" + config.getPort() + path),
                HttpMethod.GET, new HttpEntity<>(headers), Map.class);
        if (!response.getStatusCode().is2xxSuccessful() || response.getBody() == null
                || Boolean.FALSE.equals(response.getBody().get("success"))) throw new IllegalStateException("Node query failed");
        return response.getBody();
    }
    private Map<String, Object> envelope(Map<String, Object> response) {
        if (!Boolean.TRUE.equals(response.get("success")) || !(response.get("data") instanceof Map))
            throw new IllegalStateException("Node query failed");
        return (Map<String, Object>) response.get("data");
    }
    private Map<String, Object> result(Collection<?> items, int total, long at, boolean cached) {
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("items", items); result.put("totalCount", total); result.put("snapshotAtMillis", at); result.put("cached", cached);
        return result;
    }
}
