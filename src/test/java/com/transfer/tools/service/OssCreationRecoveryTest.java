package com.transfer.tools.service;

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
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class OssCreationRecoveryTest {
    @Rule public TemporaryFolder folder = new TemporaryFolder();
    private RocksDBService db; private String path; private TaskCreationService creations;
    @Before public void setup() throws Exception { path = folder.newFolder().getAbsolutePath(); open(); }
    private void open() {
        db = new RocksDBService(); ReflectionTestUtils.setField(db,"dbPath",path); db.init();
        creations = new TaskCreationService(); ReflectionTestUtils.setField(creations,"rocksDBService",db);
    }
    @After public void close() { db.destroy(); }
    private BucketPair pair() { BucketPair pair = new BucketPair(); pair.setSourceBucket("source"); pair.setDestBucket("dest"); return pair; }
    @Test public void stopFencesTheOldScannerEvenAfterResumeAndRequestSurvivesRestart() {
        BucketPair request = pair(); String id = creations.create(request); creations.enterScope(id,request);
        creations.recordTaskIds(Arrays.asList(71,72));
        assertEquals(Arrays.asList(71,72),creations.stopByTaskIds(Collections.singletonList(72)));
        List<BucketPair> requests = creations.resumeByTaskIds(Collections.singletonList(71)); assertEquals(1,requests.size());
        try { creations.ensureActive(id); fail("old scanner must remain fenced"); } catch (java.util.concurrent.CancellationException expected) { }
        creations.clearScope(); db.destroy(); open(); creations.recoverInterrupted();
        assertEquals("FAILED",creations.get(id).getStatus()); assertEquals("source",creations.get(id).getRequest().getSourceBucket());
        assertEquals(Integer.valueOf(71),creations.findByTaskId(72).getPublicTaskId());
        assertEquals(Arrays.asList(71,72,99),creations.expandTaskIds(Arrays.asList(72,99,71)));
    }
    @Test public void completedScanResumesOnlyExistingRangesAndIncompleteLegacyIsRejected() {
        String id = creations.create(pair()); creations.beginScope(id); creations.recordTaskIds(Arrays.asList(1,2)); creations.complete(id); creations.clearScope();
        creations.stopByTaskIds(Collections.singletonList(1)); assertTrue(creations.resumeByTaskIds(Collections.singletonList(2)).isEmpty());
        creations.ensureReadyToStart(Collections.singletonList(1));
        String legacy = creations.create(); creations.beginScope(legacy); creations.recordTaskIds(Collections.singletonList(8)); creations.fail(legacy,"interrupted"); creations.clearScope();
        try { creations.resumeByTaskIds(Collections.singletonList(8)); fail("unsafe legacy rescan"); }
        catch (TransferValidationException expected) { assertEquals("OSS_SCAN_CHECKPOINT_MISSING",expected.getCode()); }
    }
    @Test public void repeatedResumeDoesNotReplaceAnActiveOrQueuedScanner() {
        BucketPair request = pair(); String id = creations.create(request); creations.enterScope(id,request);
        creations.recordTaskIds(Collections.singletonList(11)); long initial = creations.get(id).getGeneration();
        assertTrue(creations.resumeByTaskIds(Collections.singletonList(11)).isEmpty());
        assertEquals(initial,creations.get(id).getGeneration()); creations.ensureActive(id);
        creations.stopByTaskIds(Collections.singletonList(11));
        assertEquals(1,creations.resumeByTaskIds(Collections.singletonList(11)).size());
        long queued = creations.get(id).getGeneration();
        assertTrue(creations.resumeByTaskIds(Collections.singletonList(11)).isEmpty());
        assertEquals(queued,creations.get(id).getGeneration());
    }
    @Test public void deletedIncompleteCreationCannotResumeOrDispatchAfterRestart() {
        BucketPair request = pair(); String id = creations.create(request); creations.enterScope(id,request);
        creations.recordTaskIds(Arrays.asList(51,52)); long generation = creations.get(id).getGeneration();
        assertEquals(Arrays.asList(51,52),creations.markDeletedByTaskIds(Collections.singletonList(52)));
        assertTrue(creations.get(id).isDeleted()); assertTrue(creations.get(id).isStopped());
        assertTrue(creations.get(id).getGeneration() > generation);
        try { creations.recordTaskIds(Collections.singletonList(53)); fail("stale callback must not append ranges"); }
        catch (java.util.concurrent.CancellationException expected) { }
        creations.clearScope(); db.destroy(); open(); creations.recoverInterrupted();
        assertTrue(creations.get(id).isDeleted()); assertEquals(Arrays.asList(51,52),creations.get(id).getTaskIds());
        assertFalse(creations.enterScope(id,request));
        try { creations.resumeByTaskIds(Collections.singletonList(51)); fail("deleted scan must not resume"); }
        catch (TransferValidationException expected) { assertEquals("OSS_TASK_DELETED",expected.getCode()); }
        try { creations.ensureReadyToStart(Collections.singletonList(52)); fail("deleted group must not start"); }
        catch (TransferValidationException expected) { assertEquals("OSS_TASK_DELETED",expected.getCode()); }
        try { creations.ensureActive(id); fail("deleted pending dispatch must remain fenced"); }
        catch (java.util.concurrent.CancellationException expected) { }
    }
    @Test public void deletionRejectsWholeResumeSelectionBeforeAnyGroupIsChanged() {
        String first = creations.create(pair()); creations.beginScope(first); creations.recordTaskIds(Collections.singletonList(1)); creations.complete(first); creations.clearScope();
        String second = creations.create(pair()); creations.beginScope(second); creations.recordTaskIds(Collections.singletonList(2)); creations.complete(second); creations.clearScope();
        creations.stopByTaskIds(Collections.singletonList(1)); creations.markDeletedByTaskIds(Collections.singletonList(2));
        try { creations.resumeByTaskIds(Arrays.asList(1,2)); fail("mixed resume must fail atomically"); }
        catch (TransferValidationException expected) { assertEquals("OSS_TASK_DELETED",expected.getCode()); }
        assertTrue(creations.get(first).isStopped()); assertTrue(creations.get(second).isDeleted());
    }
    @Test public void affinityResumeKeepsOriginalSelectedWorkerAfterDistributorRestart() throws Exception {
        BucketPair request=pair();request.setEligibleWorkerNodes(Arrays.asList("worker-a","worker-b"));
        String id=creations.create(request);creations.enterScope(id,request);
        DistributedConfig config=mock(DistributedConfig.class);when(config.getPort()).thenReturn(8081);
        when(config.getWorkerIps()).thenReturn(Arrays.asList("worker-a","worker-b","unselected"));
        SubTaskManagerService manager=mock(SubTaskManagerService.class);when(manager.allocateTaskIdRange(1)).thenReturn(71,72);
        DistributedService first=new DistributedService();
        ReflectionTestUtils.setField(first,"distributedConfig",config);ReflectionTestUtils.setField(first,"subTaskManagerService",manager);
        ReflectionTestUtils.setField(first,"taskCreationService",creations);
        // Select the second eligible worker, then recreate the distributor with its default index of zero.
        ((java.util.concurrent.atomic.AtomicInteger)ReflectionTestUtils.getField(first,"distributedIdx")).set(1);
        MockRestServiceServer server=MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(first,"restTemplate")).build();
        server.expect(requestTo("http://worker-b:8081/api/worker/tasks")).andRespond(withSuccess("{\"success\":true}",MediaType.APPLICATION_JSON));
        ListSubTask initial=new ListSubTask("source","dest",null,null,"","k128",null,null,false);
        first.distributeTasks(Collections.singletonList(initial),"affinity");server.verify();creations.clearScope();
        db.destroy();open();creations.recoverInterrupted();
        BucketPair resumed=creations.resumeByTaskIds(Collections.singletonList(71)).get(0);creations.enterScope(id,resumed);
        DistributedService recovered=new DistributedService();
        ReflectionTestUtils.setField(recovered,"distributedConfig",config);ReflectionTestUtils.setField(recovered,"subTaskManagerService",manager);
        ReflectionTestUtils.setField(recovered,"taskCreationService",creations);
        MockRestServiceServer after=MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(recovered,"restTemplate")).build();
        after.expect(requestTo("http://worker-b:8081/api/worker/tasks")).andRespond(withSuccess("{\"success\":true}",MediaType.APPLICATION_JSON));
        recovered.distributeTasks(Collections.singletonList(new ListSubTask("source","dest",null,null,"k128","k256",null,null,false)),"affinity");
        after.verify();assertEquals(Collections.singletonList("worker-b"),creations.get(id).getParticipatingWorkerNodes());
    }

    @Test public void lostAcknowledgementReplaysSameIdsNodesTokensAndRangesAfterDatabaseReopen() throws Exception {
        BucketPair request = pair(); request.setEligibleWorkerNodes(Collections.singletonList("worker-a"));
        String id = creations.create(request); creations.enterScope(id,request);
        DistributedConfig config = mock(DistributedConfig.class); when(config.getPort()).thenReturn(8081);
        when(config.getWorkerIps()).thenReturn(Arrays.asList("worker-a","unselected-worker"));
        SubTaskManagerService manager = mock(SubTaskManagerService.class); when(manager.allocateTaskIdRange(2)).thenReturn(111);
        DistributedService distributor = new DistributedService();
        ReflectionTestUtils.setField(distributor,"distributedConfig",config); ReflectionTestUtils.setField(distributor,"subTaskManagerService",manager);
        ReflectionTestUtils.setField(distributor,"taskCreationService",creations);
        MockRestServiceServer server = MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(distributor,"restTemplate")).build();
        List<String> bodies = new ArrayList<>();
        server.expect(requestTo("http://worker-a:8081/api/worker/tasks")).andExpect(req -> bodies.add(((MockClientHttpRequest)req).getBodyAsString()))
                .andRespond(withServerError());
        ListSubTask first = new ListSubTask("source","dest",null,null,"","k128",null,null,false); first.setDirection(TransferDirection.OSS_TO_OSS);
        ListSubTask second = new ListSubTask("source","dest",null,null,"k128","k256",null,null,false); second.setDirection(TransferDirection.OSS_TO_OSS);
        try { distributor.distributeTasks(Arrays.asList(first,second),"balanced"); fail("lost acknowledgement"); } catch (RuntimeException expected) { }
        assertEquals(Arrays.asList(111,112),creations.get(id).getTaskIds()); assertEquals("",creations.get(id).getMarker());
        creations.clearScope(); db.destroy(); open(); creations.recoverInterrupted();
        List<BucketPair> resume = creations.resumeByTaskIds(Collections.singletonList(111)); creations.enterScope(id,resume.get(0));
        ReflectionTestUtils.setField(distributor,"taskCreationService",creations); server.reset();
        when(config.getWorkerIps()).thenReturn(Arrays.asList("worker-a","unselected-worker","new-worker"));
        assertEquals(Collections.singletonList("worker-a"), resume.get(0).getEligibleWorkerNodes());
        server.expect(requestTo("http://worker-a:8081/api/worker/tasks")).andExpect(req -> bodies.add(((MockClientHttpRequest)req).getBodyAsString()))
                .andRespond(withSuccess("{\"success\":true}",MediaType.APPLICATION_JSON));
        distributor.replayOssCreation(id); server.verify(); assertEquals(bodies.get(0),bodies.get(1));
        assertEquals("k256",creations.get(id).getMarker()); assertEquals(2,creations.get(id).getNextShardIndex());
        assertTrue(creations.get(id).getPendingDispatches().isEmpty()); verify(manager,times(1)).allocateTaskIdRange(2);
        List<Map> sent = new ObjectMapper().readValue(bodies.get(0),List.class);
        assertEquals(111,sent.get(0).get("taskIndex")); assertNull(sent.get(0).get("parentTaskId"));
        assertTrue(String.valueOf(sent.get(0).get("shardId")).startsWith("oss-"+id));
    }
}
