package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.LocalFileEntry;
import com.transfer.tools.model.LocalNodeBinding;
import com.transfer.tools.model.LocalTransferRequest;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;

import java.util.Collections;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.fail;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.header;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo;
import static org.springframework.test.web.client.response.MockRestResponseCreators.withSuccess;

public class LocalTransferServiceDispatchTest {

    @Test
    public void workerBatchDispatchIncludesInternalToken() {
        LocalTransferService service = service("cluster-secret");
        RestTemplate restTemplate = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(restTemplate).build();
        server.expect(requestTo("http://10.0.0.2:8081/api/worker/local-transfers/batches"))
                .andExpect(header("X-Internal-Token", "cluster-secret"))
                .andRespond(withSuccess("{\"success\":true}", MediaType.APPLICATION_JSON));

        service.dispatchBatch(request(), bindings(), 0,
                Collections.singletonList(new LocalFileEntry("a.txt", 1L, 2L)), true);

        server.verify();
    }

    @Test
    public void workerRejectionPreservesValidationCodeAndMessage() {
        LocalTransferService service = service("cluster-secret");
        RestTemplate restTemplate = (RestTemplate) ReflectionTestUtils.getField(service, "restTemplate");
        MockRestServiceServer server = MockRestServiceServer.bindTo(restTemplate).build();
        server.expect(requestTo("http://10.0.0.2:8081/api/worker/local-transfers/batches"))
                .andRespond(withSuccess("{\"success\":false,\"code\":\"LOCAL_PATH_NOT_READABLE\","
                        + "\"message\":\"source cannot be read\"}", MediaType.APPLICATION_JSON));

        try {
            service.dispatchBatch(request(), bindings(), 0, Collections.<LocalFileEntry>emptyList(), true);
            fail("expected worker rejection");
        } catch (DownstreamWorkerException error) {
            assertEquals("LOCAL_PATH_NOT_READABLE", error.getCode());
            assertEquals("source cannot be read", error.getMessage());
        }
        server.verify();
    }

    private LocalTransferService service(String token) {
        LocalTransferService service = new LocalTransferService();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getLocalIp()).thenReturn("10.0.0.1");
        when(config.getPort()).thenReturn(8081);
        when(config.getInternalToken()).thenReturn(token);
        ReflectionTestUtils.setField(service, "distributedConfig", config);
        return service;
    }

    private LocalTransferRequest request() {
        LocalTransferRequest request = new LocalTransferRequest();
        request.setParentTaskId(86);
        request.setDistributionMode("balanced");
        return request;
    }

    private java.util.List<LocalNodeBinding> bindings() {
        return Collections.singletonList(new LocalNodeBinding("10.0.0.2", "/data/source"));
    }
}
