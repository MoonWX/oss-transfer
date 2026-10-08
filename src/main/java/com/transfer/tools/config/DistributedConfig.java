package com.transfer.tools.config;

import com.transfer.tools.service.NodeConfigService;
import lombok.Setter;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.boot.context.properties.bind.Bindable;
import org.springframework.boot.context.properties.bind.Binder;
import org.springframework.context.annotation.Lazy;
import org.springframework.core.env.Environment;
import org.springframework.stereotype.Component;

import javax.annotation.PostConstruct;
import java.util.Collections;
import java.util.List;

/**
 * 分布式协同配置：yml 静态默认值 + 运行时持久化委托。
 *
 * 字段在 {@link #init()} 中通过 {@link Environment} 手动从 yml 读取，作为首次启动的默认值。
 * 运行时实际生效值由 {@link NodeConfigService}（RocksDB 持久化）提供，
 * 除服务端口始终跟随监听配置外，各 getter 优先返回运行时值，未初始化时回退 yml 默认值。
 *
 * 说明：不使用 @ConfigurationProperties 自动绑定，因为手写 getter（运行时委托）
 * 会破坏 JavaBean 绑定器对字段的写入，导致 masterIp/port/workerIps 绑定为 null。
 */
@Component
public class DistributedConfig {

    @Setter
    private String masterIp;

    @Setter
    private String localIp;

    @Setter
    private Integer port = 1685;

    @Setter
    private List<String> workerIps;

    @Setter
    private List<String> advertisedLocalRoots;

    @Setter
    private List<AdvertisedLocalBinding> advertisedLocalBindings;

    @Setter
    private String internalToken;

    @Setter
    private String masterWebUrl;

    private String role;

    /**
     * 共享存储的节点能力声明：逻辑存储 ID + 本机挂载路径。
     * 仅用于能力展示（/api/worker/local-capabilities），任务创建前仍由 Worker 执行真实路径预检。
     */
    @lombok.Data
    public static class AdvertisedLocalBinding {
        private String storageId;
        private String localPath;
    }

    @Autowired
    private Environment environment;

    @Lazy
    @Autowired
    private NodeConfigService nodeConfigService;

    /**
     * 手动从 Environment 读取 yml 静态默认值填充字段。
     * 绕开 @ConfigurationProperties 自动绑定（被手写 getter 破坏）。
     */
    @PostConstruct
    public void init() {
        Binder binder = Binder.get(environment);
        masterIp = environment.getProperty("transfer.master-ip");
        localIp = environment.getProperty("transfer.local-ip");
        // server.port is the actual listener. Its default placeholder follows transfer.port,
        // so overriding either field works without a stale persisted port splitting the cluster.
        port = environment.getProperty("server.port", Integer.class);
        if (port == null) port = environment.getProperty("transfer.port", Integer.class, 1685);
        if (port < 1 || port > 65535)
            throw new IllegalStateException("服务端口必须为 1–65535，请检查 server.port / transfer.port");
        workerIps = binder.bind("transfer.worker-ips", Bindable.listOf(String.class)).orElse(null);
        advertisedLocalRoots = binder.bind("transfer.advertised-local-roots", Bindable.listOf(String.class)).orElse(null);
        advertisedLocalBindings = bindingsFromEnvironment();
        internalToken = environment.getProperty("transfer.internal-token");
        masterWebUrl = environment.getProperty("transfer.master-web-url");
        role = environment.getProperty("transfer.role", "master").toLowerCase(java.util.Locale.ROOT);
        if (!("master".equals(role) || "agent".equals(role)))
            throw new IllegalStateException("transfer.role must be master or agent");
        if (isAgent()) {
            String bind = environment.getProperty("server.address", "");
            if (!("127.0.0.1".equals(bind) || "::1".equals(bind)))
                throw new IllegalStateException("Agents must bind server.address to loopback");
            localIp = environment.getProperty("transfer.agent.address", localIp);
            masterWebUrl = environment.getProperty("transfer.agent.master-url", masterWebUrl);
            if (masterWebUrl != null && !masterWebUrl.isEmpty())
                masterIp = java.net.URI.create(masterWebUrl).getHost();
            workerIps = java.util.Collections.singletonList(localIp);
        }
        if (internalToken == null || internalToken.isEmpty() || "local-dev-internal-token".equals(internalToken)) {
            byte[] secret = new byte[32]; new java.security.SecureRandom().nextBytes(secret);
            internalToken = java.util.Base64.getUrlEncoder().withoutPadding().encodeToString(secret);
        }
    }

    /**
     * 从 Environment 读取 transfer.advertised-local-bindings 配置并转换为
     * {@link AdvertisedLocalBinding} 列表。
     */
    private List<AdvertisedLocalBinding> bindingsFromEnvironment() {
        List<AdvertisedLocalBinding> result =
                Binder.get(environment).bind("transfer.advertised-local-bindings",
                        Bindable.listOf(AdvertisedLocalBinding.class)).orElse(null);
        if (result == null) {
            return Collections.emptyList();
        }
        return result;
    }

    /** 运行时主节点 IP，未初始化时回退 yml 默认值 */
    public String getMasterIp() {
        return nodeConfigService != null && nodeConfigService.get() != null
                && nodeConfigService.getMasterIp() != null
                ? nodeConfigService.getMasterIp() : masterIp;
    }

    /** 运行时当前节点 IP，未初始化时回退 yml 默认值（含自动取本机 IP 兜底） */
    public String getLocalIp() {
        if (nodeConfigService != null && nodeConfigService.get() != null
                && nodeConfigService.getLocalIp() != null) {
            return nodeConfigService.getLocalIp();
        }
        if (localIp != null && !localIp.isEmpty()) {
            return localIp;
        }
        try {
            return java.net.InetAddress.getLocalHost().getHostAddress();
        } catch (Exception e) {
            return "127.0.0.1";
        }
    }

    /** Service port follows the listener configuration; old RocksDB snapshots cannot override it. */
    public Integer getPort() {
        return port;
    }

    /** 运行时工作节点列表，未初始化时回退 yml 默认值 */
    public List<String> getWorkerIps() {
        return nodeConfigService != null && nodeConfigService.get() != null
                && nodeConfigService.getWorkerIps() != null
                ? nodeConfigService.getWorkerIps() : workerIps;
    }

    /** 运行时节点能力根目录列表，未初始化时回退 yml 默认值 */
    public List<String> getAdvertisedLocalRoots() {
        return nodeConfigService != null && nodeConfigService.get() != null
                && nodeConfigService.getAdvertisedLocalRoots() != null
                ? nodeConfigService.getAdvertisedLocalRoots() : advertisedLocalRoots;
    }

    /** 共享存储节点能力声明列表（storageId + localPath），仅用于能力展示，回退 yml 默认值 */
    public List<AdvertisedLocalBinding> getAdvertisedLocalBindings() {
        return advertisedLocalBindings;
    }

    /** 节点间共享密钥（内部调用认证），用于主节点调用 worker 接口时附加 X-Internal-Token 头 */
    public String getInternalToken() {
        return internalToken;
    }

    /** A server-configured public origin; never derived from a browser Host/return URL. */
    public String getMasterWebUrl() {
        String value = masterWebUrl;
        if (value == null || value.trim().isEmpty()) {
            String host = getMasterIp();
            if (host == null || !host.matches("[A-Za-z0-9.-]+"))
                throw new IllegalStateException("请配置 transfer.master-web-url 为主节点公开地址");
            value = "http://" + host + ":" + getPort();
        }
        try {
            java.net.URI uri = new java.net.URI(value.trim());
            if (!("http".equalsIgnoreCase(uri.getScheme()) || "https".equalsIgnoreCase(uri.getScheme()))
                    || uri.getHost() == null || uri.getUserInfo() != null || uri.getQuery() != null
                    || uri.getFragment() != null || uri.getPort() == 0 || uri.getPort() > 65535
                    || !(uri.getPath() == null || uri.getPath().isEmpty() || "/".equals(uri.getPath())))
                throw new IllegalArgumentException();
            return uri.getScheme().toLowerCase(java.util.Locale.ROOT) + "://" + uri.getRawAuthority();
        } catch (Exception e) {
            throw new IllegalStateException("transfer.master-web-url 必须是无路径、参数和账号的 http(s) 主节点地址");
        }
    }

    public boolean isMaster() {
        if ("agent".equals(role)) return false;
        if ("master".equals(role)) return true;
        String m = getMasterIp();
        if (m == null || m.isEmpty()) {
            return false;
        }
        return m.equals(getLocalIp());
    }

    public boolean isWorker() {
        List<String> workers = getWorkerIps();
        if (workers == null || workers.isEmpty()) {
            return false;
        }
        return workers.contains(getLocalIp());
    }

    public boolean isAgent() { return "agent".equals(role); }
}
