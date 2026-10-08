package com.transfer.tools.agent;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;
import org.springframework.boot.context.event.ApplicationReadyEvent;
import org.springframework.context.event.EventListener;
import org.springframework.core.env.Environment;
import org.springframework.beans.factory.ObjectProvider;
import com.transfer.tools.service.LocalTransferService;
import org.springframework.stereotype.Component;

import javax.annotation.PreDestroy;
import java.io.*;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.concurrent.*;

@Component
public class AgentRunner {
    private static final Logger log = LoggerFactory.getLogger(AgentRunner.class);
    private final AgentIdentityStore identity;
    private final DistributedConfig config;
    private final Environment environment;
    private final ObjectMapper mapper;
    private final ObjectProvider<LocalTransferService> recovery;
    private final ExecutorService polling = Executors.newSingleThreadExecutor(r -> daemon(r, "agent-poll"));
    private final ExecutorService execution = Executors.newFixedThreadPool(8, r -> daemon(r, "agent-command"));
    private final Semaphore available = new Semaphore(8);
    private volatile boolean stopped;
    private volatile boolean masterReady;

    public AgentRunner(AgentIdentityStore identity, DistributedConfig config, Environment environment, ObjectMapper mapper,
                       ObjectProvider<LocalTransferService> recovery) {
        this.identity = identity; this.config = config; this.environment = environment; this.mapper = mapper; this.recovery = recovery;
    }

    private static Thread daemon(Runnable work, String name) {
        Thread thread = new Thread(work, name); thread.setDaemon(true); return thread;
    }

    @EventListener(ApplicationReadyEvent.class)
    public void start() {
        if (!"agent".equalsIgnoreCase(environment.getProperty("transfer.role", "master"))) return;
        polling.execute(() -> {
            boolean recovered = false;
            while (!stopped) {
                try {
                    if (!identity.isReady()) identity.bootstrap();
                    if (!identity.isReady()) throw new IOException("Agent identity is not ready");
                    available.acquire();
                    RpcCommand command;
                    try { command = poll(); }
                    catch (Exception e) { available.release(); throw e; }
                    if (command == null) available.release();
                    else execution.execute(() -> {
                            try { execute(command); }
                            finally { available.release(); }
                        });
                    if (!recovered && masterReady) {
                        recovery.getObject().recoverDiscoveries();
                        recovered = true;
                    }
                } catch (InterruptedException e) { Thread.currentThread().interrupt(); return; }
                catch (Exception e) {
                    // Do not log URLs, headers, request bodies or join credentials.
                    log.warn("Agent connection unavailable; retrying in 5 seconds ({})", e.getClass().getSimpleName());
                    try { Thread.sleep(5000); } catch (InterruptedException interrupted) { Thread.currentThread().interrupt(); return; }
                }
            }
        });
    }

    private RpcCommand poll() throws IOException {
        HttpURLConnection connection = master("/api/agents/poll", "GET", 35000);
        try {
            int status = connection.getResponseCode();
            masterReady = "true".equalsIgnoreCase(connection.getHeaderField("X-Agent-Ready"));
            if (status == 204) return null;
            if (status != 200) throw new IOException("Agent poll rejected with HTTP " + status);
            try (InputStream input = connection.getInputStream()) {
                return mapper.readValue(AgentRpcPolicy.readProtocolBounded(input), RpcCommand.class);
            }
        } finally { connection.disconnect(); }
    }

    private void execute(RpcCommand command) {
        RpcResult result = new RpcResult(); result.setId(command.getId());
        try {
            AgentRpcPolicy.requireAllowed(command.getMethod(), command.getPath());
            byte[] request = AgentRpcPolicy.decode(command.getBody());
            long remaining = command.getDeadline() - System.currentTimeMillis();
            if (remaining <= 0) throw new IOException("Agent command expired before execution");
            HttpURLConnection connection = (HttpURLConnection) new URL("http://127.0.0.1:"
                    + config.getPort() + command.getPath()).openConnection();
            connection.setInstanceFollowRedirects(false);
            connection.setRequestMethod(command.getMethod());
            connection.setConnectTimeout((int)Math.min(5000, remaining));
            connection.setReadTimeout((int)Math.min(120000, remaining));
            connection.setRequestProperty("Content-Type", command.getContentType() == null ? "application/json" : command.getContentType());
            connection.setRequestProperty("X-Internal-Token", config.getInternalToken());
            try {
                if (request.length > 0) {
                    connection.setDoOutput(true); connection.setFixedLengthStreamingMode(request.length);
                    try (OutputStream output = connection.getOutputStream()) { output.write(request); }
                }
                result.setStatus(connection.getResponseCode());
                Map<String, String> headers = new LinkedHashMap<>();
                headers.put("Content-Type", connection.getContentType());
                result.setHeaders(AgentRpcPolicy.responseHeaders(headers));
                try (InputStream input = result.getStatus() >= 400 ? connection.getErrorStream() : connection.getInputStream()) {
                    result.setBody(Base64.getEncoder().encodeToString(AgentRpcPolicy.readBounded(input)));
                }
            } finally { connection.disconnect(); }
        } catch (Exception e) {
            result.setStatus(502);
            result.setHeaders(Collections.singletonMap("Content-Type", "application/json"));
            result.setBody(Base64.getEncoder().encodeToString("{\"success\":false,\"message\":\"Agent command failed or expired\"}".getBytes(StandardCharsets.UTF_8)));
            log.warn("Agent command {} failed ({})", command.getId(), e.getClass().getSimpleName());
        }
        // A lost acknowledgement does not execute a mutation again. Late results are harmless.
        for (int attempt = 0; attempt < 3 && !stopped; attempt++) {
            try { sendResult(result); return; }
            catch (IOException e) {
                if (attempt == 2) log.warn("Agent command {} result could not be acknowledged", command.getId());
                else try { Thread.sleep(500); } catch (InterruptedException interrupted) { Thread.currentThread().interrupt(); return; }
            }
        }
    }

    private void sendResult(RpcResult result) throws IOException {
        HttpURLConnection connection = master("/api/agents/result", "POST", 10000);
        byte[] bytes = mapper.writeValueAsBytes(result);
        try {
            connection.setDoOutput(true); connection.setFixedLengthStreamingMode(bytes.length);
            try (OutputStream output = connection.getOutputStream()) { output.write(bytes); }
            int status = connection.getResponseCode();
            if (status < 200 || status >= 300) throw new IOException("Agent result rejected with HTTP " + status);
        } finally { connection.disconnect(); }
    }

    private HttpURLConnection master(String path, String method, int timeout) throws IOException {
        HttpURLConnection connection = (HttpURLConnection) new URL(identity.getMasterUrl() + path).openConnection();
        connection.setInstanceFollowRedirects(false); connection.setRequestMethod(method);
        connection.setConnectTimeout(5000); connection.setReadTimeout(timeout);
        connection.setRequestProperty("Content-Type", "application/json");
        connection.setRequestProperty("X-Agent-Key", identity.getCredential());
        return connection;
    }

    @PreDestroy public void stop() {
        stopped = true; polling.shutdownNow(); execution.shutdownNow();
    }
}
