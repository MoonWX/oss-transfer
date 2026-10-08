package com.transfer.tools.service;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import org.junit.Test;
import org.springframework.http.HttpStatus;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.web.server.ResponseStatusException;

import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.zip.*;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static com.transfer.tools.service.ReportCsvTestSupport.*;

/** A creation is a durable directory of physical range IDs, never a bucket-name grouping. */
public class OssCreationTaskReportTest {
    @Test public void eitherPhysicalIdExportsAllRangesAndNodesWithStablePublicIdentity() throws Exception {
        Fixture f = fixture();
        SubTaskStatus first = f.service.scopes.get("one/7").getTasks().get(0);
        first.setShardId("range-0"); first.setStartKey("a"); first.setEndKey("b");
        TaskReportService.ReportScope scope = f.service.resolveExportableScope(8);
        assertNull(scope.getSummary().getShardId()); assertNull(scope.getSummary().getStartKey());
        assertNull(scope.getSummary().getEndKey());
        assertEquals(Integer.valueOf(7), scope.getTaskId());
        assertEquals(Long.valueOf(2), scope.getSummary().getPutObjectCount());
        Map<String, String> zip = export(f.service, scope);
        Map meta = metadata(zip);
        assertEquals(2, meta.get("schemaVersion"));
        assertPublicIds(zip, 7);
        assertEquals("OSS_CREATION_WITH_ALL_SHARDS", meta.get("scope"));
        assertEquals(Arrays.asList(7, 8), meta.get("physicalTaskIds"));
        assertEquals(Arrays.asList("one", "two"), meta.get("respondedNodes"));
        assertEquals(true, meta.get("complete"));
        assertEquals(2, meta.get("fileRows"));
        assertEquals(8, f.service.scopeReads); // Every physical range on every node, before and after.
        assertEquals(2, f.service.pageReads.size());
        String files = zip.get("file-results.csv");
        List<Map<String, String>> fileRows = rows(files);
        assertEquals("7-1", fileRows.get(0).get("childTaskId"));
        assertEquals("a", fileRows.get(0).get("logicalKey"));
        assertEquals("7-2", fileRows.get(1).get("childTaskId"));
        assertEquals("b", fileRows.get(1).get("logicalKey"));
        assertTrue(files.contains("\"mapped/b\""));
        assertEquals(Integer.valueOf(8), f.service.pages.get("two/8/null").getItems().get(0).getReportTaskId());
        assertTrue(zip.get("task-summary.csv").contains("\"ADD_PREFIX\""));
    }

    @Test public void physicalRangeSplitAcrossNodesCountsEachNodeOnce() throws Exception {
        Fixture f = fixture(); add(f, "one", 8, "c");
        TaskReportService.ReportScope scope = f.service.resolveExportableScope(7);
        assertEquals(Long.valueOf(3), scope.getSummary().getPutObjectCount());
        Map<String, String> zip = export(f.service, scope);
        Map meta = metadata(zip);
        assertEquals(3, meta.get("fileRows")); assertEquals(true, meta.get("complete"));
        assertPublicIds(zip, 7);
        List<Map<String, String>> files = rows(zip.get("file-results.csv"));
        assertEquals("7-1", files.get(0).get("childTaskId"));
        assertEquals("7-2", files.get(1).get("childTaskId"));
        assertEquals("7-2", files.get(2).get("childTaskId"));
        assertNotEquals(files.get(1).get("node"), files.get(2).get("node"));
        List<Map<String, String>> summary = rows(zip.get("task-summary.csv"));
        assertEquals("", summary.get(0).get("childTaskId"));
        assertEquals(2, summary.stream().filter(row -> "7-2".equals(row.get("childTaskId"))).count());
    }

    @Test public void persistedSequenceMatchesSummaryFilesAndErrorsAcrossNodes() throws Exception {
        Fixture f = fixture(); add(f, "one", 8, "c");
        for (String node : Arrays.asList("one", "two")) {
            SubTaskStatus task = f.service.scopes.get(node + "/8").getTasks().get(0);
            task.setShardIndex(3); task.setShardId("7-oss-0");
            task.setStatus(SubTaskStatus.TaskStatus.PARTIAL_FAILED);
            task.setPutObjectCount(0L); task.setPutObjectSize(0L);
            task.setFailedObjectCount(1L); task.setFailedObjectSize(10L);
            FileReportRecord row = f.service.pages.get(node + "/8/null").getItems().get(0);
            row.setResult("FAILED"); row.setReasonCode("WRITE_FAILED"); row.setShardId("7-oss-0");
        }
        Map<String, String> zip = export(f.service, f.service.resolveExportableScope(8));
        assertEquals(true, metadata(zip).get("complete"));
        assertPublicIds(zip, 7);
        List<Map<String, String>> files = rows(zip.get("file-results.csv"));
        assertEquals("7-1", files.get(0).get("childTaskId"));
        assertEquals("7-4", files.get(1).get("childTaskId"));
        assertEquals("7-4", files.get(2).get("childTaskId"));
        List<Map<String, String>> errors = rows(zip.get("exception-details.csv"));
        assertEquals(2, errors.size());
        for (Map<String, String> error : errors) assertEquals("7-4", error.get("childTaskId"));
        assertEquals(2, rows(zip.get("task-summary.csv")).stream()
                .filter(row -> "7-4".equals(row.get("childTaskId"))).count());
        for (String node : Arrays.asList("one", "two")) {
            FileReportRecord ledgerRow = f.service.pages.get(node + "/8/null").getItems().get(0);
            assertEquals(Integer.valueOf(8), ledgerRow.getReportTaskId());
            assertEquals(Integer.valueOf(8), ledgerRow.getTaskId());
            assertEquals("7-oss-0", ledgerRow.getShardId());
        }
    }

    @Test public void physicalRangeDisappearingAtEndMakesEntireReportPartial() throws Exception {
        Fixture f = fixture();
        f.service.endScopes.put("two/8", empty("two", 8));
        Map meta = metadata(export(f.service, f.service.resolveExportableScope(7)));
        assertEquals(false, meta.get("complete")); assertEquals(Collections.singletonList(8), meta.get("missingTaskIds"));
        assertEquals(Collections.singletonList("two"), meta.get("changedNodes"));
    }

    @Test public void unfinishedRangeCannotHideBehindCompletedFirstRange() {
        Fixture f = fixture();
        for (SubTaskStatus.TaskStatus status : Arrays.asList(SubTaskStatus.TaskStatus.PENDING,
                SubTaskStatus.TaskStatus.RUNNING, SubTaskStatus.TaskStatus.LIST_DONE)) {
            f.service.scopes.get("two/8").getTasks().get(0).setStatus(status);
            rejected(f, 7, HttpStatus.CONFLICT);
        }
    }

    @Test public void stillScanningCreationCannotExportEvenWhenAllCurrentRangesCompleted() {
        Fixture f = fixture();
        for (String status : Arrays.asList("QUEUED", "RUNNING")) {
            f.creation.setStatus(status); f.creation.setScanComplete(false);
            rejected(f, 8, HttpStatus.CONFLICT);
        }
    }

    @Test public void missingOnePhysicalRangeFailsClosedAndInternalSnapshotIsIncomplete() throws Exception {
        Fixture f = fixture(); f.service.scopes.put("two/8", empty("two", 8));
        rejected(f, 7, HttpStatus.SERVICE_UNAVAILABLE);
        Map meta = metadata(export(f.service, f.service.resolveScope(7)));
        assertEquals(false, meta.get("complete"));
        assertEquals(Collections.singletonList(8), meta.get("missingTaskIds"));
        assertTrue(((List) meta.get("partialReasons")).contains("TASK_SHARD_MISSING"));
    }

    @Test public void missingAllPhysicalRangesDoesNotExportGhostCreation() {
        Fixture f = fixture();
        f.service.scopes.put("one/7", empty("one", 7)); f.service.scopes.put("two/8", empty("two", 8));
        rejected(f, 7, HttpStatus.NOT_FOUND);
    }

    @Test public void removedHistoricalParticipantCannotDisappearFromReportScope() {
        Fixture f = fixture(); f.creation.setParticipatingWorkerNodes(Arrays.asList("one", "two", "removed"));
        rejected(f, 7, HttpStatus.SERVICE_UNAVAILABLE);
        TaskReportService.ReportScope scope = f.service.resolveScope(7);
        assertTrue(scope.getRequiredNodes().contains("removed"));
        assertTrue(scope.getUnreachableNodes().contains("removed"));
    }

    @Test public void unavailableNodeCannotProveThatAllRangesWereDeleted() {
        Fixture f = fixture();
        f.service.scopes.put("one/7", empty("one", 7)); f.service.scopes.remove("two/8");
        rejected(f, 7, HttpStatus.SERVICE_UNAVAILABLE);
    }

    @Test public void newRangeOrRescanDuringExportMarksEntireReportPartial() throws Exception {
        Fixture f = fixture();
        TaskReportService.ReportScope scope = f.service.resolveExportableScope(7);
        TaskCreationService.Creation changed = new ObjectMapper().convertValue(f.creation, TaskCreationService.Creation.class);
        changed.setStatus("RUNNING"); changed.setScanComplete(false); changed.getTaskIds().add(9);
        when(f.creations.get(f.creation.getId())).thenReturn(changed);
        Map meta = metadata(export(f.service, scope));
        assertEquals(false, meta.get("complete")); assertEquals(true, meta.get("runningSnapshot"));
        assertEquals("RUNNING", ((Map) meta.get("statusSnapshotAtEnd")).get("status"));
        assertTrue(((List) meta.get("partialReasons")).contains("STATE_CHANGED_DURING_EXPORT"));
    }

    @Test public void creationDeletionDuringExportDoesNotRemainComplete() throws Exception {
        Fixture f = fixture(); TaskReportService.ReportScope scope = f.service.resolveExportableScope(7);
        when(f.creations.get(f.creation.getId())).thenReturn(null);
        Map meta = metadata(export(f.service, scope));
        assertEquals(false, meta.get("complete")); assertEquals(false, meta.get("scanComplete"));
    }

    @Test public void secondRangeLedgerRevisionIsCheckedIndependentlyOfFirstRange() throws Exception {
        Fixture f = fixture();
        f.service.pages.get("two/8/null").getCoverage().setRevision(2);
        Map meta = metadata(export(f.service, f.service.resolveExportableScope(7)));
        assertEquals(false, meta.get("complete")); assertEquals(Collections.singletonList("two"), meta.get("changedNodes"));
    }

    @Test public void stoppedCreationRequiresEveryExecutionDrainedAndRemainsPartial() throws Exception {
        Fixture f = fixture(); f.creation.setStatus("STOPPED"); f.creation.setScanComplete(false);
        f.service.scopes.get("two/8").setExecutionActive(true);
        rejected(f, 7, HttpStatus.CONFLICT);
        f.service.scopes.get("two/8").setExecutionActive(false);
        Map meta = metadata(export(f.service, f.service.resolveExportableScope(7)));
        assertEquals(false, meta.get("complete")); assertEquals(true, meta.get("stoppedSnapshot"));
        assertEquals(false, meta.get("scanComplete"));
    }

    @Test public void ledgerCannotLeakAnotherPhysicalTaskIntoGroup() throws Exception {
        Fixture f = fixture(); f.service.pages.get("two/8/null").getItems().get(0).setTaskId(9);
        Map meta = metadata(export(f.service, f.service.resolveExportableScope(7)));
        assertEquals(false, meta.get("complete")); assertEquals(1, meta.get("fileRows"));
        assertEquals(Collections.singletonList("two"), meta.get("unreachableNodes"));
    }

    @Test public void unrelatedReturnedPhysicalScopeIsRejectedRatherThanDoubleCounted() {
        Fixture f = fixture(); f.service.scopes.get("two/8").getTasks().get(0).setTaskIndex(7);
        rejected(f, 7, HttpStatus.SERVICE_UNAVAILABLE);
    }

    private static void rejected(Fixture f, int taskId, HttpStatus status) {
        try { f.service.resolveExportableScope(taskId); fail("expected " + status); }
        catch (ResponseStatusException expected) { assertEquals(status, expected.getStatus()); }
    }

    private static Fixture fixture() {
        Fixture f = new Fixture();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Arrays.asList("one", "two"));
        ReflectionTestUtils.setField(f.service, "distributedConfig", config);
        ReflectionTestUtils.setField(f.service, "parentTasks", mock(LocalParentTaskService.class));
        ReflectionTestUtils.setField(f.service, "taskCreations", f.creations);
        f.creation.setId("00000000-0000-0000-0000-000000000001");
        f.creation.setPublicTaskId(7); f.creation.setTaskIds(new ArrayList<>(Arrays.asList(7, 8)));
        f.creation.setStatus("COMPLETED"); f.creation.setScanComplete(true);
        BucketPair request = new BucketPair();
        OssKeyMapping mapping = new OssKeyMapping(); mapping.setMode(OssKeyMapping.Mode.ADD_PREFIX); mapping.setTargetPrefix("mapped/");
        request.setKeyMapping(mapping); f.creation.setRequest(request);
        when(f.creations.findByTaskId(7)).thenReturn(f.creation); when(f.creations.findByTaskId(8)).thenReturn(f.creation);
        when(f.creations.get(f.creation.getId())).thenReturn(f.creation);
        for (String node : Arrays.asList("one", "two")) for (int id : Arrays.asList(7, 8)) f.service.scopes.put(node + "/" + id, empty(node, id));
        add(f, "one", 7, "a"); add(f, "two", 8, "b");
        return f;
    }

    private static void add(Fixture f, String node, int id, String key) {
        TaskReportWorkerSnapshot s = f.service.scopes.get(node + "/" + id); s.setFound(true);
        SubTaskStatus task = new SubTaskStatus(); task.setTaskIndex(id); task.setDirection(TransferDirection.OSS_TO_OSS);
        task.setStatus(SubTaskStatus.TaskStatus.COMPLETED); task.setListObjectCount(1L); task.setPutObjectCount(1L);
        task.setListObjectSize(10L); task.setPutObjectSize(10L); s.setTasks(Collections.singletonList(task));
        ReportCoverage coverage = new ReportCoverage(); coverage.setLedgerCoverage("TRACKED"); coverage.setScanComplete(true); coverage.setRevision(1);
        s.setCoverage(coverage);
        FileReportRecord r = new FileReportRecord(); r.setReportTaskId(id); r.setTaskId(id); r.setDirection(TransferDirection.OSS_TO_OSS);
        r.setLogicalKey(key); r.setSourceKey(key); r.setDestinationKey("mapped/" + key); r.setResult("SUCCESS"); r.setSize(10L);
        FileReportPage page = new FileReportPage(); page.setItems(Collections.singletonList(r)); page.setDone(true);
        ReportCoverage pageCoverage = new ReportCoverage(); pageCoverage.setRevision(1); page.setCoverage(pageCoverage);
        f.service.pages.put(node + "/" + id + "/null", page);
    }
    private static TaskReportWorkerSnapshot empty(String node, int id) {
        TaskReportWorkerSnapshot s = new TaskReportWorkerSnapshot(); s.setReportTaskId(id); s.setNode(node); s.setExecutionActive(false); return s;
    }
    private static class Fixture {
        final FakeService service = new FakeService();
        final TaskCreationService creations = mock(TaskCreationService.class);
        final TaskCreationService.Creation creation = new TaskCreationService.Creation();
    }
    private static class FakeService extends TaskReportService {
        final Map<String, TaskReportWorkerSnapshot> scopes = new HashMap<>(), endScopes = new HashMap<>();
        final Set<String> fetched = new HashSet<>();
        final Map<String, FileReportPage> pages = new HashMap<>();
        final List<String> pageReads = new ArrayList<>(); int scopeReads;
        @Override protected TaskReportWorkerSnapshot fetchScope(String node, Integer taskId) {
            scopeReads++; String key = node + "/" + taskId;
            TaskReportWorkerSnapshot s = fetched.contains(key) && endScopes.containsKey(key) ? endScopes.get(key) : scopes.get(key);
            fetched.add(key);
            if (s == null) throw new IllegalStateException("offline"); return s;
        }
        @Override protected FileReportPage fetchPage(String node, Integer taskId, String cursor) {
            String key = node + "/" + taskId + "/" + cursor; pageReads.add(key);
            FileReportPage page = pages.get(key); if (page == null) throw new IllegalStateException("missing page"); return page;
        }
    }
    private static Map<String, String> export(FakeService service, TaskReportService.ReportScope scope) throws Exception {
        ByteArrayOutputStream output = new ByteArrayOutputStream(); service.write(scope, output);
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
}
