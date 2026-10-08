package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.model.BucketPair;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;
import java.text.SimpleDateFormat;
import java.util.*;

/** Presents ranges from one acknowledged creation as one transfer without rewriting worker records. */
@Service
public class OssTaskGroupService {
    @Autowired private TaskCreationService taskCreationService;
    private final ObjectMapper mapper = new ObjectMapper();
    private static final List<String> COUNTERS = Arrays.asList("listObjectCount", "listObjectSize", "putObjectCount",
            "putObjectSize", "failedObjectCount", "failedObjectSize", "skippedObjectCount", "skippedObjectSize",
            "currentSpeedBytesPerSecond");

    public List<Object> group(List<Object> rows, boolean deleted, boolean partial) {
        Map<Integer, TaskCreationService.Creation> owners = new HashMap<>();
        for (TaskCreationService.Creation c : taskCreationService.getAll())
            for (Integer id : c.getTaskIds()) owners.put(id, c);
        Map<String, List<Map<String,Object>>> groups = new LinkedHashMap<>();
        Map<String, TaskCreationService.Creation> creations = new LinkedHashMap<>();
        List<Object> result = new ArrayList<>();
        if (rows != null) for (Object value : rows) {
            Map<String,Object> row = mapper.convertValue(value, Map.class);
            Integer id = row.get("taskIndex") instanceof Number ? ((Number)row.get("taskIndex")).intValue() : null;
            TaskCreationService.Creation c = owners.get(id);
            if (c == null) result.add(value);
            else { groups.computeIfAbsent(c.getId(), key -> new ArrayList<>()).add(row); creations.put(c.getId(), c); }
        }
        // Build only from physical rows. Historical creation records never resurrect deleted tasks.
        for (Map.Entry<String,List<Map<String,Object>>> entry : groups.entrySet())
            result.add(summarize(creations.get(entry.getKey()), entry.getValue(), deleted, partial));
        return result;
    }

    private Map<String,Object> summarize(TaskCreationService.Creation c, List<Map<String,Object>> children,
                                         boolean deleted, boolean partial) {
        children.sort(Comparator.comparingLong(row -> number(row.get("taskIndex"))));
        Map<String,Object> result = new LinkedHashMap<>(children.get(0));
        Set<Integer> seen = new LinkedHashSet<>(); Set<String> nodes = new LinkedHashSet<>();
        Set<String> statuses = new HashSet<>();
        long start = 0, end = 0, created = 0; boolean scan = Boolean.TRUE.equals(c.getScanComplete());
        boolean duplicate = false;
        for (String field : COUNTERS) result.put(field,0L);
        for (Map<String,Object> row : children) {
            int id = ((Number)row.get("taskIndex")).intValue();
            if (!seen.add(id)) { duplicate = true; continue; }
            for (String field : COUNTERS) result.put(field,number(result.get(field))+number(row.get(field)));
            for (String field : Arrays.asList("workerNode", "executionNodeIp"))
                if (row.get(field) != null && !String.valueOf(row.get(field)).isEmpty()) nodes.add(String.valueOf(row.get(field)));
            if (row.get("participatingWorkerNodes") instanceof Collection)
                for (Object node : (Collection<?>)row.get("participatingWorkerNodes")) if (node != null) nodes.add(String.valueOf(node));
            String status = String.valueOf(row.get("status")); statuses.add(status);
            scan &= Boolean.TRUE.equals(row.get("scanComplete")) || (row.get("scanComplete") == null
                    && ("COMPLETED".equals(status) || "LIST_DONE".equals(status)));
            long t = time(row.get("startTime")); if (t > 0 && (start == 0 || t < start)) start = t;
            t = time(row.get("createTime")); if (t > 0 && (created == 0 || t < created)) created = t;
            end = Math.max(end, time(row.get("endTime")));
        }
        boolean complete = !partial && !duplicate && seen.containsAll(c.getTaskIds());
        String status;
        if (statuses.contains("RUNNING") || statuses.contains("LIST_DONE")) status = "RUNNING";
        else if (!deleted && c.isStopped()) status = "STOPPED";
        else if (!complete) status = "FAILED";
        else if (!Boolean.TRUE.equals(c.getScanComplete())) status = "FAILED".equals(c.getStatus()) ? "FAILED" : "PENDING";
        else if (statuses.contains("PENDING")) status = "PENDING";
        else if (statuses.contains("STOPPED")) status = "STOPPED";
        else if (statuses.contains("FAILED") || statuses.contains("PARTIAL_FAILED"))
            status = statuses.size() == 1 && statuses.contains("FAILED") ? "FAILED" : "PARTIAL_FAILED";
        else if (statuses.size() == 1 && statuses.contains("COMPLETED")) status = "COMPLETED";
        else status = "FAILED";
        result.put("taskIndex", c.getPublicTaskId()); result.put("ossTaskGroup",true);
        result.put("creationId",c.getId()); result.put("creationStatus",c.getStatus());
        result.put("autoStart",Boolean.TRUE.equals(c.getAutoStart()));
        result.put("childTaskIds",new ArrayList<>(c.getTaskIds())); result.put("shardCount",c.getTaskIds().size());
        result.put("shards",children); result.put("participatingWorkerNodes",new ArrayList<>(nodes));
        result.put("workerNode", nodes.size() == 1 ? nodes.iterator().next() : null);
        result.put("executionNodeIp", null); result.put("status",status);
        // Completed children retain their window tail while the logical task is active.
        // Only the final public task state controls whether current speed is zero.
        if (!"RUNNING".equals(status) && !"PENDING".equals(status))
            result.put("currentSpeedBytesPerSecond", 0L);
        result.put("snapshotComplete",complete); result.put("partial",!complete); result.put("scanComplete",scan && complete);
        result.put("orchestrationComplete",Boolean.TRUE.equals(c.getScanComplete()));
        result.put("missingTaskIds", missing(c.getTaskIds(),seen));
        result.put("startKey",null); result.put("endKey",null); result.put("marker",null);
        result.put("shardId",null); result.put("shardIndex",null);
        if (!complete) { result.put("errorCode","TASK_GROUP_INCOMPLETE"); result.put("errorMessage","部分执行子任务不可用"); }
        else if (c.getMessage() != null && !Boolean.TRUE.equals(c.getScanComplete())) result.put("errorMessage",c.getMessage());
        BucketPair request = c.getRequest();
        if (request != null) {
            Map<String,Object> fields = mapper.convertValue(request,Map.class);
            for (String field : Arrays.asList("sourceBucket", "destBucket", "sourceS3Alias", "destS3Alias", "direction",
                    "sourceEndpoint", "destinationEndpoint", "dateAfter", "dateBefore", "overwrite", "conflictPolicy",
                    "distributionMode", "keyMapping", "objectKey", "taskBandwidthScopeId", "taskBandwidthLimitMbps"))
                if (fields.get(field) != null) result.put(field,fields.get(field));
        }
        if (created > 0) result.put("createTime", format(created));
        result.put("startTime", start > 0 ? format(start) : null);
        boolean ended = !"PENDING".equals(status) && !"RUNNING".equals(status) && complete;
        result.put("endTime", ended && end > 0 ? format(end) : null);
        long elapsed = start == 0 ? 0 : Math.max(1,(ended && end > 0 ? end : System.currentTimeMillis())-start);
        result.put("averageSpeedBytesPerSecond", elapsed == 0 ? 0L : number(result.get("putObjectSize"))*1000/elapsed);
        result.put("duration",elapsed == 0 ? "-" : (elapsed/1000)+"s");
        return result;
    }
    private List<Integer> missing(List<Integer> ids, Set<Integer> seen) {
        List<Integer> values = new ArrayList<>(); for (Integer id : ids) if (!seen.contains(id)) values.add(id); return values;
    }
    private long number(Object value) { return value instanceof Number ? ((Number)value).longValue() : 0; }
    private long time(Object value) {
        if (value instanceof Number) return ((Number)value).longValue();
        if (value != null) try { return new SimpleDateFormat("yyyy-MM-dd HH:mm:ss").parse(String.valueOf(value)).getTime(); }
        catch (Exception ignored) { }
        return 0;
    }
    private String format(long value) { return new SimpleDateFormat("yyyy-MM-dd HH:mm:ss").format(new Date(value)); }
}
