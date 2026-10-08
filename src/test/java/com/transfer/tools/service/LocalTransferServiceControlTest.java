package com.transfer.tools.service;

import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.LocalDiscoveryState;
import com.transfer.tools.model.LocalNodeBinding;
import com.transfer.tools.model.LocalTransferRequest;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.model.TransferEndpoint;
import com.transfer.tools.storage.RocksDBService;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.Arrays;
import java.util.Collections;
import java.util.Map;

import static org.junit.Assert.assertEquals;
import static org.mockito.Mockito.doThrow;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

public class LocalTransferServiceControlTest {
    private LocalTransferService service;
    private SubTaskManagerService taskManager;
    private BufferQueueManager queue;
    private SubTaskStatus firstShard;
    private SubTaskStatus secondShard;
    private SubTaskStatus unrelatedShard;

    @Before
    public void setUp() {
        service = new LocalTransferService();
        taskManager = mock(SubTaskManagerService.class);
        queue = mock(BufferQueueManager.class);
        RocksDBService store = mock(RocksDBService.class);
        EndpointNormalizationService normalization = mock(EndpointNormalizationService.class);
        ReflectionTestUtils.setField(service, "subTaskManagerService", taskManager);
        ReflectionTestUtils.setField(service, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(service, "rocksDBService", store);
        ReflectionTestUtils.setField(service, "normalizationService", normalization);
        ReflectionTestUtils.setField(service, "controlService", new LocalTaskControlService());
        ReflectionTestUtils.setField(service, "transferSpeedService", mock(TransferSpeedService.class));

        firstShard = shard(-101, 1);
        secondShard = shard(-102, 1);
        unrelatedShard = shard(-201, 2);
        when(taskManager.getTaskIdList()).thenReturn(Arrays.asList(-101, -102, -201));
        when(taskManager.getTaskStatus(-101)).thenReturn(firstShard);
        when(taskManager.getTaskStatus(-102)).thenReturn(secondShard);
        when(taskManager.getTaskStatus(-201)).thenReturn(unrelatedShard);
        when(taskManager.resumeTask(org.mockito.ArgumentMatchers.anyInt())).thenAnswer(call -> {
            queue.offerListTask(taskManager.getTaskStatus(call.getArgument(0)));
            return true;
        });
        when(taskManager.stopTask(org.mockito.ArgumentMatchers.anyInt())).thenAnswer(call -> {
            SubTaskStatus task = taskManager.getTaskStatus(call.getArgument(0));
            task.setStatus(SubTaskStatus.TaskStatus.STOPPED);
            return true;
        });

        TransferEndpoint local = new TransferEndpoint();
        LocalNodeBinding binding = new LocalNodeBinding("10.0.0.1", "/data/source");
        local.setNodeBindings(Collections.singletonList(binding));
        LocalTransferRequest request = new LocalTransferRequest();
        request.setParentTaskId(1);
        request.setDirection(TransferDirection.LOCAL_TO_OSS);
        request.setSourceEndpoint(local);
        request.setDestinationEndpoint(new TransferEndpoint());
        LocalDiscoveryState discovery = new LocalDiscoveryState();
        discovery.setParentTaskId(1);
        discovery.setRequest(request);
        discovery.setDiscoveryWorker("10.0.0.1");
        discovery.setStatus(LocalDiscoveryState.Status.COMPLETED);
        when(normalization.bindingFor(local, "10.0.0.1"))
                .thenReturn(binding);
        when(store.get("local:discovery:1", LocalDiscoveryState.class)).thenReturn(discovery);
        when(store.scanByPrefix("local:discovery:", LocalDiscoveryState.class))
                .thenReturn(Collections.singletonList(discovery));
    }

    @After
    public void tearDown() {
        service.shutdownDiscovery();
    }

    @Test
    public void retryParentQueuesPhysicalShardsWithoutReportingDiscoveryAsFailed() {
        Map<String, Object> result = service.retry(Collections.singletonList(1));

        assertEquals(2, result.get("accepted"));
        assertEquals(0, result.get("ignored"));
        assertEquals(0, result.get("failed"));
        verify(queue).offerListTask(firstShard);
        verify(queue).offerListTask(secondShard);
        verify(queue, never()).offerListTask(unrelatedShard);
        verify(taskManager, never()).getTaskStatus(1);
    }

    @Test
    public void stopParentDoesNotCountDiscoveryAsAnIgnoredPhysicalShard() {
        Map<String, Object> result = service.stop(Collections.singletonList(1));

        assertEquals(2, result.get("accepted"));
        assertEquals(0, result.get("ignored"));
        assertEquals(0, result.get("failed"));
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, firstShard.getStatus());
        assertEquals(SubTaskStatus.TaskStatus.STOPPED, secondShard.getStatus());
        assertEquals(SubTaskStatus.TaskStatus.RUNNING, unrelatedShard.getStatus());
        verify(taskManager, never()).getTaskStatus(1);
    }

    @Test
    public void retryStillReportsARealShardFailure() {
        doThrow(new IllegalStateException("queue unavailable")).when(queue).offerListTask(firstShard);

        Map<String, Object> result = service.retry(Collections.singletonList(1));

        assertEquals(1, result.get("accepted"));
        assertEquals(1, result.get("failed"));
        verify(queue).offerListTask(secondShard);
        verify(taskManager, never()).getTaskStatus(1);
    }

    @Test public void restartDoesNotStartUnstartedOssTaskButRecoversInterruptedAndExplicitlyResumedTasks() {
        firstShard.setDirection(TransferDirection.OSS_TO_OSS); firstShard.setStatus(SubTaskStatus.TaskStatus.PENDING);
        secondShard.setDirection(TransferDirection.OSS_TO_OSS); secondShard.setStatus(SubTaskStatus.TaskStatus.LIST_DONE);
        unrelatedShard.setDirection(TransferDirection.OSS_TO_OSS); unrelatedShard.setStatus(SubTaskStatus.TaskStatus.PENDING); unrelatedShard.setAttempt(1);
        service.recoverDiscoveries();
        verify(taskManager, never()).recoverTask(firstShard);
        verify(taskManager).recoverTask(secondShard);
        verify(taskManager).recoverTask(unrelatedShard);
    }

    @Test public void invalidLegacyInventoryDoesNotPreventOtherTaskRecovery() {
        firstShard.setDirection(TransferDirection.OSS_TO_OSS);
        firstShard.setStartKey("legacy.incr"); firstShard.setEndKey(null);
        doThrow(new IllegalStateException("旧清单缺少快照")).when(taskManager).recoverTask(firstShard);
        service.recoverDiscoveries();
        assertEquals(SubTaskStatus.TaskStatus.FAILED, firstShard.getStatus());
        assertEquals("TASK_RECOVERY_FAILED", firstShard.getErrorCode());
        verify(taskManager).recoverTask(secondShard);
        verify(taskManager).recoverTask(unrelatedShard);
        RocksDBService store = (RocksDBService) ReflectionTestUtils.getField(service,"rocksDBService");
        verify(store).putSync("task:-101", firstShard);
    }

    private SubTaskStatus shard(int taskId, int parentId) {
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(taskId);
        task.setParentTaskId(parentId);
        task.setShardId(parentId + "-local-" + taskId);
        task.setDirection(TransferDirection.LOCAL_TO_OSS);
        task.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        return task;
    }
}
