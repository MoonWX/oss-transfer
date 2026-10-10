package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.NodeConfig;
import com.transfer.tools.storage.RocksDBService;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;
import javax.annotation.PostConstruct;
import java.nio.file.Paths;
import java.util.*;
import java.util.concurrent.Callable;
import java.util.concurrent.locks.ReentrantLock;
import java.util.concurrent.locks.ReentrantReadWriteLock;

/** Durable membership. Scheduling holds the read lock through task persistence / RPC. */
@Service
public class NodeConfigService {
    private static final String KEY = "node_config";
    private volatile NodeConfig current;
    private final ReentrantReadWriteLock membershipLock = new ReentrantReadWriteLock(true);
    private final ReentrantLock storageSynchronizationLock = new ReentrantLock(true);
    @Autowired private RocksDBService rocksDBService;
    @Autowired private DistributedConfig distributedConfig;

    @PostConstruct public void init() {
        NodeConfig saved = rocksDBService.get(KEY, NodeConfig.class);
        if (saved == null) {
            saved = fromYml();
            rocksDBService.putSync(KEY, saved);
        } else if (!Objects.equals(saved.getPort(), distributedConfig.getPort())) {
            // Listener settings are deployment configuration, unlike durable membership.
            // Preserve addresses, labels and roots when changing a cluster service port.
            saved = copy(saved);
            saved.setPort(distributedConfig.getPort());
            rocksDBService.putSync(KEY, saved);
        }
        current = copy(saved);
    }

    public NodeConfig get() { return current == null ? null : copy(current); }

    /** The legacy worker endpoint may change advertised roots, never bypass registry guards. */
    public NodeConfig update(NodeConfig incoming) {
        return withMembershipWriteLock(() -> {
            if (incoming == null) throw invalid("节点配置不能为空");
            NodeConfig base = current == null ? fromYml() : copy(current);
            if ((incoming.getLocalIp() != null && !Objects.equals(base.getLocalIp(), incoming.getLocalIp()))
                    || (incoming.getMasterIp() != null && !Objects.equals(base.getMasterIp(), incoming.getMasterIp()))
                    || (incoming.getPort() != null && !Objects.equals(base.getPort(), incoming.getPort()))
                    || (incoming.getWorkerIps() != null && !Objects.equals(base.getWorkerIps(), incoming.getWorkerIps()))
                    || incoming.getNodeLabels() != null)
                throw new TransferValidationException("NODE_IDENTITY_READ_ONLY",
                        "节点身份、端口及成员列表不可通过本接口修改，请使用主节点的节点管理功能");
            if (incoming.getAdvertisedLocalRoots() != null)
                base.setAdvertisedLocalRoots(validateRoots(incoming.getAdvertisedLocalRoots()));
            publish(base);
            return copy(base);
        });
    }

    /** Only NodeRegistryService invokes this after identity, quiescence and history checks. */
    void replaceMembership(List<String> workers, Map<String, String> labels) {
        if (!membershipLock.isWriteLockedByCurrentThread()) throw new IllegalStateException("membership write lock required");
        NodeConfig next = current == null ? fromYml() : copy(current);
        next.setWorkerIps(new ArrayList<>(workers));
        next.setNodeLabels(new LinkedHashMap<>(labels));
        publish(next);
    }
    public void replaceMembershipWithChanges(List<String> workers, Map<String, String> labels, Map<String, Object> changes) {
        if (!membershipLock.isWriteLockedByCurrentThread()) throw new IllegalStateException("membership write lock required");
        NodeConfig next = current == null ? fromYml() : copy(current);
        next.setWorkerIps(new ArrayList<>(workers)); next.setNodeLabels(new LinkedHashMap<>(labels));
        Map<String, Object> batch = new LinkedHashMap<>(changes); batch.put(KEY, next);
        rocksDBService.putBatchSync(batch); current = copy(next);
    }

    /** Invitation consumption, credential hash and membership commit in one durable batch. */
    public void registerAgent(String address, String label, Map<String, Object> changes) {
        withMembershipWriteLock(() -> {
            NodeConfig next = current == null ? fromYml() : copy(current);
            if (next.getWorkerIps().contains(address))
                throw new TransferValidationException("NODE_EXISTS", "Node address is already registered");
            next.getWorkerIps().add(address);
            next.getNodeLabels().put(address, label);
            Map<String, Object> batch = new LinkedHashMap<>(changes);
            batch.put(KEY, next);
            rocksDBService.putBatchSync(batch);
            current = copy(next);
            return null;
        });
    }

    private void publish(NodeConfig value) {
        rocksDBService.putSync(KEY, value);
        current = copy(value);
    }

    public <T> T withMembershipReadLock(Callable<T> work) {
        membershipLock.readLock().lock();
        try { return call(work); } finally { membershipLock.readLock().unlock(); }
    }

    /**
     * Serialize a storage mutation and its worker acknowledgments with background
     * reconciliation. Always acquire membership before storage, including when a
     * caller already holds the reentrant membership write lock for node admission.
     * Agent polls must only enqueue reconciliation, never acquire this lock.
     */
    public <T> T withStorageSynchronizationLock(Callable<T> work) {
        return withMembershipReadLock(() -> {
            storageSynchronizationLock.lock();
            try { return call(work); } finally { storageSynchronizationLock.unlock(); }
        });
    }
    public <T> T withMembershipWriteLock(Callable<T> work) {
        membershipLock.writeLock().lock();
        try { return call(work); } finally { membershipLock.writeLock().unlock(); }
    }
    private <T> T call(Callable<T> work) {
        try { return work.call(); }
        catch (RuntimeException e) { throw e; }
        catch (Exception e) { throw new IllegalStateException(e); }
    }

    public static List<String> validateRoots(List<String> values) {
        if (values.size() > 100) throw invalid("展示目录最多100项");
        LinkedHashSet<String> roots = new LinkedHashSet<>();
        for (String root : values) {
            if (root == null || root.trim().isEmpty() || root.length() > 4096
                    || root.indexOf('\0') >= 0 || root.indexOf('\n') >= 0 || root.indexOf('\r') >= 0
                    || !Paths.get(root.trim()).isAbsolute())
                throw invalid("展示目录必须是绝对路径，每行一个");
            roots.add(Paths.get(root.trim()).normalize().toString());
        }
        return new ArrayList<>(roots);
    }
    private static TransferValidationException invalid(String message) {
        return new TransferValidationException("INVALID_NODE_CONFIG", message);
    }
    private NodeConfig fromYml() {
        NodeConfig config = new NodeConfig();
        config.setMasterIp(distributedConfig.getMasterIp());
        config.setLocalIp(distributedConfig.getLocalIp());
        config.setPort(distributedConfig.getPort());
        config.setWorkerIps(distributedConfig.getWorkerIps());
        config.setAdvertisedLocalRoots(distributedConfig.getAdvertisedLocalRoots());
        return copy(config);
    }
    private static NodeConfig copy(NodeConfig source) {
        NodeConfig result = new NodeConfig();
        result.setMasterIp(source.getMasterIp()); result.setLocalIp(source.getLocalIp()); result.setPort(source.getPort());
        result.setWorkerIps(source.getWorkerIps() == null ? new ArrayList<>() : new ArrayList<>(source.getWorkerIps()));
        result.setAdvertisedLocalRoots(source.getAdvertisedLocalRoots() == null ? new ArrayList<>() : new ArrayList<>(source.getAdvertisedLocalRoots()));
        result.setNodeLabels(source.getNodeLabels() == null ? new LinkedHashMap<>() : new LinkedHashMap<>(source.getNodeLabels()));
        return result;
    }
    public String getMasterIp() { return current == null ? null : current.getMasterIp(); }
    public String getLocalIp() { return current == null ? null : current.getLocalIp(); }
    public Integer getPort() { return current == null ? null : current.getPort(); }
    public List<String> getWorkerIps() { return current == null ? null : new ArrayList<>(current.getWorkerIps()); }
    public List<String> getAdvertisedLocalRoots() { return current == null ? null : new ArrayList<>(current.getAdvertisedLocalRoots()); }
    public boolean isMaster() { return getMasterIp() != null && getMasterIp().equals(getLocalIp()); }
    public boolean isWorker() { return getWorkerIps() != null && getWorkerIps().contains(getLocalIp()); }
}
