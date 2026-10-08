package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.transfer.tools.agent.AgentRegistryService;
import com.transfer.tools.model.S3ClientConfig;
import com.transfer.tools.storage.RocksDBService;
import org.junit.Before;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class S3ClientManagerAgentNotificationTest {
    private final Queue<AmazonS3> clients = new ArrayDeque<>();
    private final S3ClientManager manager = new S3ClientManager() {
        @Override AmazonS3 createS3Client(S3ClientConfig config) { return clients.remove(); }
    };
    private RocksDBService db;
    private AgentRegistryService registry;
    private AmazonS3 original, originalList;
    @Before public void setup() {
        db = mock(RocksDBService.class); registry = mock(AgentRegistryService.class);
        ReflectionTestUtils.setField(manager, "rocksDBService", db);
        ReflectionTestUtils.setField(manager, "agentRegistry", registry);
        original = mock(AmazonS3.class); originalList = mock(AmazonS3.class);
        clients.add(original); clients.add(originalList);
        assertTrue(manager.addClient("archive", "https://old.example.com", "test-access", "test-secret", false));
        doThrow(new IllegalStateException("storage generation write failed")).when(registry).storageChanged();
    }
    @Test public void notificationFailureCannotCloseTheAlreadyPublishedAndDurableConnection() {
        AmazonS3 replacement = mock(AmazonS3.class), replacementList = mock(AmazonS3.class);
        clients.add(replacement); clients.add(replacementList);
        assertTrue(manager.updateClient("archive", "https://current.example.com", "test-access", "test-secret", false, true, false));
        assertSame(replacement, manager.getClient("archive"));
        assertSame(replacementList, manager.getListClient("archive"));
        assertEquals("https://current.example.com", manager.getConfig("archive").getEndpoint());
        verify(replacement, never()).shutdown(); verify(replacementList, never()).shutdown();
        verify(original).shutdown(); verify(originalList).shutdown();
        verify(db, times(2)).putSync(eq("s3_config_archive"), any(S3ClientConfig.class));
    }
    @Test public void notificationFailureCannotReportAnAcknowledgedDeletionAsFailed() {
        assertTrue(manager.removeClient("archive"));
        assertNull(manager.getClient("archive")); assertNull(manager.getConfig("archive"));
        verify(db).delete("s3_config_archive");
        verify(original).shutdown(); verify(originalList).shutdown();
    }
}
