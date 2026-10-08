package com.transfer.tools.utils;

import com.google.common.util.concurrent.RateLimiter;

import java.io.IOException;
import java.io.InputStream;

public class InputStreamFlow extends InputStream {
    private final InputStream inputStream;
    private final RateLimiter bandwidthLimiter;

    public InputStreamFlow(InputStream inputStream, RateLimiter bandwidthLimiter) {
        this.inputStream = inputStream;
        this.bandwidthLimiter = bandwidthLimiter;
    }

    @Override
    public int read() throws IOException {
        if (bandwidthLimiter != null) {
            bandwidthLimiter.acquire(1);
        }
        return inputStream.read();
    }

    @Override
    public int read(byte[] b) throws IOException {
        if (bandwidthLimiter != null) {
            bandwidthLimiter.acquire(b.length);
        }
        return inputStream.read(b);
    }

    @Override
    public int read(byte[] b, int off, int len) throws IOException {
        if (bandwidthLimiter != null) {
            bandwidthLimiter.acquire(len);
        }
        return inputStream.read(b, off, len);
    }

    @Override
    public void close() throws IOException {
        inputStream.close();
    }

    @Override
    public int available() throws IOException {
        return inputStream.available();
    }
}
