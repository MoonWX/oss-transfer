package com.transfer.tools.service;

import com.transfer.tools.model.ConflictPolicy;
import com.transfer.tools.model.LocalFileItem;
import org.junit.Rule;
import org.junit.Test;
import org.junit.Before;
import org.junit.After;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import com.transfer.tools.storage.RocksDBService;

import java.io.ByteArrayInputStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.List;

import static org.junit.Assert.*;

public class LocalFileServiceTest {
    @Rule public TemporaryFolder temporaryFolder = new TemporaryFolder();
    private final LocalFileService service = new LocalFileService();
    private RocksDBService database;

    @Before public void registerTemporaryFiles() throws Exception {
        database = new RocksDBService();
        ReflectionTestUtils.setField(database, "dbPath", temporaryFolder.newFolder("db").getAbsolutePath());
        database.init();
        LocalTemporaryFileRegistry registry = new LocalTemporaryFileRegistry();
        ReflectionTestUtils.setField(registry, "rocksDBService", database);
        registry.recoverInterrupted();
        ReflectionTestUtils.setField(service, "temporaryFileRegistry", registry);
    }

    @After public void closeDatabase() { if (database != null) database.destroy(); }

    @Test
    public void scansRegularFilesInStableRelativeOrder() throws Exception {
        Path root = temporaryFolder.newFolder("源目录").toPath();
        Files.createDirectories(root.resolve("b"));
        Files.write(root.resolve("b/二.txt"), "2".getBytes(StandardCharsets.UTF_8));
        Files.write(root.resolve("a.txt"), "1".getBytes(StandardCharsets.UTF_8));
        List<LocalFileItem> items = new ArrayList<>();
        service.scan(root.toString(), items::add);
        assertEquals(2, items.size());
        assertEquals("a.txt", items.get(0).getRelativeKey());
        assertEquals("b/二.txt", items.get(1).getRelativeKey());
    }

    @Test
    public void atomicWriteHonorsSkipAndOverwrite() throws Exception {
        Path root = temporaryFolder.newFolder("destination").toPath();
        assertTrue(service.writeAtomically(stream("first"), root.toString(), "nested/file.txt", 7, ConflictPolicy.SKIP));
        assertFalse(service.writeAtomically(stream("second"), root.toString(), "nested/file.txt", 7, ConflictPolicy.SKIP));
        assertEquals("first", new String(Files.readAllBytes(root.resolve("nested/file.txt")), StandardCharsets.UTF_8));
        assertTrue(service.writeAtomically(stream("second"), root.toString(), "nested/file.txt", 7, ConflictPolicy.OVERWRITE));
        assertEquals("second", new String(Files.readAllBytes(root.resolve("nested/file.txt")), StandardCharsets.UTF_8));
        assertEquals(0, Files.list(root.resolve("nested")).filter(path -> path.getFileName().toString().endsWith(".part")).count());
    }

    @Test(expected = TransferValidationException.class)
    public void rejectsPathTraversal() throws Exception {
        Path root = temporaryFolder.newFolder("safe-root").toPath();
        service.resolveDestination(root.toString(), "../../escape.txt");
    }

    private ByteArrayInputStream stream(String value) {
        return new ByteArrayInputStream(value.getBytes(StandardCharsets.UTF_8));
    }
}
