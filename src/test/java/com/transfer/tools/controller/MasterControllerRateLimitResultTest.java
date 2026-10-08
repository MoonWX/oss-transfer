package com.transfer.tools.controller;

import com.sun.net.httpserver.HttpServer;
import com.transfer.tools.config.DistributedConfig;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;

import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.Collections;
import java.util.concurrent.atomic.AtomicInteger;

import static org.hamcrest.Matchers.containsString;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.get;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;
import static org.junit.Assert.assertEquals;

public class MasterControllerRateLimitResultTest {
    private HttpServer worker;
    private DistributedConfig config;
    private MockMvc mvc;
    private String responseJson;
    private int responseStatus;
    private String lastRawQuery;
    private final AtomicInteger workerCalls = new AtomicInteger();

    @Before
    public void setUp() throws Exception {
        responseJson = "{\"success\":true,\"message\":\"applied\"}";
        responseStatus = 200;
        worker = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        worker.createContext("/api/worker/", exchange -> {
            workerCalls.incrementAndGet();
            lastRawQuery = exchange.getRequestURI().getRawQuery();
            while (exchange.getRequestBody().read() != -1) { }
            byte[] response = responseJson.getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(responseStatus, response.length == 0 ? -1 : response.length);
            if (response.length > 0) exchange.getResponseBody().write(response);
            exchange.close();
        });
        worker.start();
        config = mock(DistributedConfig.class);
        when(config.getPort()).thenReturn(worker.getAddress().getPort());
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("127.0.0.1"));
        MasterController controller = new MasterController();
        ReflectionTestUtils.setField(controller, "distributedConfig", config);
        mvc = MockMvcBuilders.standaloneSetup(controller).setUseSuffixPatternMatch(false).build();
    }

    @After
    public void tearDown() {
        if (worker != null) worker.stop(0);
    }

    @Test
    public void globalUpdateRetainsAppliedNodeAndReportsOfflineNode() throws Exception {
        when(config.getWorkerIps()).thenReturn(Arrays.asList("127.0.0.1", "127.0.0.2"));
        submit("").andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.partial").value(true))
                .andExpect(jsonPath("$.successfulWorkers[0]").value("127.0.0.1"))
                .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.2"))
                .andExpect(jsonPath("$.message", containsString("已成功应用节点：127.0.0.1")))
                .andExpect(jsonPath("$.message", containsString("失败节点：127.0.0.2")));
        assertEquals(1, workerCalls.get());
    }

    @Test
    public void globalUpdateReportsWorkerRejection() throws Exception {
        responseJson = "{\"success\":false,\"message\":\"rate rejected\"}";
        submit("").andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.successfulWorkers").isEmpty())
                .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.1"))
                .andExpect(jsonPath("$.message", containsString("rate rejected")));
    }

    @Test
    public void globalUpdateReportsAllAcknowledgedNodes() throws Exception {
        submit("").andExpect(jsonPath("$.success").value(true))
                .andExpect(jsonPath("$.partial").value(false))
                .andExpect(jsonPath("$.successfulWorkers[0]").value("127.0.0.1"))
                .andExpect(jsonPath("$.failedWorkers").isEmpty());
    }

    @Test
    public void emptyInvalidOrHttpErrorResponsesCannotBeAcknowledged() throws Exception {
        for (String suffix : new String[]{"", "/127.0.0.1"}) {
            for (String body : new String[]{"", "null", "{}", "not-json", "[]"}) {
                responseJson = body;
                submit(suffix).andExpect(jsonPath("$.success").value(false))
                        .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.1"));
            }
            responseJson = "{\"success\":true}";
            responseStatus = 503;
            submit(suffix).andExpect(jsonPath("$.success").value(false))
                    .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.1"));
            responseStatus = 200;
        }
    }

    @Test
    public void singleNodeUpdateIdentifiesAnOfflineNode() throws Exception {
        submit("/127.0.0.2").andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.successfulWorkers").isEmpty())
                .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.2"))
                .andExpect(jsonPath("$.message", containsString("127.0.0.2")));
        assertEquals(0, workerCalls.get());
    }

    @Test
    public void singleNodeUpdatePreservesWorkerRejectionDetails() throws Exception {
        responseJson = "{\"success\":false,\"message\":\"rate rejected\"}";
        submit("/127.0.0.1").andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.1"))
                .andExpect(jsonPath("$.message", containsString("rate rejected")));
    }

    @Test
    public void validNonemptyLegacyAcknowledgmentIsStillAccepted() throws Exception {
        responseJson = "{\"message\":\"applied\"}";
        for (String suffix : new String[]{"", "/127.0.0.1"}) {
            submit(suffix).andExpect(jsonPath("$.success").value(true))
                    .andExpect(jsonPath("$.successfulWorkers[0]").value("127.0.0.1"));
        }
    }

    @Test
    public void rateStatusPassesThroughPolicyAndReportsMissingNodes() throws Exception {
        responseJson = "{\"success\":true,\"rateLimit\":{\"put\":{\"limitQps\":500,\"manualLimitQps\":1000,\"state\":\"REDUCED\"},\"autoAdjust\":{\"processId\":\"test-process\",\"sequence\":2}}}";
        when(config.getWorkerIps()).thenReturn(Arrays.asList("127.0.0.1", "127.0.0.2"));
        mvc.perform(get("/api/master/rate-limit")).andExpect(status().isOk())
                .andExpect(jsonPath("$.success").value(true))
                .andExpect(jsonPath("$.complete").value(false))
                .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.2"))
                .andExpect(jsonPath("$.rateLimits[0].put.manualLimitQps").value(1000))
                .andExpect(jsonPath("$.rateLimits[0].autoAdjust.processId").value("test-process"));
    }

    @Test
    public void historyKeepsReceivedReasonsAndCannotClaimAnOfflineNodeHasNoHistory() throws Exception {
        responseJson = "{\"success\":true,\"adjustments\":[{\"type\":\"put\",\"value\":500,\"reasonCode\":\"OSS_503\",\"previousValue\":1000,\"timestamp\":1000}]}";
        when(config.getWorkerIps()).thenReturn(Arrays.asList("127.0.0.1", "127.0.0.2"));
        mvc.perform(get("/api/master/rate-limit/history")).andExpect(status().isOk())
                .andExpect(jsonPath("$.success").value(true))
                .andExpect(jsonPath("$.complete").value(false))
                .andExpect(jsonPath("$.partial").value(true))
                .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.2"))
                .andExpect(jsonPath("$.adjustments[0].reasonCode").value("OSS_503"))
                .andExpect(jsonPath("$.adjustments[0].workerIp").value("127.0.0.1"));
    }

    @Test
    public void invalidHistoryAndMissingWorkerStatusAreExplicitFailures() throws Exception {
        responseJson = "null";
        mvc.perform(get("/api/master/rate-limit/history")).andExpect(status().isOk())
                .andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.complete").value(false))
                .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.1"));
        for (String invalid : new String[]{"null", "{}", "{\"success\":false}", "{\"queueStatus\":{}}"}) {
            responseJson = invalid;
            mvc.perform(get("/api/master/worker-status")).andExpect(status().isOk())
                    .andExpect(jsonPath("$.workers['127.0.0.1'].success").value(false));
        }
    }

    @Test
    public void adjustmentDetailProxyPreservesEvidenceAndEncodesQueryOnce() throws Exception {
        responseJson = "{\"success\":true,\"event\":{\"id\":\"00000000-0000-0000-0000-000000000000:1\",\"triggerRequest\":{\"statusCode\":503,\"requestId\":\"request-1\"}},\"events\":[],\"done\":true}";
        mvc.perform(get("/api/master/rate-limit/adjustment").param("workerIp", "127.0.0.1")
                        .param("eventId", "00000000-0000-0000-0000-000000000000:1").param("cursor", "a+b/=x"))
                .andExpect(status().isOk()).andExpect(jsonPath("$.success").value(true))
                .andExpect(jsonPath("$.workerIp").value("127.0.0.1"))
                .andExpect(jsonPath("$.event.triggerRequest.requestId").value("request-1"));
        assertEquals("eventId=00000000-0000-0000-0000-000000000000%3A1&cursor=a%2Bb%2F%3Dx", lastRawQuery);
    }

    @Test
    public void adjustmentDetailProxyRejectsUnconfiguredNodesAndMissingRecords() throws Exception {
        mvc.perform(get("/api/master/rate-limit/adjustment").param("workerIp", "169.254.169.254")
                        .param("eventId", "00000000-0000-0000-0000-000000000000:1"))
                .andExpect(status().isOk()).andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.message").value("节点未配置"));
        assertEquals(0, workerCalls.get());
        responseJson = "{\"success\":false,\"message\":\"旧记录未保存详情\"}";
        mvc.perform(get("/api/master/rate-limit/adjustment").param("workerIp", "127.0.0.1")
                        .param("eventId", "00000000-0000-0000-0000-000000000000:1"))
                .andExpect(status().isOk()).andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.message").value("旧记录未保存详情"));
    }

    private ResultActions submit(String workerSuffix) throws Exception {
        return mvc.perform(post("/api/master/rate-limit/put" + workerSuffix)
                        .contentType(MediaType.APPLICATION_JSON).content("{\"qps\":1400}"))
                .andExpect(status().isOk());
    }
}
