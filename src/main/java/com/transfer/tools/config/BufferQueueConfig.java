package com.transfer.tools.config;

import lombok.Data;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.stereotype.Component;

@Data
@Component
@ConfigurationProperties(prefix = "buffer-queue")
public class BufferQueueConfig {
    private ExploreConfig explore = new ExploreConfig();
    private ListConfig list = new ListConfig();
    private PutConfig put = new PutConfig();
    private ProgressConfig progress = new ProgressConfig();
    private FailedConfig failed = new FailedConfig();
    private FallbackConfig fallback = new FallbackConfig();

    @Data
    public static class ExploreConfig {
        private Integer size = 100;
    }

    @Data
    public static class ListConfig {
        private Integer size = 1000;
    }

    @Data
    public static class PutConfig {
        private Integer size = 100;
    }

    @Data
    public static class ProgressConfig {
        private Integer size = 100000;
    }

    @Data
    public static class FailedConfig {
        private Integer size = 100000;
    }

    @Data
    public static class FallbackConfig {
        private Integer size = 100000;
    }
}
