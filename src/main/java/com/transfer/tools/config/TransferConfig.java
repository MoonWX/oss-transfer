package com.transfer.tools.config;

import lombok.Data;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.stereotype.Component;

@Data
@Component
@ConfigurationProperties(prefix = "transfer")
public class TransferConfig {
    private String tmpDir;
    private String mode = "auto";
    private Boolean cleanMetadata = false;
    private Boolean dryRun = false;
    private String dryRunOutputFile = "dry-run-objects.log";

    public boolean isAutoMode() {
        return "auto".equalsIgnoreCase(mode);
    }

    public boolean isUrlMode() {
        return "url".equalsIgnoreCase(mode);
    }

    public boolean isDiskMode() {
        return "disk".equalsIgnoreCase(mode);
    }

    public boolean shouldCleanMetadata() {
        return cleanMetadata != null && cleanMetadata;
    }

    public boolean isDryRun() {
        return dryRun != null && dryRun;
    }
}
