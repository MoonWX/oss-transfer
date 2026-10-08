package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

import java.text.SimpleDateFormat;
import java.util.Date;
import java.util.TimeZone;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class FallbackObject {
    private Integer taskIndex;
    private String sourceBucket;
    private String destBucket;
    private String key;
    /** Actual OSS destination; key remains the source identity for retries. */
    private String destinationKey;
    private Long timestamp;
    private String errorMessage;
    private String tmpFilePath;

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
