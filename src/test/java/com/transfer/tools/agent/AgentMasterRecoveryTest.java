package com.transfer.tools.agent;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.service.LocalParentTaskService;
import com.transfer.tools.service.MasterLocalTransferService;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.net.SocketTimeoutException;
import java.util.Collections;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class AgentMasterRecoveryTest {
    private final MasterLocalTransferService service = new MasterLocalTransferService();
    private final AgentRegistryService agents = mock(AgentRegistryService.class);
    private final LocalParentTaskService parents = mock(LocalParentTaskService.class);
    private final LocalParentTaskRecord record = new LocalParentTaskRecord();

    private MockRestServiceServer setup() {
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.isMaster()).thenReturn(true); when(config.getPort()).thenReturn(1685);
        LocalTransferRequest request = new LocalTransferRequest(); request.setParentTaskId(2); request.setTaskId(2);
        request.setDirection(TransferDirection.LOCAL_TO_OSS);
        TransferEndpoint source = new TransferEndpoint(EndpointType.LOCAL, null, null, null, null, "10.0.0.2", "/data");
        source.setNodeBindings(Collections.singletonList(new LocalNodeBinding("10.0.0.2", "/data")));
        request.setSourceEndpoint(source);
        request.setDestinationEndpoint(new TransferEndpoint(EndpointType.OSS, "storage", "bucket", null, null, null, null));
        record.setParentTaskId(2); record.setRequest(request); record.setStatus(SubTaskStatus.TaskStatus.PENDING);
        record.setOrchestrationComplete(false);
        when(parents.recoverable()).thenReturn(Collections.singletonList(record)); when(parents.get(2)).thenReturn(record);
        when(parents.markRunning(2)).thenReturn(true);
        when(agents.isAgent("10.0.0.2")).thenReturn(true);
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "parentTaskService", parents);
        ReflectionTestUtils.setField(service, "agentRegistry", agents);
        return MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(service, "restTemplate")).build();
    }

    @Test public void masterRestartDefersOfflineParentAndRetriesOnceAfterSynchronization() {
        MockRestServiceServer server = setup();
        server.expect(requestTo("http://10.0.0.2:1685/api/worker/local-transfers/discover"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"accepted\":true}}", MediaType.APPLICATION_JSON));
        when(agents.online("10.0.0.2")).thenReturn(false);
        service.recoverPendingParents(); service.retryDeferredParents();
        verify(parents, never()).markRunning(2); verify(parents, never()).markFailed(anyInt(), anyString(), anyString());
        assertEquals(SubTaskStatus.TaskStatus.PENDING, record.getStatus());
        when(agents.online("10.0.0.2")).thenReturn(true);
        service.retryDeferredParents(); service.retryDeferredParents(); service.recoverPendingParents();
        verify(parents, times(1)).markRunning(2); verify(parents, times(1)).markOrchestrationComplete(2);
        server.verify();
    }

    @Test public void provenBeforeDeliveryDisconnectCanBeDeferredWithoutMarkingFailed() {
        MockRestServiceServer server = setup(); when(agents.online("10.0.0.2")).thenReturn(true);
        server.expect(requestTo("http://10.0.0.2:1685/api/worker/local-transfers/discover"))
                .andRespond(request -> { throw new AgentUnavailableException("Agent disconnected before delivery"); });
        server.expect(requestTo("http://10.0.0.2:1685/api/worker/local-transfers/discover"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"accepted\":true}}", MediaType.APPLICATION_JSON));
        service.recoverPendingParents();
        verify(parents).deferUnstartedRecovery(2); verify(parents, never()).markFailed(anyInt(), anyString(), anyString());
        service.retryDeferredParents(); service.retryDeferredParents();
        verify(parents, times(1)).markOrchestrationComplete(2); server.verify();
    }

    @Test public void ambiguousMutationTimeoutIsNeverRetriedAutomatically() {
        MockRestServiceServer server = setup(); when(agents.online("10.0.0.2")).thenReturn(true);
        server.expect(requestTo("http://10.0.0.2:1685/api/worker/local-transfers/discover"))
                .andRespond(request -> { throw new SocketTimeoutException("Acknowledgement outcome unknown"); });
        service.recoverPendingParents(); service.retryDeferredParents(); service.recoverPendingParents();
        verify(parents, times(1)).markFailed(eq(2), eq("WORKER_UNAVAILABLE"), anyString());
        verify(parents, never()).deferUnstartedRecovery(anyInt()); server.verify();
    }

    @Test public void stoppedDeferredTaskDoesNotRestartOnAgentReconnection() {
        MockRestServiceServer server = setup(); when(agents.online("10.0.0.2")).thenReturn(false);
        service.recoverPendingParents();
        when(parents.isStopped(2)).thenReturn(true); when(agents.online("10.0.0.2")).thenReturn(true);
        service.retryDeferredParents();
        verify(parents, never()).markRunning(anyInt()); server.verify();
    }
}
