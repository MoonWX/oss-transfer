package com.transfer.tools.config;

import lombok.Data;
import org.springframework.boot.context.properties.ConfigurationProperties;
import org.springframework.stereotype.Component;

@Data
@Component
@ConfigurationProperties(prefix = "connection-pool")
public class ConnectionPoolConfig {
    private Integer maxConnections = 5000;
    private Integer connectionTimeout = 60000;
    private Integer socketTimeout = 180000;
    private Long connectionMaxIdleMillis = 60000L;
    private Boolean useExpectContinue = false;
    private Boolean useReaper = true;
    private Integer maxErrorRetry = 5;
}
