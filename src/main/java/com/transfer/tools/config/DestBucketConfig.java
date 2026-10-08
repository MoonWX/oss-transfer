package com.transfer.tools.config;

import lombok.Data;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.stereotype.Component;

@Data
@Component
@ConfigurationProperties(prefix = "dest")
public class DestBucketConfig {
    private MultipartConfig multipart;

    @Data
    public static class MultipartConfig {
        private Long threshold;
        private Long partsize;
        /** Maximum submitted-but-not-completed parts for one object. */
        private Integer maxInFlightPartsPerObject = 4;
    }
}
