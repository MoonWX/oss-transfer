package com.transfer.tools.agent;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.service.NodeConfigService;
import com.transfer.tools.storage.RocksDBService;
import lombok.Data;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.context.annotation.Lazy;
import org.springframework.stereotype.Service;
import javax.annotation.PostConstruct;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.SecureRandom;
import java.time.Instant;
import java.util.*;
import java.util.concurrent.ConcurrentHashMap;

/** One-use bootstrap invitations and independently revocable Agent identities. */
@Service
public class AgentRegistryService {
    static final long INVITATION_MS = 30 * 60 * 1000L;
    @Autowired private RocksDBService db;
    @Autowired private NodeConfigService nodes;
    @Autowired private DistributedConfig config;
    @Autowired @Lazy private AgentBroker broker;
    private final Map<String, AgentRecord> agents = new ConcurrentHashMap<>();
    private final Map<String, String> keys = new ConcurrentHashMap<>();
    private long storageVersion;
    @Data public static class Invitation {
        private String id, address, label, instanceId;
        private long expiresAt;
        private boolean consumed;
    }
    @Data public static class AgentRecord {
        private String instanceId, enrollmentId, address, label, credentialHash;
        private long joinedAt;
        private volatile long lastSeen;
        private volatile boolean enabled = true;
        private volatile boolean synchronizedStorage;
    }
    @Data public static class InviteRequest { private String address, label; }
    @Data public static class JoinRequest { private String token, instanceId, address, label, credential; }

    /** Master-owned enrollment identity; an address may be reused after revocation. */
    public static final class Session {
        private final String address, enrollmentId;
        private Session(String address, String enrollmentId) { this.address = address; this.enrollmentId = enrollmentId; }
        public String getAddress() { return address; }
        public String getEnrollmentId() { return enrollmentId; }
    }

    @PostConstruct public void init() {
        Long savedVersion = db.get("agent:storage-version", Long.class);
        storageVersion = savedVersion == null ? 0 : savedVersion;
        for (AgentRecord record : db.scanByPrefix("agent:member:", AgentRecord.class)) {
            if (record.enrollmentId == null || record.enrollmentId.isEmpty()) {
                record.enrollmentId = UUID.randomUUID().toString();
                db.putSync("agent:member:" + record.address, record);
            }
            record.lastSeen = 0; record.synchronizedStorage = false;
            agents.put(record.address, record);
            if (record.enabled) keys.put(record.credentialHash, record.address);
        }
    }
    public synchronized Map<String, Object> invite(InviteRequest request) {
        requireMaster();
        String address = cleanAddress(request == null ? null : request.address, false);
        String label = cleanLabel(request == null ? null : request.label);
        if (!address.isEmpty() && nodes.getWorkerIps().contains(address))
            throw new IllegalArgumentException("Node address is already registered");
        String token = randomSecret();
        Invitation invitation = new Invitation();
        invitation.id = UUID.randomUUID().toString(); invitation.address = address; invitation.label = label;
        invitation.expiresAt = System.currentTimeMillis() + INVITATION_MS;
        db.putSync("agent:invite:" + hash(token), invitation);
        String origin = config.getMasterWebUrl();
        String link = origin + "/api/agents/bootstrap/" + token;
        String identity = address.isEmpty() ? "$(hostname -I | awk '{print $1}')" : address;
        String docker = "docker run -d --name oss-transfer-agent --restart unless-stopped"
                + " -e TRANSFER_ROLE=agent -e TRANSFER_AGENT_MASTER_URL=" + quote(origin)
                + " -e TRANSFER_AGENT_JOIN_TOKEN=" + quote(token)
                + " -e TRANSFER_AGENT_ADDRESS=" + (address.isEmpty() ? "\"" + identity + "\"" : quote(identity))
                + " -e TRANSFER_AGENT_LABEL=" + quote(label)
                + " -v oss-transfer-agent-data:/app/data moonwx/osstransfer:" + releaseVersion();
        Map<String, Object> response = new LinkedHashMap<>();
        response.put("success", true); response.put("id", invitation.id);
        response.put("expiresAt", Instant.ofEpochMilli(invitation.expiresAt).toString());
        response.put("joinUrl", link); response.put("dockerCommand", docker);
        response.put("traditionalCommand", "(umask 077; script=$(mktemp); trap 'rm -f \"$script\"' EXIT; curl -fsSL "
                + quote(link) + " -o \"$script\" && sudo bash \"$script\")");
        return response;
    }
    public Map<String, Object> join(JoinRequest request) {
        // Membership mutations always acquire the membership lock before this registry's monitor.
        return nodes.withMembershipWriteLock(() -> {
            synchronized (this) { return joinLocked(request); }
        });
    }
    private Map<String, Object> joinLocked(JoinRequest request) {
        requireMaster();
        if (request == null || request.token == null) throw new IllegalArgumentException("Join token is required");
        String tokenHash = hash(request.token);
        Invitation invitation = db.get("agent:invite:" + tokenHash, Invitation.class);
        if (invitation == null) throw new IllegalArgumentException("Join invitation is invalid or expired");
        if (request.instanceId == null || !request.instanceId.matches("[a-zA-Z0-9-]{16,64}"))
            throw new IllegalArgumentException("Invalid Agent instance ID");
        if (request.credential == null || !request.credential.matches("[A-Za-z0-9_-]{43,128}"))
            throw new IllegalArgumentException("Invalid Agent credential");
        String address = cleanAddress(request.address, true);
        String credentialHash = hash(request.credential);
        AgentRecord existing = agents.get(address);
        if (invitation.consumed) {
            // A lost HTTP response may be retried only by the same durable, authenticated instance.
            if (existing != null && existing.enabled && existing.instanceId.equals(request.instanceId)
                    && existing.credentialHash.equals(credentialHash) && request.instanceId.equals(invitation.instanceId))
                return joined(existing);
            throw new IllegalArgumentException("Join invitation has already been used");
        }
        if (invitation.expiresAt <= System.currentTimeMillis())
            throw new IllegalArgumentException("Join invitation has expired");
        if (!invitation.address.isEmpty() && !invitation.address.equals(address))
            throw new IllegalArgumentException("Agent address does not match the invitation");
        if (keys.containsKey(credentialHash)) throw new IllegalArgumentException("Agent credential is already registered");
        AgentRecord record = new AgentRecord();
        record.address = address; record.instanceId = request.instanceId;
        record.enrollmentId = UUID.randomUUID().toString();
        record.label = invitation.label; record.credentialHash = credentialHash;
        record.joinedAt = System.currentTimeMillis();
        invitation.consumed = true; invitation.instanceId = request.instanceId;
        Map<String, Object> changes = new LinkedHashMap<>();
        changes.put("agent:invite:" + tokenHash, invitation); changes.put("agent:member:" + address, record);
        nodes.registerAgent(address, record.label, changes);
        agents.put(address, record); keys.put(credentialHash, address);
        return joined(record);
    }
    private Map<String, Object> joined(AgentRecord record) {
        Map<String, Object> result = new LinkedHashMap<>();
        result.put("success", true); result.put("address", record.address);
        result.put("masterUrl", config.getMasterWebUrl()); result.put("instanceId", record.instanceId);
        return result;
    }
    public synchronized Session authenticatedSession(String credential) {
        if (credential == null || credential.length() > 128 || credential.length() < 43) return null;
        String credentialHash = hash(credential);
        String address = keys.get(credentialHash);
        AgentRecord record = address == null ? null : agents.get(address);
        return record != null && record.enabled && credentialHash.equals(record.credentialHash)
                ? new Session(address, record.enrollmentId) : null;
    }
    public String authenticatedAddress(String credential) {
        Session session = authenticatedSession(credential);
        return session == null ? null : session.address;
    }
    public synchronized boolean isSessionValid(String credential, Session session) {
        Session current = authenticatedSession(credential);
        return session != null && current != null && session.address.equals(current.address)
                && Objects.equals(session.enrollmentId, current.enrollmentId);
    }
    public synchronized String currentEnrollment(String address) {
        AgentRecord record = agents.get(address);
        return record != null && record.enabled ? record.enrollmentId : null;
    }
    public synchronized Session currentSession(String address) {
        AgentRecord record = agents.get(address);
        return record != null && record.enabled ? new Session(address, record.enrollmentId) : null;
    }
    private boolean active(Session session) {
        AgentRecord record = session == null ? null : agents.get(session.address);
        return record != null && record.enabled && Objects.equals(session.enrollmentId, record.enrollmentId);
    }
    public boolean isAgent(String address) { return agents.containsKey(address); }
    /** A control connection can synchronize storage before the node becomes schedulable. */
    public boolean connected(String address) {
        AgentRecord record = agents.get(address);
        if (record == null || !record.enabled || record.lastSeen == 0) return false;
        long elapsed = System.currentTimeMillis() - record.lastSeen;
        return elapsed >= 0 && elapsed < 65000;
    }
    public boolean online(String address) {
        AgentRecord record = agents.get(address);
        return record != null && record.synchronizedStorage && connected(address);
    }
    public synchronized boolean online(Session session) { return active(session) && online(session.address); }
    public long lastSeen(String address) { AgentRecord value = agents.get(address); return value == null ? 0 : value.lastSeen; }
    public synchronized void touch(String address) {
        touch(currentSession(address));
    }
    public synchronized boolean touch(Session session) {
        if (active(session)) {
            AgentRecord record = agents.get(session.address);
            long now = System.currentTimeMillis();
            if (record.lastSeen == 0 || now - record.lastSeen > 45000) record.synchronizedStorage = false;
            record.lastSeen = now;
            return true;
        }
        return false;
    }
    public boolean isMaster() { return config.isMaster(); }
    public synchronized void markSynchronized(String address) {
        markSynchronized(address, storageVersion);
    }
    public synchronized void markSynchronized(String address, long expectedVersion) {
        markSynchronized(currentSession(address), expectedVersion);
    }
    public synchronized boolean markSynchronized(Session session, long expectedVersion) {
        if (expectedVersion != storageVersion || !active(session)) return false;
        AgentRecord record = agents.get(session.address);
        if (!record.synchronizedStorage) {
            record.synchronizedStorage = true;
            try { db.putSync("agent:member:" + session.address, record); }
            catch (RuntimeException failure) { record.synchronizedStorage = false; throw failure; }
        }
        return true;
    }
    public synchronized long storageVersion() { return storageVersion; }
    public synchronized void storageChanged() {
        if (!config.isMaster()) return;
        storageVersion++;
        Map<String, Object> batch = new LinkedHashMap<>(); batch.put("agent:storage-version", storageVersion);
        for (AgentRecord record : agents.values()) {
            record.synchronizedStorage = false; batch.put("agent:member:" + record.address, record);
        }
        db.putBatchSync(batch);
    }
    public boolean needsSynchronization(String address) {
        AgentRecord record = agents.get(address);
        return record != null && record.enabled && !record.synchronizedStorage;
    }
    public synchronized void revoke(String address) {
        AgentRecord record = agents.get(address);
        if (record != null) {
            AgentRecord disabled = disabled(record);
            db.putSync("agent:member:" + address, disabled);
            agents.put(address, disabled); keys.remove(record.credentialHash);
            if (broker != null) broker.invalidate(address, record.enrollmentId);
        }
    }
    /** Called with the membership write lock already held: removal and revocation are atomic. */
    public synchronized void revokeMembership(String address, List<String> workers, Map<String, String> labels) {
        AgentRecord current = agents.get(address);
        if (current == null) throw new IllegalArgumentException("Agent is not registered");
        AgentRecord disabled = disabled(current);
        nodes.replaceMembershipWithChanges(workers, labels, Collections.singletonMap("agent:member:" + address, disabled));
        agents.put(address, disabled); keys.remove(current.credentialHash);
        if (broker != null) broker.invalidate(address, current.enrollmentId);
    }
    private AgentRecord disabled(AgentRecord current) {
        AgentRecord disabled = new AgentRecord();
        disabled.instanceId = current.instanceId; disabled.enrollmentId = current.enrollmentId;
        disabled.address = current.address; disabled.label = current.label;
        disabled.credentialHash = current.credentialHash; disabled.joinedAt = current.joinedAt;
        disabled.lastSeen = current.lastSeen; disabled.enabled = false;
        return disabled;
    }
    public synchronized Invitation validInvitation(String token) {
        requireMaster();
        if (token == null || !token.matches("[A-Za-z0-9_-]{43}")) throw new IllegalArgumentException("Invalid invitation");
        Invitation value = db.get("agent:invite:" + hash(token), Invitation.class);
        if (value == null || value.consumed || value.expiresAt <= System.currentTimeMillis())
            throw new IllegalArgumentException("Join invitation is invalid, expired or already used");
        return value;
    }
    private void requireMaster() { if (!config.isMaster()) throw new IllegalArgumentException("This endpoint requires a Master"); }
    static String cleanAddress(String value, boolean required) {
        if (value == null || value.trim().isEmpty()) {
            if (required) throw new IllegalArgumentException("Agent IPv4 address is required");
            return "";
        }
        String address = value.trim(); String[] parts = address.split("\\.", -1);
        if (parts.length != 4) throw new IllegalArgumentException("Agent address must be an IPv4 address");
        for (String part : parts) {
            if (!part.matches("0|[1-9][0-9]{0,2}") || Integer.parseInt(part) > 255)
                throw new IllegalArgumentException("Invalid IPv4 address");
        }
        if (address.startsWith("127.") || address.equals("0.0.0.0") || Integer.parseInt(parts[0]) >= 224)
            throw new IllegalArgumentException("Use the node's unicast IPv4 address");
        return address;
    }
    static String cleanLabel(String label) {
        String result = label == null ? "" : label.trim();
        if (result.length() > 80 || result.chars().anyMatch(c -> c < 32 || c == 127))
            throw new IllegalArgumentException("Node label must be at most 80 characters with no control characters");
        return result;
    }
    public static String quote(String value) { return "'" + value.replace("'", "'\"'\"'") + "'"; }
    private String releaseVersion() {
        String manifestVersion = getClass().getPackage().getImplementationVersion();
        if (manifestVersion != null && manifestVersion.matches("[0-9]+\\.[0-9]+\\.[0-9]+([.-][A-Za-z0-9.-]+)?"))
            return manifestVersion;
        try (java.io.InputStream input = getClass().getResourceAsStream("/META-INF/maven/com.transfer/oss-transfer/pom.properties")) {
            if (input != null) {
                Properties properties = new Properties(); properties.load(input);
                String version = properties.getProperty("version", "");
                if (version.matches("[0-9]+\\.[0-9]+\\.[0-9]+([.-][A-Za-z0-9.-]+)?")) return version;
            }
        } catch (java.io.IOException ignored) { }
        return "4.0.0";
    }
    public static String randomSecret() {
        byte[] value = new byte[32]; new SecureRandom().nextBytes(value);
        return Base64.getUrlEncoder().withoutPadding().encodeToString(value);
    }
    static String hash(String value) {
        try { return Base64.getUrlEncoder().withoutPadding().encodeToString(MessageDigest.getInstance("SHA-256").digest(value.getBytes(StandardCharsets.UTF_8))); }
        catch (Exception e) { throw new IllegalStateException(e); }
    }
}
