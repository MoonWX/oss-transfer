package com.transfer.tools.service;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;
public class TaskCreationServiceTest {
    @Rule public TemporaryFolder temp = new TemporaryFolder();
    private RocksDBService db;
    private String path;
    private TaskCreationService service;
    @Before public void setUp() throws Exception { path=temp.newFolder().getAbsolutePath(); open(); }
    private void open() { db=new RocksDBService(); ReflectionTestUtils.setField(db,"dbPath",path); db.init(); service=new TaskCreationService(); ReflectionTestUtils.setField(service,"rocksDBService",db); }
    @After public void close() { db.destroy(); }
    @Test public void acknowledgedTasksSurviveRestartAndInterruptedCreationIsExplicit() {
        String id=service.create(); service.beginScope(id); service.recordTaskIds(Arrays.asList(101,102)); service.clearScope();
        db.destroy(); open(); service.recoverInterrupted();
        assertEquals(Arrays.asList(101,102),service.get(id).getTaskIds());
        assertEquals("FAILED",service.get(id).getStatus());
        assertTrue(service.get(id).getMessage().contains("重启"));
    }
    @Test public void duplicateAcknowledgementsAndThreadScopeCannotMixCreations() throws Exception {
        String first=service.create(), second=service.create();
        service.beginScope(first); service.recordTaskIds(Arrays.asList(1,1,2));
        Thread other=new Thread(() -> { service.beginScope(second); service.recordTaskIds(Collections.singletonList(9)); service.clearScope(); });
        other.start(); other.join(); service.recordTaskIds(Collections.singletonList(3)); service.clearScope();
        service.recordTaskIds(Collections.singletonList(99));
        assertEquals(Arrays.asList(1,2,3),service.get(first).getTaskIds());
        assertEquals(Collections.singletonList(9),service.get(second).getTaskIds());
    }
    @Test public void completedEmptyCreationAndInvalidIdRemainDistinct() {
        String id=service.create(); service.complete(id); service.recoverInterrupted();
        assertEquals("COMPLETED",service.get(id).getStatus()); assertTrue(service.get(id).getTaskIds().isEmpty());
        assertNull(service.get("../anything"));
    }
}
