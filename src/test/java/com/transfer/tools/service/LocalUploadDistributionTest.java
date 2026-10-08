package com.transfer.tools.service;

import com.amazonaws.services.s3.AmazonS3;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.config.RateLimitConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.storage.RocksDBService;
import org.junit.*;
import org.junit.rules.TemporaryFolder;
import org.springframework.http.MediaType;
import org.springframework.test.util.ReflectionTestUtils;
import org.springframework.test.web.client.MockRestServiceServer;
import org.springframework.web.client.RestTemplate;

import java.util.*;

import static org.junit.Assert.*;
import static org.mockito.ArgumentMatchers.*;
import static org.mockito.Mockito.*;
import static org.springframework.test.web.client.match.MockRestRequestMatchers.*;
import static org.springframework.test.web.client.response.MockRestResponseCreators.*;

public class LocalUploadDistributionTest {
    @Rule public TemporaryFolder temp=new TemporaryFolder();
    private RocksDBService db;
    private MasterLocalTransferService service;
    private LocalParentTaskService parents;
    private SubTaskManagerService tasks;
    private MasterTaskBandwidthService bandwidth;
    private MockRestServiceServer workers;
    private static final String A="10.0.0.1", B="10.0.0.2";

    @Before public void setup() throws Exception {
        db=new RocksDBService();ReflectionTestUtils.setField(db,"dbPath",temp.newFolder("db").getAbsolutePath());db.init();
        tasks=mock(SubTaskManagerService.class);when(tasks.allocateTaskIdRange(anyInt())).thenReturn(101);
        parents=new LocalParentTaskService();ReflectionTestUtils.setField(parents,"rocksDBService",db);
        ReflectionTestUtils.setField(parents,"subTaskManagerService",tasks);
        DistributedConfig config=mock(DistributedConfig.class);when(config.getWorkerIps()).thenReturn(Arrays.asList(A,B));
        when(config.getPort()).thenReturn(1785);when(config.isMaster()).thenReturn(true);
        AmazonS3 s3=mock(AmazonS3.class);when(s3.doesBucketExistV2("target")).thenReturn(true);
        S3ClientManager clients=mock(S3ClientManager.class);when(clients.getClient("qa")).thenReturn(s3);
        bandwidth=mock(MasterTaskBandwidthService.class);
        RateLimitConfig rates=new RateLimitConfig();rates.setBandwidth(100_000_000L);
        service=new MasterLocalTransferService();ReflectionTestUtils.setField(service,"distributedConfig",config);
        ReflectionTestUtils.setField(service,"normalizationService",new EndpointNormalizationService());
        ReflectionTestUtils.setField(service,"s3ClientManager",clients);ReflectionTestUtils.setField(service,"parentTaskService",parents);
        ReflectionTestUtils.setField(service,"rateLimitConfig",rates);ReflectionTestUtils.setField(service,"taskBandwidth",bandwidth);
        workers=MockRestServiceServer.bindTo((RestTemplate)ReflectionTestUtils.getField(service,"restTemplate")).build();
    }
    @After public void close(){db.destroy();}

    private LocalTransferRequest request(String mode,String... nodes) {
        LocalTransferRequest request=new LocalTransferRequest();request.setDirection(TransferDirection.LOCAL_TO_OSS);
        request.setDistributionMode(mode);request.setTaskBandwidthLimitMbps(100.0);
        TransferEndpoint source=new TransferEndpoint();source.setType(EndpointType.LOCAL);
        List<LocalNodeBinding> bindings=new ArrayList<>();for(String node:nodes)bindings.add(new LocalNodeBinding(node,"/shared/source"));
        source.setNodeBindings(bindings);request.setSourceEndpoint(source);
        TransferEndpoint target=new TransferEndpoint();target.setType(EndpointType.OSS);target.setS3Alias("qa");target.setBucket("target");
        request.setDestinationEndpoint(target);return request;
    }
    private void preflight(String node,String kind,String mode) {
        workers.expect(requestTo("http://"+node+":1785/api/worker/local-transfers/validate"))
                .andExpect(jsonPath("$.distributionMode").value(mode))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"valid\":true,\"pathType\":\""+kind+"\"}}",MediaType.APPLICATION_JSON));
    }
    private void discovery(String node,String mode,int count) {
        workers.expect(requestTo("http://"+node+":1785/api/worker/local-transfers/discover"))
                .andExpect(jsonPath("$.distributionMode").value(mode))
                .andExpect(jsonPath("$.sourceEndpoint.nodeBindings.length()").value(count))
                .andRespond(withSuccess("{\"success\":true,\"data\":{\"accepted\":true}}",MediaType.APPLICATION_JSON));
    }
    private void untouched() {
        assertTrue(parents.getAll().isEmpty());verify(tasks,never()).allocateTaskIdRange(anyInt());verifyZeroInteractions(bandwidth);
    }
    @Test public void singleNodeDirectoryPreviewAndCreateAlwaysUseAffinityAndFullBudget() {
        LocalTransferRequest request=request("balanced",B);
        preflight(B,"DIRECTORY","affinity");preflight(B,"DIRECTORY","affinity");discovery(B,"affinity",1);
        assertEquals(true,service.validate(Collections.singletonList(request)).get("valid"));
        assertEquals("affinity",request.getDistributionMode());assertEquals(Double.valueOf(100_000_000L),request.getBandwidthBitsPerSecond());
        service.create(Collections.singletonList(request));
        LocalTransferRequest saved=parents.get(101).getRequest();
        assertEquals("affinity",saved.getDistributionMode());assertEquals(1,saved.getSourceEndpoint().getNodeBindings().size());
        assertEquals(Double.valueOf(100.0),saved.getTaskBandwidthLimitMbps());
        verify(bandwidth).prepareLocal(argThat(r->"affinity".equals(r.getDistributionMode())&&r.getSourceEndpoint().getNodeBindings().size()==1));
        workers.verify();
    }
    @Test public void multiNodeDirectoryPreviewAndCreateOverrideLegacyAffinityWithBalanced() {
        LocalTransferRequest request=request("affinity",A,B);
        for(int n=0;n<2;n++){preflight(A,"DIRECTORY","balanced");preflight(B,"DIRECTORY","balanced");}
        discovery(B,"balanced",2);
        Map<String,Object> preview=service.validate(Collections.singletonList(request));assertEquals(true,preview.get("valid"));
        for(Object row:(List<?>)preview.get("nodeBindings"))assertEquals("balanced",((Map)row).get("distributionMode"));
        Map<String,Object> created=service.create(Collections.singletonList(request));
        assertEquals("balanced",parents.get(101).getRequest().getDistributionMode());
        assertEquals("balanced",((Map)((List<?>)created.get("acceptedNodeBindings")).get(0)).get("distributionMode"));
        assertEquals(Double.valueOf(50_000_000L),request.getBandwidthBitsPerSecond());workers.verify();
    }
    @Test public void singleFileRequiresExactlyOneExplicitNodeAndKeepsItsWholeBudget() {
        LocalTransferRequest request=request("balanced",A);
        preflight(A,"FILE","affinity");discovery(A,"affinity",1);
        service.create(Collections.singletonList(request));
        assertEquals("affinity",parents.get(101).getRequest().getDistributionMode());
        assertEquals(A,parents.get(101).getRequest().getSourceEndpoint().getNodeBindings().get(0).getExecutionNodeIp());
        assertEquals(Double.valueOf(100_000_000L),request.getBandwidthBitsPerSecond());workers.verify();
    }
    @Test public void fileOnMultipleNodesFailsPreviewOnEveryBindingWithoutSilentlyChoosingOne() {
        LocalTransferRequest request=request("affinity",A,B);
        preflight(A,"FILE","balanced");preflight(B,"FILE","balanced");
        Map<String,Object> preview=service.validate(Collections.singletonList(request));assertEquals(false,preview.get("valid"));
        List<?> rows=(List<?>)preview.get("nodeBindings");assertEquals(2,rows.size());
        for(Object row:rows){assertEquals(false,((Map)row).get("valid"));assertEquals("SINGLE_FILE_REQUIRES_ONE_NODE",((Map)row).get("code"));
            assertEquals("单文件上传请只保留一个执行节点",((Map)row).get("message"));}
        assertEquals(2,request.getSourceEndpoint().getNodeBindings().size());untouched();workers.verify();
    }
    @Test public void directCreateAlsoRejectsMultiNodeFileBeforeAllocatingIdsOrPolicies() {
        LocalTransferRequest request=request("affinity",A,B);preflight(A,"FILE","balanced");
        try{service.create(Collections.singletonList(request));fail("multi-node file");}
        catch(TransferValidationException expected){assertEquals("SINGLE_FILE_REQUIRES_ONE_NODE",expected.getCode());}
        untouched();workers.verify();
    }
    @Test public void laterInvalidFileRejectsWholeBatchBeforePersistingEarlierValidDirectory() {
        LocalTransferRequest directory=request("balanced",A),file=request("affinity",A,B);
        preflight(A,"DIRECTORY","affinity");preflight(A,"FILE","balanced");
        try{service.create(Arrays.asList(directory,file));fail("batch must validate before persistence");}
        catch(TransferValidationException expected){assertEquals("SINGLE_FILE_REQUIRES_ONE_NODE",expected.getCode());}
        untouched();workers.verify();
    }
    @Test public void unknownWorkerPathKindFailsClosedInsteadOfAllowingMultiNodeFile() {
        LocalTransferRequest request=request("affinity",A,B);preflight(A,"UNKNOWN","balanced");preflight(B,"UNKNOWN","balanced");
        Map<String,Object> preview=service.validate(Collections.singletonList(request));assertEquals(false,preview.get("valid"));
        for(Object row:(List<?>)preview.get("nodeBindings"))assertEquals("SOURCE_TYPE_UNAVAILABLE",((Map)row).get("code"));
        untouched();workers.verify();
    }
    @Test public void recoveryKeepsSavedAffinityWithMultipleMembersAndDoesNotRevalidateAsNewTask() {
        LocalTransferRequest saved=request("affinity",A,B);parents.create(Collections.singletonList(saved));
        discovery(B,"affinity",2);service.recoverPendingParents();
        assertEquals("affinity",parents.get(101).getRequest().getDistributionMode());
        assertEquals(2,parents.get(101).getRequest().getSourceEndpoint().getNodeBindings().size());
        verify(bandwidth).prepareLocal(argThat(r->"affinity".equals(r.getDistributionMode())&&r.getSourceEndpoint().getNodeBindings().size()==2));
        workers.verify();
    }
}
