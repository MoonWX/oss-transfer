package com.transfer.tools.controller;

import com.amazonaws.services.s3.AmazonS3;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.ExploreExecutorService;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.service.*;
import org.junit.Before;
import org.junit.Test;
import org.mockito.ArgumentCaptor;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.*;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;

public class MasterOssNodeSelectionTest {
    private MasterController controller;
    private TaskCreationService creations;
    private MasterTaskBandwidthService bandwidth;
    private ExploreExecutorService exploration;
    private S3ClientManager clients;
    private static final List<String> NODES=Arrays.asList("node-a","node-b");

    @Before public void setup() {
        controller=new MasterController();
        DistributedConfig config=mock(DistributedConfig.class); when(config.getWorkerIps()).thenReturn(NODES);
        creations=mock(TaskCreationService.class);bandwidth=mock(MasterTaskBandwidthService.class);
        exploration=mock(ExploreExecutorService.class);clients=mock(S3ClientManager.class);
        when(clients.getClient(anyString())).thenReturn(mock(AmazonS3.class));
        when(exploration.getExploreQueueRemainingCapacity()).thenReturn(100);
        when(exploration.offerExploreTask(any())).thenReturn(true);
        when(creations.create(any(BucketPair.class), org.mockito.ArgumentMatchers.eq(true))).thenReturn("creation-fixture");
        ReflectionTestUtils.setField(controller,"distributedConfig",config);
        ReflectionTestUtils.setField(controller,"taskCreationService",creations);
        ReflectionTestUtils.setField(controller,"taskBandwidth",bandwidth);
        ReflectionTestUtils.setField(controller,"exploreExecutorService",exploration);
        ReflectionTestUtils.setField(controller,"s3ClientManager",clients);
        ReflectionTestUtils.setField(controller,"endpointNormalizationService",new EndpointNormalizationService());
    }
    private BucketPair pair(List<String> nodes) {
        BucketPair pair=new BucketPair();pair.setSourceBucket("source");pair.setDestBucket("target");pair.setEligibleWorkerNodes(nodes);return pair;
    }
    @Test public void omittedSelectionCapturesAllRegisteredNodesBeforeSaving() {
        BucketPair request=pair(null);
        assertEquals(true,controller.addBucketPairs(Collections.singletonList(request)).get("success"));
        ArgumentCaptor<BucketPair> saved=ArgumentCaptor.forClass(BucketPair.class);verify(creations).create(saved.capture(), org.mockito.ArgumentMatchers.eq(true));
        assertEquals(NODES,saved.getValue().getEligibleWorkerNodes());
        verify(bandwidth).prepareCreation(same(request),eq("creation-fixture"),eq(NODES));
    }
    @Test public void oneNodeSelectionIsUsedForBothCreationAndBandwidth() {
        BucketPair request=pair(Collections.singletonList("node-b"));
        assertEquals(true,controller.addBucketPairs(Collections.singletonList(request)).get("success"));
        verify(bandwidth).prepareCreation(same(request),eq("creation-fixture"),eq(Collections.singletonList("node-b")));
        verify(exploration).offerExploreTask(same(request));
    }
    @Test public void twoNodeSelectionDeduplicatesWithoutAddingOtherMembers() {
        BucketPair request=pair(Arrays.asList("node-b","node-a","node-b"));
        assertEquals(true,controller.addBucketPairs(Collections.singletonList(request)).get("success"));
        assertEquals(Arrays.asList("node-b","node-a"),request.getEligibleWorkerNodes());
        verify(bandwidth).prepareCreation(same(request),eq("creation-fixture"),eq(Arrays.asList("node-b","node-a")));
    }
    @Test public void invalidNodeAnywhereInBatchRejectsBeforeCreationOrPolicyChanges() {
        assertEquals(false,controller.addBucketPairs(Arrays.asList(pair(NODES),pair(Collections.singletonList("unknown")))).get("success"));
        verify(creations,never()).create(any(BucketPair.class), org.mockito.ArgumentMatchers.eq(true));verifyZeroInteractions(bandwidth);
        verify(exploration,never()).offerExploreTask(any());verifyZeroInteractions(clients);
    }
    @Test public void explicitEmptyOrBlankSelectionCannotFallBackToEveryNode() {
        for(List<String> invalid:Arrays.asList(Collections.<String>emptyList(),Collections.singletonList(" "),Collections.<String>singletonList(null)))
            assertEquals(false,controller.addBucketPairs(Collections.singletonList(pair(invalid))).get("success"));
        verify(creations,never()).create(any(BucketPair.class), org.mockito.ArgumentMatchers.eq(true));verifyZeroInteractions(bandwidth);
        verify(exploration,never()).offerExploreTask(any());
    }
}
