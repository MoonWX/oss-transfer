package com.transfer.tools.service;

import com.transfer.tools.model.ConflictPolicy;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;

import java.io.ByteArrayInputStream;
import java.nio.file.*;
import java.util.List;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class LocalTemporaryFileRegistryTest {
    @Rule public TemporaryFolder temporary = new TemporaryFolder();
    private RocksDBService database;
    private LocalTemporaryFileRegistry registry;
    private Path databasePath;
    private Path root;

    @Before public void setup() throws Exception {
        databasePath = temporary.newFolder("database").toPath();
        root = temporary.newFolder("destination").toPath().toRealPath();
        openDatabase();
        registry = newRegistry();
    }

    @After public void close() { if (database != null) database.destroy(); }

    @Test public void reopenedDatabaseCleansOnlyOwnedInterruptedFileBeforeRetry() throws Exception {
        Path target = root.resolve("result.bin");
        Files.write(target, new byte[]{7});
        Path unrelated = Files.write(root.resolve(".transfer-user.part"), new byte[]{8});
        LocalTemporaryFileRegistry.OwnedFile interrupted = registry.create(root, target, "result.bin", -123);
        interrupted.getOutputStream().write(new byte[]{1, 2, 3});
        interrupted.getOutputStream().close(); // The process exits without OwnedFile.close().
        Path abandoned = interrupted.getPath();
        reopenAndRecover();
        assertFalse(Files.exists(abandoned));
        assertArrayEquals(new byte[]{7}, Files.readAllBytes(target));
        assertArrayEquals(new byte[]{8}, Files.readAllBytes(unrelated));
        assertTrue(entries().isEmpty());

        LocalFileService files = files();
        assertTrue(files.writeAtomically(new ByteArrayInputStream(new byte[]{4, 5}), root.toString(),
                "result.bin", -123, ConflictPolicy.OVERWRITE, 2));
        assertArrayEquals(new byte[]{4, 5}, Files.readAllBytes(target));
        assertTrue(entries().isEmpty());
    }

    @Test public void completedRenameNeverDeletesFinalTargetEvenIfRecordSurvives() throws Exception {
        Path target = root.resolve("complete.bin");
        LocalTemporaryFileRegistry.OwnedFile file = registry.create(root, target, "complete.bin", 4);
        file.getOutputStream().write(new byte[]{4});
        file.getOutputStream().close();
        Files.move(file.getPath(), target, StandardCopyOption.ATOMIC_MOVE);
        reopenAndRecover();
        assertArrayEquals(new byte[]{4}, Files.readAllBytes(target));
        assertTrue(entries().isEmpty());
    }

    @Test public void replacementInodeAndSymlinkArePreserved() throws Exception {
        LocalTemporaryFileRegistry.OwnedFile first = registry.create(root, root.resolve("one"), "one", 1);
        first.getOutputStream().close();
        Path original = root.resolve("original-kept-for-test");
        Files.move(first.getPath(), original);
        Files.write(first.getPath(), new byte[]{9});
        LocalTemporaryFileRegistry.OwnedFile second = registry.create(root, root.resolve("two"), "two", 2);
        second.getOutputStream().close();
        Files.delete(second.getPath());
        Files.createSymbolicLink(second.getPath(), original);
        reopenAndRecover();
        assertArrayEquals(new byte[]{9}, Files.readAllBytes(first.getPath()));
        assertTrue(Files.isSymbolicLink(second.getPath()));
        assertTrue(Files.exists(original));
        assertEquals(2, entries().size());
    }

    @Test public void parentSymlinkReplacementCannotRedirectCleanup() throws Exception {
        Path parent = Files.createDirectory(root.resolve("nested"));
        LocalTemporaryFileRegistry.OwnedFile file = registry.create(root, parent.resolve("target"), "nested/target", 3);
        file.getOutputStream().write(5);
        file.getOutputStream().close();
        Path oldParent = root.resolve("moved");
        Files.move(parent, oldParent);
        Path external = temporary.newFolder("unrelated").toPath();
        Files.write(external.resolve(file.getPath().getFileName()), new byte[]{8});
        Files.createSymbolicLink(parent, external);
        reopenAndRecover();
        assertArrayEquals(new byte[]{8}, Files.readAllBytes(file.getPath()));
        assertArrayEquals(new byte[]{5}, Files.readAllBytes(oldParent.resolve(file.getPath().getFileName())));
        assertEquals(1, entries().size());
    }

    @Test public void plannedFileWithUnknownIdentityIsKeptWithoutBody() throws Exception {
        LocalTemporaryFileRegistry.OwnedFile file = registry.create(root, root.resolve("target"), "target", 5);
        file.getOutputStream().close();
        LocalTemporaryFileRegistry.Entry entry = entries().get(0);
        entry.stage = "PLANNED"; entry.fileKey = null;
        database.putSync(LocalTemporaryFileRegistry.PREFIX + entry.id, entry);
        reopenAndRecover();
        assertEquals(0, Files.size(file.getPath()));
        assertEquals(1, entries().size());
    }

    @Test public void activeCurrentProcessFileIsNotCollected() throws Exception {
        try (LocalTemporaryFileRegistry.OwnedFile file = registry.create(root, null, "upload-spool", 6)) {
            if (Files.getFileStore(root).supportsFileAttributeView("posix")) {
                assertEquals(java.nio.file.attribute.PosixFilePermissions.fromString("rw-------"),
                        Files.getPosixFilePermissions(file.getPath()));
            }
            file.getOutputStream().write(1);
            registry.recoverInterrupted();
            assertTrue(Files.exists(file.getPath()));
            assertEquals(1, entries().size());
            file.getOutputStream().write(2);
        }
        assertTrue(entries().isEmpty());
    }

    @Test public void mutableFileAndDirectoryTimesAreNotUsedAsIdentity() throws Exception {
        LocalTemporaryFileRegistry.OwnedFile file = registry.create(root, root.resolve("target"), "target", 10);
        file.getOutputStream().write(new byte[]{1, 2});
        file.getOutputStream().close();
        Files.setLastModifiedTime(file.getPath(), java.nio.file.attribute.FileTime.fromMillis(1000));
        Files.setLastModifiedTime(root, java.nio.file.attribute.FileTime.fromMillis(2000));
        file.verifyIdentity();
        reopenAndRecover();
        assertFalse(Files.exists(file.getPath()));
        assertTrue(entries().isEmpty());
    }

    @Test public void openRegistrationFailureNeverReadsBodyOrChangesDestination() throws Exception {
        RuntimeException failure = new RuntimeException("OPEN WAL failure");
        doAnswer(call -> {
            LocalTemporaryFileRegistry.Entry entry = call.getArgument(1);
            if ("OPEN".equals(entry.stage)) throw failure;
            return call.callRealMethod();
        }).when(database).putSync(startsWith(LocalTemporaryFileRegistry.PREFIX), any(LocalTemporaryFileRegistry.Entry.class));
        ByteArrayInputStream input = spy(new ByteArrayInputStream(new byte[]{1}));
        Files.write(root.resolve("target"), new byte[]{8});
        try { files().writeAtomically(input, root.toString(), "target", 7, ConflictPolicy.OVERWRITE); fail(); }
        catch (RuntimeException expected) { assertSame(failure, expected); }
        verify(input, never()).read(any(byte[].class));
        assertArrayEquals(new byte[]{8}, Files.readAllBytes(root.resolve("target")));
        assertTrue(entries().isEmpty());
        try (java.util.stream.Stream<Path> children = Files.list(root)) { assertEquals(1, children.count()); }
    }

    @Test public void failedRecordDeletionIsRetriedWithoutTouchingRenamedTarget() throws Exception {
        doThrow(new IllegalStateException("delete failed")).when(database).delete(startsWith(LocalTemporaryFileRegistry.PREFIX));
        assertTrue(files().writeAtomically(new ByteArrayInputStream(new byte[]{3}), root.toString(),
                "target", 8, ConflictPolicy.OVERWRITE));
        assertEquals(1, entries().size());
        reopenAndRecover();
        assertArrayEquals(new byte[]{3}, Files.readAllBytes(root.resolve("target")));
        assertTrue(entries().isEmpty());
    }

    @Test public void startupDatabaseScanFailureDoesNotPretendRecoverySucceeded() {
        doThrow(new IllegalStateException("unreadable ownership WAL")).when(database)
                .scanPage(eq(LocalTemporaryFileRegistry.PREFIX), isNull(), anyInt(), eq(LocalTemporaryFileRegistry.Entry.class));
        try { newRegistry(); fail(); }
        catch (IllegalStateException expected) { assertEquals("unreadable ownership WAL", expected.getMessage()); }
    }

    private LocalFileService files() {
        LocalFileService service = new LocalFileService();
        ReflectionTestUtils.setField(service, "temporaryFileRegistry", registry);
        return service;
    }
    private List<LocalTemporaryFileRegistry.Entry> entries() {
        return database.scanByPrefix(LocalTemporaryFileRegistry.PREFIX, LocalTemporaryFileRegistry.Entry.class);
    }
    private void openDatabase() {
        database = spy(new RocksDBService());
        ReflectionTestUtils.setField(database, "dbPath", databasePath.toString());
        database.init();
    }
    private LocalTemporaryFileRegistry newRegistry() {
        LocalTemporaryFileRegistry result = new LocalTemporaryFileRegistry();
        ReflectionTestUtils.setField(result, "rocksDBService", database);
        result.recoverInterrupted();
        return result;
    }
    private void reopenAndRecover() {
        database.destroy();
        openDatabase();
        registry = newRegistry();
    }
}
