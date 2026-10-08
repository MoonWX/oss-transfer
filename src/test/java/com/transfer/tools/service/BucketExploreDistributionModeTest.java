package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.ListObjectsRequest;
import com.amazonaws.services.s3.model.ObjectListing;
import com.amazonaws.services.s3.model.S3ObjectSummary;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.SubTaskStatusVO;
import com.transfer.tools.model.TaskImport;
import com.transfer.tools.storage.RocksDBService;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.MediaType;
import org.springframework.mock.http.client.MockClientHttpRequest;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;

import java.util.Arrays;
import java.util.Collections;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo;
import static org.springframework.test.web.client.response.MockRestResponseCreators.withSuccess;

/** Preserve the selected OSS allocation mode through exploration, worker wire format and durable status. */
public class BucketExploreDistributionModeTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService db;
    private SubTaskManagerService tasks;
    private BucketExploreService explorer;
    private DistributedConfig config;
    private DistributedService distributor;

    @Before public void setup() throws Exception {
        db = new RocksDBService();
        ReflectionTestUtils.setField(db, "dbPath", temporary.newFolder("db").getAbsolutePath());
        db.init();
        tasks = new SubTaskManagerService();
        FileReportLedgerService ledger = new FileReportLedgerService();
        ReflectionTestUtils.setField(ledger, "rocksDBService", db);
        ReflectionTestUtils.setField(tasks, "rocksDBService", db);
        ReflectionTestUtils.setField(tasks, "fileReportLedgerService", ledger);
        ReflectionTestUtils.setField(tasks, "endpointNormalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(tasks, "localTaskControlService", new LocalTaskControlService());
        config = mock(DistributedConfig.class);
        when(config.getPort()).thenReturn(8081);
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("qa-worker"));
        distributor = new DistributedService();
        ReflectionTestUtils.setField(distributor, "distributedConfig", config);
        ReflectionTestUtils.setField(distributor, "subTaskManagerService", tasks);
        explorer = new BucketExploreService();
        ReflectionTestUtils.setField(explorer, "distributedConfig", config);
        ReflectionTestUtils.setField(explorer, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(explorer, "distributedService", distributor);
        ReflectionTestUtils.setField(explorer, "rateLimiterManager", mock(RateLimiterManager.class));
        AmazonS3 s3 = mock(AmazonS3.class);
        S3ClientManager clients = mock(S3ClientManager.class);
        when(clients.getListClient("source")).thenReturn(s3);
        ReflectionTestUtils.setField(explorer, "s3ClientManager", clients);
        when(s3.listObjects(any(ListObjectsRequest.class))).thenAnswer(call -> {
            ListObjectsRequest request = call.getArgument(0);
            ObjectListing result = new ObjectListing();
            if (request.getMarker() == null || "fixture/object.bin".compareTo(request.getMarker()) > 0) {
                S3ObjectSummary object = new S3ObjectSummary();
                object.setKey("fixture/object.bin"); object.setSize(17);
                result.getObjectSummaries().add(object);
            }
            return result;
        });
    }

    @After public void close() { if (db != null) db.destroy(); }

    @Test public void balancedModeSurvivesWorkerPayloadAndDatabaseReopen() throws Exception {
        verifyWorkerDispatch("balanced");
    }
    @Test public void affinityModeSurvivesWorkerPayloadAndDatabaseReopen() throws Exception {
        verifyWorkerDispatch("affinity");
    }
    @Test public void legacyMissingModeStillUsesAffinityAfterWorkerImport() throws Exception {
        verifyWorkerDispatch(null);
    }
    @Test public void balancedModeIsSavedWithoutConfiguredWorkers() {
        verifyStandalone("balanced");
    }
    @Test public void affinityModeIsSavedWithoutConfiguredWorkers() {
        verifyStandalone("affinity");
    }
    @Test public void legacyStandaloneDefaultRemainsAffinity() {
        verifyStandalone(null);
    }

    private void verifyWorkerDispatch(String requestedMode) throws Exception {
        when(config.isMaster()).thenReturn(true);
        RestTemplate client = (RestTemplate) ReflectionTestUtils.getField(distributor, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(client).build();
        server.expect(requestTo("http://qa-worker:8081/api/worker/tasks"))
                .andExpect(request -> {
                    String body = ((MockClientHttpRequest) request).getBodyAsString();
                    TaskImport[] imported = new ObjectMapper().readValue(body, TaskImport[].class);
                    assertTrue(imported.length > 0);
                    for (TaskImport task : imported) {
                        assertEquals("The worker must receive the mode selected for this creation", requestedMode,
                                task.getDistributionMode());
                        assertEquals("qa-worker", task.getWorkerNode());
                    }
                    tasks.importTasksWithTaskId(Arrays.asList(imported));
                })
                .andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));
        explore(requestedMode);
        server.verify();
        assertDurableMode(requestedMode);
    }

    private void verifyStandalone(String requestedMode) {
        when(config.isMaster()).thenReturn(false);
        when(config.getWorkerIps()).thenReturn(Collections.emptyList());
        explore(requestedMode);
        assertDurableMode(requestedMode);
    }

    private void explore(String mode) {
        explorer.exploreSingleBucketAndCreateSubTasks("source-bucket", "target-bucket", "source", "target",
                null, null, null, false, mode);
    }

    private void assertDurableMode(String requestedMode) {
        assertFalse(tasks.getTaskIdList().isEmpty());
        for (Integer id : tasks.getTaskIdList())
            assertEquals(requestedMode, db.get("task:" + id, SubTaskStatus.class).getDistributionMode());
        db.destroy(); db.init();
        for (Integer id : tasks.getTaskIdList()) {
            assertEquals(requestedMode, db.get("task:" + id, SubTaskStatus.class).getDistributionMode());
            SubTaskStatus restored = tasks.getTaskStatus(id);
            assertEquals(requestedMode == null ? "affinity" : requestedMode,
                    SubTaskStatusVO.from(restored).getDistributionMode());
            assertEquals(SubTaskStatus.TaskStatus.PENDING, restored.getStatus());
        }
    }
}
