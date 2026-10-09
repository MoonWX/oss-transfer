package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.model.*;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;
import java.util.*;
import java.util.regex.*;

@Service
public class WorkerTaskChildrenService {
    @Autowired private SubTaskManagerService tasks;
    @Autowired private TransferSpeedService speed;
    private final ObjectMapper mapper = new ObjectMapper();
    private final Map<Integer, Metadata> membership = new HashMap<>();
    private final Map<Integer, Long> versions = new HashMap<>();
    private static final Pattern INDEX = Pattern.compile("(?:-oss-|-local-|batch-|^oss-.+-)(\\d+)$");
    private static final Pattern CREATION = Pattern.compile("^oss-(.+)-[0-9]+$");
    private static final class Metadata {
        final Integer parent, index; final String identity, creation; final boolean local;
        Metadata(SubTaskStatus task) {
            parent = task.getParentTaskId() == null ? task.getTaskIndex() : task.getParentTaskId();
            index = index(task.getShardIndex(), task.getShardId());
            identity = String.valueOf(task.getShardId() == null ? task.getTaskIndex() : task.getShardId());
            Matcher match = CREATION.matcher(task.getShardId() == null ? "" : task.getShardId());
            creation = match.matches() ? match.group(1) : null;
            local = task.getDirection() != null && task.getDirection() != TransferDirection.OSS_TO_OSS;
        }
    }
    public static Integer index(Integer index, String id) {
        if (index != null && index >= 0) return index;
        Matcher matcher = INDEX.matcher(id == null ? "" : id);
        if (matcher.find()) try { return Integer.parseInt(matcher.group(1)); } catch (NumberFormatException ignored) { }
        return null;
    }
    public Map<String, Object> query(int parent, int offset, int limit, List<Integer> selectedIds) {
        return query(parent, offset, limit, selectedIds, null, false);
    }
    public synchronized Map<String, Object> query(int parent, int offset, int limit, List<Integer> selectedIds,
                                                  String creationId, boolean includeIndex) {
        Map<Integer, Integer> selected = new LinkedHashMap<>(), numbers = new LinkedHashMap<>();
        int total = 0;
        if (selectedIds != null && creationId == null) {
            for (Integer id : selectedIds) selected.put(id, 0);
        } else {
            Set<Integer> retained = new HashSet<>(tasks.getTaskIdList());
            membership.keySet().retainAll(retained); versions.keySet().retainAll(retained);
            for (Integer id : retained) {
                long incarnation = tasks.taskIdentityVersion(id);
                if (!membership.containsKey(id) || !Objects.equals(versions.get(id), incarnation)) {
                    SubTaskStatus task = tasks.getTaskStatus(id);
                    membership.put(id, task == null || "discovery".equals(task.getShardId()) ? null : new Metadata(task));
                    // A concurrent import must trigger another lookup, never bless old metadata with its newer version.
                    versions.put(id, incarnation);
                }
            }
            List<Map.Entry<Integer, Metadata>> children = new ArrayList<>();
            for (Map.Entry<Integer, Metadata> entry : membership.entrySet()) {
                Metadata value = entry.getValue();
                if (value != null && (creationId == null ? value.local && value.parent == parent : creationId.equals(value.creation))) children.add(entry);
            }
            children.sort(Comparator.comparing((Map.Entry<Integer, Metadata> e) -> e.getValue().index,
                    Comparator.nullsLast(Integer::compareTo)).thenComparing(e -> e.getValue().identity));
            Set<Integer> occupied = new HashSet<>();
            for (Map.Entry<Integer, Metadata> child : children)
                if (child.getValue().index != null) occupied.add(child.getValue().index + 1);
            int next = 1;
            for (Map.Entry<Integer, Metadata> child : children) {
                Integer ordinal = child.getValue().index;
                if (ordinal == null) { while (occupied.contains(next)) next++; ordinal = next++; occupied.add(ordinal); }
                else ordinal++;
                total = Math.max(total, ordinal); numbers.put(child.getKey(), ordinal);
                if (ordinal > offset && ordinal <= (long) offset + limit) selected.put(child.getKey(), ordinal);
            }
        }
        List<Map<String, Object>> items = new ArrayList<>();
        for (Map.Entry<Integer, Integer> entry : selected.entrySet()) {
            SubTaskStatus task = tasks.getTaskStatus(entry.getKey());
            if (task == null || "discovery".equals(task.getShardId())) continue;
            if (selectedIds == null && creationId == null && !Objects.equals(task.getParentTaskId() == null ? task.getTaskIndex() : task.getParentTaskId(), parent)) continue;
            if (speed != null) task.setCurrentSpeedBytesPerSecond(speed.currentBytesPerSecond(task.getTaskIndex()));
            Map<String, Object> item = mapper.convertValue(SubTaskStatusVO.from(task), Map.class);
            item.put("errorMessage", task.getErrorMessage()); item.put("errorCode", task.getErrorCode());
            item.put("targetRelativePath", task.getTargetRelativePath());
            if (entry.getValue() > 0) item.put("subtaskNumber", entry.getValue());
            items.add(item);
        }
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("items", items); result.put("totalCount", total);
        if (includeIndex) result.put("taskNumbers", numbers);
        return result;
    }
}
