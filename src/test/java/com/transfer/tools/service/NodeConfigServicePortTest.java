package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.NodeConfig;
import com.transfer.tools.storage.RocksDBService;
import org.junit.Test;
import org.mockito.ArgumentCaptor;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.Arrays;
import java.util.Collections;
import java.util.LinkedHashMap;

import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class NodeConfigServicePortTest {
    private NodeConfig saved(Integer port) {
        NodeConfig saved = new NodeConfig();
        saved.setPort(port);
        saved.setMasterIp("10.0.0.10");
        saved.setLocalIp("10.0.0.11");
        saved.setWorkerIps(Arrays.asList("10.0.0.10", "10.0.0.11", "10.0.0.12"));
        saved.setAdvertisedLocalRoots(Collections.singletonList("/data/shared"));
        saved.setNodeLabels(new LinkedHashMap<>(Collections.singletonMap("10.0.0.12", "retained label")));
        return saved;
    }

    private DistributedConfig configured() {
        DistributedConfig config = new DistributedConfig();
        config.setPort(18443);
        config.setMasterIp("192.0.2.10");
        config.setLocalIp("192.0.2.11");
        config.setWorkerIps(Collections.singletonList("192.0.2.11"));
        return config;
    }

    private NodeConfigService service(RocksDBService database, DistributedConfig config) {
        NodeConfigService service = new NodeConfigService();
        ReflectionTestUtils.setField(service, "rocksDBService", database);
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        ReflectionTestUtils.setField(config, "nodeConfigService", service);
        service.init();
        return service;
    }

    @Test public void restartUpdatesOnlyThePortAndPreservesDurableMembershipAndIdentity() {
        NodeConfig original = saved(1685);
        RocksDBService database = mock(RocksDBService.class);
        when(database.get("node_config", NodeConfig.class)).thenReturn(original);
        DistributedConfig config = configured();
        NodeConfigService service = service(database, config);
        assertEquals(Integer.valueOf(18443), service.getPort());
        assertEquals(Integer.valueOf(18443), config.getPort());
        assertEquals("http://10.0.0.10:18443", config.getMasterWebUrl());
        assertEquals(original.getMasterIp(), service.getMasterIp());
        assertEquals(original.getLocalIp(), service.getLocalIp());
        assertEquals(original.getWorkerIps(), service.getWorkerIps());
        assertEquals(original.getAdvertisedLocalRoots(), service.getAdvertisedLocalRoots());
        assertEquals(original.getNodeLabels(), service.get().getNodeLabels());
        assertEquals(Integer.valueOf(1685), original.getPort());
        ArgumentCaptor<NodeConfig> persisted = ArgumentCaptor.forClass(NodeConfig.class);
        verify(database).putSync(eq("node_config"), persisted.capture());
        assertEquals(service.get(), persisted.getValue());
    }

    @Test public void missingHistoricalPortIsRepairedWithoutReplacingMembership() {
        RocksDBService database = mock(RocksDBService.class);
        NodeConfig original = saved(null);
        when(database.get("node_config", NodeConfig.class)).thenReturn(original);
        NodeConfigService service = service(database, configured());
        assertEquals(Integer.valueOf(18443), service.getPort());
        assertEquals(original.getWorkerIps(), service.getWorkerIps());
        verify(database).putSync(eq("node_config"), any(NodeConfig.class));
    }

    @Test public void unchangedPortDoesNotRewriteTheStoredConfiguration() {
        RocksDBService database = mock(RocksDBService.class);
        when(database.get("node_config", NodeConfig.class)).thenReturn(saved(18443));
        NodeConfigService service = service(database, configured());
        assertEquals(Integer.valueOf(18443), service.getPort());
        verify(database, never()).putSync(eq("node_config"), any(NodeConfig.class));
    }

    @Test public void firstBootUsesConfiguredPortAndAddresses() {
        RocksDBService database = mock(RocksDBService.class);
        DistributedConfig config = configured();
        NodeConfigService service = service(database, config);
        assertEquals(Integer.valueOf(18443), service.getPort());
        assertEquals("192.0.2.10", service.getMasterIp());
        assertEquals(Collections.singletonList("192.0.2.11"), service.getWorkerIps());
        verify(database).putSync(eq("node_config"), any(NodeConfig.class));
    }
}
