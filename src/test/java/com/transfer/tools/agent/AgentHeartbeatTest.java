package com.transfer.tools.agent;

import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.Map;
import static org.junit.Assert.*;

public class AgentHeartbeatTest {
    @Test @SuppressWarnings("unchecked") public void heartbeatAvailabilityIsDistinctFromStorageReadiness() {
        AgentRegistryService registry = new AgentRegistryService();
        Map<String, AgentRegistryService.AgentRecord> agents =
                (Map<String, AgentRegistryService.AgentRecord>)ReflectionTestUtils.getField(registry, "agents");
        AgentRegistryService.AgentRecord record = new AgentRegistryService.AgentRecord();
        record.setAddress("10.0.0.2"); record.setLastSeen(System.currentTimeMillis());
        agents.put(record.getAddress(), record);
        assertTrue(registry.connected(record.getAddress()));
        assertFalse("An unsynchronized Agent can receive its synchronization commands", registry.online(record.getAddress()));
        record.setSynchronizedStorage(true);
        assertTrue(registry.online(record.getAddress()));
        record.setLastSeen(0);
        assertFalse(registry.connected(record.getAddress())); assertFalse(registry.online(record.getAddress()));
        record.setLastSeen(System.currentTimeMillis() - 65001);
        assertFalse(registry.connected(record.getAddress()));
        record.setLastSeen(System.currentTimeMillis()); record.setEnabled(false);
        assertTrue("Revocation must not switch the transport to legacy HTTP", registry.isAgent(record.getAddress()));
        assertFalse(registry.connected(record.getAddress())); assertFalse(registry.online(record.getAddress()));
        assertFalse(registry.connected("10.0.0.99"));
    }
}
