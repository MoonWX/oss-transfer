package com.transfer.tools.agent;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.service.NodeConfigService;
import com.transfer.tools.storage.RocksDBService;
import org.junit.Before;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;

public class AgentEnrollmentTest {
    private final Map<String, Object> durable = new HashMap<>();
    private AgentRegistryService registry;
    private NodeConfigService nodes;
    @Before public void setup() {
        registry = new AgentRegistryService(); nodes = mock(NodeConfigService.class);
        RocksDBService db = mock(RocksDBService.class); DistributedConfig config = mock(DistributedConfig.class);
        when(config.isMaster()).thenReturn(true); when(config.getMasterWebUrl()).thenReturn("https://transfer.example.com");
        when(nodes.getWorkerIps()).thenReturn(Collections.singletonList("10.0.0.1"));
        when(nodes.withMembershipWriteLock(any())).thenAnswer(call -> ((java.util.concurrent.Callable<?>)call.getArgument(0)).call());
        when(db.get(anyString(), any())).thenAnswer(call -> durable.get(call.getArgument(0)));
        doAnswer(call -> { durable.put(call.getArgument(0), call.getArgument(1)); return null; }).when(db).putSync(anyString(), any());
        doAnswer(call -> { durable.putAll(call.getArgument(2)); return null; }).when(nodes).registerAgent(anyString(), anyString(), anyMap());
        ReflectionTestUtils.setField(registry, "db", db); ReflectionTestUtils.setField(registry, "config", config);
        ReflectionTestUtils.setField(registry, "nodes", nodes);
    }
    private String invite(String address) {
        AgentRegistryService.InviteRequest request = new AgentRegistryService.InviteRequest(); request.setAddress(address);
        Map<String, Object> result = registry.invite(request); String url = (String)result.get("joinUrl");
        return url.substring(url.lastIndexOf('/') + 1);
    }
    private AgentRegistryService.JoinRequest join(String token) {
        AgentRegistryService.JoinRequest request = new AgentRegistryService.JoinRequest();
        request.setToken(token); request.setAddress("10.0.0.2"); request.setInstanceId(UUID.randomUUID().toString());
        request.setCredential(AgentRegistryService.randomSecret()); return request;
    }
    @Test public void consumedInvitationAllowsOnlyTheSameDurableRetry() {
        String token = invite("10.0.0.2"); AgentRegistryService.JoinRequest request = join(token);
        assertEquals(true, registry.join(request).get("success"));
        assertEquals(true, registry.join(request).get("success"));
        assertEquals("10.0.0.2", registry.authenticatedAddress(request.getCredential()));
        try { registry.join(join(token)); fail(); } catch (IllegalArgumentException expected) { }
        verify(nodes, times(1)).registerAgent(anyString(), anyString(), anyMap());
        for (Object value : durable.values()) assertFalse(value.toString().contains(request.getCredential()));
    }
    @Test public void expiredOrWrongAddressCannotJoin() {
        String token = invite("10.0.0.3");
        try { registry.join(join(token)); fail(); } catch (IllegalArgumentException expected) { }
        AgentRegistryService.Invitation invitation = registry.validInvitation(token); invitation.setExpiresAt(1);
        try { registry.join(join(token)); fail(); } catch (IllegalArgumentException expected) { }
        verify(nodes, never()).registerAgent(anyString(), anyString(), anyMap());
    }
    @Test public void revokedCredentialAndStoredHeartbeatCannotReportOnline() {
        AgentRegistryService.JoinRequest request = join(invite("10.0.0.2")); registry.join(request);
        registry.touch("10.0.0.2"); registry.markSynchronized("10.0.0.2"); assertTrue(registry.online("10.0.0.2"));
        registry.revoke("10.0.0.2"); assertNull(registry.authenticatedAddress(request.getCredential()));
        assertFalse(registry.online("10.0.0.2"));
    }
    @Test public void replacedAddressCannotAuthenticateUsingAnEarlierCredential() {
        AgentRegistryService.JoinRequest old = join(invite("10.0.0.2")); registry.join(old); registry.revoke("10.0.0.2");
        AgentRegistryService.JoinRequest replacement = join(invite("10.0.0.2")); registry.join(replacement);
        assertNull(registry.authenticatedAddress(old.getCredential()));
        assertEquals("10.0.0.2", registry.authenticatedAddress(replacement.getCredential()));
    }
    @Test public void shellLabelsAreQuotedAndControlCharactersRejected() {
        AgentRegistryService.InviteRequest request = new AgentRegistryService.InviteRequest(); request.setLabel("node' $(id)");
        String command = (String) registry.invite(request).get("dockerCommand");
        assertTrue(command.contains("'node'\"'\"' $(id)'"));
        request.setLabel("node\nroot");
        try { registry.invite(request); fail(); } catch (IllegalArgumentException expected) { }
    }
    @Test public void dockerInvitationDefaultsToTheLatestPublishedImage() {
        AgentRegistryService.InviteRequest request = new AgentRegistryService.InviteRequest();
        String command = (String) registry.invite(request).get("dockerCommand");
        assertTrue(command.endsWith("moonwx/osstransfer:latest"));
        assertTrue(command.contains("TRANSFER_ROLE=agent"));
        assertTrue(command.contains("TRANSFER_AGENT_MASTER_URL='https://transfer.example.com'"));
        assertTrue(command.contains("TRANSFER_AGENT_JOIN_TOKEN="));
    }
    @Test public void agentOriginAndAddressRejectCredentialsTraversalAndLoopback() {
        assertEquals("https://transfer.example.com", AgentIdentityStore.validateOrigin("https://transfer.example.com/"));
        for (String value : Arrays.asList("https://user:pass@host", "http://host/a", "file:///tmp", "http://host/?x=1")) {
            try { AgentIdentityStore.validateOrigin(value); fail(value); } catch (IllegalArgumentException expected) { }
        }
        for (String value : Arrays.asList("127.0.0.1", "01.0.0.2", "10.0.0.256", "0.0.0.0", "224.0.0.1")) {
            try { AgentRegistryService.cleanAddress(value, true); fail(value); } catch (IllegalArgumentException expected) { }
        }
    }
}
