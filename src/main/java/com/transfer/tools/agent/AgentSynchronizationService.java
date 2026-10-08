package com.transfer.tools.agent;

import com.transfer.tools.service.NodeRegistryService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.Lazy;
import org.springframework.stereotype.Service;
import javax.annotation.PreDestroy;
import java.util.Set;
import java.util.concurrent.*;

@Service
public class AgentSynchronizationService {
    @Autowired private AgentRegistryService registry;
    @Lazy @Autowired private NodeRegistryService nodes;
    @Lazy @Autowired private com.transfer.tools.service.MasterLocalTransferService localTransfers;
    private final Set<String> pending = ConcurrentHashMap.newKeySet();
    private final ExecutorService executor = Executors.newFixedThreadPool(2, runnable -> {
        Thread thread = new Thread(runnable, "agent-storage-sync"); thread.setDaemon(true); return thread;
    });
    public void connected(String address) {
        connected(registry.currentSession(address));
    }
    public void connected(AgentRegistryService.Session session) {
        if (!registry.touch(session)) return;
        String address = session.getAddress();
        String enrollment = address + ":" + session.getEnrollmentId();
        if (!registry.needsSynchronization(address) || !pending.add(enrollment)) return;
        executor.execute(() -> {
            long version = registry.storageVersion();
            try {
                nodes.synchronizeAgentStorage(address);
                if (registry.markSynchronized(session, version) && registry.online(session) && localTransfers != null)
                    localTransfers.retryDeferredParents();
            }
            catch (RuntimeException ignored) { /* Retry on a later heartbeat; credentials are never logged. */ }
            finally { pending.remove(enrollment); }
        });
    }
    @PreDestroy public void stop() { executor.shutdownNow(); }
}
