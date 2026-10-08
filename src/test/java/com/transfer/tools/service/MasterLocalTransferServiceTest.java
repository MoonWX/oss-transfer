package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.ApiResponse;
import com.transfer.tools.model.ConflictPolicy;
import com.transfer.tools.model.EndpointType;
import com.transfer.tools.model.FailedKey;
import com.transfer.tools.model.FailureStage;
import com.transfer.tools.model.LocalNodeBinding;
import com.transfer.tools.model.LocalParentTaskRecord;
import com.transfer.tools.model.LocalTransferRequest;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.model.TransferEndpoint;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.zip.ZipEntry;
import java.util.zip.ZipInputStream;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.never;
import static org.mockito.ArgumentMatchers.anyInt;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.ArgumentMatchers.argThat;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.jsonPath;
import static org.springframework.test.web.client.response.MockRestResponseCreators.withSuccess;

public class MasterLocalTransferServiceTest {

    @Test public void parentSpeedKeepsCompletedChildWindowTailWhileOtherChildrenRun() {
        MasterLocalTransferService service=failureQueryService();
        RestTemplate rest=(RestTemplate)ReflectionTestUtils.getField(service,"restTemplate");
        MockRestServiceServer server=MockRestServiceServer.bindTo(rest).build();
        for(String state:Arrays.asList("RUNNING","COMPLETED","STOPPED","FAILED")) {
            server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/status"))
                    .andRespond(withSuccess("{\"success\":true,\"data\":{\"tasks\":["
                            +"{\"taskIndex\":-1,\"parentTaskId\":86,\"direction\":\"OSS_TO_LOCAL\",\"status\":\"COMPLETED\",\"scanComplete\":true,\"currentSpeedBytesPerSecond\":5000000},"
                            +"{\"taskIndex\":-2,\"parentTaskId\":86,\"direction\":\"OSS_TO_LOCAL\",\"status\":\""+state+"\",\"scanComplete\":true,\"currentSpeedBytesPerSecond\":7000000}]}}",
                            org.springframework.http.MediaType.APPLICATION_JSON));
        }
        for(String state:Arrays.asList("RUNNING","COMPLETED","STOPPED","FAILED")) {
            Map<String,Object> response=service.aggregateStatus(null,null,null,null,null);
            SubTaskStatus parent=(SubTaskStatus)((List<?>)response.get("tasks")).get(0);
            assertEquals(Long.valueOf("RUNNING".equals(state)?12_000_000L:0L),parent.getCurrentSpeedBytesPerSecond());
        }
        server.verify();
    }

    @Test public void stopWithoutIdsCanCollectPersistedParents() {
        MasterLocalTransferService service = new MasterLocalTransferService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("127.0.0.1"));
        when(config.getPort()).thenReturn(8081);
        LocalParentTaskService parents = mock(LocalParentTaskService.class);
        when(parents.getAll()).thenReturn(Collections.singletonList(parent(86)));
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "parentTaskService", parents);
        MockRestServiceServer server = MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(service,"restTemplate")).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/stop"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"accepted\":1}}", org.springframework.http.MediaType.APPLICATION_JSON));
        assertEquals(1, service.control("stop", (Map<String,Object>)null).get("accepted"));
        verify(parents).markStopped(86);
        server.verify();
    }

    @Test
    @SuppressWarnings("unchecked")
    public void returnsPersistedParentBeforeWorkerCreatesShards() {
        MasterLocalTransferService service = new MasterLocalTransferService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("127.0.0.1"));
        when(config.getPort()).thenReturn(8081);
        when(config.getInternalToken()).thenReturn("");
        LocalParentTaskService parents = mock(LocalParentTaskService.class);
        LocalParentTaskRecord record = parent(86);
        when(parents.getAll()).thenReturn(Collections.singletonList(record));
        when(parents.toStatus(record)).thenReturn(parentStatus(record));
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "parentTaskService", parents);

        RestTemplate restTemplate = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(restTemplate).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/status"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"tasks\":[]}}",
                        org.springframework.http.MediaType.APPLICATION_JSON));

        Map<String, Object> result = service.aggregateStatus(null, null, null, null, null, null);

        assertEquals(1, result.get("totalTasks"));
        assertEquals(1, result.get("totalParentTasks"));
        List<SubTaskStatus> tasks = (List<SubTaskStatus>) result.get("tasks");
        assertEquals(Integer.valueOf(86), tasks.get(0).getTaskIndex());
        assertEquals(SubTaskStatus.TaskStatus.PENDING, tasks.get(0).getStatus());
        assertTrue(((List<?>) result.get("shards")).isEmpty());
        server.verify();
    }

    @Test
    @SuppressWarnings("unchecked")
    public void exposesParentOrchestrationFailureWithoutWorkerShard() {
        MasterLocalTransferService service = new MasterLocalTransferService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.emptyList());
        LocalParentTaskService parents = mock(LocalParentTaskService.class);
        LocalParentTaskRecord record = parent(86);
        record.setStatus(SubTaskStatus.TaskStatus.FAILED);
        record.setErrorCode("LOCAL_PATH_NOT_WRITABLE");
        record.setErrorMessage("destination is read-only");
        record.setUpdatedAt(123L);
        when(parents.getAll()).thenReturn(Collections.singletonList(record));
        when(parents.toStatus(record)).thenReturn(parentStatus(record));
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "parentTaskService", parents);

        Map<String, Object> result = service.aggregateFailures(
                86, TransferDirection.OSS_TO_LOCAL, null, null, 0, 100);

        assertEquals(1, result.get("total"));
        List<FailedKey> failures = (List<FailedKey>) result.get("items");
        assertEquals(1, failures.size());
        assertEquals(Integer.valueOf(86), failures.get(0).getParentTaskId());
        assertEquals("LOCAL_PATH_NOT_WRITABLE", failures.get(0).getErrorCode());
        assertEquals(FailureStage.DISPATCH, failures.get(0).getStage());
    }

    @Test
    @SuppressWarnings("unchecked")
    public void aggregatesSerializedWorkerFailuresWithoutFalseUnreachable() throws Exception {
        MasterLocalTransferService service = failureQueryService();
        MockRestServiceServer server = workerFailureResponse(service,
                "/api/worker/local-transfers/failures?taskId=3&offset=0&limit=1000");

        Map<String, Object> result = service.aggregateFailures(3, null, null, null, 0, 25);

        assertEquals(2, result.get("total"));
        List<FailedKey> failures = (List<FailedKey>) result.get("items");
        assertEquals(2, failures.size());
        assertEquals(false, result.get("partial"));
        assertTrue(((List<?>) result.get("unreachableWorkers")).isEmpty());
        assertEquals(Integer.valueOf(3), failures.get(0).getParentTaskId());
        assertEquals(FailureStage.WRITE_DESTINATION, failures.get(0).getStage());
        assertEquals("127.0.0.1", failures.get(0).getWorkerNode());
        assertEquals("fixture/small/alpha.bin", failures.get(0).getKey());
        assertEquals("2026-09-22 18:46:06", failures.get(0).getTimestampFormatted());
        ObjectMapper mapper = new ObjectMapper();
        assertEquals("2026-09-22 18:46:06", mapper.readTree(mapper.writeValueAsString(result))
                .path("items").get(0).path("timestampFormatted").asText());
        server.verify();
    }

    @Test
    public void exportsSerializedWorkerFailuresAsRealReportRows() throws Exception {
        MasterLocalTransferService service = failureQueryService();
        MockRestServiceServer server = workerFailureResponse(service,
                "/api/worker/local-transfers/failures?offset=0&limit=1000");
        List<String> unreachable = new ArrayList<>();

        List<FailedKey> failures = service.reportFailures(unreachable);

        assertEquals(2, failures.size());
        assertTrue(unreachable.isEmpty());
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        new LocalReportService().write(output, Collections.emptyList(), failures, unreachable);
        String exceptionCsv = null;
        try (ZipInputStream zip = new ZipInputStream(new ByteArrayInputStream(output.toByteArray()), StandardCharsets.UTF_8)) {
            ZipEntry entry;
            byte[] buffer = new byte[1024];
            while ((entry = zip.getNextEntry()) != null) {
                if (!"exception-details.csv".equals(entry.getName())) continue;
                ByteArrayOutputStream content = new ByteArrayOutputStream();
                int read;
                while ((read = zip.read(buffer)) > 0) content.write(buffer, 0, read);
                exceptionCsv = new String(content.toByteArray(), StandardCharsets.UTF_8);
            }
        }
        assertTrue(exceptionCsv != null);
        assertEquals(3, exceptionCsv.trim().split("\\r?\\n").length);
        assertTrue(exceptionCsv.contains("fixture/small/alpha.bin"));
        assertTrue(exceptionCsv.contains("fixture/small/nested/beta.bin"));
        assertTrue(exceptionCsv.contains("WRITE_DESTINATION"));
        assertTrue(exceptionCsv.contains("TRANSFER_FAILED"));
        assertFalse(exceptionCsv.contains("WORKER_UNAVAILABLE"));
        server.verify();
    }

    @Test
    public void recordsParentStopBeforeContactingWorkers() {
        MasterLocalTransferService service = failureQueryService();
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTaskService");
        when(parents.get(86)).thenReturn(parent(86));
        RestTemplate rest = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(rest).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/stop"))
                .andRespond(request -> {
                    verify(parents).markStopped(86);
                    return withSuccess("{\"success\":true,\"data\":{\"accepted\":2}}",
                            org.springframework.http.MediaType.APPLICATION_JSON).createResponse(request);
                });
        Map<String, Object> result = service.control("stop", Collections.singletonList(86));
        assertEquals(2, result.get("accepted"));
        assertEquals(false, result.get("partial"));
        server.verify();
    }

    @Test
    public void failedResumeStopsAlreadyResumedNodesAndKeepsParentStopped() {
        MasterLocalTransferService service = failureQueryService();
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTaskService");
        when(parents.get(86)).thenReturn(parent(86));
        RestTemplate rest = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(rest).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/resume"))
                .andRespond(withSuccess("{\"success\":false,\"message\":\"unavailable\"}",
                        org.springframework.http.MediaType.APPLICATION_JSON));
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/stop"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{}}", org.springframework.http.MediaType.APPLICATION_JSON));
        Map<String, Object> result = service.control("resume", Collections.singletonList(86));
        assertEquals(0, result.get("accepted"));
        assertEquals(true, result.get("partial"));
        verify(parents).markStopped(86);
        verify(parents, never()).markResuming(anyInt());
        server.verify();
    }

    @Test
    public void resumeOfDeletedParentNeverSendsAnEmptyAllTasksCommand() {
        MasterLocalTransferService service = failureQueryService();
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTaskService");
        when(parents.isDeleted(86)).thenReturn(true);
        RestTemplate rest = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(rest).build();
        assertEquals(0, service.control("resume", Collections.singletonList(86)).get("accepted"));
        verify(parents, never()).markResuming(anyInt());
        server.verify();
    }

    @Test
    public void completedShardsDoNotCompleteParentWhileMoreRangesAreStillBeingDiscovered() {
        MasterLocalTransferService service = new MasterLocalTransferService();
        SubTaskStatus persisted = parentStatus(parent(86));
        persisted.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        persisted.setScanComplete(false);
        SubTaskStatus derived = parentStatus(parent(86));
        derived.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
        derived.setScanComplete(true);
        derived.setPutObjectCount(1000L); derived.setListObjectCount(1000L);
        derived.setEndTime(123L);
        SubTaskStatus merged = ReflectionTestUtils.invokeMethod(service,
                "mergePersistedParent", persisted, derived, 1);
        assertEquals(SubTaskStatus.TaskStatus.RUNNING, merged.getStatus());
        assertFalse(merged.getScanComplete());
        assertEquals(null, merged.getEndTime());
        assertEquals(Long.valueOf(1000), merged.getListObjectCount());
    }

    @Test
    public void legacyRecoveryStartsAfterTheLastAcknowledgedContiguousRange() {
        MasterLocalTransferService service = failureQueryService();
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTaskService");
        LocalParentTaskRecord record = parent(86); record.setStartTime(1L);
        record.getRequest().getSourceEndpoint().setPrefix("objects/");
        RestTemplate rest = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(rest).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/status?taskId=86"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"tasks\":["
                        + "{\"parentTaskId\":86,\"shardIndex\":1,\"startKey\":\"objects/a\",\"endKey\":\"objects/b\"},"
                        + "{\"parentTaskId\":86,\"shardIndex\":0,\"startKey\":\"objects/\",\"endKey\":\"objects/a\"}]}}",
                        org.springframework.http.MediaType.APPLICATION_JSON));
        service.prepareLegacyCheckpoint(record);
        verify(parents).saveExploreCheckpoint(eq(86), argThat(state ->
                state.getNextShardIndex() == 2 && "objects/b".equals(state.getMarker()) && !state.isComplete()));
        server.verify();
    }

    @Test
    public void legacyRecoveryRejectsMissingRangeBeforeResumingAnyWorker() {
        MasterLocalTransferService service = failureQueryService();
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTaskService");
        LocalParentTaskRecord record = parent(86); record.setStartTime(1L);
        when(parents.get(86)).thenReturn(record);
        RestTemplate rest = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(rest).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/status?taskId=86"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"tasks\":["
                        + "{\"parentTaskId\":86,\"shardIndex\":1,\"startKey\":\"a\",\"endKey\":\"b\"}]}}",
                        org.springframework.http.MediaType.APPLICATION_JSON));
        Map<String, Object> result = service.control("resume", Collections.singletonList(86));
        assertEquals(0, result.get("accepted")); assertEquals(1, result.get("failed"));
        assertEquals("INVALID_RESUME_CHECKPOINT", result.get("code"));
        verify(parents, never()).markResuming(anyInt());
        server.verify();
    }

    @Test
    public void permanentParentFenceHidesResidualWorkerDiscoveryRows() {
        MasterLocalTransferService service = failureQueryService();
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTaskService");
        when(parents.isDeleted(86)).thenReturn(true);
        RestTemplate rest = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(rest).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/status"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"tasks\":["
                        + "{\"taskIndex\":86,\"parentTaskId\":86,\"shardId\":\"discovery\","
                        + "\"direction\":\"LOCAL_TO_OSS\",\"status\":\"COMPLETED\"}]}}",
                        org.springframework.http.MediaType.APPLICATION_JSON));
        Map<String, Object> result = service.aggregateStatus(null, null, null, null, null, null);
        assertEquals(0, result.get("totalTasks"));
        assertTrue(((List<?>)result.get("tasks")).isEmpty());
        assertTrue(((List<?>)result.get("shards")).isEmpty());
        server.verify();
    }

    @Test
    public void uploadResumeDefersDiscoveryUntilEveryWorkerHasResumed() {
        MasterLocalTransferService service = failureQueryService();
        DistributedConfig config = (DistributedConfig) ReflectionTestUtils.getField(service, "distributedConfig");
        when(config.getWorkerIps()).thenReturn(Arrays.asList("127.0.0.1", "127.0.0.2"));
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTaskService");
        LocalParentTaskRecord record = parent(86);
        LocalTransferRequest request = record.getRequest();
        TransferEndpoint local = request.getDestinationEndpoint();
        local.setNodeBindings(Arrays.asList(new LocalNodeBinding("127.0.0.1", "/data"), new LocalNodeBinding("127.0.0.2", "/data")));
        request.setDestinationEndpoint(request.getSourceEndpoint()); request.setSourceEndpoint(local);
        request.setDirection(TransferDirection.LOCAL_TO_OSS); record.setOrchestrationComplete(true);
        record.setStatus(SubTaskStatus.TaskStatus.STOPPED);
        when(parents.get(86)).thenReturn(record);
        RestTemplate rest = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(rest).build();
        for (String worker : Arrays.asList("127.0.0.1", "127.0.0.2"))
            server.expect(requestTo("http://" + worker + ":8081/api/worker/local-transfers/resume"))
                    .andExpect(jsonPath("$.deferDiscovery").value(true))
                    .andRespond(withSuccess("{\"success\":true,\"data\":{\"accepted\":1}}", org.springframework.http.MediaType.APPLICATION_JSON));
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/local-transfers/discover"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"accepted\":true}}", org.springframework.http.MediaType.APPLICATION_JSON));
        Map<String, Object> result = service.control("resume", Collections.singletonList(86));
        assertEquals(0, result.get("failed")); assertEquals(false, result.get("partial"));
        verify(parents).markResuming(86);
        verify(parents).markOrchestrationComplete(86);
        server.verify();
    }

    private MasterLocalTransferService failureQueryService() {
        MasterLocalTransferService service = new MasterLocalTransferService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("127.0.0.1"));
        when(config.getPort()).thenReturn(8081);
        LocalParentTaskService parents = mock(LocalParentTaskService.class);
        when(parents.getAll()).thenReturn(Collections.emptyList());
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "parentTaskService", parents);
        return service;
    }

    private MockRestServiceServer workerFailureResponse(MasterLocalTransferService service, String path) throws Exception {
        Map<String, Object> data = new LinkedHashMap<>();
        data.put("total", 2);
        data.put("items", Arrays.asList(workerFailure("fixture/small/alpha.bin"),
                workerFailure("fixture/small/nested/beta.bin")));
        ObjectMapper mapper = new ObjectMapper();
        String response = mapper.writeValueAsString(ApiResponse.success(data));
        assertEquals("2026-09-22 18:46:06", mapper.readTree(response)
                .path("data").path("items").get(0).path("timestampFormatted").asText());
        RestTemplate restTemplate = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(restTemplate).build();
        server.expect(requestTo("http://127.0.0.1:8081" + path))
                .andRespond(withSuccess(response, org.springframework.http.MediaType.APPLICATION_JSON));
        return server;
    }

    private FailedKey workerFailure(String key) {
        FailedKey failure = new FailedKey();
        failure.setTaskIndex(-1495011755);
        failure.setParentTaskId(3);
        failure.setShardId("3-oss-0");
        failure.setDirection(TransferDirection.OSS_TO_LOCAL);
        failure.setKey(key);
        failure.setTimestamp(1790073966877L);
        failure.setObjectSize(16384L);
        failure.setExecutionNodeIp("127.0.0.1");
        failure.setStage(FailureStage.WRITE_DESTINATION);
        failure.setErrorCode("TRANSFER_FAILED");
        failure.setErrorMessage("destination path is not a directory");
        return failure;
    }

    private LocalParentTaskRecord parent(int id) {
        LocalTransferRequest request = new LocalTransferRequest();
        request.setTaskId(id);
        request.setParentTaskId(id);
        request.setDirection(TransferDirection.OSS_TO_LOCAL);
        request.setConflictPolicy(ConflictPolicy.SKIP);
        request.setDistributionMode("balanced");
        TransferEndpoint source = new TransferEndpoint();
        source.setType(EndpointType.OSS);
        source.setS3Alias("source");
        source.setBucket("bucket");
        TransferEndpoint destination = new TransferEndpoint();
        destination.setType(EndpointType.LOCAL);
        destination.setStorageId("shared");
        destination.setNodeBindings(Arrays.asList(new LocalNodeBinding("127.0.0.1", "/data")));
        request.setSourceEndpoint(source);
        request.setDestinationEndpoint(destination);
        return new LocalParentTaskRecord(id, request, SubTaskStatus.TaskStatus.PENDING,
                null, null, false, 1L, null, null, 1L);
    }

    private SubTaskStatus parentStatus(LocalParentTaskRecord record) {
        SubTaskStatus status = new SubTaskStatus();
        status.setTaskIndex(record.getParentTaskId());
        status.setParentTaskId(record.getParentTaskId());
        status.setDirection(record.getRequest().getDirection());
        status.setSourceEndpoint(record.getRequest().getSourceEndpoint());
        status.setDestinationEndpoint(record.getRequest().getDestinationEndpoint());
        status.setStorageId("shared");
        status.setEligibleWorkerNodes(Collections.singletonList("127.0.0.1"));
        status.setStatus(record.getStatus());
        status.setListObjectCount(0L); status.setListObjectSize(0L);
        status.setPutObjectCount(0L); status.setPutObjectSize(0L);
        status.setSkippedObjectCount(0L); status.setSkippedObjectSize(0L);
        status.setFailedObjectCount(0L); status.setFailedObjectSize(0L);
        return status;
    }
}
