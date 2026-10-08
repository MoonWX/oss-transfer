package com.transfer.tools.util;

import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.service.LocalTaskRateLimiterService;
import com.transfer.tools.service.WorkerTaskBandwidthService;

import java.io.FilterInputStream;
import java.io.IOException;
import java.io.InputStream;

public class LocalTaskThrottlingInputStream extends FilterInputStream {
    private final LocalTaskRateLimiterService limiter;
    private final ObjectListMessage message;
    private final WorkerTaskBandwidthService taskBandwidth;

    public LocalTaskThrottlingInputStream(InputStream input, LocalTaskRateLimiterService limiter,
                                          ObjectListMessage message) {
        this(input, limiter, message, null);
    }

    public LocalTaskThrottlingInputStream(InputStream input, LocalTaskRateLimiterService limiter,
                                          ObjectListMessage message, WorkerTaskBandwidthService taskBandwidth) {
        super(input);
        this.limiter = limiter;
        this.message = message;
        this.taskBandwidth = taskBandwidth;
    }

    @Override
    public int read() throws IOException {
        int value = super.read();
        if (value >= 0 && useLegacyLimit()) limiter.acquireBandwidth(message, 1);
        return value;
    }

    @Override
    public int read(byte[] bytes, int offset, int length) throws IOException {
        int read = super.read(bytes, offset, length);
        if (read > 0 && useLegacyLimit()) limiter.acquireBandwidth(message, read);
        return read;
    }

    private boolean useLegacyLimit() {
        return taskBandwidth == null || (message.getTaskBandwidthScopeId() == null && !taskBandwidth.hasPolicy(message));
    }
}
