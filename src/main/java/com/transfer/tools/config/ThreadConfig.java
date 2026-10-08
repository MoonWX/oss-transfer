package com.transfer.tools.config;

import lombok.Data;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.stereotype.Component;

@Data
@Component
@ConfigurationProperties(prefix = "thread-count")
public class ThreadConfig {
    private SrcThreadConfig src;

    @Data
    public static class SrcThreadConfig {
        private Integer explore;
        private Integer list;
    }

    private DestThreadConfig dest;

    @Data
    public static class DestThreadConfig {
        private Integer put;
    }
}
