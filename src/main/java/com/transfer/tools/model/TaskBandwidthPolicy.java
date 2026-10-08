package com.transfer.tools.model;

import lombok.Data;
import lombok.NoArgsConstructor;
import java.util.ArrayList;
import java.util.List;

/** Logical transfer budget. Each fixed member owns an equal share, including while peers are offline. */
@Data
@NoArgsConstructor
public class TaskBandwidthPolicy {
    private String scopeId;
    private Double limitMbps;
    private List<String> members = new ArrayList<>();
    private long revision;

    public static void validateLimit(Double limitMbps) {
        if (limitMbps != null && (!Double.isFinite(limitMbps) || limitMbps < 0.1 || limitMbps > 1_000_000))
            throw new IllegalArgumentException("任务带宽上限须为 0.1 至 1,000,000 Mbps；留空取消额外限制");
    }
}
