package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.*;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.MediaType;
import org.springframework.mock.http.client.MockClientHttpRequest;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.io.IOException;
import java.nio.file.Paths;
import java.util.*;
import java.util.function.Consumer;
import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo;
import static org.springframework.test.web.client.response.MockRestResponseCreators.withSuccess;

public class LocalDiscoveryJournalTest {
    @Rule public TemporaryFolder temp = new TemporaryFolder();
    private RocksDBService store;
    private final ObjectMapper mapper = new ObjectMapper();
    private final List<LocalTransferService> services = new ArrayList<>();
    private LocalTransferRequest request;
    @Before public void setup() throws Exception {
        store = new RocksDBService(); ReflectionTestUtils.setField(store, "dbPath", temp.newFolder("journal").getAbsolutePath()); store.init();
        request = new LocalTransferRequest(); request.setParentTaskId(77); request.setDirection(TransferDirection.LOCAL_TO_OSS);
        TransferEndpoint source = new TransferEndpoint(); source.setType(EndpointType.LOCAL);
        source.setNodeBindings(Arrays.asList(new LocalNodeBinding("worker-a", "/shared"), new LocalNodeBinding("worker-b", "/shared")));
        TransferEndpoint destination = new TransferEndpoint(); destination.setType(EndpointType.OSS); destination.setBucket("bucket"); destination.setS3Alias("storage");
        request.setSourceEndpoint(source); request.setDestinationEndpoint(destination); request.setDistributionMode("affinity");
    }
    @After public void close() { for (LocalTransferService service : services) service.shutdownDiscovery(); store.destroy(); }
    @Test public void lostAckReplaysPersistedExactBatchBeforeScanningChangedDirectory() throws Exception {
        LocalFileService originalFiles = mock(LocalFileService.class);
        doAnswer(call -> { Consumer<LocalFileItem> accept = call.getArgument(2); accept.accept(item("a.bin")); accept.accept(item("b.bin")); return null; })
                .when(originalFiles).scanAfter(eq("/shared"), isNull(), any());
        LocalTransferService original = service(originalFiles);
        MockRestServiceServer first = server(original);
        first.expect(requestTo(url())).andRespond(http -> {
            LocalFileBatch sent = mapper.readValue(((MockClientHttpRequest) http).getBodyAsString(), LocalFileBatch.class);
            LocalDiscoveryState saved = store.get("local:discovery:77", LocalDiscoveryState.class);
            assertNotNull("batch must be durable before the request is sent", saved.getPendingBatch());
            assertEquals(Arrays.asList("a.bin", "b.bin"), keys(saved.getPendingBatch()));
            assertEquals(sent.getShardId(), saved.getPendingBatch().getShardId());
            throw new IOException("ack lost after worker accepted the batch");
        });
        LocalDiscoveryState initial = new LocalDiscoveryState(77, request, "worker-a", null, 0, LocalDiscoveryState.Status.PENDING, null, 0L);
        ReflectionTestUtils.invokeMethod(original, "discoverAndDispatch", initial); first.verify();
        LocalDiscoveryState recovered = store.get("local:discovery:77", LocalDiscoveryState.class);
        assertEquals(LocalDiscoveryState.Status.FAILED, recovered.getStatus());
        assertNull(recovered.getLastCommittedRelativeKey()); assertNotNull(recovered.getPendingBatch());
        LocalFileService changedFiles = mock(LocalFileService.class);
        doAnswer(call -> { Consumer<LocalFileItem> accept = call.getArgument(2); accept.accept(item("c.bin")); accept.accept(item("d.bin")); return null; })
                .when(changedFiles).scanAfter(eq("/shared"), eq("b.bin"), any());
        LocalTransferService restarted = service(changedFiles); MockRestServiceServer replay = server(restarted);
        replay.expect(requestTo(url())).andRespond(http -> {
            LocalFileBatch sent = mapper.readValue(((MockClientHttpRequest) http).getBodyAsString(), LocalFileBatch.class);
            assertEquals("77-local-0", sent.getShardId()); assertEquals(Arrays.asList("a.bin", "b.bin"), keys(sent));
            assertEquals("worker-b", sent.getNodeBinding().getExecutionNodeIp());
            return withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON).createResponse(http);
        });
        replay.expect(requestTo(url())).andRespond(http -> {
            LocalFileBatch sent = mapper.readValue(((MockClientHttpRequest) http).getBodyAsString(), LocalFileBatch.class);
            assertEquals("77-local-1", sent.getShardId()); assertEquals(Arrays.asList("c.bin", "d.bin"), keys(sent));
            return withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON).createResponse(http);
        });
        ReflectionTestUtils.invokeMethod(restarted, "discoverAndDispatch", recovered); replay.verify();
        LocalDiscoveryState done = store.get("local:discovery:77", LocalDiscoveryState.class);
        assertEquals(LocalDiscoveryState.Status.COMPLETED, done.getStatus()); assertNull(done.getPendingBatch());
        assertEquals("d.bin", done.getLastCommittedRelativeKey()); assertEquals(Integer.valueOf(2), done.getNextBatchSequence());
        verify(changedFiles).scanAfter(eq("/shared"), eq("b.bin"), any());
    }
    @Test public void acknowledgedFinalPendingBatchCompletesWithoutRescanningChangedSource() {
        LocalFileService files = mock(LocalFileService.class); LocalTransferService service = service(files);
        LocalDiscoveryState state = new LocalDiscoveryState(77, request, "worker-a", null, 0, LocalDiscoveryState.Status.RUNNING, null, 0L);
        state.setPendingBatch(new LocalFileBatch(77, "77-local-0", 0, 0, "worker-a", request,
                request.getSourceEndpoint().getNodeBindings().get(1), Collections.singletonList(new LocalFileEntry("old.bin", 1L, 0L)), true));
        MockRestServiceServer server = server(service);
        server.expect(requestTo(url())).andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));
        ReflectionTestUtils.invokeMethod(service, "discoverAndDispatch", state); server.verify();
        assertEquals(LocalDiscoveryState.Status.COMPLETED, state.getStatus()); assertNull(state.getPendingBatch());
        try { verify(files, never()).scanAfter(anyString(), any(), any()); } catch (IOException error) { throw new AssertionError(error); }
    }
    @Test public void balancedDataThresholdSplitsLargeFilesAndJournalsTheSelectedNodeBeforeDispatch() throws Exception {
        request.setDistributionMode("balanced");
        LocalFileService files = mock(LocalFileService.class);
        long mib = 1024L * 1024;
        doAnswer(call -> {
            Consumer<LocalFileItem> accept = call.getArgument(2);
            accept.accept(new LocalFileItem(Paths.get("/shared/a"), "a", 200 * mib, 0));
            accept.accept(new LocalFileItem(Paths.get("/shared/b"), "b", 100 * mib, 0));
            accept.accept(new LocalFileItem(Paths.get("/shared/c"), "c", 1024 * mib, 0));
            return null;
        }).when(files).scanAfter(eq("/shared"), isNull(), any());
        LocalTransferService service = service(files);
        LocalTransferConfig config = (LocalTransferConfig)ReflectionTestUtils.getField(service, "localTransferConfig");
        config.getBatch().setMaxItems(1000);
        BalancedWorkerScheduler scheduler = mock(BalancedWorkerScheduler.class);
        when(scheduler.select(anyList(), anyLong(), anyLong())).thenReturn("worker-b");
        ReflectionTestUtils.setField(service, "balancedWorkerScheduler", scheduler);
        MockRestServiceServer server = server(service);
        for (String key : Arrays.asList("a", "b", "c")) server.expect(requestTo(url())).andRespond(http -> {
            LocalFileBatch sent = mapper.readValue(((MockClientHttpRequest)http).getBodyAsString(), LocalFileBatch.class);
            assertEquals(Collections.singletonList(key), keys(sent));
            LocalFileBatch saved = store.get("local:discovery:77", LocalDiscoveryState.class).getPendingBatch();
            assertEquals(sent.getShardId(), saved.getShardId());
            assertEquals("worker-b", saved.getNodeBinding().getExecutionNodeIp());
            return withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON).createResponse(http);
        });
        LocalDiscoveryState state = new LocalDiscoveryState(77, request, "worker-a", null, 0, LocalDiscoveryState.Status.PENDING, null, 0L);
        ReflectionTestUtils.invokeMethod(service, "discoverAndDispatch", state); server.verify();
        assertEquals(LocalDiscoveryState.Status.COMPLETED, state.getStatus());
        verify(scheduler).select(anyList(), eq(200 * mib), eq(1L));
        verify(scheduler).select(anyList(), eq(100 * mib), eq(1L));
        verify(scheduler).select(anyList(), eq(1024 * mib), eq(1L));
    }

    @Test public void balancedPendingJournalNeverCallsSchedulerAgainAfterRestart() {
        request.setDistributionMode("balanced");
        LocalFileService files = mock(LocalFileService.class); LocalTransferService service = service(files);
        BalancedWorkerScheduler scheduler = mock(BalancedWorkerScheduler.class);
        ReflectionTestUtils.setField(service, "balancedWorkerScheduler", scheduler);
        LocalDiscoveryState state = new LocalDiscoveryState(77, request, "worker-a", null, 0, LocalDiscoveryState.Status.RUNNING, null, 0L);
        state.setPendingBatch(new LocalFileBatch(77, "77-local-0", 0, 0, "worker-a", request,
                request.getSourceEndpoint().getNodeBindings().get(1), Collections.singletonList(new LocalFileEntry("old.bin", 1L, 0L)), true));
        MockRestServiceServer server = server(service);
        server.expect(requestTo(url())).andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));
        ReflectionTestUtils.invokeMethod(service, "discoverAndDispatch", state); server.verify();
        verifyZeroInteractions(scheduler);
        assertEquals(LocalDiscoveryState.Status.COMPLETED, state.getStatus());
    }

    private LocalTransferService service(LocalFileService files) {
        LocalTransferService service = new LocalTransferService(); services.add(service);
        DistributedConfig config = new DistributedConfig(); config.setLocalIp("worker-a"); config.setPort(8081);
        LocalTransferConfig local = new LocalTransferConfig(); local.getBatch().setMaxItems(2);
        FileReportLedgerService ledger = new FileReportLedgerService(); ReflectionTestUtils.setField(ledger, "rocksDBService", store); ledger.discoveryRegistered(77);
        ReflectionTestUtils.setField(service, "distributedConfig", config); ReflectionTestUtils.setField(service, "normalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(service, "localTransferConfig", local); ReflectionTestUtils.setField(service, "localFileService", files);
        ReflectionTestUtils.setField(service, "controlService", new LocalTaskControlService()); ReflectionTestUtils.setField(service, "rocksDBService", store);
        ReflectionTestUtils.setField(service, "fileReportLedgerService", ledger); ReflectionTestUtils.setField(service, "bufferQueueManager", mock(BufferQueueManager.class));
        return service;
    }
    private MockRestServiceServer server(LocalTransferService service) { return MockRestServiceServer.bindTo((RestTemplate) ReflectionTestUtils.getField(service, "restTemplate")).build(); }
    private String url() { return "http://worker-b:8081/api/worker/local-transfers/batches"; }
    private LocalFileItem item(String key) { return new LocalFileItem(Paths.get("/shared/" + key), key, 1L, 0L); }
    private List<String> keys(LocalFileBatch batch) { List<String> result = new ArrayList<>(); for (LocalFileEntry entry : batch.getEntries()) result.add(entry.getRelativeKey()); return result; }
}
