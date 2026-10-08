package com.transfer.tools.config;

import lombok.Data;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.stereotype.Component;

@Data
@Component
@ConfigurationProperties(prefix = "rate-limit")
public class RateLimitConfig {
    private SrcRateLimitConfig src;
    private DestRateLimitConfig dest;
    private Long bandwidth;
    private AutoAdjustConfig autoAdjust = new AutoAdjustConfig();

    @Data
    public static class SrcRateLimitConfig {
        private QpsConfig qps;

        @Data
        public static class QpsConfig {
            private Integer list;
        }
    }

    @Data
    public static class DestRateLimitConfig {
        private QpsConfig qps;

        @Data
        public static class QpsConfig {
            private Integer put;
            private Integer part;
            private Integer head;
        }
    }

    @Data
    public static class AutoAdjustConfig {
        private Double increaseFactor = 1.2;
        private Long intervalMs = 10000L;
        private Double reduceFactor = 0.95;
        private Integer maxRetryOn503 = 3;
        private Long retryDelayMs = 500L;
        private Integer minPutQps = 400;
        private Integer consecutive503Threshold = 50;
    }
}
