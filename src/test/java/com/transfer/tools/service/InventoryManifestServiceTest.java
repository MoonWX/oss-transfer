package com.transfer.tools.service;

import com.transfer.tools.storage.RocksDBService;
import org.junit.After;
import org.junit.Before;
import org.junit.Rule;
import org.junit.Test;
import org.junit.rules.TemporaryFolder;
import org.mockito.ArgumentCaptor;
import org.springframework.test.util.ReflectionTestUtils;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardOpenOption;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class InventoryManifestServiceTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService database;
    private InventoryManifestService manifests;

    @Before public void open() throws Exception {
        database = spy(new RocksDBService());
        ReflectionTestUtils.setField(database, "dbPath", temporary.newFolder("db").getAbsolutePath());
        database.init();
        manifests = new InventoryManifestService();
        ReflectionTestUtils.setField(manifests, "rocksDBService", database);
    }

    @After public void close() { if (database != null) database.destroy(); }

    @Test public void readyWriteFailureRemovesOnlyNewSnapshotIncludingAlreadyWrittenReadyMarker() throws Exception {
        String preserved = manifests.create(input("preserved.incr", 1));
        RuntimeException failure = new RuntimeException("ready write failed");
        doAnswer(call -> {
            call.callRealMethod();
            throw failure;
        }).when(database).putSync(startsWith("inventory:"), eq(true));

        try {
            manifests.create(input("failing.incr", 1001));
            fail("expected ready write failure");
        } catch (RuntimeException caught) {
            assertSame(failure, caught);
        }

        ArgumentCaptor<String> deleted = ArgumentCaptor.forClass(String.class);
        verify(database).deleteByPrefix(deleted.capture());
        assertNotEquals("inventory:" + preserved + ":", deleted.getValue());
        assertTrue(database.scanPage(deleted.getValue(), null, 1000, Object.class).getItems().isEmpty());
        manifests.requireReady(preserved);
        assertEquals(1, manifests.page(preserved, null).getItems().size());
    }

    @Test public void invalidUtf8AfterCommittedBatchRemovesPartialSnapshot() throws Exception {
        Path source = input("malformed.incr", 2000);
        Files.write(source, new byte[]{(byte) 0xc3, 0x28}, StandardOpenOption.APPEND);
        try {
            manifests.create(source);
            fail("expected malformed UTF-8 failure");
        } catch (IOException expected) {
            assertEquals(0, expected.getSuppressed().length);
        }
        verify(database, atLeastOnce()).putBatchSync(anyMap());
        verify(database).deleteByPrefix(startsWith("inventory:"));
        assertTrue(database.scanPage("inventory:", null, 1000, Object.class).getItems().isEmpty());
    }

    @Test public void cleanupFailureIsSuppressedWithoutReplacingOriginalFailure() throws Exception {
        RuntimeException original = new RuntimeException("ready write failed");
        RuntimeException cleanup = new RuntimeException("cleanup failed");
        doThrow(original).when(database).putSync(startsWith("inventory:"), eq(true));
        doThrow(cleanup).when(database).deleteByPrefix(startsWith("inventory:"));
        try {
            manifests.create(input("cleanup-failure.incr", 1001));
            fail("expected ready write failure");
        } catch (RuntimeException caught) {
            assertSame(original, caught);
            assertArrayEquals(new Throwable[]{cleanup}, caught.getSuppressed());
        }
        verify(database, times(2)).putBatchSync(anyMap());
    }

    private Path input(String name, int count) throws IOException {
        Path source = temporary.newFile(name).toPath();
        StringBuilder text = new StringBuilder();
        for (int index = 0; index < count; index++) {
            text.append("source/long-enough-key-to-exceed-reader-buffer/").append(index).append('\n');
        }
        Files.write(source, text.toString().getBytes(StandardCharsets.UTF_8));
        return source;
    }
}
