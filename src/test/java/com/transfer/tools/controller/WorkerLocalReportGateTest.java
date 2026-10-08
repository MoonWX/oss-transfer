package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.interceptor.AuthInterceptor;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.service.LocalReportService;
import com.transfer.tools.service.LocalTransferQueryService;
import com.transfer.tools.service.LocalTransferService;
import com.transfer.tools.service.TaskReportService;
import com.transfer.tools.service.UserService;
import org.junit.Before;
import org.junit.Test;
import org.springframework.http.HttpStatus;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.MvcResult;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;
import org.springframework.web.server.ResponseStatusException;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.Collections;
import java.util.List;
import java.util.zip.ZipEntry;
import java.util.zip.ZipInputStream;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.*;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.*;

public class WorkerLocalReportGateTest {
    private MockMvc mvc;
    private DistributedConfig config;
    private LocalTransferService transfers;
    private LocalTransferQueryService queries;
    private TaskReportService reports;

    @Before public void setup() {
        config = mock(DistributedConfig.class);
        when(config.isMaster()).thenReturn(true);
        transfers = mock(LocalTransferService.class);
        queries = mock(LocalTransferQueryService.class);
        reports = mock(TaskReportService.class);
        when(queries.localFailures()).thenReturn(Collections.emptyList());
        WorkerLocalTransferController controller = new WorkerLocalTransferController();
        ReflectionTestUtils.setField(controller, "distributedConfig", config);
        ReflectionTestUtils.setField(controller, "localTransferService", transfers);
        ReflectionTestUtils.setField(controller, "queryService", queries);
        ReflectionTestUtils.setField(controller, "taskReportService", reports);
        ReflectionTestUtils.setField(controller, "reportService", new LocalReportService());
        UserService users = mock(UserService.class);
        when(users.getUsernameByToken("session")).thenReturn("admin");
        AuthInterceptor auth = new AuthInterceptor();
        ReflectionTestUtils.setField(auth, "userService", users);
        ReflectionTestUtils.setField(auth, "distributedConfig", config);
        ReflectionTestUtils.setField(auth, "objectMapper", new ObjectMapper());
        mvc = MockMvcBuilders.standaloneSetup(controller).addInterceptors(auth).build();
    }

    @Test public void workerRejectsEvenAuthenticatedLegacyDownloadBeforeReadingLocalData() throws Exception {
        when(config.isMaster()).thenReturn(false);
        mvc.perform(get("/api/worker/local-transfers/report").header("Authorization", "Bearer session"))
                .andExpect(status().isForbidden()).andExpect(jsonPath("success").value(false))
                .andExpect(content().contentType("application/json;charset=UTF-8"))
                .andExpect(jsonPath("message").value("请从主节点导出任务报告"))
                .andExpect(header().string("Cache-Control", "no-store"))
                .andExpect(header().doesNotExist("Content-Disposition"));
        verifyZeroInteractions(transfers, queries, reports);
    }

    @Test public void masterCannotExportFailedOrFinishedLocalRowsWhenGlobalTaskHasNotEnded() throws Exception {
        doThrow(new ResponseStatusException(HttpStatus.CONFLICT, "任务尚未结束"))
                .when(reports).resolveExportableScope(7);
        for (SubTaskStatus.TaskStatus localStatus : Arrays.asList(SubTaskStatus.TaskStatus.FAILED,
                SubTaskStatus.TaskStatus.COMPLETED, SubTaskStatus.TaskStatus.STOPPED)) {
            when(transfers.localTasks()).thenReturn(Collections.singletonList(task(-31, 7, localStatus)));
            mvc.perform(get("/api/worker/local-transfers/report").header("Authorization", "Bearer session"))
                    .andExpect(status().isConflict()).andExpect(jsonPath("success").value(false))
                    .andExpect(header().doesNotExist("Content-Disposition"));
        }
        verify(queries, never()).localFailures();
        verify(reports, times(3)).resolveExportableScope(7);
    }

    @Test public void masterDoesNotTreatMissingRemoteExecutionRecordsAsFinished() throws Exception {
        when(transfers.localTasks()).thenReturn(Collections.singletonList(task(-31, 7, SubTaskStatus.TaskStatus.COMPLETED)));
        when(reports.resolveExportableScope(7)).thenThrow(new ResponseStatusException(HttpStatus.SERVICE_UNAVAILABLE,
                "执行节点不可达，无法确认任务已结束"));
        mvc.perform(get("/api/worker/local-transfers/report").header("Authorization", "Bearer session"))
                .andExpect(status().isServiceUnavailable()).andExpect(jsonPath("success").value(false))
                .andExpect(header().doesNotExist("Content-Disposition"));
        verify(queries, never()).localFailures();
    }

    @Test public void masterChecksEachLogicalTaskOnceAndStreamsNodeLocalTerminalRows() throws Exception {
        SubTaskStatus discovery = task(7, 7, SubTaskStatus.TaskStatus.COMPLETED);
        discovery.setShardId("discovery");
        List<SubTaskStatus> tasks = Arrays.asList(task(-31, 7, SubTaskStatus.TaskStatus.COMPLETED),
                task(-32, 7, SubTaskStatus.TaskStatus.PARTIAL_FAILED), discovery,
                task(12, null, SubTaskStatus.TaskStatus.STOPPED));
        when(transfers.localTasks()).thenReturn(tasks);
        MvcResult pending = mvc.perform(get("/api/worker/local-transfers/report")
                .header("Authorization", "Bearer session")).andExpect(request().asyncStarted()).andReturn();
        MvcResult download = mvc.perform(asyncDispatch(pending)).andExpect(status().isOk())
                .andExpect(header().string("Content-Disposition", "attachment; filename=local-transfer-report.zip"))
                .andExpect(header().string("Cache-Control", "no-store")).andReturn();
        verify(reports).resolveExportableScope(7);
        verify(reports).resolveExportableScope(12);
        verifyNoMoreInteractions(reports);
        verify(transfers).localTasks();
        verify(queries).localFailures();
        String summary = zipEntry(download.getResponse().getContentAsByteArray(), "task-summary.csv");
        assertEquals(5, summary.split("\n").length);
        assertTrue(summary.contains("PARTIAL_FAILED"));
        assertTrue(summary.contains("\n\"TASK\",\"12\",,"));
        assertNotNull(zipEntry(download.getResponse().getContentAsByteArray(), "exception-details.csv"));
        java.util.Map metadata = new ObjectMapper().readValue(zipEntry(download.getResponse().getContentAsByteArray(), "report-metadata.json"), java.util.Map.class);
        assertEquals(false, metadata.get("complete")); assertEquals(true, metadata.get("stoppedSnapshot"));
        assertEquals(Collections.singletonList(12), metadata.get("stoppedTaskIds"));
        assertTrue(((List) metadata.get("partialReasons")).contains("TASK_STOPPED"));
    }

    @Test public void mixedStoppedAndActiveTasksCannotExportGlobalReport() throws Exception {
        when(transfers.localTasks()).thenReturn(Arrays.asList(task(-31, 7, SubTaskStatus.TaskStatus.STOPPED),
                task(-40, 8, SubTaskStatus.TaskStatus.RUNNING)));
        when(reports.resolveExportableScope(8)).thenThrow(new ResponseStatusException(HttpStatus.CONFLICT, "running child"));
        mvc.perform(get("/api/worker/local-transfers/report").header("Authorization", "Bearer session"))
                .andExpect(status().isConflict()).andExpect(header().doesNotExist("Content-Disposition"));
        verify(queries, never()).localFailures();
    }

    @Test public void stoppedParentIsMarkedEvenWhenThisNodesOwnRowsAreCompleted() throws Exception {
        when(transfers.localTasks()).thenReturn(Collections.singletonList(task(-31, 7, SubTaskStatus.TaskStatus.COMPLETED)));
        TaskReportService.ReportScope scope = mock(TaskReportService.ReportScope.class);
        when(scope.isStoppedSnapshot()).thenReturn(true);
        when(reports.resolveExportableScope(7)).thenReturn(scope);
        MvcResult pending = mvc.perform(get("/api/worker/local-transfers/report").header("Authorization", "Bearer session"))
                .andExpect(request().asyncStarted()).andReturn();
        MvcResult download = mvc.perform(asyncDispatch(pending)).andExpect(status().isOk()).andReturn();
        java.util.Map metadata = new ObjectMapper().readValue(zipEntry(download.getResponse().getContentAsByteArray(), "report-metadata.json"), java.util.Map.class);
        assertEquals(true, metadata.get("stoppedSnapshot"));
        assertEquals(Collections.singletonList(7), metadata.get("stoppedTaskIds"));
    }

    private static SubTaskStatus task(int taskId, Integer parentId, SubTaskStatus.TaskStatus status) {
        SubTaskStatus task = new SubTaskStatus();
        task.setTaskIndex(taskId); task.setParentTaskId(parentId); task.setStatus(status);
        return task;
    }

    private static String zipEntry(byte[] bytes, String name) throws Exception {
        try (ZipInputStream zip = new ZipInputStream(new ByteArrayInputStream(bytes), StandardCharsets.UTF_8)) {
            ZipEntry entry;
            while ((entry = zip.getNextEntry()) != null) {
                if (!entry.getName().equals(name)) continue;
                ByteArrayOutputStream output = new ByteArrayOutputStream();
                byte[] buffer = new byte[1024]; int count;
                while ((count = zip.read(buffer)) != -1) output.write(buffer, 0, count);
                return new String(output.toByteArray(), StandardCharsets.UTF_8);
            }
        }
        fail("Missing ZIP entry: " + name);
        return null;
    }
}
