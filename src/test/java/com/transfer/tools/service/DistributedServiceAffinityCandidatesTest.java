package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.ListSubTask;
import com.transfer.tools.model.LocalNodeBinding;
import com.transfer.tools.model.TransferDirection;
import org.junit.Before;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.List;

import static org.junit.Assert.assertEquals;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.jsonPath;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo;
import static org.springframework.test.web.client.response.MockRestResponseCreators.withSuccess;

public class DistributedServiceAffinityCandidatesTest {
    private DistributedService service;
    private MockRestServiceServer server;

    @Before
    public void setUp() {
        service = new DistributedService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getPort()).thenReturn(8081);
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "subTaskManagerService", mock(SubTaskManagerService.class));
        RestTemplate restTemplate = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        server = MockRestServiceServer.bindTo(restTemplate).build();
    }

    @Test
    public void affinityDispatchCanShrinkFromTwoCandidatesToOne() {
        expectDispatch(88, "10.0.0.1");
        expectDispatch(89, "10.0.0.1");

        dispatch(88, "10.0.0.1", "10.0.0.2");
        ListSubTask task = dispatch(89, "10.0.0.1");

        assertEquals("10.0.0.1", task.getExecutionNodeIp());
        server.verify();
    }

    @Test
    public void cachedAffinityIsReassignedWhenItsWorkerIsNoLongerEligible() {
        expectDispatch(88, "10.0.0.1");
        expectDispatch(88, "10.0.0.2");

        dispatch(88, "10.0.0.1", "10.0.0.2");
        ListSubTask task = dispatch(88, "10.0.0.2");

        assertEquals("10.0.0.2", task.getExecutionNodeIp());
        server.verify();
    }

    @Test
    public void eligibleCachedWorkerRemainsStableWhenCandidatesGrowAndReorder() {
        expectDispatch(88, "10.0.0.1");
        expectDispatch(88, "10.0.0.1");
        expectDispatch(89, "10.0.0.2");

        dispatch(88, "10.0.0.1", "10.0.0.2");
        dispatch(88, "10.0.0.3", "10.0.0.2", "10.0.0.1");
        dispatch(89, "10.0.0.3", "10.0.0.2", "10.0.0.1");

        server.verify();
    }

    private void expectDispatch(int parentId, String worker) {
        server.expect(requestTo("http://" + worker + ":8081/api/worker/tasks"))
                .andExpect(jsonPath("$[0].parentTaskId").value(parentId))
                .andExpect(jsonPath("$[0].executionNodeIp").value(worker))
                .andExpect(jsonPath("$[0].localPath").value("/data/" + worker))
                .andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));
    }

    private ListSubTask dispatch(int parentId, String... workers) {
        ListSubTask task = new ListSubTask();
        task.setDirection(TransferDirection.OSS_TO_LOCAL);
        task.setParentTaskId(parentId);
        task.setShardId(parentId + "-oss-0");
        task.setSourceBucket("source");
        List<LocalNodeBinding> bindings = new ArrayList<>();
        for (String worker : workers) bindings.add(new LocalNodeBinding(worker, "/data/" + worker));
        service.distributeTasks(Collections.singletonList(task), DistributedService.MODE_AFFINITY,
                Arrays.asList(workers), bindings);
        return task;
    }
}
