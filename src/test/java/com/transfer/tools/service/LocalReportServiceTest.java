package com.transfer.tools.service;

import com.transfer.tools.model.*;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.junit.Test;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.Collections;
import java.util.HashMap;
import java.util.Map;
import java.util.zip.ZipEntry;
import java.util.zip.ZipInputStream;

import static org.junit.Assert.*;

public class LocalReportServiceTest {
    @Test public void parentSummaryAndRetainedFailureUsePublicReportIdentifiers() throws Exception {
        SubTaskStatus parent = new SubTaskStatus();
        parent.setTaskIndex(224); parent.setParentTaskId(224);
        parent.setDirection(TransferDirection.OSS_TO_LOCAL);
        parent.setStatus(SubTaskStatus.TaskStatus.PARTIAL_FAILED);
        FailedKey failure = new FailedKey();
        failure.setTaskIndex(-849684113); failure.setParentTaskId(224);
        failure.setShardId("224-oss-0"); failure.setKey("data.bin");
        Map<String, String> entries = exported(Collections.singletonList(parent), Collections.singletonList(failure));
        assertTrue(entries.get("task-summary.csv").startsWith("rowType,parentTaskId,childTaskId,direction,"));
        assertTrue(entries.get("task-summary.csv").contains("\"TASK\",\"224\",,\"OSS_TO_LOCAL\""));
        assertTrue(entries.get("exception-details.csv").startsWith("parentTaskId,childTaskId,direction,"));
        assertTrue(entries.get("exception-details.csv").contains("\"224\",\"224-1\","));
        assertFalse(entries.get("exception-details.csv").contains("-849684113"));
        assertFalse(entries.get("exception-details.csv").contains("224-oss-0"));
        assertEquals(2, new ObjectMapper().readValue(entries.get("report-metadata.json"), Map.class).get("schemaVersion"));
        assertEquals(Integer.valueOf(-849684113), failure.getTaskIndex());
        assertEquals("224-oss-0", failure.getShardId());
    }

    @Test public void childSummaryAndFailureSharePersistedSequenceInsteadOfPhysicalId() throws Exception {
        SubTaskStatus child = new SubTaskStatus();
        child.setTaskIndex(-42); child.setParentTaskId(225); child.setShardId("225-local-0");
        child.setShardIndex(4); child.setWorkerNode("worker-a");
        child.setDirection(TransferDirection.LOCAL_TO_OSS);
        FailedKey failure = new FailedKey();
        failure.setTaskIndex(-42); failure.setParentTaskId(225); failure.setShardId("225-local-0");
        failure.setWorkerNode("worker-a");
        Map<String, String> entries = exported(Collections.singletonList(child), Collections.singletonList(failure));
        assertTrue(entries.get("task-summary.csv").contains("\"CHILD_TASK\",\"225\",\"225-5\","));
        assertTrue(entries.get("exception-details.csv").contains("\"225\",\"225-5\","));
        assertFalse(entries.get("task-summary.csv").contains("-42"));
        assertEquals(Integer.valueOf(4), child.getShardIndex());
    }

    @Test public void discoveryAndUnknownHistoricalFailuresDoNotInventChildNumbers() throws Exception {
        FailedKey discovery = new FailedKey();
        discovery.setTaskIndex(226); discovery.setParentTaskId(226); discovery.setShardId("discovery");
        discovery.setKey("pending.bin");
        FailedKey unknown = new FailedKey(); unknown.setTaskIndex(-99); unknown.setKey("old.bin");
        Map<String, String> entries = exported(Collections.emptyList(), Arrays.asList(discovery, unknown));
        String errors = entries.get("exception-details.csv");
        assertTrue(errors.contains("\"226\",,,,\"pending.bin\""));
        assertTrue(errors.contains("\n,,,,\"old.bin\""));
        assertFalse(errors.contains("226-1")); assertFalse(errors.contains("-99"));
    }

    private Map<String, String> exported(java.util.Collection<SubTaskStatus> tasks,
                                         java.util.Collection<FailedKey> failures) throws Exception {
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        new LocalReportService().write(output, tasks, failures, Collections.emptyList());
        Map<String, String> result = new HashMap<>();
        try (ZipInputStream zip = new ZipInputStream(new ByteArrayInputStream(output.toByteArray()), StandardCharsets.UTF_8)) {
            ZipEntry entry;
            while ((entry = zip.getNextEntry()) != null) {
                ByteArrayOutputStream bytes = new ByteArrayOutputStream(); byte[] buffer = new byte[1024]; int count;
                while ((count = zip.read(buffer)) != -1) bytes.write(buffer, 0, count);
                result.put(entry.getName(), new String(bytes.toByteArray(), StandardCharsets.UTF_8));
            }
        }
        return result;
    }

    @Test
    public void producesTwoUtf8CsvFilesWithLegacyMetadataAndEscapesValues() throws Exception {
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(1);
        task.setDirection(TransferDirection.OSS_TO_LOCAL);
        task.setStatus(SubTaskStatus.TaskStatus.PARTIAL_FAILED);
        task.setSourceEndpoint(new TransferEndpoint(EndpointType.OSS, "alias", "bucket", null, null, null, null));
        task.setDestinationEndpoint(new TransferEndpoint(EndpointType.LOCAL, null, null, null, null, "127.0.0.1", "/tmp/data"));
        FailedKey failure = new FailedKey();
        failure.setTaskIndex(1);
        failure.setDirection(TransferDirection.OSS_TO_LOCAL);
        failure.setKey("comma,key");
        failure.setErrorMessage("bad \"value\"");
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        new LocalReportService().write(output, Collections.singletonList(task), Collections.singletonList(failure), Collections.emptyList());

        Map<String, String> entries = new HashMap<>();
        try (ZipInputStream zip = new ZipInputStream(new ByteArrayInputStream(output.toByteArray()), StandardCharsets.UTF_8)) {
            ZipEntry entry;
            byte[] buffer = new byte[1024];
            while ((entry = zip.getNextEntry()) != null) {
                ByteArrayOutputStream content = new ByteArrayOutputStream();
                int read;
                while ((read = zip.read(buffer)) > 0) content.write(buffer, 0, read);
                entries.put(entry.getName(), new String(content.toByteArray(), StandardCharsets.UTF_8));
            }
        }
        assertEquals(3, entries.size());
        Map metadata = new ObjectMapper().readValue(entries.get("report-metadata.json"), Map.class);
        assertEquals(false, metadata.get("complete"));
        assertEquals("LEGACY_OR_PARTIAL", metadata.get("coverage"));
        assertTrue(entries.containsKey("task-summary.csv"));
        assertTrue(entries.get("exception-details.csv").contains("\"comma,key\""));
        assertTrue(entries.get("exception-details.csv").contains("\"bad \"\"value\"\"\""));
        assertFalse(entries.get("task-summary.csv").toLowerCase().contains("secretkey"));
    }

    @Test public void spreadsheetFormulaLikeObjectKeysAreExportedAsText() throws Exception {
        FailedKey failure = new FailedKey();
        failure.setKey("=HYPERLINK(\"https://example.invalid\")");
        failure.setErrorMessage("@SUM(1,2)");
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        new LocalReportService().write(output, Collections.emptyList(), Collections.singletonList(failure), Collections.emptyList());
        try (ZipInputStream zip = new ZipInputStream(new ByteArrayInputStream(output.toByteArray()), StandardCharsets.UTF_8)) {
            ZipEntry entry;
            while ((entry = zip.getNextEntry()) != null) {
                if (!"exception-details.csv".equals(entry.getName())) continue;
                ByteArrayOutputStream content = new ByteArrayOutputStream();
                byte[] buffer = new byte[1024]; int read;
                while ((read = zip.read(buffer)) != -1) content.write(buffer, 0, read);
                String csv = new String(content.toByteArray(), StandardCharsets.UTF_8);
                assertTrue(csv.contains("\"'=HYPERLINK(\"\"https://example.invalid\"\")\""));
                assertTrue(csv.contains("\"'@SUM(1,2)\""));
                return;
            }
        }
        fail("exception CSV was missing");
    }
}
