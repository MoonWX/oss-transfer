package com.transfer.tools.executor;

import com.transfer.tools.config.BufferQueueConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.service.LocalTaskControlService;
import org.junit.Test;
import org.springframework.test.util.ReflectionTestUtils;
import java.util.Arrays;
import java.util.concurrent.*;
import static org.junit.Assert.*;

public class QueueBackpressureTest {
    private BufferQueueManager queue() {
        BufferQueueConfig config=new BufferQueueConfig(); config.getList().setSize(1); config.getPut().setSize(1);
        BufferQueueManager queue=new BufferQueueManager(); ReflectionTestUtils.setField(queue,"bufferQueueConfig",config); queue.init(); return queue;
    }
    private SubTaskStatus task(int id) { SubTaskStatus task=new SubTaskStatus();task.setTaskIndex(id);return task; }
    @Test(timeout=5000) public void fullListQueueWaitsAndEventuallyEnqueuesEverySelectedTask() throws Exception {
        BufferQueueManager queue=queue(); queue.offerListTask(task(0));
        ListExecutionService service=new ListExecutionService();
        ReflectionTestUtils.setField(service,"bufferQueueManager",queue);
        ReflectionTestUtils.setField(service,"localTaskControlService",new LocalTaskControlService());
        ExecutorService executor=Executors.newSingleThreadExecutor();
        try {
            Future<?> future=executor.submit(()->{service.enqueueStartedTasks(Arrays.asList(task(1),task(2)));return null;});
            try {future.get(100,TimeUnit.MILLISECONDS);fail("producer must wait at capacity");} catch(TimeoutException expected){}
            assertEquals(Integer.valueOf(0),queue.takeListTask().getTaskIndex());
            assertEquals(Integer.valueOf(1),queue.takeListTask().getTaskIndex());
            assertEquals(Integer.valueOf(2),queue.takeListTask().getTaskIndex());
            future.get(2,TimeUnit.SECONDS);
        } finally {executor.shutdownNow();}
    }
    @Test(timeout=5000) public void stoppingBlockedListProducerDoesNotDropOtherSelectedTasks() throws Exception {
        BufferQueueManager queue=queue(); queue.offerListTask(task(0));
        LocalTaskControlService control=new LocalTaskControlService();
        ListExecutionService service=new ListExecutionService();
        ReflectionTestUtils.setField(service,"bufferQueueManager",queue);ReflectionTestUtils.setField(service,"localTaskControlService",control);
        ExecutorService executor=Executors.newSingleThreadExecutor();
        try {
            Future<?> future=executor.submit(()->{service.enqueueStartedTasks(Arrays.asList(task(1),task(2)));return null;});
            long deadline=System.nanoTime()+TimeUnit.SECONDS.toNanos(2);
            while(!control.isActive(1)&&System.nanoTime()<deadline)Thread.yield();
            assertTrue(control.isActive(1)); control.stop(1);
            assertEquals(Integer.valueOf(0),queue.takeListTask().getTaskIndex());
            assertEquals(Integer.valueOf(2),queue.takeListTask().getTaskIndex());
            future.get(2,TimeUnit.SECONDS); assertEquals(0,queue.listQueueSize());
        } finally {executor.shutdownNow();}
    }
    @Test(timeout=5000) public void fullPutQueueBlocksAndCancellationDoesNotEnqueueCancelledMessage() throws Exception {
        BufferQueueManager queue=queue(); ObjectListMessage first=new ObjectListMessage();queue.putPutMessage(first);
        LocalTaskControlService control=new LocalTaskControlService();ExecutorService executor=Executors.newSingleThreadExecutor();
        try {
            Future<?> future=executor.submit(()->{try(LocalTaskControlService.Operation ignored=control.begin(1)){queue.putPutMessage(new ObjectListMessage());}catch(InterruptedException expected){}return null;});
            long deadline=System.nanoTime()+TimeUnit.SECONDS.toNanos(2);
            while(!control.isActive(1)&&System.nanoTime()<deadline)Thread.yield();
            assertTrue(control.isActive(1));control.stop(1);future.get(2,TimeUnit.SECONDS);
            assertSame(first,queue.takePutMessage());assertEquals(0,queue.putQueueSize());
        } finally {executor.shutdownNow();}
    }
}
