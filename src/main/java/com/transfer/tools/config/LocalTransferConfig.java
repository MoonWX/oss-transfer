package com.transfer.tools.config;

import lombok.Data;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.stereotype.Component;

@Data
@Component
@ConfigurationProperties(prefix = "local-transfer")
public class LocalTransferConfig {
    private int smallFileConcurrency = 64;
    private int completionWriteBatch = 500;
    private Batch batch = new Batch();

    @Data
    public static class Batch {
        private int maxItems = 1000;
        private int maxBytes = 4194304;
    }
}
