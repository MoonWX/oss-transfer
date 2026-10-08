package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class TaskReportWorkerServiceTest {
    @Test
    public void returnsOnlyRequestedParentShardsWithoutEmbeddedFileManifest() {
        TaskReportWorkerService service = new TaskReportWorkerService();
        SubTaskManagerService tasks = mock(SubTaskManagerService.class);
        FileReportLedgerService ledger = mock(FileReportLedgerService.class);
        DistributedConfig config = mock(DistributedConfig.class);
        ReflectionTestUtils.setField(service, "tasks", tasks); ReflectionTestUtils.setField(service, "ledger", ledger);
        LocalTaskControlService control = new LocalTaskControlService();
        ReflectionTestUtils.setField(service, "control", control);
        ReflectionTestUtils.setField(service, "database", mock(RocksDBService.class)); ReflectionTestUtils.setField(service, "distributedConfig", config);
        when(tasks.getTaskIdList()).thenReturn(Arrays.asList(-1, -2, 88));
        when(tasks.getDeletedTaskIdList()).thenReturn(Collections.emptyList());
        for (int id : new int[]{-1, -2, 88}) {
            SubTaskStatus task = new SubTaskStatus(); task.setTaskIndex(id);
            task.setParentTaskId(id < 0 ? 7 : null);
            task.setDirection(id < 0 ? TransferDirection.LOCAL_TO_OSS : TransferDirection.OSS_TO_OSS);
            task.setLocalFileEntries(Collections.singletonList(new LocalFileEntry("large-manifest", 1L, 2L)));
            when(tasks.getTaskStatus(id)).thenReturn(task);
        }
        TaskReportWorkerSnapshot snapshot = service.scope(7);
        assertTrue(snapshot.isFound()); assertEquals(2, snapshot.getTasks().size());
        assertTrue(snapshot.getTasks().stream().allMatch(task -> task.getLocalFileEntries() == null));
        assertNotNull(tasks.getTaskStatus(-1).getLocalFileEntries());
        assertEquals(1, service.scope(88).getTasks().size());
        assertFalse(service.scope(999).isFound());
        ReportCoverage orphan = new ReportCoverage(); orphan.setDiscoveredCount(1L);
        when(ledger.coverage(999)).thenReturn(orphan);
        assertTrue(service.scope(999).isScopeMissing());
        assertFalse(service.scope(999).isFound());
        ReportCoverage missingShard = new ReportCoverage(); missingShard.setRegisteredScans(3);
        when(ledger.coverage(7)).thenReturn(missingShard);
        assertTrue(service.scope(7).isScopeMissing());
        missingShard.setRegisteredScans(2);
        assertFalse(service.scope(7).isScopeMissing());
        assertEquals(Boolean.FALSE, service.scope(7).getExecutionActive());
        // Persisted STOPPED does not mean the interrupted operation has exited yet.
        tasks.getTaskStatus(-1).setStatus(SubTaskStatus.TaskStatus.STOPPED);
        try (LocalTaskControlService.Operation ignored = control.begin(-1, 7)) {
            assertEquals(Boolean.TRUE, service.scope(7).getExecutionActive());
            assertEquals(Boolean.FALSE, service.scope(88).getExecutionActive());
        }
        assertEquals(Boolean.FALSE, service.scope(7).getExecutionActive());
        try (LocalTaskControlService.Operation ignored = control.begin(7)) {
            assertEquals(Boolean.TRUE, service.scope(7).getExecutionActive());
        }
    }
}
