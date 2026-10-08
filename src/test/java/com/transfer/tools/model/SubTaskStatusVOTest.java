package com.transfer.tools.model;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.junit.Test;
import static org.junit.Assert.*;

public class SubTaskStatusVOTest {
    private final ObjectMapper mapper = new ObjectMapper();

    @Test public void scanCompletionSurvivesWorkerStatusJsonMapping() {
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(100); task.setDirection(TransferDirection.OSS_TO_OSS);
        task.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        for (boolean complete : new boolean[]{false, true}) {
            task.setScanComplete(complete);
            JsonNode json = mapper.valueToTree(SubTaskStatusVO.from(task));
            assertTrue(json.get("scanComplete").isBoolean());
            assertEquals(complete, json.get("scanComplete").booleanValue());
        }
    }

    @Test public void legacyUnknownScanCompletionRemainsUnknown() {
        SubTaskStatus task = new SubTaskStatus(); task.setStatus(SubTaskStatus.TaskStatus.STOPPED);
        assertNull(SubTaskStatusVO.from(task).getScanComplete());
        JsonNode json = mapper.valueToTree(SubTaskStatusVO.from(task));
        assertTrue(json.get("scanComplete").isNull());
    }
    @Test public void apiPreservesInternalIndexForStableUserVisibleChildSequence() {
        SubTaskStatus child=new SubTaskStatus();child.setTaskIndex(501);child.setParentTaskId(500);
        child.setShardId("oss-creation-7");child.setShardIndex(7);child.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        SubTaskStatusVO row=SubTaskStatusVO.from(child);
        assertEquals(Integer.valueOf(7),row.getShardIndex());
        assertEquals(Integer.valueOf(500),row.getParentTaskId());
        assertEquals("oss-creation-7",row.getShardId());
    }
}
