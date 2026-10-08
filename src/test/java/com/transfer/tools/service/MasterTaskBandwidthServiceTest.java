package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.HttpMethod;
import org.springframework.http.HttpStatus;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.util.*;
import java.util.concurrent.atomic.AtomicReference;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class MasterTaskBandwidthServiceTest {
    @Rule public TemporaryFolder temp=new TemporaryFolder();
    private RocksDBService db;
    private String dbPath;
    private DistributedConfig config;
    private LocalParentTaskService parents;
    private WorkerTaskBandwidthService worker;
    private MasterTaskBandwidthService master;
    private MasterLocalTransferService localTransfers;
    private MockRestServiceServer remote;
    private final AtomicReference<TaskBandwidthPolicy> localPolicy=new AtomicReference<>();
    private final ObjectMapper json=new ObjectMapper();
    private static final List<String> NODES=Arrays.asList("10.0.0.1","10.0.0.2");

    @Before public void setup() throws Exception {
        dbPath=temp.newFolder("db").toString();openDb();
        config=new DistributedConfig();config.setLocalIp(NODES.get(0));config.setMasterIp(NODES.get(0));
        config.setPort(1785);config.setWorkerIps(NODES);config.setInternalToken("fixture-token");
        parents=mock(LocalParentTaskService.class);worker=mock(WorkerTaskBandwidthService.class);
        LocalTransferRequest request=new LocalTransferRequest();request.setDirection(TransferDirection.LOCAL_TO_OSS);
        TransferEndpoint local=new TransferEndpoint();local.setType(EndpointType.LOCAL);
        List<LocalNodeBinding> bindings=new ArrayList<>();
        for(String node:NODES){LocalNodeBinding b=new LocalNodeBinding();b.setExecutionNodeIp(node);b.setLocalPath("/test");bindings.add(b);}
        local.setNodeBindings(bindings);request.setSourceEndpoint(local);
        LocalParentTaskRecord parent=new LocalParentTaskRecord();parent.setParentTaskId(7);parent.setRequest(request);
        when(parents.get(7)).thenReturn(parent);
        when(worker.apply(any(TaskBandwidthPolicy.class))).thenAnswer(call->{TaskBandwidthPolicy p=call.getArgument(0);localPolicy.set(p);return p;});
        when(worker.get(anyString())).thenAnswer(call->localPolicy.get());
        localTransfers=mock(MasterLocalTransferService.class);
        when(localTransfers.aggregateStatus(eq(7), isNull(), isNull(), isNull(), isNull()))
                .thenReturn(localSnapshot("RUNNING", "RUNNING", "COMPLETED"));
        createMaster();
    }
    private void openDb(){db=new RocksDBService();ReflectionTestUtils.setField(db,"dbPath",dbPath);db.init();}
    private void createMaster(){
        master=new MasterTaskBandwidthService();
        ReflectionTestUtils.setField(master,"rocksDBService",db);ReflectionTestUtils.setField(master,"config",config);
        ReflectionTestUtils.setField(master,"parents",parents);ReflectionTestUtils.setField(master,"workerPolicies",worker);
        ReflectionTestUtils.setField(master,"nodeConfig",new NodeConfigService());
        ReflectionTestUtils.setField(master,"localTransfers",localTransfers);
        remote=MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(master,"restTemplate")).build();
    }
    @After public void close(){if(db!=null)db.destroy();}
    private TaskBandwidthPolicy policy(Double limit,long revision){
        TaskBandwidthPolicy p=new TaskBandwidthPolicy();p.setScopeId("local:7");p.setMembers(NODES);p.setLimitMbps(limit);p.setRevision(revision);return p;
    }
    private void succeeds(HttpMethod method,Double limit,long revision) throws Exception {
        remote.expect(requestTo("http://10.0.0.2:1785/api/worker/task-bandwidth-policy?scopeId=local%3A7"))
                .andExpect(method(method)).andExpect(header("X-Internal-Token","fixture-token"))
                .andRespond(withSuccess(json.writeValueAsString(ApiResponse.success(policy(limit,revision))),MediaType.APPLICATION_JSON));
    }
    private Map<String,Object> localSnapshot(String parentStatus, String... childStatuses) {
        SubTaskStatus parent=new SubTaskStatus();parent.setTaskIndex(7);parent.setStatus(SubTaskStatus.TaskStatus.valueOf(parentStatus));parent.setScanComplete(true);
        List<SubTaskStatus> children=new ArrayList<>();
        for(int i=0;i<childStatuses.length;i++){SubTaskStatus child=new SubTaskStatus();child.setTaskIndex(-i-1);child.setParentTaskId(7);child.setStatus(SubTaskStatus.TaskStatus.valueOf(childStatuses[i]));children.add(child);}
        Map<String,Object> result=new HashMap<>();result.put("tasks",Collections.singletonList(parent));result.put("shards",children);result.put("partial",false);return result;
    }
    @Test public void completedLocalTaskCannotChangeDesiredRevisionOrWorkerPolicy() {
        when(localTransfers.aggregateStatus(eq(7), isNull(), isNull(), isNull(), isNull()))
                .thenReturn(localSnapshot("COMPLETED", "COMPLETED", "COMPLETED"));
        assertEquals(false, master.get(7).get("canEdit"));
        try { master.update(7, 100.0); fail("completed task must be immutable"); }
        catch (TransferValidationException expected) { assertEquals("TASK_COMPLETED", expected.getCode()); }
        assertNull(master.desired("local:7")); verify(worker,never()).apply(any()); remote.verify();
    }
    @Test public void emptyCompletedLocalTaskWithOnlyDiscoveryStillCannotChangeBandwidth() {
        Map<String,Object> snapshot=localSnapshot("COMPLETED", "COMPLETED");
        SubTaskStatus discovery=(SubTaskStatus)((List<?>)snapshot.get("shards")).get(0);
        discovery.setTaskIndex(7);discovery.setShardId("discovery");
        discovery.setListObjectCount(0L);discovery.setPutObjectCount(0L);
        when(localTransfers.aggregateStatus(eq(7), isNull(), isNull(), isNull(), isNull())).thenReturn(snapshot);
        assertEquals(false,master.get(7).get("canEdit"));
        try {master.update(7,100.0);fail("empty completed task");}
        catch(TransferValidationException expected){assertEquals("TASK_COMPLETED",expected.getCode());}
        verify(worker,never()).apply(any());assertNull(master.desired("local:7"));
    }
    @Test public void localChildStatesOverrideAnInaccurateCompletedParent() throws Exception {
        when(localTransfers.aggregateStatus(eq(7), isNull(), isNull(), isNull(), isNull()))
                .thenReturn(localSnapshot("COMPLETED", "COMPLETED", "STOPPED"));
        assertEquals(true, master.get(7).get("canEdit"));
        succeeds(HttpMethod.PUT,3.0,1);
        assertEquals(true, master.update(7,3.0).get("canEdit")); remote.verify();
    }
    @Test public void selectedCreationNodeReceivesEntireTaskBudgetAndOtherNodesAreUntouched() {
        BucketPair pair=new BucketPair();pair.setTaskBandwidthLimitMbps(100.0);
        pair.setEligibleWorkerNodes(Arrays.asList(NODES.get(0), NODES.get(0)));
        master.prepareCreation(pair,"one-node",NODES);
        assertEquals(Collections.singletonList(NODES.get(0)), master.desired("creation:one-node").getMembers());
        assertEquals(Collections.singletonList(NODES.get(0)), pair.getEligibleWorkerNodes());
        assertEquals(Double.valueOf(100), localPolicy.get().getLimitMbps()); remote.verify();
        config.setWorkerIps(Arrays.asList(NODES.get(0),NODES.get(1),"10.0.0.3"));
        master.prepareCreation(pair,"one-node",config.getWorkerIps());
        assertEquals(Collections.singletonList(NODES.get(0)), master.desired("creation:one-node").getMembers());
        assertEquals(1L, master.desired("creation:one-node").getRevision());
    }
    @Test public void invalidCreationSelectionNeverSavesOrAppliesPolicy() {
        for(List<String> selected: Arrays.asList(Collections.<String>emptyList(), Collections.singletonList("unregistered"), Collections.<String>singletonList(null))) {
            BucketPair pair=new BucketPair();pair.setEligibleWorkerNodes(selected);
            try {master.prepareCreation(pair,"invalid",NODES);fail("invalid nodes");}
            catch(TransferValidationException expected){assertEquals("NO_ELIGIBLE_WORKER",expected.getCode());}
        }
        assertNull(master.desired("creation:invalid"));verify(worker,never()).apply(any());remote.verify();
    }
    @Test public void savedPolicySurvivesMasterRestartAndRevisionsIncreaseForRaiseAndClear() throws Exception {
        succeeds(HttpMethod.PUT,4.0,1);assertTrue((Boolean)master.update(7,4.0).get("applied"));remote.verify();
        db.destroy();openDb();createMaster();
        succeeds(HttpMethod.GET,4.0,1);Map<String,Object> restored=master.get(7);
        assertEquals(4.0,restored.get("limitMbps"));assertEquals(1L,restored.get("revision"));assertEquals(true,restored.get("applied"));remote.verify();
        remote.reset();succeeds(HttpMethod.PUT,8.0,2);assertEquals(true,master.update(7,8.0).get("applied"));remote.verify();
        remote.reset();succeeds(HttpMethod.PUT,null,3);assertEquals(true,master.update(7,null).get("applied"));remote.verify();
        assertNull(master.desired("local:7").getLimitMbps());assertEquals(3L,master.desired("local:7").getRevision());
    }
    @Test public void offlineNodeLeavesDesiredValueSavedButNeverReportsFullyApplied() throws Exception {
        remote.expect(requestTo("http://10.0.0.2:1785/api/worker/task-bandwidth-policy?scopeId=local%3A7"))
                .andRespond(withStatus(HttpStatus.SERVICE_UNAVAILABLE));
        Map<String,Object> result=master.update(7,2.0);
        assertEquals(false,result.get("applied"));assertEquals(2.0,result.get("limitMbps"));
        List<Map<String,Object>> nodes=(List<Map<String,Object>>)result.get("nodes");
        assertEquals(true,nodes.get(0).get("applied"));assertEquals(false,nodes.get(1).get("applied"));
        assertEquals(Double.valueOf(2),master.desired("local:7").getLimitMbps());remote.verify();
        remote.reset();succeeds(HttpMethod.PUT,2.0,2);
        assertEquals(true,master.update(7,2.0).get("applied"));remote.verify();
    }
    @Test public void staleWorkerAcknowledgementDoesNotCountAsSuccess() throws Exception {
        succeeds(HttpMethod.PUT,1.0,1);master.update(7,1.0);remote.verify();remote.reset();
        succeeds(HttpMethod.PUT,1.0,1);
        Map<String,Object> result=master.update(7,2.0);
        assertEquals(false,result.get("applied"));assertEquals(2.0,result.get("limitMbps"));remote.verify();
    }
    @Test public void databaseFailureDoesNotApplyAnUncommittedPolicy() throws Exception {
        RocksDBService failing=spy(db);doThrow(new RuntimeException("disk unavailable")).when(failing).putSync(anyString(),any());
        ReflectionTestUtils.setField(master,"rocksDBService",failing);
        try{master.update(7,3.0);fail("Expected durable storage failure");}catch(RuntimeException expected){assertEquals("disk unavailable",expected.getMessage());}
        verify(worker,never()).apply(any());assertNull(master.desired("local:7"));remote.verify();
    }
    @Test public void newScopeRequiresEveryNodeBeforeDispatchAndFixesMembership() throws Exception {
        remote.expect(requestTo("http://10.0.0.2:1785/api/worker/task-bandwidth-policy?scopeId=local%3A7"))
                .andRespond(withStatus(HttpStatus.SERVICE_UNAVAILABLE));
        try{master.initialize("local:7",4.0,NODES);fail("Missing worker must block dispatch");}
        catch(TransferValidationException expected){assertEquals("TASK_BANDWIDTH_NOT_READY",expected.getCode());}
        assertEquals(NODES,master.desired("local:7").getMembers());remote.verify();
        try{master.initialize("local:7",4.0,Collections.singletonList(NODES.get(0)));fail("Cannot redistribute offline quota");}
        catch(TransferValidationException expected){assertEquals("TASK_BANDWIDTH_MEMBERS_CHANGED",expected.getCode());}
    }
}
