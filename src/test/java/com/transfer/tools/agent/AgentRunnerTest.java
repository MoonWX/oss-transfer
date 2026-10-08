package com.transfer.tools.agent;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpServer;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.service.LocalTransferService;
import org.junit.Test;
import org.springframework.beans.factory.ObjectProvider;
import org.springframework.mock.env.MockEnvironment;
import java.net.InetSocketAddress;
import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class AgentRunnerTest {
    @Test @SuppressWarnings("unchecked") public void executesOnlyLoopbackWithLocalTokenAndReturnsOutboundResult() throws Exception {
        ObjectMapper mapper = new ObjectMapper();
        HttpServer worker = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        HttpServer master = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        ExecutorService serverThreads = Executors.newCachedThreadPool();
        master.setExecutor(serverThreads);
        AtomicReference<String> localToken = new AtomicReference<>();
        worker.createContext("/api/worker/status", exchange -> {
            localToken.set(exchange.getRequestHeaders().getFirst("X-Internal-Token"));
            byte[] body = "{\"success\":true}".getBytes("UTF-8");
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, body.length); exchange.getResponseBody().write(body); exchange.close();
        });
        AtomicBoolean sent = new AtomicBoolean();
        AtomicBoolean ready = new AtomicBoolean();
        AtomicReference<String> pollKey = new AtomicReference<>();
        master.createContext("/api/agents/poll", exchange -> {
            pollKey.set(exchange.getRequestHeaders().getFirst("X-Agent-Key"));
            exchange.getResponseHeaders().set("X-Agent-Ready", Boolean.toString(ready.get()));
            if (sent.compareAndSet(false, true)) {
                RpcCommand command = new RpcCommand(); command.setId("command-1"); command.setMethod("GET");
                command.setPath("/api/worker/status"); command.setDeadline(System.currentTimeMillis() + 5000);
                byte[] body = mapper.writeValueAsBytes(command);
                exchange.sendResponseHeaders(200, body.length); exchange.getResponseBody().write(body);
            } else {
                try { Thread.sleep(100); } catch (InterruptedException interrupted) { Thread.currentThread().interrupt(); }
                exchange.sendResponseHeaders(204, -1);
            }
            exchange.close();
        });
        CountDownLatch returned = new CountDownLatch(1);
        AtomicReference<RpcResult> result = new AtomicReference<>();
        master.createContext("/api/agents/result", exchange -> {
            result.set(mapper.readValue(exchange.getRequestBody(), RpcResult.class));
            exchange.sendResponseHeaders(200, -1); exchange.close(); returned.countDown();
        });
        AgentIdentityStore identity = mock(AgentIdentityStore.class);
        when(identity.isReady()).thenReturn(true); when(identity.getCredential()).thenReturn("per-agent-key");
        when(identity.getMasterUrl()).thenReturn("http://127.0.0.1:" + master.getAddress().getPort());
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getPort()).thenReturn(worker.getAddress().getPort()); when(config.getInternalToken()).thenReturn("local-only-key");
        LocalTransferService recovery = mock(LocalTransferService.class);
        ObjectProvider<LocalTransferService> provider = mock(ObjectProvider.class); when(provider.getObject()).thenReturn(recovery);
        AgentRunner runner = new AgentRunner(identity, config, new MockEnvironment().withProperty("transfer.role", "agent"), mapper, provider);
        try {
            worker.start(); master.start(); runner.start();
            assertTrue(returned.await(3, TimeUnit.SECONDS));
            assertEquals("per-agent-key", pollKey.get()); assertEquals("local-only-key", localToken.get());
            assertEquals(200, result.get().getStatus()); assertEquals("command-1", result.get().getId());
            assertEquals("{\"success\":true}", new String(Base64.getDecoder().decode(result.get().getBody()), "UTF-8"));
            verify(recovery, never()).recoverDiscoveries();
            ready.set(true);
            verify(recovery, timeout(1000).times(1)).recoverDiscoveries();
        } finally { runner.stop(); master.stop(0); worker.stop(0); serverThreads.shutdownNow(); }
    }
}
