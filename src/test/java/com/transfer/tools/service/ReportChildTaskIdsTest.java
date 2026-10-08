package com.transfer.tools.service;

import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;
import org.junit.Test;

import java.util.*;

import static org.junit.Assert.*;

public class ReportChildTaskIdsTest {
    @Test public void persistedIndexWinsOverLegacySuffixAndCreationOrder() {
        ReportChildTaskIds ids = new ReportChildTaskIds(224, Arrays.asList(224, 225));
        SubTaskStatus task = task(225, "224-oss-9", 2, TransferDirection.OSS_TO_OSS);
        ids.add("one", task); ids.build();
        assertEquals("224-3", ids.childId("one", 225, "224-oss-9"));
        assertEquals(Integer.valueOf(225), task.getTaskIndex());
        assertEquals("224-oss-9", task.getShardId());
        assertEquals(Integer.valueOf(2), task.getShardIndex());
    }

    @Test public void legacySequenceWinsOverCreationOrderAndIsOneBased() {
        ReportChildTaskIds ids = new ReportChildTaskIds(224, Arrays.asList(224, 225));
        ids.add("one", task(225, "224-oss-0", null, TransferDirection.OSS_TO_OSS));
        ids.add("two", task(224, "224-oss-1", null, TransferDirection.OSS_TO_OSS));
        assertEquals("224-1", ids.childId("one", 225, null));
        assertEquals("224-2", ids.childId("two", 224, null));
    }

    @Test public void creationOrderUsesDurableDirectoryRatherThanNumericTaskIdOrder() {
        ReportChildTaskIds ids = new ReportChildTaskIds(224, Arrays.asList(905, 301));
        ids.add("two", task(301, null, null, TransferDirection.OSS_TO_OSS));
        ids.add("one", task(905, null, null, TransferDirection.OSS_TO_OSS));
        assertEquals("224-1", ids.childId("one", 905, null));
        assertEquals("224-2", ids.childId("two", 301, null));
    }

    @Test public void oneChildKeepsItsIdAcrossWorkersWithDifferentInternalIds() {
        ReportChildTaskIds ids = new ReportChildTaskIds(224, Collections.emptyList());
        ids.add("one", task(-1670275759, "224-local-2", null, TransferDirection.LOCAL_TO_OSS));
        ids.add("two", task(-1670275999, "224-local-2", 2, TransferDirection.LOCAL_TO_OSS));
        assertEquals("224-3", ids.childId("one", -1670275759, null));
        assertEquals("224-3", ids.childId("two", -1670275999, null));
        assertEquals("224-3", ids.childId("another", null, "224-local-2"));
    }

    @Test public void onePhysicalCreationChildKeepsItsIdAcrossWorkers() {
        ReportChildTaskIds ids = new ReportChildTaskIds(224, Arrays.asList(224, 225));
        ids.add("one", task(225, null, null, TransferDirection.OSS_TO_OSS));
        ids.add("two", task(225, null, null, TransferDirection.OSS_TO_OSS));
        assertEquals("224-2", ids.childId("one", 225, null));
        assertEquals("224-2", ids.childId("two", 225, null));
    }

    @Test public void fallbackIsStableAcrossInputOrderAndSkipsReservedNumbers() {
        ReportChildTaskIds forward = new ReportChildTaskIds(224, Collections.emptyList());
        ReportChildTaskIds reverse = new ReportChildTaskIds(224, Collections.emptyList());
        List<SubTaskStatus> tasks = Arrays.asList(
                task(-30, "224-local-0", 0, TransferDirection.LOCAL_TO_OSS),
                task(-2, null, null, TransferDirection.LOCAL_TO_OSS),
                task(-10, null, null, TransferDirection.LOCAL_TO_OSS));
        for (SubTaskStatus task : tasks) forward.add("one", task);
        for (int i = tasks.size() - 1; i >= 0; i--) reverse.add("one", tasks.get(i));
        forward.build(); reverse.build();
        assertEquals("224-1", forward.childId("one", -30, null));
        assertEquals("224-2", forward.childId("one", -2, null));
        assertEquals("224-3", forward.childId("one", -10, null));
        for (SubTaskStatus task : tasks) assertEquals(forward.childId("one", task.getTaskIndex(), null),
                reverse.childId("one", task.getTaskIndex(), null));
    }

    @Test public void eachParentStartsItsOwnSequenceWithoutExposingNegativeIds() {
        for (int parent : Arrays.asList(224, 225)) {
            ReportChildTaskIds ids = new ReportChildTaskIds(parent, Collections.emptyList());
            ids.add("one", task(-1670275759, null, null, TransferDirection.OSS_TO_LOCAL));
            assertEquals(parent + "-1", ids.childId("one", -1670275759, null));
        }
    }

    @Test public void historicalShardAloneNeedsRecognizedIdentityEvidence() {
        ReportChildTaskIds ids = new ReportChildTaskIds(224, Collections.emptyList());
        assertEquals("224-1", ids.childId("one", null, "224-oss-0"));
        assertEquals("224-4", ids.childId("one", null, "224-local-3"));
        assertEquals("224-3", ids.childId("one", null, "batch-2"));
        assertEquals("224-5", ids.childId("one", null, "oss-creation-id-4"));
        assertNull(ids.childId("one", null, "range-0"));
        assertNull(ids.childId("one", null, "225-oss-0"));
        assertNull(ids.childId("one", null, "224-oss-9999999999999999999999999999"));
    }

    @Test public void discoveryOrchestrationParentAndUnknownRowsHaveNoChildId() {
        ReportChildTaskIds ids = new ReportChildTaskIds(224, Collections.singletonList(224));
        ids.add("one", task(-1, "discovery", 0, TransferDirection.LOCAL_TO_OSS));
        ids.add("one", task(-2, "orchestration", 1, TransferDirection.LOCAL_TO_OSS));
        SubTaskStatus parent = task(224, null, null, TransferDirection.LOCAL_TO_OSS);
        parent.setParentTaskId(224); ids.add("one", parent);
        assertNull(ids.childId("one", -1, "discovery"));
        assertNull(ids.childId("one", -2, "orchestration"));
        assertNull(ids.childId("one", 224, null));
        assertNull(ids.childId("one", null, null));
        assertNull(ids.childId("one", -999, "unknown"));
    }

    @Test public void standaloneLegacyOssTaskStillHasOneChild() {
        ReportChildTaskIds ids = new ReportChildTaskIds(224, Collections.emptyList());
        ids.add("one", task(224, null, null, TransferDirection.OSS_TO_OSS));
        assertEquals("224-1", ids.childId("one", 224, null));
        assertNull(ids.childId("one", null, null));
    }

    private static SubTaskStatus task(Integer id, String shard, Integer index, TransferDirection direction) {
        SubTaskStatus task = new SubTaskStatus(); task.setTaskIndex(id); task.setShardId(shard);
        task.setShardIndex(index); task.setDirection(direction); return task;
    }
}
