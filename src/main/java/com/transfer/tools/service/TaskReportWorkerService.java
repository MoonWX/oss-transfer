package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.springframework.beans.BeanUtils;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;
import java.util.*;

@Service
public class TaskReportWorkerService {
    @Autowired private SubTaskManagerService tasks;
    @Autowired private FileReportLedgerService ledger;
    @Autowired private RocksDBService database;
    @Autowired private DistributedConfig distributedConfig;
    @Autowired private LocalTaskControlService control;

    public TaskReportWorkerSnapshot scope(Integer taskId) {
        TaskReportWorkerSnapshot result = new TaskReportWorkerSnapshot();
        result.setReportTaskId(taskId);
        result.setNode(distributedConfig.getLocalIp());
        result.setObservedAt(System.currentTimeMillis());
        Set<Integer> seen = new HashSet<>();
        List<Integer> active = tasks.getTaskIdList();
        if (active != null) for (Integer id : active) {
            seen.add(id);
            add(result, tasks.getTaskStatus(id), taskId);
        }
        List<Integer> deleted = tasks.getDeletedTaskIdList();
        if (deleted != null) for (Integer id : deleted) {
            if (seen.add(id)) add(result, tasks.getDeletedTaskStatus(id), taskId);
        }
        // Discovery is a parent coordinator, not a file-transfer shard. Do not add
        // its synthetic counters to the transfer summary.
        LocalDiscoveryState discovery = database.get("local:discovery:" + taskId, LocalDiscoveryState.class);
        if (discovery != null) {
            result.setFound(true);
            result.setDiscoveryComplete(discovery.getStatus() == LocalDiscoveryState.Status.COMPLETED);
            result.setDiscoveryStatus(discovery.getStatus() == null ? null : discovery.getStatus().name());
            result.setDiscoveryError(discovery.getErrorMessage());
        }
        result.getTasks().sort(Comparator.comparing(SubTaskStatus::getTaskIndex));
        // STOPPED is persisted before interrupted I/O has necessarily exited.
        // Include both the parent/discovery operation and every retained shard.
        boolean executionActive = control.isActive(taskId);
        for (SubTaskStatus task : result.getTasks()) executionActive |= control.isActive(task.getTaskIndex());
        result.setExecutionActive(executionActive);
        result.setCoverage(ledger.coverage(taskId));
        ReportCoverage coverage = result.getCoverage();
        if (coverage != null) {
            int knownScans = result.getTasks().size() + (discovery == null ? 0 : 1);
            result.setScopeMissing(coverage.getRegisteredScans() > knownScans
                    || (coverage.getDiscoveredCount() > 0 && !result.isFound()));
        }
        return result;
    }

    private void add(TaskReportWorkerSnapshot result, SubTaskStatus task, Integer reportTaskId) {
        if (task == null) return;
        Integer id = task.getDirection() == TransferDirection.OSS_TO_OSS || task.getParentTaskId() == null
                ? task.getTaskIndex() : task.getParentTaskId();
        if (!reportTaskId.equals(id)) return;
        SubTaskStatus summary = new SubTaskStatus();
        BeanUtils.copyProperties(task, summary);
        summary.setLocalFileEntries(null);
        result.getTasks().add(summary);
        result.setFound(true);
    }
}
