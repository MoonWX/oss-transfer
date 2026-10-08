package com.transfer.tools.agent;

import com.transfer.tools.config.DistributedConfig;
import org.junit.Test;
import org.springframework.beans.factory.ObjectProvider;
import org.springframework.http.HttpMethod;
import org.springframework.http.client.*;
import org.springframework.mock.env.MockEnvironment;
import org.springframework.web.client.RestTemplate;
import java.net.URI;
import java.util.Base64;
import java.util.concurrent.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class AgentHttpRequestFactoryTest {
    @SuppressWarnings("unchecked") private static <T> ObjectProvider<T> provider(T value) {
        ObjectProvider<T> provider = mock(ObjectProvider.class); when(provider.getObject()).thenReturn(value); return provider;
    }

    @Test public void routesRegisteredAgentWithoutConnectingToItsAddress() throws Exception {
        DistributedConfig config = mock(DistributedConfig.class); when(config.getPort()).thenReturn(1685);
        AgentRegistryService registry = mock(AgentRegistryService.class); when(registry.isAgent("10.0.0.2")).thenReturn(true);
        when(registry.connected("10.0.0.2")).thenReturn(true);
        ClientHttpRequestFactory fallback = mock(ClientHttpRequestFactory.class);
        AgentBroker broker = new AgentBroker();
        AgentHttpRequestFactory factory = new AgentHttpRequestFactory(fallback, provider(registry), provider(broker),
                provider(mock(AgentIdentityStore.class)), provider(config), new MockEnvironment());
        ExecutorService executor = Executors.newSingleThreadExecutor();
        try {
            Future<String> response = executor.submit(() -> new RestTemplate(factory).getForObject("http://10.0.0.2:1685/api/worker/status?taskId=1", String.class));
            RpcCommand command = broker.poll("10.0.0.2", 1000);
            assertEquals("GET", command.getMethod()); assertEquals("/api/worker/status?taskId=1", command.getPath());
            RpcResult result = new RpcResult(); result.setId(command.getId()); result.setStatus(200);
            result.setBody(Base64.getEncoder().encodeToString("{\"success\":true}".getBytes()));
            broker.complete("10.0.0.2", result);
            assertEquals("{\"success\":true}", response.get(1, TimeUnit.SECONDS));
            verifyZeroInteractions(fallback);
        } finally { executor.shutdownNow(); broker.close(); }
    }

    @Test public void leavesStorageAndUnregisteredLegacyNodesOnOriginalTransport() throws Exception {
        DistributedConfig config = mock(DistributedConfig.class); when(config.getPort()).thenReturn(1685);
        AgentRegistryService registry = mock(AgentRegistryService.class); when(registry.isAgent("10.0.0.2")).thenReturn(true);
        ClientHttpRequestFactory fallback = mock(ClientHttpRequestFactory.class);
        ClientHttpRequest request = mock(ClientHttpRequest.class);
        when(fallback.createRequest(any(URI.class), any(HttpMethod.class))).thenReturn(request);
        AgentHttpRequestFactory factory = new AgentHttpRequestFactory(fallback, provider(registry), provider(new AgentBroker()),
                provider(mock(AgentIdentityStore.class)), provider(config), new MockEnvironment());
        assertSame(request, factory.createRequest(URI.create("http://10.0.0.2:9000/bucket/key"), HttpMethod.GET));
        assertSame(request, factory.createRequest(URI.create("http://10.0.0.2:1685/bucket/key"), HttpMethod.GET));
        assertSame(request, factory.createRequest(URI.create("http://10.0.0.3:1685/api/worker/status"), HttpMethod.GET));
        verify(fallback, times(3)).createRequest(any(URI.class), any(HttpMethod.class));
    }

    @Test public void agentUsesLoopbackForItsOwnIdentityAndRelaysMasterIdentityEvenWhen127() throws Exception {
        DistributedConfig config = mock(DistributedConfig.class); when(config.getPort()).thenReturn(1685);
        when(config.getLocalIp()).thenReturn("10.0.0.2");
        ClientHttpRequestFactory fallback = mock(ClientHttpRequestFactory.class);
        ClientHttpRequest request = mock(ClientHttpRequest.class);
        when(fallback.createRequest(any(URI.class), any(HttpMethod.class))).thenReturn(request);
        AgentHttpRequestFactory factory = new AgentHttpRequestFactory(fallback, provider(mock(AgentRegistryService.class)),
                provider(new AgentBroker()), provider(mock(AgentIdentityStore.class)), provider(config),
                new MockEnvironment().withProperty("transfer.role", "agent"));
        assertSame(request, factory.createRequest(URI.create("http://10.0.0.2:1685/api/worker/worker-status?taskId=1"), HttpMethod.GET));
        verify(fallback).createRequest(URI.create("http://127.0.0.1:1685/api/worker/worker-status?taskId=1"), HttpMethod.GET);
        assertNotSame(request, factory.createRequest(URI.create("http://127.0.0.1:1685/api/worker/worker-status"), HttpMethod.GET));
        verify(fallback, times(1)).createRequest(any(URI.class), any(HttpMethod.class));
    }

    @Test public void offlineOrRevokedKnownAgentFailsImmediatelyWithoutQueueOrLegacyFallback() throws Exception {
        DistributedConfig config = mock(DistributedConfig.class); when(config.getPort()).thenReturn(1685);
        AgentRegistryService registry = mock(AgentRegistryService.class); when(registry.isAgent("10.0.0.2")).thenReturn(true);
        when(registry.connected("10.0.0.2")).thenReturn(false);
        ClientHttpRequestFactory fallback = mock(ClientHttpRequestFactory.class);
        AgentBroker broker = new AgentBroker();
        AgentHttpRequestFactory factory = new AgentHttpRequestFactory(fallback, provider(registry), provider(broker),
                provider(mock(AgentIdentityStore.class)), provider(config), new MockEnvironment());
        long started = System.nanoTime();
        for (int i = 0; i < 10; i++) {
            try { factory.createRequest(URI.create("http://10.0.0.2:1685/api/worker/status"), HttpMethod.GET); fail(); }
            catch (java.io.IOException expected) { assertTrue(expected.getMessage().contains("offline")); }
        }
        assertTrue("Repeated refreshes must not wait for RPC deadlines", System.nanoTime() - started < TimeUnit.SECONDS.toNanos(1));
        assertEquals(0, broker.pendingCount()); verifyZeroInteractions(fallback);
    }

    @Test public void agentDisconnectedAfterRequestCreationCannotEnqueueAMutation() throws Exception {
        DistributedConfig config = mock(DistributedConfig.class); when(config.getPort()).thenReturn(1685);
        AgentRegistryService registry = mock(AgentRegistryService.class); when(registry.isAgent("10.0.0.2")).thenReturn(true);
        when(registry.connected("10.0.0.2")).thenReturn(true, false);
        when(registry.online("10.0.0.2")).thenReturn(true);
        ClientHttpRequestFactory fallback = mock(ClientHttpRequestFactory.class);
        AgentBroker broker = new AgentBroker();
        AgentHttpRequestFactory factory = new AgentHttpRequestFactory(fallback, provider(registry), provider(broker),
                provider(mock(AgentIdentityStore.class)), provider(config), new MockEnvironment());
        ClientHttpRequest request = factory.createRequest(URI.create("http://10.0.0.2:1685/api/worker/start"), HttpMethod.POST);
        try { request.execute(); fail(); } catch (java.io.IOException expected) { }
        assertEquals(0, broker.pendingCount()); verifyZeroInteractions(fallback);
    }

    @Test public void unsynchronizedAgentAllowsInspectionStopAndReconcileButCannotStartWork() throws Exception {
        DistributedConfig config = mock(DistributedConfig.class); when(config.getPort()).thenReturn(1685);
        AgentRegistryService registry = mock(AgentRegistryService.class); when(registry.isAgent("10.0.0.2")).thenReturn(true);
        when(registry.connected("10.0.0.2")).thenReturn(true); when(registry.online("10.0.0.2")).thenReturn(false);
        ClientHttpRequestFactory fallback = mock(ClientHttpRequestFactory.class);
        AgentBroker broker = new AgentBroker();
        AgentHttpRequestFactory factory = new AgentHttpRequestFactory(fallback, provider(registry), provider(broker),
                provider(mock(AgentIdentityStore.class)), provider(config), new MockEnvironment());
        for (String path : new String[]{"tasks", "start", "resume", "retry-task", "local-transfers", "local-transfers/discover",
                "local-transfers/batches", "local-transfers/resume", "local-transfers/retry-failures"}) {
            try { factory.createRequest(URI.create("http://10.0.0.2:1685/api/worker/" + path), HttpMethod.POST); fail(path); }
            catch (AgentUnavailableException expected) { assertTrue(expected.getMessage().contains("synchronized")); }
        }
        assertNotNull(factory.createRequest(URI.create("http://10.0.0.2:1685/api/worker/status"), HttpMethod.GET));
        assertNotNull(factory.createRequest(URI.create("http://10.0.0.2:1685/api/worker/stop"), HttpMethod.POST));
        assertNotNull(factory.createRequest(URI.create("http://10.0.0.2:1685/api/worker/s3-config"), HttpMethod.POST));
        assertNotNull(factory.createRequest(URI.create("http://10.0.0.2:1685/api/worker/task-bandwidth-policy"), HttpMethod.PUT));
        assertNotNull(factory.createRequest(URI.create("http://10.0.0.2:1685/api/worker/rate-limit/put"), HttpMethod.POST));
        assertEquals(0, broker.pendingCount()); verifyZeroInteractions(fallback);
    }
}
