package com.transfer.tools.controller;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.service.S3ClientManager;
import org.junit.Before;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.Collections;
import java.util.HashMap;
import java.util.Map;
import java.util.Arrays;
import java.util.List;
import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class S3ConfigMutationTest {
    private S3ClientManager manager;
    private DistributedConfig config;
    private MasterController master;
    private WorkerController worker;

    @Before public void setup() {
        manager = mock(S3ClientManager.class); config = mock(DistributedConfig.class);
        master = new MasterController(); worker = new WorkerController();
        ReflectionTestUtils.setField(master, "s3ClientManager", manager);
        ReflectionTestUtils.setField(master, "distributedConfig", config);
        ReflectionTestUtils.setField(master, "nodeConfigService", new com.transfer.tools.service.NodeConfigService());
        ReflectionTestUtils.setField(master, "nodeRegistryService", new com.transfer.tools.service.NodeRegistryService());
        ReflectionTestUtils.setField(worker, "s3ClientManager", manager);
        ReflectionTestUtils.setField(worker, "nodeConfigService", new com.transfer.tools.service.NodeConfigService());
        when(config.getLocalIp()).thenReturn("qa-node");
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("qa-node"));
    }

    @Test public void masterDuplicatePostIsRejectedBeforeAnyWorkerSync() {
        when(manager.addClient(anyString(), anyString(), anyString(), anyString(), anyBoolean(), anyBoolean(), anyBoolean()))
                .thenThrow(new IllegalArgumentException("S3配置已存在，请使用更新接口: test"));
        Map<String, Object> result = master.addS3Config(params());
        assertEquals(false, result.get("success")); assertTrue(result.get("message").toString().contains("已存在"));
        verify(config, never()).getWorkerIps();
        verify(manager, never()).updateClient(anyString(), anyString(), anyString(), anyString(), anyBoolean(), anyBoolean(), anyBoolean());
    }

    @Test public void workerDuplicatePostIsRejectedInsteadOfUpdating() {
        when(manager.addClient(anyString(), anyString(), anyString(), anyString(), anyBoolean(), anyBoolean(), anyBoolean()))
                .thenThrow(new IllegalArgumentException("S3配置已存在，请使用更新接口: test"));
        Map<String, Object> result = worker.addS3Config(params());
        assertEquals(false, result.get("success")); assertTrue(result.get("message").toString().contains("已存在"));
        verify(manager, never()).updateClient(anyString(), anyString(), anyString(), anyString(), anyBoolean(), anyBoolean(), anyBoolean());
    }

    @Test public void newWorkerAndMasterConnectionsStillUseCreate() {
        assertEquals(true, worker.addS3Config(params()).get("success"));
        assertEquals(true, master.addS3Config(params()).get("success"));
        verify(manager, times(2)).addClient("test", "http://qa.example", "fake", "fake", false, true, true);
        verify(manager, never()).updateClient(anyString(), anyString(), anyString(), anyString(), anyBoolean(), anyBoolean(), anyBoolean());
    }

    @Test public void editsUseAtomicUpdateWithoutRejectingPersistedButUnloadedClients() {
        // A failed startup can leave a durable configuration without a live client.
        when(manager.containsClient("test")).thenReturn(false);
        assertEquals(true, worker.updateS3Config("test", params()).get("success"));
        assertEquals(true, master.updateS3Config("test", params()).get("success"));
        verify(manager, times(2)).updateClient("test", "http://qa.example", "fake", "fake", false, true, true);
        verify(manager, never()).addClient(anyString(), anyString(), anyString(), anyString(), anyBoolean(), anyBoolean(), anyBoolean());
        verify(manager, never()).containsClient(anyString());
    }

    @Test public void everyOperationNamesAndCountsLocalAndRemoteOutcomes() throws Exception {
        final String[] acknowledgment = {"{\"success\" : true}"};
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/api/worker/s3-config", exchange -> {
            while (exchange.getRequestBody().read() != -1) { }
            byte[] bytes = acknowledgment[0].getBytes(StandardCharsets.UTF_8);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, bytes.length);
            exchange.getResponseBody().write(bytes); exchange.close();
        });
        server.start();
        try {
            when(config.getPort()).thenReturn(server.getAddress().getPort());
            when(config.getWorkerIps()).thenReturn(Arrays.asList("qa-node", "127.0.0.1"));
            when(manager.removeClient("test")).thenReturn(true);
            for (boolean failed : new boolean[]{false, true}) {
                acknowledgment[0] = failed ? "{\"success\":false}" : "{\"success\" : true}";
                List<Map<String, Object>> results = Arrays.asList(master.addS3Config(params()),
                        master.updateS3Config("test", params()), master.removeS3Config("test"));
                String[] operations = {"添加", "更新", "删除"};
                for (int i = 0; i < results.size(); i++) {
                    Map<String, Object> result = results.get(i);
                    String operation = operations[i];
                    assertEquals(true, result.get("success"));
                    assertEquals(failed ? 1 : 2, result.get("successCount"));
                    assertEquals(failed ? 1 : 0, result.get("failedCount"));
                    assertEquals(failed, result.get("partial"));
                    List<?> details = (List<?>) result.get("details");
                    assertEquals(2, details.size());
                    assertEquals("qa-node: " + operation + "成功", details.get(0));
                    assertEquals("127.0.0.1: " + operation + (failed ? "失败" : "成功"), details.get(1));
                    assertEquals(operation + "结果：成功 " + (failed ? 1 : 2) + " 个节点，失败 "
                            + (failed ? 1 : 0) + " 个节点", result.get("message"));
                }
            }
        } finally { server.stop(0); }
    }

    private Map<String, Object> params() {
        Map<String, Object> values = new HashMap<>();
        values.put("alias", "test"); values.put("endpoint", "http://qa.example");
        values.put("accessKey", "fake"); values.put("secretKey", "fake");
        values.put("useV2SignerType", false); values.put("pathStyleEnable", true); values.put("validate", true);
        return values;
    }
}
