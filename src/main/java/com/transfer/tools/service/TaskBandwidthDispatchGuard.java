package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.TaskBandwidthPolicy;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

/** Reject missing or misrouted policy before durable dispatch can become runnable. */
@Service
public class TaskBandwidthDispatchGuard {
    @Autowired private WorkerTaskBandwidthService policies;
    @Autowired private DistributedConfig config;
    public void requireReady(String scope, Double limit) {
        MasterTaskBandwidthService.validateLimit(limit);
        if (scope == null) {
            if (limit != null) throw new TransferValidationException("TASK_BANDWIDTH_NOT_READY", "请通过主节点创建带宽受限任务");
            return;
        }
        TaskBandwidthPolicy policy = policies.get(scope);
        if (policy == null || !policy.getMembers().contains(config.getLocalIp()))
            throw new TransferValidationException("TASK_BANDWIDTH_NOT_READY", "任务带宽策略未就绪或当前节点不在任务范围内");
    }
}
