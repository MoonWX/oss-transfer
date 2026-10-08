package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.AmazonS3Exception;
import com.amazonaws.services.s3.model.*;
import com.transfer.tools.config.DestBucketConfig;
import com.transfer.tools.config.SourceBucketConfig;
import com.transfer.tools.config.TransferConfig;
import com.transfer.tools.model.RateLimitRequest;
import com.transfer.tools.model.SubTaskStatus;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.model.ConflictPolicy;
import com.transfer.tools.util.BandwidthThrottlingInputStream;
import com.transfer.tools.util.LocalTaskThrottlingInputStream;
import com.transfer.tools.util.TaskBandwidthInputStream;
import com.transfer.tools.util.OssKeyMapper;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.beans.factory.annotation.Qualifier;
import org.springframework.beans.factory.annotation.Value;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import javax.annotation.PreDestroy;
import java.io.*;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.channels.FileChannel;
import java.nio.file.StandardOpenOption;
import java.nio.file.LinkOption;
import java.nio.ByteBuffer;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.Date;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Future;
import java.util.concurrent.CompletionService;
import java.util.concurrent.ExecutorCompletionService;
import java.util.concurrent.RejectedExecutionException;
import java.util.concurrent.ThreadPoolExecutor;
import java.util.concurrent.TimeUnit;

@Slf4j
@Service
public class ObjectMigrationService {
    private final ThreadLocal<RateLimitRequest> rateRequestScope = new ThreadLocal<>();

    /** User-facing request evidence identifies the parent; cancellation targets the executing shard. */
    private static class RateRequestContext extends RateLimitRequest {
        private final Integer executionTaskId;
        private final ObjectListMessage bandwidthMessage;
        RateRequestContext(ObjectListMessage message) {
            bandwidthMessage = message;
            executionTaskId = message.getTaskIndex();
            setTaskId(message.getParentTaskId() == null ? executionTaskId : message.getParentTaskId());
        }
    }

    private Integer executionTaskId(RateLimitRequest scope) {
        return scope instanceof RateRequestContext ? ((RateRequestContext) scope).executionTaskId
                : scope == null ? null : scope.getTaskId();
    }
    private final Map<Throwable, RateLimitRequest> rateRequestEvidence =
            java.util.Collections.synchronizedMap(new java.util.WeakHashMap<>());



    private ExecutorService partUploadExecutor;

    @Value("${thread-count.dest.part:10}")
    private int partThreadCount;

    @Autowired
    private DestBucketConfig destBucketConfig;

    @Autowired
    private RateLimiterManager rateLimiterManager;

    @Autowired
    private TransferConfig transferConfig;

    @Autowired
    private SubTaskManagerService subTaskManagerService;

    @Autowired
    private S3ClientManager s3ClientManager;

    @Autowired
    private LocalFileService localFileService;

    @Autowired
    private LocalTemporaryFileRegistry temporaryFileRegistry;

    @Autowired
    private LocalTaskRateLimiterService localTaskRateLimiterService;

    @Autowired
    private WorkerTaskBandwidthService taskBandwidth;

    @Autowired
    private TransferSpeedService transferSpeedService;

    @Autowired
    private LocalTaskControlService localTaskControlService;

    @PostConstruct
    public void init() {
        int queueSize = Math.max(partThreadCount * 2, 1);
        BlockingQueue<Runnable> workQueue = new ArrayBlockingQueue<>(queueSize);
        partUploadExecutor = new ThreadPoolExecutor(
            partThreadCount,
            partThreadCount,
            0L,
            TimeUnit.MILLISECONDS,
            workQueue,
            (task, executor) -> {
                if (executor.isShutdown()) {
                    throw new RejectedExecutionException("分片上传线程池已关闭");
                }
                try {
                    executor.getQueue().put(task);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    throw new RejectedExecutionException("等待分片上传队列时被中断", e);
                }
            }
        );
        log.info("分片上传线程池已初始化，线程数: {}, 队列大小: {}", partThreadCount, queueSize);

        if (transferConfig.isDryRun()) {
            try {
                String outputFile = transferConfig.getDryRunOutputFile();
                java.io.FileWriter writer = new java.io.FileWriter(outputFile, true);
                writer.close();
                log.info("Dry-run模式已启用，输出文件: {}", outputFile);
            } catch (Exception e) {
                log.error("初始化dry-run输出文件失败", e);
            }
        }
    }

    private void logDryRunObject(String key, String sourceBucket, String destBucket) {
        try {
            String outputFile = transferConfig.getDryRunOutputFile();
            java.io.FileWriter writer = new java.io.FileWriter(outputFile, true);
            writer.write(sourceBucket + "/" + key + "\n");
            writer.close();
            log.debug("Dry-run: {}", key);
        } catch (Exception e) {
            log.error("写入dry-run日志失败: {}", key, e);
        }
    }

    @PreDestroy
    public void destroy() {
        if (partUploadExecutor != null) {
            partUploadExecutor.shutdown();
            log.info("分片上传线程池已关闭");
        }
    }

    public boolean migrateObject(S3ObjectSummary objectSummary, String sourceBucket, String destBucket,
            Boolean overwrite, Integer taskIndex, String srcS3Alias, String dstS3Alias) {
        RateLimitRequest previous = rateRequestScope.get();
        ObjectListMessage message = new ObjectListMessage(); message.setTaskIndex(taskIndex);
        RateLimitRequest scope = new RateRequestContext(message);
        scope.setTaskId(taskIndex); scope.setDirection("OSS_TO_OSS");
        scope.setSourceBucket(sourceBucket); scope.setDestinationBucket(destBucket);
        rateRequestScope.set(scope);
        try { return migrateObjectWithContext(objectSummary, sourceBucket, destBucket, overwrite, taskIndex, srcS3Alias, dstS3Alias); }
        finally { restoreRateRequestScope(previous); }
    }

    private boolean migrateObjectWithContext(S3ObjectSummary objectSummary, String sourceBucket, String destBucket,
            Boolean overwrite, Integer taskIndex, String srcS3Alias, String dstS3Alias) {
        String key = objectSummary.getKey();
        String targetKey = destinationKey(key);
        long objectSize = objectSummary.getSize();
        Date srcLastModified = objectSummary.getLastModified();

        try {
            if (!shouldOverwrite(overwrite)) {
                if (objectExistsInDest(targetKey, srcLastModified, destBucket, dstS3Alias)) {
                    log.debug("对象已存在，跳过: {}", key);
                    return false;
                }
            }

            if (transferConfig.isDryRun()) {
                logDryRunObject(key, sourceBucket, destBucket);
                return true;
            }

            Exception firstException = null;

            if (transferConfig.isDiskMode()) {
                migrateWithFallback(key, "disk mode", sourceBucket, destBucket, taskIndex, srcS3Alias, dstS3Alias);
                log.info("硬盘迁移成功: {}", key);
                return true;
            }

            if (transferConfig.isUrlMode()) {
                migrateWithPresignedUrlRetry(key, objectSize, sourceBucket, destBucket, srcS3Alias, dstS3Alias);
                rateLimiterManager.resetConsecutive503Count();
                log.info("预签名链接迁移成功: {}", key);
                return true;
            }

            try {
                if (objectSize <= destBucketConfig.getMultipart().getThreshold()) {
                    migrateWithPutRetry(key, sourceBucket, destBucket, srcS3Alias, dstS3Alias);
                } else {
                    migrateWithMultipart(key, objectSize, sourceBucket, destBucket, srcS3Alias, dstS3Alias);
                }
                rateLimiterManager.resetConsecutive503Count();
                log.debug("迁移成功: {}", key);
                return true;
            } catch (Exception e) {
                checkScopedRunning();
                firstException = e;

                if (isConnectionPoolTimeout(e)) {
                    log.warn("检测到连接池超时，重建S3客户端: srcAlias={}, dstAlias={}", srcS3Alias, dstS3Alias);
                    if (srcS3Alias != null) {
                        s3ClientManager.recreateClient(srcS3Alias);
                    }
                }

                if (is503SlowDown(e)) {
                    handle503SlowDown(e, "TRANSFER_OBJECT", null, null, null, key, null, null, "OUTER_RETHROW");
                    throw e;
                }
                log.warn("常规迁移失败，尝试硬盘迁移，sourceBucket：{}，key: {}", sourceBucket, key, e);
            }

            long fallbackThreshold = 10L * 1024 * 1024 * 1024;
            if (objectSize > fallbackThreshold) {
                log.error("对象大小超过10G，不支持硬盘迁移模式，迁移失败，sourceBucket：{}，key: {}, 大小: {} bytes", sourceBucket, key, objectSize);
                throw new RuntimeException("对象大小超过10G，不支持硬盘迁移模式: " + key + ", 大小: " + objectSize + " bytes");
            }

            try {
                String errorMsg = firstException.getMessage();
                migrateWithFallback(key, errorMsg, sourceBucket, destBucket, taskIndex, srcS3Alias, dstS3Alias);
                log.info("硬盘迁移成功: {}", key);
                return true;
            } catch (Exception e) {
                log.error("所有迁移方式均失败，sourceBucket：{}，key: {}", sourceBucket, key, e);
                throw new RuntimeException("所有迁移方式均失败: " + key, e);
            }
        } catch (Exception e) {
            throw new RuntimeException("迁移失败: " + key, e);
        }
    }

    public boolean migrate(ObjectListMessage message) {
        try (LocalTaskControlService.Operation ignored = localTaskControlService.begin(message.getTaskIndex())) {
            return migrateWhileRunning(message);
        } catch (RuntimeException error) {
            if (localTaskControlService.isStopped(message.getTaskIndex())) {
                throw new TaskCancelledException("task " + message.getTaskIndex() + " is stopped");
            }
            throw error;
        }
    }

    private boolean migrateWhileRunning(ObjectListMessage message) {
        if (taskBandwidth != null) taskBandwidth.check(taskBandwidth.scope(message));
        TransferDirection direction = message.getDirection() == null
                ? TransferDirection.OSS_TO_OSS : message.getDirection();
        if (direction == TransferDirection.OSS_TO_LOCAL) {
            return migrateOssToLocal(message);
        }
        if (direction == TransferDirection.LOCAL_TO_OSS) {
            return migrateLocalToOss(message);
        }
        RateLimitRequest previous = rateRequestScope.get();
        RateRequestContext scope = new RateRequestContext(message); scope.setDirection("OSS_TO_OSS");
        scope.setSourceBucket(message.getSourceBucket()); scope.setDestinationBucket(message.getDestBucket());
        rateRequestScope.set(scope);
        try {
            boolean migrated = migrateObjectWithContext(message.getObjectSummary(), message.getSourceBucket(), message.getDestBucket(),
                    message.getOverwrite(), message.getTaskIndex(), message.getSourceS3Alias(), message.getDestS3Alias());
            if (migrated && transferConfig.isDryRun()) message.setReportReasonCode("DRY_RUN");
            return migrated;
        } finally { restoreRateRequestScope(previous); }
    }

    public boolean migrateOssToLocal(ObjectListMessage message) {
        RateLimitRequest previous = rateRequestScope.get();
        RateLimitRequest scope = new RateRequestContext(message);
        scope.setDirection("OSS_TO_LOCAL");
        scope.setSourceBucket(message.getSourceBucket());
        rateRequestScope.set(scope);
        try { return migrateOssToLocalWithContext(message); }
        finally { restoreRateRequestScope(previous); }
    }

    private boolean migrateOssToLocalWithContext(ObjectListMessage message) {
        localTaskControlService.checkRunning(message.getTaskIndex());
        S3ObjectSummary summary = message.getObjectSummary();
        if (summary == null) {
            throw new IllegalArgumentException("OSS source metadata is required");
        }
        String key = summary.getKey();
        String relative = message.getTargetRelativePath() != null && !message.getTargetRelativePath().isEmpty()
                ? message.getTargetRelativePath() : key;
        String root = message.getLocalPath() != null
                ? message.getLocalPath() : message.getDestinationEndpoint().getLocalPath();
        if (key.endsWith("/")) {
            try {
                localFileService.createDirectoryMarker(root, relative);
                return true;
            } catch (IOException e) {
                throw new RuntimeException("创建本地目录失败: " + key, e);
            }
        }
        AmazonS3 source = s3ClientManager.getClient(message.getSourceS3Alias());
        if (source == null) {
            throw new RuntimeException("源S3客户端不存在: " + message.getSourceS3Alias());
        }
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();
        Exception last = null;
        for (int attempt = 0; attempt <= maxRetry; attempt++) {
            S3Object object = null;
            try {
                localTaskControlService.checkRunning(message.getTaskIndex());
                localTaskRateLimiterService.acquireRead(message);
                object = source.getObject(message.getSourceBucket(), key);
                try (InputStream raw = object.getObjectContent();
                     InputStream cancellable = new CancellableInputStream(raw, message.getTaskIndex(), localTaskControlService);
                     InputStream globallyThrottled = new BandwidthThrottlingInputStream(cancellable, rateLimiterManager);
                     InputStream throttled = new LocalTaskThrottlingInputStream(globallyThrottled, localTaskRateLimiterService, message, taskBandwidth);
                     InputStream taskStream = new TaskBandwidthInputStream(throttled, taskBandwidth,
                             taskBandwidth == null ? null : taskBandwidth.scope(message), false,
                             transferSpeedService, message.getTaskIndex())) {
                    boolean migrated = localFileService.writeAtomically(taskStream, root, relative, message.getTaskIndex(),
                            policy(message), summary.getSize());
                    rateLimiterManager.resetConsecutive503Count();
                    return migrated;
                }
            } catch (TransferValidationException | TaskCancelledException e) {
                throw e;
            } catch (Exception e) {
                localTaskControlService.checkRunning(message.getTaskIndex());
                last = e;
                if (is503SlowDown(e)) {
                    handle503SlowDown(e, "GET_OBJECT", "GET", message.getSourceS3Alias(),
                            message.getSourceBucket(), key, null, attempt + 1, "LOCAL_DOWNLOAD_RETRY");
                }
                if (attempt < maxRetry) {
                    sleepBeforeRetry(attempt);
                }
            } finally {
                if (object != null) {
                    try {
                        object.close();
                    } catch (IOException ignored) {
                    }
                }
            }
        }
        throw new RuntimeException("OSS下载到本地失败: " + key, last);
    }

    public boolean migrateLocalToOss(ObjectListMessage message) {
        RateLimitRequest previous = rateRequestScope.get();
        RateLimitRequest scope = new RateRequestContext(message);
        scope.setDirection("LOCAL_TO_OSS");
        scope.setDestinationBucket(message.getDestBucket());
        rateRequestScope.set(scope);
        try { return migrateLocalToOssWithContext(message); }
        finally { restoreRateRequestScope(previous); }
    }

    private boolean migrateLocalToOssWithContext(ObjectListMessage message) {
        localTaskControlService.checkRunning(message.getTaskIndex());
        Path file = Paths.get(message.getLocalPath());
        String key = message.getKey();
        long size;
        try {
            size = Files.size(file);
        } catch (IOException e) {
            throw new RuntimeException("读取本地文件元数据失败: " + file, e);
        }
        if (!shouldOverwrite(message.getOverwrite())) {
            localTaskRateLimiterService.acquireHead(message);
            Date modified = message.getSourceLastModified() == null
                    ? new Date(0) : new Date(message.getSourceLastModified());
            if (objectExistsInDest(key, modified, message.getDestBucket(), message.getDestS3Alias())) {
                return false;
            }
        }
        if (size <= destBucketConfig.getMultipart().getThreshold()) {
            uploadLocalWithRetry(message, file, size);
        } else {
            uploadLocalMultipart(message, file, size);
        }
        rateLimiterManager.resetConsecutive503Count();
        return true;
    }

    private void uploadLocalWithRetry(ObjectListMessage message, Path file, long size) {
        Exception last = null;
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();
        for (int attempt = 0; attempt <= maxRetry; attempt++) {
            try {
                localTaskControlService.checkRunning(message.getTaskIndex());
                AmazonS3 destination = requireDestination(message.getDestS3Alias());
                ObjectMetadata metadata = new ObjectMetadata();
                metadata.setContentLength(size);
                localTaskRateLimiterService.acquirePut(message);
                rateLimiterManager.acquirePut();
                try (InputStream raw = new SeekableFileInputStream(file, 0, size);
                     InputStream cancellable = new CancellableInputStream(raw, message.getTaskIndex(), localTaskControlService);
                     InputStream globallyThrottled = new BandwidthThrottlingInputStream(cancellable, rateLimiterManager);
                     InputStream throttled = new LocalTaskThrottlingInputStream(globallyThrottled, localTaskRateLimiterService, message, taskBandwidth)) {
                    putWithTaskBandwidth(destination, message.getDestBucket(), message.getKey(), throttled, metadata, message);
                }
                return;
            } catch (TransferValidationException | TaskCancelledException e) {
                throw e;
            } catch (Exception e) {
                localTaskControlService.checkRunning(message.getTaskIndex());
                last = e;
                if (is503SlowDown(e)) {
                    handle503SlowDown(e, "PUT_OBJECT", "PUT", message.getDestS3Alias(),
                            message.getDestBucket(), message.getKey(), null, attempt + 1, "LOCAL_UPLOAD_RETRY");
                }
                if (attempt < maxRetry) {
                    sleepBeforeRetry(attempt);
                }
            }
        }
        throw new RuntimeException("本地文件上传失败: " + message.getKey(), last);
    }

    private void uploadLocalMultipart(ObjectListMessage message, Path file, long size) {
        AmazonS3 destination = requireDestination(message.getDestS3Alias());
        ObjectMetadata metadata = new ObjectMetadata();
        localTaskRateLimiterService.acquirePut(message);
        InitiateMultipartUploadRequest initiateRequest = new InitiateMultipartUploadRequest(
                message.getDestBucket(), message.getKey()).withObjectMetadata(metadata);
        String uploadId = initiateMultipartUploadWithRetry(message.getDestS3Alias(), initiateRequest, message.getKey());
        List<PartETag> parts = new ArrayList<>();
        long partSize = effectivePartSize(size);
        int totalParts = calculateTotalParts(size, partSize);
        log.info("开始本地大文件multipart上传: key={}, size={}, partSize={}, totalParts={}",
                message.getKey(), size, partSize, totalParts);
        try {
            int partNumber = 1;
            for (long offset = 0; offset < size; offset += partSize, partNumber++) {
                localTaskControlService.checkRunning(message.getTaskIndex());
                long length = Math.min(partSize, size - offset);
                parts.add(uploadLocalPartWithRetry(destination, message, file, uploadId,
                        partNumber, offset, length));
            }
            localTaskRateLimiterService.acquirePut(message);
            completeMultipartUploadWithRetry(message.getDestS3Alias(), new CompleteMultipartUploadRequest(
                    message.getDestBucket(), message.getKey(), uploadId, parts), message.getKey(), parts.size());
        } catch (Exception e) {
            try {
                destination.abortMultipartUpload(new AbortMultipartUploadRequest(
                        message.getDestBucket(), message.getKey(), uploadId));
            } catch (Exception abortError) {
                log.warn("中止本地文件multipart失败: {}", message.getKey(), abortError);
            }
            throw e instanceof RuntimeException ? (RuntimeException) e
                    : new RuntimeException("本地multipart上传失败: " + message.getKey(), e);
        }
    }

    private PartETag uploadLocalPartWithRetry(AmazonS3 destination, ObjectListMessage message,
                                              Path file, String uploadId, int partNumber,
                                              long offset, long length) {
        Exception last = null;
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();
        for (int attempt = 0; attempt <= maxRetry; attempt++) {
            try {
                localTaskControlService.checkRunning(message.getTaskIndex());
                localTaskRateLimiterService.acquirePut(message);
                rateLimiterManager.acquirePut();
                try (InputStream ranged = new SeekableFileInputStream(file, offset, length)) {
                    InputStream cancellable = new CancellableInputStream(ranged, message.getTaskIndex(), localTaskControlService);
                    try (InputStream globallyThrottled = new BandwidthThrottlingInputStream(cancellable, rateLimiterManager);
                         InputStream throttled = new LocalTaskThrottlingInputStream(globallyThrottled, localTaskRateLimiterService, message, taskBandwidth)) {
                        UploadPartResult result = uploadPartWithTaskBandwidth(destination, new UploadPartRequest()
                                .withBucketName(message.getDestBucket())
                                .withKey(message.getKey())
                                .withUploadId(uploadId)
                                .withPartNumber(partNumber)
                                .withPartSize(length)
                                .withInputStream(throttled), message);
                        return result.getPartETag();
                    }
                }
            } catch (TaskCancelledException e) {
                throw e;
            } catch (Exception e) {
                localTaskControlService.checkRunning(message.getTaskIndex());
                last = e;
                if (is503SlowDown(e)) {
                    handle503SlowDown(e, "UPLOAD_PART", "PUT", message.getDestS3Alias(),
                            message.getDestBucket(), message.getKey(), partNumber, attempt + 1, "LOCAL_PART_RETRY");
                }
                if (attempt < maxRetry) {
                    sleepBeforeRetry(attempt);
                }
            }
        }
        throw new RuntimeException("本地multipart分片上传失败: " + message.getKey() + ", part=" + partNumber, last);
    }

    private AmazonS3 requireDestination(String alias) {
        AmazonS3 destination = s3ClientManager.getClient(alias);
        if (destination == null) {
            throw new RuntimeException("目标S3客户端不存在: " + alias);
        }
        return destination;
    }

    private ConflictPolicy policy(ObjectListMessage message) {
        if (message.getConflictPolicy() != null) {
            return message.getConflictPolicy();
        }
        return Boolean.TRUE.equals(message.getOverwrite()) ? ConflictPolicy.OVERWRITE : ConflictPolicy.SKIP;
    }

    /** File-backed streams keep SDK signature/retry replay bounded without buffering a whole part in heap. */
    private static class SeekableFileInputStream extends InputStream {
        private final FileChannel channel;
        private final long end;
        private long mark;

        SeekableFileInputStream(Path path, long offset, long length) throws IOException {
            if (offset < 0 || length < 0 || offset > Long.MAX_VALUE - length
                    || Files.isSymbolicLink(path) || !Files.isRegularFile(path, LinkOption.NOFOLLOW_LINKS)) {
                throw new IOException("invalid upload file or range");
            }
            channel = FileChannel.open(path, StandardOpenOption.READ, LinkOption.NOFOLLOW_LINKS);
            try {
                end = offset + length;
                if (end > channel.size()) throw new EOFException("upload source became shorter");
                channel.position(offset);
                mark = offset;
            } catch (IOException | RuntimeException error) {
                channel.close();
                throw error;
            }
        }

        @Override public int read() throws IOException {
            byte[] one = new byte[1];
            return read(one, 0, 1) < 0 ? -1 : one[0] & 255;
        }

        @Override public int read(byte[] bytes, int offset, int length) throws IOException {
            if (offset < 0 || length < 0 || offset > bytes.length - length) throw new IndexOutOfBoundsException();
            if (length == 0) return 0;
            long remaining = end - channel.position();
            if (remaining == 0) return -1;
            int read = channel.read(ByteBuffer.wrap(bytes, offset, (int) Math.min(length, remaining)));
            if (read < 0) throw new EOFException("upload source became shorter");
            return read;
        }

        @Override public synchronized void mark(int readLimit) {
            try { mark = channel.position(); } catch (IOException error) { throw new java.io.UncheckedIOException(error); }
        }
        @Override public synchronized void reset() throws IOException { channel.position(mark); }
        @Override public boolean markSupported() { return true; }
        @Override public void close() throws IOException { channel.close(); }
    }

    private static class TemporaryUploadInputStream extends SeekableFileInputStream {
        private final LocalTemporaryFileRegistry.OwnedFile temporary;
        TemporaryUploadInputStream(LocalTemporaryFileRegistry.OwnedFile temporary, long length) throws IOException {
            super(temporary.getPath(), 0, length);
            this.temporary = temporary;
        }
        @Override public void close() throws IOException {
            try { super.close(); } finally { temporary.close(); }
        }
    }

    /** The SDK hashes HTTP payloads before sending them; network streams cannot seek back afterwards. */
    private InputStream replayableUploadStream(InputStream source, long length) throws IOException {
        if (length < 0) throw new IOException("upload source size is unknown");
        final int memoryLimit = 64 * 1024;
        byte[] buffer = new byte[memoryLimit];
        LocalTemporaryFileRegistry.OwnedFile temporary = null;
        try {
            ByteArrayOutputStream memory = length <= memoryLimit ? new ByteArrayOutputStream((int) length) : null;
            if (memory == null) {
                String configured = transferConfig.getTmpDir();
                Path directory = configured == null || configured.trim().isEmpty()
                        ? Paths.get(System.getProperty("java.io.tmpdir")) : Paths.get(configured);
                Files.createDirectories(directory);
                RateLimitRequest scope = rateRequestScope.get();
                temporary = temporaryFileRegistry.create(directory, null,
                        scope == null ? "upload-replay" : scope.getKey(), executionTaskId(scope));
            }
            try (OutputStream output = memory != null ? memory : temporary.getOutputStream()) {
                long remaining = length;
                while (remaining > 0) {
                    checkScopedRunning();
                    int read = source.read(buffer, 0, (int) Math.min(buffer.length, remaining));
                    if (read < 0) throw new EOFException("upload source became shorter");
                    if (read == 0) continue;
                    output.write(buffer, 0, read);
                    remaining -= read;
                }
                checkScopedRunning();
                if (source.read() != -1) throw new IOException("upload source exceeds expected size");
            }
            if (temporary != null) temporary.verifyIdentity();
            InputStream replay = memory != null ? new ByteArrayInputStream(memory.toByteArray())
                    : new TemporaryUploadInputStream(temporary, length);
            try { return cancellableForScope(replay); }
            catch (RuntimeException error) { replay.close(); throw error; }
        } catch (IOException | RuntimeException error) {
            if (temporary != null) {
                try { temporary.close(); }
                catch (IOException cleanupError) { error.addSuppressed(cleanupError); }
            }
            throw error;
        }
    }

    private static class CancellableInputStream extends FilterInputStream {
        private final Integer taskId;
        private final LocalTaskControlService control;
        private final AutoCloseable cancellation;

        CancellableInputStream(InputStream input, Integer taskId, LocalTaskControlService control) {
            super(input);
            this.taskId = taskId;
            this.control = control;
            this.cancellation = control.onCancel(taskId, () -> {
                try {
                    if (input instanceof com.amazonaws.services.s3.model.S3ObjectInputStream) {
                        ((com.amazonaws.services.s3.model.S3ObjectInputStream) input).abort();
                    } else input.close();
                } catch (IOException ignored) { }
            });
        }

        @Override public void close() throws IOException {
            try { cancellation.close(); } catch (Exception ignored) { }
            super.close();
        }

        @Override
        public int read() throws IOException {
            control.checkRunning(taskId);
            return super.read();
        }

        @Override
        public int read(byte[] bytes, int offset, int length) throws IOException {
            control.checkRunning(taskId);
            return super.read(bytes, offset, length);
        }
    }

    private boolean is503SlowDown(Exception e) {
        if (e instanceof AmazonS3Exception) {
            AmazonS3Exception s3Exception = (AmazonS3Exception) e;
            return s3Exception.getStatusCode() == 503;
        }
        Throwable cause = e.getCause();
        while (cause != null) {
            if (cause instanceof AmazonS3Exception) {
                AmazonS3Exception s3Exception = (AmazonS3Exception) cause;
                return s3Exception.getStatusCode() == 503;
            }
            cause = cause.getCause();
        }
        return false;
    }

    private boolean isConnectionPoolTimeout(Exception e) {
        String message = e.getMessage();
        if (message != null && message.contains("Timeout waiting for connection from pool")) {
            return true;
        }
        Throwable cause = e.getCause();
        while (cause != null) {
            String causeMessage = cause.getMessage();
            if (causeMessage != null && causeMessage.contains("Timeout waiting for connection from pool")) {
                return true;
            }
            cause = cause.getCause();
        }
        return false;
    }

    private boolean is404NoSuchUpload(Exception e) {
        if (e instanceof AmazonS3Exception) {
            AmazonS3Exception s3Exception = (AmazonS3Exception) e;
            if (s3Exception.getStatusCode() == 404) {
                String errorCode = s3Exception.getErrorCode();
                return "NoSuchUpload".equals(errorCode) || "404".equals(errorCode);
            }
        }
        Throwable cause = e.getCause();
        while (cause != null) {
            if (cause instanceof AmazonS3Exception) {
                AmazonS3Exception s3Exception = (AmazonS3Exception) cause;
                if (s3Exception.getStatusCode() == 404) {
                    String errorCode = s3Exception.getErrorCode();
                    return "NoSuchUpload".equals(errorCode) || "404".equals(errorCode);
                }
            }
            cause = cause.getCause();
        }
        return false;
    }

    private void restoreRateRequestScope(RateLimitRequest previous) {
        if (previous == null) rateRequestScope.remove(); else rateRequestScope.set(previous);
    }

    private RateLimitRequest requestEvidence(Exception error, String operation, String method, String alias,
                                             String bucket, String key, Integer part, Integer attempt, String stage) {
        for (Throwable cause = error; cause != null; cause = cause.getCause()) {
            RateLimitRequest recorded = rateRequestEvidence.get(cause);
            if (recorded != null) {
                RateLimitRequest copy = recorded.copy();
                copy.setHandlingStage(stage);
                if (copy.getAttempt() == null) copy.setAttempt(attempt);
                return copy;
            }
        }
        return RateLimitRequest.capture(error, operation, method, alias, bucket, key, part, attempt,
                stage, rateRequestScope.get());
    }

    private void rememberRequest(Exception error, String operation, String method, String alias,
                                 String bucket, String key) {
        if (is503SlowDown(error)) rateRequestEvidence.put(error, requestEvidence(error, operation,
                method, alias, bucket, key, null, null, "SDK_REQUEST"));
    }

    private void handle503SlowDown(Exception error, String operation, String method, String alias,
                                   String bucket, String key, Integer part, Integer attempt, String stage) {
        RateLimitRequest evidence = requestEvidence(error, operation, method, alias, bucket, key, part, attempt, stage);
        rateRequestEvidence.put(error, evidence);
        int count = rateLimiterManager.onOss503SlowDown(evidence);
        log.warn("检测到503限速错误，连续次数: {}", count);
    }

    private void checkScopedRunning() {
        RateLimitRequest scope = rateRequestScope.get();
        Integer executionId = executionTaskId(scope);
        if (executionId != null && localTaskControlService != null) {
            localTaskControlService.checkRunning(executionId);
        } else if (Thread.currentThread().isInterrupted()) {
            throw new TaskCancelledException("transfer interrupted");
        }
    }

    private InputStream cancellableForScope(InputStream input) {
        RateLimitRequest scope = rateRequestScope.get();
        Integer executionId = executionTaskId(scope);
        return executionId != null && localTaskControlService != null
                ? new CancellableInputStream(input, executionId, localTaskControlService) : input;
    }

    private void sleepBeforeRetry(int retryCount) {
        checkScopedRunning();
        try {
            long retryDelayMs = rateLimiterManager.getAutoAdjustConfig().getRetryDelayMs();
            long delay = retryDelayMs * (1L << retryCount);
            log.info("等待 {} ms 后重试", delay);
            Thread.sleep(delay);
        } catch (InterruptedException ie) {
            Thread.currentThread().interrupt();
            throw new TaskCancelledException("transfer interrupted");
        }
    }

    private void migrateWithPutRetry(String key, String sourceBucket, String destBucket, String srcS3Alias, String dstS3Alias) {
        Exception lastException = null;
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();
        for (int i = 0; i <= maxRetry; i++) {
            checkScopedRunning();
            try {
                migrateWithPut(key, sourceBucket, destBucket, srcS3Alias, dstS3Alias);
                return;
            } catch (Exception e) {
                checkScopedRunning();
                lastException = e;

                if (isConnectionPoolTimeout(e)) {
                    log.warn("连接池超时，不重试，直接降级: {}, error: {}", key, e.getMessage());
                    throw e;
                }

                if (isSdkInternalError(e)) {
                    log.warn("SDK内部错误，不重试，直接降级: {}, error: {}", key, e.getMessage());
                    throw e;
                }

                if (is503SlowDown(e)) {
                    handle503SlowDown(e, "TRANSFER_OBJECT", null, null, null, key, null, i + 1, "TRANSFER_RETRY");
                }
                if (i < maxRetry) {
                    sleepBeforeRetry(i);
                    log.warn("迁移对象异常，自动重试: {}, 重试次数: {}, Exception: {}", key, i + 1, e.getMessage());
                    continue;
                }
                throw e;
            }
        }
        throw new RuntimeException("迁移失败: " + key, lastException);
    }

    private boolean isSdkInternalError(Exception e) {
        if (e instanceof IllegalArgumentException) {
            String message = e.getMessage();
            if (message != null && message.contains("encoded in multiple of 2 bytes")) {
                return true;
            }
        }
        Throwable cause = e.getCause();
        while (cause != null) {
            if (cause instanceof IllegalArgumentException) {
                String message = cause.getMessage();
                if (message != null && message.contains("encoded in multiple of 2 bytes")) {
                    return true;
                }
            }
            cause = cause.getCause();
        }
        return false;
    }

    private void migrateWithPresignedUrlRetry(String key, long objectSize, String sourceBucket, String destBucket, String srcS3Alias, String dstS3Alias) {
        Exception lastException = null;
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();
        for (int i = 0; i <= maxRetry; i++) {
            checkScopedRunning();
            try {
                migrateWithPresignedUrl(key, objectSize, sourceBucket, destBucket, srcS3Alias, dstS3Alias);
                return;
            } catch (Exception e) {
                checkScopedRunning();
                lastException = e;
                if (is503SlowDown(e)) {
                    handle503SlowDown(e, "PRESIGNED_TRANSFER", null, null, null, key, null, i + 1, "PRESIGNED_TRANSFER_RETRY");
                }
                if (i < maxRetry) {
                    sleepBeforeRetry(i);
                    log.warn("预签名迁移异常，自动重试: {}, 重试次数: {},  Exception: {}", key, i + 1, e.getMessage());
                    continue;
                }
                throw e;
            }
        }
        throw new RuntimeException("预签名迁移失败: " + key, lastException);
    }

    private boolean shouldOverwrite(Boolean overwrite) {
        return overwrite != null && overwrite;
    }

    private boolean objectExistsInDest(String key, Date srcLastModified, String destBucket, String dstS3Alias) {
        Exception lastException = null;
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();
        for (int i = 0; i <= maxRetry; i++) {
            checkScopedRunning();
            try {
                return objectExistsInDestOnce(key, srcLastModified, destBucket, dstS3Alias);
            } catch (Exception e) {
                checkScopedRunning();
                lastException = e;
                if (is503SlowDown(e)) {
                    handle503SlowDown(e, "GET_OBJECT_METADATA", "HEAD", dstS3Alias, destBucket, key, null, i + 1, "HEAD_RETRY");
                }
                if (i < maxRetry) {
                    sleepBeforeRetry(i);
                    log.warn("head请求异常，自动重试: {}, 重试次数: {}, Exception: {}", key, i + 1, e.getMessage());
                    continue;
                }
                throw e;
            }
        }
        throw new RuntimeException("head请求失败: " + key, lastException);
    }

    private boolean objectExistsInDestOnce(String key, Date srcLastModified, String destBucket, String dstS3Alias) {
        AmazonS3 dstS3Client = s3ClientManager.getClient(dstS3Alias);
        if (dstS3Client == null) {
            throw new RuntimeException("目标S3客户端不存在: " + dstS3Alias);
        }

        rateLimiterManager.acquireHead();

        try {
            GetObjectMetadataRequest request = new GetObjectMetadataRequest(
                destBucket, key);
            request.putCustomQueryParameter("x-oss-process", "download");
            request.putCustomRequestHeader("x-oss-type", "mirror");

            ObjectMetadata destMetadata = dstS3Client.getObjectMetadata(request);

            Date destLastModified = destMetadata.getLastModified();

            return destLastModified != null && srcLastModified != null
                && (destLastModified.compareTo(srcLastModified) >= 0);
        } catch (AmazonS3Exception e) {
            if (e.getStatusCode() == 404) {
                return false;
            }
            throw e;
        }
    }

    /** All OSS execution-chain keys stay source keys; mapping happens only at destination boundaries. */
    private String destinationKey(String sourceKey) {
        ObjectListMessage message = taskBandwidthMessage();
        return OssKeyMapper.destinationKey(message == null ? null : message.getKeyMapping(), sourceKey);
    }

    private ObjectListMessage taskBandwidthMessage() {
        RateLimitRequest scope = rateRequestScope.get();
        return scope instanceof RateRequestContext ? ((RateRequestContext) scope).bandwidthMessage : null;
    }

    private void putWithTaskBandwidth(AmazonS3 destination, String bucket, String key, InputStream input,
                                      ObjectMetadata metadata, ObjectListMessage message) {
        if (message == null || (taskBandwidth == null && transferSpeedService == null)) {
            destination.putObject(bucket, key, input, metadata); return;
        }
        TaskBandwidthInputStream stream = new TaskBandwidthInputStream(input, taskBandwidth,
                taskBandwidth == null ? null : taskBandwidth.scope(message), true, transferSpeedService, message.getTaskIndex());
        PutObjectRequest request = new PutObjectRequest(bucket, key, stream, metadata);
        request.setGeneralProgressListener(stream.uploadListener());
        destination.putObject(request);
    }

    private UploadPartResult uploadPartWithTaskBandwidth(AmazonS3 destination, UploadPartRequest request,
                                                        ObjectListMessage message) {
        if (message != null && (taskBandwidth != null || transferSpeedService != null)) {
            TaskBandwidthInputStream stream = new TaskBandwidthInputStream(request.getInputStream(), taskBandwidth,
                    taskBandwidth == null ? null : taskBandwidth.scope(message), true, transferSpeedService, message.getTaskIndex());
            request.setInputStream(stream); request.setGeneralProgressListener(stream.uploadListener());
        }
        return destination.uploadPart(request);
    }

    private void migrateWithPut(String key, String sourceBucket, String destBucket, String srcS3Alias, String dstS3Alias) {
        AmazonS3 srcS3Client = s3ClientManager.getClient(srcS3Alias);
        AmazonS3 dstS3Client = s3ClientManager.getClient(dstS3Alias);

        if (srcS3Client == null) {
            throw new RuntimeException("源S3客户端不存在: " + srcS3Alias);
        }
        if (dstS3Client == null) {
            throw new RuntimeException("目标S3客户端不存在: " + dstS3Alias);
        }

        rateLimiterManager.acquirePut();

        S3Object s3Object = null;
        String operation = "GET_OBJECT";
        try {
            s3Object = srcS3Client.getObject(
                sourceBucket, key);

            ObjectMetadata srcMetadata = s3Object.getObjectMetadata();
            processContentDisposition(srcMetadata);

            try (InputStream srcInputStream = s3Object.getObjectContent();
                 InputStream throttlingInputStream = new BandwidthThrottlingInputStream(cancellableForScope(srcInputStream), rateLimiterManager);
                 InputStream replay = replayableUploadStream(throttlingInputStream, srcMetadata.getContentLength());
                 InputStream upload = new BandwidthThrottlingInputStream(replay, rateLimiterManager)) {

                operation = "PUT_OBJECT";
                putWithTaskBandwidth(dstS3Client,
                    destBucket,
                    destinationKey(key),
                    upload,
                    cleanMetadata(srcMetadata), taskBandwidthMessage());
            }

            log.debug("迁移成功: {}", key);
        } catch (Exception e) {
            rememberRequest(e, operation, "GET_OBJECT".equals(operation) ? "GET" : "PUT",
                    "GET_OBJECT".equals(operation) ? srcS3Alias : dstS3Alias,
                    "GET_OBJECT".equals(operation) ? sourceBucket : destBucket,
                    "GET_OBJECT".equals(operation) ? key : destinationKey(key));
            throw new RuntimeException("sdk迁移失败: " + key, e);
        } finally {
            if (s3Object != null) {
                try {
                    s3Object.close();
                } catch (Exception e) {
                    log.warn("关闭S3Object失败: {}", key, e);
                }
            }
        }
    }

    private void processContentDisposition(ObjectMetadata metadata) {
        String contentDisposition = metadata.getContentDisposition();

        if (contentDisposition != null && contentDisposition.length() > 500) {
            String disposition = "inline";
            if (contentDisposition.toLowerCase().contains("attachment")) {
                disposition = "attachment";
            }
            metadata.setContentDisposition(disposition);
            log.info("ContentDisposition长度超过500，已简化为: {}", disposition);
        }
    }

    private ObjectMetadata cleanMetadata(ObjectMetadata srcMetadata) {
        if (!transferConfig.shouldCleanMetadata()) {
            return srcMetadata;
        }

        ObjectMetadata cleanMetadata = new ObjectMetadata();
        if (srcMetadata.getContentType() != null) {
            cleanMetadata.setContentType(srcMetadata.getContentType());
        }
        if (srcMetadata.getContentEncoding() != null) {
            cleanMetadata.setContentEncoding(srcMetadata.getContentEncoding());
        }
        if (srcMetadata.getCacheControl() != null) {
            cleanMetadata.setCacheControl(srcMetadata.getCacheControl());
        }
        if (srcMetadata.getContentDisposition() != null) {
            cleanMetadata.setContentDisposition(srcMetadata.getContentDisposition());
        }
        if (srcMetadata.getContentLanguage() != null) {
            cleanMetadata.setContentLanguage(srcMetadata.getContentLanguage());
        }
        if (srcMetadata.getUserMetadata() != null) {
            cleanMetadata.setUserMetadata(srcMetadata.getUserMetadata());
        }
        if (srcMetadata.getContentLength() > 0) {
            cleanMetadata.setContentLength(srcMetadata.getContentLength());
        }
        return cleanMetadata;
    }

    private void migrateWithMultipart(String key, long objectSize, String sourceBucket, String destBucket, String srcS3Alias, String dstS3Alias) {
        AmazonS3 srcS3Client = s3ClientManager.getClient(srcS3Alias);
        if (srcS3Client == null) {
            throw new RuntimeException("源S3客户端不存在: " + srcS3Alias);
        }

        ObjectMetadata srcMetadata = srcS3Client.getObjectMetadata(
            sourceBucket, key);
        processContentDisposition(srcMetadata);

        InitiateMultipartUploadRequest initRequest =
            new InitiateMultipartUploadRequest(
                destBucket, destinationKey(key))
                .withObjectMetadata(cleanMetadata(srcMetadata));

        String uploadId = initiateMultipartUploadWithRetry(dstS3Alias, initRequest, destinationKey(key));

        try {
            long partSize = effectivePartSize(objectSize);
            int totalParts = calculateTotalParts(objectSize, partSize);

            PartETag[] partETags = new PartETag[totalParts];
            uploadPartsWithBoundedConcurrency(key, objectSize, partSize, totalParts,
                    sourceBucket, destBucket, uploadId, srcS3Alias, dstS3Alias, partETags);

            CompleteMultipartUploadRequest completeRequest =
                new CompleteMultipartUploadRequest(
                    destBucket,
                    destinationKey(key),
                    uploadId,
                    Arrays.asList(partETags));

            completeMultipartUploadWithRetry(dstS3Alias, completeRequest, destinationKey(key), totalParts);

        } catch (Exception e) {
            AmazonS3 dstS3Client = s3ClientManager.getClient(dstS3Alias);
            if (dstS3Client != null) {
                dstS3Client.abortMultipartUpload(
                    new AbortMultipartUploadRequest(
                        destBucket, destinationKey(key), uploadId));
            }
            throw e;
        }
    }

    private void uploadPartsWithBoundedConcurrency(String key, long objectSize, long partSize,
                                                    int totalParts, String sourceBucket,
                                                    String destBucket, String uploadId,
                                                    String srcS3Alias, String dstS3Alias,
                                                    PartETag[] partETags) {
        int configuredWindow = destBucketConfig.getMultipart().getMaxInFlightPartsPerObject() == null
                ? 4 : destBucketConfig.getMultipart().getMaxInFlightPartsPerObject();
        int window = Math.max(1, Math.min(configuredWindow, partThreadCount));
        CompletionService<PartUploadResult> completion = new ExecutorCompletionService<>(partUploadExecutor);
        List<Future<PartUploadResult>> inFlight = new ArrayList<>(window);
        int nextPart = 1;
        int completed = 0;
        try {
            while (nextPart <= totalParts && inFlight.size() < window) {
                inFlight.add(submitPart(completion, key, objectSize, partSize, nextPart++,
                        sourceBucket, destBucket, uploadId, srcS3Alias, dstS3Alias));
            }
            while (completed < totalParts) {
                Future<PartUploadResult> finished = completion.take();
                inFlight.remove(finished);
                PartUploadResult result = finished.get();
                if (result == null || result.partETag == null) {
                    throw new IllegalStateException("分片上传未返回ETag");
                }
                partETags[result.partNumber - 1] = result.partETag;
                completed++;
                log.info("大对象分片进度: key={}, completed={}/{}, part={}",
                        key, completed, totalParts, result.partNumber);
                if (nextPart <= totalParts) {
                    inFlight.add(submitPart(completion, key, objectSize, partSize, nextPart++,
                            sourceBucket, destBucket, uploadId, srcS3Alias, dstS3Alias));
                }
            }
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            cancelFutures(inFlight);
            throw new RuntimeException("分片上传被中断: " + key, e);
        } catch (Exception e) {
            settleFutures(inFlight, key);
            throw new RuntimeException("sdk分片上传失败: " + key, e);
        }
    }

    private Future<PartUploadResult> submitPart(CompletionService<PartUploadResult> completion,
                                                String key, long objectSize, long partSize,
                                                int partNumber, String sourceBucket,
                                                String destBucket, String uploadId,
                                                String srcS3Alias, String dstS3Alias) {
        long bytePosition = (long) (partNumber - 1) * partSize;
        long lastByte = Math.min(bytePosition + partSize - 1, objectSize - 1);
        RateLimitRequest scope = rateRequestScope.get();
        return completion.submit(() -> {
            RateLimitRequest previous = rateRequestScope.get();
            if (scope == null) rateRequestScope.remove(); else rateRequestScope.set(scope);
            Integer executionId = executionTaskId(scope);
            try (LocalTaskControlService.Operation ignored = executionId != null && localTaskControlService != null
                    ? localTaskControlService.begin(executionId) : null) {
                return uploadPart(key, sourceBucket, destBucket, uploadId,
                        partNumber, bytePosition, lastByte, srcS3Alias, dstS3Alias);
            } finally { restoreRateRequestScope(previous); }
        });
    }

    private void cancelFutures(List<? extends Future<?>> futures) {
        for (Future<?> future : futures) {
            future.cancel(true);
        }
    }

    private void settleFutures(List<? extends Future<?>> futures, String key) {
        for (Future<?> future : futures) {
            try {
                future.get();
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                cancelFutures(futures);
                return;
            } catch (Exception e) {
                log.debug("等待同一uploadId的在途分片结束: key={}, result={}", key, e.getMessage());
            }
        }
    }

    long effectivePartSize(long objectSize) {
        Long configuredValue = destBucketConfig.getMultipart().getPartsize();
        long configured = configuredValue == null ? 32L * 1024 * 1024 : configuredValue;
        long minimumForPartLimit = ((objectSize - 1L) / 10000L) + 1L;
        long partSize = Math.max(configured, minimumForPartLimit);
        long fiveMiB = 5L * 1024 * 1024;
        partSize = Math.max(partSize, fiveMiB);
        long oneMiB = 1024L * 1024L;
        return ((partSize + oneMiB - 1) / oneMiB) * oneMiB;
    }

    int calculateTotalParts(long objectSize, long partSize) {
        if (objectSize <= 0 || partSize <= 0) {
            throw new IllegalArgumentException("对象大小和分片大小必须大于0");
        }
        long total = ((objectSize - 1L) / partSize) + 1L;
        if (total > 10000) {
            throw new IllegalArgumentException("multipart分片数量超过S3上限: " + total);
        }
        return (int) total;
    }

    private String initiateMultipartUploadWithRetry(String dstS3Alias, InitiateMultipartUploadRequest request, String key) {
        AmazonS3 s3Client = s3ClientManager.getClient(dstS3Alias);
        if (s3Client == null) {
            throw new RuntimeException("目标S3客户端不存在: " + dstS3Alias);
        }

        Exception lastException = null;
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();

        for (int retry = 0; retry <= maxRetry; retry++) {
            checkScopedRunning();
            try {
                InitiateMultipartUploadResult result = s3Client.initiateMultipartUpload(request);
                return result.getUploadId();
            } catch (Exception e) {
                checkScopedRunning();
                lastException = e;
                if (is503SlowDown(e)) {
                    handle503SlowDown(e, "INITIATE_MULTIPART_UPLOAD", "POST", dstS3Alias, request.getBucketName(), key, null, retry + 1, "INITIATE_MULTIPART_RETRY");
                }
                if (retry < maxRetry) {
                    sleepBeforeRetry(retry);
                    log.warn("初始化分片上传异常，自动重试: {}, 重试次数: {}", key, retry + 1);
                    continue;
                }
                throw new RuntimeException("初始化分片上传失败: " + key, e);
            }
        }
        throw new RuntimeException("初始化分片上传重试耗尽: " + key, lastException);
    }

    private void completeMultipartUploadWithRetry(String dstS3Alias, CompleteMultipartUploadRequest request, String key, int totalParts) {
        AmazonS3 s3Client = s3ClientManager.getClient(dstS3Alias);
        if (s3Client == null) {
            throw new RuntimeException("目标S3客户端不存在: " + dstS3Alias);
        }

        Exception lastException = null;
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();

        for (int retry = 0; retry <= maxRetry; retry++) {
            checkScopedRunning();
            try {
                checkScopedRunning();
                s3Client.completeMultipartUpload(request);
                log.info("大对象并发分片上传完成: {}, 共{}个分片", key, totalParts);
                return;
            } catch (Exception e) {
                checkScopedRunning();
                lastException = e;
                if (is503SlowDown(e)) {
                    handle503SlowDown(e, "COMPLETE_MULTIPART_UPLOAD", "POST", dstS3Alias, request.getBucketName(), key, null, retry + 1, "COMPLETE_MULTIPART_RETRY");
                }
                if (retry < maxRetry) {
                    sleepBeforeRetry(retry);
                    log.warn("完成分片上传异常，自动重试: {}, 重试次数: {}", key, retry + 1);
                    continue;
                }
                throw new RuntimeException("完成分片上传失败: " + key, e);
            }
        }
        throw new RuntimeException("完成分片上传重试耗尽: " + key, lastException);
    }

    private static class PartUploadResult {
        int partNumber;
        PartETag partETag;

        PartUploadResult(int partNumber, PartETag partETag) {
            this.partNumber = partNumber;
            this.partETag = partETag;
        }
    }

    private PartUploadResult uploadPart(String key, String sourceBucket, String destBucket,
            String uploadId, int partNumber, long bytePosition, long lastByte,
            String srcS3Alias, String dstS3Alias) {
        Exception lastException = null;
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();

        for (int retry = 0; retry <= maxRetry; retry++) {
            checkScopedRunning();
            if (Thread.currentThread().isInterrupted()) {
                throw new RuntimeException("分片上传被取消: " + key + ", part: " + partNumber);
            }
            long actualPartSize = lastByte - bytePosition + 1;
            S3Object partObject = null;
            String operation = "GET_OBJECT";
            try {
                AmazonS3 srcS3Client = s3ClientManager.getClient(srcS3Alias);
                if (srcS3Client == null) {
                    throw new RuntimeException("源S3客户端不存在: " + srcS3Alias);
                }

                partObject = srcS3Client.getObject(
                    new GetObjectRequest(
                        sourceBucket, key)
                        .withRange(bytePosition, lastByte));

                try (InputStream inputStream = partObject.getObjectContent();
                     InputStream throttlingInputStream = new BandwidthThrottlingInputStream(cancellableForScope(inputStream), rateLimiterManager);
                     InputStream replay = replayableUploadStream(throttlingInputStream, actualPartSize);
                     InputStream upload = new BandwidthThrottlingInputStream(replay, rateLimiterManager)) {

                    UploadPartRequest uploadRequest = new UploadPartRequest()
                        .withBucketName(destBucket)
                        .withKey(destinationKey(key))
                        .withUploadId(uploadId)
                        .withPartNumber(partNumber)
                        .withPartSize(actualPartSize)
                        .withInputStream(upload);

                    rateLimiterManager.acquirePut();

                    AmazonS3 dstS3Client = s3ClientManager.getClient(dstS3Alias);
                    if (dstS3Client == null) {
                        throw new RuntimeException("目标S3客户端不存在: " + dstS3Alias);
                    }

                    operation = "UPLOAD_PART";
                    UploadPartResult uploadResult = uploadPartWithTaskBandwidth(dstS3Client, uploadRequest, taskBandwidthMessage());

                    return new PartUploadResult(partNumber, uploadResult.getPartETag());
                }
            } catch (Exception e) {
                checkScopedRunning();
                lastException = e;

                if (is404NoSuchUpload(e)) {
                    log.warn("分片上传uploadId已失效，不再重试: {}, part: {}", key, partNumber);
                    throw new RuntimeException("分片上传uploadId已失效: " + key + ", part: " + partNumber, e);
                }

                if (is503SlowDown(e)) {
                    handle503SlowDown(e, operation, "GET_OBJECT".equals(operation) ? "GET" : "PUT", "GET_OBJECT".equals(operation) ? srcS3Alias : dstS3Alias, "GET_OBJECT".equals(operation) ? sourceBucket : destBucket, "GET_OBJECT".equals(operation) ? key : destinationKey(key), partNumber, retry + 1, "UPLOAD_PART_RETRY");
                }
                if (retry < maxRetry) {
                    sleepBeforeRetry(retry);
                    log.warn("分片上传异常，自动重试: {}, part: {}, 重试次数: {}, 异常堆栈：{} ", key, partNumber, retry + 1, e.getMessage());
                    continue;
                }
                throw new RuntimeException("分片上传失败: " + key + ", part: " + partNumber, e);
            } finally {
                if (partObject != null) {
                    try {
                        partObject.close();
                    } catch (Exception e) {
                        log.warn("关闭分片S3Object失败: {}, part: {}", key, partNumber, e);
                    }
                }
            }
        }
        throw new RuntimeException("分片上传重试耗尽: " + key + ", part: " + partNumber, lastException);
    }

    private void migrateWithPresignedUrl(String key, long objectSize, String sourceBucket, String destBucket, String srcS3Alias, String dstS3Alias) {
        AmazonS3 srcS3Client = s3ClientManager.getClient(srcS3Alias);
        if (srcS3Client == null) {
            throw new RuntimeException("源S3客户端不存在: " + srcS3Alias);
        }

        Date expiration = new Date(System.currentTimeMillis() + 3600000);
        URL presignedUrl = srcS3Client.generatePresignedUrl(
            sourceBucket, key, expiration);

        ObjectMetadata minimalMetadata = new ObjectMetadata();
        if (objectSize > 0) {
            minimalMetadata.setContentLength(objectSize);
        }

        if (objectSize <= destBucketConfig.getMultipart().getThreshold()) {
            migrateWithPresignedUrlPut(key, presignedUrl, minimalMetadata, destBucket, dstS3Alias);
        } else {
            migrateWithPresignedUrlMultipart(key, objectSize, presignedUrl, minimalMetadata, destBucket, dstS3Alias);
        }
    }

    private void migrateWithPresignedUrlPut(String key, URL presignedUrl, ObjectMetadata metadata, String destBucket, String dstS3Alias) {
        AmazonS3 dstS3Client = s3ClientManager.getClient(dstS3Alias);
        if (dstS3Client == null) {
            throw new RuntimeException("目标S3客户端不存在: " + dstS3Alias);
        }

        rateLimiterManager.acquirePut();

        try (InputStream inputStream = presignedUrl.openStream();
             InputStream throttlingInputStream = new BandwidthThrottlingInputStream(cancellableForScope(inputStream), rateLimiterManager);
             InputStream replay = replayableUploadStream(throttlingInputStream, metadata.getContentLength());
             InputStream upload = new BandwidthThrottlingInputStream(replay, rateLimiterManager)) {

            putWithTaskBandwidth(dstS3Client,
                destBucket,
                destinationKey(key),
                upload,
                metadata, taskBandwidthMessage());
        } catch (Exception e) {
            // HTTP URL reads throw IO exceptions; only the SDK destination call
            // can contribute an AmazonS3Exception 503 at this handler.
            rememberRequest(e, "PUT_OBJECT", "PUT", dstS3Alias, destBucket, destinationKey(key));
            throw new RuntimeException("预签名链接put迁移失败: " + key, e);
        }
    }

    private void migrateWithPresignedUrlMultipart(String key, long objectSize, URL presignedUrl, ObjectMetadata srcMetadata, String destBucket, String dstS3Alias) {
        InitiateMultipartUploadRequest initRequest =
            new InitiateMultipartUploadRequest(
                destBucket, destinationKey(key))
                .withObjectMetadata(srcMetadata);

        String uploadId = initiateMultipartUploadWithRetry(dstS3Alias, initRequest, destinationKey(key));

        try {
            long partSize = effectivePartSize(objectSize);
            long bytePosition = 0;
            int partNumber = 1;
            int totalParts = calculateTotalParts(objectSize, partSize);

            PartETag[] partETags = new PartETag[totalParts];

            while (bytePosition < objectSize) {
                checkScopedRunning();
                long lastByte = Math.min(bytePosition + partSize - 1,
                                        objectSize - 1);
                long actualPartSize = lastByte - bytePosition + 1;

                PartETag partETag = uploadPresignedPartWithRetry(key, presignedUrl, destBucket,
                        uploadId, partNumber, bytePosition, lastByte, actualPartSize, dstS3Alias);
                partETags[partNumber - 1] = partETag;

                bytePosition += partSize;
                partNumber++;
            }

            CompleteMultipartUploadRequest completeRequest =
                new CompleteMultipartUploadRequest(
                    destBucket,
                    destinationKey(key),
                    uploadId,
                    Arrays.asList(partETags));

            completeMultipartUploadWithRetry(dstS3Alias, completeRequest, destinationKey(key), totalParts);

        } catch (Exception e) {
            AmazonS3 dstS3Client = s3ClientManager.getClient(dstS3Alias);
            if (dstS3Client != null) {
                dstS3Client.abortMultipartUpload(
                    new AbortMultipartUploadRequest(
                        destBucket, destinationKey(key), uploadId));
            }
            throw new RuntimeException("预签名链接分片迁移失败: " + key, e);
        }
    }

    private PartETag uploadPresignedPartWithRetry(String key, URL presignedUrl, String destBucket,
            String uploadId, int partNumber, long bytePosition, long lastByte, long actualPartSize,
            String dstS3Alias) {
        Exception lastException = null;
        int maxRetry = rateLimiterManager.getAutoAdjustConfig().getMaxRetryOn503();

        for (int retry = 0; retry <= maxRetry; retry++) {
            checkScopedRunning();
            HttpURLConnection conn = null;
            String operation = "PRESIGNED_GET_OBJECT";
            try {
                String rangeHeader = "bytes=" + bytePosition + "-" + lastByte;
                conn = (HttpURLConnection) presignedUrl.openConnection();
                conn.setRequestProperty("Range", rangeHeader);
                conn.connect();

                try (InputStream inputStream = conn.getInputStream();
                     InputStream throttlingInputStream = new BandwidthThrottlingInputStream(cancellableForScope(inputStream), rateLimiterManager);
                     InputStream replay = replayableUploadStream(throttlingInputStream, actualPartSize);
                     InputStream upload = new BandwidthThrottlingInputStream(replay, rateLimiterManager)) {
                    UploadPartRequest uploadRequest = new UploadPartRequest()
                        .withBucketName(destBucket)
                        .withKey(destinationKey(key))
                        .withUploadId(uploadId)
                        .withPartNumber(partNumber)
                        .withPartSize(actualPartSize)
                        .withInputStream(upload);

                    rateLimiterManager.acquirePut();

                    AmazonS3 dstS3Client = s3ClientManager.getClient(dstS3Alias);
                    if (dstS3Client == null) {
                        throw new RuntimeException("目标S3客户端不存在: " + dstS3Alias);
                    }

                    operation = "UPLOAD_PART";
                    UploadPartResult uploadResult = uploadPartWithTaskBandwidth(dstS3Client, uploadRequest, taskBandwidthMessage());

                    log.debug("预签名链接分片上传成功: {}, part: {}", key, partNumber);
                    return uploadResult.getPartETag();
                }
            } catch (Exception e) {
                checkScopedRunning();
                lastException = e;

                if (is404NoSuchUpload(e)) {
                    log.warn("预签名分片上传uploadId已失效，不再重试: {}, part: {}", key, partNumber);
                    throw new RuntimeException("预签名分片上传uploadId已失效: " + key + ", part: " + partNumber, e);
                }

                if (is503SlowDown(e)) {
                    handle503SlowDown(e, operation, "PRESIGNED_GET_OBJECT".equals(operation) ? "GET" : "PUT", "PRESIGNED_GET_OBJECT".equals(operation) ? null : dstS3Alias, "PRESIGNED_GET_OBJECT".equals(operation) ? null : destBucket, "PRESIGNED_GET_OBJECT".equals(operation) ? key : destinationKey(key), partNumber, retry + 1, "PRESIGNED_PART_RETRY");
                }
                if (retry < maxRetry) {
                    sleepBeforeRetry(retry);
                    log.warn("预签名分片上传异常，自动重试: {}, part: {}, 重试次数: {}", key, partNumber, retry + 1);
                    continue;
                }
                throw new RuntimeException("预签名分片上传失败: " + key + ", part: " + partNumber, e);
            } finally {
                if (conn != null) {
                    conn.disconnect();
                }
            }
        }
        throw new RuntimeException("预签名分片上传重试耗尽: " + key + ", part: " + partNumber, lastException);
    }

    private void migrateWithFallback(String key, String errorMessage, String sourceBucket, String destBucket, Integer taskIndex, String srcS3Alias, String dstS3Alias) {
        String tmpDir = transferConfig.getTmpDir();
        if (tmpDir == null || tmpDir.trim().isEmpty()) {
            throw new RuntimeException("降级迁移失败：未配置临时目录");
        }

        File tmpDirFile = new File(tmpDir);
        if (!tmpDirFile.exists()) {
            tmpDirFile.mkdirs();
        }

        String tmpFilePath = tmpDir + File.separator + key.replace("/", "_") + "_" + System.currentTimeMillis();
        File tmpFile = new File(tmpFilePath);

        String operation = "PRESIGNED_GET_OBJECT";
        try {
            AmazonS3 srcS3Client = s3ClientManager.getClient(srcS3Alias);
            if (srcS3Client == null) {
                throw new RuntimeException("源S3客户端不存在: " + srcS3Alias);
            }

            Date expiration = new Date(System.currentTimeMillis() + 3600000);
            URL presignedUrl = srcS3Client.generatePresignedUrl(sourceBucket, key, expiration);

            try (InputStream raw = presignedUrl.openStream();
                 InputStream inputStream = new BandwidthThrottlingInputStream(cancellableForScope(raw), rateLimiterManager);
                 FileOutputStream fos = new FileOutputStream(tmpFile)) {

                byte[] buffer = new byte[8192];
                int bytesRead;
                while ((bytesRead = inputStream.read(buffer)) != -1) {
                    fos.write(buffer, 0, bytesRead);
                }
            }

            log.info("对象已下载到临时文件: {} -> {}", key, tmpFilePath);

            rateLimiterManager.acquirePut();
            ObjectMetadata metadata = new ObjectMetadata();
            metadata.setContentLength(tmpFile.length());

            AmazonS3 dstS3Client = s3ClientManager.getClient(dstS3Alias);
            if (dstS3Client == null) {
                throw new RuntimeException("目标S3客户端不存在: " + dstS3Alias);
            }

            try (InputStream uploadStream = new SeekableFileInputStream(tmpFile.toPath(), 0, tmpFile.length());
                 InputStream throttledUploadStream = new BandwidthThrottlingInputStream(cancellableForScope(uploadStream), rateLimiterManager)) {
                operation = "PUT_OBJECT";
                putWithTaskBandwidth(dstS3Client, destBucket, destinationKey(key), throttledUploadStream, metadata, taskBandwidthMessage());
            }

            log.info("对象已从临时文件上传: {} -> {}", tmpFilePath, key);

            tmpFile.delete();

            if (taskIndex != null) {
                subTaskManagerService.recordFallbackObject(taskIndex, key, tmpFilePath, errorMessage);
            }

        } catch (Exception e) {
            boolean sourceRead = "PRESIGNED_GET_OBJECT".equals(operation);
            rememberRequest(e, operation, sourceRead ? "GET" : "PUT", sourceRead ? srcS3Alias : dstS3Alias,
                    sourceRead ? sourceBucket : destBucket, sourceRead ? key : destinationKey(key));
            log.error("降级迁移失败: {}", key, e);
            if (tmpFile.exists()) {
                tmpFile.delete();
            }
            throw new RuntimeException("降级迁移失败: " + key, e);
        }
    }
}
