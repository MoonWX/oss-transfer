package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.ListObjectsRequest;
import com.amazonaws.services.s3.model.ObjectListing;
import com.amazonaws.services.s3.model.S3ObjectSummary;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.model.EndpointType;
import com.transfer.tools.model.ListSubTask;
import com.transfer.tools.model.LocalNodeBinding;
import com.transfer.tools.model.TransferDirection;
import com.transfer.tools.model.TransferEndpoint;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.Collections;
import java.util.List;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyList;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.Mockito.doAnswer;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

public class BucketExploreTailCoverageTest {
    private static final String PREFIX = "codex-v4-qa-20260922/full/";

    @Test
    public void coversAll116500ObjectsAfterOptimizedMarkerOverrunsThePrefixTail() {
        List<String> keys = new ArrayList<>();
        addKeys(keys, "large_1GiB", 10);
        addKeys(keys, "large_4GiB", 10);
        addKeys(keys, "medium_1MiB", 10240);
        addKeys(keys, "medium_8MiB", 3840);
        addKeys(keys, "small_16KiB", 65536);
        addKeys(keys, "small_256KiB", 36864);
        Explorer fixture = new Explorer(keys);

        BucketPair affinity = localPair(); affinity.setDistributionMode("affinity");
        fixture.service.exploreLocalBucketAndCreateSubTasks(affinity);

        assertTrue("Exercise the same empty optimized-marker page as the live failure",
                fixture.emptyMarkers.contains(PREFIX + "small_256KiB/004"));
        assertCoveredExactlyOnce(keys, fixture.tasks);
        assertEquals(1, coverage(PREFIX + "small_256KiB/0033/00033000.bin", fixture.tasks));
        assertEquals(1, coverage(PREFIX + "small_256KiB/0036/00036863.bin", fixture.tasks));
        assertTrue("Tail coverage must survive the progressive 40-task batch flush",
                fixture.batchSizes.size() > 1);
        assertEquals(Integer.valueOf(40), fixture.batchSizes.get(0));
        for (ListSubTask task : fixture.tasks) {
            assertEquals(Integer.valueOf(4), task.getParentTaskId());
            assertEquals(PREFIX, task.getSourceEndpoint().getPrefix());
        }
    }

    @Test
    public void ossToOssPrefixTailDoesNotIncludeObjectsBeyondTheRequestedPrefix() {
        List<String> keys = new ArrayList<>();
        addKeys(keys, "small", 4500);
        String outside = "codex-v4-qa-20260922/other/unrelated.bin";
        keys.add(outside);
        Explorer fixture = new Explorer(keys);

        fixture.service.exploreSingleBucketAndCreateSubTasks("source-bucket", "target-bucket",
                "source", "target", null, null, PREFIX, false, "affinity");

        assertFalse("The fixture must exercise an empty optimized-marker page", fixture.emptyMarkers.isEmpty());
        assertCoveredExactlyOnce(keys.subList(0, 4500), fixture.tasks);
        assertEquals("Do not fix the missing tail by creating an unbounded OSS-to-OSS range",
                0, coverage(outside, fixture.tasks));
        assertFalse(fixture.tasks.get(fixture.tasks.size() - 1).getEndKey().isEmpty());
    }

    @Test
    public void aRealFinalPageRemainsBoundedWithoutAnExtraEmptyTailRange() {
        List<String> keys = new ArrayList<>();
        addKeys(keys, "small", 12);
        Explorer fixture = new Explorer(keys);

        fixture.service.exploreLocalBucketAndCreateSubTasks(localPair());

        assertCoveredExactlyOnce(keys, fixture.tasks);
        assertEquals(1, fixture.tasks.size());
        assertEquals(keys.get(keys.size() - 1), fixture.tasks.get(0).getEndKey());
        assertTrue(fixture.emptyMarkers.isEmpty());
    }

    @Test
    public void exactPrefixKeyIsIncludedWithoutObjectsOutsideItsPrefix() {
        for (String key : Arrays.asList("fixtures/000-empty.bin", "fixtures/001 with spaces.txt", "fixtures/中文.txt")) {
            Explorer fixture = new Explorer(Arrays.asList("aaa-before", key, "zzz-after"));
            fixture.service.exploreSingleBucketAndCreateSubTasks("source-bucket", "target-bucket",
                    "source", "target", null, null, key, false, "balanced");
            assertEquals(1, fixture.tasks.size());
            assertEquals("", fixture.tasks.get(0).getStartKey());
            assertEquals(key, fixture.tasks.get(0).getEndKey());
            assertEquals(Boolean.TRUE, fixture.tasks.get(0).getSourceEndpoint().getPrefixFilterEnabled());
            assertEquals(1, coverage(key, fixture.tasks));
            assertEquals(0, coverage("aaa-before", fixture.tasks));
            assertEquals(0, coverage("zzz-after", fixture.tasks));
        }
    }

    @Test
    public void prefixObjectAndMultiPageDescendantsAreCoveredExactlyOnce() {
        List<String> keys = new ArrayList<>(); keys.add(PREFIX); addKeys(keys, "small", 4500);
        List<String> all = new ArrayList<>(keys); all.add("aaa-before"); all.add("zzz-after");
        Explorer fixture = new Explorer(all);
        fixture.service.exploreSingleBucketAndCreateSubTasks("source-bucket", "target-bucket",
                "source", "target", null, null, PREFIX, false, "balanced");
        assertCoveredExactlyOnce(keys, fixture.tasks);
        assertEquals("", fixture.tasks.get(0).getStartKey());
        assertEquals(0, coverage("aaa-before", fixture.tasks));
        assertEquals(0, coverage("zzz-after", fixture.tasks));
        for (int i = 1; i < fixture.tasks.size(); i++)
            assertEquals(fixture.tasks.get(i - 1).getEndKey(), fixture.tasks.get(i).getStartKey());
    }

    @Test
    public void localDownloadAlsoIncludesObjectWhoseKeyEqualsPrefix() {
        Explorer fixture = new Explorer(Arrays.asList(PREFIX, PREFIX + "child", "zzz-after"));
        fixture.service.exploreLocalBucketAndCreateSubTasks(localPair());
        assertCoveredExactlyOnce(Arrays.asList(PREFIX, PREFIX + "child"), fixture.tasks);
        assertEquals(0, coverage("zzz-after", fixture.tasks));
    }

    @Test public void balancedEmptySourceStillCreatesOneExplicitTaskForBothDirections() {
        Explorer ordinary = new Explorer(Collections.emptyList());
        ordinary.service.exploreSingleBucketAndCreateSubTasks("source-bucket", "target-bucket", "source", "target",
                null, null, PREFIX, false, "balanced");
        assertEquals(1, ordinary.tasks.size());
        assertEquals(Long.valueOf(0), ordinary.tasks.get(0).getEstimatedObjectCount());
        assertEquals(PREFIX, ordinary.tasks.get(0).getSourceEndpoint().getPrefix());
        Explorer local = new Explorer(Collections.emptyList());
        local.service.exploreLocalBucketAndCreateSubTasks(localPair());
        assertEquals(1, local.tasks.size());
        assertEquals(Integer.valueOf(4), local.tasks.get(0).getParentTaskId());
        assertEquals(TransferDirection.OSS_TO_LOCAL, local.tasks.get(0).getDirection());
    }

    @Test public void balancedUsesMeasuredBoundedRangesAcrossSizeSkewWithoutPrefixJumps() {
        List<String> keys = new ArrayList<>(); addKeys(keys, "large", 10); addKeys(keys, "small", 5000);
        java.util.Map<String, Long> sizes = new java.util.HashMap<>();
        for (String key : keys) sizes.put(key, key.contains("/large/") ? 1024L * 1024 * 1024 : 1024L);
        Explorer fixture = new Explorer(keys, sizes);
        fixture.service.exploreLocalBucketAndCreateSubTasks(localPair());
        assertCoveredExactlyOnce(keys, fixture.tasks);
        assertTrue(fixture.emptyMarkers.isEmpty());
        long totalBytes = 0, totalItems = 0;
        for (ListSubTask task : fixture.tasks) {
            long actualItems = 0, actualBytes = 0;
            for (String key : keys) if (coverage(key, Collections.singletonList(task)) == 1) {
                actualItems++; actualBytes += sizes.get(key);
            }
            assertEquals(Long.valueOf(actualItems), task.getEstimatedObjectCount());
            assertEquals(Long.valueOf(actualBytes), task.getEstimatedObjectSize());
            assertTrue(actualItems <= BalancedWorkerScheduler.MAX_ITEMS);
            assertTrue(actualBytes <= BalancedWorkerScheduler.MAX_DATA_BYTES || actualItems == 1);
            totalItems += actualItems; totalBytes += actualBytes;
        }
        assertEquals(5010, totalItems); assertEquals(10L * 1024 * 1024 * 1024 + 5000 * 1024, totalBytes);
        assertTrue(fixture.batchSizes.size() > 1);
    }

    private static void addKeys(List<String> keys, String group, int count) {
        for (int i = 0; i < count; i++) {
            keys.add(PREFIX + group + "/" + String.format("%04d/%08d.bin", i / 1000, i));
        }
    }

    private static BucketPair localPair() {
        BucketPair pair = new BucketPair();
        pair.setSourceBucket("source-bucket");
        pair.setSourceS3Alias("source");
        pair.setPrefix(PREFIX);
        pair.setDirection(TransferDirection.OSS_TO_LOCAL);
        pair.setParentTaskId(4);
        pair.setDistributionMode("balanced");
        pair.setEligibleWorkerNodes(Arrays.asList("10.0.0.1", "10.0.0.2"));
        pair.setNodeBindings(Arrays.asList(new LocalNodeBinding("10.0.0.1", "/download"),
                new LocalNodeBinding("10.0.0.2", "/download")));
        TransferEndpoint source = new TransferEndpoint();
        source.setType(EndpointType.OSS);
        source.setS3Alias("source");
        source.setBucket("source-bucket");
        source.setPrefix(PREFIX);
        pair.setSourceEndpoint(source);
        return pair;
    }

    private static void assertCoveredExactlyOnce(List<String> keys, List<ListSubTask> tasks) {
        long missing = keys.stream().filter(key -> coverage(key, tasks) == 0).count();
        long repeated = keys.stream().filter(key -> coverage(key, tasks) > 1).count();
        assertEquals("Objects omitted from generated (startKey, endKey] ranges", 0L, missing);
        assertEquals("Objects covered by overlapping generated ranges", 0L, repeated);
    }

    private static int coverage(String key, List<ListSubTask> tasks) {
        int count = 0;
        for (ListSubTask task : tasks) {
            String prefix = task.getSourceEndpoint() == null ? "" : task.getSourceEndpoint().getPrefix();
            if ((prefix == null || key.startsWith(prefix)) && key.compareTo(task.getStartKey()) > 0
                    && (task.getEndKey().isEmpty() || key.compareTo(task.getEndKey()) <= 0)) count++;
        }
        return count;
    }

    private static class Explorer {
        final BucketExploreService service = new BucketExploreService();
        final List<ListSubTask> tasks = new ArrayList<>();
        final List<Integer> batchSizes = new ArrayList<>();
        final List<String> emptyMarkers = new ArrayList<>();

        Explorer(List<String> sourceKeys) { this(sourceKeys, Collections.emptyMap()); }
        Explorer(List<String> sourceKeys, java.util.Map<String, Long> sizes) {
            List<String> sorted = new ArrayList<>(sourceKeys);
            Collections.sort(sorted);
            DistributedConfig config = mock(DistributedConfig.class);
            when(config.isMaster()).thenReturn(true);
            when(config.getWorkerIps()).thenReturn(Arrays.asList("10.0.0.1", "10.0.0.2"));
            ReflectionTestUtils.setField(service, "distributedConfig", config);
            ReflectionTestUtils.setField(service, "rateLimiterManager", mock(RateLimiterManager.class));
            DistributedService distributed = mock(DistributedService.class);
            doAnswer(invocation -> { collect(invocation.getArgument(0)); return null; })
                    .when(distributed).distributeTasks(anyList(), anyString());
            doAnswer(invocation -> { collect(invocation.getArgument(0)); return null; })
                    .when(distributed).distributeTasks(anyList(), anyString(), anyList(), anyList());
            ReflectionTestUtils.setField(service, "distributedService", distributed);
            S3ClientManager clients = mock(S3ClientManager.class);
            AmazonS3 s3 = mock(AmazonS3.class);
            when(clients.getListClient("source")).thenReturn(s3);
            ReflectionTestUtils.setField(service, "s3ClientManager", clients);
            when(s3.listObjects(any(ListObjectsRequest.class))).thenAnswer(invocation -> {
                ListObjectsRequest request = invocation.getArgument(0);
                ObjectListing listing = new ObjectListing();
                String marker = request.getMarker() == null ? "" : request.getMarker();
                String prefix = request.getPrefix() == null ? "" : request.getPrefix();
                int start = Collections.binarySearch(sorted, marker);
                start = start >= 0 ? start + 1 : -start - 1;
                for (int i = start; i < sorted.size(); i++) {
                    String key = sorted.get(i);
                    if (!key.startsWith(prefix)) continue;
                    if (listing.getObjectSummaries().size() == request.getMaxKeys()) {
                        listing.setTruncated(true);
                        break;
                    }
                    S3ObjectSummary object = new S3ObjectSummary();
                    object.setKey(key); object.setSize(sizes.getOrDefault(key, 0L));
                    listing.getObjectSummaries().add(object);
                }
                if (listing.isTruncated()) listing.setNextMarker(listing.getObjectSummaries()
                        .get(listing.getObjectSummaries().size() - 1).getKey());
                if (listing.getObjectSummaries().isEmpty()) emptyMarkers.add(marker);
                return listing;
            });
        }

        private void collect(List<ListSubTask> batch) {
            tasks.addAll(batch);
            batchSizes.add(batch.size());
        }
    }
}
