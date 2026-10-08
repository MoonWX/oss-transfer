package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpServer;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.ExploreExecutorService;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.service.*;
import org.junit.*;
import org.springframework.test.util.ReflectionTestUtils;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.mockito.ArgumentMatchers.*;

public class MasterOssTaskGroupTest {
    private MasterController controller;
    private TaskCreationService creations;
    private HttpServer worker;
    private final ObjectMapper mapper = new ObjectMapper();
    private List<Map<String,Object>> rows;
    private final Map<String,List<Integer>> writes = new LinkedHashMap<>();
    private final Map<String,Object> selected = Collections.singletonMap("taskIds", Collections.singletonList(101));

    @Before public void setup() throws Exception {
        rows = new ArrayList<>(Arrays.asList(row(101,"PENDING"),row(102,"PENDING"),row(999,"PENDING")));
        worker = HttpServer.create(new InetSocketAddress("127.0.0.1",0),0);
        worker.createContext("/api/worker/", exchange -> {
            String path=exchange.getRequestURI().getPath(); Object response;
            if ("GET".equals(exchange.getRequestMethod())) response=Collections.singletonMap("tasks",rows);
            else {
                Map body=mapper.readValue(exchange.getRequestBody(),Map.class);
                List<Integer> ids=(List<Integer>)body.get("taskIds"); writes.put(path,ids);
                Map<String,Object> ack=new LinkedHashMap<>(); ack.put("success",true); ack.put("accepted",ids.size()); response=ack;
            }
            byte[] bytes=mapper.writeValueAsBytes(response);
            exchange.getResponseHeaders().set("Content-Type","application/json");
            exchange.sendResponseHeaders(200,bytes.length); exchange.getResponseBody().write(bytes); exchange.close();
        }); worker.start();
        DistributedConfig config=mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("127.0.0.1"));
        when(config.getPort()).thenReturn(worker.getAddress().getPort());
        creations=mock(TaskCreationService.class);
        TaskCreationService.Creation group=new TaskCreationService.Creation(); group.setId("one");
        group.setPublicTaskId(101); group.setTaskIds(Arrays.asList(101,102)); group.setStatus("COMPLETED"); group.setScanComplete(true);
        when(creations.findByTaskId(101)).thenReturn(group); when(creations.findByTaskId(102)).thenReturn(group);
        when(creations.getAll()).thenReturn(Collections.singletonList(group));
        when(creations.expandTaskIds(anyCollection())).thenReturn(Arrays.asList(101,102));
        when(creations.resumeByTaskIds(anyCollection())).thenReturn(Collections.emptyList());
        OssTaskGroupService groups=new OssTaskGroupService(); ReflectionTestUtils.setField(groups,"taskCreationService",creations);
        MasterLocalTransferService local=mock(MasterLocalTransferService.class);
        when(local.aggregateStatus(any(),any(),any(),any(),any(),any())).thenReturn(Collections.singletonMap("tasks",Collections.emptyList()));
        controller=new MasterController(); ReflectionTestUtils.setField(controller,"distributedConfig",config);
        ReflectionTestUtils.setField(controller,"taskCreationService",creations); ReflectionTestUtils.setField(controller,"ossTaskGroups",groups);
        ReflectionTestUtils.setField(controller,"masterLocalTransferService",local);
    }
    @After public void cleanup() { worker.stop(0); }
    private Map<String,Object> row(int id,String state) {
        Map<String,Object> row=new LinkedHashMap<>(); row.put("taskIndex",id); row.put("status",state);
        row.put("direction","OSS_TO_OSS"); row.put("sourceBucket","source"); row.put("destBucket","dest");
        row.put("workerNode","127.0.0.1"); row.put("scanComplete","COMPLETED".equals(state)); return row;
    }
    @Test public void onePublicStartStartsEveryPendingRangeAndNoOtherCreation() {
        assertEquals(true,controller.start(selected).get("success"));
        assertEquals(Arrays.asList(101,102),writes.get("/api/worker/start"));
    }
    @Test public void completedRangeDoesNotBlockStartingRemainingRanges() {
        rows.set(0,row(101,"COMPLETED"));
        assertEquals(true,controller.start(selected).get("success"));
        assertEquals(Collections.singletonList(102),writes.get("/api/worker/start"));
    }
    @Test public void stopFencesScanningAndStopsAllRanges() {
        assertEquals(true,controller.stopTasks(selected).get("success"));
        verify(creations).stopByTaskIds(Collections.singletonList(101));
        assertEquals(Arrays.asList(101,102),writes.get("/api/worker/stop"));
    }
    @Test public void resumeStartsPendingAndResumesStoppedWithoutReplayingCompleted() {
        rows.set(0,row(101,"STOPPED"));
        assertEquals(true,controller.resumeTasks(selected).get("success"));
        assertEquals(Collections.singletonList(101),writes.get("/api/worker/resume"));
        assertEquals(Collections.singletonList(102),writes.get("/api/worker/start"));
    }
    @Test public void missingRangeBlocksStartBeforeAnyWorkerMutation() {
        rows.remove(1);
        assertEquals(false,controller.start(selected).get("success")); assertTrue(writes.isEmpty());
    }
    @Test public void retryIgnoresSuccessfulRangesAndRetriesFailedMember() {
        rows.set(0,row(101,"COMPLETED")); rows.set(1,row(102,"FAILED"));
        assertEquals(true,controller.retryTask(selected).get("success"));
        assertEquals(Collections.singletonList(102),writes.get("/api/worker/retry-task"));
    }
    @Test public void unfinishedScanCannotStartItsFirstAcknowledgedRange() {
        doThrow(new IllegalStateException("扫描尚未完成")).when(creations).ensureReadyToStart(anyCollection());
        assertEquals(false,controller.start(selected).get("success")); assertTrue(writes.isEmpty());
    }
    @Test public void resumingInterruptedScanQueuesDiscoveryWithoutStartingExistingRanges() {
        BucketPair request=new BucketPair(); request.setCreationId("one");
        creations.findByTaskId(101).setScanComplete(false);
        when(creations.resumeByTaskIds(anyCollection())).thenReturn(Collections.singletonList(request));
        ExploreExecutorService executor=mock(ExploreExecutorService.class); when(executor.offerExploreTask(request)).thenReturn(true);
        ReflectionTestUtils.setField(controller,"exploreExecutorService",executor);
        assertEquals(true,controller.resumeTasks(selected).get("success"));
        verify(executor).offerExploreTask(request); assertTrue(writes.isEmpty());
    }
    @Test public void mixedResumeQueuesScanAndResumesCompleteGroup() {
        TaskCreationService.Creation scanning=creations.findByTaskId(101); scanning.setScanComplete(false);
        TaskCreationService.Creation ready=new TaskCreationService.Creation(); ready.setId("two");
        ready.setPublicTaskId(999); ready.setTaskIds(Collections.singletonList(999)); ready.setScanComplete(true);
        when(creations.findByTaskId(999)).thenReturn(ready);
        when(creations.expandTaskIds(anyCollection())).thenReturn(Arrays.asList(101,102,999));
        rows.set(2,row(999,"STOPPED"));
        BucketPair scan=new BucketPair(); scan.setCreationId("one");
        when(creations.resumeByTaskIds(anyCollection())).thenReturn(Collections.singletonList(scan));
        ExploreExecutorService executor=mock(ExploreExecutorService.class); when(executor.offerExploreTask(scan)).thenReturn(true);
        ReflectionTestUtils.setField(controller,"exploreExecutorService",executor);
        Map result=controller.resumeTasks(Collections.singletonMap("taskIds",Arrays.asList(101,999)));
        assertEquals(true,result.get("success")); assertEquals(1,result.get("resumedScans"));
        assertEquals(Collections.singletonList(999),writes.get("/api/worker/resume"));
        assertFalse(writes.containsKey("/api/worker/start"));
    }
    @Test public void automaticPartialScanResumesExistingStoppedRangesAndReplaysOnlyMissingPlans() {
        TaskCreationService.Creation group = creations.findByTaskId(101);
        group.setAutoStart(true); group.setScanComplete(false); group.setTaskIds(Arrays.asList(101,102,103));
        when(creations.expandTaskIds(anyCollection())).thenReturn(Arrays.asList(101,102,103));
        TaskCreationService.Dispatch pending = new TaskCreationService.Dispatch(); pending.setFirstTaskId(103);
        pending.setTasks(Collections.singletonList(new com.transfer.tools.model.ListSubTask()));
        group.setPendingDispatches(Collections.singletonList(pending));
        rows.set(0,row(101,"STOPPED")); rows.set(1,row(102,"COMPLETED"));
        BucketPair scan = new BucketPair(); scan.setCreationId("one");
        when(creations.resumeByTaskIds(anyCollection())).thenReturn(Collections.singletonList(scan));
        ExploreExecutorService executor = mock(ExploreExecutorService.class); when(executor.offerExploreTask(scan)).thenReturn(true);
        ReflectionTestUtils.setField(controller,"exploreExecutorService",executor);
        Map result = controller.resumeTasks(selected);
        assertEquals(true,result.get("success")); assertEquals(1,result.get("resumedScans"));
        assertEquals(Collections.singletonList(101),writes.get("/api/worker/resume"));
        assertFalse(writes.containsKey("/api/worker/start")); verify(executor).offerExploreTask(scan);
    }
    @Test public void fullScanQueueReportsAlreadyAcceptedScan() {
        creations.findByTaskId(101).setScanComplete(false);
        BucketPair first=new BucketPair(); first.setCreationId("one");
        BucketPair second=new BucketPair(); second.setCreationId("two");
        when(creations.resumeByTaskIds(anyCollection())).thenReturn(Arrays.asList(first,second));
        ExploreExecutorService executor=mock(ExploreExecutorService.class); when(executor.offerExploreTask(first)).thenReturn(true);
        when(executor.offerExploreTask(second)).thenReturn(false);
        ReflectionTestUtils.setField(controller,"exploreExecutorService",executor);
        Map result=controller.resumeTasks(selected);
        assertEquals(false,result.get("success")); assertEquals(true,result.get("partial"));
        assertEquals(1,result.get("accepted")); assertEquals(1,result.get("failed")); assertTrue(writes.isEmpty());
        verify(creations).fail(eq("two"),anyString());
    }
    @Test public void missingEntireGroupCannotReopenScan() {
        creations.findByTaskId(101).setScanComplete(false); rows.clear();
        Map result=controller.resumeTasks(selected); assertEquals(false,result.get("success"));
        verify(creations,never()).resumeByTaskIds(anyCollection()); assertTrue(writes.isEmpty());
    }
    @Test public void statusFiltersApplyAfterAggregation() {
        rows.set(0,row(101,"COMPLETED")); rows.set(1,row(102,"RUNNING")); rows.remove(2);
        assertEquals(0,controller.status(null,null,"COMPLETED",null).get("totalTasks"));
        Map<String,Object> result=controller.status(null,null,"RUNNING",null);
        assertEquals(1,result.get("totalTasks"));
        Map group=(Map)((List)result.get("tasks")).get(0); assertEquals(2,group.get("shardCount"));
    }
}
