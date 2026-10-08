package com.transfer.tools.model;

import lombok.Data;

@Data
public class FileReportPage {
    private java.util.List<FileReportRecord> items = new java.util.ArrayList<>();
    private String nextCursor;
    private boolean done;
    private ReportCoverage coverage;
}
