package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpServer;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.ExploreExecutorService;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.model.ListSubTask;
import com.transfer.tools.service.TaskCreationService;
import org.junit.*;
import org.springframework.test.util.ReflectionTestUtils;

import java.net.InetSocketAddress;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class MasterOssStopFenceTest {
    private HttpServer worker;
    private MasterController controller;
    private TaskCreationService creations;
    private ExploreExecutorService explore;
    private TaskCreationService.Creation group;
    private final ObjectMapper mapper = new ObjectMapper();
    private final Map<String,List<Integer>> writes = new LinkedHashMap<>();
    private final List<Map<String,Object>> rows = new ArrayList<>();
    private boolean rejectResume;
    private boolean alreadyFenced;
    private final Map<String,Object> selection = Collections.singletonMap("taskIds", Collections.singletonList(101));

    @Before public void setup() throws Exception {
        worker = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        worker.createContext("/api/worker/", exchange -> {
            Object response;
            if ("GET".equals(exchange.getRequestMethod())) response = Collections.singletonMap("tasks", rows);
            else {
                Map body = mapper.readValue(exchange.getRequestBody(), Map.class);
                List<Integer> ids = (List<Integer>) body.get("taskIds");
                writes.put(exchange.getRequestURI().getPath(), ids);
                Map<String,Object> ack = new HashMap<>();
                boolean success = !rejectResume || !exchange.getRequestURI().getPath().endsWith("/resume");
                ack.put("success", success); ack.put("accepted", success && !alreadyFenced ? ids.size() : 0);
                ack.put("ignored", success && alreadyFenced ? ids.size() : 0);
                ack.put("message", success ? "ok" : "fence unavailable"); response = ack;
            }
            byte[] bytes = mapper.writeValueAsBytes(response);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, bytes.length); exchange.getResponseBody().write(bytes); exchange.close();
        });
        worker.start();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("127.0.0.1"));
        when(config.getPort()).thenReturn(worker.getAddress().getPort());
        creations = mock(TaskCreationService.class); explore = mock(ExploreExecutorService.class);
        group = new TaskCreationService.Creation(); group.setId("one"); group.setPublicTaskId(101);
        group.setTaskIds(Arrays.asList(101, 102)); group.setAutoStart(true); group.setScanComplete(false);
        TaskCreationService.Dispatch planned = new TaskCreationService.Dispatch();
        planned.setWorker("127.0.0.1"); planned.setFirstTaskId(102);
        planned.setTasks(Collections.singletonList(new ListSubTask()));
        group.setPendingDispatches(Collections.singletonList(planned));
        when(creations.findByTaskId(101)).thenReturn(group);
        when(creations.expandTaskIds(anyCollection())).thenReturn(Arrays.asList(101, 102));
        BucketPair scan = new BucketPair(); scan.setCreationId("one");
        when(creations.resumeByTaskIds(anyCollection())).thenReturn(Collections.singletonList(scan));
        when(explore.offerExploreTask(any())).thenAnswer(call -> {
            assertNotNull(writes.get("/api/worker/resume")); return true;
        });
        controller = new MasterController();
        ReflectionTestUtils.setField(controller, "distributedConfig", config);
        ReflectionTestUtils.setField(controller, "taskCreationService", creations);
        ReflectionTestUtils.setField(controller, "exploreExecutorService", explore);
        row(101, "PENDING");
    }

    @After public void close() { worker.stop(0); }
    private void row(int id, String state) {
        Map<String,Object> row = new HashMap<>(); row.put("taskIndex", id); row.put("status", state); rows.add(row);
    }

    @Test public void stopIncludesDurablePlanMissingFromWorkerSnapshot() {
        assertEquals(true, controller.stopTasks(selection).get("success"));
        verify(creations).stopByTaskIds(Collections.singletonList(101));
        assertEquals(Arrays.asList(101, 102), writes.get("/api/worker/stop"));
    }

    @Test public void stopCanFenceAPlannedCreationBeforeAnyRecordArrives() {
        rows.clear();
        Map result = controller.stopTasks(selection);
        assertEquals(true, result.get("success")); assertEquals(1, result.get("accepted"));
        assertEquals(Collections.singletonList(102), writes.get("/api/worker/stop"));
    }

    @Test public void repeatedStopAcknowledgesAnExistingFenceBeforeAnyRecordArrives() {
        rows.clear(); alreadyFenced = true;
        Map result = controller.stopTasks(selection);
        assertEquals(true, result.get("success")); assertEquals(false, result.get("partial"));
        assertEquals(0, result.get("accepted")); assertEquals(1, result.get("ignored"));
        assertEquals(Collections.singletonList(102), writes.get("/api/worker/stop"));
    }

    @Test public void resumeReleasesMissingPlanFenceBeforeQueueingScanner() {
        rows.clear(); row(101, "COMPLETED");
        Map result = controller.resumeTasks(selection);
        assertEquals(true, result.get("success")); assertEquals(1, result.get("resumedScans"));
        assertEquals(Collections.singletonList(102), writes.get("/api/worker/resume"));
        assertFalse(writes.containsKey("/api/worker/start")); verify(explore).offerExploreTask(any());
    }

    @Test public void firstPendingDispatchCanResumeEvenWhenNoWorkerRecordHasArrived() {
        rows.clear();
        TaskCreationService.Dispatch pending = group.getPendingDispatches().get(0);
        pending.setFirstTaskId(101); pending.setTasks(Arrays.asList(new ListSubTask(), new ListSubTask()));
        assertEquals(true, controller.stopTasks(selection).get("success"));
        assertEquals(Arrays.asList(101, 102), writes.get("/api/worker/stop"));
        Map result = controller.resumeTasks(selection);
        assertEquals(true, result.get("success")); assertEquals(1, result.get("resumedScans"));
        assertEquals(Arrays.asList(101, 102), writes.get("/api/worker/resume"));
        verify(explore).offerExploreTask(any());
    }

    @Test public void deletedCreationDoesNotReleaseItsPendingStopFence() {
        group.setDeleted(true);
        assertEquals(false, controller.resumeTasks(selection).get("success"));
        assertTrue(writes.isEmpty()); verify(creations, never()).resumeByTaskIds(anyCollection());
    }

    @Test public void rejectedFenceReleaseDoesNotQueueScannerOrClaimSuccess() {
        rejectResume = true;
        Map result = controller.resumeTasks(selection);
        assertEquals(false, result.get("success")); assertEquals(true, result.get("partial"));
        verify(creations, never()).resumeByTaskIds(anyCollection());
        verify(explore, never()).offerExploreTask(any());
        assertFalse(writes.containsKey("/api/worker/start"));
    }
}
