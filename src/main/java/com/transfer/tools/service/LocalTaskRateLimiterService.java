package com.transfer.tools.service;

import com.google.common.util.concurrent.RateLimiter;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.SubTaskStatus;
import org.springframework.stereotype.Service;

import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentMap;

@Service
public class LocalTaskRateLimiterService {
    private final ConcurrentMap<String, Holder> limiters = new ConcurrentHashMap<>();

    public void acquireList(SubTaskStatus task) {
        acquire(key(task.getParentTaskId(), task.getTaskIndex(), "list"), task.getListQpsLimit(), 1);
    }

    public void acquireHead(ObjectListMessage message) {
        acquire(key(message.getParentTaskId(), message.getTaskIndex(), "head"), message.getHeadQpsLimit(), 1);
    }

    public void acquireRead(ObjectListMessage message) {
        acquire(key(message.getParentTaskId(), message.getTaskIndex(), "list"), message.getListQpsLimit(), 1);
    }

    public void acquirePut(ObjectListMessage message) {
        acquire(key(message.getParentTaskId(), message.getTaskIndex(), "put"), message.getPutQpsLimit(), 1);
    }

    public void acquireBandwidth(ObjectListMessage message, int bytes) {
        if (bytes <= 0) return;
        acquire(key(message.getParentTaskId(), message.getTaskIndex(), "bandwidth"),
                message.getBandwidthBitsPerSecond(), bytes * 8);
    }

    private void acquire(String key, Double rate, int permits) {
        if (rate == null || rate <= 0) return;
        Holder holder = limiters.get(key);
        if (holder == null || rate < holder.rate) {
            Holder replacement = new Holder(rate, RateLimiter.create(rate));
            limiters.put(key, replacement);
            holder = replacement;
        }
        holder.limiter.acquire(permits);
    }

    private String key(Integer parentTaskId, Integer taskId, String type) {
        return (parentTaskId == null ? taskId : parentTaskId) + ":" + type;
    }

    private static class Holder {
        private final double rate;
        private final RateLimiter limiter;

        private Holder(double rate, RateLimiter limiter) {
            this.rate = rate;
            this.limiter = limiter;
        }
    }
}
