package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import org.junit.Test;
import org.springframework.http.HttpStatus;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.web.server.ResponseStatusException;
import org.springframework.http.MediaType;
import org.springframework.web.client.RestTemplate;
import org.springframework.test.web.client.MockRestServiceServer;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.zip.*;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static com.transfer.tools.service.ReportCsvTestSupport.*;

public class TaskReportServiceTest {
    @Test public void publicExportRequiresEveryShardAndDiscoveryToHaveEnded() {
        for (SubTaskStatus.TaskStatus status : SubTaskStatus.TaskStatus.values()) {
            FakeService service = service("one");
            TaskReportWorkerSnapshot snapshot = snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED");
            snapshot.getTasks().get(0).setStatus(status);
            service.scopes.put("one", snapshot);
            if (status == SubTaskStatus.TaskStatus.COMPLETED || status == SubTaskStatus.TaskStatus.FAILED
                    || status == SubTaskStatus.TaskStatus.PARTIAL_FAILED || status == SubTaskStatus.TaskStatus.STOPPED) {
                assertNotNull(service.resolveExportableScope(7));
            } else assertExportRejected(service, HttpStatus.CONFLICT);
        }
        FakeService service = service("one");
        TaskReportWorkerSnapshot snapshot = snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED");
        snapshot.setDiscoveryStatus("RUNNING"); snapshot.setDiscoveryComplete(false);
        service.scopes.put("one", snapshot);
        assertExportRejected(service, HttpStatus.CONFLICT);
        snapshot.setDiscoveryStatus("FAILED");
        assertNotNull(service.resolveExportableScope(7));
        snapshot.setDiscoveryStatus("STOPPED");
        assertNotNull(service.resolveExportableScope(7));
    }

    @Test public void stoppedReportRequiresConfirmedDrainedExecutionOnEveryRequiredNode() {
        FakeService service = service("one", "two");
        TaskReportWorkerSnapshot first = snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED");
        first.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.STOPPED);
        service.scopes.put("one", first);
        assertExportRejected(service, HttpStatus.SERVICE_UNAVAILABLE);
        TaskReportWorkerSnapshot second = snapshot("two", TransferDirection.OSS_TO_OSS, 0, 0, 0, "TRACKED");
        second.setTasks(Collections.emptyList()); second.setFound(false);
        second.setExecutionActive(null); service.scopes.put("two", second);
        assertExportRejected(service, HttpStatus.SERVICE_UNAVAILABLE);
        second.setExecutionActive(false); first.setExecutionActive(true);
        assertExportRejected(service, HttpStatus.CONFLICT);
        first.setExecutionActive(false); second.setScopeMissing(true);
        assertExportRejected(service, HttpStatus.SERVICE_UNAVAILABLE);
        second.setScopeMissing(false);
        assertNotNull(service.resolveExportableScope(7));
    }

    @Test public void stoppedParentCannotHidePendingRunningOrListDoneShardOrDiscovery() {
        FakeService service = service("one");
        configureParent(service, TransferDirection.LOCAL_TO_OSS, "one");
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTasks");
        LocalParentTaskRecord parent = parents.get(7); parent.setStatus(SubTaskStatus.TaskStatus.STOPPED);
        parent.setOrchestrationComplete(false);
        TaskReportWorkerSnapshot snapshot = snapshot("one", TransferDirection.LOCAL_TO_OSS, 1, 1, 0, "TRACKED");
        service.scopes.put("one", snapshot);
        for (SubTaskStatus.TaskStatus status : Arrays.asList(SubTaskStatus.TaskStatus.PENDING,
                SubTaskStatus.TaskStatus.RUNNING, SubTaskStatus.TaskStatus.LIST_DONE)) {
            snapshot.getTasks().get(0).setStatus(status);
            assertExportRejected(service, HttpStatus.CONFLICT);
        }
        snapshot.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.STOPPED);
        snapshot.setDiscoveryComplete(false);
        for (String status : Arrays.asList("PENDING", "RUNNING")) {
            snapshot.setDiscoveryStatus(status); assertExportRejected(service, HttpStatus.CONFLICT);
        }
        snapshot.setDiscoveryStatus("STOPPED");
        assertNotNull(service.resolveExportableScope(7));
    }

    @Test public void stoppedSnapshotIsPartialEvenWhenScanAndAllCurrentCountersReconcile() throws Exception {
        for (TransferDirection direction : TransferDirection.values()) {
            FakeService service = service("one");
            if (direction != TransferDirection.OSS_TO_OSS) configureParent(service, direction, "one");
            TaskReportWorkerSnapshot snapshot = snapshot("one", direction, 2, 1, 0, "TRACKED");
            snapshot.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.STOPPED);
            service.scopes.put("one", snapshot);
            service.pages.put("one:null", page(true, null, 1, record(direction, "a", "SUCCESS"), record(direction, "b", "UNPROCESSED")));
            assertNotNull(service.resolveExportableScope(7));
            Map meta = metadata(export(service));
            assertEquals(true, meta.get("stoppedSnapshot")); assertEquals(false, meta.get("runningSnapshot"));
            assertEquals(true, meta.get("stableSnapshot")); assertEquals(true, meta.get("scanComplete"));
            assertEquals(true, meta.get("countsReconciled")); assertEquals(false, meta.get("complete"));
            assertEquals("PARTIAL", meta.get("coverage"));
            assertTrue(((List) meta.get("partialReasons")).contains("TASK_STOPPED"));
            assertEquals(1, ((Number) meta.get("unprocessedCount")).intValue());
        }
    }

    @Test public void stoppedTaskResumedDuringExportRemainsAnUnstablePartialSnapshot() throws Exception {
        FakeService service = service("one");
        TaskReportWorkerSnapshot stopped = snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED");
        stopped.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.STOPPED);
        service.scopes.put("one", stopped);
        TaskReportWorkerSnapshot resumed = snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED");
        resumed.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.RUNNING); resumed.setExecutionActive(true);
        service.endScopes.put("one", resumed);
        service.pages.put("one:null", page(true, null, 1, record(TransferDirection.OSS_TO_OSS, "a", "SUCCESS")));
        Map meta = metadata(export(service));
        assertEquals(true, meta.get("runningSnapshot")); assertEquals(true, meta.get("stoppedSnapshot"));
        assertEquals(false, meta.get("stableSnapshot")); assertEquals(false, meta.get("complete"));
        assertTrue(((List) meta.get("partialReasons")).contains("STATE_CHANGED_DURING_EXPORT"));
    }

    @Test public void exportCannotTreatUnavailableOrMissingExecutionRecordsAsComplete() {
        FakeService service = service("one", "two");
        service.scopes.put("one", snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED"));
        assertExportRejected(service, HttpStatus.SERVICE_UNAVAILABLE);
        TaskReportWorkerSnapshot missing = snapshot("two", TransferDirection.OSS_TO_OSS, 0, 0, 0, "TRACKED");
        missing.setScopeMissing(true); service.scopes.put("two", missing);
        assertExportRejected(service, HttpStatus.SERVICE_UNAVAILABLE);
    }

    @Test public void failedParentCannotHideStillRunningShard() {
        FakeService service = service("one", "two");
        LocalParentTaskRecord parent = parent(); parent.setStatus(SubTaskStatus.TaskStatus.FAILED);
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTasks");
        when(parents.get(7)).thenReturn(parent);
        when(parents.toStatus(parent)).thenAnswer(call -> task(7, TransferDirection.LOCAL_TO_OSS, 0, 0, 0));
        TaskReportWorkerSnapshot running = snapshot("one", TransferDirection.LOCAL_TO_OSS, 1, 1, 0, "TRACKED");
        running.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.RUNNING);
        service.scopes.put("one", running);
        service.scopes.put("two", snapshot("two", TransferDirection.LOCAL_TO_OSS, 1, 1, 0, "TRACKED"));
        assertExportRejected(service, HttpStatus.CONFLICT);
    }

    private static void assertExportRejected(FakeService service, HttpStatus expected) {
        try { service.resolveExportableScope(7); fail("export must be rejected"); }
        catch (ResponseStatusException error) { assertEquals(expected, error.getStatus()); }
    }

    @Test
    public void legacyCursorIsEncodedExactlyOnceOnTheWorkerHttpRequest() {
        TaskReportService service = new TaskReportService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getPort()).thenReturn(1685);
        when(config.getInternalToken()).thenReturn("internal-test");
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        RestTemplate client = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(client).build();
        server.expect(requestTo("http://worker:1685/api/worker/task-reports/7/files?limit=500&cursor=legacy%3A7%3A501"))
                .andExpect(header("X-Internal-Token", "internal-test"))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"items\":[],\"done\":false,\"nextCursor\":\"legacy:7:1002\"}}", MediaType.APPLICATION_JSON));
        assertEquals("legacy:7:1002", service.fetchPage("worker", 7, "legacy:7:501").getNextCursor());
        server.verify();
    }

    @Test
    public void streamsTwoPagesInAllThreeDirectionsWithCompleteMetadata() throws Exception {
        for (TransferDirection direction : TransferDirection.values()) {
            FakeService service = service("one");
            if (direction != TransferDirection.OSS_TO_OSS) configureParent(service, direction, "one");
            TaskReportWorkerSnapshot snapshot = snapshot("one", direction, 2, 2, 0, "TRACKED");
            snapshot.getTasks().get(0).setStartKey("range/a"); snapshot.getTasks().get(0).setEndKey("range/z");
            service.scopes.put("one", snapshot);
            service.pages.put("one:null", page(false, "next", 1, record(direction, "a", "SUCCESS")));
            service.pages.put("one:next", page(true, null, 1, record(direction, "b", "SUCCESS")));
            Map<String, String> zip = export(service);
            assertEquals(4, zip.size());
            Map meta = metadata(zip);
            assertEquals(2, meta.get("schemaVersion"));
            assertPublicIds(zip, 7);
            assertEquals(true, meta.get("complete"));
            assertEquals(true, meta.get("countsReconciled"));
            assertEquals(2, ((Number) meta.get("fileRows")).intValue());
            assertEquals(false, meta.get("contentVerified"));
            assertTrue(zip.get("file-results.csv").contains("中文 空格/a"));
            assertTrue(zip.get("task-summary.csv").contains("startKey,endKey,objectKey"));
            assertTrue(zip.get("task-summary.csv").contains("\"range/a\",\"range/z\""));
            assertEquals(2, service.pageReads);
        }
    }

    @Test public void archiveIncludesMappingPolicyAndBothActualKeys() throws Exception {
        FakeService service = service("one");
        TaskReportWorkerSnapshot snapshot = snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED");
        OssKeyMapping mapping = new OssKeyMapping(); mapping.setMode(OssKeyMapping.Mode.REPLACE_PREFIX);
        mapping.setSourcePrefix("source/"); mapping.setTargetPrefix("archive/");
        snapshot.getTasks().get(0).setKeyMapping(mapping); service.scopes.put("one", snapshot);
        FileReportRecord item = record(TransferDirection.OSS_TO_OSS, "source/a", "SUCCESS");
        item.setSourceKey("source/a"); item.setDestinationKey("archive/a");
        service.pages.put("one:null", page(true, null, 1, item));
        Map<String, String> zip = export(service);
        assertTrue(zip.get("task-summary.csv").contains("keyMappingMode,keyMappingSourcePrefix,keyMappingTargetPrefix,keyMappingTargetKey"));
        assertTrue(zip.get("task-summary.csv").contains("\"REPLACE_PREFIX\",\"source/\",\"archive/\""));
        assertTrue(zip.get("file-results.csv").contains("\"source/a\""));
        assertTrue(zip.get("file-results.csv").contains("\"archive/a\""));
    }

    @Test
    public void localParentIncludesEveryShardAndMergesDiscoveryAndFinalResults() throws Exception {
        FakeService service = service("one", "two");
        LocalParentTaskRecord parent = parent();
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTasks");
        when(parents.get(7)).thenReturn(parent);
        when(parents.toStatus(parent)).thenAnswer(call -> task(7, TransferDirection.LOCAL_TO_OSS, 0, 0, 0));
        TaskReportWorkerSnapshot first = snapshot("one", TransferDirection.LOCAL_TO_OSS, 1, 1, 0, "TRACKED");
        first.getTasks().get(0).setTaskIndex(-1); first.getTasks().get(0).setParentTaskId(7);
        first.getTasks().get(0).setShardId("7-local-0"); first.getTasks().get(0).setShardIndex(0);
        first.setDiscoveryComplete(true);
        first.getTasks().get(0).setStartTime(100L); first.getTasks().get(0).setEndTime(300L);
        TaskReportWorkerSnapshot second = snapshot("two", TransferDirection.LOCAL_TO_OSS, 1, 1, 0, "TRACKED");
        second.getTasks().get(0).setTaskIndex(-2); second.getTasks().get(0).setParentTaskId(7);
        second.getTasks().get(0).setShardId("7-local-1"); second.getTasks().get(0).setShardIndex(1);
        second.getTasks().get(0).setStartTime(200L); second.getTasks().get(0).setEndTime(400L);
        service.scopes.put("one", first); service.scopes.put("two", second);
        assertEquals(Long.valueOf(100), service.resolveScope(7).getSummary().getStartTime());
        assertEquals(Long.valueOf(400), service.resolveScope(7).getSummary().getEndTime());
        FileReportRecord firstResult = record(TransferDirection.LOCAL_TO_OSS, "a", "SUCCESS"); firstResult.setTaskId(-1);
        FileReportRecord discovered = record(TransferDirection.LOCAL_TO_OSS, "b", "UNPROCESSED");
        discovered.setTaskId(null); discovered.setShardId("discovery");
        FileReportRecord secondResult = record(TransferDirection.LOCAL_TO_OSS, "b", "SUCCESS"); secondResult.setTaskId(-2);
        service.pages.put("one:null", page(true, null, 1, firstResult, discovered));
        service.pages.put("two:null", page(true, null, 1, secondResult));
        Map<String, String> zip = export(service);
        Map meta = metadata(zip);
        assertEquals(true, meta.get("complete"));
        assertEquals("LOCAL_PARENT_WITH_ALL_SHARDS", meta.get("scope"));
        assertEquals(1, ((Number) meta.get("duplicateRowsMerged")).intValue());
        assertEquals(0, ((Number) meta.get("unprocessedCount")).intValue());
        assertPublicIds(zip, 7);
        List<Map<String, String>> summary = rows(zip.get("task-summary.csv"));
        assertEquals("", summary.get(0).get("childTaskId"));
        assertEquals("7-1", summary.get(1).get("childTaskId"));
        assertEquals("7-2", summary.get(2).get("childTaskId"));
        List<Map<String, String>> files = rows(zip.get("file-results.csv"));
        assertEquals("7-1", files.get(0).get("childTaskId"));
        assertEquals("7-2", files.get(1).get("childTaskId"));
        assertEquals(Integer.valueOf(-1), firstResult.getTaskId());
        assertEquals(Integer.valueOf(-2), secondResult.getTaskId());
        assertEquals("discovery", discovered.getShardId());
    }

    @Test
    public void legacySuccessRowsCannotStandInForMissingSkippedFiles() throws Exception {
        FakeService service = service("one");
        TaskReportWorkerSnapshot snapshot = snapshot("one", TransferDirection.OSS_TO_LOCAL, 3, 1, 2, "LEGACY_UNKNOWN");
        snapshot.getCoverage().setScanComplete(false);
        service.scopes.put("one", snapshot);
        service.pages.put("one:null", page(false, "legacy:500", 2));
        service.pages.put("one:legacy:500", page(true, null, 3, record(TransferDirection.OSS_TO_LOCAL, "a", "SUCCESS")));
        Map meta = metadata(export(service));
        assertEquals(false, meta.get("complete"));
        assertEquals(false, meta.get("countsReconciled"));
        assertEquals("LEGACY_OR_PARTIAL", meta.get("coverage"));
        assertEquals(1, ((Number) meta.get("fileRows")).intValue());
        assertTrue(((List) meta.get("partialReasons")).contains("HISTORICAL_OR_PARTIAL_LEDGER"));
    }

    @Test
    public void midPageNodeFailureProducesUsablePartialZip() throws Exception {
        FakeService service = service("one");
        service.scopes.put("one", snapshot("one", TransferDirection.OSS_TO_OSS, 2, 2, 0, "TRACKED"));
        service.pages.put("one:null", page(false, "next", 1, record(TransferDirection.OSS_TO_OSS, "a", "SUCCESS")));
        Map<String, String> zip = export(service);
        Map meta = metadata(zip);
        assertEquals(4, zip.size());
        assertEquals(false, meta.get("complete"));
        assertEquals(Collections.singletonList("one"), meta.get("unreachableNodes"));
        assertEquals(1, ((Number) meta.get("fileRows")).intValue());
        assertTrue(zip.get("exception-details.csv").contains("WORKER_REPORT_INCOMPLETE"));
    }

    @Test
    public void unavailableRequiredNodeNeverDisappearsFromCompleteness() throws Exception {
        FakeService service = service("one", "two");
        service.scopes.put("one", snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED"));
        service.pages.put("one:null", page(true, null, 1, record(TransferDirection.OSS_TO_OSS, "a", "SUCCESS")));
        Map meta = metadata(export(service));
        assertEquals(false, meta.get("complete"));
        assertEquals(false, meta.get("stableSnapshot"));
        assertEquals(Arrays.asList("one", "two"), meta.get("requiredNodes"));
        assertEquals(Collections.singletonList("two"), meta.get("unreachableNodes"));
    }

    @Test
    public void revisionChangeAcrossPagesMarksPartialEvenWhenFinalCountsMatch() throws Exception {
        FakeService service = service("one");
        service.scopes.put("one", snapshot("one", TransferDirection.OSS_TO_OSS, 2, 2, 0, "TRACKED"));
        service.pages.put("one:null", page(false, "next", 1, record(TransferDirection.OSS_TO_OSS, "a", "SUCCESS")));
        service.pages.put("one:next", page(true, null, 2, record(TransferDirection.OSS_TO_OSS, "b", "SUCCESS")));
        Map meta = metadata(export(service));
        assertEquals(true, meta.get("countsReconciled"));
        assertEquals(false, meta.get("complete"));
        assertEquals(Collections.singletonList("one"), meta.get("changedNodes"));
    }

    @Test
    public void taskCounterChangeDuringExportMarksPartial() throws Exception {
        FakeService service = service("one");
        service.scopes.put("one", snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED"));
        service.endScopes.put("one", snapshot("one", TransferDirection.OSS_TO_OSS, 2, 1, 0, "TRACKED"));
        service.pages.put("one:null", page(true, null, 1, record(TransferDirection.OSS_TO_OSS, "a", "SUCCESS")));
        Map meta = metadata(export(service));
        assertEquals(false, meta.get("complete"));
        assertEquals(false, meta.get("countsReconciled"));
        assertEquals(Collections.singletonList("one"), meta.get("changedNodes"));
    }

    @Test
    public void runningTaskWithAllCurrentRowsIsStillASnapshot() throws Exception {
        FakeService service = service("one");
        TaskReportWorkerSnapshot snapshot = snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED");
        snapshot.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.RUNNING);
        service.scopes.put("one", snapshot);
        service.pages.put("one:null", page(true, null, 1, record(TransferDirection.OSS_TO_OSS, "a", "SUCCESS")));
        Map meta = metadata(export(service));
        assertEquals(true, meta.get("runningSnapshot"));
        assertEquals(false, meta.get("complete"));
    }

    @Test
    public void failedRowsAreEscapedAndIncludedInExceptionDetails() throws Exception {
        FakeService service = service("one");
        configureParent(service, TransferDirection.OSS_TO_LOCAL, "one");
        TaskReportWorkerSnapshot snapshot = snapshot("one", TransferDirection.OSS_TO_LOCAL, 1, 0, 0, "TRACKED");
        snapshot.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.PARTIAL_FAILED);
        snapshot.getTasks().get(0).setFailedObjectCount(1L);
        snapshot.getTasks().get(0).setFailedObjectSize(10L);
        snapshot.getTasks().get(0).setShardIndex(2);
        service.scopes.put("one", snapshot);
        FileReportRecord row = record(TransferDirection.OSS_TO_LOCAL, "=cmd,\"中文\"\nfile", "FAILED");
        row.setErrorMessage("bad \"destination\""); row.setReasonCode("WRITE_FAILED");
        service.pages.put("one:null", page(true, null, 1, row));
        Map<String, String> zip = export(service);
        assertEquals(true, metadata(zip).get("complete"));
        assertTrue(zip.get("file-results.csv").contains("\"'=cmd,\"\"中文\"\"\nfile\""));
        assertTrue(zip.get("exception-details.csv").contains("bad \"\"destination\"\""));
        assertTrue(String.valueOf(metadata(zip).get("csvStringEscaping")).startsWith("APOSTROPHE_PREFIX_V1"));
        assertPublicIds(zip, 7);
        assertEquals("7-3", rows(zip.get("task-summary.csv")).get(1).get("childTaskId"));
        assertEquals("7-3", rows(zip.get("file-results.csv")).get(0).get("childTaskId"));
        assertEquals("7-3", rows(zip.get("exception-details.csv")).get(0).get("childTaskId"));
    }

    @Test public void discoveryOrchestrationAndUnattributedRowsDoNotInventChildIds() throws Exception {
        FakeService service = service("one", "missing");
        configureParent(service, TransferDirection.OSS_TO_LOCAL, "one", "missing");
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTasks");
        parents.get(7).setErrorCode("ORCHESTRATION_FAILED"); parents.get(7).setErrorMessage("failed to assign");
        TaskReportWorkerSnapshot snapshot = snapshot("one", TransferDirection.OSS_TO_LOCAL, 3, 0, 0, "TRACKED");
        snapshot.getTasks().get(0).setShardIndex(0);
        snapshot.setDiscoveryStatus("FAILED"); snapshot.setDiscoveryError("source scan failed");
        service.scopes.put("one", snapshot);
        FileReportRecord discovery = record(TransferDirection.OSS_TO_LOCAL, "a", "UNPROCESSED");
        discovery.setTaskId(7); discovery.setShardId("discovery");
        FileReportRecord orchestration = record(TransferDirection.OSS_TO_LOCAL, "b", "FAILED");
        orchestration.setTaskId(7); orchestration.setShardId("orchestration"); orchestration.setReasonCode("ASSIGN_FAILED");
        FileReportRecord unknown = record(TransferDirection.OSS_TO_LOCAL, "c", "FAILED");
        unknown.setTaskId(null); unknown.setShardId(null); unknown.setReasonCode("UNKNOWN_CHILD");
        service.pages.put("one:null", page(true, null, 1, discovery, orchestration, unknown));
        Map<String, String> zip = export(service);
        assertPublicIds(zip, 7);
        assertEquals(false, metadata(zip).get("complete"));
        assertEquals(3, rows(zip.get("file-results.csv")).size());
        for (Map<String, String> row : rows(zip.get("file-results.csv"))) assertEquals("", row.get("childTaskId"));
        List<Map<String, String>> errors = rows(zip.get("exception-details.csv"));
        assertEquals(5, errors.size());
        for (Map<String, String> row : errors) assertEquals("", row.get("childTaskId"));
        Set<String> codes = new HashSet<>();
        for (Map<String, String> row : errors) codes.add(row.get("reasonCode"));
        assertTrue(codes.toString(), codes.containsAll(Arrays.asList("ASSIGN_FAILED", "UNKNOWN_CHILD",
                "ORCHESTRATION_FAILED", "SOURCE_DISCOVERY_FAILED", "WORKER_UNAVAILABLE")));
    }

    @Test
    public void csvStringEscapingIsReversibleWithoutApostropheCollisions() {
        assertEquals("\"'=x\",\"''=x\",\"'''=x\",\"'-x\",\"-1\"\r\n",
                TaskReportService.row("=x", "'=x", "''=x", "-x", -1));
    }

    @Test
    public void unknownFileSizeDoesNotReconcileAsZeroBytes() throws Exception {
        FakeService service = service("one");
        TaskReportWorkerSnapshot snapshot = snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED");
        snapshot.getTasks().get(0).setListObjectSize(0L);
        snapshot.getTasks().get(0).setPutObjectSize(0L);
        service.scopes.put("one", snapshot);
        FileReportRecord unknown = record(TransferDirection.OSS_TO_OSS, "key-only", "SUCCESS");
        unknown.setSize(null);
        service.pages.put("one:null", page(true, null, 1, unknown));
        Map meta = metadata(export(service));
        assertEquals(false, meta.get("complete"));
        assertEquals(false, meta.get("countsReconciled"));
        assertEquals(1, ((Number) meta.get("unknownFileSizeCount")).intValue());
        assertTrue(((List) meta.get("partialReasons")).contains("FILE_SIZE_UNKNOWN"));
    }

    @Test
    public void multipleEmptyLegacyBackfillPagesContinueUntilSortedResults() throws Exception {
        FakeService service = service("one");
        service.scopes.put("one", snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "LEGACY_UNKNOWN"));
        service.pages.put("one:null", page(false, "legacy:7:2", 2));
        service.pages.put("one:legacy:7:2", page(false, "legacy:7:3", 3));
        service.pages.put("one:legacy:7:3", page(true, null, 4, record(TransferDirection.OSS_TO_OSS, "a", "SUCCESS")));
        Map meta = metadata(export(service));
        assertEquals(1, ((Number) meta.get("fileRows")).intValue());
        assertEquals(false, meta.get("complete"));
        assertEquals(Collections.emptyList(), meta.get("unreachableNodes"));
        assertEquals(3, service.pageReads);
    }

    @Test
    public void missingLocalParentCannotEstablishScanCompleteness() throws Exception {
        FakeService service = service("one");
        service.scopes.put("one", snapshot("one", TransferDirection.LOCAL_TO_OSS, 1, 1, 0, "TRACKED"));
        service.pages.put("one:null", page(true, null, 1, record(TransferDirection.LOCAL_TO_OSS, "a", "SUCCESS")));
        Map meta = metadata(export(service));
        assertEquals(false, meta.get("complete"));
        assertEquals(false, meta.get("scanComplete"));
        assertTrue(((List) meta.get("partialReasons")).contains("LOCAL_PARENT_SCOPE_UNKNOWN"));
    }

    @Test
    public void missingWorkerShardScopeKeepsItsLedgerRowsAndNeverClaimsComplete() throws Exception {
        FakeService service = service("one", "two");
        configureParent(service, TransferDirection.LOCAL_TO_OSS, "one", "two");
        service.scopes.put("one", snapshot("one", TransferDirection.LOCAL_TO_OSS, 1, 1, 0, "TRACKED"));
        TaskReportWorkerSnapshot orphan = snapshot("two", TransferDirection.LOCAL_TO_OSS, 0, 0, 0, "TRACKED");
        orphan.setFound(false); orphan.setScopeMissing(true); orphan.setTasks(Collections.emptyList());
        orphan.getCoverage().setDiscoveredCount(1); service.scopes.put("two", orphan);
        service.pages.put("one:null", page(true, null, 1, record(TransferDirection.LOCAL_TO_OSS, "a", "SUCCESS")));
        service.pages.put("two:null", page(true, null, 1, record(TransferDirection.LOCAL_TO_OSS, "b", "SUCCESS")));
        Map<String, String> zip = export(service); Map meta = metadata(zip);
        assertEquals(2, ((Number) meta.get("fileRows")).intValue());
        assertEquals(false, meta.get("complete")); assertEquals(false, meta.get("stableSnapshot"));
        assertEquals(Collections.singletonList("two"), meta.get("scopeMissingNodes"));
        assertEquals(Collections.emptyList(), meta.get("unreachableNodes"));
        assertTrue(((List) meta.get("partialReasons")).contains("NODE_SCOPE_MISSING"));
        assertTrue(zip.get("exception-details.csv").contains("NODE_SCOPE_MISSING"));
    }

    @Test
    public void outputFailurePropagatesWithoutContinuingWorkerReads() throws Exception {
        FakeService service = service("one");
        service.scopes.put("one", snapshot("one", TransferDirection.OSS_TO_OSS, 1, 1, 0, "TRACKED"));
        try {
            service.write(7, new OutputStream() { public void write(int value) throws IOException { throw new IOException("client disconnected"); } });
            fail("expected IOException");
        } catch (IOException expected) { assertEquals("client disconnected", expected.getMessage()); }
        assertEquals(0, service.pageReads);
    }

    @Test
    public void malformedNonAdvancingCursorStopsInsteadOfLooping() throws Exception {
        FakeService service = service("one");
        service.scopes.put("one", snapshot("one", TransferDirection.OSS_TO_OSS, 0, 0, 0, "TRACKED"));
        service.pages.put("one:null", page(false, null, 1));
        assertEquals(false, metadata(export(service)).get("complete"));
        assertEquals(1, service.pageReads);
    }

    @Test
    public void unknownTaskIs404ButUnreachableOwnerIsNotClaimedMissing() {
        FakeService service = service("one");
        TaskReportWorkerSnapshot missing = snapshot("one", TransferDirection.OSS_TO_OSS, 0, 0, 0, "TRACKED");
        missing.setFound(false); missing.setTasks(Collections.emptyList()); service.scopes.put("one", missing);
        try { service.resolveScope(7); fail("expected not found"); }
        catch (ResponseStatusException expected) { assertEquals(HttpStatus.NOT_FOUND, expected.getStatus()); }
        service.scopes.clear();
        try { service.resolveScope(7); fail("expected unavailable"); }
        catch (ResponseStatusException expected) { assertEquals(HttpStatus.SERVICE_UNAVAILABLE, expected.getStatus()); }
    }

    private FakeService service(String... nodes) {
        FakeService service = new FakeService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Arrays.asList(nodes));
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(service, "parentTasks", mock(LocalParentTaskService.class));
        return service;
    }

    private LocalParentTaskRecord parent() {
        LocalTransferRequest request = new LocalTransferRequest();
        request.setDirection(TransferDirection.LOCAL_TO_OSS);
        TransferEndpoint source = new TransferEndpoint(); source.setType(EndpointType.LOCAL);
        source.setNodeBindings(Arrays.asList(new LocalNodeBinding("one", "/one"), new LocalNodeBinding("two", "/two")));
        request.setSourceEndpoint(source);
        LocalParentTaskRecord result = new LocalParentTaskRecord();
        result.setParentTaskId(7); result.setRequest(request); result.setOrchestrationComplete(true);
        result.setStatus(SubTaskStatus.TaskStatus.RUNNING); result.setUpdatedAt(1L);
        return result;
    }

    private void configureParent(FakeService service, TransferDirection direction, String... nodes) {
        LocalParentTaskRecord parent = parent();
        parent.getRequest().setDirection(direction);
        TransferEndpoint local = parent.getRequest().getSourceEndpoint();
        List<LocalNodeBinding> bindings = new ArrayList<>();
        for (String node : nodes) bindings.add(new LocalNodeBinding(node, "/" + node));
        local.setNodeBindings(bindings);
        if (direction == TransferDirection.OSS_TO_LOCAL) {
            parent.getRequest().setSourceEndpoint(null); parent.getRequest().setDestinationEndpoint(local);
        }
        LocalParentTaskService parents = (LocalParentTaskService) ReflectionTestUtils.getField(service, "parentTasks");
        when(parents.get(7)).thenReturn(parent);
        when(parents.toStatus(parent)).thenAnswer(call -> task(7, direction, 0, 0, 0));
    }

    private static TaskReportWorkerSnapshot snapshot(String node, TransferDirection direction, long count, long success, long skip, String coverage) {
        TaskReportWorkerSnapshot snapshot = new TaskReportWorkerSnapshot();
        snapshot.setReportTaskId(7); snapshot.setNode(node); snapshot.setFound(true);
        snapshot.setExecutionActive(false);
        snapshot.setTasks(Collections.singletonList(task(7, direction, count, success, skip)));
        ReportCoverage report = new ReportCoverage(); report.setLedgerCoverage(coverage); report.setScanComplete(true);
        report.setRegisteredScans(1); report.setCompletedScans(1); report.setRevision(1);
        snapshot.setCoverage(report);
        return snapshot;
    }

    private static SubTaskStatus task(int id, TransferDirection direction, long count, long success, long skip) {
        SubTaskStatus task = new SubTaskStatus(); task.setTaskIndex(id); task.setDirection(direction);
        task.setStatus(SubTaskStatus.TaskStatus.COMPLETED); task.setListObjectCount(count); task.setListObjectSize(count * 10);
        task.setPutObjectCount(success); task.setPutObjectSize(success * 10); task.setSkippedObjectCount(skip); task.setSkippedObjectSize(skip * 10);
        task.setFailedObjectCount(0L); task.setFailedObjectSize(0L); return task;
    }

    private static FileReportRecord record(TransferDirection direction, String key, String result) {
        FileReportRecord row = new FileReportRecord(); row.setReportTaskId(7); row.setTaskId(7); row.setDirection(direction);
        row.setLogicalKey(key); row.setSourcePath("中文 空格/" + key); row.setDestinationPath("/destination/" + key);
        row.setSize(10L); row.setResult(result); row.setCompletedAt("UNPROCESSED".equals(result) ? null : 10L); return row;
    }

    private static FileReportPage page(boolean done, String cursor, long revision, FileReportRecord... records) {
        FileReportPage page = new FileReportPage(); page.setItems(Arrays.asList(records)); page.setDone(done); page.setNextCursor(cursor);
        ReportCoverage coverage = new ReportCoverage(); coverage.setLedgerCoverage("TRACKED"); coverage.setRevision(revision); page.setCoverage(coverage);
        return page;
    }

    private static Map<String, String> export(FakeService service) throws Exception {
        ByteArrayOutputStream output = new ByteArrayOutputStream(); service.write(7, output);
        Map<String, String> entries = new LinkedHashMap<>();
        try (ZipInputStream zip = new ZipInputStream(new ByteArrayInputStream(output.toByteArray()), StandardCharsets.UTF_8)) {
            ZipEntry entry; byte[] bytes = new byte[2048];
            while ((entry = zip.getNextEntry()) != null) {
                ByteArrayOutputStream data = new ByteArrayOutputStream(); int count;
                while ((count = zip.read(bytes)) != -1) data.write(bytes, 0, count);
                entries.put(entry.getName(), new String(data.toByteArray(), StandardCharsets.UTF_8));
            }
        }
        return entries;
    }

    private static Map metadata(Map<String, String> zip) throws IOException { return new ObjectMapper().readValue(zip.get("report-metadata.json"), Map.class); }

    private static class FakeService extends TaskReportService {
        final Map<String, TaskReportWorkerSnapshot> scopes = new HashMap<>(), endScopes = new HashMap<>();
        final Map<String, FileReportPage> pages = new HashMap<>();
        final Map<String, Integer> scopeReads = new HashMap<>();
        int pageReads;
        @Override protected TaskReportWorkerSnapshot fetchScope(String node, Integer taskId) {
            int count = scopeReads.getOrDefault(node, 0); scopeReads.put(node, count + 1);
            TaskReportWorkerSnapshot result = count > 0 && endScopes.containsKey(node) ? endScopes.get(node) : scopes.get(node);
            if (result == null) throw new IllegalStateException("offline");
            return result;
        }
        @Override protected FileReportPage fetchPage(String node, Integer taskId, String cursor) {
            pageReads++; FileReportPage page = pages.get(node + ":" + cursor);
            if (page == null) throw new IllegalStateException("page read failed"); return page;
        }
    }
}
