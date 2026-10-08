package com.transfer.tools.executor;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.*;
import com.transfer.tools.config.*;
import com.transfer.tools.model.*;
import com.transfer.tools.service.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

/** Exercise exploration, persisted task endpoints, real scanning and durable report rows together. */
public class PrefixRangeExecutionTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService db;
    private SubTaskManagerService tasks;
    private FileReportLedgerService ledger;
    private BucketExploreService explorer;
    private ListExecutionService scanner;
    private LocalTaskControlService control;
    private final SortedMap<String, Long> source = new TreeMap<>();
    private final List<String> delivered = new ArrayList<>();
    private TaskProgressCache progress;
    private AmazonS3 s3;
    private final List<ObjectListMessage> messages = new ArrayList<>();

    @Before public void setup() throws Exception {
        db = new RocksDBService(); ReflectionTestUtils.setField(db, "dbPath", temporary.newFolder("db").getAbsolutePath()); db.init();
        ledger = new FileReportLedgerService(); ReflectionTestUtils.setField(ledger, "rocksDBService", db);
        control = new LocalTaskControlService(); tasks = new SubTaskManagerService();
        ReflectionTestUtils.setField(tasks, "rocksDBService", db);
        ReflectionTestUtils.setField(tasks, "fileReportLedgerService", ledger);
        ReflectionTestUtils.setField(tasks, "endpointNormalizationService", new EndpointNormalizationService());
        ReflectionTestUtils.setField(tasks, "localTaskControlService", control);
        s3 = mock(AmazonS3.class); S3ClientManager clients = mock(S3ClientManager.class);
        when(clients.getListClient("source")).thenReturn(s3);
        when(clients.getClient("source")).thenReturn(s3);
        when(s3.getObjectMetadata(eq("bucket"), anyString())).thenAnswer(call -> {
            String key = call.getArgument(1);
            if (!source.containsKey(key)) { AmazonS3Exception missing = new AmazonS3Exception("missing"); missing.setStatusCode(404); throw missing; }
            ObjectMetadata metadata = new ObjectMetadata(); metadata.setContentLength(source.get(key)); metadata.setLastModified(new Date(1700000000000L)); return metadata;
        });
        when(s3.listObjects(any(ListObjectsRequest.class))).thenAnswer(call -> {
            ListObjectsRequest request = call.getArgument(0); ObjectListing result = new ObjectListing();
            String prefix = request.getPrefix() == null ? "" : request.getPrefix();
            String marker = request.getMarker() == null ? "" : request.getMarker();
            for (Map.Entry<String, Long> entry : source.entrySet()) {
                if (!entry.getKey().startsWith(prefix) || entry.getKey().compareTo(marker) <= 0) continue;
                if (result.getObjectSummaries().size() >= Math.min(3, request.getMaxKeys())) { result.setTruncated(true); break; }
                S3ObjectSummary item = new S3ObjectSummary(); item.setBucketName("bucket"); item.setKey(entry.getKey()); item.setSize(entry.getValue());
                result.getObjectSummaries().add(item);
            }
            if (result.isTruncated()) result.setNextMarker(result.getObjectSummaries().get(result.getObjectSummaries().size() - 1).getKey());
            return result;
        });
        explorer = new BucketExploreService();
        ReflectionTestUtils.setField(explorer, "distributedConfig", mock(DistributedConfig.class));
        ReflectionTestUtils.setField(explorer, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(explorer, "rateLimiterManager", mock(RateLimiterManager.class));
        ReflectionTestUtils.setField(explorer, "s3ClientManager", clients);
        scanner = new ListExecutionService(); progress = mock(TaskProgressCache.class);
        ReflectionTestUtils.setField(scanner, "s3ClientManager", clients);
        ReflectionTestUtils.setField(scanner, "subTaskManagerService", tasks);
        ReflectionTestUtils.setField(scanner, "rateLimiterManager", mock(RateLimiterManager.class));
        ReflectionTestUtils.setField(scanner, "taskProgressCache", progress);
        ReflectionTestUtils.setField(scanner, "fileReportLedgerService", ledger);
        ReflectionTestUtils.setField(scanner, "localTaskControlService", control);
        SourceBucketConfig sourceConfig = new SourceBucketConfig(); SourceBucketConfig.ListConfig list = new SourceBucketConfig.ListConfig();
        list.setMaxKeys(2); sourceConfig.setList(list); ReflectionTestUtils.setField(scanner, "sourceBucketConfig", sourceConfig);
        BufferQueueManager queue = mock(BufferQueueManager.class);
        doAnswer(call -> {
            ObjectListMessage message = call.getArgument(0);
            if (!Boolean.TRUE.equals(message.getIsLastMessage())) {
                messages.add(message);
                delivered.add(message.getObjectSummary().getKey());
                SubTaskStatus task = tasks.getTaskStatus(message.getTaskIndex());
                ledger.skipped(task, message.getObjectSummary().getKey(), message.getObjectSummary().getSize(), null, "DESTINATION_EXISTS");
            }
            return null;
        }).when(queue).putPutMessage(any());
        ReflectionTestUtils.setField(scanner, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(tasks, "bufferQueueManager", queue);
        ReflectionTestUtils.setField(tasks, "taskProgressCache", progress);
    }
    @After public void close() { db.destroy(); Thread.interrupted(); }

    @Test public void exactEmptyObjectIsScannedAndIncludedInReport() {
        verifyExact("fixtures/000-empty.bin", 0L, false);
    }
    @Test public void exactSpaceKeySurvivesPendingStopAndResume() {
        verifyExact("fixtures/001 with spaces.txt", 65536L, true);
    }
    @Test public void exactChineseKeyIsScannedAndIncludedInReport() {
        verifyExact("fixtures/中文.txt", 17L, false);
    }
    private void verifyExact(String key, long size, boolean stopBeforeStart) {
        source.put("aaa/outside", 9L); source.put(key, size); source.put("zzz/outside", 11L);
        explore(key); assertEquals(1, tasks.getTaskIdList().size());
        Integer id = tasks.getTaskIdList().get(0); SubTaskStatus task = tasks.getTaskStatus(id);
        assertEquals("", task.getStartKey()); assertEquals(key, task.getEndKey());
        assertEquals(Boolean.TRUE, task.getSourceEndpoint().getPrefixFilterEnabled());
        if (stopBeforeStart) {
            control.stop(id); ReflectionTestUtils.invokeMethod(scanner, "executeListTask", task);
            assertTrue(delivered.isEmpty()); control.resume(id);
        }
        ReflectionTestUtils.invokeMethod(scanner, "executeListTask", tasks.getTaskStatus(id));
        assertEquals(Collections.singletonList(key), delivered);
        verify(progress).addToProgress(id, 1, size, 0, 0L, 0, 0L, null);
        FileReportPage report = ledger.page(id, null, 1000);
        assertEquals(1, report.getItems().size()); assertEquals(key, report.getItems().get(0).getLogicalKey());
        assertEquals(Long.valueOf(size), report.getItems().get(0).getSize()); assertTrue(report.getCoverage().isScanComplete());
    }
    @Test public void multipleRangesAndPagesDeliverPrefixObjectAndEveryChildExactlyOnce() {
        source.put("aaa/outside", 9L); source.put("prefix/", 0L);
        // Exceed the measured-range limit as well as both mocked LIST page limits.
        // Short S3 pages alone no longer create a new balanced range.
        int children = BalancedWorkerScheduler.MAX_ITEMS * 2 + 3;
        for (int i = 0; i < children; i++) source.put(String.format("prefix/%04d.bin", i), (long) i);
        Set<String> expected = new TreeSet<>(source.tailMap("prefix/").keySet());
        source.put("zzz/outside", 11L); explore("prefix/");
        assertTrue(tasks.getTaskIdList().size() > 1);
        for (Integer id : tasks.getTaskIdList()) ReflectionTestUtils.invokeMethod(scanner, "executeListTask", tasks.getTaskStatus(id));
        assertEquals(children + 1, delivered.size()); assertEquals(expected, new TreeSet<>(delivered));
        assertTrue(delivered.contains("prefix/")); assertFalse(delivered.contains("aaa/outside")); assertFalse(delivered.contains("zzz/outside"));
        int reportRows = 0;
        Set<String> reported = new TreeSet<>();
        for (Integer id : tasks.getTaskIdList()) {
            FileReportPage page = ledger.page(id, null, 1000); reportRows += page.getItems().size(); assertTrue(page.getCoverage().isScanComplete());
            for (FileReportRecord item : page.getItems()) assertTrue("duplicate report key " + item.getLogicalKey(), reported.add(item.getLogicalKey()));
        }
        assertEquals(children + 1, reportRows); assertEquals(expected, reported);
    }
    @Test public void legacyRangeStillUsesExclusiveMarkerAndIgnoresInferredEndpointPrefix() {
        source.put("range/a", 1L); source.put("range/b", 2L); source.put("range/c", 3L); source.put("range/d", 4L);
        ListSubTask legacy = new ListSubTask("bucket", "target", "source", "target", "range/a", "range/c", null, null, false);
        tasks.saveSubTasks(Collections.singletonList(legacy));
        SubTaskStatus task = tasks.getTaskStatus(tasks.getTaskIdList().get(0));
        assertEquals("range/a", task.getSourceEndpoint().getPrefix()); assertNull(task.getSourceEndpoint().getPrefixFilterEnabled());
        ReflectionTestUtils.invokeMethod(scanner, "executeListTask", task);
        assertEquals(Arrays.asList("range/b", "range/c"), delivered);
    }
    private void explore(String prefix) {
        explorer.exploreSingleBucketAndCreateSubTasks("bucket", "target", "source", "target", null, null, prefix, false, "balanced");
    }

    @Test public void singleObjectUsesHeadNotPrefixListingAndPersistsExactMapping() {
        String key = "fixtures/空 文件.incr";
        source.put(key, 0L); source.put(key + "/child", 77L);
        BucketPair pair = pair(); pair.setObjectKey(key);
        OssKeyMapping mapping = new OssKeyMapping(); mapping.setMode(OssKeyMapping.Mode.EXACT_KEY); mapping.setTargetKey(" renamed//目标 "); pair.setKeyMapping(mapping);
        explorer.exploreSingleBucketAndCreateSubTasks(pair);
        Integer id = tasks.getTaskIdList().get(0); SubTaskStatus task = tasks.getTaskStatus(id);
        assertEquals(key, SubTaskStatusVO.from(task).getObjectKey()); assertEquals(mapping, SubTaskStatusVO.from(task).getKeyMapping());
        tasks.stopTask(id); assertTrue(tasks.resumeTask(id));
        assertEquals(mapping, tasks.getTaskStatus(id).getKeyMapping());
        ReflectionTestUtils.invokeMethod(scanner, "executeListTask", tasks.getTaskStatus(id));
        assertEquals(Collections.singletonList(key), delivered);
        assertEquals(mapping, messages.get(0).getKeyMapping());
        verify(s3).getObjectMetadata("bucket", key); verify(s3, never()).listObjects(any(ListObjectsRequest.class));
        FileReportRecord row = ledger.page(id, null, 20).getItems().get(0);
        assertEquals(key, row.getSourceKey()); assertEquals(mapping.getTargetKey(), row.getDestinationKey());
        assertEquals(Long.valueOf(0L), row.getSize()); assertTrue(ledger.page(id,null,20).getCoverage().isScanComplete());
    }

    @Test public void mappedPrefixSurvivesAllGeneratedRangesAndDoesNotTrimWhitespace() {
        String prefix = " 中文 /";
        source.put("outside", 9L);
        for (int i=0;i<BalancedWorkerScheduler.MAX_ITEMS+2;i++) source.put(prefix+String.format("%04d",i), (long)i);
        BucketPair pair=pair(); pair.setPrefix(prefix);
        OssKeyMapping mapping=new OssKeyMapping();mapping.setMode(OssKeyMapping.Mode.REPLACE_PREFIX);mapping.setSourcePrefix(prefix);mapping.setTargetPrefix("new/");pair.setKeyMapping(mapping);
        explorer.exploreSingleBucketAndCreateSubTasks(pair);
        assertTrue(tasks.getTaskIdList().size()>1);
        for(Integer id:tasks.getTaskIdList()) {
            SubTaskStatus task=tasks.getTaskStatus(id);assertEquals(mapping,task.getKeyMapping());
            assertEquals(prefix,task.getSourceEndpoint().getPrefix());
            ReflectionTestUtils.invokeMethod(scanner,"executeListTask",task);
            for(FileReportRecord row:ledger.page(id,null,1000).getItems()) assertEquals("new/"+row.getSourceKey().substring(prefix.length()),row.getDestinationKey());
        }
        assertEquals(BalancedWorkerScheduler.MAX_ITEMS+2, delivered.size()); assertFalse(delivered.contains("outside"));
        for(ObjectListMessage message:messages) assertEquals(mapping,message.getKeyMapping());
    }

    @Test public void missingSingleObjectLeavesFailureReportAndCanResumeSameMapping() {
        BucketPair pair=pair();pair.setObjectKey("missing");
        OssKeyMapping mapping=new OssKeyMapping();mapping.setMode(OssKeyMapping.Mode.ADD_PREFIX);mapping.setTargetPrefix("copy");pair.setKeyMapping(mapping);
        explorer.exploreSingleBucketAndCreateSubTasks(pair);Integer id=tasks.getTaskIdList().get(0);
        ReflectionTestUtils.invokeMethod(scanner,"executeListTask",tasks.getTaskStatus(id));
        assertEquals(SubTaskStatus.TaskStatus.FAILED,tasks.getTaskStatus(id).getStatus());
        FileReportRecord failure=ledger.page(id,null,20).getItems().get(0);assertEquals("FAILED",failure.getResult());assertEquals("copy/missing",failure.getDestinationKey());
        source.put("missing",17L);assertTrue(tasks.resumeTask(id));
        ReflectionTestUtils.invokeMethod(scanner,"executeListTask",tasks.getTaskStatus(id));
        assertEquals(Collections.singletonList("missing"),delivered);assertEquals(mapping,messages.get(0).getKeyMapping());
    }

    @Test public void initiallyEmptySourceRetainsMappingWhenObjectsArriveBeforeExecution() {
        BucketPair pair=pair();pair.setPrefix("future/");
        OssKeyMapping mapping=new OssKeyMapping();mapping.setMode(OssKeyMapping.Mode.ADD_PREFIX);mapping.setTargetPrefix("copy");pair.setKeyMapping(mapping);
        explorer.exploreSingleBucketAndCreateSubTasks(pair);assertEquals(1,tasks.getTaskIdList().size());
        Integer id=tasks.getTaskIdList().get(0);assertEquals(mapping,tasks.getTaskStatus(id).getKeyMapping());
        source.put("future/item",3L);source.put("outside",8L);
        ReflectionTestUtils.invokeMethod(scanner,"executeListTask",tasks.getTaskStatus(id));
        assertEquals(Collections.singletonList("future/item"),delivered);
        assertEquals("copy/future/item",ledger.page(id,null,20).getItems().get(0).getDestinationKey());
    }

    @Test public void importedTopLevelPrefixFiltersAddPrefixTasksInsteadOfScanningWholeBucket() {
        source.put("aaa-outside", 8L);source.put("selected/空 文件",0L);source.put("zzz-outside",9L);
        TaskImport task=new TaskImport();task.setTaskIndex(77);task.setSourceBucket("bucket");task.setDestBucket("target");
        task.setSourceS3Alias("source");task.setDestS3Alias("target");task.setStartKey("");task.setEndKey("");task.setPrefix("selected/");
        OssKeyMapping mapping=new OssKeyMapping();mapping.setMode(OssKeyMapping.Mode.ADD_PREFIX);mapping.setTargetPrefix("copy");task.setKeyMapping(mapping);
        tasks.importTasksWithTaskId(Collections.singletonList(task));
        SubTaskStatus saved=tasks.getTaskStatus(77);assertEquals("selected/",saved.getSourceEndpoint().getPrefix());
        assertEquals(Boolean.TRUE,saved.getSourceEndpoint().getPrefixFilterEnabled());
        ReflectionTestUtils.invokeMethod(scanner,"executeListTask",saved);
        assertEquals(Collections.singletonList("selected/空 文件"),delivered);
        assertEquals("copy/selected/空 文件",ledger.page(77,null,20).getItems().get(0).getDestinationKey());
    }

    private BucketPair pair() {
        BucketPair pair=new BucketPair();pair.setSourceBucket("bucket");pair.setDestBucket("target");pair.setSourceS3Alias("source");pair.setDestS3Alias("target");pair.setDistributionMode("balanced");pair.setOverwrite(false);return pair;
    }
}
