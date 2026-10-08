package com.transfer.tools.controller;

import com.transfer.tools.model.*;
import com.transfer.tools.service.FileReportLedgerService;
import com.transfer.tools.service.TaskReportWorkerService;
import org.junit.Test;
import org.springframework.http.HttpStatus;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.web.server.ResponseStatusException;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class WorkerTaskReportControllerTest {
    @Test
    public void missingTaskDoesNotInitializeHistoricalLedger() {
        WorkerTaskReportController controller = new WorkerTaskReportController();
        TaskReportWorkerService scopes = mock(TaskReportWorkerService.class);
        FileReportLedgerService ledger = mock(FileReportLedgerService.class);
        ReflectionTestUtils.setField(controller, "reports", scopes);
        ReflectionTestUtils.setField(controller, "ledger", ledger);
        when(scopes.scope(7)).thenReturn(new TaskReportWorkerSnapshot());
        try { controller.files(7, null, 500); fail("expected 404"); }
        catch (ResponseStatusException error) { assertEquals(HttpStatus.NOT_FOUND, error.getStatus()); }
        verifyZeroInteractions(ledger);
        TaskReportWorkerSnapshot orphan = new TaskReportWorkerSnapshot(); orphan.setScopeMissing(true);
        when(scopes.scope(7)).thenReturn(orphan);
        FileReportPage retained = new FileReportPage(); when(ledger.page(7, null, 500)).thenReturn(retained);
        assertSame(retained, controller.files(7, null, 500).getData());
    }

    @Test
    public void invalidLimitAndCursorAreBadRequestsAndValidPageIsBounded() {
        WorkerTaskReportController controller = new WorkerTaskReportController();
        TaskReportWorkerService scopes = mock(TaskReportWorkerService.class);
        FileReportLedgerService ledger = mock(FileReportLedgerService.class);
        ReflectionTestUtils.setField(controller, "reports", scopes);
        ReflectionTestUtils.setField(controller, "ledger", ledger);
        TaskReportWorkerSnapshot found = new TaskReportWorkerSnapshot(); found.setFound(true);
        when(scopes.scope(7)).thenReturn(found);
        for (int limit : new int[]{0, -1, 1001}) {
            try { controller.files(7, null, limit); fail("expected 400"); }
            catch (ResponseStatusException error) { assertEquals(HttpStatus.BAD_REQUEST, error.getStatus()); }
        }
        verifyZeroInteractions(ledger);
        when(ledger.page(7, "invalid", 500)).thenThrow(new IllegalArgumentException("invalid cursor"));
        try { controller.files(7, "invalid", 500); fail("expected 400"); }
        catch (ResponseStatusException error) { assertEquals(HttpStatus.BAD_REQUEST, error.getStatus()); }
        FileReportPage page = new FileReportPage(); when(ledger.page(7, null, 1000)).thenReturn(page);
        assertSame(page, controller.files(7, null, 1000).getData());
    }
}
