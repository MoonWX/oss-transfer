package com.transfer.tools.controller;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.interceptor.AuthInterceptor;
import com.transfer.tools.service.ReportDownloadTicketService;
import com.transfer.tools.service.TaskReportService;
import com.transfer.tools.service.UserService;
import org.junit.Before;
import org.junit.Test;
import org.springframework.http.HttpStatus;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.MvcResult;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;
import org.springframework.web.server.ResponseStatusException;

import java.io.OutputStream;
import java.util.Map;
import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.*;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.*;

public class TaskReportDownloadControllerTest {
    private MockMvc mvc;
    private ReportDownloadTicketService tickets;
    private TaskReportService reports;
    private TaskReportService.ReportScope scope;
    private UserService users;

    @Before public void setup() throws Exception {
        users = mock(UserService.class);
        when(users.getUsernameByToken("session")).thenReturn("admin");
        tickets = new ReportDownloadTicketService();
        ReflectionTestUtils.setField(tickets, "userService", users);
        reports = mock(TaskReportService.class);
        scope = mock(TaskReportService.ReportScope.class);
        when(scope.getTaskId()).thenReturn(12);
        when(reports.resolveExportableScope(12)).thenReturn(scope);
        when(reports.resolveExportableScope(404)).thenThrow(new ResponseStatusException(HttpStatus.NOT_FOUND, "任务不存在"));
        doAnswer(invocation -> {
            ((OutputStream) invocation.getArgument(1)).write(new byte[]{'P','K',3,4});
            return null;
        }).when(reports).write(eq(scope), any(OutputStream.class));
        TaskReportDownloadController controller = new TaskReportDownloadController();
        ReflectionTestUtils.setField(controller, "reportService", reports);
        ReflectionTestUtils.setField(controller, "ticketService", tickets);
        MasterTaskReportController direct = new MasterTaskReportController();
        ReflectionTestUtils.setField(direct, "reports", reports);
        AuthInterceptor auth = new AuthInterceptor();
        ReflectionTestUtils.setField(auth, "userService", users);
        ReflectionTestUtils.setField(auth, "reportDownloadTicketService", tickets);
        ReflectionTestUtils.setField(auth, "objectMapper", new ObjectMapper());
        ReflectionTestUtils.setField(auth, "distributedConfig", new DistributedConfig());
        mvc = MockMvcBuilders.standaloneSetup(controller, direct).addInterceptors(auth).build();
    }

    @Test public void nativePostStreamsAttachmentWithoutSessionTokenInUrl() throws Exception {
        MvcResult issued = mvc.perform(get("/api/master/task-reports/12/download-ticket")
                .header("Authorization", "Bearer session"))
                .andExpect(status().isOk()).andExpect(header().string("Cache-Control", "no-store")).andReturn();
        Map response = new ObjectMapper().readValue(issued.getResponse().getContentAsByteArray(), Map.class);
        String ticket = (String) ((Map) response.get("data")).get("ticket");
        assertFalse(issued.getResponse().getContentAsString().contains("session"));
        MvcResult download = mvc.perform(post("/api/master/task-reports/12/download")
                .contentType(MediaType.APPLICATION_FORM_URLENCODED).param("ticket", ticket))
                .andExpect(request().asyncStarted()).andReturn();
        mvc.perform(asyncDispatch(download)).andExpect(status().isOk())
                .andExpect(header().string("Content-Disposition", "attachment; filename=task-12-report.zip"))
                .andExpect(content().bytes(new byte[]{'P','K',3,4}));
        mvc.perform(post("/api/master/task-reports/12/download")
                .contentType(MediaType.APPLICATION_FORM_URLENCODED).param("ticket", ticket))
                .andExpect(status().isUnauthorized());
    }

    @Test public void childAliasUsesLogicalIdForFilenameWithoutChangingTicketBinding() throws Exception {
        when(reports.resolveExportableScope(13)).thenReturn(scope);
        String ticket = tickets.issue(13, "session");
        MvcResult download = mvc.perform(post("/api/master/task-reports/13/download")
                .contentType(MediaType.APPLICATION_FORM_URLENCODED).param("ticket", ticket))
                .andExpect(request().asyncStarted()).andReturn();
        mvc.perform(asyncDispatch(download)).andExpect(status().isOk())
                .andExpect(header().string("Content-Disposition", "attachment; filename=task-12-report.zip"));
        MvcResult direct = mvc.perform(get("/api/master/task-reports/13").header("Authorization", "Bearer session"))
                .andExpect(request().asyncStarted()).andReturn();
        mvc.perform(asyncDispatch(direct)).andExpect(status().isOk())
                .andExpect(header().string("Content-Disposition", "attachment; filename=task-12-report.zip"));
    }

    @Test public void ticketCannotAuthorizeGetAnotherTaskOrGeneralApi() throws Exception {
        String ticket = tickets.issue(12, "session");
        mvc.perform(get("/api/master/task-reports/12/download-ticket").param("ticket", ticket))
                .andExpect(status().isUnauthorized());
        mvc.perform(post("/api/master/task-reports/13/download")
                .contentType(MediaType.APPLICATION_FORM_URLENCODED).param("ticket", ticket))
                .andExpect(status().isUnauthorized());
        verify(reports, never()).write(any(TaskReportService.ReportScope.class), any(OutputStream.class));
    }

    @Test public void urlTicketAndSessionParametersAreNotAccepted() throws Exception {
        String ticket = tickets.issue(12, "session");
        mvc.perform(post("/api/master/task-reports/12/download?ticket=" + ticket)
                .contentType(MediaType.APPLICATION_FORM_URLENCODED))
                .andExpect(status().isUnauthorized());
        mvc.perform(post("/api/master/task-reports/12/download")
                .contentType(MediaType.APPLICATION_FORM_URLENCODED).param("token", "session"))
                .andExpect(status().isUnauthorized());
    }

    @Test public void unknownTaskFailsBeforeIssuingTicketAndMissingSessionIsRejected() throws Exception {
        mvc.perform(get("/api/master/task-reports/404/download-ticket").header("Authorization", "Bearer session"))
                .andExpect(status().isNotFound()).andExpect(jsonPath("success").value(false));
        mvc.perform(get("/api/master/task-reports/12/download-ticket"))
                .andExpect(status().isUnauthorized());
    }

    @Test public void unfinishedTaskCannotIssueTicketAndIsRecheckedAtDownloadTime() throws Exception {
        String ticket = tickets.issue(12, "session");
        when(reports.resolveExportableScope(12)).thenThrow(new ResponseStatusException(HttpStatus.CONFLICT, "任务尚未结束"));
        mvc.perform(get("/api/master/task-reports/12").header("Authorization", "Bearer session"))
                .andExpect(status().isConflict()).andExpect(jsonPath("success").value(false));
        mvc.perform(get("/api/master/task-reports/12/download-ticket").header("Authorization", "Bearer session"))
                .andExpect(status().isConflict()).andExpect(jsonPath("success").value(false));
        mvc.perform(post("/api/master/task-reports/12/download")
                .contentType(MediaType.APPLICATION_FORM_URLENCODED).param("ticket", ticket))
                .andExpect(status().isConflict()).andExpect(jsonPath("success").value(false));
        verify(reports, never()).write(any(TaskReportService.ReportScope.class), any(OutputStream.class));
    }
}
