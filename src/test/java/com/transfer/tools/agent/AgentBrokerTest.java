package com.transfer.tools.agent;

import org.junit.Test;
import java.io.IOException;
import java.util.Base64;
import java.util.Collections;
import java.util.concurrent.*;
import static org.junit.Assert.*;

public class AgentBrokerTest {
    private static RpcResult result(RpcCommand command) {
        RpcResult result = new RpcResult();
        result.setId(command.getId()); result.setStatus(200);
        result.setBody(Base64.getEncoder().encodeToString("{}".getBytes()));
        result.setHeaders(Collections.singletonMap("Content-Type", "application/json"));
        return result;
    }

    @Test public void responseBelongsOnlyToItsAuthenticatedAgentAndCompletesOnce() throws Exception {
        AgentBroker broker = new AgentBroker();
        ExecutorService executor = Executors.newSingleThreadExecutor();
        try {
            Future<RpcResponse> call = executor.submit(() -> broker.call("10.0.0.2", "GET", "/api/worker/status", null, null, 2000));
            RpcCommand command = broker.poll("10.0.0.2", 1000);
            assertNotNull(command);
            try { broker.complete("10.0.0.3", result(command)); fail("Cross-Agent response must be refused"); }
            catch (SecurityException expected) { }
            assertFalse(call.isDone());
            assertTrue(broker.complete("10.0.0.2", result(command)));
            assertEquals(200, call.get(1, TimeUnit.SECONDS).getStatus());
            assertFalse(broker.complete("10.0.0.2", result(command)));
            assertNull(broker.poll("10.0.0.2", 0));
            assertEquals(0, broker.pendingCount());
        } finally { executor.shutdownNow(); broker.close(); }
    }

    @Test public void expiredCallsAreRemovedAndNeverDeliveredAgain() throws Exception {
        AgentBroker broker = new AgentBroker();
        ExecutorService executor = Executors.newSingleThreadExecutor();
        try {
            Future<RpcResponse> call = executor.submit(() -> broker.call("10.0.0.2", "POST", "/api/worker/start", "{}".getBytes(), null, 100));
            RpcCommand command = broker.poll("10.0.0.2", 1000);
            assertNotNull(command);
            try { call.get(1, TimeUnit.SECONDS); fail("No acknowledgement must time out"); }
            catch (ExecutionException expected) { assertTrue(expected.getCause() instanceof IOException); }
            assertNull(broker.poll("10.0.0.2", 0));
            assertFalse(broker.complete("10.0.0.2", result(command)));
            assertEquals(0, broker.pendingCount());
        } finally { executor.shutdownNow(); broker.close(); }
    }

    @Test public void interruptedMutationHasNoAutomaticReplay() throws Exception {
        AgentBroker broker = new AgentBroker();
        ExecutorService executor = Executors.newSingleThreadExecutor();
        try {
            Future<RpcResponse> call = executor.submit(() -> broker.call("10.0.0.2", "POST", "/api/worker/clear", null, null, 10000));
            assertNotNull(broker.poll("10.0.0.2", 1000));
            call.cancel(true);
            executor.shutdown(); assertTrue(executor.awaitTermination(1, TimeUnit.SECONDS));
            assertEquals(0, broker.pendingCount());
            assertNull(broker.poll("10.0.0.2", 0));
        } finally { executor.shutdownNow(); broker.close(); }
    }

    @Test public void requestAndResponseSizesAreBounded() throws Exception {
        AgentBroker broker = new AgentBroker();
        try { broker.call("10.0.0.2", "POST", "/api/worker/tasks", new byte[AgentRpcPolicy.MAX_BYTES + 1], null, 100); fail(); }
        catch (IllegalArgumentException expected) { }
        assertEquals(0, broker.pendingCount());
        ExecutorService executor = Executors.newSingleThreadExecutor();
        try {
            Future<RpcResponse> call = executor.submit(() -> broker.call("10.0.0.2", "GET", "/api/worker/status", null, null, 2000));
            RpcCommand command = broker.poll("10.0.0.2", 1000);
            RpcResult oversized = result(command);
            oversized.setBody(Base64.getEncoder().encodeToString(new byte[AgentRpcPolicy.MAX_BYTES + 1]));
            try { broker.complete("10.0.0.2", oversized); fail(); }
            catch (IllegalArgumentException expected) { }
            assertFalse(call.isDone());
            assertTrue(broker.complete("10.0.0.2", result(command)));
            call.get(1, TimeUnit.SECONDS);
        } finally { executor.shutdownNow(); broker.close(); }
    }
}
