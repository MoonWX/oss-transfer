package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.controller.WorkerController;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.MediaType;
import org.springframework.mock.http.client.MockClientHttpRequest;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.List;
import java.util.Map;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo;
import static org.springframework.test.web.client.response.MockRestResponseCreators.withSuccess;

public class LocalTransferOptionalStorageIdTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private final ObjectMapper mapper = new ObjectMapper();
    private final EndpointNormalizationService normalization = new EndpointNormalizationService();

    @Test
    public void uploadsAcrossTwoWorkersWithoutStorageId() throws Exception {
        dispatchUpload(null);
    }

    @Test
    public void downloadsAcrossTwoWorkersWithoutStorageId() throws Exception {
        dispatchDownload(null);
    }

    @Test
    public void preservesExistingStorageIdThroughBothDispatchPaths() throws Exception {
        dispatchUpload("existing-shared-storage");
        dispatchDownload("existing-shared-storage");
    }

    private void dispatchUpload(String storageId) throws Exception {
        LocalTransferRequest request = request(TransferDirection.LOCAL_TO_OSS, storageId);
        List<LocalNodeBinding> bindings = request.getSourceEndpoint().getNodeBindings();
        Worker first = worker(bindings.get(0), request);
        Worker second = worker(bindings.get(1), request);
        MockRestServiceServer server = MockRestServiceServer.bindTo(restTemplate(first.local)).build();
        server.expect(requestTo("http://10.0.0.2:8081/api/worker/local-transfers/batches"))
                .andRespond(http -> {
                    LocalFileBatch batch = mapper.readValue(((MockClientHttpRequest) http).getBodyAsString(), LocalFileBatch.class);
                    second.local.acceptBatch(batch);
                    return withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON).createResponse(http);
                });

        first.local.dispatchBatch(request, bindings, 0,
                Collections.singletonList(new LocalFileEntry("first.txt", 1L, 2L)), false);
        first.local.dispatchBatch(request, bindings, 1,
                Collections.singletonList(new LocalFileEntry("second.txt", 1L, 2L)), true);

        assertAssigned(first, bindings.get(0), storageId, TransferDirection.LOCAL_TO_OSS);
        assertAssigned(second, bindings.get(1), storageId, TransferDirection.LOCAL_TO_OSS);
        assertEquals("first.txt", first.accepted.get(0).getLocalFileEntries().get(0).getRelativeKey());
        assertEquals("second.txt", second.accepted.get(0).getLocalFileEntries().get(0).getRelativeKey());
        server.verify();
    }

    private void dispatchDownload(String storageId) throws Exception {
        LocalTransferRequest request = request(TransferDirection.OSS_TO_LOCAL, storageId);
        List<LocalNodeBinding> bindings = request.getDestinationEndpoint().getNodeBindings();
        Worker first = worker(bindings.get(0), request);
        Worker second = worker(bindings.get(1), request);
        DistributedService distributor = new DistributedService();
        ReflectionTestUtils.setField(distributor, "distributedConfig", first.config);
        ReflectionTestUtils.setField(distributor, "subTaskManagerService", first.tasks);
        when(first.tasks.preferredLocalShardTaskId(any(), anyInt(), anyString())).thenReturn(-101, -102);
        MockRestServiceServer server = MockRestServiceServer.bindTo(restTemplate(distributor)).build();
        for (Worker receiver : Arrays.asList(first, second)) {
            server.expect(requestTo("http://" + receiver.config.getLocalIp() + ":8081/api/worker/tasks"))
                    .andRespond(http -> {
                        List<Map<String, Object>> body = mapper.readValue(((MockClientHttpRequest) http).getBodyAsString(),
                                new TypeReference<List<Map<String, Object>>>() {});
                        Map<String, Object> result = receiver.controller.receiveTasks(body);
                        assertEquals(Boolean.TRUE, result.get("success"));
                        return withSuccess(mapper.writeValueAsString(result), MediaType.APPLICATION_JSON).createResponse(http);
                    });
        }
        ListSubTask firstShard = first.local.toSubTask(request);
        firstShard.setShardId("86-oss-0");
        ListSubTask secondShard = first.local.toSubTask(request);
        secondShard.setShardId("86-oss-1");

        distributor.distributeTasks(Arrays.asList(firstShard, secondShard), request.getDistributionMode(),
                Arrays.asList("10.0.0.1", "10.0.0.2"), bindings);

        assertAssigned(first, bindings.get(0), storageId, TransferDirection.OSS_TO_LOCAL);
        assertAssigned(second, bindings.get(1), storageId, TransferDirection.OSS_TO_LOCAL);
        server.verify();
    }

    private LocalTransferRequest request(TransferDirection direction, String storageId) throws Exception {
        TransferEndpoint local = new TransferEndpoint();
        local.setType(EndpointType.LOCAL);
        local.setStorageId(storageId);
        local.setNodeBindings(Arrays.asList(
                new LocalNodeBinding("10.0.0.1", temporary.newFolder().getAbsolutePath()),
                new LocalNodeBinding("10.0.0.2", temporary.newFolder().getAbsolutePath())));
        TransferEndpoint oss = normalization.oss("source", "bucket", null, "fixture/");
        LocalTransferRequest request = new LocalTransferRequest();
        request.setTaskId(86);
        request.setParentTaskId(86);
        request.setDirection(direction);
        request.setSourceEndpoint(direction == TransferDirection.LOCAL_TO_OSS ? local : oss);
        request.setDestinationEndpoint(direction == TransferDirection.OSS_TO_LOCAL ? local : oss);
        normalization.normalizeAndValidate(request);
        assertEquals("balanced", request.getDistributionMode());
        assertEquals(storageId, local.getStorageId());
        return request;
    }

    private Worker worker(LocalNodeBinding binding, LocalTransferRequest request) throws Exception {
        Worker worker = new Worker();
        worker.config = mock(DistributedConfig.class);
        when(worker.config.getLocalIp()).thenReturn(binding.getExecutionNodeIp());
        when(worker.config.getPort()).thenReturn(8081);
        worker.tasks = mock(SubTaskManagerService.class);
        BufferQueueManager queues = mock(BufferQueueManager.class);
        LocalFileService files = new LocalFileService();
        S3ClientManager s3 = mock(S3ClientManager.class);
        when(s3.getClient("source")).thenReturn(mock(AmazonS3.class));
        worker.local = new LocalTransferService();
        ReflectionTestUtils.setField(worker.local, "distributedConfig", worker.config);
        ReflectionTestUtils.setField(worker.local, "normalizationService", normalization);
        ReflectionTestUtils.setField(worker.local, "localFileService", files);
        ReflectionTestUtils.setField(worker.local, "s3ClientManager", s3);
        ReflectionTestUtils.setField(worker.local, "subTaskManagerService", worker.tasks);
        ReflectionTestUtils.setField(worker.local, "bufferQueueManager", queues);
        ReflectionTestUtils.setField(worker.local, "controlService", new LocalTaskControlService());
        ReflectionTestUtils.setField(worker.local, "rocksDBService", mock(RocksDBService.class));
        when(worker.tasks.saveAssignedLocalTask(any(ListSubTask.class))).thenAnswer(call -> {
            ListSubTask shard = call.getArgument(0);
            normalization.normalize(shard);
            // Production saveAssignedTask copies execution fields explicitly; these
            // two scheduling weights are not part of the persisted execution status.
            Map<String, Object> executionFields = mapper.convertValue(shard, new TypeReference<Map<String, Object>>() {});
            executionFields.remove("estimatedObjectCount");
            executionFields.remove("estimatedObjectSize");
            worker.accepted.add(mapper.convertValue(executionFields, SubTaskStatus.class));
            return -101;
        });
        doAnswer(call -> {
            for (TaskImport task : (List<TaskImport>) call.getArgument(0)) {
                SubTaskStatus status = mapper.convertValue(task, SubTaskStatus.class);
                normalization.normalize(status);
                worker.accepted.add(status);
            }
            return null;
        }).when(worker.tasks).importTasksWithTaskId(anyList());
        worker.controller = new WorkerController();
        ReflectionTestUtils.setField(worker.controller, "distributedConfig", worker.config);
        ReflectionTestUtils.setField(worker.controller, "objectMapper", mapper);
        ReflectionTestUtils.setField(worker.controller, "subTaskManagerService", worker.tasks);
        ReflectionTestUtils.setField(worker.controller, "bufferQueueManager", queues);
        ReflectionTestUtils.setField(worker.controller, "localFileService", files);
        Map<String, Object> validation = worker.local.validateOnWorker(request);
        assertEquals(Boolean.TRUE, validation.get("valid"));
        assertEquals(binding.getLocalPath(), validation.get("localPath"));
        return worker;
    }

    private void assertAssigned(Worker worker, LocalNodeBinding binding, String storageId, TransferDirection direction) {
        assertEquals(1, worker.accepted.size());
        SubTaskStatus shard = worker.accepted.get(0);
        assertEquals(direction, shard.getDirection());
        assertEquals(storageId, shard.getStorageId());
        assertEquals(binding.getExecutionNodeIp(), shard.getExecutionNodeIp());
        assertEquals(binding.getLocalPath(), shard.getLocalPath());
        assertEquals(Integer.valueOf(86), shard.getParentTaskId());
        assertEquals(Arrays.asList("10.0.0.1", "10.0.0.2"), shard.getEligibleWorkerNodes());
    }

    private RestTemplate restTemplate(Object service) {
        return (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
    }

    private static class Worker {
        private DistributedConfig config;
        private SubTaskManagerService tasks;
        private LocalTransferService local;
        private WorkerController controller;
        private final List<SubTaskStatus> accepted = new ArrayList<>();
    }
}
