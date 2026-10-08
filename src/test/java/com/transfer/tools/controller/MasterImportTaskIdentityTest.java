package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpServer;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.ListExecutionService;
import com.transfer.tools.model.*;
import com.transfer.tools.service.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.net.InetSocketAddress;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

/** Master imports must share the namespace reserved by OSS discovery, across every worker. */
public class MasterImportTaskIdentityTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private final ObjectMapper mapper = new ObjectMapper();
    private final List<String> paths = Collections.synchronizedList(new ArrayList<>());
    private RocksDBService masterDb, workerDb;
    private SubTaskManagerService masterTasks, workerTasks;
    private ListExecutionService masterExecution, workerExecution;
    private MasterController master;
    private HttpServer server;
    private boolean rejectWorker;

    @Before public void setup() throws Exception {
        masterDb = database("master"); workerDb = database("worker");
        masterTasks = manager(masterDb); workerTasks = manager(workerDb);
        masterExecution = mock(ListExecutionService.class); workerExecution = mock(ListExecutionService.class);
        WorkerController worker = new WorkerController();
        ReflectionTestUtils.setField(worker, "subTaskManagerService", workerTasks);
        ReflectionTestUtils.setField(worker, "listExecutionService", workerExecution);
        ReflectionTestUtils.setField(worker, "objectMapper", mapper);
        ReflectionTestUtils.setField(worker, "distributedConfig", mock(DistributedConfig.class));
        server = HttpServer.create(new InetSocketAddress("localhost", 0), 0);
        server.createContext("/api/worker/", exchange -> {
            String path = exchange.getRequestURI().getPath(); paths.add(path);
            Map<String,Object> response;
            if (rejectWorker || !"/api/worker/tasks".equals(path)) response = Collections.singletonMap("success", false);
            else response = worker.receiveTasks(mapper.readValue(exchange.getRequestBody(), List.class));
            byte[] bytes = mapper.writeValueAsBytes(response);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, bytes.length); exchange.getResponseBody().write(bytes); exchange.close();
        }); server.start();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getLocalIp()).thenReturn("master"); when(config.getWorkerIps()).thenReturn(Arrays.asList("master", "localhost"));
        when(config.getPort()).thenReturn(server.getAddress().getPort());
        master = new MasterController();
        ReflectionTestUtils.setField(master, "distributedConfig", config);
        ReflectionTestUtils.setField(master, "subTaskManagerService", masterTasks);
        ReflectionTestUtils.setField(master, "listExecutionService", masterExecution);
        ReflectionTestUtils.setField(master, "endpointNormalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(master, "taskBandwidth", mock(MasterTaskBandwidthService.class));
        masterDb.putSync("task:id:sequence", 179); workerDb.putSync("task:id:sequence", 150);
        TaskImport existing = task("master"); existing.setTaskIndex(179); existing.setShardId("oss-existing-0");
        masterTasks.importTasksWithTaskId(Collections.singletonList(existing));
    }

    @After public void close() {
        if (server != null) server.stop(0);
        if (masterTasks != null) masterTasks.shutdownCompletionWriter();
        if (workerTasks != null) workerTasks.shutdownCompletionWriter();
        if (masterDb != null) masterDb.destroy(); if (workerDb != null) workerDb.destroy();
    }
    private RocksDBService database(String name) throws Exception {
        RocksDBService db = new RocksDBService(); ReflectionTestUtils.setField(db, "dbPath", temporary.newFolder(name).getAbsolutePath()); db.init(); return db;
    }
    private SubTaskManagerService manager(RocksDBService db) {
        SubTaskManagerService manager = new SubTaskManagerService();
        ReflectionTestUtils.setField(manager, "rocksDBService", db);
        ReflectionTestUtils.setField(manager, "endpointNormalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(manager, "fileReportLedgerService", mock(FileReportLedgerService.class));
        LocalTaskControlService control = new LocalTaskControlService(); ReflectionTestUtils.setField(control, "rocksDBService", db);
        ReflectionTestUtils.setField(manager, "localTaskControlService", control); return manager;
    }
    private TaskImport task(String node) {
        TaskImport task = new TaskImport(); task.setWorkerNode(node); task.setTaskIndex(179);
        task.setDirection(TransferDirection.OSS_TO_OSS); task.setSourceBucket("source"); task.setDestBucket("target");
        task.setSourceS3Alias("source-connection"); task.setDestS3Alias("target-connection"); task.setObjectKey("fixture.txt");
        task.setConflictPolicy(ConflictPolicy.SKIP); task.setTaskBandwidthLimitMbps(1.0);
        OssKeyMapping mapping = new OssKeyMapping(); mapping.setMode(OssKeyMapping.Mode.EXACT_KEY); mapping.setTargetKey("result-" + node + ".txt");
        task.setKeyMapping(mapping); return task;
    }

    @Test public void mixedNodeImportUsesMasterIdsAndDoesNotJoinAnExistingOssCreation() {
        TaskCreationService creations = new TaskCreationService(); ReflectionTestUtils.setField(creations, "rocksDBService", masterDb);
        String creation = creations.create(new BucketPair()); creations.beginScope(creation);
        creations.recordTaskIds(Collections.singletonList(179)); creations.complete(creation); creations.clearScope();
        Map<String,Object> result = master.importTasks(Arrays.asList(task("localhost"), task("master")));
        assertEquals(true, result.get("success")); assertEquals(2, result.get("importedCount"));
        assertEquals(Arrays.asList(180,181), result.get("taskIds"));
        assertEquals(Collections.singletonList("/api/worker/tasks"), paths);
        assertNull(workerTasks.getTaskStatus(151)); assertNull(workerTasks.getTaskStatus(179));
        assertEquals("oss-existing-0", masterTasks.getTaskStatus(179).getShardId());
        SubTaskStatus remote = workerTasks.getTaskStatus(180), local = masterTasks.getTaskStatus(181);
        assertEquals("localhost", remote.getWorkerNode()); assertEquals("master", local.getWorkerNode());
        for (SubTaskStatus saved : Arrays.asList(remote, local)) {
            assertEquals(Boolean.TRUE, saved.getAutoStart()); assertEquals(Double.valueOf(1), saved.getTaskBandwidthLimitMbps());
            assertNotNull(saved.getTaskBandwidthScopeId()); assertEquals("fixture.txt", saved.getObjectKey());
            assertEquals(ConflictPolicy.SKIP, saved.getConflictPolicy()); assertEquals(OssKeyMapping.Mode.EXACT_KEY, saved.getKeyMapping().getMode());
        }
        verify(workerExecution).startExecution(null, null, Collections.singletonList(180));
        verify(masterExecution).startExecution(null, null, Collections.singletonList(181));
        OssTaskGroupService groups = new OssTaskGroupService(); ReflectionTestUtils.setField(groups, "taskCreationService", creations);
        List<Object> rows = Arrays.asList(masterTasks.getTaskStatus(179), remote, local);
        List<Object> grouped = groups.group(rows, false, false);
        assertEquals(3, grouped.size());
        Set<Integer> visible = new HashSet<>();
        for (Object row : grouped) visible.add(((Number) mapper.convertValue(row, Map.class).get("taskIndex")).intValue());
        assertEquals(new HashSet<>(Arrays.asList(179,180,181)), visible);
        assertEquals(Integer.valueOf(182), masterTasks.allocateTaskIdRange(1));
    }

    @Test public void rejectedWorkerIdsStayReservedAndResponseListsOnlyAcceptedIds() {
        rejectWorker = true;
        Map<String,Object> result = master.importTasks(Arrays.asList(task("localhost"), task("master")));
        assertEquals(true, result.get("success")); assertEquals(true, result.get("partial"));
        assertEquals(1, result.get("importedCount")); assertEquals(Collections.singletonList(181), result.get("taskIds"));
        assertEquals(Collections.singletonList(180), result.get("unconfirmedTaskIds"));
        assertEquals(Collections.singletonList("localhost"), result.get("failedWorkers"));
        assertNull(workerTasks.getTaskStatus(180)); assertNotNull(masterTasks.getTaskStatus(181));
        rejectWorker = false;
        Map<String,Object> next = master.importTasks(Collections.singletonList(task("localhost")));
        assertEquals(1, next.get("importedCount")); assertEquals(Collections.singletonList(182), next.get("taskIds"));
        assertNotNull(workerTasks.getTaskStatus(182)); assertNull(workerTasks.getTaskStatus(180));
    }

    @Test public void localSchedulingFailureKeepsBothPersistedAndRemoteAcknowledgedIdsInResponse() {
        doThrow(new IllegalStateException("executor shutting down")).when(masterExecution).startExecution(any(), any(), anyList());
        Map<String,Object> result = master.importTasks(Arrays.asList(task("localhost"), task("master")));
        assertEquals(true, result.get("success")); assertEquals(true, result.get("partial"));
        assertEquals(2, result.get("importedCount")); assertEquals(Arrays.asList(180,181), result.get("taskIds"));
        assertEquals(Collections.emptyList(), result.get("unconfirmedTaskIds"));
        assertEquals(Collections.singletonList("master"), result.get("failedWorkers"));
        assertEquals(Collections.singletonList(181), result.get("pendingScheduleTaskIds"));
        assertNotNull(masterTasks.getTaskStatus(181)); assertNotNull(workerTasks.getTaskStatus(180));
    }

    @Test public void noWorkerAcknowledgementDoesNotClaimSuccessAndIncludesAssignedIdsForInspection() {
        rejectWorker = true;
        Map<String,Object> result = master.importTasks(Collections.singletonList(task("localhost")));
        assertEquals(false, result.get("success")); assertEquals(true, result.get("partial"));
        assertEquals(0, result.get("importedCount")); assertEquals(Collections.emptyList(), result.get("taskIds"));
        assertEquals(1, result.get("unknownCount")); assertEquals(Collections.singletonList(180), result.get("unconfirmedTaskIds"));
        assertTrue(String.valueOf(result.get("message")).contains("180"));
        assertEquals(Integer.valueOf(181), masterTasks.allocateTaskIdRange(1));
    }
}
