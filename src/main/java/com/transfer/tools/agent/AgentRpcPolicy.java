package com.transfer.tools.agent;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.util.*;

/** Only application APIs, never arbitrary URLs or operating system commands. */
public final class AgentRpcPolicy {
    public static final int MAX_BYTES = 8 * 1024 * 1024;
    public static final int MAX_ENCODED_BYTES = 4 * ((MAX_BYTES + 2) / 3);
    private static final Set<String> GET = new HashSet<>(Arrays.asList(
            "node-info", "node-config", "worker-status", "task-status", "pending-tasks",
            "failed-tasks", "status", "deleted-tasks", "rate-limit", "rate-limit/adjustment",
            "rate-limit/history", "s3-config", "local-capabilities", "local-transfers/status",
            "local-transfers/failures", "task-bandwidth-policy", "task-children"));
    private static final Set<String> POST = new HashSet<>(Arrays.asList(
            "start", "stop", "resume", "clear", "permanent-delete", "retry-task", "tasks",
            "failed", "failed/clear", "fallback", "fallback/clear", "s3-config",
            "local-transfers", "local-transfers/validate", "local-transfers/discover",
            "local-transfers/batches", "local-transfers/stop", "local-transfers/resume",
            "local-transfers/retry-failures"));
    private static final Set<String> READY_POST = new HashSet<>(Arrays.asList(
            "start", "resume", "retry-task", "tasks", "local-transfers", "local-transfers/discover",
            "local-transfers/batches", "local-transfers/resume", "local-transfers/retry-failures"));
    private AgentRpcPolicy() { }

    public static boolean requiresReady(String method, String path) {
        if (!"POST".equals(method) || path == null || !path.startsWith("/api/worker/")) return false;
        String operation = path.substring("/api/worker/".length()).split("\\?", 2)[0];
        return READY_POST.contains(operation);
    }

    public static void requireAllowed(String method, String path) {
        if (method == null || path == null || path.length() > 16384 || path.indexOf('\\') >= 0
                || path.indexOf('#') >= 0 || path.indexOf('\r') >= 0 || path.indexOf('\n') >= 0)
            throw new IllegalArgumentException("Invalid Agent API request");
        URI uri;
        try { uri = URI.create(path); } catch (IllegalArgumentException e) { throw new IllegalArgumentException("Invalid Agent API path"); }
        if (uri.isAbsolute() || uri.getRawAuthority() != null || !path.startsWith("/api/worker/")
                || !uri.getRawPath().equals(uri.getPath()) || uri.getPath().contains(".."))
            throw new IllegalArgumentException("Agent API path is not permitted");
        String operation = uri.getPath().substring("/api/worker/".length());
        boolean allowed = ("GET".equals(method) && (GET.contains(operation)
                || operation.matches("task-reports/[0-9]+/(scope|files)")))
                || ("POST".equals(method) && (POST.contains(operation)
                || operation.matches("rate-limit/(list|head|put|bandwidth)")))
                || ("PUT".equals(method) && ("node-config".equals(operation)
                || "task-bandwidth-policy".equals(operation) || operation.matches("s3-config/[^/]+")))
                || ("DELETE".equals(method) && operation.matches("s3-config/[^/]+"));
        if (!allowed) throw new IllegalArgumentException("Agent API method or path is not permitted");
    }

    public static byte[] decode(String value) {
        if (value == null || value.isEmpty()) return new byte[0];
        if (value.length() > MAX_ENCODED_BYTES) throw new IllegalArgumentException("Agent API payload exceeds 8 MiB");
        byte[] decoded;
        try { decoded = Base64.getDecoder().decode(value); }
        catch (IllegalArgumentException e) { throw new IllegalArgumentException("Invalid Agent API payload encoding"); }
        requireSize(decoded);
        return decoded;
    }

    public static void requireSize(byte[] bytes) {
        if (bytes != null && bytes.length > MAX_BYTES) throw new IllegalArgumentException("Agent API payload exceeds 8 MiB");
    }

    public static byte[] readBounded(InputStream input) throws IOException {
        return read(input, MAX_BYTES);
    }

    public static byte[] readProtocolBounded(InputStream input) throws IOException {
        return read(input, MAX_ENCODED_BYTES + 16384);
    }

    private static byte[] read(InputStream input, int limit) throws IOException {
        if (input == null) return new byte[0];
        ByteArrayOutputStream output = new ByteArrayOutputStream();
        byte[] buffer = new byte[8192];
        int count;
        while ((count = input.read(buffer)) != -1) {
            if (output.size() + count > limit) throw new IOException("Agent API response exceeds payload limit");
            output.write(buffer, 0, count);
        }
        return output.toByteArray();
    }

    public static Map<String, String> responseHeaders(Map<String, String> headers) {
        Map<String, String> safe = new LinkedHashMap<>();
        if (headers != null) headers.forEach((key, value) -> {
            if (("Content-Type".equalsIgnoreCase(key) || "Cache-Control".equalsIgnoreCase(key))
                    && value != null && value.length() <= 1024 && value.indexOf('\r') < 0 && value.indexOf('\n') < 0)
                safe.put(key, value);
        });
        return safe;
    }
}
