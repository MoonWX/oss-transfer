package com.transfer.tools.config;

import lombok.Data;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.stereotype.Component;

@Data
@Component
@ConfigurationProperties(prefix = "src")
public class SourceBucketConfig {
    private ListConfig list;

    @Data
    public static class ListConfig {
        private Integer batchSize;
        private Integer maxKeys;
    }
}
