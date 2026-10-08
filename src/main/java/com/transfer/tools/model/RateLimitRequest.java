package com.transfer.tools.model;

import com.amazonaws.services.s3.model.AmazonS3Exception;
import lombok.Data;
import lombok.NoArgsConstructor;

/** Whitelisted request evidence; never stores a URL, request headers, or credentials. */
@Data
@NoArgsConstructor
public class RateLimitRequest {
    private String operation;
    private String method;
    private Integer taskId;
    private String direction;
    private String bucket;
    private String key;
    private String connectionAlias;
    private String sourceBucket;
    private String destinationBucket;
    private Integer partNumber;
    private Integer attempt;
    private Integer statusCode;
    private String errorCode;
    private String requestId;
    private String extendedRequestId;
    private String message;
    private String contextSource;
    private String handlingStage;

    public RateLimitRequest copy() {
        RateLimitRequest copy = new RateLimitRequest();
        copy.operation = operation; copy.method = method; copy.taskId = taskId; copy.direction = direction;
        copy.bucket = bucket; copy.key = key; copy.connectionAlias = connectionAlias;
        copy.sourceBucket = sourceBucket; copy.destinationBucket = destinationBucket;
        copy.partNumber = partNumber; copy.attempt = attempt; copy.statusCode = statusCode;
        copy.errorCode = errorCode; copy.requestId = requestId; copy.extendedRequestId = extendedRequestId;
        copy.message = message; copy.contextSource = contextSource; copy.handlingStage = handlingStage;
        return copy;
    }

    public static RateLimitRequest capture(Throwable error, String operation, String method, String alias,
                                           String bucket, String key, Integer part, Integer attempt,
                                           String handlingStage, RateLimitRequest scope) {
        RateLimitRequest result = scope == null ? new RateLimitRequest() : scope.copy();
        result.operation = safe(operation, 80); result.method = safe(method, 12);
        result.connectionAlias = safe(alias, 128); result.bucket = safe(bucket, 256);
        result.key = safe(key, 2048); result.partNumber = part; result.attempt = attempt;
        result.handlingStage = safe(handlingStage, 80);
        result.sourceBucket = safe(result.sourceBucket, 256);
        result.destinationBucket = safe(result.destinationBucket, 256);
        result.contextSource = method == null ? "TRANSFER_CONTEXT" : "REQUEST_CONTEXT";
        for (Throwable cause = error; cause != null; cause = cause.getCause()) {
            if (cause instanceof AmazonS3Exception) {
                AmazonS3Exception s3 = (AmazonS3Exception) cause;
                result.statusCode = s3.getStatusCode();
                result.errorCode = safe(s3.getErrorCode(), 128);
                result.requestId = safe(s3.getRequestId(), 256);
                result.extendedRequestId = safe(s3.getExtendedRequestId(), 512);
                result.message = safeMessage(s3.getErrorMessage());
                break;
            }
        }
        return result;
    }

    private static String safeMessage(String value) {
        if (value != null && value.matches("(?is).*(authorization|credential|signature|secret|password|access[-_ ]?key|session[-_ ]?token|security[-_ ]?token).*")) {
            return "[Sensitive error detail omitted]";
        }
        return safe(value, 512);
    }

    public static String safe(String value, int limit) {
        if (value == null) return null;
        // Service messages can include signed URLs or credential assignments. Redact
        // before truncating so truncation cannot expose a partial secret.
        if (value.matches("(?is).*(?:authorization|credential|signature|access[-_ ]?key(?:[-_ ]?id)?|secret[-_ ]?(?:access[-_ ]?)?key|session[-_ ]?token|security[-_ ]?token)\\s*[:=].*")) {
            return "[Sensitive error detail omitted]";
        }
        String safe = value.replaceAll("(?i)https?://[^\\s<>\\\"]+", "[URL redacted]")
                .replaceAll("(?i)(authorization|credential|signature|access[-_]?key(?:[-_]?id)?|secret[-_]?(?:access[-_]?)?key|session[-_]?token|security[-_]?token|x-amz-[a-z-]*(?:token|signature|credential)|x-oss-[a-z-]*(?:token|signature|credential))\\s*[:=]\\s*[^\\s,;]+", "$1=[redacted]")
                .replaceAll("[\\p{Cntrl}]", " ");
        return safe.length() <= limit ? safe : safe.substring(0, limit) + "…";
    }
}
