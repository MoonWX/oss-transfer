package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.service.*;
import org.junit.Before;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;
import java.util.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.*;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.*;

public class TaskBandwidthControllerTest {
    private MockMvc mvc;
    private MasterTaskBandwidthService master;
    private WorkerTaskBandwidthService worker;
    private DistributedConfig config;
    private final ObjectMapper json = new ObjectMapper();

    @Before public void setup() {
        master=mock(MasterTaskBandwidthService.class); worker=mock(WorkerTaskBandwidthService.class);
        config=new DistributedConfig();config.setInternalToken("fixture-internal-token");
        TaskBandwidthController controller=new TaskBandwidthController();
        ReflectionTestUtils.setField(controller,"master",master);
        ReflectionTestUtils.setField(controller,"worker",worker);
        ReflectionTestUtils.setField(controller,"config",config);
        mvc=MockMvcBuilders.standaloneSetup(controller).build();
    }
    @Test public void invalidValuesNeverUpdatePolicy() throws Exception {
        for(String body:Arrays.asList("{}","[]","{\"limitMbps\":\"100\"}","{\"limitMbps\":true}",
                "{\"limitMbps\":0}","{\"limitMbps\":-1}","{\"limitMbps\":0.09}","{\"limitMbps\":1000001}","{\"limitMbps\":1e309}"))
            mvc.perform(put("/api/master/tasks/7/bandwidth-limit").contentType(MediaType.APPLICATION_JSON).content(body))
                    .andExpect(status().isBadRequest());
        verify(master,never()).update(anyInt(),any());
    }
    @Test public void explicitNullClearsButSmallValidNumberIsPreserved() throws Exception {
        when(master.update(7,null)).thenReturn(view(null,true));
        when(master.update(7,.1)).thenReturn(view(.1,true));
        mvc.perform(put("/api/master/tasks/7/bandwidth-limit").contentType(MediaType.APPLICATION_JSON).content("{\"limitMbps\":null}"))
                .andExpect(status().isOk()).andExpect(jsonPath("$.success").value(true));
        mvc.perform(put("/api/master/tasks/7/bandwidth-limit").contentType(MediaType.APPLICATION_JSON).content("{\"limitMbps\":0.1}"))
                .andExpect(status().isOk()).andExpect(jsonPath("$.data.limitMbps").value(.1));
        verify(master).update(7,null);verify(master).update(7,.1);
    }
    @Test public void partialApplicationIsReturnedAsPartialNotFalseSuccess() throws Exception {
        Map<String,Object> value=view(4.0,false);
        value.put("nodes",Arrays.asList(node("10.0.0.1",true),node("10.0.0.2",false)));
        when(master.update(7,4.0)).thenReturn(value);
        mvc.perform(put("/api/master/tasks/7/bandwidth-limit").contentType(MediaType.APPLICATION_JSON).content("{\"limitMbps\":4}"))
                .andExpect(status().isOk()).andExpect(jsonPath("$.data.applied").value(false))
                .andExpect(jsonPath("$.data.nodes[1].applied").value(false));
    }
    @Test public void workerPoliciesRejectBrowserTokensAndWrongInternalTokens() throws Exception {
        for(String token:Arrays.asList("","wrong")) {
            mvc.perform(get("/api/worker/task-bandwidth-policy").param("scopeId","local:7")
                    .header("Authorization","Bearer valid-user-session").header("X-Internal-Token",token))
                    .andExpect(status().isForbidden());
            mvc.perform(put("/api/worker/task-bandwidth-policy").header("Authorization","Bearer valid-user-session")
                    .header("X-Internal-Token",token).contentType(MediaType.APPLICATION_JSON).content(json.writeValueAsBytes(policy())))
                    .andExpect(status().isForbidden());
        }
        verifyZeroInteractions(worker);
    }
    @Test public void internalCallReturnsAppliedVersion() throws Exception {
        when(worker.apply(any())).thenAnswer(call->call.getArgument(0));
        mvc.perform(put("/api/worker/task-bandwidth-policy").header("X-Internal-Token","fixture-internal-token")
                .contentType(MediaType.APPLICATION_JSON).content(json.writeValueAsBytes(policy())))
                .andExpect(status().isOk()).andExpect(jsonPath("$.data.revision").value(2));
    }
    @Test public void absentConfiguredInternalSecretDoesNotPermitEmptyHeader() throws Exception {
        config.setInternalToken("");
        mvc.perform(get("/api/worker/task-bandwidth-policy").param("scopeId","local:7").header("X-Internal-Token",""))
                .andExpect(status().isForbidden());
        verifyZeroInteractions(worker);
    }
    private static TaskBandwidthPolicy policy() {
        TaskBandwidthPolicy p=new TaskBandwidthPolicy();p.setScopeId("local:7");p.setLimitMbps(4.0);
        p.setRevision(2);p.setMembers(Arrays.asList("10.0.0.1","10.0.0.2"));return p;
    }
    private static Map<String,Object> view(Double limit,boolean applied) {
        Map<String,Object> m=new LinkedHashMap<>();m.put("limitMbps",limit);m.put("applied",applied);m.put("sharedTaskCount",1);return m;
    }
    private static Map<String,Object> node(String address,boolean applied) {
        Map<String,Object> m=new LinkedHashMap<>();m.put("address",address);m.put("applied",applied);return m;
    }
}
