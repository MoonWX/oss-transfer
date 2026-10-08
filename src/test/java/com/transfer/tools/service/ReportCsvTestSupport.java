package com.transfer.tools.service;

import java.util.*;

import static org.junit.Assert.*;

/** Read quoted report cells without breaking commas, quotes or newlines in object keys. */
final class ReportCsvTestSupport {
    private ReportCsvTestSupport() { }

    static List<Map<String, String>> rows(String csv) {
        List<List<String>> records = records(csv);
        List<Map<String, String>> result = new ArrayList<>();
        List<String> header = records.get(0);
        for (int i = 1; i < records.size(); i++) {
            assertEquals("CSV field count at row " + i, header.size(), records.get(i).size());
            Map<String, String> row = new LinkedHashMap<>();
            for (int j = 0; j < header.size(); j++) row.put(header.get(j), records.get(i).get(j));
            result.add(row);
        }
        return result;
    }

    static void assertPublicIds(Map<String, String> archive, int parentId) {
        for (String file : Arrays.asList("task-summary.csv", "file-results.csv", "exception-details.csv")) {
            List<String> header = records(archive.get(file)).get(0);
            assertTrue(file, header.containsAll(Arrays.asList("parentTaskId", "childTaskId")));
            assertFalse(file, header.contains("reportTaskId"));
            assertFalse(file, header.contains("taskId"));
            assertFalse(file, header.contains("shardId"));
            for (Map<String, String> row : rows(archive.get(file))) {
                assertEquals(file, String.valueOf(parentId), row.get("parentTaskId"));
                String child = row.get("childTaskId");
                assertTrue(file + ": " + child, child.isEmpty() || child.matches(parentId + "-[1-9][0-9]*"));
            }
        }
        for (Map<String, String> row : rows(archive.get("task-summary.csv"))) {
            assertTrue(Arrays.asList("TASK", "CHILD_TASK").contains(row.get("rowType")));
            if ("TASK".equals(row.get("rowType"))) assertEquals("", row.get("childTaskId"));
        }
    }

    private static List<List<String>> records(String csv) {
        List<List<String>> result = new ArrayList<>();
        List<String> record = new ArrayList<>();
        StringBuilder value = new StringBuilder();
        boolean quoted = false;
        for (int i = 0; i < csv.length(); i++) {
            char c = csv.charAt(i);
            if (c == '"') {
                if (quoted && i + 1 < csv.length() && csv.charAt(i + 1) == '"') { value.append(c); i++; }
                else quoted = !quoted;
            } else if (!quoted && c == ',') {
                record.add(value.toString()); value.setLength(0);
            } else if (!quoted && (c == '\r' || c == '\n')) {
                if (c == '\r' && i + 1 < csv.length() && csv.charAt(i + 1) == '\n') i++;
                record.add(value.toString()); result.add(record);
                record = new ArrayList<>(); value.setLength(0);
            } else value.append(c);
        }
        assertFalse("Unterminated CSV string", quoted);
        if (value.length() > 0 || !record.isEmpty()) { record.add(value.toString()); result.add(record); }
        return result;
    }
}
