package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.ListSubTask;
import com.transfer.tools.model.LocalNodeBinding;
import com.transfer.tools.model.TransferDirection;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;

import java.util.Collections;

import static org.mockito.Mockito.*;
import static org.junit.Assert.assertEquals;
import static org.junit.Assert.fail;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.content;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo;
import static org.springframework.test.web.client.response.MockRestResponseCreators.withSuccess;

public class DistributedServiceLocalIdTest {

    @Test public void preparedShardKeepsPersistedNodeEvenWhenSchedulerWouldChooseAnother() {
        DistributedService service = new DistributedService();
        BalancedWorkerScheduler scheduler = mock(BalancedWorkerScheduler.class);
        ReflectionTestUtils.setField(service, "balancedWorkerScheduler", scheduler);
        ListSubTask task = new ListSubTask(); task.setExecutionNodeIp("worker-a"); task.setLocalPath("/a");
        task.setParentTaskId(12);
        service.prepareLocalAssignments(Collections.singletonList(task), "balanced", java.util.Arrays.asList("worker-a", "worker-b"),
                java.util.Arrays.asList(new LocalNodeBinding("worker-a", "/a"), new LocalNodeBinding("worker-b", "/b")));
        assertEquals("worker-a", task.getExecutionNodeIp()); assertEquals("/a", task.getLocalPath());
        verifyZeroInteractions(scheduler);
    }

    @Test
    public void localShardUsesNegativeStableIdWithoutConsumingPublicSequence() {
        DistributedService service = new DistributedService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getPort()).thenReturn(8081);
        when(config.getInternalToken()).thenReturn("");
        SubTaskManagerService taskManager = mock(SubTaskManagerService.class);
        when(taskManager.preferredLocalShardTaskId(TransferDirection.OSS_TO_LOCAL, 86, "86-oss-0"))
                .thenReturn(-12345);
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "subTaskManagerService", taskManager);

        RestTemplate restTemplate = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(restTemplate).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/tasks"))
                .andExpect(content().string(org.hamcrest.Matchers.containsString("\"taskIndex\":-12345")))
                .andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));

        ListSubTask task = new ListSubTask();
        task.setDirection(TransferDirection.OSS_TO_LOCAL);
        task.setParentTaskId(86);
        task.setShardId("86-oss-0");
        task.setSourceBucket("source");
        task.setExecutionNodeIp("127.0.0.1");
        service.distributeTasks(Collections.singletonList(task), DistributedService.MODE_BALANCED,
                Collections.singletonList("127.0.0.1"),
                Collections.singletonList(new LocalNodeBinding("127.0.0.1", "/data")));

        verify(taskManager, never()).allocateTaskIdRange(anyInt());
        server.verify();
    }

    @Test
    public void ossToOssStillUsesPositivePublicSequence() {
        DistributedService service = new DistributedService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getPort()).thenReturn(8081);
        when(config.getInternalToken()).thenReturn("");
        SubTaskManagerService taskManager = mock(SubTaskManagerService.class);
        when(taskManager.allocateTaskIdRange(1)).thenReturn(41);
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "subTaskManagerService", taskManager);

        RestTemplate restTemplate = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(restTemplate).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/tasks"))
                .andExpect(content().string(org.hamcrest.Matchers.containsString("\"taskIndex\":41")))
                .andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));

        ListSubTask task = new ListSubTask();
        task.setDirection(TransferDirection.OSS_TO_OSS);
        task.setSourceBucket("source");
        task.setDestBucket("destination");
        service.distributeTasks(Collections.singletonList(task), DistributedService.MODE_BALANCED,
                Collections.singletonList("127.0.0.1"), null);

        verify(taskManager).allocateTaskIdRange(1);
        server.verify();
    }

    @Test
    public void preservesWorkerValidationCodeWhenShardIsRejected() {
        DistributedService service = new DistributedService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getPort()).thenReturn(8081);
        when(config.getInternalToken()).thenReturn("");
        SubTaskManagerService taskManager = mock(SubTaskManagerService.class);
        when(taskManager.preferredLocalShardTaskId(TransferDirection.OSS_TO_LOCAL, 86, "86-oss-0"))
                .thenReturn(-12345);
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "subTaskManagerService", taskManager);

        RestTemplate restTemplate = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(restTemplate).build();
        server.expect(requestTo("http://127.0.0.1:8081/api/worker/tasks"))
                .andRespond(withSuccess("{\"success\":false,\"code\":\"LOCAL_PATH_NOT_WRITABLE\","
                        + "\"message\":\"destination is read-only\"}", MediaType.APPLICATION_JSON));

        ListSubTask task = new ListSubTask();
        task.setDirection(TransferDirection.OSS_TO_LOCAL);
        task.setParentTaskId(86);
        task.setShardId("86-oss-0");
        task.setSourceBucket("source");
        task.setExecutionNodeIp("127.0.0.1");
        try {
            service.distributeTasks(Collections.singletonList(task), DistributedService.MODE_BALANCED,
                    Collections.singletonList("127.0.0.1"),
                    Collections.singletonList(new LocalNodeBinding("127.0.0.1", "/data")));
            fail("expected worker rejection");
        } catch (DownstreamWorkerException error) {
            assertEquals("LOCAL_PATH_NOT_WRITABLE", error.getCode());
            assertEquals("destination is read-only", error.getMessage());
        }
        server.verify();
    }
}
