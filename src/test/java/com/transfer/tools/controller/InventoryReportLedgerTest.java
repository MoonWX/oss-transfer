package com.transfer.tools.controller;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.*;
import com.transfer.tools.service.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import java.nio.charset.StandardCharsets;
import java.nio.file.*;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class InventoryReportLedgerTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    @Test public void importPersistsImmutableManifestAndReturnsAcceptedTaskIdsBeforeQueueing() throws Exception {
        RocksDBService db = new RocksDBService();
        ReflectionTestUtils.setField(db,"dbPath",temporary.newFolder("db").getAbsolutePath()); db.init();
        try {
            InventoryManifestService manifests = new InventoryManifestService();
            ReflectionTestUtils.setField(manifests,"rocksDBService",db);
            WorkerController controller = new WorkerController();
            FileReportLedgerService ledger = mock(FileReportLedgerService.class);
            BufferQueueManager queue = mock(BufferQueueManager.class);
            SubTaskManagerService tasks = mock(SubTaskManagerService.class);
            DistributedConfig config = new DistributedConfig(); config.setLocalIp("worker-a");
            ReflectionTestUtils.setField(controller,"distributedConfig",config);
            ReflectionTestUtils.setField(controller,"subTaskManagerService",tasks);
            ReflectionTestUtils.setField(controller,"bufferQueueManager",queue);
            ReflectionTestUtils.setField(controller,"fileReportLedgerService",ledger);
            ReflectionTestUtils.setField(controller,"inventoryManifestService",manifests);
            when(tasks.getNextTaskId()).thenReturn(41);
            Path directory = temporary.newFolder("inventory").toPath();
            Path file=directory.resolve("input.incr"); Files.write(file,"first\nsecond\nfirst\n\n".getBytes(StandardCharsets.UTF_8));
            final SubTaskStatus[] accepted = {null};
            doAnswer(call->{ accepted[0]=call.getArgument(0); manifests.requireReady(accepted[0].getInventoryManifestId()); return null; }).when(tasks).importInventoryTask(any());
            doAnswer(call->{ assertSame(accepted[0],call.getArgument(0)); return null; }).when(queue).offerListTask(any());
            Map<String,Object> params = new HashMap<>(); params.put("srcAliasBucket","src/bucket");params.put("destAliasBucket","dst/bucket");params.put("inventoryDir",directory.toString());
            Map<String,Object> result=controller.importInventory(params);
            assertEquals(true,result.get("success"));assertEquals(Collections.singletonList(41),result.get("taskIds"));
            assertEquals(SubTaskStatus.TaskStatus.PENDING,accepted[0].getStatus());assertEquals(Integer.valueOf(1),accepted[0].getAttempt());
            Files.delete(file);
            assertEquals(2,manifests.page(accepted[0].getInventoryManifestId(),null).getItems().size());
            verify(ledger).registerTask(accepted[0]); verify(queue,never()).putPutMessage(any());
        } finally { db.destroy(); }
    }
}
