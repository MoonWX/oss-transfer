package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.transfer.tools.config.*;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class LocalAdmissionBackpressureTest {
    @Rule public TemporaryFolder temporary=new TemporaryFolder();
    @Test public void retryAfterPersistenceAndQueueRejectionEnqueuesRetainedTaskWithoutRecreatingIt() throws Exception {
        RocksDBService db=new RocksDBService();
        ReflectionTestUtils.setField(db,"dbPath",temporary.newFolder("db").getAbsolutePath());db.init();
        LocalTransferService service=new LocalTransferService();
        try {
            EndpointNormalizationService normalization=new EndpointNormalizationService();
            SubTaskManagerService tasks=new SubTaskManagerService();
            ReflectionTestUtils.setField(tasks,"rocksDBService",db);
            ReflectionTestUtils.setField(tasks,"endpointNormalizationService",normalization);
            ReflectionTestUtils.setField(tasks,"fileReportLedgerService",mock(FileReportLedgerService.class));
            BufferQueueConfig queueConfig=new BufferQueueConfig();queueConfig.getList().setSize(1);
            BufferQueueManager queue=new BufferQueueManager();ReflectionTestUtils.setField(queue,"bufferQueueConfig",queueConfig);queue.init();
            SubTaskStatus occupying=new SubTaskStatus();occupying.setTaskIndex(99);queue.offerListTask(occupying);
            DistributedConfig config=new DistributedConfig();config.setLocalIp("10.0.0.1");
            S3ClientManager clients=mock(S3ClientManager.class);when(clients.getClient("storage")).thenReturn(mock(AmazonS3.class));
            ReflectionTestUtils.setField(service,"distributedConfig",config);ReflectionTestUtils.setField(service,"normalizationService",normalization);
            ReflectionTestUtils.setField(service,"localFileService",mock(LocalFileService.class));ReflectionTestUtils.setField(service,"s3ClientManager",clients);
            ReflectionTestUtils.setField(service,"subTaskManagerService",tasks);ReflectionTestUtils.setField(service,"bufferQueueManager",queue);
            LocalTaskControlService control=new LocalTaskControlService();ReflectionTestUtils.setField(service,"controlService",control);
            LocalTransferRequest request=new LocalTransferRequest();request.setTaskId(44);request.setDirection(TransferDirection.LOCAL_TO_OSS);
            TransferEndpoint source=new TransferEndpoint();source.setType(EndpointType.LOCAL);source.setNodeBindings(Collections.singletonList(new LocalNodeBinding("10.0.0.1","/shared")));
            TransferEndpoint destination=new TransferEndpoint();destination.setType(EndpointType.OSS);destination.setS3Alias("storage");destination.setBucket("bucket");
            request.setSourceEndpoint(source);request.setDestinationEndpoint(destination);
            try {service.accept(Collections.singletonList(request));fail("queue rejection must be visible");} catch(IllegalStateException expected){assertTrue(expected.getMessage().contains("queue is full"));}
            SubTaskStatus saved=db.get("task:44",SubTaskStatus.class);assertNotNull(saved);assertEquals(SubTaskStatus.TaskStatus.PENDING,saved.getStatus());
            Long created=saved.getCreateTime();assertSame(occupying,queue.takeListTask());
            assertEquals(Collections.singletonList(44),service.accept(Collections.singletonList(request)));
            assertEquals(Integer.valueOf(44),queue.takeListTask().getTaskIndex());
            assertEquals(created,db.get("task:44",SubTaskStatus.class).getCreateTime());
            assertEquals(Collections.singletonList(44),tasks.getTaskIdList());
            control.stop(44);service.accept(Collections.singletonList(request));assertEquals(0,queue.listQueueSize());
        } finally {service.shutdownDiscovery();db.destroy();}
    }
}
