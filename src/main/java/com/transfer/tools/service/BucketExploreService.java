package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.amazonaws.services.s3.model.ListObjectsRequest;
import com.amazonaws.services.s3.model.ObjectListing;
import com.amazonaws.services.s3.model.S3ObjectSummary;
import com.transfer.tools.model.ListSubTask;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.model.LocalExploreCheckpoint;
import java.util.concurrent.CancellationException;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Service;

import java.util.ArrayList;
import java.util.LinkedList;
import java.util.List;
import java.util.Queue;
import java.util.concurrent.atomic.AtomicInteger;

@Slf4j
@Service
public class BucketExploreService {

    @Autowired
    private SubTaskManagerService subTaskManagerService;

    @Autowired
    private DistributedService distributedService;

    @Autowired
    private com.transfer.tools.config.DistributedConfig distributedConfig;

    @Autowired
    private S3ClientManager s3ClientManager;

    @Autowired
    private RateLimiterManager rateLimiterManager;

    @Autowired
    private LocalParentTaskService localParentTaskService;
    @Autowired private TaskCreationService taskCreationService;

    private final Set<Integer> activeLocalExplorations = ConcurrentHashMap.newKeySet();

    private static final int LIST_LIMIT = 1000;
    private static final int SLIDING_WINDOW_SIZE = 3;
    private static final char MIN_PRINTABLE_CHAR = ' ';
    private static final char MAX_PRINTABLE_CHAR = '~';

    public void exploreBucketAndCreateSubTasks() {
        throw new IllegalStateException("该方法已废弃，请使用接口添加bucket对");
    }

    public void exploreSingleBucketAndCreateSubTasks(String sourceBucket, String destBucket,
            String sourceS3Alias, String destS3Alias, String dateAfter, Boolean overwrite) {
        exploreSingleBucketAndCreateSubTasks(sourceBucket, destBucket, sourceS3Alias, destS3Alias,
                dateAfter, null, null, overwrite, null);
    }

    public void exploreSingleBucketAndCreateSubTasks(String sourceBucket, String destBucket,
            String sourceS3Alias, String destS3Alias, String dateAfter, Boolean overwrite, String distributionMode) {
        exploreSingleBucketAndCreateSubTasks(sourceBucket, destBucket, sourceS3Alias, destS3Alias,
                dateAfter, null, null, overwrite, distributionMode);
    }

    public void exploreSingleBucketAndCreateSubTasks(String sourceBucket, String destBucket,
            String sourceS3Alias, String destS3Alias, String dateAfter, String dateBefore,
            String prefix, Boolean overwrite, String distributionMode) {
        log.info("开始扫描bucket: {} -> {}, sourceS3Alias: {}, destS3Alias: {}, dateAfter: {}, dateBefore: {}, prefix: {}, overwrite: {}, distributionMode: {}",
                sourceBucket, destBucket, sourceS3Alias, destS3Alias, dateAfter, dateBefore, prefix, overwrite, distributionMode);

        int workerCount = 1;
        if (distributedConfig.isMaster() && distributedConfig.getWorkerIps() != null && !distributedConfig.getWorkerIps().isEmpty()) {
            workerCount = distributedConfig.getWorkerIps().size();
        }

        AtomicInteger totalTaskCount = new AtomicInteger(0);

        String effectivePrefix = prefix == null ? "" : prefix;
        exploreBucketProgressively(sourceBucket, destBucket, sourceS3Alias, destS3Alias,
                dateAfter, dateBefore, effectivePrefix, overwrite, workerCount, totalTaskCount, distributionMode, null);

        log.info("bucket {} 扫描完成，共生成 {} 个子任务", sourceBucket, totalTaskCount.get());
    }

    /** Keep the creation's literal source selection and destination mapping on every range. */
    public void exploreSingleBucketAndCreateSubTasks(BucketPair pair) {
        new EndpointNormalizationService().normalize(pair);
        if (ossCreation(pair)) {
            ensureActive(pair);
            distributedService.replayOssCreation(pair.getCreationId());
            TaskCreationService.Creation state = taskCreationService.get(pair.getCreationId());
            if (Boolean.TRUE.equals(state.getScanComplete())
                    || (state.getNextShardIndex() > 0 && (state.getMarker() == null || state.getMarker().isEmpty()))
                    || (pair.getObjectKey() != null && !pair.getObjectKey().isEmpty() && state.getNextShardIndex() > 0)) return;
        }
        if (pair.getObjectKey() != null && !pair.getObjectKey().isEmpty()) {
            ListSubTask task = new ListSubTask(pair.getSourceBucket(), pair.getDestBucket(), pair.getSourceS3Alias(),
                    pair.getDestS3Alias(), "", "", pair.getDateAfter(), pair.getDateBefore(), pair.getOverwrite());
            copyOssContext(pair, task);
            dispatchOrdinary(java.util.Collections.singletonList(task), pair.getDistributionMode());
            return;
        }
        List<String> selectedWorkers = pair.getEligibleWorkerNodes() == null
                ? distributedConfig.getWorkerIps() : pair.getEligibleWorkerNodes();
        int workers = distributedConfig.isMaster() && selectedWorkers != null
                ? Math.max(1, selectedWorkers.size()) : 1;
        exploreBucketProgressively(pair.getSourceBucket(), pair.getDestBucket(), pair.getSourceS3Alias(),
                pair.getDestS3Alias(), pair.getDateAfter(), pair.getDateBefore(),
                pair.getPrefix() == null ? "" : pair.getPrefix(), pair.getOverwrite(), workers,
                new AtomicInteger(), pair.getDistributionMode(), pair);
    }

    private void dispatchOrdinary(List<ListSubTask> tasks, String mode) {
        if (distributedConfig.isMaster() && distributedConfig.getWorkerIps() != null
                && !distributedConfig.getWorkerIps().isEmpty()) distributedService.distributeTasks(tasks, mode);
        else subTaskManagerService.saveSubTasks(tasks);
    }

    private boolean ossCreation(BucketPair pair) {
        return pair != null && !isLocalContext(pair) && pair.getCreationId() != null
                && taskCreationService != null && taskCreationService.get(pair.getCreationId()) != null;
    }

    private boolean isLocalContext(BucketPair pair) {
        return pair != null && pair.getDirection() == com.transfer.tools.model.TransferDirection.OSS_TO_LOCAL;
    }

    private void exploreBucketProgressively(String sourceBucket, String destBucket,
            String sourceS3Alias, String destS3Alias, String dateAfter, String dateBefore, String prefix, Boolean overwrite,
            int workerCount, AtomicInteger totalTaskCount, String distributionMode, BucketPair localContext) {

        if (DistributedService.MODE_BALANCED.equals(distributionMode)) {
            exploreBalanced(sourceBucket, destBucket, sourceS3Alias, destS3Alias, dateAfter, dateBefore,
                    prefix, overwrite, workerCount, totalTaskCount, localContext);
            return;
        }
        log.info("bucket {} 开始探索, prefix: '{}', listLimit: {}, workerCount: {}, distributionMode: {}", sourceBucket, prefix, LIST_LIMIT, workerCount, distributionMode);

        int batchSize = workerCount * 20;
        log.info("bucket {} 边探索边分发模式启用, 每批任务数: {}", sourceBucket, batchSize);

        List<String> subTaskKeys = new ArrayList<>();
        LocalExploreCheckpoint checkpoint = checkpoint(localContext);
        // S3 markers are exclusive: using prefix as the first boundary would omit
        // an object whose complete key equals that prefix (including directory markers).
        String boundary = checkpoint == null ? "" : checkpoint.getMarker();
        subTaskKeys.add(boundary == null ? "" : boundary);
        if (checkpoint != null) totalTaskCount.set(checkpoint.getNextShardIndex());

        String currentMarker = checkpoint == null ? "" : checkpoint.getMarker();
        String lastObservedKey = null;
        Queue<String> slidingWindow = new LinkedList<>();

        while (true) {
            ensureActive(localContext);
            rateLimiterManager.acquireList();
            ensureActive(localContext);

            ListObjectsRequest request = new ListObjectsRequest()
                    .withBucketName(sourceBucket)
                    .withPrefix(prefix)
                    .withMarker(currentMarker)
                    .withMaxKeys(LIST_LIMIT);

            AmazonS3 s3Client = s3ClientManager.getListClient(sourceS3Alias);
            if (s3Client == null) {
                throw new RuntimeException("源S3客户端不存在: " + sourceS3Alias);
            }

            ObjectListing listing = s3Client.listObjects(request);
            ensureActive(localContext);
            List<S3ObjectSummary> summaries = listing.getObjectSummaries();

            if (summaries.isEmpty()) {
                // An optimized marker may jump beyond the prefix's final objects.
                // Resume from the last real key before accepting EOF so the final
                // bounded range also includes the objects skipped by that jump.
                if (lastObservedKey != null && currentMarker.compareTo(lastObservedKey) > 0) {
                    log.info("bucket {} 优化marker已越过末尾，从最后已知对象继续扫描: {}", sourceBucket, lastObservedKey);
                    currentMarker = lastObservedKey;
                    slidingWindow.clear();
                    continue;
                }
                log.info("bucket {} 没有更多对象，扫描完成", sourceBucket);
                break;
            }

            String firstKey = summaries.get(0).getKey();
            String lastKey = summaries.get(summaries.size() - 1).getKey();
            lastObservedKey = lastKey;
            String commonPrefix = findCommonPrefix(firstKey, lastKey);

            log.info("bucket {} 扫描中, firstKey: {}, lastKey: {}, commonPrefix: {}, subTaskKeys size: {}",
                    sourceBucket, firstKey, lastKey, commonPrefix, subTaskKeys.size());

            String nextMarker;
            if (commonPrefix != null && !commonPrefix.isEmpty()) {
                subTaskKeys.add(lastKey);
                slidingWindow.offer(lastKey);
                if (slidingWindow.size() == SLIDING_WINDOW_SIZE) {
                    String adjustedMarker = checkAndAdjustMarker(slidingWindow);
                    if (adjustedMarker != null) {
                        nextMarker = adjustedMarker;
                        log.info("bucket {} 检测到连续相同公共前缀，跳过到: {}", sourceBucket, nextMarker);
                        slidingWindow.clear();
                    } else {
                        slidingWindow.poll();
                        nextMarker = calculateOptimizedMarker(sourceBucket, commonPrefix, lastKey);
                    }
                } else {
                    nextMarker = calculateOptimizedMarker(sourceBucket, commonPrefix, lastKey);
                }

                log.debug("bucket {} 找到公共前缀: {}, 下一个marker: {}",
                         sourceBucket, commonPrefix, nextMarker);
            } else {
                slidingWindow.clear();
                nextMarker = lastKey;
            }

            currentMarker = nextMarker;

            if (subTaskKeys.size() >= batchSize+1) {
                distributeBatchTasks(subTaskKeys, sourceBucket, destBucket,
                        sourceS3Alias, destS3Alias, dateAfter, dateBefore, prefix, overwrite, totalTaskCount, distributionMode, localContext);
                subTaskKeys.clear();
                subTaskKeys.add(lastKey);
                log.info("bucket {} 已分发一批任务，继续探索，当前marker: {}", sourceBucket, currentMarker);
            }

            if (!listing.isTruncated()) {
                if (prefix.isEmpty()) {
                    subTaskKeys.add("");
                }
                log.info("bucket {} 已到达末尾，扫描完成", sourceBucket);
                break;
            }
        }

        if (subTaskKeys.size() > 1) {
            if (!subTaskKeys.get(subTaskKeys.size() - 1).equals("") && prefix.isEmpty()) {
                subTaskKeys.add("");
            }
            distributeBatchTasks(subTaskKeys, sourceBucket, destBucket,
                    sourceS3Alias, destS3Alias, dateAfter, dateBefore, prefix, overwrite, totalTaskCount, distributionMode, localContext);
        }
    }

    /** Balanced ranges are measured, never estimated from skipped prefix samples. */
    private void exploreBalanced(String sourceBucket, String destBucket, String sourceAlias, String destinationAlias,
                                 String after, String before, String prefix, Boolean overwrite, int workerCount,
                                 AtomicInteger total, BucketPair context) {
        LocalExploreCheckpoint saved = checkpoint(context);
        String boundary = saved == null || saved.getMarker() == null ? "" : saved.getMarker();
        String marker = boundary;
        if (saved != null) total.set(saved.getNextShardIndex());
        List<ListSubTask> ready = new ArrayList<>();
        long count = 0, bytes = 0;
        String last = boundary;
        while (true) {
            ensureActive(context);
            rateLimiterManager.acquireList();
            AmazonS3 client = s3ClientManager.getListClient(sourceAlias);
            if (client == null) throw new IllegalStateException("source S3 client is unavailable");
            ObjectListing page = client.listObjects(new ListObjectsRequest().withBucketName(sourceBucket)
                    .withPrefix(prefix).withMarker(marker).withMaxKeys(LIST_LIMIT));
            ensureActive(context);
            for (S3ObjectSummary object : page.getObjectSummaries()) {
                long size = Math.max(0, object.getSize());
                if (count > 0 && (count >= BalancedWorkerScheduler.MAX_ITEMS
                        || bytes >= BalancedWorkerScheduler.MAX_DATA_BYTES
                        || size > BalancedWorkerScheduler.MAX_DATA_BYTES - bytes)) {
                    ready.add(weightedRange(boundary, last, count, bytes, sourceBucket, destBucket, sourceAlias,
                            destinationAlias, after, before, prefix, overwrite, context, total.get() + ready.size()));
                    boundary = last; count = 0; bytes = 0;
                    if (ready.size() >= Math.max(1, workerCount) * 20) {
                        dispatchBalanced(ready, context, total); ready.clear();
                    }
                }
                last = object.getKey(); count++; bytes += size;
            }
            if (!page.isTruncated()) break;
            if (page.getObjectSummaries().isEmpty() || last.equals(marker))
                throw new IllegalStateException("truncated source listing did not advance");
            marker = last;
        }
        if (count > 0) ready.add(weightedRange(boundary, last, count, bytes, sourceBucket, destBucket,
                sourceAlias, destinationAlias, after, before, prefix, overwrite, context, total.get() + ready.size()));
        if (ready.isEmpty() && count == 0 && total.get() == 0 && !isLocalContext(context)) {
            // Keep an explicit task for an empty source so creation is reviewable and the
            // normal start/scan-complete path can produce a zero-file report.
            ready.add(weightedRange(boundary, "", 0, 0, sourceBucket, destBucket, sourceAlias,
                    destinationAlias, after, before, prefix, overwrite, context, 0));
        }
        if (!ready.isEmpty()) dispatchBalanced(ready, context, total);
    }

    private ListSubTask weightedRange(String start, String end, long count, long bytes, String sourceBucket,
                                      String destBucket, String sourceAlias, String destinationAlias, String after,
                                      String before, String prefix, Boolean overwrite, BucketPair context, int index) {
        ListSubTask task = buildSubTasks(java.util.Arrays.asList(start, end), sourceBucket, destBucket,
                sourceAlias, destinationAlias, after, before, prefix, overwrite, context, index).get(0);
        task.setDistributionMode(DistributedService.MODE_BALANCED);
        task.setEstimatedObjectCount(count); task.setEstimatedObjectSize(bytes);
        return task;
    }

    private void dispatchBalanced(List<ListSubTask> ready, BucketPair context, AtomicInteger total) {
        ensureActive(context);
        if (distributedConfig.isMaster() && distributedConfig.getWorkerIps() != null
                && !distributedConfig.getWorkerIps().isEmpty()) {
            if (!isLocalContext(context)) distributedService.distributeTasks(ready, DistributedService.MODE_BALANCED);
            else dispatchDurably(context, ready);
        } else subTaskManagerService.saveSubTasks(ready);
        total.addAndGet(ready.size());
    }

    private void distributeBatchTasks(List<String> subTaskKeys, String sourceBucket, String destBucket,
            String sourceS3Alias, String destS3Alias, String dateAfter, String dateBefore, String prefix, Boolean overwrite,
            AtomicInteger totalTaskCount, String distributionMode, BucketPair localContext) {

        ensureActive(localContext);
        if (subTaskKeys.size() < 2) {
            return;
        }

        List<ListSubTask> subTasks = buildSubTasks(subTaskKeys, sourceBucket, destBucket,
                sourceS3Alias, destS3Alias, dateAfter, dateBefore, prefix, overwrite, localContext,
                totalTaskCount.get());

        if (subTasks.isEmpty()) {
            return;
        }

        if (!isLocalContext(localContext)) {
            for (ListSubTask task : subTasks) task.setDistributionMode(distributionMode);
        }

        if (distributedConfig.isMaster() && distributedConfig.getWorkerIps() != null && !distributedConfig.getWorkerIps().isEmpty()) {
            if (!isLocalContext(localContext)) {
                distributedService.distributeTasks(subTasks, distributionMode);
            } else {
                dispatchDurably(localContext, subTasks);
            }
            log.info("bucket {} 分发 {} 个任务到工作节点, distributionMode: {}", sourceBucket, subTasks.size(), distributionMode);
        } else {
            subTaskManagerService.saveSubTasks(subTasks);
            log.info("bucket {} 保存 {} 个任务", sourceBucket, subTasks.size());
        }

        totalTaskCount.addAndGet(subTasks.size());
    }

    private String checkAndAdjustMarker(Queue<String> slidingWindow) {
        List<String> markers = new ArrayList<>(slidingWindow);
        if (markers.size() != SLIDING_WINDOW_SIZE) {
            return null;
        }

        String prefix1 = findCommonPrefix(markers.get(0), markers.get(1));
        String prefix2 = findCommonPrefix(markers.get(1), markers.get(2));

        if (prefix1 != null && prefix2 != null && prefix1.equals(prefix2)) {
            String nextMarker = calculateNextMarker(prefix1);
            log.debug("滑动窗口检测: marker1={}, marker2={}, marker3={}, commonPrefix={}, nextMarker={}",
                    markers.get(0), markers.get(1), markers.get(2), prefix1, nextMarker);
            return nextMarker;
        }

        return null;
    }

    private String calculateOptimizedMarker(String sourceBucket, String commonPrefix, String lastKey) {
        String parentDir = extractParentDirectory(lastKey);
        if (parentDir != null && parentDir.length() > commonPrefix.length()) {
            String nextMarker = calculateNextMarker(parentDir);
            log.debug("bucket {} 使用目录结构优化, commonPrefix={}, parentDir={}, nextMarker={}",
                    sourceBucket, commonPrefix, parentDir, nextMarker);
            return nextMarker;
        } else {
            return calculateNextMarker(commonPrefix);
        }
    }

    private String extractParentDirectory(String key) {
        if (key == null || key.isEmpty()) {
            return null;
        }

        int lastSlashIndex = key.lastIndexOf('/');
        if (lastSlashIndex > 0) {
            return key.substring(0, lastSlashIndex);
        }

        return null;
    }

    private String findCommonPrefix(String key1, String key2) {
        if (key1 == null || key2 == null) {
            return null;
        }

        int minLength = Math.min(key1.length(), key2.length());
        int i = 0;

        while (i < minLength && key1.charAt(i) == key2.charAt(i)) {
            i++;
        }

        return i > 0 ? key1.substring(0, i) : null;
    }

    String calculateNextMarker(String prefix) {
        if (prefix == null || prefix.isEmpty()) {
            return "";
        }

        char[] chars = prefix.toCharArray();

        if (chars.length == 1) {
            if (chars[0] == MAX_PRINTABLE_CHAR) {
                return "";
            } else if (chars[0] < MAX_PRINTABLE_CHAR) {
                chars[0] = (char) (chars[0] + 1);
                return new String(chars);
            }
        }

        if (chars[chars.length - 1] != MAX_PRINTABLE_CHAR) {
            chars[chars.length - 1] = (char) (chars[chars.length - 1] + 1);
            return new String(chars);
        }

        int lastNonTildeIndex = -1;
        for (int i = chars.length - 2; i >= 0; i--) {
            if (chars[i] != MAX_PRINTABLE_CHAR) {
                lastNonTildeIndex = i;
                break;
            }
        }

        if (lastNonTildeIndex == -1) {
            return prefix + MIN_PRINTABLE_CHAR;
        }

        chars[lastNonTildeIndex] = (char) (chars[lastNonTildeIndex] + 1);

        return new String(chars, 0, lastNonTildeIndex + 1);
    }

    private List<ListSubTask> buildSubTasks(List<String> subTaskKeys, String sourceBucket, String destBucket,
            String sourceS3Alias, String destS3Alias, String dateAfter, String dateBefore, String prefix, Boolean overwrite,
            BucketPair localContext, int shardOffset) {
        List<ListSubTask> subTasks = new ArrayList<>();

        for (int i = 0; i < subTaskKeys.size() - 1; i++) {
            String startKey = subTaskKeys.get(i);
            String endKey = subTaskKeys.get(i + 1);

            ListSubTask subTask = new ListSubTask(sourceBucket, destBucket, sourceS3Alias, destS3Alias, startKey, endKey, dateAfter, dateBefore, overwrite);
            if (isLocalContext(localContext)) {
                copyLocalContext(localContext, subTask, shardOffset + i);
            } else if (localContext != null) {
                copyOssContext(localContext, subTask);
            } else if (prefix != null && !prefix.isEmpty()) {
                // Persist the filter separately from the exclusive range boundaries.
                com.transfer.tools.model.TransferEndpoint source = new com.transfer.tools.model.TransferEndpoint();
                source.setType(com.transfer.tools.model.EndpointType.OSS);
                source.setS3Alias(sourceS3Alias);
                source.setBucket(sourceBucket);
                source.setPrefix(prefix);
                source.setPrefixFilterEnabled(true);
                subTask.setSourceEndpoint(source);
            }
            subTasks.add(subTask);

            log.debug("创建子任务 {}: {} -> {}, startKey={}, endKey={}, dateAfter={}, dateBefore={}, overwrite={}",
                     i, sourceBucket, destBucket, startKey, endKey, dateAfter, dateBefore, overwrite);
        }

        return subTasks;
    }

    public void exploreLocalBucketAndCreateSubTasks(BucketPair pair) {
        Integer parentId = pair.getParentTaskId();
        if (!activeLocalExplorations.add(parentId))
            throw new CancellationException("parent exploration is already active");
        try {
        ensureActive(pair);
        LocalExploreCheckpoint saved = checkpoint(pair);
        if (saved != null && saved.isComplete()) return;
        if (saved != null && saved.getPendingTasks() != null && !saved.getPendingTasks().isEmpty()) {
            dispatchDurably(pair, saved.getPendingTasks());
            saved = checkpoint(pair);
            if (saved != null && saved.isComplete()) return;
        }
        int workerCount = Math.max(1, pair.getEligibleWorkerNodes() == null ? 0 : pair.getEligibleWorkerNodes().size());
        AtomicInteger totalTaskCount = new AtomicInteger(0);
        String prefix = pair.getPrefix() == null ? "" : pair.getPrefix().trim();
        exploreBucketProgressively(pair.getSourceBucket(), pair.getDestBucket(), pair.getSourceS3Alias(),
                pair.getDestS3Alias(), pair.getDateAfter(), pair.getDateBefore(), prefix, pair.getOverwrite(),
                workerCount, totalTaskCount, pair.getDistributionMode(), pair);
        if (totalTaskCount.get() == 0) {
            ListSubTask empty = new ListSubTask(pair.getSourceBucket(), pair.getDestBucket(), pair.getSourceS3Alias(),
                    pair.getDestS3Alias(), prefix, "", pair.getDateAfter(), pair.getDateBefore(), pair.getOverwrite());
            copyLocalContext(pair, empty, 0);
            dispatchDurably(pair, java.util.Collections.singletonList(empty));
        }
        completeCheckpoint(pair);
        } finally {
            activeLocalExplorations.remove(parentId);
        }
    }

    public void distributeExactLocalObject(BucketPair pair) {
        ListSubTask task = new ListSubTask(pair.getSourceBucket(), pair.getDestBucket(), pair.getSourceS3Alias(),
                pair.getDestS3Alias(), "", "", pair.getDateAfter(), pair.getDateBefore(), pair.getOverwrite());
        task.setObjectKey(pair.getObjectKey());
        task.setTargetRelativePath(pair.getTargetRelativePath());
        copyLocalContext(pair, task, 0);
        ensureActive(pair);
        LocalExploreCheckpoint saved = checkpoint(pair);
        if (saved != null && saved.isComplete()) return;
        dispatchDurably(pair, saved != null && saved.getPendingTasks() != null
                ? saved.getPendingTasks() : java.util.Collections.singletonList(task));
        completeCheckpoint(pair);
    }

    private LocalExploreCheckpoint checkpoint(BucketPair pair) {
        if (ossCreation(pair)) {
            TaskCreationService.Creation creation = taskCreationService.get(pair.getCreationId());
            LocalExploreCheckpoint checkpoint = new LocalExploreCheckpoint();
            checkpoint.setMarker(creation.getMarker()); checkpoint.setNextShardIndex(creation.getNextShardIndex());
            checkpoint.setComplete(Boolean.TRUE.equals(creation.getScanComplete())); return checkpoint;
        }
        return !isLocalContext(pair) || localParentTaskService == null ? null
                : localParentTaskService.exploreCheckpoint(pair.getParentTaskId());
    }

    private void ensureActive(BucketPair pair) {
        if (ossCreation(pair)) taskCreationService.ensureActive(pair.getCreationId());
        if (isLocalContext(pair) && (Thread.currentThread().isInterrupted()
                || (localParentTaskService != null && localParentTaskService.isStopped(pair.getParentTaskId()))))
            throw new CancellationException("parent transfer has stopped");
    }

    private void completeCheckpoint(BucketPair pair) {
        if (localParentTaskService == null) return;
        boolean active = localParentTaskService.whileActive(pair.getParentTaskId(), () -> {
            LocalExploreCheckpoint state = checkpoint(pair);
            if (state == null) state = new LocalExploreCheckpoint();
            state.setComplete(true);
            localParentTaskService.saveExploreCheckpoint(pair.getParentTaskId(), state);
        });
        if (!active) throw new CancellationException("parent transfer has stopped");
    }

    private void dispatchDurably(BucketPair pair, List<ListSubTask> tasks) {
        if (localParentTaskService == null) {
            distributedService.distributeTasks(tasks, pair.getDistributionMode(),
                    pair.getEligibleWorkerNodes(), pair.getNodeBindings());
            return;
        }
        boolean active = localParentTaskService.whileActive(pair.getParentTaskId(), () -> {
            LocalExploreCheckpoint state = checkpoint(pair);
            if (state == null) state = new LocalExploreCheckpoint();
            if (state.getPendingTasks() == null || state.getPendingTasks().isEmpty()) {
                distributedService.prepareLocalAssignments(tasks, pair.getDistributionMode(),
                        pair.getEligibleWorkerNodes(), pair.getNodeBindings());
                state.setPendingTasks(new ArrayList<>(tasks));
                localParentTaskService.saveExploreCheckpoint(pair.getParentTaskId(), state);
            }
            List<ListSubTask> pending = state.getPendingTasks();
            distributedService.distributePreparedLocalTasks(pending);
            ListSubTask last = pending.get(pending.size() - 1);
            state.setMarker(last.getEndKey());
            state.setNextShardIndex(last.getShardIndex() + 1);
            state.setComplete(last.getEndKey() == null || last.getEndKey().isEmpty());
            state.setPendingTasks(null);
            localParentTaskService.saveExploreCheckpoint(pair.getParentTaskId(), state);
        });
        if (!active) throw new CancellationException("parent transfer has stopped");
    }

    private void copyLocalContext(BucketPair pair, ListSubTask task, int shardIndex) {
        task.setDirection(pair.getDirection());
        task.setSourceEndpoint(pair.getSourceEndpoint());
        task.setDestinationEndpoint(pair.getDestinationEndpoint());
        task.setConflictPolicy(pair.getConflictPolicy());
        task.setDistributionMode(pair.getDistributionMode());
        task.setParentTaskId(pair.getParentTaskId());
        task.setShardIndex(shardIndex);
        task.setShardId(pair.getParentTaskId() + "-oss-" + shardIndex);
        task.setAttempt(0);
        task.setStorageId(pair.getStorageId());
        task.setEligibleWorkerNodes(pair.getEligibleWorkerNodes());
        task.setNodeBindings(pair.getNodeBindings());
        task.setListQpsLimit(pair.getListQpsLimit());
        task.setHeadQpsLimit(pair.getHeadQpsLimit());
        task.setPutQpsLimit(pair.getPutQpsLimit());
        task.setBandwidthBitsPerSecond(pair.getBandwidthBitsPerSecond());
        task.setTaskBandwidthLimitMbps(pair.getTaskBandwidthLimitMbps());
        task.setTaskBandwidthScopeId(pair.getTaskBandwidthScopeId());
    }

    private void copyOssContext(BucketPair pair, ListSubTask task) {
        task.setDirection(com.transfer.tools.model.TransferDirection.OSS_TO_OSS);
        task.setSourceEndpoint(pair.getSourceEndpoint());
        task.setDestinationEndpoint(pair.getDestinationEndpoint());
        task.setObjectKey(pair.getObjectKey());
        task.setKeyMapping(pair.getKeyMapping());
        task.setEligibleWorkerNodes(pair.getEligibleWorkerNodes() == null ? null
                : new ArrayList<>(pair.getEligibleWorkerNodes()));
        task.setConflictPolicy(pair.getConflictPolicy());
        task.setDistributionMode(pair.getDistributionMode());
        task.setTaskBandwidthLimitMbps(pair.getTaskBandwidthLimitMbps());
        task.setTaskBandwidthScopeId(pair.getTaskBandwidthScopeId());
    }
}
