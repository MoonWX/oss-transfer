package com.transfer.tools.service;

import com.amazonaws.ClientConfiguration;
import com.amazonaws.SdkClientException;
import com.amazonaws.auth.AWSStaticCredentialsProvider;
import com.amazonaws.auth.BasicAWSCredentials;
import com.amazonaws.client.builder.AwsClientBuilder;
import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.AmazonS3ClientBuilder;
import com.amazonaws.services.s3.model.*;
import com.sun.net.httpserver.HttpServer;
import com.transfer.tools.config.DestBucketConfig;
import com.transfer.tools.config.RateLimitConfig;
import com.transfer.tools.config.TransferConfig;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.OssKeyMapping;
import com.transfer.tools.model.RateLimitRequest;
import com.fasterxml.jackson.databind.ObjectMapper;
import org.mockito.ArgumentCaptor;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.io.*;
import java.net.InetSocketAddress;
import java.nio.file.*;
import java.security.MessageDigest;
import java.util.*;
import java.util.concurrent.*;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.AtomicReference;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

/** Real AWS SDK signing against a loopback HTTP endpoint, never a cloud account. */
public class ObjectMigrationReplayTest {
    @Rule public TemporaryFolder temp = new TemporaryFolder();
    private final byte[] payload = new byte[1024 * 1024 + 123];
    private final List<byte[]> received = new CopyOnWriteArrayList<>();
    private final List<String> contentHashes = new CopyOnWriteArrayList<>();
    private final List<String> destinationPaths = new CopyOnWriteArrayList<>();
    private final List<String> rawDestinationPaths = new CopyOnWriteArrayList<>();
    private HttpServer server;
    private AmazonS3 destination;
    private AmazonS3 source;
    private RateLimiterManager rates;
    private ObjectMigrationService migration;
    private LocalTaskControlService control;
    private Path replayDirectory;
    private RocksDBService replayDatabase;
    private final AtomicInteger rejectRequests = new AtomicInteger();

    @Before public void setUp() throws Exception {
        new Random(913).nextBytes(payload);
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/", exchange -> {
            try {
                if ("GET".equals(exchange.getRequestMethod())) {
                    exchange.sendResponseHeaders(200, payload.length); exchange.getResponseBody().write(payload); return;
                }
                destinationPaths.add(exchange.getRequestURI().getPath());
                rawDestinationPaths.add(exchange.getRequestURI().getRawPath());
                ByteArrayOutputStream body = new ByteArrayOutputStream();
                byte[] buffer = new byte[8192]; int n;
                while ((n = exchange.getRequestBody().read(buffer)) >= 0) body.write(buffer, 0, n);
                if ("POST".equals(exchange.getRequestMethod())) {
                    String query = exchange.getRequestURI().getRawQuery();
                    byte[] xml = (query != null && query.startsWith("uploads")
                            ? "<InitiateMultipartUploadResult><Bucket>target</Bucket><Key>known.bin</Key><UploadId>qa-upload</UploadId></InitiateMultipartUploadResult>"
                            : "<CompleteMultipartUploadResult><Location>fixture</Location><Bucket>target</Bucket><Key>known.bin</Key><ETag>qa-etag</ETag></CompleteMultipartUploadResult>").getBytes("UTF-8");
                    exchange.sendResponseHeaders(200, xml.length); exchange.getResponseBody().write(xml); return;
                }
                received.add(body.toByteArray());
                contentHashes.add(exchange.getRequestHeaders().getFirst("x-amz-content-sha256"));
                if (rejectRequests.getAndDecrement() > 0) {
                    byte[] error = "<Error><Code>InternalError</Code><Message>retry fixture</Message></Error>".getBytes("UTF-8");
                    exchange.sendResponseHeaders(500, error.length); exchange.getResponseBody().write(error);
                } else {
                    exchange.getResponseHeaders().set("ETag", "\"qa-etag\"");
                    exchange.sendResponseHeaders(200, -1);
                }
            } finally { exchange.close(); }
        });
        server.start();
        ClientConfiguration clientConfig = new ClientConfiguration().withConnectionTimeout(3000)
                .withSocketTimeout(3000).withMaxErrorRetry(1).withUseExpectContinue(false);
        destination = AmazonS3ClientBuilder.standard()
                .withCredentials(new AWSStaticCredentialsProvider(new BasicAWSCredentials("qa-fake", "qa-fake-secret")))
                .withEndpointConfiguration(new AwsClientBuilder.EndpointConfiguration("http://127.0.0.1:" + server.getAddress().getPort(), "default"))
                .withClientConfiguration(clientConfig).withPathStyleAccessEnabled(true).disableChunkedEncoding().build();
        source = mock(AmazonS3.class);
        S3ClientManager clients = mock(S3ClientManager.class);
        when(clients.getClient("source")).thenReturn(source); when(clients.getClient("target")).thenReturn(destination);
        rates = mock(RateLimiterManager.class);
        RateLimitConfig.AutoAdjustConfig adjust = new RateLimitConfig.AutoAdjustConfig(); adjust.setMaxRetryOn503(0);
        when(rates.getAutoAdjustConfig()).thenReturn(adjust);
        DestBucketConfig target = new DestBucketConfig(); DestBucketConfig.MultipartConfig multipart = new DestBucketConfig.MultipartConfig();
        multipart.setThreshold(32L * 1024 * 1024); multipart.setPartsize(5L * 1024 * 1024); target.setMultipart(multipart);
        replayDirectory = temp.newFolder("replay").toPath();
        TransferConfig transfer = new TransferConfig(); transfer.setTmpDir(replayDirectory.toString());
        control = new LocalTaskControlService(); migration = new ObjectMigrationService();
        ReflectionTestUtils.setField(migration, "s3ClientManager", clients);
        ReflectionTestUtils.setField(migration, "rateLimiterManager", rates);
        ReflectionTestUtils.setField(migration, "destBucketConfig", target);
        ReflectionTestUtils.setField(migration, "transferConfig", transfer);
        ReflectionTestUtils.setField(migration, "localTaskControlService", control);
        ReflectionTestUtils.setField(migration, "localTaskRateLimiterService", mock(LocalTaskRateLimiterService.class));
        ReflectionTestUtils.setField(migration, "localFileService", new LocalFileService());
        ReflectionTestUtils.setField(migration, "subTaskManagerService", mock(SubTaskManagerService.class));
        replayDatabase = new RocksDBService();
        ReflectionTestUtils.setField(replayDatabase, "dbPath", temp.newFolder("replay-db").getAbsolutePath());
        replayDatabase.init();
        LocalTemporaryFileRegistry registry = new LocalTemporaryFileRegistry();
        ReflectionTestUtils.setField(registry, "rocksDBService", replayDatabase);
        registry.recoverInterrupted();
        ReflectionTestUtils.setField(migration, "temporaryFileRegistry", registry);
    }

    @After public void tearDown() {
        if (migration != null) migration.destroy();
        if (destination != null) destination.shutdown();
        if (server != null) server.stop(0);
        if (replayDatabase != null) replayDatabase.destroy();
    }

    @Test public void mappedPutKeepsSourceKeyAndRetriesTheExactUnicodeTargetWithoutContextLeak() throws Exception {
        AtomicLong quota = attachTaskBandwidth(90);
        String sourceKey = "源目录/a +%2F//文件.txt", targetKey = "备份/a +%2F//文件.txt";
        ObjectListMessage message = mappedMessage(90, sourceKey, "REPLACE_PREFIX", "源目录/", "备份/", null);
        when(source.getObject("source", sourceKey)).thenAnswer(call -> object(nonRepeatable(payload), payload.length));
        rejectRequests.set(1);
        assertTrue(migration.migrate(message));
        assertEquals(Arrays.asList("/target/" + targetKey, "/target/" + targetKey), destinationPaths);
        // The SDK may encode the second consecutive slash as %2F. The decoded path
        // above must still preserve both slashes and the literal source text "%2F".
        assertTrue(rawDestinationPaths.get(0), rawDestinationPaths.get(0).contains("%252F"));
        assertTrue(rawDestinationPaths.get(0), rawDestinationPaths.get(0).contains("%20%2B"));
        assertEquals("SDK retry must retain the same encoded destination", rawDestinationPaths.get(0), rawDestinationPaths.get(1));
        assertEquals(sourceKey, message.getKey()); assertEquals(sourceKey, message.getObjectSummary().getKey());
        assertEquals(2L * payload.length, quota.get()); assertPayloads(payload);
        verify(source, times(1)).getObject("source", sourceKey);
        // A subsequent legacy call on this executor must not inherit the previous mapping.
        assertTrue(migration.migrateObject(message.getObjectSummary(), "source", "target", true, 90, "source", "target"));
        assertEquals("/target/" + sourceKey, destinationPaths.get(2));
        assertEquals(3L * payload.length, quota.get());
    }

    @Test public void mappedUrlAndDiskUploadsPreserveSourceUrlAndSourceFallbackRecord() throws Exception {
        AtomicLong quota = attachTaskBandwidth(91);
        ObjectListMessage message = mappedMessage(91, "known.bin", "ADD_PREFIX", null, "archive/", null);
        when(source.generatePresignedUrl(eq("source"), eq("known.bin"), any(Date.class)))
                .thenReturn(new java.net.URL("http://127.0.0.1:" + server.getAddress().getPort() + "/download"));
        TransferConfig config = (TransferConfig) ReflectionTestUtils.getField(migration, "transferConfig");
        config.setMode("url"); assertTrue(migration.migrate(message));
        config.setMode("disk"); assertTrue(migration.migrate(message));
        assertEquals(Arrays.asList("/target/archive/known.bin", "/target/archive/known.bin"), destinationPaths);
        verify(source, times(2)).generatePresignedUrl(eq("source"), eq("known.bin"), any(Date.class));
        verify((SubTaskManagerService) ReflectionTestUtils.getField(migration, "subTaskManagerService"))
                .recordFallbackObject(eq(91), eq("known.bin"), anyString(), anyString());
        assertEquals(2L * payload.length, quota.get()); assertPayloads(payload);
    }

    @Test public void mappedSdkAndUrlMultipartUseTargetForInitiatePartAndComplete() throws Exception {
        AtomicLong quota = attachTaskBandwidth(92);
        ObjectListMessage message = mappedMessage(92, "known.bin", "EXACT_KEY", null, null, "single/目标.bin");
        DestBucketConfig config = (DestBucketConfig) ReflectionTestUtils.getField(migration, "destBucketConfig");
        config.getMultipart().setThreshold(1L); ReflectionTestUtils.setField(migration, "partThreadCount", 2); migration.init();
        when(source.getObjectMetadata("source", "known.bin")).thenReturn(object(new ByteArrayInputStream(payload), payload.length).getObjectMetadata());
        when(source.getObject(any(GetObjectRequest.class))).thenAnswer(call -> {
            assertEquals("known.bin", call.<GetObjectRequest>getArgument(0).getKey());
            return object(nonRepeatable(payload), payload.length);
        });
        assertTrue(migration.migrate(message));
        when(source.generatePresignedUrl(eq("source"), eq("known.bin"), any(Date.class)))
                .thenReturn(new java.net.URL("http://127.0.0.1:" + server.getAddress().getPort() + "/download"));
        ((TransferConfig) ReflectionTestUtils.getField(migration, "transferConfig")).setMode("url");
        assertTrue(migration.migrate(message));
        assertEquals(6, destinationPaths.size());
        for (String path : destinationPaths) assertEquals("/target/single/目标.bin", path);
        verify(source).getObjectMetadata("source", "known.bin");
        verify(source).generatePresignedUrl(eq("source"), eq("known.bin"), any(Date.class));
        assertEquals(2L * payload.length, quota.get()); assertPayloads(payload);
    }

    @Test public void mappedDestinationHeadSkipsWithoutReadingTheMissingSource() {
        AmazonS3 target = mock(AmazonS3.class);
        S3ClientManager clients = (S3ClientManager) ReflectionTestUtils.getField(migration, "s3ClientManager");
        when(clients.getClient("target")).thenReturn(target);
        ObjectListMessage message = mappedMessage(93, "missing-source.bin", "EXACT_KEY", null, null, "already-there.bin");
        message.setOverwrite(false); message.getObjectSummary().setLastModified(new Date(1000));
        when(target.getObjectMetadata(any(GetObjectMetadataRequest.class))).thenAnswer(call -> {
            assertEquals("already-there.bin", call.<GetObjectMetadataRequest>getArgument(0).getKey());
            ObjectMetadata metadata = new ObjectMetadata(); metadata.setLastModified(new Date(2000)); return metadata;
        });
        assertFalse(migration.migrate(message));
        verify(source, never()).getObject(anyString(), anyString());
        verify(target, never()).putObject(any(PutObjectRequest.class));
        assertEquals("missing-source.bin", message.getKey());
    }

    @Test public void stoppedMappedMultipartAbortsOnlyTheMappedUploadAndDoesNotStartFallback() {
        AmazonS3 target = mock(AmazonS3.class);
        S3ClientManager clients = (S3ClientManager) ReflectionTestUtils.getField(migration, "s3ClientManager");
        when(clients.getClient("target")).thenReturn(target);
        ObjectListMessage message = mappedMessage(94, "known.bin", "EXACT_KEY", null, null, "stopped/target.bin");
        DestBucketConfig config = (DestBucketConfig) ReflectionTestUtils.getField(migration, "destBucketConfig");
        config.getMultipart().setThreshold(1L); ReflectionTestUtils.setField(migration, "partThreadCount", 1); migration.init();
        when(source.getObjectMetadata("source", "known.bin")).thenReturn(object(new ByteArrayInputStream(payload), payload.length).getObjectMetadata());
        when(target.initiateMultipartUpload(any(InitiateMultipartUploadRequest.class))).thenAnswer(call -> {
            assertEquals("stopped/target.bin", call.<InitiateMultipartUploadRequest>getArgument(0).getKey());
            InitiateMultipartUploadResult result = new InitiateMultipartUploadResult(); result.setUploadId("stopped-upload"); return result;
        });
        when(source.getObject(any(GetObjectRequest.class))).thenAnswer(call -> {
            assertEquals("known.bin", call.<GetObjectRequest>getArgument(0).getKey());
            control.stop(94); throw new TaskCancelledException("stopped fixture");
        });
        try { migration.migrate(message); fail("must stop"); } catch (TaskCancelledException expected) { }
        ArgumentCaptor<AbortMultipartUploadRequest> aborted = ArgumentCaptor.forClass(AbortMultipartUploadRequest.class);
        verify(target).abortMultipartUpload(aborted.capture());
        assertEquals("stopped/target.bin", aborted.getValue().getKey()); assertEquals("stopped-upload", aborted.getValue().getUploadId());
        verify(target, never()).completeMultipartUpload(any(CompleteMultipartUploadRequest.class));
        verify(source, never()).generatePresignedUrl(anyString(), anyString(), any(Date.class));
        assertEquals("known.bin", message.getKey()); assertFalse(control.isActive(94));
    }

    @Test public void stoppedMultipartWaitsForItsPartOperationToExitBeforeAborting() throws Exception {
        AmazonS3 target = mock(AmazonS3.class);
        S3ClientManager clients = (S3ClientManager) ReflectionTestUtils.getField(migration, "s3ClientManager");
        when(clients.getClient("target")).thenReturn(target);
        ObjectListMessage message = mappedMessage(96, "known.bin", "EXACT_KEY", null, null, "stopped/target.bin");
        DestBucketConfig config = (DestBucketConfig) ReflectionTestUtils.getField(migration, "destBucketConfig");
        config.getMultipart().setThreshold(1L); ReflectionTestUtils.setField(migration, "partThreadCount", 1); migration.init();
        when(source.getObjectMetadata("source", "known.bin")).thenReturn(object(new ByteArrayInputStream(payload), payload.length).getObjectMetadata());
        InitiateMultipartUploadResult initiated = new InitiateMultipartUploadResult(); initiated.setUploadId("stopped-upload");
        when(target.initiateMultipartUpload(any(InitiateMultipartUploadRequest.class))).thenReturn(initiated);
        CountDownLatch stopped = new CountDownLatch(1), partMayExit = new CountDownLatch(1);
        when(source.getObject(any(GetObjectRequest.class))).thenAnswer(call -> {
            control.stop(96); Thread.interrupted(); stopped.countDown();
            assertTrue("The test must release the running part", partMayExit.await(3, TimeUnit.SECONDS));
            throw new TaskCancelledException("stopped fixture");
        });
        ExecutorService executor = Executors.newSingleThreadExecutor();
        try {
            Future<Boolean> transfer = executor.submit(() -> migration.migrate(message));
            assertTrue(stopped.await(2, TimeUnit.SECONDS));
            try { transfer.get(100, TimeUnit.MILLISECONDS); fail("The part still owns active I/O"); }
            catch (TimeoutException expected) { }
            assertTrue(control.isActive(96));
            verify(target, never()).abortMultipartUpload(any(AbortMultipartUploadRequest.class));
            partMayExit.countDown();
            try { transfer.get(2, TimeUnit.SECONDS); fail("The stopped transfer must not succeed"); }
            catch (ExecutionException expected) { assertTrue(expected.getCause() instanceof TaskCancelledException); }
            assertFalse(control.isActive(96));
            ArgumentCaptor<AbortMultipartUploadRequest> aborted = ArgumentCaptor.forClass(AbortMultipartUploadRequest.class);
            verify(target).abortMultipartUpload(aborted.capture());
            assertEquals("stopped/target.bin", aborted.getValue().getKey());
            verify(target, never()).completeMultipartUpload(any(CompleteMultipartUploadRequest.class));
            verify(source, never()).generatePresignedUrl(anyString(), anyString(), any(Date.class));
        } finally { partMayExit.countDown(); executor.shutdownNow(); }
    }

    @Test public void stalledMultipartHasABoundedStopAndCannotUploadAfterAbortEvenIfIoIgnoresInterruption() throws Exception {
        AmazonS3 target = mock(AmazonS3.class);
        S3ClientManager clients = (S3ClientManager) ReflectionTestUtils.getField(migration, "s3ClientManager");
        when(clients.getClient("target")).thenReturn(target);
        ReflectionTestUtils.setField(migration, "partThreadCount", 1);
        ReflectionTestUtils.setField(migration, "partSettleTimeoutMillis", 50L); migration.init();
        when(source.getObjectMetadata("source", "known.bin")).thenReturn(object(new ByteArrayInputStream(payload), payload.length).getObjectMetadata());
        InitiateMultipartUploadResult initiated = new InitiateMultipartUploadResult(); initiated.setUploadId("stalled-upload");
        when(target.initiateMultipartUpload(any(InitiateMultipartUploadRequest.class))).thenReturn(initiated);
        CountDownLatch partStarted = new CountDownLatch(1), releaseIo = new CountDownLatch(1), objectClosed = new CountDownLatch(1);
        S3Object lateObject = spy(object(nonRepeatable(payload), payload.length));
        doAnswer(call -> { try { return call.callRealMethod(); } finally { objectClosed.countDown(); } }).when(lateObject).close();
        when(source.getObject(any(GetObjectRequest.class))).thenAnswer(call -> {
            partStarted.countDown();
            // Some blocking clients ignore or clear interruption before returning their response.
            while (releaseIo.getCount() != 0) try { releaseIo.await(); } catch (InterruptedException ignored) { }
            return lateObject;
        });
        AtomicReference<Thread> migrationThread = new AtomicReference<>();
        ExecutorService executor = Executors.newSingleThreadExecutor();
        try {
            Future<?> transfer = executor.submit(() -> {
                migrationThread.set(Thread.currentThread());
                ReflectionTestUtils.invokeMethod(migration, "migrateWithMultipart", "known.bin", (long) payload.length,
                        "source", "target", "source", "target");
            });
            assertTrue(partStarted.await(2, TimeUnit.SECONDS)); migrationThread.get().interrupt();
            try { transfer.get(2, TimeUnit.SECONDS); fail("Stalled I/O must not make stop unbounded"); }
            catch (ExecutionException expected) { assertTrue(expected.getCause() instanceof RuntimeException); }
            verify(target).abortMultipartUpload(any(AbortMultipartUploadRequest.class));
            releaseIo.countDown(); assertTrue(objectClosed.await(2, TimeUnit.SECONDS));
            verify(target, never()).uploadPart(any(UploadPartRequest.class));
            verify(target, never()).completeMultipartUpload(any(CompleteMultipartUploadRequest.class));
        } finally { releaseIo.countDown(); executor.shutdownNow(); }
    }

    @Test public void mapped503EvidenceUsesTheRequestSideKeyAndKeepsTheSourceIdentity() {
        AmazonS3 target = mock(AmazonS3.class);
        S3ClientManager clients = (S3ClientManager) ReflectionTestUtils.getField(migration, "s3ClientManager");
        when(clients.getClient("target")).thenReturn(target);
        ObjectListMessage message = mappedMessage(95, "known.bin", "EXACT_KEY", null, null, "errors/target.bin");
        when(source.getObject("source", "known.bin")).thenAnswer(call -> object(nonRepeatable(payload), payload.length));
        AmazonS3Exception denied = new AmazonS3Exception("SlowDown"); denied.setStatusCode(503); denied.setErrorCode("SlowDown");
        when(target.putObject(eq("target"), eq("errors/target.bin"), any(InputStream.class), any(ObjectMetadata.class))).thenThrow(denied);
        try { migration.migrate(message); fail(); } catch (RuntimeException expected) { }
        ArgumentCaptor<RateLimitRequest> events = ArgumentCaptor.forClass(RateLimitRequest.class);
        verify(rates, atLeastOnce()).onOss503SlowDown(events.capture());
        for (RateLimitRequest event : events.getAllValues()) {
            assertEquals("errors/target.bin", event.getKey()); assertEquals("PUT", event.getMethod()); assertEquals("target", event.getBucket());
        }
        assertEquals("known.bin", message.getKey());
        clearInvocations(rates);
        AmazonS3Exception sourceDenied = new AmazonS3Exception("SlowDown"); sourceDenied.setStatusCode(503); sourceDenied.setErrorCode("SlowDown");
        when(source.getObject("source", "known.bin")).thenThrow(sourceDenied);
        try { migration.migrate(message); fail(); } catch (RuntimeException expected) { }
        ArgumentCaptor<RateLimitRequest> reads = ArgumentCaptor.forClass(RateLimitRequest.class);
        verify(rates, atLeastOnce()).onOss503SlowDown(reads.capture());
        for (RateLimitRequest event : reads.getAllValues()) { assertEquals("known.bin", event.getKey()); assertEquals("GET", event.getMethod()); }
    }

    private ObjectListMessage mappedMessage(int id, String sourceKey, String mode, String sourcePrefix, String targetPrefix, String targetKey) {
        Map<String, Object> rule = new LinkedHashMap<>(); rule.put("mode", mode);
        rule.put("sourcePrefix", sourcePrefix); rule.put("targetPrefix", targetPrefix); rule.put("targetKey", targetKey);
        S3ObjectSummary object = summary(); object.setKey(sourceKey);
        ObjectListMessage message = new ObjectListMessage(object, id, false, "source", "target", "source", "target", true, sourceKey);
        message.setKeyMapping(new ObjectMapper().convertValue(rule, OssKeyMapping.class)); return message;
    }

    @Test public void taskQuotaCountsSdkPayloadRetryButNotSourceReadOrSignaturePreRead() throws Exception {
        AtomicLong acquired = attachTaskBandwidth(71);
        when(source.getObject("source", "known.bin")).thenReturn(object(nonRepeatable(payload), payload.length));
        rejectRequests.set(1);
        assertTrue(migration.migrateObject(summary(), "source", "target", true, 71, "source", "target"));
        assertEquals(2, received.size()); assertPayloads(payload);
        assertEquals(2L * payload.length, acquired.get());
        verify(rates, atLeastOnce()).acquireBandwidth(anyInt());
    }

    @Test public void localUploadAndRangedPartCountOnlyActualSignedHttpBodies() throws Exception {
        AtomicLong acquired = attachTaskBandwidth(72);
        Path file = temp.newFile("task-limit-local.bin").toPath(); Files.write(file, payload);
        ObjectListMessage message = new ObjectListMessage(); message.setTaskIndex(72); message.setDestBucket("target");
        message.setDestS3Alias("target"); message.setKey("local.bin");
        ReflectionTestUtils.invokeMethod(migration, "uploadLocalWithRetry", message, file, (long) payload.length);
        assertEquals(payload.length, acquired.get());
        ReflectionTestUtils.invokeMethod(migration, "uploadLocalPartWithRetry", destination, message, file,
                "qa-upload", 1, 0L, (long) payload.length);
        assertEquals(2L * payload.length, acquired.get()); assertEquals(2, received.size()); assertPayloads(payload);
        verify((LocalTaskRateLimiterService) ReflectionTestUtils.getField(migration, "localTaskRateLimiterService"), never())
                .acquireBandwidth(any(ObjectListMessage.class), anyInt());
    }

    @Test public void multipartChildThreadsRetainTheSameTaskBandwidthScope() throws Exception {
        AtomicLong acquired = attachTaskBandwidth(73);
        DestBucketConfig target = (DestBucketConfig) ReflectionTestUtils.getField(migration, "destBucketConfig");
        target.getMultipart().setThreshold(1L);
        ReflectionTestUtils.setField(migration, "partThreadCount", 2); migration.init();
        when(source.getObject(any(GetObjectRequest.class))).thenReturn(object(nonRepeatable(payload), payload.length));
        when(source.getObjectMetadata("source", "known.bin")).thenReturn(object(new ByteArrayInputStream(payload), payload.length).getObjectMetadata());
        assertTrue(migration.migrateObject(summary(), "source", "target", true, 73, "source", "target"));
        assertEquals(payload.length, acquired.get()); assertEquals(1, received.size()); assertPayloads(payload);
    }

    @Test public void urlAndDiskFallbackUseTheSameTaskQuotaWithoutCountingTheDownloadLeg() throws Exception {
        AtomicLong acquired = attachTaskBandwidth(74);
        when(source.generatePresignedUrl(eq("source"), eq("known.bin"), any(Date.class)))
                .thenReturn(new java.net.URL("http://127.0.0.1:" + server.getAddress().getPort() + "/download"));
        TransferConfig config = (TransferConfig) ReflectionTestUtils.getField(migration, "transferConfig");
        config.setMode("url");
        assertTrue(migration.migrateObject(summary(), "source", "target", true, 74, "source", "target"));
        assertEquals(payload.length, acquired.get());
        config.setMode("disk");
        AtomicLong nodeAcquired = new AtomicLong();
        doAnswer(call -> { nodeAcquired.addAndGet(call.<Integer>getArgument(0)); return null; })
                .when(rates).acquireBandwidth(anyInt());
        S3ClientManager clients = (S3ClientManager) ReflectionTestUtils.getField(migration, "s3ClientManager");
        when(clients.getClient("target")).thenAnswer(call -> {
            // This lookup follows source download and precedes target signing or upload.
            assertEquals(payload.length, nodeAcquired.get());
            assertEquals("source download must not consume task upload quota", payload.length, acquired.get());
            return destination;
        });
        assertTrue(migration.migrateObject(summary(), "source", "target", true, 74, "source", "target"));
        assertEquals(2L * payload.length, acquired.get()); assertEquals(2, received.size()); assertPayloads(payload);
    }

    @Test public void downloadUsesOneTaskQuotaReadLegAndExplicitScopeMustBeInstalledBeforeIo() throws Exception {
        AtomicLong acquired = attachTaskBandwidth(75);
        when(source.getObject("source", "known.bin")).thenReturn(object(nonRepeatable(payload), payload.length));
        LocalFileService files = mock(LocalFileService.class);
        ReflectionTestUtils.setField(migration, "localFileService", files);
        when(files.writeAtomically(any(InputStream.class), anyString(), anyString(), anyInt(), any(), anyLong())).thenAnswer(call -> {
            byte[] bytes = new byte[8192]; int count; ByteArrayOutputStream data = new ByteArrayOutputStream();
            InputStream input = call.getArgument(0);
            while ((count = input.read(bytes)) != -1) data.write(bytes, 0, count);
            assertArrayEquals(payload, data.toByteArray()); return true;
        });
        ObjectListMessage message = new ObjectListMessage(summary(), 75, false, "source", "target", "source", "target", true, "known.bin");
        message.setDirection(com.transfer.tools.model.TransferDirection.OSS_TO_LOCAL); message.setLocalPath(replayDirectory.toString());
        message.setTaskBandwidthScopeId("missing");
        try { migration.migrate(message); fail(); } catch (TransferValidationException expected) { }
        verify(source, never()).getObject(anyString(), anyString());
        message.setTaskBandwidthScopeId(null);
        assertTrue(migration.migrate(message)); assertEquals(payload.length, acquired.get());
    }

    @Test public void stopInterruptsRealSdkUploadWaitingOnTaskQuota() throws Exception {
        attachTaskBandwidth(76);
        WorkerTaskBandwidthService service = (WorkerTaskBandwidthService) ReflectionTestUtils.getField(migration, "taskBandwidth");
        com.transfer.tools.model.TaskBandwidthPolicy policy = service.get("task:76");
        policy.setRevision(2); policy.setLimitMbps(0.1); service.apply(policy);
        CountDownLatch waiting = new CountDownLatch(1);
        doAnswer(call -> { waiting.countDown(); return call.callRealMethod(); })
                .when(service).reserve(any(WorkerTaskBandwidthService.Scope.class), anyInt());
        when(source.getObject("source", "known.bin")).thenReturn(object(nonRepeatable(payload), payload.length));
        ObjectListMessage message = new ObjectListMessage(summary(), 76, false, "source", "target", "source", "target", true, "known.bin");
        ExecutorService executor = Executors.newSingleThreadExecutor();
        try {
            Future<?> transfer = executor.submit(() -> migration.migrate(message));
            assertTrue(waiting.await(3, TimeUnit.SECONDS));
            assertFalse(transfer.isDone()); control.stop(76);
            try { transfer.get(2, TimeUnit.SECONDS); fail("stopped task must exit quota wait"); }
            catch (ExecutionException expected) { assertTrue(expected.getCause() instanceof TaskCancelledException); }
            assertEquals(Double.valueOf(0.1), service.get("task:76").getLimitMbps());
        } finally { executor.shutdownNow(); }
    }

    private AtomicLong attachTaskBandwidth(int id) {
        WorkerTaskBandwidthService service = spy(new WorkerTaskBandwidthService());
        com.transfer.tools.config.DistributedConfig config = mock(com.transfer.tools.config.DistributedConfig.class);
        when(config.getLocalIp()).thenReturn("one");
        ReflectionTestUtils.setField(service, "rocksDBService", replayDatabase);
        ReflectionTestUtils.setField(service, "distributedConfig", config); ReflectionTestUtils.setField(service, "control", control);
        service.restore();
        com.transfer.tools.model.TaskBandwidthPolicy policy = new com.transfer.tools.model.TaskBandwidthPolicy();
        policy.setScopeId("task:" + id); policy.setRevision(1); policy.setMembers(Collections.singletonList("one")); service.apply(policy);
        ReflectionTestUtils.setField(migration, "taskBandwidth", service);
        AtomicLong acquired = new AtomicLong();
        TransferSpeedService speed = mock(TransferSpeedService.class);
        ReflectionTestUtils.setField(migration, "transferSpeedService", speed);
        doAnswer(call -> { acquired.addAndGet(call.<Long>getArgument(1)); return null; })
                .when(speed).record(eq(id), anyLong());
        // The integration assertions count actual HTTP payload bytes. Dedicated stream tests
        // additionally check that the shared quota is reserved before reads and refunds unused bytes.
        return acquired;
    }

    @Test public void interruptedReplayIsRegisteredAndRemovedAfterDatabaseReopen() throws Exception {
        Path legacy = Files.write(replayDirectory.resolve("oss-upload-replay-legacy.tmp"), new byte[]{9});
        InputStream replay = ReflectionTestUtils.invokeMethod(migration, "replayableUploadStream",
                nonRepeatable(payload), (long) payload.length);
        assertNotNull(replay);
        try {
            List<LocalTemporaryFileRegistry.Entry> entries = replayDatabase.scanByPrefix(
                    LocalTemporaryFileRegistry.PREFIX, LocalTemporaryFileRegistry.Entry.class);
            assertEquals(1, entries.size());
            Path spool = Paths.get(entries.get(0).temp);
            assertArrayEquals(payload, Files.readAllBytes(spool));
            // Reopen the WAL without running the old stream's normal cleanup, as after process exit.
            replayDatabase.destroy();
            replayDatabase.init();
            LocalTemporaryFileRegistry nextProcess = new LocalTemporaryFileRegistry();
            ReflectionTestUtils.setField(nextProcess, "rocksDBService", replayDatabase);
            nextProcess.recoverInterrupted();
            assertFalse(Files.exists(spool));
            assertArrayEquals(new byte[]{9}, Files.readAllBytes(legacy));
            assertTrue(replayDatabase.scanByPrefix(LocalTemporaryFileRegistry.PREFIX,
                    LocalTemporaryFileRegistry.Entry.class).isEmpty());
        } finally { replay.close(); }
    }

    @Test public void originalNonRepeatableKnownLengthStreamFailsBeforeHttpSend() throws Exception {
        ObjectMetadata metadata = new ObjectMetadata(); metadata.setContentLength(payload.length);
        try (InputStream input = nonRepeatable(payload)) {
            try { destination.putObject("target", "known.bin", input, metadata); fail("original stream must reproduce signature reset failure"); }
            catch (SdkClientException expected) { assertTrue(allMessages(expected), allMessages(expected).contains("Unable to reset stream after calculating AWS4 signature")); }
        }
        assertTrue(received.isEmpty());
    }

    @Test public void knownSizeNetworkPutSignsAndReplaysAfterSdkRetryWithoutHeapSizedBuffer() throws Exception {
        when(source.getObject("source", "known.bin")).thenReturn(object(nonRepeatable(payload), payload.length));
        rejectRequests.set(1);
        S3ObjectSummary summary = summary();
        assertTrue(migration.migrateObject(summary, "source", "target", true, 71, "source", "target"));
        assertEquals(2, received.size()); assertPayloads(payload); assertReplayEmpty();
        verify(source, times(1)).getObject("source", "known.bin");
    }

    @Test public void cachedUploadAndRetryStillAcquireBandwidthDuringDestinationReads() throws Exception {
        when(source.getObject("source", "known.bin")).thenReturn(object(nonRepeatable(payload), payload.length));
        AmazonS3 target = mock(AmazonS3.class);
        S3ClientManager clients = mock(S3ClientManager.class);
        when(clients.getClient("source")).thenReturn(source); when(clients.getClient("target")).thenReturn(target);
        ReflectionTestUtils.setField(migration, "s3ClientManager", clients);
        java.util.concurrent.atomic.AtomicLong acquired = new java.util.concurrent.atomic.AtomicLong();
        doAnswer(call -> { acquired.addAndGet(call.<Integer>getArgument(0)); return null; }).when(rates).acquireBandwidth(anyInt());
        when(target.putObject(eq("target"), eq("known.bin"), any(InputStream.class), any(ObjectMetadata.class))).thenAnswer(call -> {
            InputStream upload = call.getArgument(2);
            long preparedBytes = acquired.get();
            assertEquals(payload.length, preparedBytes);
            upload.mark(1);
            for (int replay = 0; replay < 2; replay++) {
                ByteArrayOutputStream bytes = new ByteArrayOutputStream(); byte[] buffer = new byte[8192]; int count;
                while ((count = upload.read(buffer)) >= 0) bytes.write(buffer, 0, count);
                assertArrayEquals(payload, bytes.toByteArray());
                assertEquals(preparedBytes + (replay + 1L) * payload.length, acquired.get());
                upload.reset();
            }
            return new PutObjectResult();
        });
        assertTrue(migration.migrateObject(summary(), "source", "target", true, 75, "source", "target"));
        assertReplayEmpty();
    }

    @Test public void networkMultipartPartHasReplayableSignedBody() throws Exception {
        when(source.getObject(any(GetObjectRequest.class))).thenReturn(object(nonRepeatable(payload), payload.length));
        Object result = ReflectionTestUtils.invokeMethod(migration, "uploadPart", "known.bin", "source", "target", "upload-qa", 1, 0L, (long) payload.length - 1, "source", "target");
        assertNotNull(result); assertEquals(1, received.size()); assertPayloads(payload); assertReplayEmpty();
    }

    @Test public void localPartSeeksOnlyItsRangeForSigningAndBody() throws Exception {
        Path file = temp.newFile("local.bin").toPath();
        try (OutputStream output = Files.newOutputStream(file)) { output.write(new byte[7]); output.write(payload); output.write(new byte[11]); }
        ObjectListMessage message = new ObjectListMessage(); message.setTaskIndex(72); message.setDestBucket("target"); message.setDestS3Alias("target"); message.setKey("part.bin");
        Object result = ReflectionTestUtils.invokeMethod(migration, "uploadLocalPartWithRetry", destination, message, file, "upload-qa", 1, 7L, (long) payload.length);
        assertNotNull(result); assertEquals(1, received.size()); assertPayloads(payload); assertReplayEmpty();
    }

    @Test public void localPutUsesSeekableOriginalFile() throws Exception {
        Path file = temp.newFile("local-put.bin").toPath(); Files.write(file, payload);
        ObjectListMessage message = new ObjectListMessage(); message.setTaskIndex(73); message.setDestBucket("target"); message.setDestS3Alias("target"); message.setKey("local.bin");
        ReflectionTestUtils.invokeMethod(migration, "uploadLocalWithRetry", message, file, (long) payload.length);
        assertEquals(1, received.size()); assertPayloads(payload); assertReplayEmpty(); assertArrayEquals(payload, Files.readAllBytes(file));
    }

    @Test public void shortAndOversizedSourceCleanBoundedReplayFile() throws Exception {
        for (long expected : new long[]{payload.length - 1L, payload.length + 1L}) {
            try (InputStream input = nonRepeatable(payload)) {
                try { ReflectionTestUtils.invokeMethod(migration, "replayableUploadStream", input, expected); fail("size mismatch must fail"); }
                catch (Exception expectedFailure) { assertTrue(allMessages(expectedFailure).contains("upload source")); }
            }
            assertReplayEmpty();
        }
    }

    @Test public void stopDuringSpoolingClosesSourceAndDeletesReplayFile() throws Exception {
        CountDownLatch blocked = new CountDownLatch(1), closed = new CountDownLatch(1);
        InputStream slow = new InputStream() {
            int position;
            @Override public int read() throws IOException { byte[] one = new byte[1]; return read(one, 0, 1) < 0 ? -1 : one[0] & 255; }
            @Override public int read(byte[] bytes, int offset, int length) throws IOException {
                if (position >= 128 * 1024) {
                    blocked.countDown();
                    try { closed.await(); } catch (InterruptedException error) { throw new IOException("interrupted", error); }
                    throw new IOException("cancelled");
                }
                int size = Math.min(length, 128 * 1024 - position); System.arraycopy(payload, position, bytes, offset, size); position += size; return size;
            }
            @Override public void close() { closed.countDown(); }
        };
        when(source.getObject("source", "known.bin")).thenReturn(object(slow, payload.length));
        ObjectListMessage message = new ObjectListMessage(summary(), 74, false, "source", "target", "source", "target", true, "known.bin");
        ExecutorService executor = Executors.newSingleThreadExecutor();
        try {
            Future<Boolean> future = executor.submit(() -> migration.migrate(message));
            assertTrue(blocked.await(3, TimeUnit.SECONDS)); control.stop(74);
            try { future.get(3, TimeUnit.SECONDS); fail("stopped upload must not succeed"); }
            catch (ExecutionException expected) { assertTrue(expected.getCause() instanceof TaskCancelledException); }
            assertEquals(0, closed.getCount()); assertTrue(received.isEmpty()); assertReplayEmpty();
        } finally { executor.shutdownNow(); }
    }

    private S3ObjectSummary summary() { S3ObjectSummary s = new S3ObjectSummary(); s.setKey("known.bin"); s.setSize(payload.length); s.setLastModified(new Date(1)); return s; }
    private S3Object object(InputStream input, long length) { S3Object o = new S3Object(); o.setObjectContent(input); o.getObjectMetadata().setContentLength(length); return o; }
    private InputStream nonRepeatable(byte[] bytes) { return new FilterInputStream(new ByteArrayInputStream(bytes)) { @Override public boolean markSupported() { return false; } @Override public void reset() throws IOException { throw new IOException("network stream cannot rewind"); } }; }
    private void assertReplayEmpty() throws IOException { try (java.util.stream.Stream<Path> files = Files.list(replayDirectory)) { assertEquals(0L, files.count()); } }
    private void assertPayloads(byte[] bytes) throws Exception {
        StringBuilder expectedHash = new StringBuilder(); for (byte value : MessageDigest.getInstance("SHA-256").digest(bytes)) expectedHash.append(String.format("%02x", value & 255));
        for (int i = 0; i < received.size(); i++) { assertArrayEquals(bytes, received.get(i)); assertEquals(expectedHash.toString(), contentHashes.get(i)); }
    }
    private String allMessages(Throwable error) { StringBuilder text = new StringBuilder(); for (Throwable t = error; t != null; t = t.getCause()) text.append(t.getMessage()).append('\n'); return text.toString(); }
}
