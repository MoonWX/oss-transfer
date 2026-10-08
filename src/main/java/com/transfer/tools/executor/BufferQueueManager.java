package com.transfer.tools.executor;

import com.transfer.tools.config.BufferQueueConfig;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.model.FailedKeyMessage;
import com.transfer.tools.model.FallbackMessage;
import com.transfer.tools.model.ObjectListMessage;
import com.transfer.tools.model.ProgressMessage;
import com.transfer.tools.model.SubTaskStatus;
import lombok.extern.slf4j.Slf4j;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.stereotype.Component;

import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.BlockingQueue;

@Slf4j
@Component
public class BufferQueueManager {

    @Autowired
    private BufferQueueConfig bufferQueueConfig;

    private BlockingQueue<BucketPair> exploreQueue;
    private BlockingQueue<SubTaskStatus> listQueue;
    private BlockingQueue<ObjectListMessage> putQueue;
    private BlockingQueue<ProgressMessage> progressQueue;
    private BlockingQueue<FailedKeyMessage> failedKeyQueue;
    private BlockingQueue<FallbackMessage> fallbackQueue;

    @javax.annotation.PostConstruct
    public void init() {
        int exploreQueueSize = bufferQueueConfig.getExplore().getSize();
        int listQueueSize = bufferQueueConfig.getList().getSize();
        int putQueueSize = bufferQueueConfig.getPut().getSize();
        int progressQueueSize = bufferQueueConfig.getProgress().getSize();
        int failedQueueSize = bufferQueueConfig.getFailed().getSize();
        int fallbackQueueSize = bufferQueueConfig.getFallback().getSize();

        this.exploreQueue = new ArrayBlockingQueue<>(exploreQueueSize);
        this.listQueue = new ArrayBlockingQueue<>(listQueueSize);
        this.putQueue = new ArrayBlockingQueue<>(putQueueSize);
        this.progressQueue = new ArrayBlockingQueue<>(progressQueueSize);
        this.failedKeyQueue = new ArrayBlockingQueue<>(failedQueueSize);
        this.fallbackQueue = new ArrayBlockingQueue<>(fallbackQueueSize);

        log.info("缓冲队列初始化完成 - Explore队列容量: {}, List队列容量: {}, Put队列容量: {}, Progress队列容量: {}, FailedKey队列容量: {}, Fallback队列容量: {}",
                exploreQueueSize, listQueueSize, putQueueSize, progressQueueSize, failedQueueSize, fallbackQueueSize);
    }

    public boolean offerExploreTask(BucketPair bucketPair) {
        boolean success = exploreQueue.offer(bucketPair);
        if (success) {
            log.debug("Explore任务已放入队列，当前队列大小: {}", exploreQueue.size());
        }
        return success;
    }

    public BucketPair takeExploreTask() throws InterruptedException {
        BucketPair task = exploreQueue.take();
        log.debug("Explore任务已从队列取出，当前队列大小: {}", exploreQueue.size());
        return task;
    }

    public int exploreQueueSize() {
        return exploreQueue.size();
    }

    public int exploreQueueRemainingCapacity() {
        return exploreQueue.remainingCapacity();
    }

    public void putListTask(SubTaskStatus task) throws InterruptedException {
        listQueue.put(task);
        log.debug("List任务已放入队列，当前队列大小: {}", listQueue.size());
    }

    public SubTaskStatus takeListTask() throws InterruptedException {
        SubTaskStatus task = listQueue.take();
        log.debug("List任务已从队列取出，当前队列大小: {}", listQueue.size());
        return task;
    }

    public boolean tryOfferListTask(SubTaskStatus task) {
        return listQueue.offer(task);
    }

    public void offerListTask(SubTaskStatus task) {
        if (!listQueue.offer(task)) throw new IllegalStateException("List task queue is full");
    }

    public void removeTaskMessages(Integer taskId) {
        if (listQueue != null) listQueue.removeIf(task -> taskId.equals(task.getTaskIndex()));
        if (putQueue != null) putQueue.removeIf(message -> taskId.equals(message.getTaskIndex()));
        if (fallbackQueue != null) fallbackQueue.removeIf(message -> taskId.equals(message.getTaskIndex()));
    }

    public void removeTaskProgress(Integer taskId) {
        if (progressQueue != null) progressQueue.removeIf(message -> taskId.equals(message.getTaskIndex()));
    }

    public void putPutMessage(ObjectListMessage message) throws InterruptedException {
        putQueue.put(message);
        log.debug("Put消息已放入队列，当前队列大小: {}", putQueue.size());
    }

    public ObjectListMessage takePutMessage() throws InterruptedException {
        ObjectListMessage message = putQueue.take();
        log.debug("Put消息已从队列取出，当前队列大小: {}", putQueue.size());
        return message;
    }

    public boolean offerProgressMessage(ProgressMessage message) {
        return progressQueue.offer(message);
    }

    public ProgressMessage takeProgressMessage() throws InterruptedException {
        return progressQueue.take();
    }

    public ProgressMessage takeProgressMessageNoBlock() {
        return progressQueue.poll();
    }

    public int drainProgressQueue(java.util.Collection<? super ProgressMessage> collection, int maxElements) {
        return progressQueue.drainTo(collection, maxElements);
    }

    public int listQueueSize() {
        return listQueue.size();
    }

    public int listQueueCapacity() {
        return bufferQueueConfig.getList().getSize();
    }

    public int putQueueSize() {
        return putQueue.size();
    }

    public int putQueueCapacity() {
        return bufferQueueConfig.getPut().getSize();
    }

    public int progressQueueSize() {
        return progressQueue.size();
    }

    public int progressQueueCapacity() {
        return bufferQueueConfig.getProgress().getSize();
    }

    public boolean progressQueueIsEmpty() {
        return progressQueue.isEmpty();
    }

    public void clearListQueue() {
        listQueue.clear();
        log.info("List队列已清空");
    }

    public void clearPutQueue() {
        putQueue.clear();
        log.info("Put队列已清空");
    }

    public void clearExploreQueue() {
        exploreQueue.clear();
        log.info("Explore队列已清空");
    }

    public void clearProgressQueue() {
        progressQueue.clear();
        log.info("Progress队列已清空");
    }

    public void putFailedKey(FailedKeyMessage message) throws InterruptedException {
        failedKeyQueue.put(message);
    }

    public boolean offerFailedKey(FailedKeyMessage message) {
        return failedKeyQueue.offer(message);
    }

    public FailedKeyMessage takeFailedKey() throws InterruptedException {
        return failedKeyQueue.take();
    }

    public int drainFailedKeyQueue(java.util.Collection<? super FailedKeyMessage> collection, int maxElements) {
        return failedKeyQueue.drainTo(collection, maxElements);
    }

    public int failedKeyQueueSize() {
        return failedKeyQueue.size();
    }

    public int failedKeyQueueCapacity() {
        return bufferQueueConfig.getFailed().getSize();
    }

    public void putFallback(FallbackMessage message) throws InterruptedException {
        fallbackQueue.put(message);
    }

    public boolean offerFallback(FallbackMessage message) {
        return fallbackQueue.offer(message);
    }

    public FallbackMessage takeFallback() throws InterruptedException {
        return fallbackQueue.take();
    }

    public int fallbackQueueSize() {
        return fallbackQueue.size();
    }

    public int fallbackQueueCapacity() {
        return bufferQueueConfig.getFallback().getSize();
    }

    public void clearFailedKeyQueue() {
        failedKeyQueue.clear();
        log.info("FailedKey队列已清空");
    }

    public void clearFallbackQueue() {
        fallbackQueue.clear();
        log.info("Fallback队列已清空");
    }
}
