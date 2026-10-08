package com.transfer.tools.util;

import com.transfer.tools.config.DistributedConfig;
import org.springframework.http.HttpStatus;
import org.springframework.web.context.request.RequestAttributes;
import org.springframework.web.context.request.RequestContextHolder;
import org.springframework.web.context.request.ServletRequestAttributes;
import org.springframework.web.server.ResponseStatusException;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;

/** Only Master may assign a worker request to a shared bandwidth scope. */
public final class TaskBandwidthRequestGuard {
    private TaskBandwidthRequestGuard() { }

    public static void requireInternal(DistributedConfig config, boolean containsPolicyFields) {
        if (!containsPolicyFields) return; // Preserve existing unscoped CLI imports.
        RequestAttributes attributes = RequestContextHolder.getRequestAttributes();
        // Direct Java calls are internal; HTTP handlers always have servlet request attributes.
        if (!(attributes instanceof ServletRequestAttributes)) return;
        String provided = ((ServletRequestAttributes) attributes).getRequest().getHeader("X-Internal-Token");
        String expected = config == null ? null : config.getInternalToken();
        if (expected == null || expected.isEmpty() || provided == null
                || !MessageDigest.isEqual(expected.getBytes(StandardCharsets.UTF_8), provided.getBytes(StandardCharsets.UTF_8))) {
            throw new ResponseStatusException(HttpStatus.FORBIDDEN,
                    "任务带宽设置请通过主节点提交");
        }
    }
}
