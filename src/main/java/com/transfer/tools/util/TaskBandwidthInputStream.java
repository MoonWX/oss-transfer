package com.transfer.tools.util;

import com.amazonaws.event.ProgressEvent;
import com.amazonaws.event.ProgressEventType;
import com.amazonaws.event.SyncProgressListener;
import com.transfer.tools.service.WorkerTaskBandwidthService;
import com.transfer.tools.service.TransferSpeedService;
import java.io.*;

/** Throttles one transfer leg; SDK upload hashing is excluded, actual retries are included. */
public final class TaskBandwidthInputStream extends FilterInputStream {
    private static final int CHUNK_SIZE = 64 * 1024;
    private final WorkerTaskBandwidthService service;
    private final WorkerTaskBandwidthService.Scope scope;
    private volatile boolean transmitting;
    private final TransferSpeedService speed;
    private final Integer taskId;

    public TaskBandwidthInputStream(InputStream input, WorkerTaskBandwidthService service,
                                    WorkerTaskBandwidthService.Scope scope, boolean sdkUpload) {
        this(input, service, scope, sdkUpload, null, null);
    }

    public TaskBandwidthInputStream(InputStream input, WorkerTaskBandwidthService service,
                                    WorkerTaskBandwidthService.Scope scope, boolean sdkUpload,
                                    TransferSpeedService speed, Integer taskId) {
        super(input); this.service = service; this.scope = scope; this.speed = speed; this.taskId = taskId;
        transmitting = !sdkUpload;
        if (service != null) service.check(scope);
    }

    public SyncProgressListener uploadListener() {
        return new SyncProgressListener() {
            @Override public void progressChanged(ProgressEvent event) {
                ProgressEventType type = event.getEventType();
                if (type == ProgressEventType.HTTP_REQUEST_STARTED_EVENT) transmitting = true;
                else if (type == ProgressEventType.HTTP_REQUEST_COMPLETED_EVENT
                        || type == ProgressEventType.CLIENT_REQUEST_RETRY_EVENT
                        || type == ProgressEventType.CLIENT_REQUEST_FAILED_EVENT
                        || type == ProgressEventType.CLIENT_REQUEST_SUCCESS_EVENT) transmitting = false;
            }
        };
    }

    @Override public int read() throws IOException {
        boolean actualTransfer = transmitting;
        WorkerTaskBandwidthService.Reservation reservation = actualTransfer && service != null
                ? service.reserve(scope, 1) : null;
        int consumed = 0;
        try {
            if (actualTransfer && service != null) service.check(scope);
            int value = in.read();
            consumed = value < 0 ? 0 : 1;
            if (consumed > 0 && actualTransfer && speed != null) speed.record(taskId, consumed);
            return value;
        } finally {
            if (reservation != null) reservation.complete(consumed);
        }
    }

    @Override public int read(byte[] bytes, int offset, int length) throws IOException {
        if (bytes == null) throw new NullPointerException("bytes");
        if (offset < 0 || length < 0 || length > bytes.length - offset) throw new IndexOutOfBoundsException();
        if (length == 0) return 0;
        boolean actualTransfer = transmitting;
        WorkerTaskBandwidthService.Reservation reservation = actualTransfer && service != null
                ? service.reserve(scope, Math.min(length, CHUNK_SIZE)) : null;
        int count = 0;
        try {
            if (actualTransfer && service != null) service.check(scope);
            count = in.read(bytes, offset, reservation == null ? Math.min(length, CHUNK_SIZE) : reservation.getBytes());
            if (count > 0 && actualTransfer && speed != null) speed.record(taskId, count);
            return count;
        } finally {
            if (reservation != null) reservation.complete(count);
        }
    }

    @Override public long skip(long length) throws IOException {
        long remaining = Math.max(0, length); byte[] buffer = new byte[(int) Math.min(CHUNK_SIZE, remaining)];
        while (remaining > 0) {
            int count = read(buffer, 0, (int) Math.min(buffer.length, remaining));
            if (count < 0) break;
            if (count == 0) { if (read() < 0) break; count = 1; }
            remaining -= count;
        }
        return Math.max(0, length) - remaining;
    }

}
