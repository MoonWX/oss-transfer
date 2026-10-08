package com.transfer.tools.model;

import lombok.AllArgsConstructor;
import lombok.Data;
import lombok.NoArgsConstructor;

@Data
@AllArgsConstructor
@NoArgsConstructor
public class FallbackMessage {
    private Integer taskIndex;
    private String sourceBucket;
    private String destBucket;
    private String key;
    /** Actual OSS destination; key remains the source identity for retries. */
    private String destinationKey;
}
