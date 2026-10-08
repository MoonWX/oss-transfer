package com.transfer.tools.controller;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.service.LocalTransferService;
import com.transfer.tools.service.SubTaskManagerService;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;
import java.util.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.*;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.*;

public class TaskBandwidthScopeGuardTest {
    private DistributedConfig config() {
        DistributedConfig c=new DistributedConfig();c.setInternalToken("fixture-token");c.setLocalIp("10.0.0.1");return c;
    }
    @Test public void directWorkerImportsCannotJoinAnotherTaskQuota() throws Exception {
        WorkerController c=new WorkerController();SubTaskManagerService tasks=mock(SubTaskManagerService.class);
        ReflectionTestUtils.setField(c,"distributedConfig",config());ReflectionTestUtils.setField(c,"subTaskManagerService",tasks);
        MockMvc mvc=MockMvcBuilders.standaloneSetup(c).build();
        for(String path:Arrays.asList("/api/worker/tasks","/api/worker/import"))
            for(String field:Arrays.asList("\"taskBandwidthScopeId\":\"creation:existing\"","\"taskBandwidthLimitMbps\":100"))
                mvc.perform(post(path).header("Authorization","Bearer browser-user").contentType(MediaType.APPLICATION_JSON)
                        .content("[{"+field+"}]")) .andExpect(status().isForbidden());
        verifyZeroInteractions(tasks);
    }
    @Test public void localCreationDiscoveryAndNestedBatchesRequireInternalAuthentication() throws Exception {
        WorkerLocalTransferController c=new WorkerLocalTransferController();LocalTransferService local=mock(LocalTransferService.class);
        ReflectionTestUtils.setField(c,"distributedConfig",config());ReflectionTestUtils.setField(c,"localTransferService",local);
        MockMvc mvc=MockMvcBuilders.standaloneSetup(c).build();
        String scoped="{\"taskBandwidthScopeId\":\"local:7\"}";
        mvc.perform(post("/api/worker/local-transfers").contentType(MediaType.APPLICATION_JSON).content("["+scoped+"]"))
                .andExpect(status().isForbidden()).andExpect(jsonPath("$.code").value("INTERNAL_AUTH_REQUIRED"));
        mvc.perform(post("/api/worker/local-transfers/discover").contentType(MediaType.APPLICATION_JSON).content(scoped))
                .andExpect(status().isForbidden());
        mvc.perform(post("/api/worker/local-transfers/batches").contentType(MediaType.APPLICATION_JSON).content("{\"request\":"+scoped+"}"))
                .andExpect(status().isForbidden());
        verifyZeroInteractions(local);
    }
    @Test public void internalScopesAndLegacyUnscopedRequestsRemainAccepted() throws Exception {
        WorkerLocalTransferController c=new WorkerLocalTransferController();LocalTransferService local=mock(LocalTransferService.class);
        ReflectionTestUtils.setField(c,"distributedConfig",config());ReflectionTestUtils.setField(c,"localTransferService",local);
        when(local.accept(anyList())).thenReturn(Collections.singletonList(7));
        MockMvc mvc=MockMvcBuilders.standaloneSetup(c).build();
        mvc.perform(post("/api/worker/local-transfers").header("X-Internal-Token","fixture-token").contentType(MediaType.APPLICATION_JSON)
                .content("[{\"taskBandwidthScopeId\":\"local:7\"}]")) .andExpect(status().isOk());
        mvc.perform(post("/api/worker/local-transfers").contentType(MediaType.APPLICATION_JSON).content("[{}]"))
                .andExpect(status().isOk());
        verify(local,times(2)).accept(anyList());
    }
}
