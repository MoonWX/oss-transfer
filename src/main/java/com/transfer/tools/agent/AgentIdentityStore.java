package com.transfer.tools.agent;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.service.NodeConfigService;
import com.transfer.tools.storage.RocksDBService;
import lombok.Data;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.core.env.Environment;
import org.springframework.http.*;
import org.springframework.http.client.SimpleClientHttpRequestFactory;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;
import java.net.URI;
import java.util.*;

/** Agent credentials and pending enrollment survive both crashes and lost join responses. */
@Service
public class AgentIdentityStore {
    private static final String KEY = "agent:identity";
    @Autowired private RocksDBService db;
    @Autowired private Environment env;
    @Autowired private DistributedConfig config;
    @Autowired private NodeConfigService nodes;
    private volatile Identity identity;
    @Data public static class Identity {
        private String masterUrl, address, credential, instanceId;
        private boolean joined;
    }
    public synchronized void bootstrap() {
        if (!config.isAgent()) return;
        if (identity == null) identity = db.get(KEY, Identity.class);
        String master = validateOrigin(env.getProperty("transfer.agent.master-url", ""));
        String address = AgentRegistryService.cleanAddress(env.getProperty("transfer.agent.address", ""), true);
        if (identity != null && (!master.equals(identity.masterUrl) || !address.equals(identity.address)))
            throw new IllegalStateException("Persisted Agent identity differs from deployment settings; use a new data directory to enroll a different node");
        if (identity == null) {
            Identity pending = new Identity(); pending.masterUrl = master; pending.address = address;
            pending.instanceId = UUID.randomUUID().toString(); pending.credential = AgentRegistryService.randomSecret();
            db.putSync(KEY, pending); identity = pending;
        }
        if (identity.joined) return;
        String joinToken = env.getProperty("transfer.agent.join-token", "");
        if (joinToken.isEmpty()) throw new IllegalStateException("Agent has not joined; transfer.agent.join-token is required");
        Map<String, Object> request = new LinkedHashMap<>();
        request.put("token", joinToken); request.put("instanceId", identity.instanceId);
        request.put("credential", identity.credential); request.put("address", address);
        SimpleClientHttpRequestFactory factory = new SimpleClientHttpRequestFactory() {
            @Override protected void prepareConnection(java.net.HttpURLConnection connection, String method) throws java.io.IOException {
                super.prepareConnection(connection, method); connection.setInstanceFollowRedirects(false);
            }
        };
        factory.setConnectTimeout(5000); factory.setReadTimeout(10000);
        RestTemplate http = new RestTemplate(factory);
        Map response = http.postForObject(master + "/api/agents/join", request, Map.class);
        if (response == null || !Boolean.TRUE.equals(response.get("success")) || !address.equals(response.get("address")))
            throw new IllegalStateException("Master did not acknowledge enrollment");
        identity.joined = true; db.putSync(KEY, identity);
    }
    public boolean isReady() { Identity value = identity; return value != null && value.joined; }
    public String getCredential() { return identity == null ? null : identity.credential; }
    public String getMasterUrl() { return identity == null ? env.getProperty("transfer.agent.master-url") : identity.masterUrl; }
    public String getAddress() { return identity == null ? env.getProperty("transfer.agent.address") : identity.address; }
    static String validateOrigin(String value) {
        try {
            URI uri = URI.create(value);
            if (!("http".equals(uri.getScheme()) || "https".equals(uri.getScheme())) || uri.getHost() == null
                    || uri.getUserInfo() != null || uri.getQuery() != null || uri.getFragment() != null
                    || uri.getPort() == 0 || uri.getPort() > 65535 || !(uri.getPath().isEmpty() || uri.getPath().equals("/")))
                throw new IllegalArgumentException();
            return uri.getScheme() + "://" + uri.getRawAuthority();
        } catch (Exception e) { throw new IllegalArgumentException("Agent Master URL must be an HTTP(S) origin"); }
    }
}
