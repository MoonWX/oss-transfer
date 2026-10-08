package com.transfer.tools.service;

import com.transfer.tools.model.BucketPair;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;

public class OssTaskGroupServiceTest {
    @Rule public TemporaryFolder folder = new TemporaryFolder();
    private RocksDBService db; private TaskCreationService creations; private OssTaskGroupService groups;
    @Before public void setup() throws Exception {
        db = new RocksDBService(); ReflectionTestUtils.setField(db,"dbPath",folder.newFolder().getAbsolutePath()); db.init();
        creations = new TaskCreationService(); ReflectionTestUtils.setField(creations,"rocksDBService",db);
        groups = new OssTaskGroupService(); ReflectionTestUtils.setField(groups,"taskCreationService",creations);
    }
    @After public void close() { db.destroy(); }
    private String create(List<Integer> ids, boolean complete) {
        BucketPair pair = new BucketPair(); pair.setSourceBucket("source"); pair.setDestBucket("destination");
        String id = creations.create(pair); creations.beginScope(id); creations.recordTaskIds(ids);
        if (complete) creations.complete(id); creations.clearScope(); return id;
    }
    private Map<String,Object> row(int id, String state) {
        Map<String,Object> row = new LinkedHashMap<>(); row.put("taskIndex",id); row.put("status",state);
        row.put("listObjectCount",128); row.put("putObjectCount",128); row.put("putObjectSize",1024L);
        row.put("scanComplete",true); row.put("workerNode",id%2 == 0 ? "worker-a" : "worker-b");
        row.put("startTime","2026-09-23 10:00:00"); row.put("endTime","2026-09-23 10:00:10"); return row;
    }
    @Test public void hundredsOfRangesBecomeOneCreationWithExactTotals() {
        List<Integer> ids = new ArrayList<>(); List<Object> rows = new ArrayList<>();
        for (int i=111;i<231;i++) { ids.add(i); rows.add(row(i,"COMPLETED")); }
        String id = create(ids,true);
        List<Object> values = groups.group(rows,false,false); assertEquals(1,values.size());
        Map result = (Map)values.get(0); assertEquals(111,result.get("taskIndex")); assertEquals(id,result.get("creationId"));
        assertEquals(120,result.get("shardCount")); assertEquals(15360L,result.get("putObjectCount"));
        assertEquals(12288L,result.get("averageSpeedBytesPerSecond")); assertEquals("COMPLETED",result.get("status"));
        assertEquals(2,((List)result.get("participatingWorkerNodes")).size());
    }
    @Test public void currentSpeedIncludesCompletedChildTailUntilLogicalTaskEnds() {
        create(Arrays.asList(1,2),true);
        Map<String,Object> finished=row(1,"COMPLETED");finished.put("currentSpeedBytesPerSecond",5_000_000L);
        Map<String,Object> active=row(2,"RUNNING");active.put("currentSpeedBytesPerSecond",7_000_000L);
        Map running=(Map)groups.group(Arrays.asList(finished,active),false,false).get(0);
        assertEquals(12_000_000L,running.get("currentSpeedBytesPerSecond"));
        for(String state:Arrays.asList("COMPLETED","STOPPED","FAILED")) {
            active.put("status",state);
            Map ended=(Map)groups.group(Arrays.asList(finished,active),false,false).get(0);
            assertEquals(0L,ended.get("currentSpeedBytesPerSecond"));
        }
    }

    @Test public void sameBucketsDoNotMergeIndependentCreationsOrImportedTasks() {
        create(Arrays.asList(1,2),true); create(Arrays.asList(3,4),true);
        List<Object> result = groups.group(Arrays.asList(row(1,"COMPLETED"),row(2,"COMPLETED"),row(3,"COMPLETED"),row(4,"COMPLETED"),row(99,"COMPLETED")),false,false);
        assertEquals(3,result.size());
    }
    @Test public void missingNodeOrShardNeverBecomesSuccessfulAndDeletedGroupsDoNotReappear() {
        create(Arrays.asList(1,2),true);
        Map partial = (Map)groups.group(Collections.singletonList(row(1,"COMPLETED")),false,false).get(0);
        assertEquals("FAILED",partial.get("status")); assertEquals(false,partial.get("scanComplete"));
        assertEquals(true,partial.get("partial")); assertEquals(Collections.singletonList(2),partial.get("missingTaskIds"));
        Map unreachable = (Map)groups.group(Arrays.asList(row(1,"COMPLETED"),row(2,"COMPLETED")),false,true).get(0);
        assertEquals("FAILED",unreachable.get("status"));
        assertTrue(groups.group(Collections.emptyList(),false,false).isEmpty());
        assertTrue(groups.group(Collections.emptyList(),true,false).isEmpty());
    }
    @Test public void runningShardTakesPriorityOverStoppedFenceAndFinishedRangesWaitForScan() {
        String id = create(Arrays.asList(1,2),false);
        Map scanning = (Map)groups.group(Arrays.asList(row(1,"COMPLETED"),row(2,"COMPLETED")),false,false).get(0);
        assertEquals("PENDING",scanning.get("status")); assertEquals(false,scanning.get("scanComplete"));
        creations.stopByTaskIds(Collections.singletonList(1));
        Map stopping = (Map)groups.group(Arrays.asList(row(1,"RUNNING"),row(2,"STOPPED")),false,false).get(0);
        assertEquals("RUNNING",stopping.get("status"));
    }
}
