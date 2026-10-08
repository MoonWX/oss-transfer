package com.transfer.tools.model;

import lombok.Data;

import java.util.List;
import java.util.Map;

/**
 * 节点配置模型，用于持久化 transfer.* 分布式协同配置。
 * 成员及身份持久化在 RocksDB，yml 作为首次启动时的默认值；端口在启动时跟随监听配置。
 */
@Data
public class NodeConfig {

    /** 主节点 IP */
    private String masterIp;

    /** 当前节点 IP（节点身份标识，持久化后只读） */
    private String localIp;

    /** 最近一次启动的服务端口，启动时根据当前监听配置同步 */
    private Integer port;

    /** 工作节点 IP 列表 */
    private List<String> workerIps;

    /** 节点能力展示的本地根目录列表 */
    private List<String> advertisedLocalRoots;

    /** Master-side display names, keyed by registered node address. */
    private Map<String, String> nodeLabels;
}
