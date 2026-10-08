package com.transfer.tools.controller;

import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.service.MasterLocalTransferService;
import com.transfer.tools.service.LocalReportService;
import com.transfer.tools.service.TaskReportService;
import org.junit.Before;
import org.junit.Test;
import org.springframework.http.HttpStatus;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.web.server.ResponseStatusException;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.mockito.ArgumentMatchers.*;

public class MasterLocalReportGateTest {
    private MasterLocalTransferController controller;
    private MasterLocalTransferService master;
    private TaskReportService reports;
    private SubTaskStatus task;

    @Before public void setup() {
        controller = new MasterLocalTransferController(); master = mock(MasterLocalTransferService.class);
        reports = mock(TaskReportService.class);
        ReflectionTestUtils.setField(controller, "masterService", master);
        ReflectionTestUtils.setField(controller, "reportService", mock(LocalReportService.class));
        ReflectionTestUtils.setField(controller, "taskReportService", reports);
        task = new SubTaskStatus(); task.setTaskIndex(7); task.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
        when(master.reportTasks(anyList())).thenReturn(Collections.singletonList(task));
    }

    @Test public void unfinishedTasksAndUnavailableWorkersBlockLegacyExport() {
        for (SubTaskStatus.TaskStatus status : Arrays.asList(SubTaskStatus.TaskStatus.PENDING,
                SubTaskStatus.TaskStatus.RUNNING, SubTaskStatus.TaskStatus.LIST_DONE)) {
            task.setStatus(status); rejected(HttpStatus.CONFLICT);
        }
        verify(master, never()).reportFailures(anyList());
        task.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
        when(master.reportTasks(anyList())).thenAnswer(call -> {
            ((List<String>) call.getArgument(0)).add("offline"); return Collections.singletonList(task);
        });
        rejected(HttpStatus.SERVICE_UNAVAILABLE);
    }

    @Test public void terminalAggregateIsRecheckedForRunningChildBeforeLegacyExport() {
        when(reports.resolveExportableScope(7)).thenThrow(new ResponseStatusException(HttpStatus.CONFLICT, "running child"));
        for (SubTaskStatus.TaskStatus status : Arrays.asList(SubTaskStatus.TaskStatus.FAILED, SubTaskStatus.TaskStatus.STOPPED)) {
            task.setStatus(status); rejected(HttpStatus.CONFLICT);
        }
        verify(master, never()).reportFailures(anyList());
    }

    @Test public void confirmedTerminalTaskCanExport() {
        assertEquals(HttpStatus.OK, controller.report().getStatusCode());
        verify(reports).resolveExportableScope(7);
        verify(master).reportFailures(anyList());
    }

    @Test public void confirmedStoppedTaskCanExportButMixedActiveTaskCannot() {
        task.setStatus(SubTaskStatus.TaskStatus.STOPPED);
        assertEquals(HttpStatus.OK, controller.report().getStatusCode());
        verify(reports).resolveExportableScope(7);
        SubTaskStatus active = new SubTaskStatus(); active.setTaskIndex(8); active.setStatus(SubTaskStatus.TaskStatus.RUNNING);
        when(master.reportTasks(anyList())).thenReturn(Arrays.asList(task, active));
        rejected(HttpStatus.CONFLICT);
        verify(master, times(1)).reportFailures(anyList());
    }

    @Test public void workerFailureWhileCollectingExceptionsRejectsBeforeAttachment() {
        task.setStatus(SubTaskStatus.TaskStatus.STOPPED);
        when(master.reportFailures(anyList())).thenAnswer(call -> {
            ((List<String>) call.getArgument(0)).add("offline"); return Collections.emptyList();
        });
        rejected(HttpStatus.SERVICE_UNAVAILABLE);
    }

    private void rejected(HttpStatus status) {
        try { controller.report(); fail("export should be rejected"); }
        catch (ResponseStatusException expected) { assertEquals(status, expected.getStatus()); }
    }
}
