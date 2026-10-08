package com.transfer.tools.agent;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import org.springframework.beans.factory.ObjectProvider;
import org.springframework.core.env.Environment;
import org.springframework.http.*;
import org.springframework.http.client.*;

import java.io.*;
import java.net.*;
import java.util.*;

/** Routes node control traffic through the outbound Agent channel. Storage traffic is untouched. */
public final class AgentHttpRequestFactory implements ClientHttpRequestFactory {
    private final ClientHttpRequestFactory delegate;
    private final ObjectProvider<AgentRegistryService> registry;
    private final ObjectProvider<AgentBroker> broker;
    private final ObjectProvider<AgentIdentityStore> identity;
    private final ObjectProvider<DistributedConfig> config;
    private final Environment environment;
    private final ObjectMapper mapper = new ObjectMapper();

    public AgentHttpRequestFactory(ClientHttpRequestFactory delegate, ObjectProvider<AgentRegistryService> registry,
                                  ObjectProvider<AgentBroker> broker, ObjectProvider<AgentIdentityStore> identity,
                                  ObjectProvider<DistributedConfig> config, Environment environment) {
        this.delegate = delegate; this.registry = registry; this.broker = broker;
        this.identity = identity; this.config = config; this.environment = environment;
    }

    @Override public ClientHttpRequest createRequest(URI uri, HttpMethod method) throws IOException {
        boolean nodeApi = "http".equalsIgnoreCase(uri.getScheme()) && uri.getUserInfo() == null
                && uri.getHost() != null && uri.getPort() == config.getObject().getPort()
                && uri.getRawPath().startsWith("/api/worker/");
        if (!nodeApi) return delegate.createRequest(uri, method);
        boolean agentRole = "agent".equalsIgnoreCase(environment.getProperty("transfer.role", "master"));
        if (agentRole) {
            String host = uri.getHost();
            if (host.equals(config.getObject().getLocalIp())) {
                URI loopback = URI.create("http://127.0.0.1:" + config.getObject().getPort() + uri.getRawPath()
                        + (uri.getRawQuery() == null ? "" : "?" + uri.getRawQuery()));
                return delegate.createRequest(loopback, method);
            }
            return new RoutedRequest(uri, method, true);
        }
        if (!registry.getObject().isAgent(uri.getHost())) return delegate.createRequest(uri, method);
        requireAvailable(uri.getHost(), method.name(), uri.getRawPath());
        return new RoutedRequest(uri, method, false);
    }

    private void requireAvailable(String address, String method, String path) throws IOException {
        AgentRegistryService value = registry.getObject();
        if (!value.connected(address)) throw new AgentUnavailableException("Agent is offline or its credential has been revoked");
        if (AgentRpcPolicy.requiresReady(method, path) && !value.online(address))
            throw new AgentUnavailableException("Agent storage settings are not synchronized");
    }

    private final class RoutedRequest implements ClientHttpRequest {
        private final URI uri;
        private final HttpMethod method;
        private final boolean relay;
        private final HttpHeaders headers = new HttpHeaders();
        private final ByteArrayOutputStream output = new ByteArrayOutputStream();
        private boolean executed;
        RoutedRequest(URI uri, HttpMethod method, boolean relay) { this.uri = uri; this.method = method; this.relay = relay; }
        @Override public HttpMethod getMethod() { return method; }
        @Override public String getMethodValue() { return method.name(); }
        @Override public URI getURI() { return uri; }
        @Override public HttpHeaders getHeaders() { return headers; }
        @Override public OutputStream getBody() {
            return new OutputStream() {
                @Override public void write(int value) throws IOException { ensure(1); output.write(value); }
                @Override public void write(byte[] bytes, int offset, int count) throws IOException { ensure(count); output.write(bytes, offset, count); }
                private void ensure(int count) throws IOException {
                    if (executed || count > AgentRpcPolicy.MAX_BYTES - output.size()) throw new IOException("Agent API payload exceeds 8 MiB");
                }
            };
        }
        @Override public ClientHttpResponse execute() throws IOException {
            if (executed) throw new IOException("Agent API request already executed");
            executed = true;
            if (!relay) requireAvailable(uri.getHost(), method.name(), uri.getRawPath());
            String path = uri.getRawPath() + (uri.getRawQuery() == null ? "" : "?" + uri.getRawQuery());
            String type = headers.getFirst(HttpHeaders.CONTENT_TYPE);
            RpcResponse response = relay ? relay(uri.getHost(), method.name(), path, output.toByteArray(), type)
                    : broker.getObject().call(uri.getHost(), method.name(), path, output.toByteArray(), type,
                    path.startsWith("/api/worker/task-reports/") ? 60000 : 15000);
            return new AgentResponse(response);
        }
    }

    private RpcResponse relay(String address, String method, String path, byte[] body, String type) throws IOException {
        AgentRpcPolicy.requireAllowed(method, path);
        AgentIdentityStore store = identity.getObject();
        if (!store.isReady()) throw new IOException("Agent has not joined the master");
        RpcRelayRequest request = new RpcRelayRequest();
        request.setAddress(address); request.setMethod(method); request.setPath(path);
        request.setBody(Base64.getEncoder().encodeToString(body)); request.setContentType(type);
        HttpURLConnection connection = (HttpURLConnection) new URL(store.getMasterUrl() + "/api/agents/relay").openConnection();
        connection.setInstanceFollowRedirects(false); connection.setRequestMethod("POST");
        connection.setConnectTimeout(5000); connection.setReadTimeout(25000);
        connection.setRequestProperty("Content-Type", "application/json");
        connection.setRequestProperty("X-Agent-Key", store.getCredential());
        byte[] bytes = mapper.writeValueAsBytes(request);
        try {
            connection.setDoOutput(true); connection.setFixedLengthStreamingMode(bytes.length);
            try (OutputStream stream = connection.getOutputStream()) { stream.write(bytes); }
            if (connection.getResponseCode() != 200) throw new IOException("Master rejected Agent relay with HTTP " + connection.getResponseCode());
            try (InputStream stream = connection.getInputStream()) {
                // RpcResponse uses byte[]; Jackson encodes it as base64 on the wire.
                RpcResponse response = mapper.readValue(AgentRpcPolicy.readProtocolBounded(stream), RpcResponse.class);
                AgentRpcPolicy.requireSize(response.getBody());
                response.setHeaders(AgentRpcPolicy.responseHeaders(response.getHeaders()));
                return response;
            }
        } finally { connection.disconnect(); }
    }

    private static final class AgentResponse implements ClientHttpResponse {
        private final RpcResponse response;
        private final HttpHeaders headers = new HttpHeaders();
        AgentResponse(RpcResponse response) {
            this.response = response;
            AgentRpcPolicy.responseHeaders(response.getHeaders()).forEach(headers::set);
        }
        @Override public HttpStatus getStatusCode() { return HttpStatus.valueOf(response.getStatus()); }
        @Override public int getRawStatusCode() { return response.getStatus(); }
        @Override public String getStatusText() { HttpStatus status = HttpStatus.resolve(response.getStatus()); return status == null ? "" : status.getReasonPhrase(); }
        @Override public void close() { }
        @Override public InputStream getBody() { return new ByteArrayInputStream(response.getBody() == null ? new byte[0] : response.getBody()); }
        @Override public HttpHeaders getHeaders() { return headers; }
    }
}
