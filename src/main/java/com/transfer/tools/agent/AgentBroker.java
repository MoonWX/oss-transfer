package com.transfer.tools.agent;

import org.springframework.stereotype.Service;
import javax.annotation.PreDestroy;
import java.io.IOException;
import java.util.*;
import java.util.concurrent.*;

/** Requests live only until their caller's deadline and are never replayed after delivery. */
@Service
public class AgentBroker {
    private final Map<String, BlockingQueue<RpcCommand>> queues = new ConcurrentHashMap<>();
    private final Map<String, Pending> pending = new ConcurrentHashMap<>();
    private final Semaphore capacity = new Semaphore(4096);
    private final Semaphore payloadCapacity = new Semaphore(48 * 1024 * 1024);
    private volatile boolean closed;

    public RpcResponse call(String address, String method, String path, byte[] body,
                            String contentType, long timeoutMs) throws IOException {
        AgentRpcPolicy.requireAllowed(method, path);
        AgentRpcPolicy.requireSize(body);
        if (address == null || address.isEmpty()) throw new IllegalArgumentException("Agent address is required");
        if (closed || !capacity.tryAcquire()) throw new IOException("Agent request queue is unavailable");
        int payloadCost = (body == null ? 0 : body.length * 3) + 4096;
        if (!payloadCapacity.tryAcquire(payloadCost)) {
            capacity.release();
            throw new IOException("Agent request payload queue is full");
        }
        long boundedTimeout = Math.min(120000, Math.max(100, timeoutMs));
        RpcCommand command = new RpcCommand();
        command.setId(UUID.randomUUID().toString());
        command.setMethod(method);
        command.setPath(path);
        command.setBody(Base64.getEncoder().encodeToString(body == null ? new byte[0] : body));
        command.setContentType(contentType == null ? "application/json" : contentType);
        command.setDeadline(System.currentTimeMillis() + boundedTimeout);
        Pending request = new Pending(address);
        BlockingQueue<RpcCommand> queue = queues.computeIfAbsent(address, key -> new ArrayBlockingQueue<>(256));
        pending.put(command.getId(), request);
        try {
            if (!queue.offer(command)) throw new IOException("Agent request queue is full");
            return request.result.get(boundedTimeout, TimeUnit.MILLISECONDS);
        } catch (TimeoutException e) {
            throw new IOException("Agent did not respond before the deadline; command outcome may be unknown", e);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            throw new IOException("Agent request interrupted; command outcome may be unknown", e);
        } catch (ExecutionException e) {
            throw new IOException("Agent request failed", e.getCause());
        } finally {
            pending.remove(command.getId(), request);
            queue.remove(command);
            capacity.release();
            payloadCapacity.release(payloadCost);
        }
    }

    public RpcCommand poll(String address, long waitMs) throws InterruptedException {
        if (closed) return null;
        BlockingQueue<RpcCommand> queue = queues.computeIfAbsent(address, key -> new ArrayBlockingQueue<>(256));
        long deadline = System.currentTimeMillis() + Math.min(25000, Math.max(0, waitMs));
        do {
            RpcCommand command = queue.poll(Math.max(0, deadline - System.currentTimeMillis()), TimeUnit.MILLISECONDS);
            if (command == null) return null;
            Pending request = pending.get(command.getId());
            if (request != null && address.equals(request.address) && command.getDeadline() > System.currentTimeMillis())
                return command;
        } while (System.currentTimeMillis() < deadline);
        return null;
    }

    public boolean complete(String address, RpcResult result) {
        if (result == null || result.getId() == null) throw new IllegalArgumentException("Agent command ID is required");
        Pending request = pending.get(result.getId());
        if (request == null) return false;
        if (!request.address.equals(address)) throw new SecurityException("Agent command belongs to another node");
        if (result.getStatus() < 100 || result.getStatus() > 599) throw new IllegalArgumentException("Invalid Agent response status");
        byte[] body = AgentRpcPolicy.decode(result.getBody());
        return request.result.complete(new RpcResponse(result.getStatus(), body,
                AgentRpcPolicy.responseHeaders(result.getHeaders())));
    }

    public int pendingCount() { return pending.size(); }

    @PreDestroy public void close() {
        closed = true;
        pending.values().forEach(request -> request.result.completeExceptionally(new IOException("Master is stopping")));
        queues.clear();
    }

    private static final class Pending {
        final String address;
        final CompletableFuture<RpcResponse> result = new CompletableFuture<>();
        Pending(String address) { this.address = address; }
    }
}
