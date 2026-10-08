package com.transfer.tools.service;

import com.google.common.util.concurrent.RateLimiter;
import com.transfer.tools.config.RateLimitConfig;
import com.transfer.tools.model.RateLimitAdjustment;
import com.transfer.tools.model.RateLimitRequest;
import com.transfer.tools.model.RocksDBPage;
import com.transfer.tools.storage.RocksDBService;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import javax.annotation.PostConstruct;
import javax.annotation.PreDestroy;
import java.util.ArrayList;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.UUID;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicLong;

@Slf4j
@Service
public class RateLimiterManager {

    private static final String RATE_ADJUSTMENT_PREFIX = "rate_adjustment:";
    private static final String RATE_ADJUSTMENT_COUNT_KEY = "rate_adjustment:count";
    private static final String MANUAL_RATE_PREFIX = "rate_limit:manual:";
    private static final double EPSILON = 0.01;
    private static final int MAX_RECENT_EVENTS = 100;
    private final String processId = UUID.randomUUID().toString();
    private final Map<String, Double> configuredTargets = new HashMap<>();
    private final Map<String, String> activeIncidents = new HashMap<>();
    private String current503Incident;
    private RateLimitRequest current503Request;
    private static final String EVENT_PREFIX = "rate_event:";
    private static final String INCIDENT_PREFIX = "rate_incident:";
    private static final String INCIDENT_EVENT_PREFIX = "rate_incident_event:";
    private static final String INCIDENT_GAP_PREFIX = "rate_incident_gap:";
    private final java.util.Set<String> incompleteIncidents = new java.util.HashSet<>();
    private final Map<String, RateLimitAdjustment> lastAdjustments = new HashMap<>();
    private final Deque<RateLimitAdjustment> recentEvents = new ArrayDeque<>();
    private long adjustmentSequence;
    private int consecutive503Count;
    private Long last503At;
    private Long nextRecoveryAt;

    @Autowired
    private RateLimitConfig rateLimitConfig;

    @Autowired
    private RocksDBService rocksDBService;

    private double rateIncreaseFactor;
    private long autoAdjustIntervalMs;

    private volatile RateLimiter listRateLimiter;
    private volatile RateLimiter putRateLimiter;
    private volatile RateLimiter headRateLimiter;
    private volatile RateLimiter bandwidthRateLimiter;

    private volatile double manualListQps = -1;
    private volatile double manualPutQps = -1;
    private volatile double manualHeadQps = -1;
    private volatile double manualBandwidthBps = -1;

    private AtomicLong listRequestCount = new AtomicLong(0);
    private AtomicLong putRequestCount = new AtomicLong(0);
    private AtomicLong headRequestCount = new AtomicLong(0);
    private AtomicLong bandwidthBitsCount = new AtomicLong(0);

    private Long lastStatTime = System.currentTimeMillis();
    private volatile double listCurrentQps = 0.0;
    private volatile double putCurrentQps = 0.0;
    private volatile double headCurrentQps = 0.0;
    private volatile double currentBandwidth = 0.0;

    private Thread autoAdjustThread;
    private Thread statThread;
    private final AtomicBoolean running = new AtomicBoolean(true);

    @PostConstruct
    public synchronized void init() {
        rateIncreaseFactor = rateLimitConfig.getAutoAdjust().getIncreaseFactor();
        autoAdjustIntervalMs = rateLimitConfig.getAutoAdjust().getIntervalMs();
        // Read every override before starting threads; a corrupt saved limit must not
        // silently fall back to a higher YAML limit or to unlimited throughput.
        double listQps = loadManualRate("list", rateLimitConfig.getSrc() == null
                || rateLimitConfig.getSrc().getQps() == null ? null : rateLimitConfig.getSrc().getQps().getList());
        double putQps = loadManualRate("put", rateLimitConfig.getDest() == null
                || rateLimitConfig.getDest().getQps() == null ? null : rateLimitConfig.getDest().getQps().getPut());
        double headQps = loadManualRate("head", rateLimitConfig.getDest() == null
                || rateLimitConfig.getDest().getQps() == null ? null : rateLimitConfig.getDest().getQps().getHead());
        double bandwidth = loadManualRate("bandwidth", rateLimitConfig.getBandwidth());
        listRateLimiter = applyRate(null, listQps);
        putRateLimiter = applyRate(null, putQps);
        headRateLimiter = applyRate(null, headQps);
        bandwidthRateLimiter = applyRate(null, bandwidth);
        manualListQps = listQps;
        manualPutQps = putQps;
        manualHeadQps = headQps;
        manualBandwidthBps = bandwidth;
        log.info("节点限速已初始化: list={}, put={}, head={}, bandwidth={} bps (0为不限速)",
                listQps, putQps, headQps, bandwidth);

        log.info("限速自动调整配置: increaseFactor={}, intervalMs={}, reduceFactor={}, maxRetryOn503={}, retryDelayMs={}, minPutQps={}",
            rateIncreaseFactor, autoAdjustIntervalMs,
            rateLimitConfig.getAutoAdjust().getReduceFactor(),
            rateLimitConfig.getAutoAdjust().getMaxRetryOn503(),
            rateLimitConfig.getAutoAdjust().getRetryDelayMs(),
            rateLimitConfig.getAutoAdjust().getMinPutQps());

        running.set(true);
        nextRecoveryAt = System.currentTimeMillis() + autoAdjustIntervalMs;
        statThread = new Thread(this::statQps, "rate-limiter-stat");
        statThread.setDaemon(true);
        statThread.start();

        autoAdjustThread = new Thread(this::autoAdjustRateLimit, "rate-limiter-auto-adjust");
        autoAdjustThread.setDaemon(true);
        autoAdjustThread.start();
        log.info("限速器自动调整线程已启动");
    }

    public void acquireList() {
        RateLimiter limiter = listRateLimiter;
        if (limiter != null) {
            limiter.acquire();
        }
        listRequestCount.incrementAndGet();
    }

    public void acquirePut() {
        RateLimiter limiter = putRateLimiter;
        if (limiter != null) {
            limiter.acquire();
        }
        putRequestCount.incrementAndGet();
    }

    public void acquireHead() {
        RateLimiter limiter = headRateLimiter;
        if (limiter != null) {
            limiter.acquire();
        }
        headRequestCount.incrementAndGet();
    }

    public void acquireBandwidth(int bytes) {
        RateLimiter limiter = bandwidthRateLimiter;
        if (limiter != null) {
            int bits = bytes * 8;
            limiter.acquire(bits);
        }
        bandwidthBitsCount.addAndGet((long) bytes * 8);
    }

    private void statQps() {
        while (running.get()) {
            try {
                Thread.sleep(5000);

                long now = System.currentTimeMillis();
                long elapsed = now - lastStatTime;

                if (elapsed > 0) {
                    long listCount = listRequestCount.getAndSet(0);
                    long putCount = putRequestCount.getAndSet(0);
                    long headCount = headRequestCount.getAndSet(0);
                    long bandwidthBits = bandwidthBitsCount.getAndSet(0);

                    synchronized (this) {
                        listCurrentQps = listCount * 1000.0 / elapsed;
                        putCurrentQps = putCount * 1000.0 / elapsed;
                        headCurrentQps = headCount * 1000.0 / elapsed;
                        currentBandwidth = bandwidthBits * 1000.0 / elapsed;
                        lastStatTime = now;
                    }
                }
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                break;
            }
        }
    }

    public synchronized Map<String, Object> getRateLimitStatus() {
        Map<String, Object> status = new HashMap<>();
        status.put("list", rateStatus("list", listRateLimiter, manualListQps, listCurrentQps));
        status.put("put", rateStatus("put", putRateLimiter, manualPutQps, putCurrentQps));
        status.put("head", rateStatus("head", headRateLimiter, manualHeadQps, headCurrentQps));
        status.put("bandwidth", rateStatus("bandwidth", bandwidthRateLimiter, manualBandwidthBps, currentBandwidth));
        RateLimitConfig.AutoAdjustConfig config = getAutoAdjustConfig();
        Map<String, Object> auto = new HashMap<>();
        auto.put("processId", processId);
        auto.put("observedAt", System.currentTimeMillis());
        auto.put("measurementIntervalMs", 5000);
        auto.put("lastMeasurementAt", lastStatTime);
        auto.put("intervalMs", autoAdjustIntervalMs);
        auto.put("increaseFactor", rateIncreaseFactor);
        auto.put("reduceFactor", config.getReduceFactor());
        auto.put("minQps", config.getMinPutQps());
        auto.put("consecutive503Threshold", config.getConsecutive503Threshold());
        auto.put("consecutive503Count", consecutive503Count);
        auto.put("last503At", last503At);
        auto.put("nextRecoveryAt", running.get() ? nextRecoveryAt : null);
        auto.put("sequence", adjustmentSequence);
        auto.put("events", new ArrayList<>(recentEvents));
        status.put("autoAdjust", auto);
        return status;
    }

    private Map<String, Object> rateStatus(String type, RateLimiter limiter, double target, double measured) {
        Map<String, Object> result = new HashMap<>();
        String unit = "bandwidth".equals(type) ? "Bps" : "Qps";
        double current = rateOf(limiter);
        result.put("limit" + unit, limiter == null ? -1 : Math.round(current * 100.0) / 100.0);
        result.put("current" + unit, Math.round(measured * 100.0) / 100.0);
        result.put("manualLimit" + unit, target);
        double configured = configuredTargets.get(type);
        result.put("configuredLimit" + unit, configured);
        result.put("limitSource", Double.compare(target, configured) == 0 ? "CONFIG" : "PERSISTED_MANUAL");
        RateLimitAdjustment last = lastAdjustments.get(type);
        String state = "NORMAL";
        if (target <= 0) state = "UNLIMITED";
        else if (current < target - EPSILON) {
            state = last != null && "RECOVERY_STEP".equals(last.getReasonCode()) ? "RECOVERING" : "REDUCED";
        } else if (last != null && "RECOVERED".equals(last.getReasonCode())) state = "RECOVERED";
        result.put("state", state);
        result.put("lastAdjustment", last);
        return result;
    }

    private void recordAdjustment(String type, double previous, double value, double previousTarget,
                                  double target, boolean manual, String reason) {
        if (Double.compare(previous, value) == 0 && Double.compare(previousTarget, target) == 0) return;
        RateLimitAdjustment adjustment = new RateLimitAdjustment(type, value, manual, System.currentTimeMillis());
        adjustment.setProcessId(processId);
        adjustment.setSequence(++adjustmentSequence);
        adjustment.setId(processId + ":" + adjustmentSequence);
        adjustment.setPreviousValue(previous);
        adjustment.setPreviousManualTarget(previousTarget);
        adjustment.setManualTarget(target);
        adjustment.setReasonCode(reason);
        adjustment.setIntervalMs(autoAdjustIntervalMs);
        adjustment.setIncreaseFactor(rateIncreaseFactor);
        if ("OSS_503".equals(reason)) {
            adjustment.setIncidentId(current503Incident);
            adjustment.setTriggerRequest(current503Request == null ? null : current503Request.copy());
            activeIncidents.put(type, current503Incident);
        } else {
            adjustment.setIncidentId(activeIncidents.get(type));
        }
        if (manual || "RECOVERED".equals(reason)) activeIncidents.remove(type);
        if ("OSS_503".equals(reason)) {
            adjustment.setConsecutive503Count(consecutive503Count);
            adjustment.setConsecutive503Threshold(getAutoAdjustConfig().getConsecutive503Threshold());
            adjustment.setFactor(getAutoAdjustConfig().getReduceFactor());
            adjustment.setMinQps(getAutoAdjustConfig().getMinPutQps());
        } else if ("RECOVERY_STEP".equals(reason) || "RECOVERED".equals(reason)) {
            adjustment.setFactor(rateIncreaseFactor);
        }
        lastAdjustments.put(type, adjustment);
        recentEvents.addLast(adjustment);
        while (recentEvents.size() > MAX_RECENT_EVENTS) recentEvents.removeFirst();
        try {
            Long count = rocksDBService.get(RATE_ADJUSTMENT_COUNT_KEY, Long.class);
            long newIndex = count == null ? 1L : count + 1;
            Map<String, Object> records = new HashMap<>();
            adjustment.setDetailsAvailable(true);
            records.put(RATE_ADJUSTMENT_PREFIX + newIndex, adjustment);
            records.put(RATE_ADJUSTMENT_COUNT_KEY, newIndex);
            records.put(EVENT_PREFIX + adjustment.getId(), adjustment);
            if (adjustment.getIncidentId() != null) {
                String incidentKey = INCIDENT_PREFIX + adjustment.getIncidentId();
                if ("OSS_503".equals(reason) && rocksDBService.get(incidentKey, String.class) == null) {
                    records.put(incidentKey, adjustment.getId());
                }
                if (incompleteIncidents.contains(adjustment.getIncidentId())) {
                    records.put(INCIDENT_GAP_PREFIX + adjustment.getIncidentId(), true);
                }
                records.put(INCIDENT_EVENT_PREFIX + adjustment.getIncidentId() + ":"
                        + String.format(java.util.Locale.ROOT, "%020d", adjustment.getSequence()), adjustment.getId());
            }
            rocksDBService.putBatchSync(records);
            log.debug("记录限速调整: type={}, value={}, reason={}", type, value, reason);
        } catch (Exception e) {
            adjustment.setDetailsAvailable(false);
            if (adjustment.getIncidentId() != null) incompleteIncidents.add(adjustment.getIncidentId());
            log.error("记录限速调整失败", e);
        }
    }

    // The counter and both limit changes share the status snapshot lock. The same
    // migration success paths still reset this counter; this is not a file count.
    public synchronized int onOss503SlowDown() {
        return onOss503SlowDown(null);
    }

    public synchronized int onOss503SlowDown(RateLimitRequest triggerRequest) {
        consecutive503Count++;
        last503At = System.currentTimeMillis();
        RateLimitConfig.AutoAdjustConfig config = getAutoAdjustConfig();
        if (consecutive503Count >= config.getConsecutive503Threshold()) {
            current503Incident = activeIncidents.get("put");
            if (current503Incident == null) current503Incident = activeIncidents.get("head");
            if (current503Incident == null) current503Incident = UUID.randomUUID().toString();
            current503Request = triggerRequest;
            try {
                reduceFor503("put", putRateLimiter, config);
                reduceFor503("head", headRateLimiter, config);
            } finally {
                current503Incident = null;
                current503Request = null;
            }
        }
        return consecutive503Count;
    }

    /** Stable detail lookup, independent of the 100-event process notification ring. */
    public synchronized Map<String, Object> getAdjustmentDetail(String eventId, String cursor) {
        if (eventId == null || !eventId.matches("[0-9a-fA-F-]{36}:[0-9]{1,20}")) {
            throw new IllegalArgumentException("无效的调节事件 ID");
        }
        RateLimitAdjustment event = rocksDBService.get(EVENT_PREFIX + eventId, RateLimitAdjustment.class);
        if (event == null) throw new IllegalArgumentException("该调节详情未记录，可能来自不支持详情的旧版本");
        Map<String, Object> detail = new HashMap<>();
        detail.put("event", event);
        detail.put("incidentId", event.getIncidentId());
        detail.put("processRestarted", !processId.equals(event.getProcessId()));
        detail.put("incidentActive", event.getIncidentId() != null && activeIncidents.containsValue(event.getIncidentId()));
        if (event.getIncidentId() == null) {
            detail.put("triggerEvent", null);
            detail.put("historyComplete", true);
            detail.put("events", java.util.Collections.singletonList(event));
            detail.put("nextCursor", null);
            detail.put("done", true);
            return detail;
        }
        String triggerId = rocksDBService.get(INCIDENT_PREFIX + event.getIncidentId(), String.class);
        RateLimitAdjustment trigger = triggerId == null ? null : rocksDBService.get(EVENT_PREFIX + triggerId, RateLimitAdjustment.class);
        detail.put("triggerEvent", trigger);
        detail.put("historyComplete", trigger != null && !incompleteIncidents.contains(event.getIncidentId())
                && !Boolean.TRUE.equals(rocksDBService.get(INCIDENT_GAP_PREFIX + event.getIncidentId(), Boolean.class)));
        RocksDBPage<String> page = rocksDBService.scanPage(INCIDENT_EVENT_PREFIX + event.getIncidentId() + ":", cursor, 200, String.class);
        List<RateLimitAdjustment> timeline = new ArrayList<>();
        for (String id : page.getItems().values()) {
            RateLimitAdjustment item = rocksDBService.get(EVENT_PREFIX + id, RateLimitAdjustment.class);
            if (item == null) throw new IllegalStateException("调节历史记录缺失");
            timeline.add(item);
        }
        detail.put("events", timeline);
        detail.put("nextCursor", page.getNextCursor());
        detail.put("done", page.isDone());
        return detail;
    }

    public synchronized void resetConsecutive503Count() {
        consecutive503Count = 0;
    }

    private void reduceFor503(String type, RateLimiter limiter, RateLimitConfig.AutoAdjustConfig config) {
        double current = rateOf(limiter);
        if (current > config.getMinPutQps()) {
            double next = Math.max(current * config.getReduceFactor(), config.getMinPutQps());
            updateRate(type, next, false, "OSS_503");
            log.warn("检测到连续503错误，{} QPS从 {} 自动降低到 {}", type, current, next);
        }
    }

    public synchronized List<RateLimitAdjustment> getAdjustmentHistory(Integer limit) {
        if (limit == null || limit <= 0) {
            limit = 20;
        }

        List<RateLimitAdjustment> adjustments = new ArrayList<>();

        try {
            Long count = rocksDBService.get(RATE_ADJUSTMENT_COUNT_KEY, Long.class);
            if (count == null || count == 0) {
                return adjustments;
            }

            int fetched = 0;
            for (long i = count; i > 0 && fetched < limit; i--) {
                String key = RATE_ADJUSTMENT_PREFIX + i;
                RateLimitAdjustment adjustment = rocksDBService.get(key, RateLimitAdjustment.class);
                if (adjustment != null) {
                    if (adjustment.getDetailsAvailable() == null) adjustment.setDetailsAvailable(false);
                    adjustments.add(adjustment);
                    fetched++;
                }
            }
        } catch (Exception e) {
            log.error("获取限速调整历史失败", e);
            throw new IllegalStateException("限速调整历史读取失败", e);
        }

        return adjustments;
    }

    public boolean updateListRateLimit(double qps) {
        return updateListRateLimit(qps, true);
    }

    public synchronized boolean updateListRateLimit(double qps, boolean isManual) {
        return updateRate("list", qps, isManual, isManual ? "MANUAL_SET" : "AUTO_ADJUST");
    }

    public boolean updatePutRateLimit(double qps) {
        return updatePutRateLimit(qps, true);
    }

    public synchronized boolean updatePutRateLimit(double qps, boolean isManual) {
        return updateRate("put", qps, isManual, isManual ? "MANUAL_SET" : "AUTO_ADJUST");
    }

    public boolean updateHeadRateLimit(double qps) {
        return updateHeadRateLimit(qps, true);
    }

    public synchronized boolean updateHeadRateLimit(double qps, boolean isManual) {
        return updateRate("head", qps, isManual, isManual ? "MANUAL_SET" : "AUTO_ADJUST");
    }

    public RateLimiter getListRateLimiter() {
        return listRateLimiter;
    }

    public RateLimiter getPutRateLimiter() {
        return putRateLimiter;
    }

    public RateLimiter getHeadRateLimiter() {
        return headRateLimiter;
    }

    public RateLimiter getBandwidthRateLimiter() {
        return bandwidthRateLimiter;
    }

    public boolean updateBandwidthRateLimit(double bps) {
        return updateBandwidthRateLimit(bps, true);
    }

    public synchronized boolean updateBandwidthRateLimit(double bps, boolean isManual) {
        return updateRate("bandwidth", bps, isManual, isManual ? "MANUAL_SET" : "AUTO_ADJUST");
    }

    private boolean updateRate(String type, double requested, boolean manual, String reason) {
        double rate = normalizeRate(requested);
        RateLimiter limiter;
        double target;
        switch (type) {
            case "list": limiter = listRateLimiter; target = manualListQps; break;
            case "put": limiter = putRateLimiter; target = manualPutQps; break;
            case "head": limiter = headRateLimiter; target = manualHeadQps; break;
            case "bandwidth": limiter = bandwidthRateLimiter; target = manualBandwidthBps; break;
            default: throw new IllegalArgumentException("Unsupported limit: " + type);
        }
        if (manual) persistManualRate(type, rate);
        else if (target <= 0) return true;
        else rate = Math.min(rate, target);
        double previous = rateOf(limiter);
        limiter = applyRate(limiter, rate);
        double nextTarget = manual ? rate : target;
        switch (type) {
            case "list": listRateLimiter = limiter; manualListQps = nextTarget; break;
            case "put": putRateLimiter = limiter; manualPutQps = nextTarget; break;
            case "head": headRateLimiter = limiter; manualHeadQps = nextTarget; break;
            case "bandwidth": bandwidthRateLimiter = limiter; manualBandwidthBps = nextTarget; break;
            default: throw new IllegalArgumentException("Unsupported limit: " + type);
        }
        recordAdjustment(type, previous, rate, target, nextTarget, manual, reason);
        return true;
    }

    private double rateOf(RateLimiter limiter) {
        return limiter == null ? 0 : limiter.getRate();
    }

    private double loadManualRate(String type, Number configured) {
        String key = MANUAL_RATE_PREFIX + type;
        double configuredTarget = normalizeRate(configured == null ? 0 : configured.doubleValue());
        configuredTargets.put(type, configuredTarget);
        try {
            Double saved = rocksDBService.get(key, Double.class);
            if (saved == null) {
                if (rocksDBService.exists(key)) {
                    throw new IllegalStateException("saved rate is null");
                }
                return configuredTarget;
            }
            if (!Double.isFinite(saved) || saved < 0) {
                throw new IllegalStateException("saved rate must be finite and nonnegative");
            }
            return saved;
        } catch (RuntimeException e) {
            throw new IllegalStateException("无法加载已保存的 " + type + " 限速配置", e);
        }
    }

    private void persistManualRate(String type, double rate) {
        try {
            rocksDBService.putSync(MANUAL_RATE_PREFIX + type, rate);
        } catch (RuntimeException e) {
            throw new IllegalStateException(type + " 限速保存失败，原限速未改变", e);
        }
    }

    private double normalizeRate(double rate) {
        if (!Double.isFinite(rate)) {
            throw new IllegalArgumentException("限速必须是有限数值");
        }
        return rate <= 0 ? 0 : rate;
    }

    private RateLimiter applyRate(RateLimiter limiter, double rate) {
        if (rate <= 0) return null;
        if (limiter == null) return RateLimiter.create(rate);
        limiter.setRate(rate);
        return limiter;
    }

    private void autoAdjustRateLimit() {
        while (running.get()) {
            try {
                Thread.sleep(autoAdjustIntervalMs);

                increaseCurrentRates();

            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                break;
            } catch (Exception e) {
                log.error("自动调整限速值异常", e);
            }
        }
        log.info("限速器自动调整线程已停止");
    }

    private synchronized void increaseCurrentRates() {
        if (!running.get()) return;
        tryIncreaseRateLimit(listRateLimiter, manualListQps, "list");
        tryIncreaseRateLimit(putRateLimiter, manualPutQps, "put");
        tryIncreaseRateLimit(headRateLimiter, manualHeadQps, "head");
        tryIncreaseRateLimit(bandwidthRateLimiter, manualBandwidthBps, "bandwidth");
        nextRecoveryAt = System.currentTimeMillis() + autoAdjustIntervalMs;
    }

    private void tryIncreaseRateLimit(RateLimiter rateLimiter, double targetRate, String type) {
        if (rateLimiter == null || targetRate <= 0) {
            return;
        }

        double currentRate = rateLimiter.getRate();
        if (currentRate >= targetRate - EPSILON) {
            return;
        }

        double newRate = Math.min(currentRate * rateIncreaseFactor, targetRate);
        if (newRate > currentRate + EPSILON) {
            rateLimiter.setRate(newRate);
            recordAdjustment(type, currentRate, newRate, targetRate, targetRate, false,
                    newRate >= targetRate - EPSILON ? "RECOVERED" : "RECOVERY_STEP");
            log.info("{}限速器自动回升: {} -> {} (目标: {})", type,
                String.format("%.2f", currentRate),
                String.format("%.2f", newRate),
                String.format("%.2f", targetRate));
        }
    }

    @PreDestroy
    public void destroy() {
        running.set(false);
        stopThread(autoAdjustThread);
        stopThread(statThread);
        log.info("RateLimiterManager已关闭");
    }

    private void stopThread(Thread thread) {
        if (thread == null) return;
        thread.interrupt();
        try {
            thread.join(1000);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    public RateLimitConfig.AutoAdjustConfig getAutoAdjustConfig() {
        return rateLimitConfig.getAutoAdjust();
    }
}
