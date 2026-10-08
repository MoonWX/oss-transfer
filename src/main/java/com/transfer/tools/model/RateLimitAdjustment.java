package com.transfer.tools.model;

import lombok.Data;
import lombok.NoArgsConstructor;

import java.text.SimpleDateFormat;
import java.util.Date;
import java.util.TimeZone;

@Data
@NoArgsConstructor
public class RateLimitAdjustment {
    private String type;
    private Double value;
    private Boolean manual;
    private Long timestamp;
    // Nullable for history recorded before adjustment reasons were available.
    private String id;
    private String processId;
    private Long sequence;
    private Double previousValue;
    private Double previousManualTarget;
    private Double manualTarget;
    private String reasonCode;
    private Integer consecutive503Count;
    private Integer consecutive503Threshold;
    private Double factor;
    private Integer minQps;
    private Long intervalMs;
    private Double increaseFactor;
    private String incidentId;
    private RateLimitRequest triggerRequest;
    private Boolean detailsAvailable;

    public RateLimitAdjustment(String type, Double value, Boolean manual, Long timestamp) {
        this.type = type;
        this.value = value;
        this.manual = manual;
        this.timestamp = timestamp;
    }

    private static final SimpleDateFormat DATE_FORMAT;

    static {
        DATE_FORMAT = new SimpleDateFormat("yyyy-MM-dd HH:mm:ss");
        DATE_FORMAT.setTimeZone(TimeZone.getTimeZone("Asia/Shanghai"));
    }

    public String getTimestampFormatted() {
        if (timestamp == null) {
            return null;
        }
        synchronized (DATE_FORMAT) {
            return DATE_FORMAT.format(new Date(timestamp));
        }
    }
}
