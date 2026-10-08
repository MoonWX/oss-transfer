package com.transfer.tools.model;

import lombok.Data;

@Data
public class ReportCoverage {
    private String ledgerCoverage = "LEGACY_UNKNOWN";
    private boolean scanComplete;
    private int registeredScans;
    private int completedScans;
    private long discoveredCount;
    private long revision;
    private Long scanStartedAt;
    private Long scanCompletedAt;
    private String lastError;
}
