package com.transfer.tools.service;

import com.amazonaws.services.s3.model.AmazonS3Exception;
import com.transfer.tools.model.RateLimitRequest;
import org.junit.Test;

import static org.junit.Assert.*;

public class RateLimitRequestTest {
    @Test
    public void captureKeepsTheActual503CodeAndCopiesTaskContextWithoutMutatingIt() {
        RateLimitRequest scope = new RateLimitRequest();
        scope.setTaskId(48);
        scope.setDirection("OSS_TO_OSS");
        scope.setSourceBucket("source-bucket");
        scope.setDestinationBucket("target-bucket");
        AmazonS3Exception error = error("Service temporarily unavailable");
        RateLimitRequest captured = RateLimitRequest.capture(new RuntimeException("outer wrapper", error),
                "UPLOAD_PART", "PUT", "target", "target-bucket", "nested/数据.bin", 2, 3, "UPLOAD_PART_RETRY", scope);

        assertEquals(Integer.valueOf(503), captured.getStatusCode());
        assertEquals("ServiceUnavailable", captured.getErrorCode());
        assertEquals("request-id", captured.getRequestId());
        assertEquals("extended-request-id", captured.getExtendedRequestId());
        assertEquals(Integer.valueOf(48), captured.getTaskId());
        assertEquals("OSS_TO_OSS", captured.getDirection());
        assertEquals("source-bucket", captured.getSourceBucket());
        assertEquals("target-bucket", captured.getDestinationBucket());
        assertEquals("REQUEST_CONTEXT", captured.getContextSource());
        assertEquals(Integer.valueOf(2), captured.getPartNumber());
        assertEquals(Integer.valueOf(3), captured.getAttempt());
        assertNull(scope.getOperation());
        assertNull(scope.getRequestId());
        captured.setTaskId(49);
        assertEquals(Integer.valueOf(48), scope.getTaskId());
    }

    @Test
    public void ambiguousTransferContextDoesNotInventAnHttpMethodOrEndpoint() {
        RateLimitRequest captured = RateLimitRequest.capture(error("Slow service"), "TRANSFER_OBJECT", null,
                null, null, "key", null, 1, "OUTER_RETHROW", null);
        assertEquals("TRANSFER_CONTEXT", captured.getContextSource());
        assertNull(captured.getMethod());
        assertNull(captured.getBucket());
        assertNull(captured.getConnectionAlias());
    }

    @Test
    public void sensitiveAuthorizationAndCredentialMessagesAreNotPersisted() {
        String[] messages = {
                "Authorization: Bearer bearer.secret.jwt",
                "Authorization=\"Bearer quoted secret jwt\"",
                "Credential = \"quoted credential value\"",
                "AccessKeyId: access-key-secret-value",
                "x-amz-security-token=temporary-secret-token",
                "x-oss-signature=signature-secret-value",
                "GET https://example.test/object?X-Amz-Credential=secret-credential&X-Amz-Signature=secret-signature",
        };
        for (String message : messages) {
            String captured = RateLimitRequest.capture(error(message), "PUT_OBJECT", "PUT", "target", "bucket",
                    "key", null, 1, "TRANSFER_RETRY", null).getMessage();
            assertFalse(message, captured.contains("secret"));
            assertFalse(message, captured.contains("quoted credential value"));
            assertFalse(message, captured.contains("https://"));
        }
    }

    @Test
    public void urlsControlCharactersAndOversizedServiceValuesAreSanitized() {
        String message = "Upstream https://example.test/private/resource failed\r\n\tTry later\u0000";
        String safe = RateLimitRequest.safe(message, 512);
        assertFalse(safe.contains("example.test"));
        assertFalse(safe.contains("\n"));
        assertFalse(safe.contains("\r"));
        assertFalse(safe.contains("\t"));
        assertFalse(safe.contains("\u0000"));
        assertTrue(safe.contains("Try later"));

        String oversized = new String(new char[900]).replace('\0', 'x');
        RateLimitRequest captured = RateLimitRequest.capture(error(oversized), "PUT_OBJECT", "PUT", "target", "bucket",
                "key", null, 1, "TRANSFER_RETRY", null);
        assertTrue(captured.getMessage().length() <= 513);
        assertTrue(captured.getMessage().endsWith("…"));
        assertNull(RateLimitRequest.safe(null, 128));
    }

    private static AmazonS3Exception error(String message) {
        AmazonS3Exception error = new AmazonS3Exception(message);
        error.setStatusCode(503);
        error.setErrorCode("ServiceUnavailable");
        error.setRequestId("request-id");
        error.setExtendedRequestId("extended-request-id");
        return error;
    }
}
