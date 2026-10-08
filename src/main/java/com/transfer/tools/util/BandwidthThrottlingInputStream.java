package com.transfer.tools.util;

import com.transfer.tools.service.RateLimiterManager;
import lombok.extern.slf4j.Slf4j;

import java.io.IOException;
import java.io.InputStream;

@Slf4j
public class BandwidthThrottlingInputStream extends InputStream {

    private final InputStream delegate;
    private final RateLimiterManager rateLimiterManager;
    private long totalBytesRead = 0;

    public BandwidthThrottlingInputStream(InputStream delegate, RateLimiterManager rateLimiterManager) {
        this.delegate = delegate;
        this.rateLimiterManager = rateLimiterManager;
    }

    @Override
    public int read() throws IOException {
        int data = delegate.read();
        if (data != -1) {
            totalBytesRead++;
            rateLimiterManager.acquireBandwidth(1);
        }
        return data;
    }

    @Override
    public int read(byte[] b, int off, int len) throws IOException {
        int bytesRead = delegate.read(b, off, len);
        if (bytesRead > 0) {
            totalBytesRead += bytesRead;
            rateLimiterManager.acquireBandwidth(bytesRead);
        }
        return bytesRead;
    }

    @Override
    public int read(byte[] b) throws IOException {
        return read(b, 0, b.length);
    }

    @Override
    public long skip(long n) throws IOException {
        return delegate.skip(n);
    }

    @Override
    public int available() throws IOException {
        return delegate.available();
    }

    @Override
    public void close() throws IOException {
        delegate.close();
    }

    @Override
    public synchronized void mark(int readlimit) {
        delegate.mark(readlimit);
    }

    @Override
    public synchronized void reset() throws IOException {
        delegate.reset();
    }

    @Override
    public boolean markSupported() {
        return delegate.markSupported();
    }

    public long getTotalBytesRead() {
        return totalBytesRead;
    }
}
