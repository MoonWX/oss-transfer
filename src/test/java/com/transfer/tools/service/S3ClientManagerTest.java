package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.transfer.tools.model.S3ClientConfig;
import com.transfer.tools.storage.RocksDBService;
import org.junit.Before;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.ArrayDeque;
import java.util.Queue;
import java.util.List;
import java.util.ArrayList;
import java.util.Map;
import java.util.concurrent.locks.ReentrantReadWriteLock;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.mockito.ArgumentMatchers.*;

public class S3ClientManagerTest {
    private final Queue<AmazonS3> clients = new ArrayDeque<>();
    private final List<String> createdEndpoints = new ArrayList<>();
    private final S3ClientManager manager = new S3ClientManager() {
        @Override AmazonS3 createS3Client(S3ClientConfig config) {
            createdEndpoints.add(config.getEndpoint());
            return clients.remove();
        }
    };
    private RocksDBService db;
    private AmazonS3 original, originalList;
    @Before public void setup() {
        db = mock(RocksDBService.class);
        ReflectionTestUtils.setField(manager, "rocksDBService", db);
        original = mock(AmazonS3.class); originalList = mock(AmazonS3.class);
        clients.add(original); clients.add(originalList);
        manager.addClient("test", "https://original.example", "test", "test", false);
        clearInvocations(db);
        createdEndpoints.clear();
    }
    @Test public void failedValidationKeepsWorkingConnectionAndClosesRejectedClients() {
        AmazonS3 rejected = mock(AmazonS3.class), rejectedList = mock(AmazonS3.class);
        clients.add(rejected); clients.add(rejectedList);
        when(rejected.listBuckets()).thenThrow(new IllegalArgumentException("invalid endpoint"));
        try { manager.updateClient("test", "https://invalid.example", "test", "test", null, null, true); fail(); }
        catch (RuntimeException expected) { }
        assertSame(original, manager.getClient("test"));
        assertSame(originalList, manager.getListClient("test"));
        assertEquals("https://original.example", manager.getConfig("test").getEndpoint());
        verify(original, never()).shutdown(); verify(originalList, never()).shutdown();
        verify(rejected).shutdown(); verify(rejectedList).shutdown();
        verify(db, never()).putSync(anyString(), any());
    }
    @Test public void failedPersistenceDoesNotPublishTheReplacement() {
        AmazonS3 replacement = mock(AmazonS3.class), replacementList = mock(AmazonS3.class);
        clients.add(replacement); clients.add(replacementList);
        doThrow(new RuntimeException("disk full")).when(db).putSync(anyString(), any());
        try { manager.updateClient("test", "https://new.example", "test", "test", null, null, false); fail(); }
        catch (RuntimeException expected) { }
        assertSame(original, manager.getClient("test"));
        verify(original, never()).shutdown(); verify(replacement).shutdown(); verify(replacementList).shutdown();
    }
    @Test public void validReplacementPublishesBothClientsAfterPersistence() {
        AmazonS3 replacement = mock(AmazonS3.class), replacementList = mock(AmazonS3.class);
        clients.add(replacement); clients.add(replacementList);
        manager.updateClient("test", "https://new.example", "test", "test", null, null, false);
        assertSame(replacement, manager.getClient("test")); assertSame(replacementList, manager.getListClient("test"));
        org.mockito.InOrder order = inOrder(db, original, originalList);
        order.verify(db).putSync(eq("s3_config_test"), any(S3ClientConfig.class));
        order.verify(original).shutdown(); order.verify(originalList).shutdown();
    }
    @Test public void failedDeletionKeepsConnectionUsable() {
        doThrow(new RuntimeException("disk full")).when(db).delete("s3_config_test");
        assertFalse(manager.removeClient("test")); assertSame(original, manager.getClient("test"));
        verify(original, never()).shutdown();
    }

    @Test public void queuedRecreationCannotUndoAnAcknowledgedConfigurationUpdate() throws Exception {
        AmazonS3 replacement = mock(AmazonS3.class), replacementList = mock(AmazonS3.class), recreated = mock(AmazonS3.class);
        clients.add(replacement); clients.add(replacementList); clients.add(recreated);
        ReentrantReadWriteLock lock = aliasLock();
        Thread recreation = new Thread(() -> manager.recreateClient("test"), "queued-recreation");
        lock.writeLock().lock();
        try {
            recreation.start(); awaitQueued(lock, recreation);
            manager.updateClient("test", "https://new.example", "test", "test", null, null, false);
        } finally { lock.writeLock().unlock(); }
        recreation.join(3000); assertFalse(recreation.isAlive());
        assertEquals("https://new.example", createdEndpoints.get(createdEndpoints.size() - 1));
        assertSame(recreated, manager.getClient("test"));
        assertSame(replacementList, manager.getListClient("test"));
        assertEquals("https://new.example", manager.getConfig("test").getEndpoint());
    }

    @Test public void queuedRecreationCannotResurrectADeletedConnection() throws Exception {
        clients.add(mock(AmazonS3.class));
        ReentrantReadWriteLock lock = aliasLock();
        Thread recreation = new Thread(() -> manager.recreateClient("test"), "queued-recreation-delete");
        lock.writeLock().lock();
        try {
            recreation.start(); awaitQueued(lock, recreation);
            assertTrue(manager.removeClient("test"));
        } finally { lock.writeLock().unlock(); }
        recreation.join(3000); assertFalse(recreation.isAlive());
        assertNull(manager.getClient("test")); assertNull(manager.getListClient("test"));
        assertNull(manager.getConfig("test")); assertTrue(createdEndpoints.isEmpty());
    }

    @Test public void duplicateCreationCannotReplaceClientsOrDurableConfiguration() {
        try { manager.addClient("test", "https://replacement.example", "changed", "changed", false); fail(); }
        catch (RuntimeException expected) { assertTrue(expected.getMessage().contains("已存在")); }
        assertSame(original, manager.getClient("test"));
        assertSame(originalList, manager.getListClient("test"));
        assertEquals("https://original.example", manager.getConfig("test").getEndpoint());
        assertTrue(createdEndpoints.isEmpty());
        verify(db, never()).putSync(anyString(), any());
        verify(original, never()).shutdown(); verify(originalList, never()).shutdown();
    }

    @Test public void persistedAliasWhoseClientFailedAtStartupCanOnlyBeRepairedByUpdate() {
        when(db.get("s3_config_unloaded", S3ClientConfig.class)).thenReturn(
                new S3ClientConfig("unloaded", "https://old.example", "old", "old", false, true));
        try { manager.addClient("unloaded", "https://replacement.example", "test", "test", false); fail(); }
        catch (RuntimeException expected) { assertTrue(expected.getMessage().contains("已存在")); }
        assertTrue(createdEndpoints.isEmpty()); verify(db, never()).putSync(anyString(), any());
        AmazonS3 repaired = mock(AmazonS3.class), repairedList = mock(AmazonS3.class);
        clients.add(repaired); clients.add(repairedList);
        assertTrue(manager.updateClient("unloaded", "https://repaired.example", "test", "test", false, true, false));
        assertSame(repaired, manager.getClient("unloaded"));
        assertSame(repairedList, manager.getListClient("unloaded"));
        verify(db).putSync(eq("s3_config_unloaded"), any(S3ClientConfig.class));
    }

    @Test public void updateCannotCreateAMissingAlias() {
        try { manager.updateClient("missing", "https://new.example", "test", "test", null, null, false); fail(); }
        catch (RuntimeException expected) { assertTrue(expected.getMessage().contains("不存在")); }
        assertTrue(createdEndpoints.isEmpty()); verify(db, never()).putSync(anyString(), any());
        assertNull(manager.getConfig("missing"));
    }

    @Test public void partialClientInitializationFailurePreservesExistingConnection() {
        AmazonS3 rejected = mock(AmazonS3.class); clients.add(rejected);
        // Creating the list client throws when the queue is empty.
        try { manager.updateClient("test", "https://new.example", "test", "test", null, null, false); fail(); }
        catch (RuntimeException expected) { }
        assertSame(original, manager.getClient("test"));
        assertSame(originalList, manager.getListClient("test"));
        verify(rejected).shutdown(); verify(original, never()).shutdown(); verify(originalList, never()).shutdown();
        verify(db, never()).putSync(anyString(), any());
    }

    @Test public void concurrentCreatesPublishExactlyOneConnection() throws Exception {
        AmazonS3 first = mock(AmazonS3.class), firstList = mock(AmazonS3.class);
        clients.add(first); clients.add(firstList);
        java.util.concurrent.CountDownLatch start = new java.util.concurrent.CountDownLatch(1);
        java.util.concurrent.atomic.AtomicInteger accepted = new java.util.concurrent.atomic.AtomicInteger();
        java.util.concurrent.atomic.AtomicInteger conflicts = new java.util.concurrent.atomic.AtomicInteger();
        java.util.concurrent.atomic.AtomicReference<Throwable> unexpected = new java.util.concurrent.atomic.AtomicReference<>();
        Runnable create = () -> {
            try { start.await(); manager.addClient("new", "https://new.example", "test", "test", false); accepted.incrementAndGet(); }
            catch (Throwable error) {
                if (error.getMessage() != null && error.getMessage().contains("已存在")) conflicts.incrementAndGet();
                else unexpected.set(error);
            }
        };
        Thread one = new Thread(create), two = new Thread(create); one.start(); two.start(); start.countDown();
        one.join(3000); two.join(3000); assertFalse(one.isAlive()); assertFalse(two.isAlive());
        assertNull(unexpected.get()); assertEquals(1, accepted.get()); assertEquals(1, conflicts.get());
        assertSame(first, manager.getClient("new")); assertSame(firstList, manager.getListClient("new"));
        assertEquals(2, createdEndpoints.size()); verify(db, times(1)).putSync(eq("s3_config_new"), any(S3ClientConfig.class));
    }

    private ReentrantReadWriteLock aliasLock() {
        return (ReentrantReadWriteLock) ((Map) ReflectionTestUtils.getField(manager, "clientLocks")).get("test");
    }

    private void awaitQueued(ReentrantReadWriteLock lock, Thread thread) throws Exception {
        long deadline = System.nanoTime() + java.util.concurrent.TimeUnit.SECONDS.toNanos(3);
        while (!lock.hasQueuedThread(thread) && System.nanoTime() < deadline) Thread.sleep(2);
        assertTrue("recreation must be waiting behind the update lock", lock.hasQueuedThread(thread));
    }
}
