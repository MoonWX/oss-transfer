package com.transfer.tools.service;

import com.transfer.tools.agent.AgentRegistryService;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import org.junit.*;
import org.springframework.http.*;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import org.springframework.web.server.ResponseStatusException;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class TaskChildrenQueryTest {
    private MasterTaskChildrenService master;
    private TaskCreationService creations;
    private LocalParentTaskService parents;
    private AgentRegistryService agents;
    private MockRestServiceServer server;
    @Before public void setup() {
        master = new MasterTaskChildrenService(); creations = mock(TaskCreationService.class);
        parents = mock(LocalParentTaskService.class); agents = mock(AgentRegistryService.class);
        DistributedConfig config = new DistributedConfig(); config.setLocalIp("10.0.0.10");
        config.setWorkerIps(Collections.singletonList("10.0.0.20")); config.setInternalToken("test-only");
        ReflectionTestUtils.setField(master,"config",config); ReflectionTestUtils.setField(master,"creations",creations);
        ReflectionTestUtils.setField(master,"parents",parents); ReflectionTestUtils.setField(master,"agents",agents);
        server = MockRestServiceServer.bindTo((RestTemplate) ReflectionTestUtils.getField(master,"http")).build();
    }
    @After public void close() { master.close(); }
    private void creation(Integer... ids) {
        TaskCreationService.Creation value = new TaskCreationService.Creation(); value.setId("creation-224"); value.setPublicTaskId(224);
        value.setTaskIds(Arrays.asList(ids)); when(creations.findByTaskId(224)).thenReturn(value);
    }
    private void capability(String response) {
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/node-info"))
                .andExpect(header("X-Internal-Token","test-only"))
                .andRespond(withSuccess(response,MediaType.APPLICATION_JSON));
    }
    @Test public void newNodeGetsOnlyRequestedIdsAndKeepsGlobalNumber() {
        creation(70,71,72); capability("{\"taskChildren\":true}");
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/task-children?parentTaskId=224&offset=2&limit=1&creationId=creation-224&includeIndex=true"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"totalCount\":3,\"taskNumbers\":{\"70\":1,\"71\":2,\"72\":3},\"items\":[{\"taskIndex\":72,\"subtaskNumber\":3,\"status\":\"COMPLETED\"}]}}",MediaType.APPLICATION_JSON));
        Map<String,Object> value = master.query(224,2,1,null);
        Map item = (Map)((List)value.get("items")).get(0);
        assertEquals("224-3",item.get("childTaskId")); assertEquals(3,item.get("subtaskNumber"));
        assertEquals(3,value.get("totalCount")); assertEquals(false,value.get("partial")); server.verify();
    }
    @Test public void olderAgentUsesCachedLegacySnapshotWithoutSendingUnsupportedRpc() {
        creation(70,71); when(agents.isAgent("10.0.0.20")).thenReturn(true);
        when(agents.currentEnrollment("10.0.0.20")).thenReturn("enrollment-1");
        capability("{\"localIp\":\"10.0.0.20\"}");
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/status"))
                .andRespond(withSuccess("{\"tasks\":[{\"taskIndex\":70},{\"taskIndex\":71}]}",MediaType.APPLICATION_JSON));
        Map first = master.query(224,0,1,null), second = master.query(224,1,1,null);
        assertEquals(false,first.get("cached")); assertEquals(true,second.get("cached"));
        assertEquals(first.get("snapshotAt"),second.get("snapshotAt"));
        assertEquals("224-2",((Map)((List)second.get("items")).get(0)).get("childTaskId")); server.verify();
    }
    @Test public void upgradedNodeWithLegacyOssRecordsKeepsLegacyPagingAfterTheFirstIndexProbe() {
        creation(100,101,102); when(agents.isAgent("10.0.0.20")).thenReturn(true);
        when(agents.currentEnrollment("10.0.0.20")).thenReturn("test"); capability("{\"taskChildren\":true}");
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/task-children?parentTaskId=224&offset=0&limit=1&creationId=creation-224&includeIndex=true"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"items\":[],\"taskNumbers\":{},\"totalCount\":0}}",MediaType.APPLICATION_JSON));
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/status"))
                .andRespond(withSuccess("{\"tasks\":[{\"taskIndex\":100,\"shardIndex\":0},{\"taskIndex\":101,\"shardIndex\":2},{\"taskIndex\":102,\"shardIndex\":1}]}",MediaType.APPLICATION_JSON));
        master.query(224,0,1,null);
        Map value=master.query(224,1,1,null); Map child=(Map)((List)value.get("items")).get(0);
        assertEquals(102,child.get("taskIndex")); assertEquals("224-2",child.get("childTaskId"));
        assertEquals(true,value.get("cached")); assertEquals(false,value.get("partial")); server.verify();
    }
    @Test public void capabilityFailureIsPartialAndNeverFallsBackToLegacy() {
        creation(70);
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/node-info")).andRespond(withStatus(HttpStatus.SERVICE_UNAVAILABLE));
        Map value = master.query(224,0,25,null);
        assertEquals(true,value.get("partial")); assertEquals(Collections.singletonList("10.0.0.20"),value.get("unreachableWorkers")); server.verify();
    }
    @Test public void replacedAgentCannotInheritOldCapabilityOrSnapshot() {
        creation(70); when(agents.isAgent("10.0.0.20")).thenReturn(true);
        when(agents.currentEnrollment("10.0.0.20")).thenReturn("old","old","new","new");
        capability("{}"); server.expect(requestTo("http://10.0.0.20:1685/api/worker/status"))
                .andRespond(withSuccess("{\"tasks\":[{\"taskIndex\":70}]}",MediaType.APPLICATION_JSON));
        capability("{\"taskChildren\":true}");
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/task-children?parentTaskId=224&offset=0&limit=25&creationId=creation-224&includeIndex=true"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"totalCount\":1,\"taskNumbers\":{\"70\":1},\"items\":[{\"taskIndex\":70,\"subtaskNumber\":1}]}}",MediaType.APPLICATION_JSON));
        master.query(224,0,25,null); Map value = master.query(224,0,25,null);
        assertEquals(false,value.get("cached")); server.verify();
    }
    @Test public void unregisteredNodeAndOversizedPageAreRejectedBeforeAnyNetworkRequest() {
        for (String node : Arrays.asList("127.0.0.1","example.com")) try { master.query(224,0,25,node); fail(); }
        catch (ResponseStatusException expected) { assertEquals(HttpStatus.BAD_REQUEST,expected.getStatus()); }
        try { master.query(224,0,201,null); fail(); } catch (ResponseStatusException expected) { }
        server.verify();
    }
    @Test public void localOrdinalHolesAndCopiesAcrossNodesRemainVisible() {
        DistributedConfig config = (DistributedConfig)ReflectionTestUtils.getField(master,"config");
        config.setWorkerIps(Arrays.asList("10.0.0.20","10.0.0.21"));
        server = MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(master,"http")).ignoreExpectOrder(true).build();
        for (String node : config.getWorkerIps()) {
            server.expect(requestTo("http://"+node+":1685/api/worker/node-info")).andRespond(withSuccess("{\"taskChildren\":true}",MediaType.APPLICATION_JSON));
            server.expect(requestTo("http://"+node+":1685/api/worker/task-children?parentTaskId=224&offset=2&limit=1"))
                    .andRespond(withSuccess("{\"success\":true,\"data\":{\"totalCount\":9,\"items\":[{\"taskIndex\":-71,\"subtaskNumber\":3}]}}",MediaType.APPLICATION_JSON));
        }
        Map value = master.query(224,2,1,null); assertEquals(2,((List)value.get("items")).size());
        assertEquals(9,value.get("totalCount")); assertEquals(true,value.get("hasMore")); assertEquals(true,value.get("partial")); server.verify();
    }
    @Test public void balancedInterleavedTaskIdsKeepShardOrdinalAndSingleNodeIndexIsCached() {
        creation(100,101,102); when(agents.isAgent("10.0.0.20")).thenReturn(true);
        when(agents.currentEnrollment("10.0.0.20")).thenReturn("test");
        capability("{\"taskChildren\":true}");
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/task-children?parentTaskId=224&offset=1&limit=1&creationId=creation-224&includeIndex=true"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"totalCount\":3,\"taskNumbers\":{\"102\":2},\"items\":[{\"taskIndex\":102,\"subtaskNumber\":2}]}}",MediaType.APPLICATION_JSON));
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/task-children?parentTaskId=224&offset=1&limit=1&creationId=creation-224&includeIndex=false"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"totalCount\":3,\"items\":[{\"taskIndex\":102,\"subtaskNumber\":2}]}}",MediaType.APPLICATION_JSON));
        for (int attempt=0;attempt<2;attempt++) {
            Map value=master.query(224,1,1,"10.0.0.20");
            Map child=(Map)((List)value.get("items")).get(0);
            assertEquals(102,child.get("taskIndex")); assertEquals("224-2",child.get("childTaskId"));
            assertEquals(false,value.get("partial"));
        }
        server.verify();
    }
    @Test public void successfulRpcsWithMissingExpectedPhysicalTaskArePartial() {
        creation(70,71); capability("{\"taskChildren\":true}");
        server.expect(requestTo("http://10.0.0.20:1685/api/worker/task-children?parentTaskId=224&offset=1&limit=1&creationId=creation-224&includeIndex=true"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"totalCount\":2,\"taskNumbers\":{\"70\":1,\"71\":2},\"items\":[]}}",MediaType.APPLICATION_JSON));
        Map value=master.query(224,1,1,null); assertEquals(true,value.get("partial"));
        assertEquals(Collections.singletonList(71),value.get("missingTaskIds")); server.verify();
    }
    @Test public void reusedLocalPhysicalIdRefreshesMembershipEvenWithoutAnIntermediateEmptyQuery() {
        WorkerTaskChildrenService service=new WorkerTaskChildrenService(); SubTaskManagerService tasks=mock(SubTaskManagerService.class);
        ReflectionTestUtils.setField(service,"tasks",tasks); when(tasks.getTaskIdList()).thenReturn(Collections.singletonList(-1));
        SubTaskStatus first=new SubTaskStatus(); first.setTaskIndex(-1); first.setParentTaskId(224); first.setShardIndex(0);
        first.setDirection(TransferDirection.OSS_TO_LOCAL); first.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
        SubTaskStatus replacement=new SubTaskStatus(); replacement.setTaskIndex(-1); replacement.setParentTaskId(225); replacement.setShardIndex(4);
        replacement.setDirection(TransferDirection.LOCAL_TO_OSS); replacement.setStatus(SubTaskStatus.TaskStatus.PENDING);
        when(tasks.getTaskStatus(-1)).thenReturn(first);
        service.query(224,0,25,null);
        when(tasks.taskIdentityVersion(-1)).thenReturn(1L); when(tasks.getTaskStatus(-1)).thenReturn(replacement);
        assertEquals(0,((List)service.query(224,0,25,null).get("items")).size());
        Map value=service.query(225,0,25,null);
        assertEquals(5,((Map)((List)value.get("items")).get(0)).get("subtaskNumber"));
    }
    @Test public void workerIndexesMetadataOnceAndReadsOnlyVisibleRowsAfterward() {
        WorkerTaskChildrenService service = new WorkerTaskChildrenService();
        SubTaskManagerService tasks = mock(SubTaskManagerService.class);
        ReflectionTestUtils.setField(service,"tasks",tasks);
        when(tasks.getTaskIdList()).thenReturn(Arrays.asList(-1,-2,-3));
        for (int index=0;index<3;index++) {
            SubTaskStatus value = new SubTaskStatus(); value.setTaskIndex(-index-1); value.setParentTaskId(224);
            value.setShardIndex(index*2); value.setDirection(TransferDirection.OSS_TO_LOCAL); value.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
            when(tasks.getTaskStatus(-index-1)).thenReturn(value);
        }
        Map first = service.query(224,2,1,null); assertEquals(5,first.get("totalCount"));
        assertEquals(3,((Map)((List)first.get("items")).get(0)).get("subtaskNumber"));
        clearInvocations(tasks);
        service.query(224,4,1,null);
        verify(tasks,times(1)).getTaskStatus(-3); verify(tasks,never()).getTaskStatus(-1); verify(tasks,never()).getTaskStatus(-2);
        when(tasks.getTaskIdList()).thenReturn(Collections.singletonList(-1)); clearInvocations(tasks);
        Map remaining = service.query(224,0,25,null); assertEquals(1,remaining.get("totalCount"));
        assertEquals(1,((List)remaining.get("items")).size()); verify(tasks,never()).getTaskStatus(-2); verify(tasks,never()).getTaskStatus(-3);
    }
    @Test public void concurrentIdReuseDoesNotBlessOldMetadataWithTheNewIncarnation() {
        WorkerTaskChildrenService service=new WorkerTaskChildrenService(); SubTaskManagerService tasks=mock(SubTaskManagerService.class);
        ReflectionTestUtils.setField(service,"tasks",tasks); when(tasks.getTaskIdList()).thenReturn(Collections.singletonList(-1));
        SubTaskStatus first=new SubTaskStatus(); first.setTaskIndex(-1); first.setParentTaskId(224); first.setShardIndex(0);
        first.setDirection(TransferDirection.OSS_TO_LOCAL); first.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
        SubTaskStatus replacement=new SubTaskStatus(); replacement.setTaskIndex(-1); replacement.setParentTaskId(225); replacement.setShardIndex(4);
        replacement.setDirection(TransferDirection.LOCAL_TO_OSS); replacement.setStatus(SubTaskStatus.TaskStatus.PENDING);
        java.util.concurrent.atomic.AtomicLong version=new java.util.concurrent.atomic.AtomicLong();
        when(tasks.taskIdentityVersion(-1)).thenAnswer(call -> version.get());
        when(tasks.getTaskStatus(-1)).thenAnswer(call -> { if (version.compareAndSet(0,1)) return first; return replacement; });
        service.query(224,0,25,null);
        assertEquals(1,((List)service.query(225,0,25,null).get("items")).size());
    }
    @Test public void workerOssPointLookupDoesNotScanLocalOrUnrelatedTasks() {
        WorkerTaskChildrenService service = new WorkerTaskChildrenService(); SubTaskManagerService tasks=mock(SubTaskManagerService.class);
        ReflectionTestUtils.setField(service,"tasks",tasks);
        SubTaskStatus value=new SubTaskStatus(); value.setTaskIndex(71); value.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
        when(tasks.getTaskStatus(71)).thenReturn(value);
        assertEquals(1,((List)service.query(224,0,25,Collections.singletonList(71)).get("items")).size());
        verify(tasks,never()).getTaskIdList(); verify(tasks,times(1)).getTaskStatus(71);
    }
}
