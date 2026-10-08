package com.transfer.tools.executor;

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

public class InventoryResumeTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService db;
    private InventoryManifestService manifests;
    private ListExecutionService scanner;
    private BufferQueueManager queue;
    private S3ClientManager s3;
    private SubTaskManagerService tasks;
    private TaskProgressCache progress;
    private FileReportLedgerService ledger;
    private LocalTaskControlService control;
    @Before public void setup() throws Exception {
        db=new RocksDBService(); ReflectionTestUtils.setField(db,"dbPath",temporary.newFolder("db").getAbsolutePath());db.init();
        manifests=new InventoryManifestService();ReflectionTestUtils.setField(manifests,"rocksDBService",db);
        scanner=new ListExecutionService();queue=mock(BufferQueueManager.class);s3=mock(S3ClientManager.class);
        tasks=mock(SubTaskManagerService.class);progress=mock(TaskProgressCache.class);ledger=mock(FileReportLedgerService.class);control=new LocalTaskControlService();
        ReflectionTestUtils.setField(scanner,"inventoryManifestService",manifests);ReflectionTestUtils.setField(scanner,"bufferQueueManager",queue);
        ReflectionTestUtils.setField(scanner,"s3ClientManager",s3);ReflectionTestUtils.setField(scanner,"subTaskManagerService",tasks);
        ReflectionTestUtils.setField(scanner,"taskProgressCache",progress);ReflectionTestUtils.setField(scanner,"fileReportLedgerService",ledger);
        ReflectionTestUtils.setField(scanner,"localTaskControlService",control);
    }
    @After public void close(){db.destroy();Thread.interrupted();}
    private SubTaskStatus task(String manifest) {
        SubTaskStatus t=new SubTaskStatus();t.setTaskIndex(41);t.setStatus(SubTaskStatus.TaskStatus.PENDING);t.setAttempt(3);
        t.setDirection(TransferDirection.OSS_TO_OSS);t.setSourceBucket("source");t.setDestBucket("target");
        t.setStartKey("input.incr");t.setInventoryManifestId(manifest);return t;
    }
    @Test public void restartReadsOnlySnapshottedKeysAndCountsBeforeQueueConsumption() throws Exception {
        Path file=temporary.newFile("source.incr").toPath();
        StringBuilder text=new StringBuilder();for(int i=0;i<1001;i++)text.append("key/").append(i).append('\n');
        text.append("key/0\n");Files.write(file,text.toString().getBytes(StandardCharsets.UTF_8));
        String id=manifests.create(file);Files.delete(file);db.destroy();db.init();
        SubTaskStatus task=task(id); List<String> keys=new ArrayList<>();
        doAnswer(call->{ObjectListMessage m=call.getArgument(0);assertEquals(Integer.valueOf(3),m.getExecutionAttempt());
            if(!Boolean.TRUE.equals(m.getIsLastMessage())) {keys.add(m.getKey());assertEquals(Boolean.TRUE,m.getSourceSizeUnknown());}
            return null;}).when(queue).putPutMessage(any());
        ReflectionTestUtils.invokeMethod(scanner,"executeListTask",task);
        assertEquals(1001,keys.size());assertEquals(1001,new HashSet<>(keys).size());
        verifyZeroInteractions(s3);verify(progress,times(1001)).addToProgress(41,1,0L,0,0L,0,0L,null);
        verify(ledger).scanCompleted(task);verify(progress).updateTaskStatus(41,SubTaskStatus.TaskStatus.LIST_DONE);
    }
    @Test public void missingSnapshotAndLegacyInventoryNeverFallBackToBucketListing() throws Exception {
        ReflectionTestUtils.invokeMethod(scanner,"executeListTask",task(UUID.randomUUID().toString()));
        verify(tasks).failTask(41);verifyZeroInteractions(s3);verify(queue,never()).putPutMessage(any());
        reset(tasks);ReflectionTestUtils.invokeMethod(scanner,"executeListTask",task(null));
        verify(tasks).failTask(41);verifyZeroInteractions(s3);verify(queue,never()).putPutMessage(any());
    }
    @Test public void stopDuringInventoryScanDoesNotQueueRemainingKeysOrMarkFailed() throws Exception {
        Path file=temporary.newFile("stop.incr").toPath();Files.write(file,"one\ntwo\nthree\n".getBytes(StandardCharsets.UTF_8));
        doAnswer(call->{control.stop(41);throw new InterruptedException("stopped");}).when(queue).putPutMessage(any());
        ReflectionTestUtils.invokeMethod(scanner,"executeListTask",task(manifests.create(file)));
        verify(queue,times(1)).putPutMessage(any());verify(tasks,never()).failTask(any());verify(ledger,never()).scanCompleted(any());
        assertFalse(Thread.currentThread().isInterrupted());verifyZeroInteractions(s3);
    }
}
