package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.config.LocalTransferConfig;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.io.IOException;
import java.nio.file.Paths;
import java.util.Collections;
import java.util.function.Consumer;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class LocalDiscoveryReportLedgerTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService store;
    @After public void close() { if (store != null) store.destroy(); }

    @Test public void discoveryFailureKeepsDiscoveredUnassignedFileDurableAndCoverageIncomplete() throws Exception {
        store = new RocksDBService();
        ReflectionTestUtils.setField(store, "dbPath", temporary.newFolder("ledger").getAbsolutePath());
        store.init();
        FileReportLedgerService ledger = new FileReportLedgerService();
        ReflectionTestUtils.setField(ledger, "rocksDBService", store);
        LocalTransferService service = new LocalTransferService();
        DistributedConfig distributed = new DistributedConfig(); distributed.setLocalIp("worker-a");
        LocalFileService files = mock(LocalFileService.class);
        BufferQueueManager queue = mock(BufferQueueManager.class);
        ReflectionTestUtils.setField(service, "distributedConfig", distributed);
        ReflectionTestUtils.setField(service, "normalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(service, "localFileService", files);
        ReflectionTestUtils.setField(service, "localTransferConfig", new LocalTransferConfig());
        ReflectionTestUtils.setField(service, "fileReportLedgerService", ledger);
        ReflectionTestUtils.setField(service, "rocksDBService", store);
        ReflectionTestUtils.setField(service, "controlService", mock(LocalTaskControlService.class));
        ReflectionTestUtils.setField(service, "bufferQueueManager", queue);
        LocalTransferRequest request = new LocalTransferRequest();
        request.setDirection(TransferDirection.LOCAL_TO_OSS); request.setParentTaskId(77);
        TransferEndpoint source = new TransferEndpoint(); source.setType(EndpointType.LOCAL);
        source.setNodeBindings(Collections.singletonList(new LocalNodeBinding("worker-a", "/shared")));
        TransferEndpoint destination = new TransferEndpoint(); destination.setType(EndpointType.OSS);
        destination.setS3Alias("storage"); destination.setBucket("bucket"); destination.setPrefix("destination/prefix");
        request.setSourceEndpoint(source); request.setDestinationEndpoint(destination);
        LocalDiscoveryState state = new LocalDiscoveryState(77, request, "worker-a", null, 0,
                LocalDiscoveryState.Status.PENDING, null, System.currentTimeMillis());
        doAnswer(call -> {
            Consumer<LocalFileItem> consumer = call.getArgument(2);
            consumer.accept(new LocalFileItem(Paths.get("/shared/a.bin"), "a.bin", 19L, 0L));
            // No batch has reached its dispatch threshold; an error must not erase this discovery.
            throw new IOException("directory scan interrupted");
        }).when(files).scanAfter(eq("/shared"), isNull(), any());
        ledger.discoveryRegistered(77);
        ReflectionTestUtils.invokeMethod(service, "discoverAndDispatch", state);
        assertEquals(LocalDiscoveryState.Status.FAILED, state.getStatus());
        FileReportPage page = ledger.page(77, null, 500);
        assertEquals(1, page.getItems().size());
        FileReportRecord row = page.getItems().get(0);
        assertEquals("UNPROCESSED", row.getResult());
        assertEquals("destination/prefix/a.bin", row.getLogicalKey());
        assertEquals("/shared/a.bin", row.getSourcePath());
        assertEquals("discovery", row.getShardId());
        assertFalse(page.getCoverage().isScanComplete());
        assertEquals("directory scan interrupted", page.getCoverage().getLastError());
        verify(queue, never()).offerListTask(any());
        verify(queue).putFailedKey(any(FailedKeyMessage.class));
        service.shutdownDiscovery();
    }
}
