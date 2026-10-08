package com.transfer.tools.service;

import com.transfer.tools.config.DistributedConfig;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;
import java.util.*;
import static org.junit.Assert.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.requestTo;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class BalancedWorkerSchedulerTest {
    private static final List<String> NODES = Arrays.asList("10.0.0.1", "10.0.0.2");
    private BalancedWorkerScheduler scheduler;
    private MockRestServiceServer server;
    private void setup(String first, String second) {
        scheduler = new BalancedWorkerScheduler();
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getPort()).thenReturn(8081);
        ReflectionTestUtils.setField(scheduler, "distributedConfig", config);
        server = MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(scheduler, "restTemplate")).build();
        server.expect(requestTo("http://10.0.0.1:8081/api/worker/worker-status")).andRespond(withSuccess(first, MediaType.APPLICATION_JSON));
        server.expect(requestTo("http://10.0.0.2:8081/api/worker/worker-status")).andRespond(withSuccess(second, MediaType.APPLICATION_JSON));
    }
    private String status(long bandwidth, int backlog) {
        return "{\"success\":true,\"queueStatus\":{\"putQueueSize\":"+backlog+",\"listQueueSize\":0},"
                +"\"rateLimit\":{\"put\":{\"limitQps\":1000,\"currentQps\":0},\"bandwidth\":{\"limitBps\":"+bandwidth+",\"currentBps\":0}}}";
    }
    @Test public void repeatedSingleShardBatchesDoNotAlwaysChooseFirstNode() {
        setup(status(1000000000,0),status(1000000000,0));
        String first=scheduler.select(NODES, 100*1024*1024,128);
        String second=scheduler.select(NODES,100*1024*1024,128);
        assertNotEquals(first,second); server.verify();
    }
    @Test public void weightsAccountForDataBytesRatherThanOnlyTaskCount() {
        setup(status(1000000000,0),status(1000000000,0));
        assertEquals(NODES.get(0),scheduler.select(NODES,1024L*1024*1024,1));
        for(int i=0;i<6;i++) assertEquals(NODES.get(1),scheduler.select(NODES,1024*1024,1));
        server.verify();
    }
    @Test public void configuredCapacityAndQueueBacklogMatterButInstantaneousZeroDoesNot() {
        setup(status(100000000,0),status(1000000000,0));
        assertEquals(NODES.get(1),scheduler.select(NODES,100*1024*1024,1)); server.verify();
        setup(status(1000000000,25811),status(1000000000,0));
        assertEquals(NODES.get(1),scheduler.select(NODES,100*1024*1024,1)); server.verify();
    }
    @Test public void incompleteMetricsAreNotPreferredOverKnownHealthyNode() {
        setup("{\"success\":true}",status(1000000000,100));
        assertEquals(NODES.get(1),scheduler.select(NODES,1024,1)); server.verify();
    }
    @Test public void allIncompleteMetricsUseWeightedFallbackWithoutAssumingZeroMeasuredRateIsNoCapacity() {
        setup("{\"success\":true}","{\"success\":true}");
        assertNotEquals(scheduler.select(NODES,100*1024*1024,128),scheduler.select(NODES,100*1024*1024,128));
        server.verify();
    }
    @Test public void unavailableWorkerIsNeverSelectedAsEmpty() {
        setup("{\"success\":false}",status(1000000000,100));
        assertEquals(NODES.get(1),scheduler.select(NODES,1024,1)); server.verify();
    }
}
