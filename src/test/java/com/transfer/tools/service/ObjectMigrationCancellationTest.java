package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.*;
import com.transfer.tools.config.RateLimitConfig;
import com.transfer.tools.model.*;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import java.io.*;
import java.nio.file.*;
import java.util.concurrent.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class ObjectMigrationCancellationTest {
    @Rule public TemporaryFolder temp = new TemporaryFolder();
    private com.transfer.tools.storage.RocksDBService database;
    @After public void closeDatabase() { if (database != null) database.destroy(); }
    @Test public void stopInterruptsBlockedDownloadAndLeavesNoPartialDestinationOrRetries() throws Exception {
        LocalTaskControlService control = new LocalTaskControlService();
        LocalFileService files = new LocalFileService();
        ReflectionTestUtils.setField(files, "localTaskControlService", control);
        database = new com.transfer.tools.storage.RocksDBService();
        ReflectionTestUtils.setField(database, "dbPath", temp.newFolder("db").getAbsolutePath());
        database.init();
        LocalTemporaryFileRegistry registry = new LocalTemporaryFileRegistry();
        ReflectionTestUtils.setField(registry, "rocksDBService", database);
        registry.recoverInterrupted();
        ReflectionTestUtils.setField(files, "temporaryFileRegistry", registry);
        AmazonS3 source = mock(AmazonS3.class);
        S3ClientManager clients = mock(S3ClientManager.class);
        when(clients.getClient("source")).thenReturn(source);
        CountDownLatch entered = new CountDownLatch(1), closed = new CountDownLatch(1);
        InputStream blocked = new InputStream() {
            @Override public int read() throws IOException { return read(new byte[1], 0, 1); }
            @Override public int read(byte[] bytes, int off, int len) throws IOException {
                entered.countDown();
                try { closed.await(); } catch (InterruptedException error) { throw new IOException("cancelled", error); }
                throw new IOException("closed");
            }
            @Override public void close() { closed.countDown(); }
        };
        S3Object object = new S3Object(); object.setObjectContent(new S3ObjectInputStream(blocked, null));
        when(source.getObject("bucket", "file")).thenReturn(object);
        RateLimiterManager rates = mock(RateLimiterManager.class);
        when(rates.getAutoAdjustConfig()).thenReturn(new RateLimitConfig.AutoAdjustConfig());
        ObjectMigrationService migration = new ObjectMigrationService();
        ReflectionTestUtils.setField(migration, "localTaskControlService", control);
        ReflectionTestUtils.setField(migration, "localFileService", files);
        ReflectionTestUtils.setField(migration, "s3ClientManager", clients);
        ReflectionTestUtils.setField(migration, "rateLimiterManager", rates);
        ReflectionTestUtils.setField(migration, "localTaskRateLimiterService", mock(LocalTaskRateLimiterService.class));
        Path root = temp.newFolder("downloads").toPath(); Files.write(root.resolve("file"), new byte[]{9});
        S3ObjectSummary summary = new S3ObjectSummary(); summary.setKey("file"); summary.setSize(100);
        ObjectListMessage message = new ObjectListMessage(summary, 100, false, "bucket", null, "source", null, true, "file");
        message.setDirection(TransferDirection.OSS_TO_LOCAL); message.setConflictPolicy(ConflictPolicy.OVERWRITE); message.setLocalPath(root.toString());
        ExecutorService worker = Executors.newSingleThreadExecutor();
        try {
            Future<Boolean> future = worker.submit(() -> migration.migrate(message));
            assertTrue(entered.await(3, TimeUnit.SECONDS)); control.stop(100);
            try { future.get(3, TimeUnit.SECONDS); fail("cancelled download must not complete"); }
            catch (ExecutionException error) { assertTrue(error.getCause() instanceof TaskCancelledException); }
            assertArrayEquals(new byte[]{9}, Files.readAllBytes(root.resolve("file")));
            try (java.util.stream.Stream<Path> children = Files.list(root)) { assertEquals(1L, children.count()); }
            verify(source, times(1)).getObject("bucket", "file");
            assertFalse(control.isActive(100));
            assertFalse(worker.submit(() -> Thread.currentThread().isInterrupted()).get(3, TimeUnit.SECONDS));
        } finally { worker.shutdownNow(); }
    }
}
