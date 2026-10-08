package com.transfer.tools.service;

import com.alibaba.fastjson.JSON;
import com.transfer.tools.model.*;
import org.junit.Test;

import static org.junit.Assert.*;

public class EndpointNormalizationServiceTest {
    private final EndpointNormalizationService service = new EndpointNormalizationService();

    @Test
    public void legacyJsonDefaultsToOssToOss() {
        SubTaskStatus status = JSON.parseObject("{\"sourceBucket\":\"src\",\"destBucket\":\"dst\",\"sourceS3Alias\":\"a\",\"destS3Alias\":\"b\",\"overwrite\":false}", SubTaskStatus.class);
        service.normalize(status);
        assertEquals(TransferDirection.OSS_TO_OSS, status.getDirection());
        assertEquals(EndpointType.OSS, status.getSourceEndpoint().getType());
        assertEquals("src", status.getSourceEndpoint().getBucket());
        assertEquals(ConflictPolicy.SKIP, status.getConflictPolicy());
    }

    @Test(expected = TransferValidationException.class)
    public void rejectsDirectionEndpointMismatch() {
        LocalTransferRequest request = new LocalTransferRequest();
        request.setDirection(TransferDirection.OSS_TO_LOCAL);
        request.setSourceEndpoint(service.local("127.0.0.1", "/tmp/source"));
        request.setDestinationEndpoint(service.local("127.0.0.1", "/tmp/destination"));
        service.normalizeAndValidate(request);
    }

    @Test
    public void canonicalOssPairBackfillsLegacyFields() {
        BucketPair pair = new BucketPair();
        pair.setDirection(TransferDirection.OSS_TO_OSS);
        pair.setSourceEndpoint(service.oss("source", "bucket-a", null, "prefix/"));
        pair.setDestinationEndpoint(service.oss("destination", "bucket-b", null, null));
        service.normalize(pair);
        assertEquals("source", pair.getSourceS3Alias());
        assertEquals("bucket-a", pair.getSourceBucket());
        assertEquals("prefix/", pair.getPrefix());
        assertFalse(pair.getOverwrite());
    }

    @Test
    public void multiNodeLocalRequestDefaultsToBalanced() {
        LocalTransferRequest request = new LocalTransferRequest();
        request.setDirection(TransferDirection.OSS_TO_LOCAL);
        request.setSourceEndpoint(service.oss("source", "bucket-a", null, "photos/"));
        TransferEndpoint local = service.local("10.0.0.1", "/data/photos");
        local.setStorageId("shared-photos");
        local.setNodeBindings(java.util.Arrays.asList(
                new LocalNodeBinding("10.0.0.1", "/data/photos"),
                new LocalNodeBinding("10.0.0.2", "/mnt/photos")));
        request.setDestinationEndpoint(local);

        service.normalizeAndValidate(request);

        assertEquals("balanced", request.getDistributionMode());
    }

    @Test
    public void explicitBalancedModeIsNotRewrittenForSingleBinding() {
        LocalTransferRequest request = new LocalTransferRequest();
        request.setDirection(TransferDirection.OSS_TO_LOCAL);
        request.setSourceEndpoint(service.oss("source", "bucket-a", null, "photos/"));
        request.setDestinationEndpoint(service.local("10.0.0.1", "/data/photos"));
        request.setDistributionMode("balanced");

        service.normalizeAndValidate(request);

        assertEquals("balanced", request.getDistributionMode());
    }

    @Test
    public void singleBindingWithoutModeDefaultsToAffinity() {
        LocalTransferRequest request = new LocalTransferRequest();
        request.setDirection(TransferDirection.OSS_TO_LOCAL);
        request.setSourceEndpoint(service.oss("source", "bucket-a", null, "photos/"));
        request.setDestinationEndpoint(service.local("10.0.0.1", "/data/photos"));

        service.normalizeAndValidate(request);

        assertEquals("affinity", request.getDistributionMode());
    }
}
