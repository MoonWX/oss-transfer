package com.transfer.tools.agent;

import org.junit.Test;
import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.util.*;
import static org.junit.Assert.*;

public class AgentRpcPolicyTest {
    @Test public void permitPagedReportsAndTaskControlOnly() {
        AgentRpcPolicy.requireAllowed("GET", "/api/worker/task-reports/224/files?limit=500&cursor=abc%2Fxyz");
        AgentRpcPolicy.requireAllowed("POST", "/api/worker/local-transfers/batches");
        AgentRpcPolicy.requireAllowed("PUT", "/api/worker/task-bandwidth-policy?scopeId=local%3A1");
    }

    @Test public void rejectArbitraryUrlsTraversalAndNonWorkerApis() {
        for (String path : Arrays.asList("https://example.com/api/worker/status", "//example.com/api/worker/status",
                "/api/master/nodes", "/api/worker/../auth/login", "/api/worker/%2e%2e/auth/login",
                "/api/worker/status#fragment", "/api/worker/local-transfers/report", "/api/worker/shell",
                "/api/worker/status\nX-Test: yes")) {
            try { AgentRpcPolicy.requireAllowed("GET", path); fail(path); }
            catch (IllegalArgumentException expected) { }
        }
        try { AgentRpcPolicy.requireAllowed("DELETE", "/api/worker/tasks"); fail(); }
        catch (IllegalArgumentException expected) { }
    }

    @Test public void doNotForwardTokensCookiesOrUntrustedHeaders() {
        Map<String, String> headers = new LinkedHashMap<>();
        headers.put("Content-Type", "application/json"); headers.put("Set-Cookie", "private=value");
        headers.put("X-Internal-Token", "private"); headers.put("Cache-Control", "no-store\r\nX-Evil: yes");
        assertEquals(Collections.singletonMap("Content-Type", "application/json"), AgentRpcPolicy.responseHeaders(headers));
    }

    @Test public void decodeAndStreamAreBounded() throws Exception {
        assertArrayEquals(new byte[0], AgentRpcPolicy.decode(null));
        try { AgentRpcPolicy.decode("not-base64*"); fail(); } catch (IllegalArgumentException expected) { }
        try { AgentRpcPolicy.readBounded(new ByteArrayInputStream(new byte[AgentRpcPolicy.MAX_BYTES + 1])); fail(); }
        catch (IOException expected) { }
    }
}
