package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.interceptor.AuthInterceptor;
import com.transfer.tools.model.*;
import com.transfer.tools.service.*;
import org.junit.Before;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.MvcResult;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;

import java.io.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.zip.*;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.*;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.*;

/** Real eligibility and ZIP generation behind all three authenticated public entry points. */
public class StoppedReportDownloadTest {
    private MockMvc mvc;
    private TaskReportWorkerSnapshot snapshot;

    @Before public void setup() {
        SubTaskStatus task = new SubTaskStatus(); task.setTaskIndex(12); task.setDirection(TransferDirection.OSS_TO_OSS);
        task.setStatus(SubTaskStatus.TaskStatus.STOPPED); task.setListObjectCount(1L); task.setListObjectSize(4L);
        task.setPutObjectCount(1L); task.setPutObjectSize(4L);
        ReportCoverage coverage = new ReportCoverage(); coverage.setLedgerCoverage("TRACKED");
        coverage.setScanComplete(true); coverage.setRevision(1);
        snapshot = new TaskReportWorkerSnapshot(); snapshot.setReportTaskId(12); snapshot.setFound(true);
        snapshot.setNode("one"); snapshot.setTasks(Collections.singletonList(task));
        snapshot.setCoverage(coverage); snapshot.setExecutionActive(false);
        FileReportRecord row = new FileReportRecord(); row.setTaskId(12); row.setReportTaskId(12);
        row.setLogicalKey("already-copied"); row.setSize(4L); row.setResult("SUCCESS");
        FileReportPage page = new FileReportPage(); page.setDone(true); page.setCoverage(coverage);
        page.setItems(Collections.singletonList(row));
        TaskReportService reports = new TaskReportService() {
            @Override protected TaskReportWorkerSnapshot fetchScope(String node, Integer taskId) { return snapshot; }
            @Override protected FileReportPage fetchPage(String node, Integer taskId, String cursor) { return page; }
        };
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("one"));
        ReflectionTestUtils.setField(reports, "distributedConfig", config);
        ReflectionTestUtils.setField(reports, "parentTasks", mock(LocalParentTaskService.class));
        UserService users = mock(UserService.class); when(users.getUsernameByToken("session")).thenReturn("admin");
        ReportDownloadTicketService tickets = new ReportDownloadTicketService();
        ReflectionTestUtils.setField(tickets, "userService", users);
        TaskReportDownloadController download = new TaskReportDownloadController();
        ReflectionTestUtils.setField(download, "reportService", reports); ReflectionTestUtils.setField(download, "ticketService", tickets);
        MasterTaskReportController direct = new MasterTaskReportController(); ReflectionTestUtils.setField(direct, "reports", reports);
        AuthInterceptor auth = new AuthInterceptor(); ReflectionTestUtils.setField(auth, "userService", users);
        ReflectionTestUtils.setField(auth, "reportDownloadTicketService", tickets);
        ReflectionTestUtils.setField(auth, "objectMapper", new ObjectMapper()); ReflectionTestUtils.setField(auth, "distributedConfig", config);
        mvc = MockMvcBuilders.standaloneSetup(download, direct).addInterceptors(auth).build();
    }

    @Test public void directZipAndTicketPostExportTheSameExplicitlyPartialStoppedSnapshot() throws Exception {
        MvcResult direct = mvc.perform(get("/api/master/task-reports/12").header("Authorization", "Bearer session"))
                .andExpect(request().asyncStarted()).andReturn();
        assertStoppedZip(mvc.perform(asyncDispatch(direct)).andExpect(status().isOk()).andReturn());
        String ticket = issue();
        MvcResult post = mvc.perform(post("/api/master/task-reports/12/download")
                .contentType(MediaType.APPLICATION_FORM_URLENCODED).param("ticket", ticket))
                .andExpect(request().asyncStarted()).andReturn();
        assertStoppedZip(mvc.perform(asyncDispatch(post)).andExpect(status().isOk()).andReturn());
    }

    @Test public void resumeAfterTicketAndStillDrainingIoCannotBypassAnyEntryPoint() throws Exception {
        String ticket = issue();
        snapshot.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.RUNNING);
        mvc.perform(post("/api/master/task-reports/12/download").contentType(MediaType.APPLICATION_FORM_URLENCODED)
                .param("ticket", ticket)).andExpect(status().isConflict()).andExpect(header().doesNotExist("Content-Disposition"));
        snapshot.getTasks().get(0).setStatus(SubTaskStatus.TaskStatus.STOPPED); snapshot.setExecutionActive(true);
        mvc.perform(get("/api/master/task-reports/12").header("Authorization", "Bearer session"))
                .andExpect(status().isConflict()).andExpect(header().doesNotExist("Content-Disposition"));
        mvc.perform(get("/api/master/task-reports/12/download-ticket").header("Authorization", "Bearer session"))
                .andExpect(status().isConflict()).andExpect(jsonPath("success").value(false));
    }

    private String issue() throws Exception {
        MvcResult result = mvc.perform(get("/api/master/task-reports/12/download-ticket").header("Authorization", "Bearer session"))
                .andExpect(status().isOk()).andReturn();
        Map body = new ObjectMapper().readValue(result.getResponse().getContentAsByteArray(), Map.class);
        return (String) ((Map) body.get("data")).get("ticket");
    }

    private void assertStoppedZip(MvcResult result) throws Exception {
        assertEquals("attachment; filename=task-12-report.zip", result.getResponse().getHeader("Content-Disposition"));
        Map<String, String> entries = new HashMap<>();
        try (ZipInputStream zip = new ZipInputStream(new ByteArrayInputStream(result.getResponse().getContentAsByteArray()), StandardCharsets.UTF_8)) {
            ZipEntry entry;
            while ((entry = zip.getNextEntry()) != null) {
                ByteArrayOutputStream data = new ByteArrayOutputStream(); byte[] bytes = new byte[1024]; int count;
                while ((count = zip.read(bytes)) != -1) data.write(bytes, 0, count);
                entries.put(entry.getName(), new String(data.toByteArray(), StandardCharsets.UTF_8));
            }
        }
        assertEquals(4, entries.size()); assertTrue(entries.get("file-results.csv").contains("already-copied"));
        assertTrue(entries.get("task-summary.csv").contains("STOPPED"));
        Map metadata = new ObjectMapper().readValue(entries.get("report-metadata.json"), Map.class);
        assertEquals(false, metadata.get("complete")); assertEquals(true, metadata.get("stoppedSnapshot"));
        assertEquals(true, metadata.get("countsReconciled")); assertEquals("PARTIAL", metadata.get("coverage"));
        assertTrue(((List) metadata.get("partialReasons")).contains("TASK_STOPPED"));
    }
}
