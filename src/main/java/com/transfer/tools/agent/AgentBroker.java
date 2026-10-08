package com.transfer.tools.agent;

import org.springframework.stereotype.Service;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.Lazy;
import javax.annotation.PreDestroy;
import java.io.IOException;
import java.util.*;
import java.util.concurrent.*;

/** Requests live only until their caller's deadline and are never replayed after delivery. */
@Service
public class AgentBroker {
    @Autowired @Lazy private AgentRegistryService registry;
    private final Map<String, Lane> lanes = new ConcurrentHashMap<>();
    private final Map<String, Pending> pending = new ConcurrentHashMap<>();
    private final Semaphore capacity = new Semaphore(4096);
    private final Semaphore payloadCapacity = new Semaphore(48 * 1024 * 1024);
    private volatile boolean closed;

    public RpcResponse call(String address, String method, String path, byte[] body,
                            String contentType, long timeoutMs) throws IOException {
        AgentRpcPolicy.requireAllowed(method, path);
        AgentRpcPolicy.requireSize(body);
        if (address == null || address.isEmpty()) throw new IllegalArgumentException("Agent address is required");
        String enrollmentId = enrollment(address);
        if (enrollmentId == null) throw new AgentUnavailableException("Agent enrollment is unavailable");
        // Never acquire the registry monitor while holding a lane: revocation uses registry -> lane order.
        Lane lane = lane(address, enrollmentId);
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
        Pending request = new Pending(lane);
        try {
            synchronized (lane) {
                if (closed || !lane.active) throw new AgentUnavailableException("Agent enrollment was revoked before delivery");
                pending.put(command.getId(), request);
                if (!lane.queue.offer(command)) throw new IOException("Agent request queue is full");
            }
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
            lane.queue.remove(command);
            capacity.release();
            payloadCapacity.release(payloadCost);
        }
    }

    public RpcCommand poll(String address, long waitMs) throws InterruptedException {
        String enrollmentId = enrollment(address);
        return enrollmentId == null ? null : poll(address, enrollmentId, waitMs);
    }

    public RpcCommand poll(String address, String enrollmentId, long waitMs) throws InterruptedException {
        if (closed) return null;
        Lane lane = lane(address, enrollmentId);
        long deadline = System.currentTimeMillis() + Math.min(25000, Math.max(0, waitMs));
        do {
            if (!lane.active || closed) return null;
            // The sentinel wakes one poll immediately; other concurrent polls observe revocation within one second.
            RpcCommand command = lane.queue.poll(Math.min(1000, Math.max(0, deadline - System.currentTimeMillis())), TimeUnit.MILLISECONDS);
            synchronized (lane) {
                if (!lane.active || closed) return null;
                Pending request = command == null || command.getId() == null ? null : pending.get(command.getId());
                if (request != null && request.lane == lane && !request.result.isDone()
                        && command.getDeadline() > System.currentTimeMillis()) {
                    request.delivered = true;
                    return command;
                }
            }
        } while (System.currentTimeMillis() < deadline);
        return null;
    }

    public boolean complete(String address, RpcResult result) {
        return complete(address, enrollment(address), result);
    }

    public boolean complete(String address, String enrollmentId, RpcResult result) {
        if (result == null || result.getId() == null) throw new IllegalArgumentException("Agent command ID is required");
        Pending request = pending.get(result.getId());
        if (request == null) return false;
        if (!request.lane.address.equals(address) || !request.lane.enrollmentId.equals(enrollmentId))
            throw new SecurityException("Agent command belongs to another enrollment");
        if (result.getStatus() < 100 || result.getStatus() > 599) throw new IllegalArgumentException("Invalid Agent response status");
        byte[] body = AgentRpcPolicy.decode(result.getBody());
        synchronized (request.lane) {
            if (!request.lane.active || !request.delivered) return false;
            return request.result.complete(new RpcResponse(result.getStatus(), body,
                    AgentRpcPolicy.responseHeaders(result.getHeaders())));
        }
    }

    /** Keep a closed enrollment lane as a tombstone against calls that took a snapshot before revocation. */
    public void invalidate(String address, String enrollmentId) {
        if (enrollmentId == null) return;
        Lane lane = lane(address, enrollmentId);
        synchronized (lane) {
            lane.active = false;
            pending.values().stream().filter(request -> request.lane == lane).forEach(request ->
                    request.result.completeExceptionally(request.delivered
                            ? new IOException("Agent enrollment was revoked after delivery; command outcome may be unknown")
                            : new AgentUnavailableException("Agent enrollment was revoked before delivery")));
            lane.queue.clear();
            lane.queue.offer(new RpcCommand());
        }
    }

    private String enrollment(String address) { return registry == null ? "legacy" : registry.currentEnrollment(address); }
    private Lane lane(String address, String enrollmentId) {
        if (address == null || enrollmentId == null) throw new IllegalArgumentException("Agent enrollment is required");
        return lanes.computeIfAbsent(address + ":" + enrollmentId, key -> new Lane(address, enrollmentId));
    }

    public int pendingCount() { return pending.size(); }

    @PreDestroy public void close() {
        closed = true;
        pending.values().forEach(request -> request.result.completeExceptionally(new IOException("Master is stopping")));
        for (Lane lane : lanes.values()) synchronized (lane) {
            lane.active = false; lane.queue.clear(); lane.queue.offer(new RpcCommand());
        }
    }

    private static final class Lane {
        final String address, enrollmentId;
        final BlockingQueue<RpcCommand> queue = new ArrayBlockingQueue<>(256);
        volatile boolean active = true;
        Lane(String address, String enrollmentId) { this.address = address; this.enrollmentId = enrollmentId; }
    }
    private static final class Pending {
        final Lane lane;
        boolean delivered;
        final CompletableFuture<RpcResponse> result = new CompletableFuture<>();
        Pending(Lane lane) { this.lane = lane; }
    }
}
