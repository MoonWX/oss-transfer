package com.transfer.tools.agent;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.service.NodeConfigService;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.springframework.test.util.ReflectionTestUtils;
import java.io.IOException;
import java.util.*;
import java.util.concurrent.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

/** Revocation must isolate requests that were authenticated before an address was reused. */
public class AgentEnrollmentSessionTest {
    private static final String ADDRESS = "10.0.0.2";
    private final Map<String, Object> durable = new ConcurrentHashMap<>();
    private AgentRegistryService registry;
    private NodeConfigService nodes;
    private RocksDBService db;
    private AgentBroker broker;
    private DistributedConfig config;
    private ExecutorService executor;

    @Before public void setup() {
        registry = new AgentRegistryService(); nodes = mock(NodeConfigService.class);
        db = mock(RocksDBService.class); config = mock(DistributedConfig.class);
        when(config.isMaster()).thenReturn(true); when(config.getMasterWebUrl()).thenReturn("https://transfer.example.com");
        when(nodes.getWorkerIps()).thenReturn(Collections.singletonList("10.0.0.1"));
        when(nodes.withMembershipWriteLock(any())).thenAnswer(call -> ((Callable<?>)call.getArgument(0)).call());
        when(db.get(anyString(), any())).thenAnswer(call -> durable.get(call.getArgument(0)));
        doAnswer(call -> { durable.put(call.getArgument(0), call.getArgument(1)); return null; }).when(db).putSync(anyString(), any());
        doAnswer(call -> { durable.putAll(call.getArgument(2)); return null; }).when(nodes).registerAgent(anyString(), anyString(), anyMap());
        doAnswer(call -> { durable.putAll(call.getArgument(2)); return null; }).when(nodes).replaceMembershipWithChanges(anyList(), anyMap(), anyMap());
        ReflectionTestUtils.setField(registry, "db", db); ReflectionTestUtils.setField(registry, "config", config);
        ReflectionTestUtils.setField(registry, "nodes", nodes);
        executor = Executors.newCachedThreadPool();
    }
    @After public void cleanup() { executor.shutdownNow(); if (broker != null) broker.close(); }

    private AgentRegistryService.JoinRequest join(AgentRegistryService.JoinRequest previous) {
        AgentRegistryService.InviteRequest invitation = new AgentRegistryService.InviteRequest(); invitation.setAddress(ADDRESS);
        String link = (String) registry.invite(invitation).get("joinUrl");
        AgentRegistryService.JoinRequest request = new AgentRegistryService.JoinRequest();
        request.setAddress(ADDRESS); request.setToken(link.substring(link.lastIndexOf('/') + 1));
        request.setInstanceId(previous == null ? UUID.randomUUID().toString() : previous.getInstanceId());
        request.setCredential(previous == null ? AgentRegistryService.randomSecret() : previous.getCredential());
        registry.join(request); return request;
    }
    private void bindBroker(AgentBroker value) {
        broker = value; ReflectionTestUtils.setField(broker, "registry", registry);
        ReflectionTestUtils.setField(registry, "broker", broker);
    }
    private static RpcResult result(RpcCommand command) {
        RpcResult result = new RpcResult(); result.setId(command.getId()); result.setStatus(200);
        result.setBody(Base64.getEncoder().encodeToString("{}".getBytes())); return result;
    }
    private static boolean causedBy(Throwable value, Class<?> type) {
        while (value != null) { if (type.isInstance(value)) return true; value = value.getCause(); }
        return false;
    }
    private static void waitPending(AgentBroker broker) throws Exception {
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(2);
        while (broker.pendingCount() == 0 && System.nanoTime() < deadline) Thread.yield();
        assertEquals(1, broker.pendingCount());
    }

    @Test public void oldLongPollCannotConsumeReplacementCommandsEvenWhenInstanceAndCredentialAreReused() throws Exception {
        CountDownLatch oldPollEntered = new CountDownLatch(1);
        bindBroker(new AgentBroker() {
            @Override public RpcCommand poll(String address, String enrollment, long wait) throws InterruptedException {
                oldPollEntered.countDown(); return super.poll(address, enrollment, wait);
            }
        });
        AgentRegistryService.JoinRequest old = join(null);
        AgentRegistryService.Session oldSession = registry.authenticatedSession(old.getCredential());
        AgentController controller = new AgentController();
        ReflectionTestUtils.setField(controller, "registry", registry); ReflectionTestUtils.setField(controller, "broker", broker);
        ReflectionTestUtils.setField(controller, "config", config);
        ReflectionTestUtils.setField(controller, "synchronization", mock(AgentSynchronizationService.class));
        Future<?> oldPoll = executor.submit(() -> controller.poll(old.getCredential()));
        assertTrue(oldPollEntered.await(2, TimeUnit.SECONDS));
        registry.revokeMembership(ADDRESS, Collections.singletonList("10.0.0.1"), Collections.emptyMap());
        AgentRegistryService.JoinRequest replacement = join(old);
        AgentRegistryService.Session newSession = registry.authenticatedSession(replacement.getCredential());
        assertNotEquals(oldSession.getEnrollmentId(), newSession.getEnrollmentId());
        assertFalse(registry.isSessionValid(old.getCredential(), oldSession));

        Future<RpcResponse> call = executor.submit(() -> broker.call(ADDRESS, "POST", "/api/worker/start", "{}".getBytes(), null, 2000));
        RpcCommand command = broker.poll(ADDRESS, newSession.getEnrollmentId(), 1000); assertNotNull(command);
        try { oldPoll.get(2, TimeUnit.SECONDS); fail("A revoked long poll must not return 200 or 204"); }
        catch (ExecutionException expected) { assertTrue(causedBy(expected, AgentController.AgentUnauthorizedException.class)); }
        assertNull(broker.poll(ADDRESS, oldSession.getEnrollmentId(), 0));
        try { broker.complete(ADDRESS, oldSession.getEnrollmentId(), result(command)); fail("Old enrollment cannot acknowledge a new command"); }
        catch (SecurityException expected) { }
        assertFalse(call.isDone());
        assertEquals(204, controller.result(replacement.getCredential(), result(command)).getStatusCodeValue());
        assertEquals(200, call.get(2, TimeUnit.SECONDS).getStatus());
    }

    @Test public void revocationBeforeDeliveryIsDistinguishedFromUnknownMutationOutcome() throws Exception {
        bindBroker(new AgentBroker()); AgentRegistryService.JoinRequest old = join(null);
        Future<RpcResponse> queued = executor.submit(() -> broker.call(ADDRESS, "POST", "/api/worker/start", "{}".getBytes(), null, 10000));
        waitPending(broker); registry.revoke(ADDRESS);
        try { queued.get(2, TimeUnit.SECONDS); fail(); }
        catch (ExecutionException expected) { assertTrue(causedBy(expected, AgentUnavailableException.class)); }

        AgentRegistryService.JoinRequest replacement = join(old);
        Future<RpcResponse> delivered = executor.submit(() -> broker.call(ADDRESS, "POST", "/api/worker/start", "{}".getBytes(), null, 10000));
        RpcCommand command = broker.poll(ADDRESS, 1000); assertNotNull(command);
        registry.revoke(ADDRESS);
        try { delivered.get(2, TimeUnit.SECONDS); fail(); }
        catch (ExecutionException expected) {
            assertTrue(causedBy(expected, IOException.class));
            assertFalse("A delivered mutation must not be eligible for automatic replay", causedBy(expected, AgentUnavailableException.class));
            assertTrue(expected.getCause().getCause().getMessage().contains("outcome may be unknown"));
        }
        assertNull(registry.authenticatedAddress(replacement.getCredential()));
        assertFalse(broker.complete(ADDRESS, result(command)));
    }

    @Test public void staleCallSnapshotCannotRecreateAnInvalidatedEnrollmentLane() throws Exception {
        CountDownLatch captured = new CountDownLatch(1), releaseSnapshot = new CountDownLatch(1);
        AgentRegistryService snapshotRegistry = mock(AgentRegistryService.class);
        when(snapshotRegistry.currentEnrollment(ADDRESS)).thenAnswer(call -> {
            captured.countDown(); assertTrue(releaseSnapshot.await(2, TimeUnit.SECONDS)); return "old-enrollment";
        });
        broker = new AgentBroker(); ReflectionTestUtils.setField(broker, "registry", snapshotRegistry);
        Future<RpcResponse> call = executor.submit(() -> broker.call(ADDRESS, "POST", "/api/worker/start", "{}".getBytes(), null, 10000));
        assertTrue(captured.await(2, TimeUnit.SECONDS));
        broker.invalidate(ADDRESS, "old-enrollment"); releaseSnapshot.countDown();
        try { call.get(2, TimeUnit.SECONDS); fail(); }
        catch (ExecutionException expected) { assertTrue(causedBy(expected, AgentUnavailableException.class)); }
        assertNull(broker.poll(ADDRESS, "old-enrollment", 0)); assertEquals(0, broker.pendingCount());
    }

    @Test public void obsoleteSessionCannotTouchOrMarkReplacementReady() {
        AgentRegistryService.JoinRequest old = join(null);
        AgentRegistryService.Session session = registry.authenticatedSession(old.getCredential());
        registry.revoke(ADDRESS); AgentRegistryService.JoinRequest replacement = join(old);
        assertFalse(registry.touch(session)); assertEquals(0, registry.lastSeen(ADDRESS));
        assertFalse(registry.markSynchronized(session, registry.storageVersion())); assertFalse(registry.online(ADDRESS));
        AgentRegistryService.Session current = registry.authenticatedSession(replacement.getCredential());
        assertTrue(registry.touch(current)); assertTrue(registry.markSynchronized(current, registry.storageVersion()));
        assertTrue(registry.online(current)); assertFalse(registry.online(session));
    }

    @Test public void failedDurableRevocationDoesNotInvalidateTheCurrentEnrollment() {
        bindBroker(new AgentBroker()); AgentRegistryService.JoinRequest current = join(null);
        AgentRegistryService.Session session = registry.authenticatedSession(current.getCredential());
        doThrow(new IllegalStateException("storage unavailable")).when(db).putSync(eq("agent:member:" + ADDRESS), any());
        try { registry.revoke(ADDRESS); fail(); } catch (IllegalStateException expected) { }
        assertTrue(registry.isSessionValid(current.getCredential(), session));
        assertEquals(session.getEnrollmentId(), registry.currentEnrollment(ADDRESS));
    }

    @Test public void existingRecordsReceiveADurableEnrollmentIdExactlyOnce() {
        String credential = AgentRegistryService.randomSecret();
        AgentRegistryService.AgentRecord existing = new AgentRegistryService.AgentRecord();
        existing.setAddress(ADDRESS); existing.setInstanceId(UUID.randomUUID().toString());
        existing.setCredentialHash(AgentRegistryService.hash(credential));
        when(db.scanByPrefix("agent:member:", AgentRegistryService.AgentRecord.class)).thenReturn(Collections.singletonList(existing));
        registry.init(); String enrollment = registry.authenticatedSession(credential).getEnrollmentId();
        assertNotNull(enrollment); assertFalse(enrollment.isEmpty());
        assertEquals(enrollment, ((AgentRegistryService.AgentRecord)durable.get("agent:member:" + ADDRESS)).getEnrollmentId());
        registry.init(); assertEquals(enrollment, registry.currentEnrollment(ADDRESS));
        verify(db, times(1)).putSync(eq("agent:member:" + ADDRESS), any());
    }
}
