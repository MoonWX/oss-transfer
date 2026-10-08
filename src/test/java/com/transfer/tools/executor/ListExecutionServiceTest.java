package com.transfer.tools.executor;

import com.amazonaws.services.s3.model.ListObjectsRequest;
import org.junit.Test;

import static org.junit.Assert.assertEquals;

public class ListExecutionServiceTest {

    @Test
    public void ossToLocalShardKeepsSourcePrefixAndUsesStartKeyAsMarker() {
        ListExecutionService service = new ListExecutionService();

        ListObjectsRequest request = service.createListRequest(
                "source-bucket", "photos/", "photos/000999.bin", 1000);

        assertEquals("source-bucket", request.getBucketName());
        assertEquals("photos/", request.getPrefix());
        assertEquals("photos/000999.bin", request.getMarker());
        assertEquals(Integer.valueOf(1000), request.getMaxKeys());
    }
}
