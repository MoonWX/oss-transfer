package com.transfer.tools.controller;

import com.amazonaws.services.s3.AmazonS3;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.executor.ExploreExecutorService;
import com.transfer.tools.model.BucketPair;
import com.transfer.tools.service.EndpointNormalizationService;
import com.transfer.tools.service.S3ClientManager;
import com.transfer.tools.service.SubTaskManagerService;
import com.transfer.tools.service.TaskCreationService;
import org.junit.Before;
import org.junit.Test;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.servlet.MockMvc;
import org.springframework.test.web.servlet.ResultActions;
import org.springframework.test.web.servlet.setup.MockMvcBuilders;

import java.util.Collections;

import static org.hamcrest.Matchers.containsString;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyList;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;
import static org.springframework.test.web.servlet.request.MockMvcRequestBuilders.post;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.jsonPath;
import static org.springframework.test.web.servlet.result.MockMvcResultMatchers.status;

public class MasterControllerSameBucketTest {
    private MockMvc mvc;
    private ExploreExecutorService exploration;
    private SubTaskManagerService taskManager;

    @Before
    public void setUp() {
        MasterController controller = new MasterController();
        exploration = mock(ExploreExecutorService.class);
        taskManager = mock(SubTaskManagerService.class);
        when(taskManager.allocateTaskIdRange(org.mockito.ArgumentMatchers.anyInt())).thenReturn(100);
        DistributedConfig config = mock(DistributedConfig.class);
        when(config.getWorkerIps()).thenReturn(Collections.singletonList("10.0.0.1"));
        when(config.getLocalIp()).thenReturn("10.0.0.1");
        S3ClientManager clients = mock(S3ClientManager.class);
        when(clients.getClient(anyString())).thenReturn(mock(AmazonS3.class));
        when(exploration.getExploreQueueRemainingCapacity()).thenReturn(100);
        when(exploration.offerExploreTask(any(BucketPair.class))).thenReturn(true);
        TaskCreationService creations = mock(TaskCreationService.class);
        when(creations.create(any(BucketPair.class), org.mockito.ArgumentMatchers.eq(true))).thenReturn("00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002");
        ReflectionTestUtils.setField(controller, "taskCreationService", creations);
        ReflectionTestUtils.setField(controller, "distributedConfig", config);
        ReflectionTestUtils.setField(controller, "exploreExecutorService", exploration);
        ReflectionTestUtils.setField(controller, "subTaskManagerService", taskManager);
        ReflectionTestUtils.setField(controller, "listExecutionService", mock(com.transfer.tools.executor.ListExecutionService.class));
        ReflectionTestUtils.setField(controller, "s3ClientManager", clients);
        ReflectionTestUtils.setField(controller, "endpointNormalizationService", new EndpointNormalizationService());
        mvc = MockMvcBuilders.standaloneSetup(controller).build();
    }

    @Test
    public void bucketImportRejectsSameBucketBeforeAnyTaskIsQueued() throws Exception {
        submit("add-buckets", pair("production", "same", "production", "same"))
                .andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.message", containsString("同一存储桶")));
        verify(exploration, never()).offerExploreTask(any(BucketPair.class));
    }

    @Test
    public void entireBucketBatchIsValidatedBeforeQueueing() throws Exception {
        submit("add-buckets", pair("production", "source", "production", "dest") + ","
                + pair("production", "same", "production", "same"))
                .andExpect(jsonPath("$.success").value(false));
        verify(exploration, never()).offerExploreTask(any(BucketPair.class));
    }

    @Test
    public void sameAliasDifferentBucketsAndSameBucketDifferentAliasesRemainAccepted() throws Exception {
        submit("add-buckets", pair("production", "source", "production", "dest") + ","
                + pair("production", "same", "backup", "same"))
                .andExpect(jsonPath("$.success").value(true))
                .andExpect(jsonPath("$.addedCount").value(2))
                .andExpect(jsonPath("$.creationIds.length()").value(2));
        verify(exploration, times(2)).offerExploreTask(any(BucketPair.class));
    }

    @Test
    public void bucketQueuePartialAcceptanceKeepsItsExistingCountContract() throws Exception {
        when(exploration.offerExploreTask(any(BucketPair.class))).thenReturn(true, false);
        submit("add-buckets", pair("production", "source", "production", "dest") + ","
                + pair("production", "other", "production", "backup"))
                .andExpect(jsonPath("$.success").value(true))
                .andExpect(jsonPath("$.addedCount").value(1));
    }

    @Test
    public void subtaskImportRejectsSameBucketBeforeImportingTheBatch() throws Exception {
        submit("import", pair("production", "source", "production", "dest") + ","
                + pair("production", "same", "production", "same"))
                .andExpect(jsonPath("$.success").value(false))
                .andExpect(jsonPath("$.message", containsString("第 2 个任务")))
                .andExpect(jsonPath("$.message", containsString("同一存储桶")));
        verify(taskManager, never()).importTasksWithTaskId(anyList());
    }

    @Test
    public void subtaskImportAllowsDistinctBucketsOrAliases() throws Exception {
        submit("import", pair("production", "source", "production", "dest") + ","
                + pair("production", "same", "backup", "same"))
                .andExpect(jsonPath("$.success").value(true))
                .andExpect(jsonPath("$.importedCount").value(2));
        verify(taskManager).importTasksWithTaskId(anyList());
    }

    @Test
    public void bothApisRejectWhitespaceAndDefaultAliasVariants() throws Exception {
        for (String api : new String[]{"add-buckets", "import"}) {
            submit(api, pair(" production ", " same ", "production", "same"))
                    .andExpect(jsonPath("$.success").value(false))
                    .andExpect(jsonPath("$.message", containsString("同一存储桶")));
            submit(api, "{\"sourceBucket\":\"same\",\"destBucket\":\"same\",\"destS3Alias\":\"default\"}")
                    .andExpect(jsonPath("$.success").value(false))
                    .andExpect(jsonPath("$.message", containsString("同一存储桶")));
        }
        verify(exploration, never()).offerExploreTask(any(BucketPair.class));
        verify(taskManager, never()).importTasksWithTaskId(anyList());
    }

    @Test public void invalidKeyMappingsRejectWholeBucketBatchBeforeQueueing() throws Exception {
        String base = pair("production", "source", "production", "dest");
        for (String extra : new String[]{
                "\"prefix\":\"photos/\",\"objectKey\":\"photos/a\"",
                "\"keyMapping\":{\"mode\":\"EXACT_KEY\",\"targetKey\":\"renamed\"}",
                "\"prefix\":\"photos/\",\"keyMapping\":{\"mode\":\"REPLACE_PREFIX\",\"sourcePrefix\":\"other/\",\"targetPrefix\":\"archive/\"}",
                "\"keyMapping\":{\"mode\":\"ADD_PREFIX\",\"targetPrefix\":\"\"}"}) {
            submit("add-buckets", base + "," + with(base, extra)).andExpect(jsonPath("$.success").value(false));
        }
        verify(exploration, never()).offerExploreTask(any(BucketPair.class));
    }

    @Test public void exactSourceAndReplacementMappingAreAcceptedAndPropagated() throws Exception {
        String base = pair("production", "source", "production", "dest");
        submit("add-buckets", with(base, "\"objectKey\":\"photos/a\",\"keyMapping\":{\"mode\":\"EXACT_KEY\",\"targetKey\":\" archive/a \"}"))
                .andExpect(jsonPath("$.success").value(true));
        submit("add-buckets", with(base, "\"prefix\":\"photos/\",\"keyMapping\":{\"mode\":\"REPLACE_PREFIX\",\"sourcePrefix\":\"photos/\",\"targetPrefix\":\"\"}"))
                .andExpect(jsonPath("$.success").value(true));
        org.mockito.ArgumentCaptor<BucketPair> captured = org.mockito.ArgumentCaptor.forClass(BucketPair.class);
        verify(exploration, times(2)).offerExploreTask(captured.capture());
        org.junit.Assert.assertEquals("photos/a", captured.getAllValues().get(0).getObjectKey());
        org.junit.Assert.assertEquals(" archive/a ", captured.getAllValues().get(0).getKeyMapping().getTargetKey());
        org.junit.Assert.assertEquals("", captured.getAllValues().get(1).getKeyMapping().getTargetPrefix());
    }

    @Test public void mappedSubtaskImportsStillValidateScopeAndSameBucket() throws Exception {
        String base = pair("production", "source", "production", "dest");
        submit("import", with(base, "\"keyMapping\":{\"mode\":\"EXACT_KEY\",\"targetKey\":\"a\"}"))
                .andExpect(jsonPath("$.success").value(false));
        submit("import", with(base, "\"sourceEndpoint\":{\"type\":\"OSS\",\"s3Alias\":\"production\",\"bucket\":\"source\",\"prefix\":\"photos/\",\"prefixFilterEnabled\":true},\"keyMapping\":{\"mode\":\"REPLACE_PREFIX\",\"sourcePrefix\":\"other/\"}"))
                .andExpect(jsonPath("$.success").value(false));
        submit("add-buckets", with(pair("production", "same", "production", "same"), "\"objectKey\":\"a\",\"keyMapping\":{\"mode\":\"EXACT_KEY\",\"targetKey\":\"b\"}"))
                .andExpect(jsonPath("$.success").value(false));
        verify(taskManager, never()).importTasksWithTaskId(anyList());
        verify(exploration, never()).offerExploreTask(any(BucketPair.class));
    }

    private String with(String base, String fields) { return base.substring(0, base.length() - 1) + "," + fields + "}"; }

    private ResultActions submit(String operation, String rows) throws Exception {
        return mvc.perform(post("/api/master/" + operation)
                        .contentType(MediaType.APPLICATION_JSON).content("[" + rows + "]"))
                .andExpect(status().isOk());
    }

    private String pair(String sourceAlias, String sourceBucket, String destinationAlias, String destinationBucket) {
        return "{\"sourceS3Alias\":\"" + sourceAlias + "\",\"sourceBucket\":\"" + sourceBucket
                + "\",\"destS3Alias\":\"" + destinationAlias + "\",\"destBucket\":\"" + destinationBucket
                + "\",\"overwrite\":true}";
    }
}
