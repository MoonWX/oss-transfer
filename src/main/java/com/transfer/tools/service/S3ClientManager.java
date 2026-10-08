package com.transfer.tools.service;

import com.amazonaws.ClientConfiguration;
import com.amazonaws.auth.AWSStaticCredentialsProvider;
import com.amazonaws.auth.BasicAWSCredentials;
import com.amazonaws.client.builder.AwsClientBuilder;
import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.AmazonS3ClientBuilder;
import com.transfer.tools.config.ConnectionPoolConfig;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.S3ClientConfig;
import com.transfer.tools.storage.RocksDBService;
import lombok.extern.slf4j.Slf4j;
import org.apache.http.impl.conn.PoolingHttpClientConnectionManager;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.scheduling.annotation.Scheduled;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import java.lang.reflect.Field;
import java.util.*;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.locks.ReadWriteLock;
import java.util.concurrent.locks.ReentrantReadWriteLock;

@Slf4j
@Service
public class S3ClientManager {

    private static final String S3_CONFIG_KEY_PREFIX = "s3_config_";
    private static final long RECREATE_INTERVAL_MS = 10000;

    private final Map<String, AmazonS3> clientMap = new ConcurrentHashMap<>();
    private final Map<String, AmazonS3> listClientMap = new ConcurrentHashMap<>();
    private final Map<String, S3ClientConfig> configMap = new ConcurrentHashMap<>();
    private final Map<String, ReadWriteLock> clientLocks = new ConcurrentHashMap<>();
    private final Map<String, Long> lastRecreateTime = new ConcurrentHashMap<>();

    @Autowired
    private RocksDBService rocksDBService;
    @org.springframework.context.annotation.Lazy
    @Autowired(required = false) private com.transfer.tools.agent.AgentRegistryService agentRegistry;

    @Autowired
    private DistributedConfig distributedConfig;

    @Autowired
    private ConnectionPoolConfig connectionPoolConfig;

    @PostConstruct
    public void init() {
        loadAllConfigs();
    }

    private void loadAllConfigs() {
        try {
            List<S3ClientConfig> configs = rocksDBService.scanByPrefix(S3_CONFIG_KEY_PREFIX, S3ClientConfig.class);
            for (S3ClientConfig config : configs) {
                try {
                    AmazonS3 client = createS3Client(config);
                    AmazonS3 listClient = createS3Client(config);

                    clientMap.put(config.getAlias(), client);
                    listClientMap.put(config.getAlias(), listClient);
                    configMap.put(config.getAlias(), config);
                    clientLocks.put(config.getAlias(), new ReentrantReadWriteLock());
                    log.info("已加载S3配置: {} -> {}", config.getAlias(), config.getEndpoint());
                } catch (Exception e) {
                    log.error("初始化S3客户端失败: {}", config.getAlias(), e);
                }
            }
            log.info("已加载 {} 个S3配置", clientMap.size());
        } catch (Exception e) {
            log.error("加载S3配置失败", e);
        }
    }

    public boolean addClient(String alias, String endpoint, String accessKey, String secretKey) {
        return addClient(alias, endpoint, accessKey, secretKey, null, null, true);
    }

    public boolean addClient(String alias, String endpoint, String accessKey, String secretKey, boolean validate) {
        return addClient(alias, endpoint, accessKey, secretKey, null, null, validate);
    }

    public boolean addClient(String alias, String endpoint, String accessKey, String secretKey,
            Boolean useV2SignerType, Boolean pathStyleEnable, boolean validate) {
        return saveClient(alias, endpoint, accessKey, secretKey, useV2SignerType, pathStyleEnable, validate, true);
    }

    public boolean updateClient(String alias, String endpoint, String accessKey, String secretKey,
            Boolean useV2SignerType, Boolean pathStyleEnable, boolean validate) {
        return saveClient(alias, endpoint, accessKey, secretKey, useV2SignerType, pathStyleEnable, validate, false);
    }

    private boolean saveClient(String alias, String endpoint, String accessKey, String secretKey,
            Boolean useV2SignerType, Boolean pathStyleEnable, boolean validate, boolean create) {
        ReadWriteLock lock = clientLocks.computeIfAbsent(alias, key -> new ReentrantReadWriteLock());
        lock.writeLock().lock();
        AmazonS3 client = null;
        AmazonS3 listClient = null;
        try {
            // Check under the same lock as publication; concurrent POSTs must never become updates.
            // Durable entries also count when startup failed to initialize their clients.
            boolean exists = configMap.containsKey(alias)
                    || rocksDBService.get(S3_CONFIG_KEY_PREFIX + alias, S3ClientConfig.class) != null;
            if (create && exists) throw new IllegalArgumentException("S3配置已存在，请使用更新接口: " + alias);
            if (!create && !exists) throw new IllegalArgumentException("S3配置不存在: " + alias);
            S3ClientConfig config = new S3ClientConfig(alias, endpoint, accessKey, secretKey, useV2SignerType, pathStyleEnable);
            client = createS3Client(config);
            listClient = createS3Client(config);
            if (validate) client.listBuckets();

            // Keep the working connection intact until validation and durable storage succeed.
            rocksDBService.putSync(S3_CONFIG_KEY_PREFIX + alias, config);
            AmazonS3 oldClient = clientMap.put(alias, client);
            AmazonS3 oldListClient = listClientMap.put(alias, listClient);
            configMap.put(alias, config);
            notifyAgentStorageChange();
            closeClient(oldClient);
            closeClient(oldListClient);
            log.info("保存S3配置成功: {} (校验: {}, v2签名: {}, pathStyle: {})",
                    alias, validate, useV2SignerType, pathStyleEnable);
            return true;
        } catch (Exception e) {
            closeClient(client);
            closeClient(listClient);
            log.error("保存S3配置失败: {}", alias, e);
            throw new RuntimeException("S3配置验证或保存失败: " + e.getMessage(), e);
        } finally {
            lock.writeLock().unlock();
        }
    }

    private void closeClient(AmazonS3 client) {
        if (client == null) return;
        try { client.shutdown(); }
        catch (RuntimeException e) { log.warn("关闭S3客户端失败", e); }
    }

    public boolean removeClient(String alias) {
        ReadWriteLock lock = clientLocks.computeIfAbsent(alias, key -> new ReentrantReadWriteLock());
        lock.writeLock().lock();
        try {
            if (!clientMap.containsKey(alias)) return false;
            rocksDBService.delete(S3_CONFIG_KEY_PREFIX + alias);
            closeClient(clientMap.remove(alias));
            closeClient(listClientMap.remove(alias));
            configMap.remove(alias);
            notifyAgentStorageChange();
            lastRecreateTime.remove(alias);
            log.info("删除S3配置成功: {}", alias);
            return true;
        } catch (Exception e) {
            log.error("删除S3配置失败: {}", alias, e);
            return false;
        } finally {
            lock.writeLock().unlock();
        }
    }

    public AmazonS3 getClient(String alias) {
        ReadWriteLock lock = clientLocks.get(alias);
        if (lock == null) {
            return null;
        }

        lock.readLock().lock();
        try {
            return clientMap.get(alias);
        } finally {
            lock.readLock().unlock();
        }
    }
    private void notifyAgentStorageChange() {
        try { if (agentRegistry != null) agentRegistry.storageChanged(); }
        catch (RuntimeException e) {
            // The connection is already durably saved; a notification cannot roll it back.
            log.warn("Agent storage synchronization marker could not be persisted; reconnection will reconcile configurations");
        }
    }

    public AmazonS3 getListClient(String alias) {
        return listClientMap.get(alias);
    }

    public void recreateClient(String alias) {
        ReadWriteLock lock = clientLocks.get(alias);
        if (lock == null) {
            log.warn("无法重建S3客户端，锁不存在: {}", alias);
            return;
        }

        lock.writeLock().lock();
        try {
            // Configuration may have been replaced or removed while this request waited
            // for the lock. Rebuilding a previously captured value would undo that update.
            S3ClientConfig config = configMap.get(alias);
            if (config == null) {
                log.warn("无法重建S3客户端，配置不存在: {}", alias);
                return;
            }
            Long lastTime = lastRecreateTime.get(alias);
            long now = System.currentTimeMillis();

            if (lastTime != null && (now - lastTime) < RECREATE_INTERVAL_MS) {
                long remainingMs = RECREATE_INTERVAL_MS - (now - lastTime);
                log.warn("S3客户端重建过于频繁，忽略本次重建: alias={}, 距上次重建 {}ms，需等待 {}ms",
                        alias, now - lastTime, remainingMs);
                return;
            }

            log.info("开始重建S3客户端: {}", alias);

            AmazonS3 oldClient = clientMap.get(alias);

            try {
                AmazonS3 newClient = createS3Client(config);
                clientMap.put(alias, newClient);
                lastRecreateTime.put(alias, now);
                log.info("重建S3客户端成功: {}", alias);

                // 延迟关闭旧客户端，避免影响正在使用的线程
                if (oldClient != null) {
                    final AmazonS3 clientToClose = oldClient;
                    Thread shutdown = new Thread(() -> {
                        try {
                            Thread.sleep(60000);  // 等待60秒
                            clientToClose.shutdown();
                            log.info("已关闭旧的S3客户端: {}", alias);
                        } catch (Exception e) {
                            log.warn("关闭旧S3客户端失败: {}", alias, e);
                        }
                    }, "s3-client-shutdown-" + alias);
                    shutdown.setDaemon(true);
                    shutdown.start();
                }
            } catch (Exception e) {
                log.error("重建S3客户端失败: {}", alias, e);
            }
        } finally {
            lock.writeLock().unlock();
        }
    }

    public S3ClientConfig getConfig(String alias) {
        return configMap.get(alias);
    }

    public List<S3ClientConfig> getAllConfigs() {
        return new ArrayList<>(configMap.values());
    }

    public Map<String, S3ClientConfig> getConfigMap() {
        return new HashMap<>(configMap);
    }

    public boolean containsClient(String alias) {
        return clientMap.containsKey(alias);
    }

    public List<String> listBuckets(String alias) {
        AmazonS3 client = clientMap.get(alias);
        if (client == null) {
            throw new RuntimeException("S3配置不存在: " + alias);
        }

        try {
            List<com.amazonaws.services.s3.model.Bucket> buckets = client.listBuckets();
            List<String> bucketNames = new ArrayList<>();
            for (com.amazonaws.services.s3.model.Bucket bucket : buckets) {
                bucketNames.add(bucket.getName());
            }
            Collections.sort(bucketNames);
            return bucketNames;
        } catch (Exception e) {
            log.error("列出buckets失败: {}", alias, e);
            throw new RuntimeException("列出buckets失败: " + e.getMessage(), e);
        }
    }

    AmazonS3 createS3Client(S3ClientConfig config) {
        BasicAWSCredentials credentials = new BasicAWSCredentials(config.getAccessKey(), config.getSecretKey());

        ClientConfiguration clientConfig = new ClientConfiguration();
        clientConfig.setMaxConnections(connectionPoolConfig.getMaxConnections());
        clientConfig.setConnectionTimeout(connectionPoolConfig.getConnectionTimeout());
        clientConfig.setSocketTimeout(connectionPoolConfig.getSocketTimeout());
        clientConfig.setConnectionMaxIdleMillis(connectionPoolConfig.getConnectionMaxIdleMillis());
        clientConfig.setUseExpectContinue(connectionPoolConfig.getUseExpectContinue());
        clientConfig.setUseReaper(connectionPoolConfig.getUseReaper());
        clientConfig.setMaxErrorRetry(connectionPoolConfig.getMaxErrorRetry());

        if (config.getUseV2SignerType() != null && config.getUseV2SignerType()) {
            clientConfig.setSignerOverride("S3SignerType");
        }

        AmazonS3ClientBuilder builder = AmazonS3ClientBuilder.standard()
                .withCredentials(new AWSStaticCredentialsProvider(credentials))
                .withEndpointConfiguration(
                    new AwsClientBuilder.EndpointConfiguration(config.getEndpoint(), "default"))
                .withClientConfiguration(clientConfig)
                .disableChunkedEncoding();

        if (config.getPathStyleEnable() != null && config.getPathStyleEnable()) {
            builder.withPathStyleAccessEnabled(true);
        }

        return builder.build();
    }
}
