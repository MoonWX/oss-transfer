package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.ListExecutionService;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class TaskBandwidthPropagationTest {
    @Rule public TemporaryFolder temp=new TemporaryFolder();

    @Test public void creationSnapshotRestrictsAllGeneratedTasksToSameLogicalBudget() {
        DistributedConfig config=new DistributedConfig(); config.setWorkerIps(Arrays.asList("10.0.0.1","10.0.0.2")); config.setPort(1785);
        DistributedService distributor=new DistributedService();
        SubTaskManagerService tasks=mock(SubTaskManagerService.class); when(tasks.allocateTaskIdRange(2)).thenReturn(40);
        TaskCreationService creations=mock(TaskCreationService.class); when(creations.currentCreationId()).thenReturn("creation-test");
        MasterTaskBandwidthService bandwidth=mock(MasterTaskBandwidthService.class);
        TaskBandwidthPolicy policy=new TaskBandwidthPolicy(); policy.setScopeId("creation:creation-test");policy.setLimitMbps(8.0);
        policy.setMembers(Collections.singletonList("10.0.0.2"));policy.setRevision(1);
        when(bandwidth.desired(policy.getScopeId())).thenReturn(policy);
        ReflectionTestUtils.setField(distributor,"distributedConfig",config);
        ReflectionTestUtils.setField(distributor,"subTaskManagerService",tasks);
        ReflectionTestUtils.setField(distributor,"taskCreationService",creations);
        ReflectionTestUtils.setField(distributor,"taskBandwidth",bandwidth);
        MockRestServiceServer remote=MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(distributor,"restTemplate")).build();
        remote.expect(requestTo("http://10.0.0.2:1785/api/worker/tasks"))
                .andExpect(content().json("[{\"taskIndex\":40,\"taskBandwidthScopeId\":\"creation:creation-test\",\"taskBandwidthLimitMbps\":8.0},{\"taskIndex\":41,\"taskBandwidthScopeId\":\"creation:creation-test\",\"taskBandwidthLimitMbps\":8.0}]",false))
                .andRespond(withSuccess("{\"success\":true}",MediaType.APPLICATION_JSON));
        ListSubTask first=new ListSubTask(),second=new ListSubTask();
        distributor.distributeTasks(Arrays.asList(first,second),DistributedService.MODE_BALANCED);
        remote.verify(); verify(bandwidth,times(1)).requireAssignment(policy.getScopeId(),"10.0.0.2");
        verify(bandwidth).registerTask(40,policy.getScopeId()); verify(bandwidth).registerTask(41,policy.getScopeId());
        verify(creations).recordTaskIds(Arrays.asList(40,41));
    }

    @Test public void receivedTaskScopeSurvivesStatusAndQueueCopiesWithoutChangingExecutionId() {
        TaskImport incoming=new TaskImport(); incoming.setTaskBandwidthLimitMbps(9.5);incoming.setTaskBandwidthScopeId("local:7");
        incoming.setParentTaskId(7); incoming.setDirection(TransferDirection.OSS_TO_LOCAL);
        SubTaskStatus status=new SubTaskStatus();status.setTaskIndex(-71);status.setStatus(SubTaskStatus.TaskStatus.PENDING);
        ReflectionTestUtils.invokeMethod(new SubTaskManagerService(),"copyTransferFields",incoming,status);
        ObjectListMessage message=new ObjectListMessage();message.setTaskIndex(-71);
        ReflectionTestUtils.invokeMethod(new ListExecutionService(),"copyTransferFields",status,message);
        assertEquals("local:7",SubTaskStatusVO.from(status).getTaskBandwidthScopeId());
        assertEquals("local:7",message.getTaskBandwidthScopeId());assertEquals(9.5,message.getTaskBandwidthLimitMbps(),0);
        assertEquals(Integer.valueOf(-71),message.getTaskIndex());assertEquals(Integer.valueOf(7),message.getParentTaskId());
    }

    @Test public void queuedNewParentRetainsScopeAndDeletedParentCannotChangePolicy() throws Exception {
        RocksDBService db=new RocksDBService();ReflectionTestUtils.setField(db,"dbPath",temp.newFolder("db").toString());db.init();
        try {
            SubTaskManagerService tasks=mock(SubTaskManagerService.class);when(tasks.allocateTaskIdRange(1)).thenReturn(7);
            LocalParentTaskService parents=new LocalParentTaskService();ReflectionTestUtils.setField(parents,"rocksDBService",db);
            ReflectionTestUtils.setField(parents,"subTaskManagerService",tasks);
            LocalTransferRequest request=new LocalTransferRequest();request.setDirection(TransferDirection.OSS_TO_LOCAL);
            TransferEndpoint target=new TransferEndpoint();target.setType(EndpointType.LOCAL);target.setNodeBindings(Collections.singletonList(new LocalNodeBinding("10.0.0.1","/mnt/data")));
            request.setDestinationEndpoint(target);request.setTaskBandwidthLimitMbps(2.5);
            parents.create(Collections.singletonList(request));
            assertEquals("local:7",parents.get(7).getRequest().getTaskBandwidthScopeId());
            assertEquals(Double.valueOf(2.5),parents.toStatus(parents.get(7)).getTaskBandwidthLimitMbps());
            parents.softDelete(7,parents.toStatus(parents.get(7)));
            MasterTaskBandwidthService master=new MasterTaskBandwidthService();ReflectionTestUtils.setField(master,"parents",parents);
            ReflectionTestUtils.setField(master,"nodeConfig",new NodeConfigService());ReflectionTestUtils.setField(master,"rocksDBService",db);
            try {master.update(7,3.0);fail("Deleted parent must not accept a new policy");}
            catch(TransferValidationException expected){assertEquals("TASK_NOT_FOUND",expected.getCode());}
            assertNull(master.desired("local:7"));
        } finally {db.destroy();}
    }

    @Test public void legacyOrForeignImportCannotEraseAnExistingTaskScope() throws Exception {
        RocksDBService db=new RocksDBService();ReflectionTestUtils.setField(db,"dbPath",temp.newFolder("retained").toString());db.init();
        try {
            SubTaskStatus retained=new SubTaskStatus();retained.setTaskIndex(19);retained.setTaskBandwidthScopeId("creation:original");
            retained.setTaskBandwidthLimitMbps(1.0);retained.setStatus(SubTaskStatus.TaskStatus.PENDING);
            db.putSync("task:19",retained);
            SubTaskManagerService manager=new SubTaskManagerService();ReflectionTestUtils.setField(manager,"rocksDBService",db);
            ReflectionTestUtils.setField(manager,"endpointNormalizationService",mock(EndpointNormalizationService.class));
            for(String incomingScope:Arrays.asList(null,"creation:another")) {
                TaskImport incoming=new TaskImport();incoming.setTaskIndex(19);incoming.setTaskBandwidthScopeId(incomingScope);
                try {manager.importTasksWithTaskId(Collections.singletonList(incoming));fail("scope replacement must fail");}
                catch(TransferValidationException expected){assertEquals("TASK_BANDWIDTH_SCOPE_MISMATCH",expected.getCode());}
                assertEquals("creation:original",db.get("task:19",SubTaskStatus.class).getTaskBandwidthScopeId());
            }
        } finally {db.destroy();}
    }

    @Test public void missingPolicyOrWrongMemberIsRejectedBeforeTaskImport() {
        TaskBandwidthDispatchGuard guard=new TaskBandwidthDispatchGuard();WorkerTaskBandwidthService policies=mock(WorkerTaskBandwidthService.class);
        DistributedConfig config=new DistributedConfig();config.setLocalIp("10.0.0.1");
        ReflectionTestUtils.setField(guard,"policies",policies);ReflectionTestUtils.setField(guard,"config",config);
        for(Double limit:Arrays.asList(null,2.0)) {
            try {guard.requireReady("creation:missing",limit);fail();}
            catch(TransferValidationException expected){assertEquals("TASK_BANDWIDTH_NOT_READY",expected.getCode());}
        }
        TaskBandwidthPolicy foreign=new TaskBandwidthPolicy();foreign.setScopeId("creation:foreign");foreign.setMembers(Collections.singletonList("10.0.0.2"));
        when(policies.get(foreign.getScopeId())).thenReturn(foreign);
        try {guard.requireReady(foreign.getScopeId(),null);fail();}
        catch(TransferValidationException expected){assertEquals("TASK_BANDWIDTH_NOT_READY",expected.getCode());}
        guard.requireReady(null,null);
    }

    @Test public void localShardCollisionIsResolvedBeforeComparingExistingBandwidthScope() throws Exception {
        RocksDBService db=new RocksDBService();ReflectionTestUtils.setField(db,"dbPath",temp.newFolder("collision").toString());db.init();
        try {
            SubTaskStatus existing=new SubTaskStatus();existing.setTaskIndex(-19);existing.setParentTaskId(7);
            existing.setShardId("7-local-0");existing.setTaskBandwidthScopeId("local:7");existing.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
            db.putSync("task:-19",existing);
            SubTaskManagerService manager=new SubTaskManagerService();ReflectionTestUtils.setField(manager,"rocksDBService",db);
            ReflectionTestUtils.setField(manager,"endpointNormalizationService",mock(EndpointNormalizationService.class));
            ReflectionTestUtils.setField(manager,"fileReportLedgerService",mock(FileReportLedgerService.class));
            TaskImport incoming=new TaskImport();incoming.setTaskIndex(-19);incoming.setParentTaskId(8);
            incoming.setShardId("8-local-0");incoming.setDirection(TransferDirection.LOCAL_TO_OSS);incoming.setTaskBandwidthScopeId("local:8");
            manager.importTasksWithTaskId(Collections.singletonList(incoming));
            assertEquals(Integer.valueOf(-20),incoming.getTaskIndex());
            assertEquals("local:7",db.get("task:-19",SubTaskStatus.class).getTaskBandwidthScopeId());
            assertEquals("local:8",db.get("task:-20",SubTaskStatus.class).getTaskBandwidthScopeId());
        } finally {db.destroy();}
    }

    @Test public void resumingLegacyParentPreservesItsShareUntilUserConfiguresANewPolicy() throws Exception {
        RocksDBService db=new RocksDBService();ReflectionTestUtils.setField(db,"dbPath",temp.newFolder("legacy").toString());db.init();
        try {
            DistributedConfig config=new DistributedConfig();config.setLocalIp("10.0.0.1");config.setWorkerIps(Collections.singletonList("10.0.0.1"));
            WorkerTaskBandwidthService worker=mock(WorkerTaskBandwidthService.class);
            when(worker.apply(any(TaskBandwidthPolicy.class))).thenAnswer(call -> call.getArgument(0));
            MasterTaskBandwidthService master=new MasterTaskBandwidthService();
            ReflectionTestUtils.setField(master,"rocksDBService",db);ReflectionTestUtils.setField(master,"config",config);
            ReflectionTestUtils.setField(master,"workerPolicies",worker);
            LocalTransferRequest legacy=new LocalTransferRequest();legacy.setParentTaskId(7);legacy.setDirection(TransferDirection.OSS_TO_LOCAL);
            legacy.setBandwidthBitsPerSecond(50_000_000.0);
            TransferEndpoint local=new TransferEndpoint();local.setNodeBindings(Collections.singletonList(new LocalNodeBinding("10.0.0.1","/mnt/data")));
            legacy.setDestinationEndpoint(local);
            master.prepareLocal(legacy);
            assertNull(legacy.getTaskBandwidthScopeId());assertNull(master.desired("local:7"));
            assertEquals(Double.valueOf(50_000_000),legacy.getBandwidthBitsPerSecond());verifyZeroInteractions(worker);

            // An explicit user setting already persisted on Master must still be synchronized on resume.
            master.initialize("local:7",2.0,config.getWorkerIps());master.prepareLocal(legacy);
            assertEquals("local:7",legacy.getTaskBandwidthScopeId());assertEquals(Double.valueOf(2),legacy.getTaskBandwidthLimitMbps());
            verify(worker,times(2)).apply(any(TaskBandwidthPolicy.class));
        } finally {db.destroy();}
    }
}
