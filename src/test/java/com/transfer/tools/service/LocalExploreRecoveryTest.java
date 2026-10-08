package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.ListObjectsRequest;
import com.amazonaws.services.s3.model.ObjectListing;
import com.amazonaws.services.s3.model.S3ObjectSummary;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import java.util.concurrent.CancellationException;
import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class LocalExploreRecoveryTest {
    @Test
    public void lostDispatchAcknowledgementReplaysDurableOriginalAssignmentAfterRestart() {
        Store store = new Store();
        Fixture first = new Fixture(store);
        doThrow(new IllegalStateException("lost acknowledgement"))
                .when(first.distributed).distributePreparedLocalTasks(anyList());
        try { first.service.exploreLocalBucketAndCreateSubTasks(first.pair); fail("dispatch should fail"); }
        catch (IllegalStateException expected) { assertEquals("lost acknowledgement", expected.getMessage()); }
        LocalExploreCheckpoint before = first.parents.exploreCheckpoint(12);
        assertEquals("12-oss-0", before.getPendingTasks().get(0).getShardId());
        assertEquals("node-a", before.getPendingTasks().get(0).getExecutionNodeIp());
        assertEquals(0, before.getNextShardIndex());

        Fixture restarted = new Fixture(store);
        restarted.service.exploreLocalBucketAndCreateSubTasks(restarted.pair);

        verify(restarted.distributed, never()).prepareLocalAssignments(anyList(), anyString(), anyList(), anyList());
        verify(restarted.distributed).distributePreparedLocalTasks(argThat(tasks ->
                tasks.size() == 1 && "12-oss-0".equals(tasks.get(0).getShardId())
                        && "node-a".equals(tasks.get(0).getExecutionNodeIp())));
        assertEquals(Collections.singletonList("fixture/c"), restarted.markers);
        assertTrue(restarted.parents.exploreCheckpoint(12).isComplete());
        assertEquals(1, restarted.parents.exploreCheckpoint(12).getNextShardIndex());
        assertNull(restarted.parents.exploreCheckpoint(12).getPendingTasks());
    }

    @Test
    public void stoppingWhileListingReturnsDoesNotDispatchThePageOrMarkDiscoveryComplete() {
        Fixture fixture = new Fixture(new Store());
        when(fixture.s3.listObjects(any(ListObjectsRequest.class))).thenAnswer(call -> {
            fixture.parents.markStopped(12);
            return fixture.page("");
        });
        try { fixture.service.exploreLocalBucketAndCreateSubTasks(fixture.pair); fail("stopped scan should cancel"); }
        catch (CancellationException expected) { }
        fixture.parents.markOrchestrationComplete(12);
        verify(fixture.distributed, never()).distributePreparedLocalTasks(anyList());
        assertTrue(fixture.parents.isStopped(12));
        assertFalse(fixture.parents.get(12).getOrchestrationComplete());
        assertTrue(fixture.parents.recoverable().isEmpty());
        fixture.parents.markResuming(12);
        assertEquals(1, fixture.parents.recoverable().size());
    }

    @Test
    public void completedCheckpointDoesNotRescanOrDispatchAfterRestart() {
        Store store = new Store();
        Fixture fixture = new Fixture(store);
        fixture.service.exploreLocalBucketAndCreateSubTasks(fixture.pair);
        Fixture restarted = new Fixture(store);
        restarted.service.exploreLocalBucketAndCreateSubTasks(restarted.pair);
        verify(restarted.s3, never()).listObjects(any(ListObjectsRequest.class));
        verify(restarted.distributed, never()).distributePreparedLocalTasks(anyList());
    }

    private static class Fixture {
        final LocalParentTaskService parents = new LocalParentTaskService();
        final BucketExploreService service = new BucketExploreService();
        final DistributedService distributed = mock(DistributedService.class);
        final AmazonS3 s3 = mock(AmazonS3.class);
        final List<String> markers = new ArrayList<>();
        final BucketPair pair = new BucketPair();
        Fixture(Store store) {
            ReflectionTestUtils.setField(parents, "rocksDBService", store);
            SubTaskManagerService taskIds = mock(SubTaskManagerService.class);
            when(taskIds.allocateTaskIdRange(1)).thenReturn(12);
            ReflectionTestUtils.setField(parents, "subTaskManagerService", taskIds);
            if (parents.get(12) == null) {
                LocalTransferRequest request = new LocalTransferRequest();
                request.setDirection(TransferDirection.OSS_TO_LOCAL);
                parents.create(Collections.singletonList(request));
            }
            pair.setParentTaskId(12); pair.setSourceBucket("source"); pair.setSourceS3Alias("s3");
            pair.setPrefix("fixture/"); pair.setDirection(TransferDirection.OSS_TO_LOCAL);
            pair.setDistributionMode("affinity"); pair.setEligibleWorkerNodes(Arrays.asList("node-a", "node-b"));
            pair.setNodeBindings(Arrays.asList(new LocalNodeBinding("node-a", "/files"), new LocalNodeBinding("node-b", "/files")));
            ReflectionTestUtils.setField(service, "localParentTaskService", parents);
            ReflectionTestUtils.setField(service, "distributedService", distributed);
            DistributedConfig config = mock(DistributedConfig.class);
            when(config.isMaster()).thenReturn(true); when(config.getWorkerIps()).thenReturn(pair.getEligibleWorkerNodes());
            ReflectionTestUtils.setField(service, "distributedConfig", config);
            ReflectionTestUtils.setField(service, "rateLimiterManager", mock(RateLimiterManager.class));
            S3ClientManager clients = mock(S3ClientManager.class);
            when(clients.getListClient("s3")).thenReturn(s3);
            ReflectionTestUtils.setField(service, "s3ClientManager", clients);
            when(s3.listObjects(any(ListObjectsRequest.class))).thenAnswer(call -> {
                String marker = ((ListObjectsRequest) call.getArgument(0)).getMarker();
                markers.add(marker); return page(marker);
            });
            doAnswer(call -> {
                for (ListSubTask task : (List<ListSubTask>) call.getArgument(0)) {
                    task.setExecutionNodeIp("node-a"); task.setLocalPath("/files");
                }
                return null;
            }).when(distributed).prepareLocalAssignments(anyList(), anyString(), anyList(), anyList());
        }
        ObjectListing page(String marker) {
            ObjectListing result = new ObjectListing();
            for (String key : Arrays.asList("fixture/a", "fixture/b", "fixture/c")) {
                if (marker != null && key.compareTo(marker) <= 0) continue;
                S3ObjectSummary item = new S3ObjectSummary(); item.setKey(key); result.getObjectSummaries().add(item);
            }
            return result;
        }
    }

    private static class Store extends RocksDBService {
        final Map<String, byte[]> values = new HashMap<>();
        final ObjectMapper mapper = new ObjectMapper();
        @Override public void put(String key, Object value) {
            try { values.put(key, mapper.writeValueAsBytes(value)); } catch (Exception e) { throw new RuntimeException(e); }
        }
        @Override public <T> T get(String key, Class<T> type) {
            try { return values.containsKey(key) ? mapper.readValue(values.get(key), type) : null; }
            catch (Exception e) { throw new RuntimeException(e); }
        }
        @Override public void putBatchSync(Map<String, Object> data) { data.forEach(this::put); }
        @Override public void putBatch(Map<String, Object> data) { data.forEach(this::put); }
    }
}
