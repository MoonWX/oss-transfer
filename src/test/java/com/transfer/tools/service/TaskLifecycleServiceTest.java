package com.transfer.tools.service;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.SubTaskStatus;
import org.junit.*;
import org.springframework.http.*;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.mockito.ArgumentMatchers.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;
public class TaskLifecycleServiceTest {
    private TaskLifecycleService service;
    private LocalParentTaskService parents;
    private MasterLocalTransferService local;
    private MockRestServiceServer server;
    private TaskCreationService installOssGroup() {
        TaskCreationService creations=mock(TaskCreationService.class);
        TaskCreationService.Creation group=new TaskCreationService.Creation(); group.setId("oss-creation");
        group.setPublicTaskId(101); group.setTaskIds(Arrays.asList(101,102)); group.setStatus("COMPLETED"); group.setScanComplete(true);
        when(creations.getAll()).thenReturn(Collections.singletonList(group));
        when(creations.findByTaskId(101)).thenReturn(group); when(creations.findByTaskId(102)).thenReturn(group);
        when(creations.expandTaskIds(anyCollection())).thenReturn(Arrays.asList(101,102));
        OssTaskGroupService groups=new OssTaskGroupService(); ReflectionTestUtils.setField(groups,"taskCreationService",creations);
        ReflectionTestUtils.setField(service,"creations",creations); ReflectionTestUtils.setField(service,"ossGroups",groups);
        return creations;
    }
    @Test public void clearingOneOssPublicIdDeletesEveryRangeAsOneLogicalTask() {
        TaskCreationService creations=installOssGroup();
        String a="[{\"taskIndex\":101,\"status\":\"PENDING\",\"createTime\":\"2026-09-23 21:00:00\"}]";
        String b="[{\"taskIndex\":102,\"status\":\"PENDING\"},{\"taskIndex\":999,\"status\":\"PENDING\"}]";
        read("worker-a","status",a);read("worker-b","status",b);
        read("worker-a","status",a);read("worker-b","status",b);
        write("worker-a","clear",101,true);write("worker-b","clear",102,true);
        Map result=service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),false);
        assertEquals(true,result.get("success"));assertEquals(1,result.get("accepted"));
        assertEquals(Collections.singletonList(101),result.get("taskIds"));
        verify(creations).stopByTaskIds(Collections.singleton(101));
        verify(creations).markDeletedByTaskIds(Collections.singleton(101));server.verify();
    }
    @Test public void runningSiblingBlocksDeletingThePublicRepresentative() {
        TaskCreationService creations=installOssGroup();
        read("worker-a","status","[{\"taskIndex\":101,\"status\":\"COMPLETED\"}]");
        read("worker-b","status","[{\"taskIndex\":102,\"status\":\"RUNNING\"}]");
        assertEquals(false,service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),false).get("success"));
        verify(creations,never()).stopByTaskIds(anyCollection());server.verify();
    }
    @Test public void recycleBinCombinesOssRangesWithoutCombiningIndependentTasks() {
        installOssGroup();
        read("worker-a","deleted-tasks","[{\"taskIndex\":101,\"status\":\"STOPPED\"},{\"taskIndex\":999,\"status\":\"STOPPED\"}]");
        read("worker-b","deleted-tasks","[{\"taskIndex\":102,\"status\":\"STOPPED\"}]");
        Map result=service.deletedTasks(); assertEquals(2,result.get("totalCount"));
        List rows=(List)((Map)result.get("deletedTasks")).get("worker-a");
        Map group=(Map)rows.stream().filter(item -> item instanceof Map).findFirst().get();
        assertEquals(101,group.get("taskIndex"));assertEquals(Arrays.asList(101,102),group.get("childTaskIds"));server.verify();
    }
    @Test public void emptyRecycleBinRetainsEverySuccessfulWorker() {
        installOssGroup();
        read("worker-a","deleted-tasks","[]"); read("worker-b","deleted-tasks","[]");
        Map result=service.deletedTasks();
        Map rows=(Map)result.get("deletedTasks");
        assertEquals(new LinkedHashSet<>(Arrays.asList("worker-a","worker-b")),rows.keySet());
        assertEquals(Collections.emptyList(),rows.get("worker-a"));
        assertEquals(Collections.emptyList(),rows.get("worker-b"));
        assertEquals(0,result.get("totalCount")); assertEquals(true,result.get("complete"));
        assertEquals(false,result.get("partial")); assertEquals(Collections.emptyList(),result.get("failedWorkers"));
        assertEquals(Arrays.asList("worker-a","worker-b"),result.get("queriedWorkers"));
        assertEquals(Arrays.asList("worker-a","worker-b"),result.get("successfulWorkers"));
    }
    @Test public void recycleBinGroupingRetainsAnEmptySiblingWorker() {
        installOssGroup();
        read("worker-a","deleted-tasks","[{\"taskIndex\":101,\"status\":\"STOPPED\"},{\"taskIndex\":102,\"status\":\"STOPPED\"}]");
        read("worker-b","deleted-tasks","[]");
        Map result=service.deletedTasks();
        Map rows=(Map)result.get("deletedTasks");
        List localRows=(List)rows.get("worker-a");
        assertEquals(1,localRows.size());
        assertEquals(Arrays.asList(101,102),((Map)localRows.get(0)).get("childTaskIds"));
        assertEquals(Collections.emptyList(),rows.get("worker-b"));
        assertEquals(1,result.get("totalCount")); assertEquals(true,result.get("complete"));
    }
    @Test public void unavailableRecycleBinWorkerIsNotReportedAsEmpty() {
        installOssGroup();
        read("worker-a","deleted-tasks","[]"); failedRecycleRead("worker-b");
        Map result=service.deletedTasks();
        Map rows=(Map)result.get("deletedTasks");
        assertEquals(Collections.singleton("worker-a"),rows.keySet());
        assertEquals(Collections.emptyList(),rows.get("worker-a"));
        assertEquals(false,result.get("complete")); assertEquals(true,result.get("partial"));
        assertEquals(Collections.singletonList("worker-b"),result.get("failedWorkers"));
        assertEquals(Arrays.asList("worker-a","worker-b"),result.get("queriedWorkers"));
        assertEquals(Collections.singletonList("worker-a"),result.get("successfulWorkers"));
    }
    @Test public void unavailableLocalWorkerIsNotMaskedByAnEmptyParentList() {
        installOssGroup();
        failedRecycleRead("worker-a"); read("worker-b","deleted-tasks","[]");
        Map result=service.deletedTasks();
        Map rows=(Map)result.get("deletedTasks");
        assertEquals(Collections.singleton("worker-b"),rows.keySet());
        assertEquals(Collections.emptyList(),rows.get("worker-b"));
        assertEquals(false,result.get("complete")); assertEquals(true,result.get("partial"));
        assertEquals(Collections.singletonList("worker-a"),result.get("failedWorkers"));
    }
    @Test public void unavailableLocalWorkerStillRetainsKnownDeletedParentTasks() {
        installOssGroup();
        failedRecycleRead("worker-a"); read("worker-b","deleted-tasks","[]");
        when(parents.getDeleted()).thenReturn(Collections.singletonList(parent("STOPPED")));
        Map result=service.deletedTasks();
        Map rows=(Map)result.get("deletedTasks");
        List localRows=(List)rows.get("worker-a");
        assertEquals(1,localRows.size());
        assertEquals(Integer.valueOf(100),((SubTaskStatus)localRows.get(0)).getTaskIndex());
        assertEquals(Collections.emptyList(),rows.get("worker-b")); assertEquals(1,result.get("totalCount"));
        assertEquals(false,result.get("complete")); assertEquals(true,result.get("partial"));
        assertEquals(Collections.singletonList("worker-a"),result.get("failedWorkers"));
    }
    private void failedRecycleRead(String worker) {
        server.expect(requestTo("http://"+worker+":1685/api/worker/deleted-tasks"))
                .andExpect(method(HttpMethod.GET)).andRespond(withStatus(HttpStatus.SERVICE_UNAVAILABLE));
    }
    @Before public void setup() {
        service=new TaskLifecycleService(); parents=mock(LocalParentTaskService.class); local=mock(MasterLocalTransferService.class);
        DistributedConfig config=mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Arrays.asList("worker-a","worker-b")); when(config.getLocalIp()).thenReturn("worker-a"); when(config.getPort()).thenReturn(1685);
        ReflectionTestUtils.setField(service,"distributedConfig",config); ReflectionTestUtils.setField(service,"parents",parents); ReflectionTestUtils.setField(service,"localTransfers",local);
        server=MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(service,"http")).build();
        when(parents.getDeleted()).thenReturn(Collections.emptyList());
        when(local.control(eq("stop"),anyMap())).thenReturn(Collections.singletonMap("failed",0));
        when(local.aggregateStatus(any(),any(),any(),any(),any(),any())).thenReturn(Collections.singletonMap("tasks",Collections.singletonList(parent("STOPPED"))));
    }
    private SubTaskStatus parent(String state) { SubTaskStatus p=new SubTaskStatus();p.setTaskIndex(100);p.setParentTaskId(100);p.setStatus(SubTaskStatus.TaskStatus.valueOf(state));return p; }
    private Map<String,Object> selected() { return Collections.singletonMap("taskIds",Collections.singletonList(100)); }
    private void read(String worker, String route, String tasks) { server.expect(requestTo("http://"+worker+":1685/api/worker/"+route)).andExpect(method(HttpMethod.GET)).andRespond(withSuccess("{\"success\":true,\"tasks\":"+tasks+"}",MediaType.APPLICATION_JSON)); }
    private void write(String worker,String route,int id, boolean ok) { server.expect(requestTo("http://"+worker+":1685/api/worker/"+route)).andExpect(method(HttpMethod.POST)).andExpect(content().json("{\"taskIds\":["+id+"]}"))
        .andRespond(withSuccess("{\"success\":"+ok+",\"message\":\"result\"}",MediaType.APPLICATION_JSON)); }
    @After public void verifyRequests() { server.verify(); }
    @Test public void localParentDeletionTargetsItsNegativeShardsOnEveryWorker() {
        read("worker-a","status","[{\"taskIndex\":-1,\"parentTaskId\":100,\"status\":\"STOPPED\"}]");
        read("worker-b","status","[{\"taskIndex\":-2,\"parentTaskId\":100,\"status\":\"COMPLETED\"}]");
        read("worker-a","status","[{\"taskIndex\":-1,\"parentTaskId\":100,\"status\":\"STOPPED\"}]");
        read("worker-b","status","[{\"taskIndex\":-2,\"parentTaskId\":100,\"status\":\"COMPLETED\"}]");
        write("worker-a","clear",-1,true);write("worker-b","clear",-2,true);
        assertEquals(true,service.clear(selected(),false).get("success")); verify(parents).softDelete(eq(100),any(SubTaskStatus.class));
    }
    @Test public void unreachableNodePreventsAnyDeletion() {
        read("worker-a","status","[]"); server.expect(requestTo("http://worker-b:1685/api/worker/status")).andRespond(withStatus(HttpStatus.SERVICE_UNAVAILABLE));
        Map result=service.clear(selected(),false);assertEquals(false,result.get("success"));assertEquals(true,result.get("partial"));verify(parents,never()).softDelete(anyInt(),any());
    }
    @Test public void deletionFencesPendingDiscoveryAndIncludesLateShards() {
        read("worker-a","status","[]");read("worker-b","status","[]");
        when(local.aggregateStatus(any(),any(),any(),any(),any(),any())).thenReturn(Collections.singletonMap("tasks",Collections.singletonList(parent("PENDING"))));
        read("worker-a","status","[]");read("worker-b","status","[{\"taskIndex\":-3,\"parentTaskId\":100,\"status\":\"STOPPED\"}]");
        write("worker-b","clear",-3,true);
        assertEquals(true,service.clear(selected(),false).get("success"));
        org.mockito.InOrder order=inOrder(local,parents);
        order.verify(local).control(eq("stop"),eq(selected()));
        order.verify(parents).softDelete(eq(100),any(SubTaskStatus.class));
    }
    @Test public void failedDiscoveryStopRetainsAllTaskRecords() {
        read("worker-a","status","[]");read("worker-b","status","[]");
        when(local.control(eq("stop"),anyMap())).thenReturn(Collections.singletonMap("failed",1));
        assertEquals(false,service.clear(selected(),false).get("success"));
        verify(parents,never()).softDelete(anyInt(),any());
    }
    @Test public void displayStatusDatesAndDurationDoNotPreventDeletion() {
        read("worker-a","status","[{\"taskIndex\":9,\"status\":\"COMPLETED\",\"createTime\":\"2026-09-22 20:00:00\",\"startTime\":\"2026-09-22 20:01:00\",\"endTime\":\"2026-09-22 20:02:00\",\"duration\":\"1m 0s\"}]");
        read("worker-b","status","[]");write("worker-a","clear",9,true);
        assertEquals(true,service.clear(Collections.singletonMap("taskIds",Collections.singletonList(9)),false).get("success"));
    }
    @Test public void runningShardCannotBeDeletedEvenIfParentSaysStopped() {
        read("worker-a","status","[{\"taskIndex\":-1,\"parentTaskId\":100,\"status\":\"RUNNING\"}]");read("worker-b","status","[]");
        read("worker-a","status","[{\"taskIndex\":-1,\"parentTaskId\":100,\"status\":\"RUNNING\"}]");read("worker-b","status","[]");
        assertEquals(false,service.clear(selected(),false).get("success"));verify(parents,never()).softDelete(anyInt(),any());
    }
    @Test public void recycleBinShowsOneParentAndHidesItsInternalShards() {
        read("worker-a","deleted-tasks","[{\"taskIndex\":-1,\"parentTaskId\":100},{\"taskIndex\":9}]");read("worker-b","deleted-tasks","[{\"taskIndex\":-2,\"parentTaskId\":100}]");
        when(parents.getDeleted()).thenReturn(Collections.singletonList(parent("STOPPED")));
        Map result=service.deletedTasks(); assertEquals(2,result.get("totalCount"));
        List<SubTaskStatus> rows=(List<SubTaskStatus>)((Map)result.get("deletedTasks")).get("worker-a");
        assertEquals(Integer.valueOf(9),rows.get(0).getTaskIndex());assertEquals(Integer.valueOf(100),rows.get(1).getTaskIndex());
    }
    @Test public void failedPermanentDeletionRetainsTheParentForRetry() {
        read("worker-a","status","[{\"taskIndex\":-1,\"parentTaskId\":100,\"status\":\"STOPPED\"}]");read("worker-a","deleted-tasks","[]");read("worker-b","status","[]");read("worker-b","deleted-tasks","[]");
        when(parents.getDeleted()).thenReturn(Collections.singletonList(parent("STOPPED")));
        read("worker-a","status","[{\"taskIndex\":-1,\"parentTaskId\":100,\"status\":\"STOPPED\"}]");read("worker-b","status","[]");
        write("worker-a","clear",-1,true);write("worker-a","permanent-delete",-1,false);
        parentPurge("worker-b");
        Map result=service.clear(selected(),true);assertEquals(false,result.get("success"));assertEquals(true,result.get("partial"));verify(parents,never()).permanentDelete(anyInt());
    }
    private void parentPurge(String worker) {
        server.expect(requestTo("http://"+worker+":1685/api/worker/permanent-delete"))
                .andExpect(method(HttpMethod.POST))
                .andExpect(content().json("{\"taskIds\":[],\"parentTaskIds\":[100]}", true))
                .andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));
    }
    @Test public void permanentDeletionPurgesDiscoveryOnlyNodesBeforeDeletingMasterParent() {
        read("worker-a","status","[]"); read("worker-a","deleted-tasks","[]");
        read("worker-b","status","[]"); read("worker-b","deleted-tasks","[]");
        when(parents.getDeleted()).thenReturn(Collections.singletonList(parent("STOPPED")));
        read("worker-a","status","[]"); read("worker-b","status","[]");
        parentPurge("worker-a"); parentPurge("worker-b");
        assertEquals(true, service.clear(selected(), true).get("success"));
        verify(parents).permanentDelete(100);
    }
    @Test public void unknownRequestedParentDoesNotCreateAWorkerPurgeScope() {
        read("worker-a","status","[]"); read("worker-a","deleted-tasks","[]");
        read("worker-b","status","[]"); read("worker-b","deleted-tasks","[]");
        assertEquals(false, service.clear(selected(), true).get("success"));
        verify(parents, never()).permanentDelete(anyInt());
    }

    @Test public void unacknowledgedMissingOssRangePreventsDeletingVisibleSibling() {
        TaskCreationService creations=installOssGroup();
        TaskCreationService.Creation creation=creations.findByTaskId(101);
        TaskCreationService.Dispatch pending=new TaskCreationService.Dispatch(); pending.setWorker("worker-b");
        pending.setFirstTaskId(102); pending.setTasks(Collections.singletonList(new com.transfer.tools.model.ListSubTask()));
        creation.setPendingDispatches(Collections.singletonList(pending));
        read("worker-a","status","[{\"taskIndex\":101,\"status\":\"STOPPED\"}]");
        read("worker-b","status","[]");
        Map result=service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),false);
        assertEquals(false,result.get("success")); assertTrue(String.valueOf(result.get("message")).contains("全部执行子任务"));
        assertNull(result.get("accepted"));
        verify(creations,never()).stopByTaskIds(anyCollection());
        verify(creations,never()).markDeletedByTaskIds(anyCollection());
    }

    @Test public void duplicateOssRangeSnapshotIsPartialEvenWithoutMissingIdsAndCannotBeDeleted() {
        TaskCreationService creations=installOssGroup();
        read("worker-a","status","[{\"taskIndex\":101,\"status\":\"STOPPED\"},{\"taskIndex\":102,\"status\":\"STOPPED\"}]");
        read("worker-b","status","[{\"taskIndex\":102,\"status\":\"STOPPED\"}]");
        Map result=service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),false);
        assertEquals(false,result.get("success"));
        verify(creations,never()).markDeletedByTaskIds(anyCollection());
    }

    @Test public void disappearanceAfterScanStopPreventsAnyWorkerDeletion() {
        TaskCreationService creations=installOssGroup();
        read("worker-a","status","[{\"taskIndex\":101,\"status\":\"STOPPED\"}]");
        read("worker-b","status","[{\"taskIndex\":102,\"status\":\"STOPPED\"}]");
        read("worker-a","status","[{\"taskIndex\":101,\"status\":\"STOPPED\"}]");
        read("worker-b","status","[]");
        Map result=service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),false);
        assertEquals(false,result.get("success"));
        verify(creations).stopByTaskIds(Collections.singleton(101));
        verify(creations,never()).markDeletedByTaskIds(anyCollection());
    }

    @Test public void wholeGroupDisappearanceAfterScanStopCannotLookLikeSuccessfulDeletion() {
        TaskCreationService creations=installOssGroup();
        read("worker-a","status","[{\"taskIndex\":101,\"status\":\"STOPPED\"}]");
        read("worker-b","status","[{\"taskIndex\":102,\"status\":\"STOPPED\"}]");
        read("worker-a","status","[]"); read("worker-b","status","[]");
        Map result=service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),false);
        assertEquals(false,result.get("success"));
        verify(creations,never()).markDeletedByTaskIds(anyCollection());
    }

    @Test public void incompleteRecycledOssGroupCannotBePermanentlyDeleted() {
        TaskCreationService creations=installOssGroup();
        read("worker-a","status","[]"); read("worker-a","deleted-tasks","[{\"taskIndex\":101,\"status\":\"STOPPED\"}]");
        read("worker-b","status","[]"); read("worker-b","deleted-tasks","[]");
        Map result=service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),true);
        assertEquals(false,result.get("success"));
        verify(creations,never()).markDeletedByTaskIds(anyCollection());
    }

    @Test public void permanentDeletionIncludesLiveRemnantsOnlyWhenCombinedGroupIsComplete() {
        TaskCreationService creations=installOssGroup();
        for (int snapshot=0;snapshot<2;snapshot++) {
            read("worker-a","status","[{\"taskIndex\":101,\"status\":\"STOPPED\"}]"); read("worker-a","deleted-tasks","[]");
            read("worker-b","status","[]"); read("worker-b","deleted-tasks","[{\"taskIndex\":102,\"status\":\"STOPPED\"}]");
        }
        write("worker-a","clear",101,true); write("worker-a","permanent-delete",101,true);
        write("worker-b","permanent-delete",102,true);
        Map result=service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),true);
        assertEquals(true,result.get("success")); assertEquals(1,result.get("accepted"));
        verify(creations).markDeletedByTaskIds(Collections.singleton(101));
    }

    @Test public void wholeDeletionChainHoldsCreationLockAndPersistsFenceBeforeWorkerWrites() {
        TaskCreationService creations=installOssGroup();
        java.util.concurrent.atomic.AtomicBoolean fenced=new java.util.concurrent.atomic.AtomicBoolean();
        doAnswer(call -> { assertTrue(Thread.holdsLock(creations)); fenced.set(true); return null; })
                .when(creations).markDeletedByTaskIds(Collections.singleton(101));
        for (int snapshot=0;snapshot<2;snapshot++) for (int node=0;node<2;node++) {
            int id=101+node; String worker=node==0?"worker-a":"worker-b";
            server.expect(requestTo("http://"+worker+":1685/api/worker/status"))
                    .andExpect(request -> assertTrue("preflight must serialize with resume",Thread.holdsLock(creations)))
                    .andRespond(withSuccess("{\"success\":true,\"tasks\":[{\"taskIndex\":"+id+",\"status\":\"STOPPED\"}]}",MediaType.APPLICATION_JSON));
        }
        for (String worker : Arrays.asList("worker-a","worker-b"))
            server.expect(requestTo("http://"+worker+":1685/api/worker/clear"))
                    .andExpect(request -> { assertTrue(Thread.holdsLock(creations)); assertTrue("durable fence must precede deletion",fenced.get()); })
                    .andRespond(withSuccess("{\"success\":true}",MediaType.APPLICATION_JSON));
        assertEquals(true,service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),false).get("success"));
    }

    @Test public void failedWorkerDeletionRetainsDurableFenceAndNeverClaimsGroupSuccess() {
        TaskCreationService creations=installOssGroup();
        for (int snapshot=0;snapshot<2;snapshot++) {
            read("worker-a","status","[{\"taskIndex\":101,\"status\":\"STOPPED\"}]");
            read("worker-b","status","[{\"taskIndex\":102,\"status\":\"STOPPED\"}]");
        }
        write("worker-a","clear",101,true);write("worker-b","clear",102,false);
        Map result=service.clear(Collections.singletonMap("taskIds",Collections.singletonList(101)),false);
        assertEquals(false,result.get("success")); assertEquals(true,result.get("partial")); assertEquals(0,result.get("accepted"));
        verify(creations).markDeletedByTaskIds(Collections.singleton(101));
        verify(creations,never()).resumeByTaskIds(anyCollection());
    }

}
