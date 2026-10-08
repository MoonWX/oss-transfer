package com.transfer.tools.model;

import lombok.Data;
import java.util.ArrayList;
import java.util.List;

/** Task metadata only: file manifests must be read through the bounded ledger cursor. */
@Data
public class TaskReportWorkerSnapshot {
    private Integer reportTaskId;
    private String node;
    private long observedAt;
    private boolean found;
    /** Ledger evidence exists for task/scan scope that is no longer retained. */
    private boolean scopeMissing;
    /** Null on older workers; false confirms that registered execution has drained. */
    private Boolean executionActive;
    private List<SubTaskStatus> tasks = new ArrayList<>();
    private ReportCoverage coverage;
    private Boolean discoveryComplete;
    private String discoveryStatus;
    private String discoveryError;
}
