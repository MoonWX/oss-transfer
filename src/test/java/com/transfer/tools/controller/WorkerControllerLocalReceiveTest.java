package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.FailedKeyMessage;
import com.transfer.tools.model.FailureStage;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TaskImport;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.service.LocalFileService;
import com.transfer.tools.service.SubTaskManagerService;
import com.transfer.tools.service.TransferValidationException;
import org.junit.Test;
import org.mockito.ArgumentCaptor;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.test.util.ReflectionTestUtils;

import java.lang.reflect.Field;
import java.util.Collections;
import java.util.Map;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNotNull;
import static org.junit.Assert.assertTrue;
import static org.mockito.ArgumentMatchers.anyList;
import static org.mockito.Mockito.doThrow;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

public class WorkerControllerLocalReceiveTest {

    @Test
    public void localFileServiceIsAutowired() throws Exception {
        Field field = WorkerController.class.getDeclaredField("localFileService");
        assertNotNull(field.getAnnotation(Autowired.class));
    }

    @Test
    public void validatesAndImportsOssToLocalShard() {
        Fixture fixture = fixture();
        TaskImport task = localTask();
        SubTaskStatus pending = new SubTaskStatus();
        pending.setStatus(SubTaskStatus.TaskStatus.PENDING);
        when(fixture.taskManager.getTaskStatus(task.getTaskIndex())).thenReturn(pending);

        Map<String, Object> result = fixture.controller.receiveTasks(Collections.singletonList(
                fixture.objectMapper.convertValue(task, Map.class)));

        assertEquals(Boolean.TRUE, result.get("success"));
        verify(fixture.localFiles).validateDestination("/data/downloads", true);
        verify(fixture.taskManager).importTasksWithTaskId(anyList());
        verify(fixture.queues).offerListTask(pending);
    }

    @Test
    public void recordsStructuredFailureWhenLocalPathValidationFails() {
        Fixture fixture = fixture();
        TaskImport task = localTask();
        doThrow(new TransferValidationException("LOCAL_PATH_NOT_WRITABLE", "destination is read-only"))
                .when(fixture.localFiles).validateDestination("/data/downloads", true);
        when(fixture.queues.offerFailedKey(org.mockito.ArgumentMatchers.any(FailedKeyMessage.class)))
                .thenReturn(true);

        Map<String, Object> result = fixture.controller.receiveTasks(Collections.singletonList(
                fixture.objectMapper.convertValue(task, Map.class)));

        assertEquals(Boolean.FALSE, result.get("success"));
        assertEquals("LOCAL_PATH_NOT_WRITABLE", result.get("code"));
        verify(fixture.taskManager, never()).importTasksWithTaskId(anyList());
        ArgumentCaptor<FailedKeyMessage> failure = ArgumentCaptor.forClass(FailedKeyMessage.class);
        verify(fixture.queues).offerFailedKey(failure.capture());
        assertEquals(Integer.valueOf(86), failure.getValue().getParentTaskId());
        assertEquals("86-oss-0", failure.getValue().getShardId());
        assertEquals(FailureStage.VALIDATE, failure.getValue().getStage());
        assertEquals("LOCAL_PATH_NOT_WRITABLE", failure.getValue().getErrorCode());
        assertTrue(failure.getValue().getErrorMessage().contains("read-only"));
    }

    private Fixture fixture() {
        Fixture fixture = new Fixture();
        fixture.controller = new WorkerController();
        fixture.objectMapper = new ObjectMapper();
        fixture.taskManager = mock(SubTaskManagerService.class);
        fixture.localFiles = mock(LocalFileService.class);
        fixture.queues = mock(BufferQueueManager.class);
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getLocalIp()).thenReturn("192.0.2.10");
        ReflectionTestUtils.setField(fixture.controller, "objectMapper", fixture.objectMapper);
        ReflectionTestUtils.setField(fixture.controller, "subTaskManagerService", fixture.taskManager);
        ReflectionTestUtils.setField(fixture.controller, "localFileService", fixture.localFiles);
        ReflectionTestUtils.setField(fixture.controller, "bufferQueueManager", fixture.queues);
        ReflectionTestUtils.setField(fixture.controller, "distributedConfig", config);
        return fixture;
    }

    private TaskImport localTask() {
        TaskImport task = new TaskImport();
        task.setTaskIndex(-12345);
        task.setParentTaskId(86);
        task.setShardId("86-oss-0");
        task.setDirection(TransferDirection.OSS_TO_LOCAL);
        task.setSourceBucket("source-bucket");
        task.setLocalPath("/data/downloads");
        task.setExecutionNodeIp("192.0.2.10");
        task.setEligibleWorkerNodes(Collections.singletonList("192.0.2.10"));
        return task;
    }

    private static class Fixture {
        private WorkerController controller;
        private ObjectMapper objectMapper;
        private SubTaskManagerService taskManager;
        private LocalFileService localFiles;
        private BufferQueueManager queues;
    }
}
