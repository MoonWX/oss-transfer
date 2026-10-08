package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.TaskBandwidthPolicy;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.springframework.http.*;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class MasterTaskBandwidthIdentityTest {
    private MasterTaskBandwidthService master;
    private RocksDBService db;
    private WorkerTaskBandwidthService worker;
    private MockRestServiceServer remote;
    @Before public void setup() {
        DistributedConfig config=new DistributedConfig();config.setLocalIp("10.0.0.1");config.setPort(1785);
        config.setWorkerIps(Arrays.asList("10.0.0.1","10.0.0.2"));config.setInternalToken("fixture-token");
        db=mock(RocksDBService.class);worker=mock(WorkerTaskBandwidthService.class);
        master=new MasterTaskBandwidthService();ReflectionTestUtils.setField(master,"config",config);
        ReflectionTestUtils.setField(master,"rocksDBService",db);ReflectionTestUtils.setField(master,"workerPolicies",worker);
        ReflectionTestUtils.setField(master,"parents",mock(LocalParentTaskService.class));
        ReflectionTestUtils.setField(master,"nodeConfig",new NodeConfigService());
        remote=MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(master,"restTemplate")).build();
    }
    private void node(String address,String tasks) {
        remote.expect(requestTo("http://"+address+":1785/api/worker/status"))
                .andExpect(header("X-Internal-Token","fixture-token"))
                .andRespond(withSuccess("{\"tasks\":"+tasks+"}",MediaType.APPLICATION_JSON));
    }
    @Test public void duplicateIdRejectsReadAndUpdateEvenWithSavedScopeMapping() {
        when(db.get("task_bandwidth:task:71",String.class)).thenReturn("creation:first");
        for(boolean update:Arrays.asList(false,true)) {
            node("10.0.0.1","[{\"taskIndex\":71,\"taskBandwidthScopeId\":\"creation:first\"}]");
            node("10.0.0.2","[{\"taskIndex\":71,\"taskBandwidthScopeId\":\"creation:second\"}]");
            try {if(update)master.update(71,2.0);else master.get(71);fail("Duplicate IDs must not select the first worker");}
            catch(TransferValidationException expected){assertEquals("TASK_ID_AMBIGUOUS",expected.getCode());}
            remote.verify();remote.reset();
        }
        verify(db,never()).putSync(anyString(),any());verifyZeroInteractions(worker);
    }
    @Test public void unreachableLaterNodePreventsSelectingEarlierMatch() {
        node("10.0.0.1","[{\"taskIndex\":71}]");
        remote.expect(requestTo("http://10.0.0.2:1785/api/worker/status")).andRespond(withStatus(HttpStatus.SERVICE_UNAVAILABLE));
        try {master.update(71,2.0);fail("Incomplete inventory cannot prove uniqueness");}
        catch(DownstreamWorkerException expected){assertEquals("WORKER_UNAVAILABLE",expected.getCode());}
        remote.verify();verify(db,never()).putSync(anyString(),any());verifyZeroInteractions(worker);
    }
    @Test public void malformedNodeInventoryIsNotTreatedAsEmpty() {
        node("10.0.0.1","[{\"taskIndex\":71}]");node("10.0.0.2","[{\"status\":\"COMPLETED\"}]");
        try {master.get(71);fail();}
        catch(DownstreamWorkerException expected){assertEquals("WORKER_UNAVAILABLE",expected.getCode());}
        remote.verify();
    }
    @Test public void uniqueLegacyTaskKeepsItsIndependentScopeAfterAllNodesConfirm() {
        node("10.0.0.1","[{\"taskIndex\":71}]");node("10.0.0.2","[{\"taskIndex\":72}]");
        Map<String,Object> result=master.get(71);
        assertEquals("task:71",result.get("scopeId"));assertEquals(true,result.get("canEdit"));
        assertEquals(true,result.get("applied"));assertNull(result.get("limitMbps"));remote.verify();
    }
    private TaskCreationService.Creation groupedCreation() {
        TaskCreationService creations=mock(TaskCreationService.class);
        TaskCreationService.Creation group=new TaskCreationService.Creation();group.setId("fixture");
        group.setTaskIds(Arrays.asList(71,72));group.setScanComplete(true);group.setPublicTaskId(71);
        when(creations.findByTaskId(71)).thenReturn(group);when(creations.findByTaskId(72)).thenReturn(group);
        ReflectionTestUtils.setField(master,"creations",creations);return group;
    }
    private void groupedStatus(String first,String second) {
        node("10.0.0.1","[{\"taskIndex\":71,\"taskBandwidthScopeId\":\"creation:fixture\",\"status\":\""+first+"\"},"
                +"{\"taskIndex\":72,\"taskBandwidthScopeId\":\"creation:fixture\",\"status\":\""+second+"\"}]");
        node("10.0.0.2","[]");
    }
    @Test public void completedOssGroupRejectsBothPublicAndChildIdWithoutPolicyChanges() {
        groupedCreation();groupedStatus("COMPLETED","COMPLETED");
        assertEquals(false,master.get(71).get("canEdit"));remote.verify();remote.reset();
        for(int taskId:Arrays.asList(71,72)) {
            groupedStatus("COMPLETED","COMPLETED");
            try {master.update(taskId,100.0);fail("all children completed");}
            catch(TransferValidationException expected){assertEquals("TASK_COMPLETED",expected.getCode());}
            remote.verify();remote.reset();
        }
        verify(db,never()).putSync(anyString(),any());verify(db,never()).putBatchSync(anyMap());verifyZeroInteractions(worker);
    }
    @Test public void mixedOssGroupRemainsEditableWhenPublicChildCompletedFirst() {
        groupedCreation();groupedStatus("COMPLETED","RUNNING");
        assertEquals(true,master.get(71).get("canEdit"));remote.verify();remote.reset();
        groupedStatus("COMPLETED","RUNNING");
        when(worker.apply(any(TaskBandwidthPolicy.class))).thenAnswer(call->call.getArgument(0));
        assertEquals(true,master.update(71,100.0).get("canEdit"));
        verify(db).putSync(eq("task_bandwidth:desired:creation:fixture"),any(TaskBandwidthPolicy.class));
        verify(worker).apply(any(TaskBandwidthPolicy.class));remote.verify();
    }
    @Test public void stoppedOrFailedOssGroupRemainsEditable() {
        TaskCreationService.Creation group=groupedCreation();
        for(String status:Arrays.asList("STOPPED","FAILED")) {
            group.setStopped("STOPPED".equals(status));groupedStatus("COMPLETED",status);
            assertEquals(true,master.get(71).get("canEdit"));remote.verify();remote.reset();
        }
    }
    @Test public void completeEmptyInventoriesReturnNotFound() {
        node("10.0.0.1","[]");node("10.0.0.2","[]");
        try {master.get(71);fail();}
        catch(TransferValidationException expected){assertEquals("TASK_NOT_FOUND",expected.getCode());}
        remote.verify();
    }
}
