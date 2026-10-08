package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.BufferQueueManager;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class OssKeyMappingPropagationTest {
    @Rule public TemporaryFolder temp = new TemporaryFolder();
    private RocksDBService db;
    private SubTaskManagerService tasks;
    @Before public void setup() throws Exception {
        db=new RocksDBService();ReflectionTestUtils.setField(db,"dbPath",temp.newFolder("db").toString());db.init();
        tasks=new SubTaskManagerService();ReflectionTestUtils.setField(tasks,"rocksDBService",db);
        ReflectionTestUtils.setField(tasks,"endpointNormalizationService",new EndpointNormalizationService());
        ReflectionTestUtils.setField(tasks,"fileReportLedgerService",mock(FileReportLedgerService.class));
    }
    @After public void close() { tasks.shutdownCompletionWriter();db.destroy(); }

    @Test public void importPersistsPrefixFilterAndMappingAcrossDatabaseRestart() {
        TaskImport incoming=task(19);OssKeyMapping mapping=replace("source/","destination/");incoming.setKeyMapping(mapping);
        TransferEndpoint endpoint=new TransferEndpoint();endpoint.setType(EndpointType.OSS);endpoint.setS3Alias("source");endpoint.setBucket("source-bucket");endpoint.setPrefix("source/");incoming.setSourceEndpoint(endpoint);
        tasks.importTasksWithTaskId(Collections.singletonList(incoming));db.destroy();db.init();
        SubTaskStatus stored=tasks.getTaskStatus(19);assertEquals(mapping,stored.getKeyMapping());
        assertEquals(Boolean.TRUE,stored.getSourceEndpoint().getPrefixFilterEnabled());assertEquals("source/",stored.getSourceEndpoint().getPrefix());
    }

    @Test public void existingTaskCannotLoseOrChangeDestinationMappingOnReimport() {
        TaskImport first=task(19);OssKeyMapping mapping=add("first");first.setKeyMapping(mapping);tasks.importTasksWithTaskId(Collections.singletonList(first));
        for(OssKeyMapping next:Arrays.asList(null,add("other"))) {
            TaskImport incoming=task(19);incoming.setKeyMapping(next);
            try {tasks.importTasksWithTaskId(Collections.singletonList(incoming));fail("mapping replaced");}
            catch(TransferValidationException expected){assertEquals("TASK_KEY_MAPPING_MISMATCH",expected.getCode());}
            assertEquals(mapping,tasks.getTaskStatus(19).getKeyMapping());
        }
        TaskImport same=task(19);same.setKeyMapping(add("first"));tasks.importTasksWithTaskId(Collections.singletonList(same));
        assertEquals(mapping,tasks.getTaskStatus(19).getKeyMapping());
    }

    @Test public void legacyUnmappedTaskCannotAcquireANewMappingByIdOverwrite() {
        tasks.importTasksWithTaskId(Collections.singletonList(task(19)));
        TaskImport incoming=task(19);incoming.setKeyMapping(add("new"));
        try {tasks.importTasksWithTaskId(Collections.singletonList(incoming));fail();}
        catch(TransferValidationException expected){assertEquals("TASK_KEY_MAPPING_MISMATCH",expected.getCode());}
        assertNull(tasks.getTaskStatus(19).getKeyMapping());
    }

    @Test public void failedAndFallbackRecordsKeepSourceKeyAndRecordMappedDestination() throws Exception {
        TaskImport incoming=task(19);incoming.setKeyMapping(add("mapped"));tasks.importTasksWithTaskId(Collections.singletonList(incoming));
        BufferQueueManager queue=mock(BufferQueueManager.class);ReflectionTestUtils.setField(tasks,"bufferQueueManager",queue);
        tasks.recordFailedKey(19,"original", "failure",7L);tasks.recordFallbackObject(19,"original",null,"fallback");
        org.mockito.ArgumentCaptor<FailedKeyMessage> failed=org.mockito.ArgumentCaptor.forClass(FailedKeyMessage.class);
        org.mockito.ArgumentCaptor<FallbackMessage> fallback=org.mockito.ArgumentCaptor.forClass(FallbackMessage.class);
        verify(queue).putFailedKey(failed.capture());verify(queue).putFallback(fallback.capture());
        assertEquals("original",failed.getValue().getKey());assertEquals("mapped/original",failed.getValue().getDestinationKey());
        assertEquals("original",fallback.getValue().getKey());assertEquals("mapped/original",fallback.getValue().getDestinationKey());
        SubTaskStatus invalid=tasks.getTaskStatus(19);invalid.setKeyMapping(replace("wrong/","new/"));db.putSync("task:19",invalid);
        tasks.recordFailedKey(19,"original", "bad mapping",7L);verify(queue,times(2)).putFailedKey(failed.capture());assertNull(failed.getValue().getDestinationKey());
    }

    @Test public void distributedWorkerReceivesSingleSelectionAndMapping() {
        DistributedConfig config=new DistributedConfig();config.setWorkerIps(Collections.singletonList("10.0.0.1"));config.setPort(1785);
        DistributedService distributor=new DistributedService();SubTaskManagerService allocator=mock(SubTaskManagerService.class);when(allocator.allocateTaskIdRange(1)).thenReturn(41);
        ReflectionTestUtils.setField(distributor,"distributedConfig",config);ReflectionTestUtils.setField(distributor,"subTaskManagerService",allocator);
        MockRestServiceServer remote=MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(distributor,"restTemplate")).build();
        remote.expect(requestTo("http://10.0.0.1:1785/api/worker/tasks"))
                .andExpect(content().json("[{\"taskIndex\":41,\"objectKey\":\"literal source\",\"keyMapping\":{\"mode\":\"ADD_PREFIX\",\"targetPrefix\":\"new\"}}]",false))
                .andRespond(withSuccess("{\"success\":true}",MediaType.APPLICATION_JSON));
        ListSubTask task=new ListSubTask();task.setObjectKey("literal source");task.setKeyMapping(add("new"));
        distributor.distributeTasks(Collections.singletonList(task),DistributedService.MODE_BALANCED);remote.verify();
    }

    @Test public void invalidImportIsRejectedBeforeAnyTaskRecordsAreWritten() {
        TaskImport valid=task(1),invalid=task(2);invalid.setKeyMapping(replace("source/","new/"));
        try {tasks.importTasksWithTaskId(Arrays.asList(valid,invalid));fail();}
        catch(TransferValidationException expected){assertEquals("INVALID_KEY_MAPPING",expected.getCode());}
        assertNull(db.get("task:1",SubTaskStatus.class));assertNull(db.get("task:2",SubTaskStatus.class));
    }

    @Test public void legacyEndpointPrefixWithoutFilterFlagRemainsRangeOnly() {
        TaskImport incoming=task(1);TransferEndpoint endpoint=new TransferEndpoint();endpoint.setType(EndpointType.OSS);endpoint.setPrefix("legacy/start");incoming.setSourceEndpoint(endpoint);
        new EndpointNormalizationService().normalizeAndValidate(incoming);assertNull(endpoint.getPrefixFilterEnabled());
    }

    @Test public void explicitImportPrefixRejectsConflictsAndExactSelection() {
        TaskImport task=task(1);task.setPrefix("selected/");task.setObjectKey("selected/key");
        try{new EndpointNormalizationService().normalizeAndValidate(task);fail();}
        catch(TransferValidationException expected){assertEquals("INVALID_SOURCE_SELECTION",expected.getCode());}
        task=task(1);task.setPrefix("selected/");TransferEndpoint endpoint=new TransferEndpoint();endpoint.setPrefix("other/");task.setSourceEndpoint(endpoint);
        try{new EndpointNormalizationService().normalizeAndValidate(task);fail();}
        catch(TransferValidationException expected){assertEquals("INVALID_SOURCE_SELECTION",expected.getCode());}
    }

    @Test public void requestOnlyPrefixSurvivesInternalJsonTransferAsNormalizedEndpoint() throws Exception {
        com.fasterxml.jackson.databind.ObjectMapper mapper=new com.fasterxml.jackson.databind.ObjectMapper();
        TaskImport received=mapper.readValue("{\"taskIndex\":1,\"sourceBucket\":\"source-bucket\",\"destBucket\":\"target-bucket\",\"prefix\":\" selected/ \",\"keyMapping\":{\"mode\":\"ADD_PREFIX\",\"targetPrefix\":\"copy\"}}",TaskImport.class);
        assertEquals(" selected/ ",received.getPrefix());
        new EndpointNormalizationService().normalizeAndValidate(received);
        String forwarded=mapper.writeValueAsString(received);
        assertFalse(mapper.readTree(forwarded).has("prefix"));
        TaskImport worker=mapper.readValue(forwarded,TaskImport.class);
        assertNull(worker.getPrefix());
        new EndpointNormalizationService().normalizeAndValidate(worker);
        assertEquals(" selected/ ",worker.getSourceEndpoint().getPrefix());
        assertEquals(Boolean.TRUE,worker.getSourceEndpoint().getPrefixFilterEnabled());
        SubTaskStatus status=mapper.convertValue(worker,SubTaskStatus.class);
        assertEquals(" selected/ ",status.getSourceEndpoint().getPrefix());
        assertEquals(received.getKeyMapping(),status.getKeyMapping());
    }

    private static TaskImport task(int id){TaskImport task=new TaskImport();task.setTaskIndex(id);task.setSourceBucket("source-bucket");task.setDestBucket("target-bucket");task.setSourceS3Alias("source");task.setDestS3Alias("target");task.setStartKey("");task.setEndKey("");return task;}
    private static OssKeyMapping add(String prefix){OssKeyMapping m=new OssKeyMapping();m.setMode(OssKeyMapping.Mode.ADD_PREFIX);m.setTargetPrefix(prefix);return m;}
    private static OssKeyMapping replace(String source,String target){OssKeyMapping m=new OssKeyMapping();m.setMode(OssKeyMapping.Mode.REPLACE_PREFIX);m.setSourcePrefix(source);m.setTargetPrefix(target);return m;}
}
