package com.transfer.tools.service;

import com.transfer.tools.model.LocalParentTaskRecord;
import com.transfer.tools.model.LocalTransferRequest;
import com.transfer.tools.model.ListSubTask;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.storage.RocksDBService;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

import static org.junit.Assert.*;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

public class LocalParentTaskServiceTest {

    @Test
    public void persistsParentsBeforeDispatchWithConsecutivePublicIds() {
        InMemoryRocksDB rocks = new InMemoryRocksDB();
        SubTaskManagerService taskManager = mock(SubTaskManagerService.class);
        when(taskManager.allocateTaskIdRange(2)).thenReturn(86);
        LocalParentTaskService service = new LocalParentTaskService();
        ReflectionTestUtils.setField(service, "rocksDBService", rocks);
        ReflectionTestUtils.setField(service, "subTaskManagerService", taskManager);

        LocalTransferRequest first = request(TransferDirection.OSS_TO_LOCAL);
        LocalTransferRequest second = request(TransferDirection.LOCAL_TO_OSS);
        List<LocalParentTaskRecord> records = service.create(Arrays.asList(first, second));

        assertEquals(Arrays.asList(86, 87), Arrays.asList(
                records.get(0).getParentTaskId(), records.get(1).getParentTaskId()));
        assertEquals(Integer.valueOf(86), first.getParentTaskId());
        assertEquals(Integer.valueOf(87), second.getTaskId());
        assertEquals(2, service.getAll().size());
        assertEquals(SubTaskStatus.TaskStatus.PENDING, service.get(86).getStatus());
    }

    @Test
    public void keepsFailedParentVisibleAndRecoverableByExplicitRetry() {
        InMemoryRocksDB rocks = new InMemoryRocksDB();
        SubTaskManagerService taskManager = mock(SubTaskManagerService.class);
        when(taskManager.allocateTaskIdRange(1)).thenReturn(12);
        LocalParentTaskService service = new LocalParentTaskService();
        ReflectionTestUtils.setField(service, "rocksDBService", rocks);
        ReflectionTestUtils.setField(service, "subTaskManagerService", taskManager);

        service.create(Arrays.asList(request(TransferDirection.OSS_TO_LOCAL)));
        service.markFailed(12, "SOURCE_DISCOVERY_FAILED", "boom");

        LocalParentTaskRecord failed = service.get(12);
        assertEquals(SubTaskStatus.TaskStatus.FAILED, failed.getStatus());
        assertEquals("SOURCE_DISCOVERY_FAILED", failed.getErrorCode());
        assertFalse(failed.getOrchestrationComplete());
        assertEquals(SubTaskStatus.TaskStatus.FAILED, service.toStatus(failed).getStatus());
    }

    @Test
    public void localShardIdsAreStableAndNegative() {
        SubTaskManagerService service = new SubTaskManagerService();
        int first = service.preferredLocalShardTaskId(TransferDirection.OSS_TO_LOCAL, 86, "86-oss-0");
        int second = service.preferredLocalShardTaskId(TransferDirection.OSS_TO_LOCAL, 86, "86-oss-0");
        int otherDirection = service.preferredLocalShardTaskId(TransferDirection.LOCAL_TO_OSS, 86, "86-oss-0");
        assertTrue(first < 0);
        assertEquals(first, second);
        assertNotEquals(first, otherDirection);
    }

    @Test
    public void localShardCollisionDoesNotOverwriteExistingTask() {
        InMemoryRocksDB rocks = new InMemoryRocksDB();
        SubTaskManagerService service = new SubTaskManagerService();
        ReflectionTestUtils.setField(service, "rocksDBService", rocks);
        ReflectionTestUtils.setField(service, "endpointNormalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(service, "fileReportLedgerService", mock(FileReportLedgerService.class));
        ListSubTask task = localShard(86, "86-oss-0");
        int preferred = service.preferredLocalShardTaskId(task.getDirection(), task.getParentTaskId(), task.getShardId());
        SubTaskStatus occupied = new SubTaskStatus();
        occupied.setTaskIndex(preferred);
        occupied.setShardId("another-shard");
        rocks.put("task:" + preferred, occupied);

        Integer assigned = service.saveAssignedLocalTask(task);

        assertNotEquals(Integer.valueOf(preferred), assigned);
        assertEquals("another-shard", ((SubTaskStatus) rocks.values.get("task:" + preferred)).getShardId());
        assertEquals("86-oss-0", service.getTaskStatus(assigned).getShardId());
    }

    @Test
    public void deletedParentCannotRestartAndPermanentDeletionOnlyRemovesItsRecords() {
        InMemoryRocksDB rocks = new InMemoryRocksDB();
        LocalParentTaskService service = new LocalParentTaskService();
        SubTaskManagerService manager = mock(SubTaskManagerService.class);
        when(manager.allocateTaskIdRange(1)).thenReturn(12);
        ReflectionTestUtils.setField(service, "rocksDBService", rocks);
        ReflectionTestUtils.setField(service, "subTaskManagerService", manager);
        service.create(Arrays.asList(request(TransferDirection.OSS_TO_LOCAL)));
        SubTaskStatus snapshot = new SubTaskStatus();
        snapshot.setTaskIndex(12); snapshot.setPutObjectCount(42L);
        service.softDelete(12, snapshot);
        assertTrue(service.getAll().isEmpty());
        assertTrue(service.isStopped(12));
        assertFalse(service.markRunning(12));
        service.markResuming(12);
        assertEquals(SubTaskStatus.TaskStatus.PENDING, service.get(12).getStatus());
        assertEquals(Long.valueOf(42), service.getDeleted().get(0).getPutObjectCount());
        service.restore(12);
        assertEquals(1, service.getAll().size());
        service.softDelete(12, snapshot);
        rocks.put("unrelated", "keep");
        service.permanentDelete(12);
        assertNull(service.get(12));
        assertFalse("A stale queued exploration must not recreate a permanently deleted parent", service.markRunning(12));
        assertTrue(service.isStopped(12));
        assertTrue(service.isDeleted(12));
        service.restore(12);
        assertTrue("Permanent deletion cannot be undone by restoring a recycle-bin marker", service.isDeleted(12));
        assertTrue(service.getDeleted().isEmpty());
        assertEquals("keep", rocks.get("unrelated", String.class));
    }

    private LocalTransferRequest request(TransferDirection direction) {
        LocalTransferRequest request = new LocalTransferRequest();
        request.setDirection(direction);
        request.setDistributionMode("balanced");
        return request;
    }

    private ListSubTask localShard(int parentId, String shardId) {
        ListSubTask task = new ListSubTask();
        task.setDirection(TransferDirection.OSS_TO_LOCAL);
        task.setParentTaskId(parentId);
        task.setShardId(shardId);
        task.setExecutionNodeIp("127.0.0.1");
        task.setLocalPath("/data");
        return task;
    }

    private static class InMemoryRocksDB extends RocksDBService {
        private final Map<String, Object> values = new HashMap<>();

        @Override
        public void put(String key, Object value) {
            values.put(key, value);
        }

        @Override
        public <T> T get(String key, Class<T> clazz) {
            return clazz.cast(values.get(key));
        }

        @Override
        public void delete(String key) { values.remove(key); }

        @Override
        public void putBatchWithDeletes(Map<String, Object> puts, List<String> deletes) {
            values.putAll(puts); for (String key : deletes) values.remove(key);
        }

        @Override
        public void putBatchSync(Map<String, Object> dataMap) { values.putAll(dataMap); }

        @Override
        public void putBatch(Map<String, Object> dataMap) {
            values.putAll(dataMap);
        }
    }
}
