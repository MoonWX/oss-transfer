package com.transfer.tools.agent;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.service.LocalParentTaskService;
import com.transfer.tools.service.NodeConfigService;
import com.transfer.tools.service.MasterTaskBandwidthService;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class AgentRelayServiceTest {
    private final ObjectMapper mapper = new ObjectMapper();
    private final LocalParentTaskService parents = mock(LocalParentTaskService.class);
    private AgentRelayService service() {
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.isMaster()).thenReturn(true); when(config.getPort()).thenReturn(1685);
        when(config.getWorkerIps()).thenReturn(Arrays.asList("10.0.0.1", "10.0.0.2", "10.0.0.3"));
        when(config.getInternalToken()).thenReturn("master-local-token");
        return new AgentRelayService(config, mock(NodeConfigService.class), parents, mapper);
    }
    private RpcRelayRequest request(String path, String method) {
        RpcRelayRequest request = new RpcRelayRequest();
        request.setAddress("10.0.0.3"); request.setMethod(method); request.setPath(path); return request;
    }
    private LocalFileBatch batch() {
        LocalTransferRequest transfer = new LocalTransferRequest();
        transfer.setParentTaskId(2); transfer.setTaskId(2); transfer.setDirection(TransferDirection.LOCAL_TO_OSS);
        transfer.setConflictPolicy(ConflictPolicy.SKIP);
        transfer.setSourceEndpoint(new TransferEndpoint(EndpointType.LOCAL, null, null, null, null, "10.0.0.2", "/data"));
        transfer.getSourceEndpoint().setNodeBindings(Arrays.asList(new LocalNodeBinding("10.0.0.2", "/data"), new LocalNodeBinding("10.0.0.3", "/data")));
        transfer.setDestinationEndpoint(new TransferEndpoint(EndpointType.OSS, "storage", "bucket", null, null, null, null));
        LocalFileBatch batch = new LocalFileBatch(); batch.setParentTaskId(2); batch.setRequest(transfer);
        batch.setShardIndex(0); batch.setShardId("2-local-0"); batch.setAttempt(0);
        batch.setDiscoveryWorker("10.0.0.2");
        LocalNodeBinding binding = new LocalNodeBinding(); binding.setExecutionNodeIp("10.0.0.3"); binding.setLocalPath("/data");
        batch.setNodeBinding(binding); return batch;
    }
    @Test public void scannerCanReadOnlyRegisteredNodeCapacity() throws Exception {
        AgentRelayService service = service();
        MockRestServiceServer server = MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(service, "http")).build();
        server.expect(requestTo("http://10.0.0.3:1685/api/worker/worker-status"))
                .andExpect(header("X-Internal-Token", "master-local-token"))
                .andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));
        assertEquals(200, service.relay("10.0.0.2", request("/api/worker/worker-status", "GET")).getStatus());
        server.verify();
        RpcRelayRequest outside = request("/api/worker/worker-status", "GET"); outside.setAddress("10.1.1.1");
        try { service.relay("10.0.0.2", outside); fail(); } catch (SecurityException expected) { }
    }
    @Test public void agentCannotRelayAdminOperationsOrAnotherScannersBatch() throws Exception {
        AgentRelayService service = service();
        try { service.relay("10.0.0.2", request("/api/worker/s3-config", "POST")); fail(); }
        catch (SecurityException expected) { }
        LocalFileBatch batch = batch();
        LocalParentTaskRecord parent = new LocalParentTaskRecord(); parent.setRequest(batch.getRequest());
        when(parents.get(2)).thenReturn(parent);
        RpcRelayRequest request = request("/api/worker/local-transfers/batches", "POST");
        request.setBody(Base64.getEncoder().encodeToString(mapper.writeValueAsBytes(batch)));
        try { service.relay("10.0.0.1", request); fail(); } catch (SecurityException expected) { }
        when(parents.isStopped(2)).thenReturn(true);
        try { service.relay("10.0.0.2", request); fail(); } catch (SecurityException expected) { }
    }
    @Test public void savedParentPreventsChangingDestinationAndConflictPolicy() throws Exception {
        AgentRelayService service = service();
        LocalFileBatch batch = batch();
        LocalParentTaskRecord parent = new LocalParentTaskRecord();
        parent.setRequest(mapper.readValue(mapper.writeValueAsBytes(batch.getRequest()), LocalTransferRequest.class));
        when(parents.get(2)).thenReturn(parent);
        batch.getRequest().getDestinationEndpoint().setBucket("another-bucket");
        RpcRelayRequest request = request("/api/worker/local-transfers/batches", "POST");
        request.setBody(Base64.getEncoder().encodeToString(mapper.writeValueAsBytes(batch)));
        try { service.relay("10.0.0.2", request); fail(); } catch (SecurityException expected) { }
    }

    @Test public void assignedScannerCannotForgePolicyBatchIdentityOrEscapingFilePaths() throws Exception {
        AgentRelayService service = service();
        LocalFileBatch batch = batch();
        LocalParentTaskRecord parent = new LocalParentTaskRecord();
        parent.setRequest(mapper.readValue(mapper.writeValueAsBytes(batch.getRequest()), LocalTransferRequest.class));
        when(parents.get(2)).thenReturn(parent);
        RpcRelayRequest request = request("/api/worker/local-transfers/batches", "POST");
        batch.getRequest().setBandwidthBitsPerSecond(1000000000.0);
        request.setBody(Base64.getEncoder().encodeToString(mapper.writeValueAsBytes(batch)));
        try { service.relay("10.0.0.2", request); fail(); } catch (SecurityException expected) { }
        batch.getRequest().setBandwidthBitsPerSecond(null); batch.setShardId("2-local-500");
        request.setBody(Base64.getEncoder().encodeToString(mapper.writeValueAsBytes(batch)));
        try { service.relay("10.0.0.2", request); fail(); } catch (SecurityException expected) { }
        batch.setShardId("2-local-0"); batch.setEntries(Collections.singletonList(new LocalFileEntry("../private", 1L, 1L)));
        request.setBody(Base64.getEncoder().encodeToString(mapper.writeValueAsBytes(batch)));
        try { service.relay("10.0.0.2", request); fail(); } catch (SecurityException expected) { }
    }

    @Test public void assignedScannerCannotImpersonateAnotherTasksDiscoveryNode() throws Exception {
        AgentRelayService service = service();
        LocalFileBatch batch = batch(); batch.setDiscoveryWorker("10.0.0.3");
        LocalParentTaskRecord parent = new LocalParentTaskRecord(); parent.setRequest(batch.getRequest());
        when(parents.get(2)).thenReturn(parent);
        RpcRelayRequest request = request("/api/worker/local-transfers/batches", "POST");
        request.setBody(Base64.getEncoder().encodeToString(mapper.writeValueAsBytes(batch)));
        try { service.relay("10.0.0.3", request); fail(); } catch (SecurityException expected) { }
    }

    @Test public void masterOverwritesRuntimeLimitAndSerializesBatchDeliveryAgainstStop() throws Exception {
        AgentRelayService service = service();
        LocalFileBatch batch = batch();
        batch.getRequest().setTaskBandwidthScopeId("local:2"); batch.getRequest().setTaskBandwidthLimitMbps(10.0);
        LocalParentTaskRecord parent = new LocalParentTaskRecord();
        parent.setRequest(mapper.readValue(mapper.writeValueAsBytes(batch.getRequest()), LocalTransferRequest.class));
        when(parents.get(2)).thenReturn(parent);
        when(parents.whileActive(eq(2), any(Runnable.class))).thenAnswer(invocation -> { ((Runnable)invocation.getArgument(1)).run(); return true; });
        MasterTaskBandwidthService bandwidth = mock(MasterTaskBandwidthService.class);
        TaskBandwidthPolicy policy = new TaskBandwidthPolicy(); policy.setLimitMbps(5.0);
        policy.setMembers(Arrays.asList("10.0.0.2", "10.0.0.3"));
        when(bandwidth.desired("local:2")).thenReturn(policy);
        ReflectionTestUtils.setField(service, "bandwidth", bandwidth);
        batch.getRequest().setTaskBandwidthLimitMbps(999.0);
        RpcRelayRequest request = request("/api/worker/local-transfers/batches", "POST");
        request.setBody(Base64.getEncoder().encodeToString(mapper.writeValueAsBytes(batch)));
        batch.getRequest().setTaskBandwidthLimitMbps(5.0);
        MockRestServiceServer server = MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(service, "http")).build();
        server.expect(requestTo("http://10.0.0.3:1685/api/worker/local-transfers/batches"))
                .andExpect(content().json(mapper.writeValueAsString(batch)))
                .andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));
        assertEquals(200, service.relay("10.0.0.2", request).getStatus());
        verify(parents).whileActive(eq(2), any(Runnable.class));
        server.verify();
    }
}
