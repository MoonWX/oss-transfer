package com.transfer.tools.controller;

import com.amazonaws.AmazonClientException;
import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.AmazonS3Exception;
import com.amazonaws.services.s3.model.ObjectMetadata;
import com.sun.net.httpserver.HttpServer;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.service.S3ClientManager;
import org.junit.After;
import org.junit.Before;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;

import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.atomic.AtomicInteger;

import static org.hamcrest.Matchers.containsString;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.*;

public class MasterFailureAndMetadataTest {
    private HttpServer worker;
    private MasterController controller;
    private DistributedConfig config;
    private MockMvc mvc;
    private String response = "{\"success\":true,\"failedKeys\":[],\"count\":0}";
    private final Map<String, String> routes = new HashMap<>();
    private final AtomicInteger retryCalls = new AtomicInteger();

    @Before public void setup() throws Exception {
        worker = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        worker.createContext("/api/worker/", exchange -> {
            while (exchange.getRequestBody().read() != -1) { }
            String path = exchange.getRequestURI().getPath();
            if (path.endsWith("/retry-task")) retryCalls.incrementAndGet();
            byte[] body = routes.getOrDefault(path, response).getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, body.length == 0 ? -1 : body.length);
            if (body.length > 0) exchange.getResponseBody().write(body);
            exchange.close();
        });
        worker.start();
        config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("127.0.0.1"));
        when(config.getPort()).thenReturn(worker.getAddress().getPort());
        controller = new MasterController();
        ReflectionTestUtils.setField(controller, "distributedConfig", config);
        mvc = MockMvcBuilders.standaloneSetup(controller).build();
    }

    @After public void close() { worker.stop(0); }

    @Test public void failureQueryRetainsAvailableRowsAndReportsUnavailableNodes() throws Exception {
        when(config.getWorkerIps()).thenReturn(Arrays.asList("127.0.0.1", "127.0.0.2"));
        response = "{\"success\":true,\"failedKeys\":[{\"key\":\"failed-file\"}],\"count\":1}";
        mvc.perform(post("/api/master/failed").contentType(MediaType.APPLICATION_JSON).content("{}"))
                .andExpect(jsonPath("$.success").value(true)).andExpect(jsonPath("$.partial").value(true))
                .andExpect(jsonPath("$.complete").value(false))
                .andExpect(jsonPath("$.successfulWorkers[0]").value("127.0.0.1"))
                .andExpect(jsonPath("$.unreachableWorkers[0]").value("127.0.0.2"))
                .andExpect(jsonPath("$.failedKeys['127.0.0.1'][0].key").value("failed-file"))
                .andExpect(jsonPath("$.failedKeys['127.0.0.2']").doesNotExist());
    }

    @Test public void missingMalformedAndRejectedFailureResponsesAreNeverAnEmptySuccess() throws Exception {
        for (String invalid : new String[]{"", "null", "{}", "{\"success\":true}", "{\"success\":false,\"message\":\"read failed\"}"}) {
            response = invalid;
            mvc.perform(post("/api/master/failed")).andExpect(jsonPath("$.success").value(false))
                    .andExpect(jsonPath("$.complete").value(false))
                    .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.1"));
        }
    }

    @Test public void clearCannotClaimSuccessAfterOfflineOrRejectedNode() throws Exception {
        response = "{\"success\":true}";
        when(config.getWorkerIps()).thenReturn(Arrays.asList("127.0.0.1", "127.0.0.2"));
        mvc.perform(post("/api/master/failed/clear")).andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.partial").value(true))
                .andExpect(jsonPath("$.message", containsString("127.0.0.2")));
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("127.0.0.1"));
        response = "{\"success\":false,\"message\":\"disk write failed\"}";
        mvc.perform(post("/api/master/failed/clear")).andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.message", containsString("disk write failed")));
        response = "{\"success\":true}";
        mvc.perform(post("/api/master/failed/clear")).andExpect(jsonPath("$.success").value(true));
    }

    @Test public void retryPrecheckCannotSilentlyDropOfflineNodeOrUnknownTask() throws Exception {
        routes.put("/api/worker/task-status", "{\"success\":true,\"taskStatuses\":{\"1\":{\"status\":\"PARTIAL_FAILED\"}}}");
        when(config.getWorkerIps()).thenReturn(Arrays.asList("127.0.0.1", "127.0.0.2"));
        mvc.perform(post("/api/master/retry-task").contentType(MediaType.APPLICATION_JSON).content("{\"taskIds\":[1]}"))
                .andExpect(jsonPath("$.success").value(false)).andExpect(jsonPath("$.accepted").value(0))
                .andExpect(jsonPath("$.failedWorkers[0]").value("127.0.0.2"));
        assertEquals(0, retryCalls.get());
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("127.0.0.1"));
        mvc.perform(post("/api/master/retry-task").contentType(MediaType.APPLICATION_JSON).content("{\"taskIds\":[2]}"))
                .andExpect(jsonPath("$.success").value(false)).andExpect(jsonPath("$.message", containsString("不存在")));
        assertEquals(0, retryCalls.get());
    }

    @Test public void retryAggregatesActualPartialAcceptanceAndDeduplicatesIds() throws Exception {
        routes.put("/api/worker/task-status", "{\"success\":true,\"taskStatuses\":{\"1\":{\"status\":\"FAILED\"},\"2\":{\"status\":\"PARTIAL_FAILED\"}}}");
        routes.put("/api/worker/retry-task", "{\"success\":false,\"accepted\":1,\"failed\":1,\"ignored\":0,\"message\":\"queue unavailable\"}");
        mvc.perform(post("/api/master/retry-task").contentType(MediaType.APPLICATION_JSON).content("{\"taskIds\":[1,1,2]}"))
                .andExpect(jsonPath("$.success").value(false)).andExpect(jsonPath("$.partial").value(true))
                .andExpect(jsonPath("$.accepted").value(1)).andExpect(jsonPath("$.failed").value(1))
                .andExpect(jsonPath("$.message", containsString("queue unavailable")));
        assertEquals(1, retryCalls.get());
    }

    @Test public void retryWithoutSelectionRequiresACompleteFailedTaskQuery() throws Exception {
        when(config.getWorkerIps()).thenReturn(Arrays.asList("127.0.0.1", "127.0.0.2"));
        routes.put("/api/worker/failed-tasks", "{\"success\":true,\"failedTaskIds\":[]}");
        mvc.perform(post("/api/master/retry-task").contentType(MediaType.APPLICATION_JSON).content("{}"))
                .andExpect(jsonPath("$.success").value(false)).andExpect(jsonPath("$.accepted").value(0));
        assertEquals(0, retryCalls.get());
    }

    @Test public void runningTaskIsRejectedBeforeDispatchAndEmptyAcknowledgmentIsNotSuccess() throws Exception {
        routes.put("/api/worker/task-status", "{\"success\":true,\"taskStatuses\":{\"1\":{\"status\":\"RUNNING\"}}}");
        mvc.perform(post("/api/master/retry-task").contentType(MediaType.APPLICATION_JSON).content("{\"taskIds\":[1]}"))
                .andExpect(jsonPath("$.success").value(false)).andExpect(jsonPath("$.accepted").value(0));
        assertEquals(0, retryCalls.get());
        routes.put("/api/worker/task-status", "{\"success\":true,\"taskStatuses\":{\"1\":{\"status\":\"FAILED\"}}}");
        routes.put("/api/worker/retry-task", "{\"success\":true}");
        mvc.perform(post("/api/master/retry-task").contentType(MediaType.APPLICATION_JSON).content("{\"taskIds\":[1]}"))
                .andExpect(jsonPath("$.success").value(false)).andExpect(jsonPath("$.accepted").value(0))
                .andExpect(jsonPath("$.failed").value(1));
    }

    @Test public void forbiddenMetadataIsUnknownAndCannotBecomeObjectMissing() {
        AmazonS3 source = sourceClient();
        AmazonS3Exception denied = new AmazonS3Exception("access denied"); denied.setStatusCode(403); denied.setErrorCode("AccessDenied");
        when(source.getObjectMetadata("source", "file")).thenThrow(denied);
        Map<String, Object> result = controller.getObjectMetadata("alias/source", "file", "alias/destination", "file");
        assertEquals(true, result.get("success")); assertEquals(true, result.get("partial"));
        Map metadata = (Map) result.get("sourceMetadata");
        assertNull(metadata.get("exists")); assertEquals("UNAVAILABLE", metadata.get("queryStatus"));
        assertEquals(403, metadata.get("statusCode")); assertEquals("AccessDenied", metadata.get("errorCode"));
        Map verification = (Map) result.get("verification");
        assertNull(verification.get("sizeMatch")); assertFalse(verification.get("message").toString().contains("不存在"));
    }

    @Test public void metadataRequiresExplicitConnectionNamesInsteadOfAnUnavailableDefault() {
        AmazonS3 client = sourceClient();
        for (String value : new String[]{"bucket", "/bucket", "alias/", "alias/bucket/path", " "}) {
            Map<String, Object> result = controller.getObjectMetadata(value, "file", "alias/destination", "file");
            assertEquals(false, result.get("success")); assertTrue(result.get("message").toString().contains("连接名称/Bucket"));
            result = controller.getObjectMetadata("alias/source", "file", value, "file");
            assertEquals(false, result.get("success"));
        }
        verify(client, never()).getObjectMetadata(anyString(), anyString());
    }

    @Test public void onlyNotFoundResponsesConfirmMissingObjectsAndAllNetworkFailuresFailRequest() {
        AmazonS3 client = sourceClient();
        AmazonS3Exception missing = new AmazonS3Exception("not found"); missing.setStatusCode(404);
        when(client.getObjectMetadata("source", "file")).thenThrow(missing);
        Map<String, Object> result = controller.getObjectMetadata("alias/source", "file", "alias/destination", "file");
        assertEquals(false, ((Map) result.get("sourceMetadata")).get("exists"));
        assertEquals("NOT_FOUND", ((Map) result.get("sourceMetadata")).get("queryStatus"));
        assertEquals(true, result.get("complete"));
        doThrow(new AmazonClientException("network unavailable")).when(client).getObjectMetadata("source", "file");
        when(client.getObjectMetadata("destination", "file")).thenThrow(new AmazonClientException("network unavailable"));
        result = controller.getObjectMetadata("alias/source", "file", "alias/destination", "file");
        assertEquals(false, result.get("success")); assertEquals(false, result.get("complete"));
        assertNull(((Map) result.get("sourceMetadata")).get("exists"));
        assertNull(((Map) result.get("destMetadata")).get("exists"));
    }

    private AmazonS3 sourceClient() {
        S3ClientManager clients = mock(S3ClientManager.class);
        AmazonS3 client = mock(AmazonS3.class);
        when(clients.getClient("alias")).thenReturn(client);
        ReflectionTestUtils.setField(controller, "s3ClientManager", clients);
        ObjectMetadata metadata = new ObjectMetadata(); metadata.setContentLength(8L);
        when(client.getObjectMetadata("source", "file")).thenReturn(metadata);
        when(client.getObjectMetadata("destination", "file")).thenReturn(metadata);
        return client;
    }
}
