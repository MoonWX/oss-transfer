package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.AmazonS3Exception;
import com.amazonaws.services.s3.model.PutObjectResult;
import com.amazonaws.services.s3.model.UploadPartRequest;
import com.amazonaws.services.s3.model.InitiateMultipartUploadRequest;
import com.amazonaws.services.s3.model.InitiateMultipartUploadResult;
import com.amazonaws.services.s3.model.GetObjectMetadataRequest;
import com.amazonaws.services.s3.model.GetObjectRequest;
import com.amazonaws.services.s3.model.ObjectMetadata;
import com.amazonaws.services.s3.model.S3Object;
import com.amazonaws.services.s3.model.S3ObjectSummary;
import com.transfer.tools.config.DestBucketConfig;
import com.transfer.tools.config.RateLimitConfig;
import com.transfer.tools.config.TransferConfig;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.RateLimitRequest;
import com.transfer.tools.model.TransferDirection;
import org.junit.Test;
import org.mockito.ArgumentCaptor;
import org.springframework.test.util.ReflectionTestUtils;

import java.io.ByteArrayInputStream;
import java.io.InputStream;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.concurrent.ExecutorCompletionService;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class ObjectMigrationRateLimitTest {
    @Test
    public void sourceGet503RetainsRequestIdentityWhenTheOuterHandlerCountsItAgain() {
        Fixture f = new Fixture();
        when(f.source.getObject("source-bucket", "sample.bin")).thenThrow(slowDown());
        f.migrateAndExpectFailure(1048);

        List<RateLimitRequest> events = f.requests(2);
        assertEquals("GET_OBJECT", events.get(0).getOperation());
        assertEquals("GET", events.get(0).getMethod());
        assertEquals("source-bucket", events.get(0).getBucket());
        assertEquals("source", events.get(0).getConnectionAlias());
        assertEquals("TRANSFER_RETRY", events.get(0).getHandlingStage());
        assertEquals(Integer.valueOf(1), events.get(0).getAttempt());
        assertEquals("OUTER_RETHROW", events.get(1).getHandlingStage());
        assertEquals(events.get(0).getRequestId(), events.get(1).getRequestId());
        assertEquals("GET_OBJECT", events.get(1).getOperation());
        for (RateLimitRequest event : events) {
            assertEquals(Integer.valueOf(1048), event.getTaskId());
            assertEquals("OSS_TO_OSS", event.getDirection());
            assertEquals("ServiceUnavailable", event.getErrorCode());
            assertEquals("REQUEST_CONTEXT", event.getContextSource());
        }
        assertNull(f.scope().get());
        verify(f.destination, never()).putObject(anyString(), anyString(), any(InputStream.class), any(ObjectMetadata.class));
    }

    @Test
    public void destinationPut503IsNotAttributedToTheSuccessfulSourceGet() {
        Fixture f = new Fixture();
        S3Object object = new S3Object();
        object.setObjectContent(new ByteArrayInputStream(new byte[]{1, 2, 3}));
        object.getObjectMetadata().setContentLength(3);
        when(f.source.getObject("source-bucket", "sample.bin")).thenReturn(object);
        when(f.destination.putObject(eq("target-bucket"), eq("sample.bin"), any(InputStream.class), any(ObjectMetadata.class)))
                .thenThrow(slowDown());
        f.migrateAndExpectFailure(2048);

        for (RateLimitRequest event : f.requests(2)) {
            assertEquals("PUT_OBJECT", event.getOperation());
            assertEquals("PUT", event.getMethod());
            assertEquals("target-bucket", event.getBucket());
            assertEquals("target", event.getConnectionAlias());
            assertEquals(Integer.valueOf(2048), event.getTaskId());
        }
        assertNull(f.scope().get());
    }

    @Test
    public void localUploadHeadRetainsItsActualDirectionAndRestoresTheCallerScope() throws Exception {
        Fixture f = new Fixture();
        Path source = Files.createTempFile("rate-request-context", ".bin");
        RateLimitRequest callerScope = new RateLimitRequest();
        callerScope.setTaskId(77);
        f.scope().set(callerScope);
        ReflectionTestUtils.setField(f.migration, "localTaskControlService", mock(LocalTaskControlService.class));
        ReflectionTestUtils.setField(f.migration, "localTaskRateLimiterService", mock(LocalTaskRateLimiterService.class));
        when(f.destination.getObjectMetadata(any(GetObjectMetadataRequest.class))).thenThrow(slowDown());
        ObjectListMessage message = new ObjectListMessage();
        message.setTaskIndex(3048);
        message.setDirection(TransferDirection.LOCAL_TO_OSS);
        message.setLocalPath(source.toString());
        message.setDestBucket("target-bucket");
        message.setDestS3Alias("target");
        message.setKey("local.bin");
        message.setOverwrite(false);
        try {
            try {
                f.migration.migrateLocalToOss(message);
                fail("Expected a failed HEAD request");
            } catch (RuntimeException expected) { }
            RateLimitRequest event = f.requests(1).get(0);
            assertEquals("GET_OBJECT_METADATA", event.getOperation());
            assertEquals("HEAD", event.getMethod());
            assertEquals("LOCAL_TO_OSS", event.getDirection());
            assertEquals(Integer.valueOf(3048), event.getTaskId());
            assertEquals("target-bucket", event.getBucket());
            assertSame(callerScope, f.scope().get());
        } finally {
            f.scope().remove();
            Files.deleteIfExists(source);
        }
    }

    @Test
    public void multipartWorkerReceivesTaskScopeAndCleansItBeforeReusingTheThread() throws Exception {
        Fixture f = new Fixture();
        when(f.source.getObject(any(GetObjectRequest.class))).thenAnswer(invocation -> { throw slowDown(); });
        ExecutorService executor = Executors.newSingleThreadExecutor();
        ExecutorCompletionService<Object> completion = new ExecutorCompletionService<>(executor);
        RateLimitRequest scope = new RateLimitRequest();
        scope.setTaskId(4048);
        scope.setDirection("OSS_TO_OSS");
        scope.setSourceBucket("source-bucket");
        scope.setDestinationBucket("target-bucket");
        f.scope().set(scope);
        try {
            Future<?> part = ReflectionTestUtils.invokeMethod(f.migration, "submitPart", completion,
                    "sample.bin", 6L, 3L, 2, "source-bucket", "target-bucket", "upload-id", "source", "target");
            expectFailedFuture(part);
            RateLimitRequest event = f.requests(1).get(0);
            assertEquals(Integer.valueOf(4048), event.getTaskId());
            assertEquals(Integer.valueOf(2), event.getPartNumber());
            assertEquals("GET_OBJECT", event.getOperation());
            assertSame(scope, f.scope().get());
            assertNull(executor.submit(() -> f.scope().get()).get(3, TimeUnit.SECONDS));

            clearInvocations(f.manager);
            f.scope().remove();
            Future<?> nextPart = ReflectionTestUtils.invokeMethod(f.migration, "submitPart", completion,
                    "next.bin", 3L, 3L, 1, "source-bucket", "target-bucket", "next-upload-id", "source", "target");
            expectFailedFuture(nextPart);
            assertNull(f.requests(1).get(0).getTaskId());
            assertNull(executor.submit(() -> f.scope().get()).get(3, TimeUnit.SECONDS));
        } finally {
            f.scope().remove();
            executor.shutdownNow();
        }
    }

    @Test
    public void overwriteLocalPut503CarriesRequestDetailsAndDoesNotResetFailures() throws Exception {
        Fixture f = new Fixture(); f.prepareLocal();
        Path file = Files.createTempFile("put-503", ".bin"); Files.write(file, new byte[]{1, 2, 3});
        ObjectListMessage message = f.localMessage(file, 5048);
        when(f.destination.putObject(anyString(), anyString(), any(InputStream.class), any(ObjectMetadata.class))).thenThrow(slowDown());
        try {
            try { f.migration.migrateLocalToOss(message); fail("503 must fail"); } catch (RuntimeException expected) { }
            RateLimitRequest event = f.requests(1).get(0);
            assertEquals("PUT_OBJECT", event.getOperation()); assertEquals("PUT", event.getMethod());
            assertEquals("LOCAL_TO_OSS", event.getDirection()); assertEquals("target", event.getConnectionAlias());
            assertEquals("target-bucket", event.getBucket()); assertEquals("local.bin", event.getKey());
            assertEquals(Integer.valueOf(5048), event.getTaskId()); assertEquals(Integer.valueOf(1), event.getAttempt());
            assertEquals("real-sdk-request-id", event.getRequestId());
            assertEquals("LOCAL_UPLOAD_RETRY", event.getHandlingStage());
            verify(f.manager, never()).resetConsecutive503Count(); assertNull(f.scope().get());
            verify(f.destination, never()).getObjectMetadata(any(GetObjectMetadataRequest.class));
        } finally { Files.deleteIfExists(file); }
    }

    @Test
    public void localDownload503CarriesSourceScopeAndRestoresCaller() {
        Fixture f = new Fixture(); f.prepareLocal();
        RateLimitRequest previous = new RateLimitRequest(); previous.setTaskId(7); f.scope().set(previous);
        ObjectListMessage message = f.downloadMessage(6048);
        when(f.source.getObject("source-bucket", "sample.bin")).thenThrow(slowDown());
        try {
            try { f.migration.migrateOssToLocal(message); fail("503 must fail"); } catch (RuntimeException expected) { }
            RateLimitRequest event = f.requests(1).get(0);
            assertEquals("GET_OBJECT", event.getOperation()); assertEquals("GET", event.getMethod());
            assertEquals("OSS_TO_LOCAL", event.getDirection()); assertEquals("source", event.getConnectionAlias());
            assertEquals("source-bucket", event.getBucket()); assertEquals("sample.bin", event.getKey());
            assertEquals(Integer.valueOf(6048), event.getTaskId()); assertEquals("real-sdk-request-id", event.getRequestId());
            assertEquals("LOCAL_DOWNLOAD_RETRY", event.getHandlingStage());
            verify(f.manager, never()).resetConsecutive503Count(); assertSame(previous, f.scope().get());
        } finally { f.scope().remove(); }
    }

    @Test
    public void localMultipart503CarriesPartNumber() throws Exception {
        Fixture f = new Fixture(); f.prepareLocal();
        DestBucketConfig config = (DestBucketConfig) ReflectionTestUtils.getField(f.migration, "destBucketConfig");
        config.getMultipart().setThreshold(1L);
        Path file = Files.createTempFile("part-503", ".bin"); Files.write(file, new byte[]{1, 2, 3});
        InitiateMultipartUploadResult initiated = new InitiateMultipartUploadResult(); initiated.setUploadId("qa-upload");
        when(f.destination.initiateMultipartUpload(any(InitiateMultipartUploadRequest.class))).thenReturn(initiated);
        when(f.destination.uploadPart(any(UploadPartRequest.class))).thenThrow(slowDown());
        try {
            ObjectListMessage message = f.localMessage(file, -7048); message.setParentTaskId(7048);
            try { f.migration.migrateLocalToOss(message); fail("503 must fail"); } catch (RuntimeException expected) { }
            RateLimitRequest event = f.requests(1).get(0);
            assertEquals("UPLOAD_PART", event.getOperation()); assertEquals("LOCAL_TO_OSS", event.getDirection());
            assertEquals(Integer.valueOf(1), event.getPartNumber()); assertEquals(Integer.valueOf(7048), event.getTaskId());
            assertEquals("local.bin", event.getKey()); assertEquals("real-sdk-request-id", event.getRequestId());
            verify(f.manager, never()).resetConsecutive503Count();
        } finally { Files.deleteIfExists(file); }
    }

    @Test
    public void successfulLocalPutRetryAndDownloadResetConsecutiveCount() throws Exception {
        Fixture f = new Fixture(); f.prepareLocal();
        f.manager.getAutoAdjustConfig().setMaxRetryOn503(1); f.manager.getAutoAdjustConfig().setRetryDelayMs(0L);
        Path file = Files.createTempFile("put-recovery", ".bin"); Files.write(file, new byte[]{1, 2, 3});
        when(f.destination.putObject(anyString(), anyString(), any(InputStream.class), any(ObjectMetadata.class)))
                .thenThrow(slowDown()).thenReturn(new PutObjectResult());
        try {
            assertTrue(f.migration.migrateLocalToOss(f.localMessage(file, 8048)));
            assertEquals(1, f.requests(1).size()); verify(f.manager, times(1)).resetConsecutive503Count();
            clearInvocations(f.manager);
            LocalFileService files = mock(LocalFileService.class);
            when(files.writeAtomically(any(InputStream.class), anyString(), anyString(), anyInt(), any(), anyLong())).thenReturn(true);
            ReflectionTestUtils.setField(f.migration, "localFileService", files);
            S3Object object = new S3Object(); object.setObjectContent(new ByteArrayInputStream(new byte[]{1, 2, 3}));
            when(f.source.getObject("source-bucket", "sample.bin")).thenThrow(slowDown()).thenReturn(object);
            assertTrue(f.migration.migrateOssToLocal(f.downloadMessage(9048)));
            assertEquals(1, f.requests(1).size()); verify(f.manager, times(1)).resetConsecutive503Count();
        } finally { Files.deleteIfExists(file); }
    }

    @Test
    public void localParentIdsAppearInPutAndDownloadEventsWithoutLeakingExecutionIds() throws Exception {
        Fixture f = new Fixture(); f.prepareLocal();
        Path file = Files.createTempFile("parent-rate-request", ".bin"); Files.write(file, new byte[]{1});
        ObjectListMessage upload = f.localMessage(file, -1224295820); upload.setParentTaskId(13);
        when(f.destination.putObject(anyString(), anyString(), any(InputStream.class), any(ObjectMetadata.class))).thenThrow(slowDown());
        try {
            try { f.migration.migrateLocalToOss(upload); fail("503 must fail"); } catch (RuntimeException expected) { }
            RateLimitRequest put = f.requests(1).get(0);
            assertEquals(Integer.valueOf(13), put.getTaskId()); assertEquals("LOCAL_TO_OSS", put.getDirection());
            assertEquals(Integer.valueOf(13), put.copy().getTaskId());
            assertEquals(RateLimitRequest.class, put.getClass()); assertNull(f.scope().get());
            clearInvocations(f.manager);
            ObjectListMessage download = f.downloadMessage(-1224295819); download.setParentTaskId(14);
            when(f.source.getObject("source-bucket", "sample.bin")).thenThrow(slowDown());
            try { f.migration.migrateOssToLocal(download); fail("503 must fail"); } catch (RuntimeException expected) { }
            RateLimitRequest get = f.requests(1).get(0);
            assertEquals(Integer.valueOf(14), get.getTaskId()); assertEquals("OSS_TO_LOCAL", get.getDirection());
            assertEquals(RateLimitRequest.class, get.getClass()); assertNull(f.scope().get());
        } finally { Files.deleteIfExists(file); }
    }

    @Test
    public void scopedChecksAndCancellationStillTargetTheExecutingShard() throws Exception {
        Fixture f = new Fixture(); f.prepareLocal();
        LocalTaskControlService control = (LocalTaskControlService) ReflectionTestUtils.getField(f.migration, "localTaskControlService");
        Path file = Files.createTempFile("parent-rate-cancel", ".bin"); Files.write(file, new byte[]{1});
        ObjectListMessage message = f.localMessage(file, -1224295820); message.setParentTaskId(13);
        when(f.destination.putObject(anyString(), anyString(), any(InputStream.class), any(ObjectMetadata.class))).thenAnswer(call -> {
            RateLimitRequest scope = f.scope().get(); assertEquals(Integer.valueOf(13), scope.getTaskId());
            java.util.concurrent.atomic.AtomicBoolean closed = new java.util.concurrent.atomic.AtomicBoolean();
            InputStream raw = new ByteArrayInputStream(new byte[]{1}) { @Override public void close() { closed.set(true); } };
            InputStream scoped = ReflectionTestUtils.invokeMethod(f.migration, "cancellableForScope", raw);
            control.stop(-1224295820);
            assertTrue("stream must register against the shard", closed.get());
            assertFalse("parent control must remain distinct", control.isStopped(13));
            try { ReflectionTestUtils.invokeMethod(f.migration, "checkScopedRunning"); fail("scope must observe stopped shard"); }
            catch (TaskCancelledException expected) { }
            scoped.close();
            throw new TaskCancelledException("stopped test shard");
        });
        try {
            try { f.migration.migrateLocalToOss(message); fail("stopped upload must fail"); }
            catch (TaskCancelledException expected) { }
            assertNull(f.scope().get()); verify(f.manager, never()).onOss503SlowDown(any(RateLimitRequest.class));
        } finally { Files.deleteIfExists(file); }
    }

    private static void expectFailedFuture(Future<?> future) throws Exception {
        try {
            future.get(3, TimeUnit.SECONDS);
            fail("Expected failed multipart request");
        } catch (java.util.concurrent.ExecutionException expected) { }
    }

    private static AmazonS3Exception slowDown() {
        AmazonS3Exception error = new AmazonS3Exception("Service temporarily unavailable");
        error.setStatusCode(503);
        error.setErrorCode("ServiceUnavailable");
        error.setRequestId("real-sdk-request-id");
        error.setExtendedRequestId("extended-sdk-request-id");
        return error;
    }

    private static class Fixture {
        final RateLimiterManager manager = mock(RateLimiterManager.class);
        final ObjectMigrationService migration = new ObjectMigrationService();
        final AmazonS3 source = mock(AmazonS3.class);
        final AmazonS3 destination = mock(AmazonS3.class);

        Fixture() {
            RateLimitConfig.AutoAdjustConfig auto = new RateLimitConfig.AutoAdjustConfig();
            auto.setMaxRetryOn503(0);
            when(manager.getAutoAdjustConfig()).thenReturn(auto);
            when(manager.onOss503SlowDown(any(RateLimitRequest.class))).thenReturn(20);
            S3ClientManager clients = mock(S3ClientManager.class);
            when(clients.getClient("source")).thenReturn(source);
            when(clients.getClient("target")).thenReturn(destination);
            DestBucketConfig destinationConfig = new DestBucketConfig();
            DestBucketConfig.MultipartConfig multipart = new DestBucketConfig.MultipartConfig();
            multipart.setThreshold(100L);
            destinationConfig.setMultipart(multipart);
            ReflectionTestUtils.setField(migration, "rateLimiterManager", manager);
            ReflectionTestUtils.setField(migration, "s3ClientManager", clients);
            ReflectionTestUtils.setField(migration, "destBucketConfig", destinationConfig);
            ReflectionTestUtils.setField(migration, "transferConfig", new TransferConfig());
        }

        void prepareLocal() {
            ReflectionTestUtils.setField(migration, "localTaskControlService", new LocalTaskControlService());
            ReflectionTestUtils.setField(migration, "localTaskRateLimiterService", mock(LocalTaskRateLimiterService.class));
        }

        ObjectListMessage localMessage(Path file, int taskId) {
            ObjectListMessage message = new ObjectListMessage(); message.setTaskIndex(taskId); message.setDirection(TransferDirection.LOCAL_TO_OSS);
            message.setLocalPath(file.toString()); message.setDestBucket("target-bucket"); message.setDestS3Alias("target");
            message.setKey("local.bin"); message.setOverwrite(true); return message;
        }

        ObjectListMessage downloadMessage(int taskId) {
            S3ObjectSummary summary = new S3ObjectSummary(); summary.setKey("sample.bin"); summary.setSize(3);
            ObjectListMessage message = new ObjectListMessage(summary, taskId, false, "source-bucket", null, "source", null, true, null);
            message.setDirection(TransferDirection.OSS_TO_LOCAL); message.setLocalPath("/tmp/qa-download"); return message;
        }

        void migrateAndExpectFailure(int taskId) {
            S3ObjectSummary summary = new S3ObjectSummary();
            summary.setKey("sample.bin");
            summary.setSize(3L);
            try {
                migration.migrateObject(summary, "source-bucket", "target-bucket", true, taskId, "source", "target");
                fail("Expected failed migration");
            } catch (RuntimeException expected) { }
        }

        List<RateLimitRequest> requests(int count) {
            ArgumentCaptor<RateLimitRequest> captor = ArgumentCaptor.forClass(RateLimitRequest.class);
            verify(manager, times(count)).onOss503SlowDown(captor.capture());
            return captor.getAllValues();
        }

        @SuppressWarnings("unchecked")
        ThreadLocal<RateLimitRequest> scope() {
            return (ThreadLocal<RateLimitRequest>) ReflectionTestUtils.getField(migration, "rateRequestScope");
        }
    }
}
