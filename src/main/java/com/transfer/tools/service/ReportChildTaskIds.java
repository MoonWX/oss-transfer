package com.transfer.tools.service;

import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;

import java.util.*;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

/** Export-only child identities. Never changes a worker task, ledger row or durable assignment. */
public final class ReportChildTaskIds {
    private static final int DISPLAY_INDEX_BASE = 1;
    private final Integer parentId;
    private final Map<Integer, Long> creationIndexes = new LinkedHashMap<>();
    private final Map<String, Child> children = new LinkedHashMap<>();
    private final Map<String, Child> byTask = new HashMap<>(), byShard = new HashMap<>();
    private final Map<Integer, Child> byCreationTask = new HashMap<>();
    private final Pattern legacyShard;
    private boolean built;

    public ReportChildTaskIds(Integer parentId, Collection<Integer> creationTaskIds) {
        this.parentId = Objects.requireNonNull(parentId, "parentId");
        long index = 0;
        if (creationTaskIds != null) for (Integer id : creationTaskIds) {
            if (id != null && !creationIndexes.containsKey(id)) creationIndexes.put(id, index++);
        }
        legacyShard = Pattern.compile("^(?:" + Pattern.quote(String.valueOf(parentId))
                + "-(?:oss|local)|batch|oss-.+)-(\\d+)$");
    }

    public void add(String node, SubTaskStatus task) {
        if (task == null || special(task.getShardId())) return;
        Integer id = task.getTaskIndex();
        String shard = nonempty(task.getShardId());
        if (id == null && shard == null) return;
        if (parentId.equals(id) && parentId.equals(task.getParentTaskId())
                && task.getShardIndex() == null && shard == null) return;
        String key = creationIndexes.containsKey(id) ? "creation:" + id
                : shard != null ? "shard:" + shard
                : task.getDirection() == TransferDirection.OSS_TO_OSS ? "oss:" + id
                : "local:" + taskKey(node, id);
        Child child = shard == null ? null : byShard.get(shard);
        if (child == null) child = children.computeIfAbsent(key,
                ignored -> new Child(shard == null ? String.valueOf(id) : shard, key));
        Long index = task.getShardIndex() != null && task.getShardIndex() >= 0 ? task.getShardIndex().longValue() : null;
        int priority = 0;
        if (index == null) { index = legacyIndex(shard); priority = 1; }
        if (index == null) { index = creationIndexes.get(id); priority = 2; }
        child.prefer(index, priority);
        if (id != null) byTask.put(taskKey(node, id), child);
        if (shard != null) byShard.put(shard, child);
        if (creationIndexes.containsKey(id)) byCreationTask.put(id, child);
        built = false;
    }

    public void build() {
        List<Child> ordered = new ArrayList<>(children.values());
        ordered.sort((a, b) -> {
            int compared = Long.compare(a.index == null ? Long.MAX_VALUE : a.index,
                    b.index == null ? Long.MAX_VALUE : b.index);
            if (compared == 0) compared = naturalCompare(a.sortKey, b.sortKey);
            return compared == 0 ? a.identity.compareTo(b.identity) : compared;
        });
        Set<Long> occupied = new HashSet<>();
        for (Child child : ordered) if (child.index != null) occupied.add(child.index);
        long next = 0;
        for (Child child : ordered) {
            if (child.index != null) child.displayIndex = child.index;
            else {
                while (occupied.contains(next)) next++;
                child.displayIndex = next++;
                occupied.add(child.displayIndex);
            }
        }
        built = true;
    }

    public String childId(String node, Integer internalTaskId, String shardId) {
        if (special(shardId)) return null;
        if (!built) build();
        Child child = internalTaskId == null ? null : byTask.get(taskKey(node, internalTaskId));
        if (child == null && nonempty(shardId) != null) child = byShard.get(shardId);
        if (child == null && internalTaskId != null) child = byCreationTask.get(internalTaskId);
        Long index = child == null ? legacyIndex(shardId) : Long.valueOf(child.displayIndex);
        return index == null ? null : parentId + "-" + (index + DISPLAY_INDEX_BASE);
    }

    private Long legacyIndex(String shardId) {
        if (shardId == null) return null;
        Matcher match = legacyShard.matcher(shardId);
        if (!match.matches()) return null;
        try {
            long index = Long.parseLong(match.group(1));
            return index <= Integer.MAX_VALUE ? index : null;
        } catch (NumberFormatException ignored) { return null; }
    }

    private static boolean special(String shard) { return "discovery".equals(shard) || "orchestration".equals(shard); }
    private static String nonempty(String value) { return value == null || value.isEmpty() ? null : value; }
    private static String taskKey(String node, Integer id) { return String.valueOf(node) + '\0' + id; }

    /** Numeric segments follow the UI's numeric-aware ordering, including historical negative IDs. */
    private static int naturalCompare(String a, String b) {
        int i = 0, j = 0;
        while (i < a.length() && j < b.length()) {
            char left = a.charAt(i), right = b.charAt(j);
            if (Character.isDigit(left) && Character.isDigit(right)) {
                int startA = i, startB = j;
                while (i < a.length() && Character.isDigit(a.charAt(i))) i++;
                while (j < b.length() && Character.isDigit(b.charAt(j))) j++;
                String numberA = a.substring(startA, i).replaceFirst("^0+(?!$)", "");
                String numberB = b.substring(startB, j).replaceFirst("^0+(?!$)", "");
                int compared = Integer.compare(numberA.length(), numberB.length());
                if (compared == 0) compared = numberA.compareTo(numberB);
                if (compared != 0) return compared;
            } else {
                int compared = Character.compare(Character.toLowerCase(left), Character.toLowerCase(right));
                if (compared != 0) return compared;
                i++; j++;
            }
        }
        return Integer.compare(a.length() - i, b.length() - j);
    }

    private static final class Child {
        private final String sortKey, identity;
        private Long index;
        private int priority = Integer.MAX_VALUE;
        private long displayIndex;
        private Child(String sortKey, String identity) { this.sortKey = sortKey; this.identity = identity; }
        private void prefer(Long candidate, int candidatePriority) {
            if (candidate != null && (candidatePriority < priority
                    || (candidatePriority == priority && (index == null || candidate < index)))) {
                index = candidate; priority = candidatePriority;
            }
        }
    }
}
