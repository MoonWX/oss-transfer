package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.service.MasterLocalTransferService;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.Arrays;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

public class MasterControllerStatusTest {

    @Test
    @SuppressWarnings("unchecked")
    public void legacyStatusShowsPersistedLocalParent() {
        MasterController controller = new MasterController();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.emptyList());
        MasterLocalTransferService localTransfers = mock(MasterLocalTransferService.class);
        SubTaskStatus parent = localParent();
        Map<String, Object> localStatus = new LinkedHashMap<>();
        localStatus.put("tasks", Collections.singletonList(parent));
        when(localTransfers.aggregateStatus(null, null, null, null, null, null)).thenReturn(localStatus);
        ReflectionTestUtils.setField(controller, "distributedConfig", config);
        ReflectionTestUtils.setField(controller, "masterLocalTransferService", localTransfers);

        Map<String, Object> result = controller.status(null, null, null, null);

        assertFalse(result.containsKey("success"));
        assertEquals(1, result.get("totalTasks"));
        assertEquals(1, result.get("failedTasks"));
        List<Object> tasks = (List<Object>) result.get("tasks");
        Map<String, Object> visible = new ObjectMapper().convertValue(tasks.get(0), Map.class);
        assertEquals(86, ((Number) visible.get("taskIndex")).intValue());
        assertEquals("OSS_TO_LOCAL", String.valueOf(visible.get("direction")));
    }

    @Test
    public void keepsOssTaskAndFiltersInternalLocalShard() {
        MasterController controller = new MasterController();
        Map<String, Object> ossTask = task(41, "OSS_TO_OSS");
        Map<String, Object> legacyOssTask = task(42, null);
        Map<String, Object> localShard = task(-12345, "OSS_TO_LOCAL");
        List<Object> visible = new ArrayList<>();

        controller.appendOssTopLevelTasks(visible, Arrays.asList(ossTask, legacyOssTask, localShard));

        assertEquals(Arrays.asList(ossTask, legacyOssTask), visible);
    }

    private SubTaskStatus localParent() {
        SubTaskStatus parent = new SubTaskStatus();
        parent.setTaskIndex(86);
        parent.setParentTaskId(86);
        parent.setDirection(TransferDirection.OSS_TO_LOCAL);
        parent.setStatus(SubTaskStatus.TaskStatus.FAILED);
        parent.setListObjectCount(0L);
        parent.setListObjectSize(0L);
        parent.setPutObjectCount(0L);
        parent.setPutObjectSize(0L);
        parent.setSkippedObjectCount(0L);
        parent.setSkippedObjectSize(0L);
        parent.setFailedObjectCount(0L);
        parent.setFailedObjectSize(0L);
        return parent;
    }

    private Map<String, Object> task(int id, String direction) {
        Map<String, Object> task = new LinkedHashMap<>();
        task.put("taskIndex", id);
        if (direction != null) task.put("direction", direction);
        return task;
    }
}
