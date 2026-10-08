package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.*;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.MediaType;
import org.springframework.mock.http.client.MockClientHttpRequest;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.util.*;
import java.util.concurrent.atomic.AtomicInteger;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.anything;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

/** A 101-range scan loses one batch acknowledgment, then resumes without duplicate ranges. */
public class OssExploreGroupIntegrationTest {
    @Rule public TemporaryFolder folder = new TemporaryFolder();
    private RocksDBService db;
    @After public void close() { if (db != null) db.destroy(); }
    @Test public void resumedHundredRangeScanKeepsOriginalIdsAndOneLogicalTask() throws Exception {
        scanAfterLostAcknowledgement(false);
    }
    @Test public void automaticScanResumesAndEveryLaterBatchRetainsExecutionIntent() throws Exception {
        scanAfterLostAcknowledgement(true);
    }
    private void scanAfterLostAcknowledgement(boolean automatic) throws Exception {
        db = new RocksDBService(); ReflectionTestUtils.setField(db,"dbPath",folder.newFolder().getAbsolutePath()); db.init();
        TaskCreationService creations = new TaskCreationService(); ReflectionTestUtils.setField(creations,"rocksDBService",db);
        DistributedConfig config = mock(DistributedConfig.class); when(config.isMaster()).thenReturn(true); when(config.getPort()).thenReturn(8081);
        when(config.getWorkerIps()).thenReturn(Arrays.asList("a","b"));
        SubTaskManagerService manager = mock(SubTaskManagerService.class); AtomicInteger sequence = new AtomicInteger(111);
        when(manager.allocateTaskIdRange(anyInt())).thenAnswer(call -> sequence.getAndAdd(call.getArgument(0)));
        DistributedService distributor = new DistributedService();
        ReflectionTestUtils.setField(distributor,"distributedConfig",config); ReflectionTestUtils.setField(distributor,"subTaskManagerService",manager);
        ReflectionTestUtils.setField(distributor,"taskCreationService",creations);
        MockRestServiceServer server = MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(distributor,"restTemplate")).build();
        BucketExploreService explorer = new BucketExploreService();
        ReflectionTestUtils.setField(explorer,"distributedConfig",config); ReflectionTestUtils.setField(explorer,"distributedService",distributor);
        ReflectionTestUtils.setField(explorer,"taskCreationService",creations); ReflectionTestUtils.setField(explorer,"rateLimiterManager",mock(RateLimiterManager.class));
        AmazonS3 s3 = mock(AmazonS3.class); S3ClientManager clients = mock(S3ClientManager.class);
        when(clients.getListClient("source")).thenReturn(s3); ReflectionTestUtils.setField(explorer,"s3ClientManager",clients);
        List<String> markers = new ArrayList<>();
        when(s3.listObjects(any(ListObjectsRequest.class))).thenAnswer(call -> {
            ListObjectsRequest request = call.getArgument(0); markers.add(request.getMarker());
            ObjectListing result = new ObjectListing(); int added=0, last=0;
            for (int i=1;i<=101;i++) {
                String key = String.format("file-%03d",i);
                if (request.getMarker()!=null && key.compareTo(request.getMarker())<=0) continue;
                S3ObjectSummary object = new S3ObjectSummary(); object.setKey(key); object.setSize(BalancedWorkerScheduler.MAX_DATA_BYTES);
                result.getObjectSummaries().add(object); added++; last=i; if (added==50) break;
            }
            result.setTruncated(last>0 && last<101); return result;
        });
        Map<Integer,Map> workerRecords = new TreeMap<>();
        org.springframework.test.web.client.RequestMatcher capture = req -> {
            assertTrue(Arrays.asList("a","b").contains(req.getURI().getHost()));
            List<Map> payload = new ObjectMapper().readValue(((MockClientHttpRequest)req).getBodyAsString(),List.class);
            for (Map task : payload) { int id = ((Number)task.get("taskIndex")).intValue();
                if (workerRecords.containsKey(id)) assertEquals(workerRecords.get(id),task); else workerRecords.put(id,task); }
        };
        server.expect(anything()).andExpect(capture).andRespond(withSuccess("{\"success\":true}",MediaType.APPLICATION_JSON));
        server.expect(anything()).andExpect(capture).andRespond(withServerError());
        BucketPair pair = new BucketPair(); pair.setSourceBucket("source-bucket"); pair.setDestBucket("target-bucket");
        pair.setEligibleWorkerNodes(Arrays.asList("a","b"));
        pair.setSourceS3Alias("source"); pair.setDestS3Alias("destination"); pair.setDistributionMode("balanced");
        String creation = creations.create(pair, automatic); creations.enterScope(creation,pair);
        try { explorer.exploreSingleBucketAndCreateSubTasks(pair); fail("lost ACK"); } catch (RuntimeException expected) { }
        assertEquals(40,workerRecords.size()); assertEquals(40,creations.get(creation).getTaskIds().size());
        creations.clearScope(); creations.recoverInterrupted(); server.reset(); markers.clear();
        when(config.getWorkerIps()).thenReturn(Arrays.asList("a","b","new-node"));
        for (int i=0;i<6;i++) server.expect(anything()).andExpect(capture).andRespond(withSuccess("{\"success\":true}",MediaType.APPLICATION_JSON));
        BucketPair resumed = (automatic ? creations.recoverableRequests() : creations.resumeByTaskIds(Collections.singletonList(111))).get(0); creations.enterScope(creation,resumed);
        explorer.exploreSingleBucketAndCreateSubTasks(resumed); creations.complete(creation); creations.clearScope(); server.verify();
        assertEquals("file-040",markers.get(0)); assertEquals(101,workerRecords.size()); assertEquals(212,sequence.get());
        Set<String> endKeys = new HashSet<>(); for (Map task : workerRecords.values()) { assertEquals(automatic, task.get("autoStart")); assertNull(task.get("parentTaskId")); endKeys.add(String.valueOf(task.get("endKey"))); }
        assertEquals(101,endKeys.size()); assertEquals(101,creations.get(creation).getNextShardIndex());
        assertEquals(new HashSet<>(Arrays.asList("a","b")),new HashSet<>(creations.get(creation).getParticipatingWorkerNodes()));
        OssTaskGroupService groups = new OssTaskGroupService(); ReflectionTestUtils.setField(groups,"taskCreationService",creations);
        List<Object> rows = new ArrayList<>(); for (Map task : workerRecords.values()) { Map<String,Object> row = new LinkedHashMap<>(task); row.put("status","PENDING"); rows.add(row); }
        List<Object> grouped = groups.group(rows,false,false); assertEquals(1,grouped.size());
        assertEquals(automatic, ((Map) grouped.get(0)).get("autoStart"));
        assertEquals(101,((Map)grouped.get(0)).get("shardCount")); assertEquals("PENDING",((Map)grouped.get(0)).get("status"));
    }
}
