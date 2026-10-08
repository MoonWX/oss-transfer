package com.transfer.tools.controller;

import com.transfer.tools.model.FailedKey;
import com.transfer.tools.model.FallbackObject;
import com.transfer.tools.service.SubTaskManagerService;
import com.transfer.tools.storage.RocksDBService;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.*;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class WorkerExceptionQueryTest {
    @Test
    public void groupQueryAppliesOneLimitAndCountsAllMatchesInOneStoragePass() {
        for (boolean fallback : Arrays.asList(false, true)) {
            Fixture fixture = new Fixture();
            fixture.seed(1, 11, "source", "target");
            fixture.seed(2, 12, "source", "target");
            fixture.seed(3, 99, "source", "target");
            fixture.seed(4, 11, "other-source", "target");
            fixture.seed(5, 12, "source", "other-target");
            fixture.seed(6, 11, "source", "target");
            fixture.data.put(fallback ? "fallback:count" : "failed:count", 7L); // Deleted record gap.
            Map<String, Object> params = params("taskIds", Arrays.asList(11, 12, 11), "limit", 2,
                    "sourceBucket", "source", "destBucket", "target");
            Map<String, Object> response = fixture.query(fallback, params);
            assertEquals(Boolean.TRUE, response.get("success"));
            assertEquals(3L, response.get("count"));
            assertEquals(Arrays.asList("key-1", "key-2"), fixture.keys(fallback, response));
            String prefix = fallback ? "fallback:" : "failed:";
            Class<?> type = fallback ? FallbackObject.class : FailedKey.class;
            verify(fixture.db, times(1)).get(prefix + "count", Long.class);
            for (int i = 1; i <= 7; i++) verify(fixture.db, times(1)).get(prefix + i, type);
        }
    }

    @Test
    public void defaultLimitIsGlobalAcross128ShardsAndCountRemainsComplete() {
        for (boolean fallback : Arrays.asList(false, true)) {
            Fixture fixture = new Fixture();
            List<Integer> ids = new ArrayList<>();
            for (int i = 1; i <= 128; i++) {
                fixture.seed(i, i, "source", "target"); ids.add(i);
            }
            Map<String, Object> response = fixture.query(fallback, params("taskIds", ids));
            assertEquals(128L, response.get("count"));
            List<String> keys = fixture.keys(fallback, response);
            assertEquals(100, keys.size());
            assertEquals("key-1", keys.get(0));
            assertEquals("key-100", keys.get(99));
        }
    }

    @Test
    public void singleTaskAndUnfilteredLegacyRequestsKeepTheirSemantics() {
        for (boolean fallback : Arrays.asList(false, true)) {
            Fixture fixture = new Fixture();
            fixture.seed(1, 11, "source", "target");
            fixture.seed(2, 12, "source", "target");
            fixture.seed(3, 11, "source", "target");
            Map<String, Object> single = fixture.query(fallback, params("taskIndex", 11, "limit", 1));
            assertEquals(2L, single.get("count"));
            assertEquals(Collections.singletonList("key-1"), fixture.keys(fallback, single));
            Map<String, Object> all = fixture.query(fallback, null);
            assertEquals(3L, all.get("count"));
            assertEquals(3, fixture.keys(fallback, all).size());
            for (int limit : Arrays.asList(0, -1)) {
                Map<String, Object> unlimited = fixture.query(fallback, params("taskIds", Arrays.asList(11, 12), "limit", limit));
                assertEquals(3L, unlimited.get("count"));
                assertEquals(3, fixture.keys(fallback, unlimited).size());
            }
        }
    }

    @Test
    public void emptyAndUnknownGroupFiltersNeverBroadenToEveryTask() {
        for (boolean fallback : Arrays.asList(false, true)) {
            Fixture fixture = new Fixture(); fixture.seed(1, 11, "source", "target");
            for (List<Integer> ids : Arrays.asList(Collections.<Integer>emptyList(), Collections.singletonList(999))) {
                Map<String, Object> response = fixture.query(fallback, params("taskIds", ids));
                assertEquals(Boolean.TRUE, response.get("success"));
                assertEquals(0L, response.get("count"));
                assertTrue(fixture.keys(fallback, response).isEmpty());
            }
        }
    }

    @Test
    public void taskIndexAndTaskIdsAreCombinedWhenBothAreProvided() {
        for (boolean fallback : Arrays.asList(false, true)) {
            Fixture fixture = new Fixture(); fixture.seed(1, 11, "source", "target"); fixture.seed(2, 12, "source", "target");
            Map<String, Object> response = fixture.query(fallback, params("taskIndex", 11, "taskIds", Arrays.asList(11, 12)));
            assertEquals(1L, response.get("count"));
            assertEquals(Collections.singletonList("key-1"), fixture.keys(fallback, response));
            assertEquals(0L, fixture.query(fallback, params("taskIndex", 11, "taskIds", Collections.singletonList(12))).get("count"));
        }
    }

    @Test
    public void invalidGroupFiltersFailClosed() {
        for (boolean fallback : Arrays.asList(false, true)) {
            Fixture fixture = new Fixture(); fixture.seed(1, 11, "source", "target");
            for (Object ids : Arrays.asList("11,12", 11, Arrays.asList("11"), Arrays.asList(11.5),
                    Arrays.asList(2147483648L), Collections.singletonList(null))) {
                Map<String, Object> response = fixture.query(fallback, params("taskIds", ids));
                assertEquals(Boolean.FALSE, response.get("success"));
                assertNull(response.get(fallback ? "fallbackObjects" : "failedKeys"));
            }
        }
    }

    @Test
    public void missingCountersReturnEmptyResults() {
        Fixture fixture = new Fixture();
        for (boolean fallback : Arrays.asList(false, true)) {
            Map<String, Object> response = fixture.query(fallback, params("taskIds", Arrays.asList(11, 12)));
            assertEquals(0L, response.get("count"));
            assertTrue(fixture.keys(fallback, response).isEmpty());
        }
    }

    private static Map<String, Object> params(Object... entries) {
        Map<String, Object> params = new HashMap<>();
        for (int i = 0; i < entries.length; i += 2) params.put((String) entries[i], entries[i + 1]);
        return params;
    }

    private static class Fixture {
        final Map<String, Object> data = new HashMap<>();
        final RocksDBService db = mock(RocksDBService.class);
        final SubTaskManagerService tasks = new SubTaskManagerService();
        final WorkerController controller = new WorkerController();
        Fixture() {
            when(db.get(anyString(), any())).thenAnswer(call -> data.get(call.getArgument(0)));
            ReflectionTestUtils.setField(tasks, "rocksDBService", db);
            ReflectionTestUtils.setField(controller, "subTaskManagerService", tasks);
        }
        void seed(int record, int taskId, String source, String target) {
            FailedKey failed = new FailedKey(); failed.setTaskIndex(taskId); failed.setSourceBucket(source);
            failed.setDestBucket(target); failed.setKey("key-" + record);
            FallbackObject fallback = new FallbackObject(); fallback.setTaskIndex(taskId); fallback.setSourceBucket(source);
            fallback.setDestBucket(target); fallback.setKey("key-" + record);
            data.put("failed:" + record, failed); data.put("fallback:" + record, fallback);
            data.put("failed:count", (long) record); data.put("fallback:count", (long) record);
        }
        Map<String, Object> query(boolean fallback, Map<String, Object> params) {
            return fallback ? controller.getFallbackObjects(params) : controller.getFailedKeys(params);
        }
        List<String> keys(boolean fallback, Map<String, Object> response) {
            List<?> items = (List<?>) response.get(fallback ? "fallbackObjects" : "failedKeys");
            List<String> keys = new ArrayList<>();
            for (Object item : items) keys.add(fallback ? ((FallbackObject) item).getKey() : ((FailedKey) item).getKey());
            return keys;
        }
    }
}
