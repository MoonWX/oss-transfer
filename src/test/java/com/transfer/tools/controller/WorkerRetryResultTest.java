package com.transfer.tools.controller;

import com.transfer.tools.service.SubTaskManagerService;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.Arrays;
import java.util.Collections;
import java.util.Map;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class WorkerRetryResultTest {
    @Test public void onlyActualAcceptedTasksAreCountedAndDuplicateIdsAreNotRetriedTwice() throws Exception {
        SubTaskManagerService tasks = mock(SubTaskManagerService.class);
        doThrow(new IllegalStateException("task still running")).when(tasks).retryTask(2);
        WorkerController controller = new WorkerController();
        ReflectionTestUtils.setField(controller, "subTaskManagerService", tasks);
        Map<String, Object> result = controller.retryTask(Collections.singletonMap("taskIds", Arrays.asList(1, 1, 2)));
        assertEquals(false, result.get("success")); assertEquals(true, result.get("partial"));
        assertEquals(1, result.get("accepted")); assertEquals(1, result.get("failed"));
        assertTrue(result.get("message").toString().contains("task still running"));
        verify(tasks, times(1)).retryTask(1);
        verify(tasks, times(1)).retryTask(2);
    }

    @Test public void emptyRequestAndFailedDurableAcceptanceCannotClaimSuccess() throws Exception {
        SubTaskManagerService tasks = mock(SubTaskManagerService.class);
        doThrow(new IllegalStateException("queue is full")).when(tasks).retryTask(1);
        WorkerController controller = new WorkerController();
        ReflectionTestUtils.setField(controller, "subTaskManagerService", tasks);
        Map<String, Object> empty = controller.retryTask(Collections.emptyMap());
        assertEquals(false, empty.get("success")); assertEquals(0, empty.get("accepted"));
        Map<String, Object> rejected = controller.retryTask(Collections.singletonMap("taskIds", Collections.singletonList(1)));
        assertEquals(false, rejected.get("success")); assertEquals(0, rejected.get("accepted"));
        assertEquals(1, rejected.get("failed"));
    }
}
