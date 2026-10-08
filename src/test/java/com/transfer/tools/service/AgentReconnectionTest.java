package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.transfer.tools.agent.AgentRegistryService;
import com.transfer.tools.agent.AgentSynchronizationService;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.S3ClientConfig;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.HttpMethod;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.AtomicReference;
import java.util.concurrent.locks.ReentrantReadWriteLock;
import java.util.function.BooleanSupplier;

import static org.junit.Assert.*;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.*;

/** Exercises durable enrollment and real Master configuration publication without contacting S3. */
public class AgentReconnectionTest {
    private static final String MASTER = "10.0.0.10", AGENT = "10.0.0.20";
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService db;
    private String dbPath;
    private DistributedConfig config;
    private NodeConfigService nodes;
    private AgentRegistryService registry;
    private AgentSynchronizationService synchronization;
    private FakeNodeRegistry remote;
    private S3ClientManager s3;
    private AgentRegistryService.JoinRequest identity;

    @Before public void setup() throws Exception {
        dbPath = temporary.newFolder("master-db").getAbsolutePath();
        config = new DistributedConfig();
        config.setMasterIp(MASTER); config.setLocalIp(MASTER); config.setPort(1785);
        config.setMasterWebUrl("https://transfer.example.com");
        config.setWorkerIps(Collections.singletonList(MASTER));
        s3 = new S3ClientManager() {
            @Override AmazonS3 createS3Client(S3ClientConfig value) { return mock(AmazonS3.class); }
        };
        remote = new FakeNodeRegistry();
        reopenServices();
        identity = enroll(AGENT);
    }

    @After public void close() {
        if (synchronization != null) synchronization.stop();
        if (remote != null) remote.releaseMutation.countDown();
        if (db != null) db.destroy();
    }

    private void reopenServices() {
        db = new RocksDBService(); ReflectionTestUtils.setField(db, "dbPath", dbPath); db.init();
        nodes = new NodeConfigService();
        ReflectionTestUtils.setField(nodes, "rocksDBService", db);
        ReflectionTestUtils.setField(nodes, "distributedConfig", config); nodes.init();
        ReflectionTestUtils.setField(config, "nodeConfigService", nodes);
        registry = new AgentRegistryService();
        ReflectionTestUtils.setField(registry, "db", db);
        ReflectionTestUtils.setField(registry, "nodes", nodes);
        ReflectionTestUtils.setField(registry, "config", config); registry.init();
        ReflectionTestUtils.setField(s3, "rocksDBService", db);
        ReflectionTestUtils.setField(s3, "agentRegistry", registry);
        ReflectionTestUtils.setField(remote, "config", config);
        ReflectionTestUtils.setField(remote, "nodeConfig", nodes);
        ReflectionTestUtils.setField(remote, "s3Clients", s3);
        synchronization = new AgentSynchronizationService();
        ReflectionTestUtils.setField(synchronization, "registry", registry);
        ReflectionTestUtils.setField(synchronization, "nodes", remote);
    }

    private AgentRegistryService.JoinRequest enroll(String address) {
        AgentRegistryService.InviteRequest invite = new AgentRegistryService.InviteRequest(); invite.setAddress(address);
        String url = (String) registry.invite(invite).get("joinUrl");
        AgentRegistryService.JoinRequest request = new AgentRegistryService.JoinRequest();
        request.setToken(url.substring(url.lastIndexOf('/') + 1)); request.setAddress(address);
        request.setInstanceId(UUID.randomUUID().toString()); request.setCredential(AgentRegistryService.randomSecret());
        assertEquals(true, registry.join(request).get("success"));
        return request;
    }

    private void add(String alias, String endpoint) {
        assertTrue(s3.addClient(alias, endpoint, "test-access", "test-secret", false));
    }

    private void reconnectAndWait() throws Exception {
        synchronization.connected(AGENT);
        eventually(() -> registry.online(AGENT));
        awaitSynchronizationFinished();
    }

    private void awaitSynchronizationFinished() throws Exception {
        eventually(() -> ((Set<?>) ReflectionTestUtils.getField(synchronization, "pending")).isEmpty());
    }

    @SuppressWarnings("unchecked") private AgentRegistryService.AgentRecord member() {
        return ((Map<String, AgentRegistryService.AgentRecord>) ReflectionTestUtils.getField(registry, "agents")).get(AGENT);
    }

    @Test public void offlineMasterAddsUpdatesAndDeletesAreReconciledBeforeAgentIsReady() throws Exception {
        add("archive", "https://old.example.com"); add("retired", "https://retired.example.com");
        reconnectAndWait();
        assertEquals(new HashSet<>(Arrays.asList("archive", "retired")), remote.connections.keySet());
        member().setLastSeen(System.currentTimeMillis() - 70000);
        assertFalse(registry.online(AGENT));

        add("new", "https://new.example.com");
        assertTrue(s3.updateClient("archive", "https://current.example.com", "updated-access", "updated-secret", true, true, false));
        assertTrue(s3.removeClient("retired"));
        assertTrue(registry.needsSynchronization(AGENT));
        assertEquals("https://old.example.com", remote.connections.get("archive").get("endpoint"));

        reconnectAndWait();
        assertEquals(new HashSet<>(Arrays.asList("archive", "new")), remote.connections.keySet());
        Map<String, Object> archive = remote.connections.get("archive");
        assertEquals("https://current.example.com", archive.get("endpoint"));
        assertEquals("updated-access", archive.get("accessKey"));
        assertEquals("updated-secret", archive.get("secretKey"));
        assertEquals(true, archive.get("pathStyleEnable"));
        assertEquals(true, archive.get("useV2SignerType"));
        assertTrue(remote.calls.contains("PUT archive"));
        assertTrue(remote.calls.contains("DELETE retired"));
        assertFalse(registry.needsSynchronization(AGENT));
    }

    @Test public void configurationChangeDuringAcknowledgmentCannotMarkAnOldGenerationReady() throws Exception {
        add("first", "https://first.example.com");
        remote.blockNextMutation = true;
        synchronization.connected(AGENT);
        assertTrue("Agent should receive the first generation", remote.mutationStarted.await(3, TimeUnit.SECONDS));
        try { add("second", "https://second.example.com"); }
        finally { remote.releaseMutation.countDown(); }
        awaitSynchronizationFinished();
        assertFalse("An old acknowledgment cannot make the Agent schedulable", registry.online(AGENT));
        assertTrue(registry.needsSynchronization(AGENT));
        assertFalse(remote.connections.containsKey("second"));

        reconnectAndWait();
        assertEquals(new HashSet<>(Arrays.asList("first", "second")), remote.connections.keySet());
    }

    @Test public void oldSynchronizationCannotMarkAReplacementEnrollmentReady() throws Exception {
        add("archive", "https://archive.example.com");
        MasterLocalTransferService localTransfers = mock(MasterLocalTransferService.class);
        ReflectionTestUtils.setField(synchronization, "localTransfers", localTransfers);
        String originalEnrollment = registry.currentEnrollment(AGENT);
        remote.blockNextMutation = true; synchronization.connected(AGENT);
        assertTrue(remote.mutationStarted.await(3, TimeUnit.SECONDS));
        try {
            nodes.withMembershipWriteLock(() -> {
                registry.revokeMembership(AGENT, Collections.singletonList(MASTER), Collections.emptyMap()); return null;
            });
            identity = enroll(AGENT);
            assertNotEquals(originalEnrollment, registry.currentEnrollment(AGENT));
        } finally { remote.releaseMutation.countDown(); }
        awaitSynchronizationFinished();
        assertFalse("The replacement must acknowledge its own synchronization", registry.online(AGENT));
        assertTrue(registry.needsSynchronization(AGENT));
        verify(localTransfers, never()).retryDeferredParents();
        reconnectAndWait(); assertTrue(registry.online(AGENT));
        verify(localTransfers, times(1)).retryDeferredParents();
    }

    @Test public void failedSynchronizationKeepsAgentUnavailableAndRetriesOnHeartbeat() throws Exception {
        add("archive", "https://archive.example.com");
        remote.failNextRead = true;
        synchronization.connected(AGENT);
        awaitSynchronizationFinished();
        assertFalse(registry.online(AGENT));
        assertTrue(registry.needsSynchronization(AGENT));
        assertTrue(remote.connections.isEmpty());
        reconnectAndWait();
        assertTrue(remote.connections.containsKey("archive"));
    }

    @Test public void masterRestartPreservesIdentityButTrustsNeitherStoredHeartbeatNorReadiness() throws Exception {
        add("archive", "https://archive.example.com"); reconnectAndWait();
        long version = registry.storageVersion();
        assertTrue(member().isSynchronizedStorage());
        synchronization.stop(); db.destroy(); reopenServices();
        assertEquals(version, registry.storageVersion());
        assertEquals(AGENT, registry.authenticatedAddress(identity.getCredential()));
        assertEquals(0, registry.lastSeen(AGENT));
        assertFalse(registry.online(AGENT));
        assertTrue(registry.needsSynchronization(AGENT));
        assertTrue(nodes.getWorkerIps().contains(AGENT));
        reconnectAndWait();
        assertTrue(registry.online(AGENT));
    }

    @Test public void aLongDisconnectedAgentMustReconcileEvenIfMasterConfigurationDidNotChange() throws Exception {
        reconnectAndWait();
        member().setLastSeen(System.currentTimeMillis() - 70000);
        remote.failNextRead = true;
        synchronization.connected(AGENT); awaitSynchronizationFinished();
        assertFalse(registry.online(AGENT));
        assertTrue(registry.needsSynchronization(AGENT));
        reconnectAndWait();
    }

    @Test public void joiningWhileAConcurrentMembershipChangeRevokesAnAgentCannotDeadlock() throws Exception {
        AgentRegistryService.InviteRequest invitation = new AgentRegistryService.InviteRequest(); invitation.setAddress("10.0.0.30");
        String url = (String) registry.invite(invitation).get("joinUrl");
        AgentRegistryService.JoinRequest request = new AgentRegistryService.JoinRequest();
        request.setAddress("10.0.0.30"); request.setToken(url.substring(url.lastIndexOf('/') + 1));
        request.setInstanceId(UUID.randomUUID().toString()); request.setCredential(AgentRegistryService.randomSecret());
        CountDownLatch membershipHeld = new CountDownLatch(1), revoke = new CountDownLatch(1);
        AtomicReference<Throwable> failure = new AtomicReference<>();
        Thread remover = daemon(() -> {
            try { nodes.withMembershipWriteLock(() -> {
                membershipHeld.countDown();
                if (!revoke.await(3, TimeUnit.SECONDS)) throw new IllegalStateException("join did not queue");
                registry.revoke(AGENT); return null;
            }); } catch (Throwable error) { failure.compareAndSet(null, error); }
        });
        Thread joiner = daemon(() -> {
            try { registry.join(request); } catch (Throwable error) { failure.compareAndSet(null, error); }
        });
        remover.start(); assertTrue(membershipHeld.await(3, TimeUnit.SECONDS));
        joiner.start();
        ReentrantReadWriteLock lock = (ReentrantReadWriteLock) ReflectionTestUtils.getField(nodes, "membershipLock");
        try { eventually(() -> lock.hasQueuedThread(joiner)); }
        finally { revoke.countDown(); }
        remover.join(3000); joiner.join(3000);
        assertFalse("Membership removal must not wait on a joining Agent's registry monitor", remover.isAlive());
        assertFalse("The queued enrollment must finish after removal releases membership", joiner.isAlive());
        if (failure.get() != null) throw new AssertionError(failure.get());
        assertNull(registry.authenticatedAddress(identity.getCredential()));
        assertEquals("10.0.0.30", registry.authenticatedAddress(request.getCredential()));
    }

    private static Thread daemon(Runnable work) { Thread thread = new Thread(work); thread.setDaemon(true); return thread; }
    private static void eventually(BooleanSupplier condition) throws Exception {
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(5);
        while (!condition.getAsBoolean() && System.nanoTime() < deadline) Thread.sleep(2);
        assertTrue("Condition did not become true before deadline", condition.getAsBoolean());
    }

    private static class FakeNodeRegistry extends NodeRegistryService {
        final Map<String, Map<String, Object>> connections = new ConcurrentHashMap<>();
        final List<String> calls = new CopyOnWriteArrayList<>();
        final CountDownLatch mutationStarted = new CountDownLatch(1), releaseMutation = new CountDownLatch(1);
        volatile boolean blockNextMutation, failNextRead;
        @SuppressWarnings("unchecked") @Override Map<String, Object> exchange(String address, String path, HttpMethod method,
                                                                                Object body, boolean authenticated) {
            assertEquals(AGENT, address); assertTrue(authenticated);
            if (method == HttpMethod.GET) {
                if (failNextRead) { failNextRead = false; throw new TransferValidationException("TEST_OFFLINE", "Agent unavailable"); }
                Map<String, Object> result = new HashMap<>(); result.put("success", true);
                result.put("configs", new ArrayList<>(connections.values())); return result;
            }
            String alias = method == HttpMethod.POST ? (String) ((Map<?, ?>) body).get("alias") : path.substring(path.lastIndexOf('/') + 1);
            calls.add(method.name() + " " + alias);
            if (method == HttpMethod.DELETE) connections.remove(alias);
            else connections.put(alias, new LinkedHashMap<>((Map<String, Object>) body));
            if (blockNextMutation) {
                blockNextMutation = false; mutationStarted.countDown();
                try { if (!releaseMutation.await(5, TimeUnit.SECONDS)) throw new IllegalStateException("test did not acknowledge mutation"); }
                catch (InterruptedException e) { Thread.currentThread().interrupt(); throw new IllegalStateException(e); }
            }
            return Collections.singletonMap("success", true);
        }
    }
}
