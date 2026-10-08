package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.NodeConfig;
import com.transfer.tools.model.S3ClientConfig;
import com.transfer.tools.model.LocalParentTaskRecord;
import com.transfer.tools.model.LocalTransferRequest;
import com.transfer.tools.model.LocalNodeBinding;
import com.transfer.tools.model.TransferEndpoint;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.HttpMethod;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import java.util.concurrent.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class NodeRegistryServiceTest {
    @Rule public TemporaryFolder temp = new TemporaryFolder();
    private RocksDBService db;
    private DistributedConfig config;
    private NodeConfigService nodeConfig;
    private MasterLocalTransferService parents;
    private S3ClientManager s3;
    private LocalParentTaskService parentRecords;
    private FakeRegistry registry;
    private String dbPath;

    @Before public void setup() throws Exception {
        dbPath = temp.newFolder("db").toString();
        db = new RocksDBService(); ReflectionTestUtils.setField(db, "dbPath", dbPath); db.init();
        config = new DistributedConfig();
        config.setMasterIp("10.0.0.1"); config.setLocalIp("10.0.0.1"); config.setPort(1785);
        config.setWorkerIps(new ArrayList<>(Arrays.asList("10.0.0.1", "10.0.0.2")));
        config.setInternalToken("test-cluster-token-never-sent-to-unverified");
        nodeConfig = configService(db); ReflectionTestUtils.setField(config, "nodeConfigService", nodeConfig);
        parents = mock(MasterLocalTransferService.class);
        when(parents.aggregateStatus(null, null, null, null, null)).thenReturn(map("tasks", Collections.emptyList(), "partial", false));
        s3 = mock(S3ClientManager.class); when(s3.getAllConfigs()).thenReturn(Collections.emptyList());
        parentRecords = mock(LocalParentTaskService.class);
        registry = new FakeRegistry();
        ReflectionTestUtils.setField(registry, "config", config); ReflectionTestUtils.setField(registry, "nodeConfig", nodeConfig);
        ReflectionTestUtils.setField(registry, "localTransfers", parents); ReflectionTestUtils.setField(registry, "s3Clients", s3);
        ReflectionTestUtils.setField(registry, "parentRecords", parentRecords);
    }
    private NodeConfigService configService(RocksDBService database) {
        NodeConfigService service = new NodeConfigService();
        ReflectionTestUtils.setField(service, "rocksDBService", database); ReflectionTestUtils.setField(service, "distributedConfig", config);
        service.init(); return service;
    }
    @After public void close() { if (db != null) db.destroy(); }

    @Test public void verifiedAdditionIsDurableAndRootsAreSeparateFromMembership() {
        NodeRegistryService.Change change = change("10.0.0.3", "new"); change.setAdvertisedLocalRoots(Arrays.asList("/mnt/data"));
        registry.create(change);
        assertEquals(Arrays.asList("proof:false", "worker-status:true", "status:true", "deleted-tasks:true", "status:true", "s3-config:true", "node-config:true"), registry.calls);
        assertEquals(Arrays.asList("10.0.0.1", "10.0.0.2", "10.0.0.3"), config.getWorkerIps());
        db.destroy(); db = new RocksDBService(); ReflectionTestUtils.setField(db, "dbPath", dbPath); db.init();
        NodeConfigService reopened = configService(db);
        assertEquals("new", reopened.get().getNodeLabels().get("10.0.0.3"));
        assertTrue(reopened.getWorkerIps().contains("10.0.0.3"));
        reopened.get().getWorkerIps().clear(); assertEquals(3, reopened.getWorkerIps().size());
    }
    @Test public void invalidProofNeverReceivesAnyAuthenticatedRequestOrCredentials() {
        registry.invalidProof = true;
        expectCode("NODE_IDENTITY_MISMATCH", () -> registry.create(change("10.0.0.3", "new")));
        assertEquals(Collections.singletonList("proof:false"), registry.calls);
        assertEquals(2, config.getWorkerIps().size());
        verify(s3, never()).getAllConfigs();
    }
    @Test public void wrongRoleAndWrongMasterCannotJoinEvenWithAValidSignature() {
        for (String field : Arrays.asList("masterIp", "worker", "localIp", "port")) {
            registry.wrongIdentityField = field;
            expectCode("NODE_IDENTITY_MISMATCH", () -> registry.create(change("10.0.0.3", "new")));
        }
        assertTrue(registry.calls.stream().noneMatch(value -> value.endsWith(":true")));
    }
    @Test public void duplicateAndMalformedAddressesDoNotContactAnyServer() {
        expectCode("NODE_EXISTS", () -> registry.create(change("10.0.0.2", "duplicate")));
        for (String address : Arrays.asList("http://10.0.0.3", "10.0.0.3:1785", "10.0.0.300", "010.0.0.3", "host.example", "0.0.0.0"))
            expectCode("INVALID_NODE_ADDRESS", () -> registry.create(change(address, "bad")));
        assertTrue(registry.calls.isEmpty());
    }
    @Test public void pendingParentAndUnreachableOrBusyWorkersBlockRemoval() {
        when(parents.aggregateStatus(null, null, null, null, null)).thenReturn(map("tasks", Arrays.asList(map("status", "PENDING"))));
        expectCode("NODE_TASKS_UNFINISHED", () -> registry.delete("10.0.0.2"));
        when(parents.aggregateStatus(null, null, null, null, null)).thenReturn(map("tasks", Collections.emptyList()));
        registry.unreachable = true; expectCode("NODE_UNAVAILABLE", () -> registry.delete("10.0.0.2"));
        registry.unreachable = false; registry.queued = true; expectCode("NODE_BUSY", () -> registry.delete("10.0.0.2"));
        assertTrue(config.getWorkerIps().contains("10.0.0.2"));
    }
    @Test public void completedHistoryAndRecycleRecordsKeepTheirOwningNodeRegistered() {
        registry.history = true; expectCode("NODE_HISTORY_REMAINS", () -> registry.delete("10.0.0.2"));
        registry.history = false; registry.deleted = true; expectCode("NODE_HISTORY_REMAINS", () -> registry.delete("10.0.0.2"));
        registry.deleted = false; registry.delete("10.0.0.2");
        assertEquals(Collections.singletonList("10.0.0.1"), config.getWorkerIps());
    }
    @Test public void legacyAggregatedParentBindingAlsoProtectsAnEmptyNode() {
        when(parents.aggregateStatus(null, null, null, null, null)).thenReturn(map("tasks", Arrays.asList(
                map("status", "COMPLETED", "nodeBindings", Arrays.asList(map("executionNodeIp", "10.0.0.2"))))));
        expectCode("NODE_HISTORY_REMAINS", () -> registry.delete("10.0.0.2"));
        assertTrue(config.getWorkerIps().contains("10.0.0.2"));
    }

    @Test public void authenticatedRequestRequiresExplicitAcknowledgment() {
        NodeRegistryService actual = new NodeRegistryService();
        ReflectionTestUtils.setField(actual, "config", config);
        org.springframework.test.web.client.MockRestServiceServer server = org.springframework.test.web.client.MockRestServiceServer
                .bindTo((org.springframework.web.client.RestTemplate) ReflectionTestUtils.getField(actual, "http")).build();
        server.expect(org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo("http://10.0.0.2:1785/api/worker/node-config"))
                .andRespond(org.springframework.test.web.client.response.MockRestResponseCreators.withSuccess("{}", org.springframework.http.MediaType.APPLICATION_JSON));
        expectCode("NODE_UNAVAILABLE", () -> actual.exchange("10.0.0.2", "/api/worker/node-config", HttpMethod.PUT, map("advertisedLocalRoots", Collections.emptyList()), true));
        server.verify();
    }

    @Test public void legacyTaskStatusAcceptsOnlyACompleteSnapshotWithoutEnvelope() throws Exception {
        Map<String, Object> good = map("tasks", Collections.emptyList(), "totalTasks", 0, "pendingTasks", 0,
                "runningTasks", 0, "listDoneTasks", 0, "completedTasks", 0, "failedTasks", 0, "partialFailedTasks", 0);
        List<Map<String, Object>> replies = new ArrayList<>();
        replies.add(good);
        replies.add(map("tasks", Collections.emptyList()));
        Map<String, Object> mismatch = new LinkedHashMap<>(good); mismatch.put("totalTasks", 1); replies.add(mismatch);
        Map<String, Object> failed = new LinkedHashMap<>(good); failed.put("success", false); replies.add(failed);
        for (int i = 0; i < replies.size(); i++) {
            NodeRegistryService actual = new NodeRegistryService(); ReflectionTestUtils.setField(actual, "config", config);
            org.springframework.test.web.client.MockRestServiceServer server = org.springframework.test.web.client.MockRestServiceServer
                    .bindTo((org.springframework.web.client.RestTemplate) ReflectionTestUtils.getField(actual, "http")).build();
            server.expect(org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo("http://10.0.0.2:1785/api/worker/status"))
                    .andRespond(org.springframework.test.web.client.response.MockRestResponseCreators.withSuccess(
                            new com.fasterxml.jackson.databind.ObjectMapper().writeValueAsString(replies.get(i)), org.springframework.http.MediaType.APPLICATION_JSON));
            if (i == 0) assertEquals(Collections.emptyList(), actual.exchange("10.0.0.2", "/api/worker/status", HttpMethod.GET, null, true).get("tasks"));
            else expectCode("NODE_UNAVAILABLE", () -> actual.exchange("10.0.0.2", "/api/worker/status", HttpMethod.GET, null, true));
            server.verify();
        }
    }
    @Test public void masterCannotBeRemovedOrMovedButItsLabelAndDisplayRootsCanChange() {
        expectCode("MASTER_NODE_REQUIRED", () -> registry.delete("10.0.0.1"));
        expectCode("MASTER_NODE_REQUIRED", () -> registry.update("10.0.0.1", change("10.0.0.4", "")));
        NodeRegistryService.Change change = change("10.0.0.1", "primary"); change.setAdvertisedLocalRoots(Arrays.asList("/mnt/x/../data"));
        registry.update("10.0.0.1", change);
        assertEquals(Collections.singletonList("/mnt/data"), nodeConfig.getAdvertisedLocalRoots());
        assertEquals("primary", nodeConfig.get().getNodeLabels().get("10.0.0.1"));
    }
    @Test public void oldWorkerConfigCannotBypassRegistryOrMutateIdentity() {
        NodeConfig change = new NodeConfig(); change.setWorkerIps(Collections.singletonList("10.0.0.9"));
        expectCode("NODE_IDENTITY_READ_ONLY", () -> nodeConfig.update(change));
        assertEquals(2, config.getWorkerIps().size());
    }
    @Test public void emptyNodeCannotBeRemovedWhileAnyActiveOrRecycledParentReferencesIt() {
        TransferEndpoint endpoint = new TransferEndpoint(); endpoint.setNodeBindings(Arrays.asList(new LocalNodeBinding("10.0.0.2", "/data")));
        LocalTransferRequest request = new LocalTransferRequest(); request.setSourceEndpoint(endpoint);
        LocalParentTaskRecord parent = new LocalParentTaskRecord(); parent.setRequest(request); parent.setStatus(SubTaskStatus.TaskStatus.COMPLETED);
        when(parentRecords.getAll()).thenReturn(Arrays.asList(parent));
        expectCode("NODE_HISTORY_REMAINS", () -> registry.delete("10.0.0.2"));
        when(parentRecords.getAll()).thenReturn(Collections.emptyList());
        SubTaskStatus deletedParent = new SubTaskStatus(); deletedParent.setNodeBindings(endpoint.getNodeBindings());
        when(parentRecords.getDeleted()).thenReturn(Arrays.asList(deletedParent));
        expectCode("NODE_HISTORY_REMAINS", () -> registry.delete("10.0.0.2"));
    }
    @Test public void unrelatedTerminalFailureDoesNotBlockRemovingAnOtherwiseEmptyNode() {
        when(parents.aggregateStatus(null, null, null, null, null)).thenReturn(map("tasks", Arrays.asList(map("status", "FAILED"), map("status", "PARTIAL_FAILED"))));
        registry.unrelatedFailure = true;
        registry.delete("10.0.0.2");
        assertFalse(config.getWorkerIps().contains("10.0.0.2"));
    }
    @Test public void unrelatedStoppedTaskDoesNotBlockAnEmptyNodeButStoppedParentBindingDoes() {
        when(parents.aggregateStatus(null, null, null, null, null)).thenReturn(map("tasks", Arrays.asList(map("status", "STOPPED"))));
        TransferEndpoint endpoint = new TransferEndpoint(); endpoint.setExecutionNodeIp("10.0.0.2");
        LocalTransferRequest request = new LocalTransferRequest(); request.setSourceEndpoint(endpoint);
        LocalParentTaskRecord parent = new LocalParentTaskRecord(); parent.setRequest(request); parent.setStatus(SubTaskStatus.TaskStatus.STOPPED);
        when(parentRecords.getAll()).thenReturn(Arrays.asList(parent));
        expectCode("NODE_HISTORY_REMAINS", () -> registry.delete("10.0.0.2"));
        when(parentRecords.getAll()).thenReturn(Collections.emptyList());
        registry.unrelatedStopped = true;
        registry.delete("10.0.0.2"); assertFalse(config.getWorkerIps().contains("10.0.0.2"));
    }
    @Test public void invalidMasterWebOriginRejectsAllMutationsBeforeAnySideEffect() {
        config.setMasterWebUrl("https://master.example/path");
        for (Runnable mutation : Arrays.<Runnable>asList(
                () -> registry.create(change("10.0.0.3", "new")),
                () -> registry.update("10.0.0.2", change("10.0.0.2", "changed")),
                () -> registry.delete("10.0.0.2"))) {
            try { mutation.run(); fail(); } catch (IllegalStateException expected) { }
        }
        assertTrue(registry.calls.isEmpty());
        assertEquals(Arrays.asList("10.0.0.1", "10.0.0.2"), config.getWorkerIps());
        assertTrue(nodeConfig.get().getNodeLabels().isEmpty());
    }
    @Test public void candidateWithDiscoveryOrTaskHistoryCannotJoinOrReceiveStorageCredentials() {
        registry.discovery = true;
        expectCode("NODE_HISTORY_REMAINS", () -> registry.create(change("10.0.0.3", "new")));
        registry.discovery = false; registry.history = true;
        expectCode("NODE_HISTORY_REMAINS", () -> registry.create(change("10.0.0.3", "new")));
        assertFalse(config.getWorkerIps().contains("10.0.0.3")); verify(s3, never()).getAllConfigs();
    }
    @Test public void storageConflictDoesNotOverwriteRemoteCredentialsOrRegisterNode() {
        when(s3.getAllConfigs()).thenReturn(Arrays.asList(new S3ClientConfig("a", "http://s3", "ak", "sk", false, true)));
        registry.remoteConfigs = Arrays.asList(map("alias", "a", "endpoint", "http://different"));
        expectCode("NODE_STORAGE_CONFLICT", () -> registry.create(change("10.0.0.3", "new")));
        assertFalse(config.getWorkerIps().contains("10.0.0.3")); assertEquals(0, registry.storagePosts);
        registry.remoteConfigs = Collections.emptyList(); registry.create(change("10.0.0.3", "new"));
        assertEquals(1, registry.storagePosts); assertTrue(config.getWorkerIps().contains("10.0.0.3"));
    }
    @Test public void persistenceFailureDoesNotPublishMembership() {
        RocksDBService failing = mock(RocksDBService.class);
        doThrow(new IllegalStateException("disk full")).when(failing).putSync(anyString(), any());
        ReflectionTestUtils.setField(nodeConfig, "rocksDBService", failing);
        try { registry.create(change("10.0.0.3", "new")); fail(); } catch (IllegalStateException expected) { }
        assertFalse(config.getWorkerIps().contains("10.0.0.3"));
    }
    @Test public void membershipMutationWaitsForInFlightDispatchReadLock() throws Exception {
        CountDownLatch entered = new CountDownLatch(1), release = new CountDownLatch(1);
        ExecutorService executor = Executors.newFixedThreadPool(2);
        try {
            Future<?> dispatch = executor.submit(() -> nodeConfig.withMembershipReadLock(() -> { entered.countDown(); release.await(); return null; }));
            assertTrue(entered.await(2, TimeUnit.SECONDS));
            Future<?> mutation = executor.submit(() -> registry.delete("10.0.0.2"));
            try { mutation.get(100, TimeUnit.MILLISECONDS); fail("must wait"); } catch (TimeoutException expected) { }
            release.countDown(); dispatch.get(2, TimeUnit.SECONDS); mutation.get(2, TimeUnit.SECONDS);
            assertFalse(config.getWorkerIps().contains("10.0.0.2"));
        } finally { release.countDown(); executor.shutdownNow(); }
    }
    @Test public void emptyNodeCannotLeaveLiveOrRecycledTaskBandwidthGroup() {
        MasterTaskBandwidthService bandwidth = mock(MasterTaskBandwidthService.class);
        com.transfer.tools.model.TaskBandwidthPolicy policy = new com.transfer.tools.model.TaskBandwidthPolicy();
        policy.setScopeId("creation:retained"); policy.setMembers(Arrays.asList("10.0.0.1", "10.0.0.2"));
        when(bandwidth.desired(policy.getScopeId())).thenReturn(policy);
        ReflectionTestUtils.setField(registry, "taskBandwidth", bandwidth);
        registry.scopedTask = map("taskIndex", 71, "status", "COMPLETED", "taskBandwidthScopeId", policy.getScopeId());
        expectCode("NODE_HISTORY_REMAINS", () -> registry.delete("10.0.0.2"));
        registry.scopedTaskDeleted = true;
        expectCode("NODE_HISTORY_REMAINS", () -> registry.delete("10.0.0.2"));
        // Retained policies alone must not keep a node forever after every task is purged.
        registry.scopedTask = null;
        registry.delete("10.0.0.2");
        assertEquals(Collections.singletonList("10.0.0.1"), config.getWorkerIps());
    }
    private NodeRegistryService.Change change(String address, String label) {
        NodeRegistryService.Change value = new NodeRegistryService.Change(); value.setAddress(address); value.setLabel(label); return value;
    }
    private void expectCode(String code, Runnable operation) {
        try { operation.run(); fail("expected " + code); } catch (TransferValidationException e) { assertEquals(code, e.getCode()); }
    }
    private static Map<String, Object> map(Object... values) {
        Map<String, Object> result = new LinkedHashMap<>();
        for (int i = 0; i < values.length; i += 2) result.put((String) values[i], values[i+1]);
        return result;
    }
    private class FakeRegistry extends NodeRegistryService {
        List<String> calls = new ArrayList<>();
        boolean invalidProof, unreachable, queued, history, deleted, discovery, unrelatedFailure, unrelatedStopped;
        String wrongIdentityField;
        int storagePosts;
        Map<String, Object> scopedTask;
        boolean scopedTaskDeleted;
        List<Map<String, Object>> remoteConfigs = Collections.emptyList();
        @Override Map<String, Object> exchange(String address, String path, HttpMethod method, Object body, boolean auth) {
            String name = path.substring(path.lastIndexOf('/') + 1).split("\\?")[0]; calls.add(name + ":" + auth);
            if (path.contains("/proof?")) {
                assertFalse(auth);
                String nonce = path.substring(path.indexOf('=') + 1);
                Map<String, Object> proof = map("nonce", nonce, "localIp", address, "masterIp", "10.0.0.1", "port", 1785, "worker", true, "master", false);
                if (wrongIdentityField != null) proof.put(wrongIdentityField, "worker".equals(wrongIdentityField) ? false : "wrong");
                proof.put("signature", invalidProof ? "bad" : NodeIdentityService.sign(proof, config.getInternalToken())); return proof;
            }
            assertTrue(auth);
            if (unreachable) throw new TransferValidationException("NODE_UNAVAILABLE", "unreachable");
            if (path.equals("/api/worker/status") && scopedTask != null && !scopedTaskDeleted && address.equals("10.0.0.1")) return map("tasks", Collections.singletonList(scopedTask));
            if (path.equals("/api/worker/deleted-tasks") && scopedTask != null && scopedTaskDeleted && address.equals("10.0.0.1")) return map("success", true, "tasks", Collections.singletonList(scopedTask));
            if (path.equals("/api/worker/status")) return map("tasks", history && !address.equals("10.0.0.1") ? Arrays.asList(map("status", "COMPLETED")) : unrelatedFailure && address.equals("10.0.0.1") ? Arrays.asList(map("status", "FAILED")) : unrelatedStopped && address.equals("10.0.0.1") ? Arrays.asList(map("status", "STOPPED")) : Collections.emptyList());
            if (path.equals("/api/worker/local-transfers/status")) return map("success", true, "data", map("tasks", discovery ? Arrays.asList(map("shardId", "discovery", "status", "COMPLETED")) : Collections.emptyList()));
            if (path.equals("/api/worker/deleted-tasks")) return map("success", true, "tasks", deleted ? Arrays.asList(map("status", "COMPLETED")) : Collections.emptyList());
            if (path.equals("/api/worker/worker-status")) return map("success", true, "queueStatus", map("listQueueSize", queued ? 1 : 0, "putQueueSize", 0, "progressQueueSize", 0, "failedKeyQueueSize", 0, "fallbackQueueSize", 0));
            if (path.equals("/api/worker/s3-config")) {
                if (method == HttpMethod.POST) storagePosts++;
                return map("success", true, "configs", remoteConfigs);
            }
            return map("success", true);
        }
    }
}
