package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpServer;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.controller.MasterController;
import com.transfer.tools.controller.WorkerController;
import com.transfer.tools.model.S3ClientConfig;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.HttpMethod;
import org.springframework.test.util.ReflectionTestUtils;

import java.net.InetSocketAddress;
import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.AtomicReference;
import java.util.concurrent.locks.ReentrantLock;
import java.util.function.BooleanSupplier;

import static org.junit.Assert.*;
import static org.mockito.Mockito.mock;

/** Real controllers, configuration storage and HTTP; only the external S3 SDK is stubbed. */
public class StorageSynchronizationRaceTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private final ExecutorService actors = Executors.newFixedThreadPool(2);
    private final ExecutorService httpThreads = Executors.newCachedThreadPool();
    private final List<Gate> gates = new ArrayList<>();
    private final List<String> workerMutations = new CopyOnWriteArrayList<>();
    private RocksDBService masterDb, workerDb;
    private BlockingClients masterClients;
    private S3ClientManager workerClients;
    private MasterController master;
    private WorkerController worker;
    private NodeConfigService nodes;
    private ObservingRegistry reconciliation;
    private HttpServer server;

    @Before public void setup() throws Exception {
        masterDb = database("master"); workerDb = database("worker");
        masterClients = new BlockingClients(); workerClients = new BlockingClients();
        ReflectionTestUtils.setField(masterClients, "rocksDBService", masterDb);
        ReflectionTestUtils.setField(workerClients, "rocksDBService", workerDb);
        worker = new WorkerController(); ReflectionTestUtils.setField(worker, "s3ClientManager", workerClients);
        ReflectionTestUtils.setField(worker, "nodeConfigService", new NodeConfigService());
        ObjectMapper mapper = new ObjectMapper();
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.setExecutor(httpThreads);
        server.createContext("/api/worker/s3-config", exchange -> {
            String method = exchange.getRequestMethod();
            String path = exchange.getRequestURI().getPath();
            Map<String, Object> response;
            if ("GET".equals(method)) response = worker.getAllS3Configs();
            else {
                workerMutations.add(method);
                if ("DELETE".equals(method)) response = worker.removeS3Config("ci");
                else {
                    @SuppressWarnings("unchecked") Map<String, Object> body = mapper.readValue(exchange.getRequestBody(), Map.class);
                    response = "POST".equals(method) ? worker.addS3Config(body)
                            : worker.updateS3Config(path.substring(path.lastIndexOf('/') + 1), body);
                }
            }
            byte[] bytes = mapper.writeValueAsBytes(response);
            exchange.getResponseHeaders().set("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, bytes.length);
            exchange.getResponseBody().write(bytes); exchange.close();
        });
        server.start();
        DistributedConfig config = new DistributedConfig();
        config.setMasterIp("192.0.2.1"); config.setLocalIp("192.0.2.1");
        config.setPort(server.getAddress().getPort());
        config.setWorkerIps(Arrays.asList("192.0.2.1", "127.0.0.1"));
        nodes = new NodeConfigService();
        ReflectionTestUtils.setField(nodes, "rocksDBService", masterDb);
        ReflectionTestUtils.setField(nodes, "distributedConfig", config); nodes.init();
        ReflectionTestUtils.setField(config, "nodeConfigService", nodes);
        master = new MasterController();
        ReflectionTestUtils.setField(master, "s3ClientManager", masterClients);
        ReflectionTestUtils.setField(master, "distributedConfig", config);
        ReflectionTestUtils.setField(master, "nodeConfigService", nodes);
        reconciliation = new ObservingRegistry();
        ReflectionTestUtils.setField(reconciliation, "config", config);
        ReflectionTestUtils.setField(reconciliation, "nodeConfig", nodes);
        ReflectionTestUtils.setField(reconciliation, "s3Clients", masterClients);
        ReflectionTestUtils.setField(master, "nodeRegistryService", reconciliation);
    }

    @After public void close() throws Exception {
        for (Gate gate : gates) gate.release.countDown();
        actors.shutdownNow(); actors.awaitTermination(5, TimeUnit.SECONDS);
        if (server != null) server.stop(0);
        httpThreads.shutdownNow(); httpThreads.awaitTermination(5, TimeUnit.SECONDS);
        if (masterDb != null) masterDb.destroy();
        if (workerDb != null) workerDb.destroy();
    }

    @Test(timeout = 15000) public void backgroundReconciliationCannotDuplicateAnInFlightUserCreate() throws Exception {
        Gate saved = gate(); masterClients.afterCreate = saved;
        Future<Map<String, Object>> request = actors.submit(() -> master.addS3Config(params("http://source.example")));
        saved.awaitEntered(); // The Master has persisted ci; its worker POST has not happened yet.
        AtomicReference<Thread> backgroundThread = new AtomicReference<>();
        Future<?> background = actors.submit(() -> { backgroundThread.set(Thread.currentThread()); reconciliation.synchronizeAgentStorage("127.0.0.1"); });
        awaitQueuedOrFinished(backgroundThread, background);
        saved.release.countDown();
        assertComplete(request.get(5, TimeUnit.SECONDS)); background.get(5, TimeUnit.SECONDS);
        assertEquals("Exactly one real worker POST must create the configuration", Collections.singletonList("POST"), workerMutations);
        assertEquals("http://source.example", workerClients.getConfig("ci").getEndpoint());
        assertNotNull(workerDb.get("s3_config_ci", S3ClientConfig.class));
    }

    @Test(timeout = 15000) public void backgroundReconciliationCannotDeleteBeforeTheUserDeleteIsAcknowledged() throws Exception {
        assertComplete(master.addS3Config(params("http://source.example"))); workerMutations.clear();
        Gate removed = gate(); masterClients.afterDelete = removed;
        Future<Map<String, Object>> request = actors.submit(() -> master.removeS3Config("ci"));
        removed.awaitEntered();
        AtomicReference<Thread> backgroundThread = new AtomicReference<>();
        Future<?> background = actors.submit(() -> { backgroundThread.set(Thread.currentThread()); reconciliation.synchronizeAgentStorage("127.0.0.1"); });
        awaitQueuedOrFinished(backgroundThread, background);
        removed.release.countDown();
        assertComplete(request.get(5, TimeUnit.SECONDS)); background.get(5, TimeUnit.SECONDS);
        assertEquals(Collections.singletonList("DELETE"), workerMutations);
        assertNull(workerClients.getConfig("ci")); assertNull(workerDb.get("s3_config_ci", S3ClientConfig.class));
    }

    @Test(timeout = 15000) public void anOldBackgroundSnapshotCannotOverwriteANewerUserUpdate() throws Exception {
        assertComplete(master.addS3Config(params("http://old.example")));
        assertEquals(true, worker.updateS3Config("ci", params("http://stale.example")).get("success"));
        Gate oldSnapshot = gate(); reconciliation.beforePut = oldSnapshot;
        Future<?> background = actors.submit(() -> reconciliation.synchronizeAgentStorage("127.0.0.1"));
        oldSnapshot.awaitEntered(); // Reconciliation has captured old.example and is about to PUT it.
        AtomicReference<Thread> requestThread = new AtomicReference<>();
        Future<Map<String, Object>> request = actors.submit(() -> {
            requestThread.set(Thread.currentThread()); return master.updateS3Config("ci", params("http://new.example"));
        });
        awaitQueuedOrFinished(requestThread, request);
        oldSnapshot.release.countDown();
        background.get(5, TimeUnit.SECONDS); assertComplete(request.get(5, TimeUnit.SECONDS));
        assertEquals("http://new.example", workerClients.getConfig("ci").getEndpoint());
        assertEquals("http://new.example", workerDb.get("s3_config_ci", S3ClientConfig.class).getEndpoint());
    }

    @Test public void duplicateUserPostsRemainStrictCreatesAndNeverOverwriteCredentials() {
        assertComplete(master.addS3Config(params("http://original.example"))); workerMutations.clear();
        assertEquals(false, master.addS3Config(params("http://replacement.example")).get("success"));
        assertEquals(false, worker.addS3Config(params("http://replacement.example")).get("success"));
        assertTrue("Rejected Master POST must not reach the worker", workerMutations.isEmpty());
        assertEquals("http://original.example", masterClients.getConfig("ci").getEndpoint());
        assertEquals("http://original.example", workerClients.getConfig("ci").getEndpoint());
    }

    private void awaitQueuedOrFinished(AtomicReference<Thread> thread, Future<?> task) throws Exception {
        ReentrantLock lock = (ReentrantLock) ReflectionTestUtils.getField(nodes, "storageSynchronizationLock");
        // A latch establishes the conflicting operation first. The competitor must
        // either wait for that operation, or (on the broken implementation) finish
        // first and deterministically produce the duplicate/stale-write failure.
        eventually(() -> task.isDone() || (thread.get() != null && lock.hasQueuedThread(thread.get())));
    }
    private static void eventually(BooleanSupplier condition) throws Exception {
        long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(3);
        while (!condition.getAsBoolean() && System.nanoTime() < deadline) Thread.sleep(1);
        assertTrue("Concurrent operation did not reach the synchronization boundary", condition.getAsBoolean());
    }
    private RocksDBService database(String name) throws Exception {
        RocksDBService db = new RocksDBService();
        ReflectionTestUtils.setField(db, "dbPath", temporary.newFolder(name).getAbsolutePath()); db.init(); return db;
    }
    private Gate gate() { Gate gate = new Gate(); gates.add(gate); return gate; }
    private static void assertComplete(Map<String, Object> response) {
        assertEquals(response.toString(), true, response.get("success"));
        assertEquals(response.toString(), false, response.get("partial"));
        assertEquals(response.toString(), 0, response.get("failedCount"));
    }
    private static Map<String, Object> params(String endpoint) {
        Map<String, Object> values = new LinkedHashMap<>();
        values.put("alias", "ci"); values.put("endpoint", endpoint);
        values.put("accessKey", "test-access"); values.put("secretKey", "test-secret");
        values.put("useV2SignerType", false); values.put("pathStyleEnable", true); values.put("validate", false);
        return values;
    }
    private static class Gate {
        final CountDownLatch entered = new CountDownLatch(1), release = new CountDownLatch(1);
        void block() {
            entered.countDown();
            try { if (!release.await(8, TimeUnit.SECONDS)) throw new IllegalStateException("Test did not release operation"); }
            catch (InterruptedException error) { Thread.currentThread().interrupt(); throw new IllegalStateException(error); }
        }
        void awaitEntered() throws Exception { assertTrue("Operation did not reach test boundary", entered.await(3, TimeUnit.SECONDS)); }
    }
    private static class BlockingClients extends S3ClientManager {
        volatile Gate afterCreate, afterDelete;
        @Override AmazonS3 createS3Client(S3ClientConfig config) { return mock(AmazonS3.class); }
        @Override public boolean addClient(String alias, String endpoint, String access, String secret, Boolean v2, Boolean path, boolean validate) {
            boolean result = super.addClient(alias, endpoint, access, secret, v2, path, validate);
            if (afterCreate != null) afterCreate.block(); return result;
        }
        @Override public boolean removeClient(String alias) {
            boolean result = super.removeClient(alias); if (afterDelete != null) afterDelete.block(); return result;
        }
    }
    private static class ObservingRegistry extends NodeRegistryService {
        volatile Gate beforePut;
        @Override Map<String, Object> exchange(String address, String path, HttpMethod method, Object body, boolean authenticated) {
            if (method == HttpMethod.PUT && beforePut != null) beforePut.block();
            return super.exchange(address, path, method, body, authenticated);
        }
    }
}
