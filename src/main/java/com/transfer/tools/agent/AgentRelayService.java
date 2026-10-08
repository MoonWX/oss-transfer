package com.transfer.tools.agent;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.transfer.tools.config.DistributedConfig;
import com.transfer.tools.model.*;
import com.transfer.tools.service.LocalParentTaskService;
import com.transfer.tools.service.NodeConfigService;
import com.transfer.tools.service.MasterTaskBandwidthService;
import com.transfer.tools.config.LocalTransferConfig;
import com.fasterxml.jackson.databind.node.ObjectNode;
import org.springframework.beans.factory.annotation.Autowired;
import org.springframework.http.*;
import org.springframework.http.client.SimpleClientHttpRequestFactory;
import org.springframework.stereotype.Service;
import org.springframework.web.client.RestTemplate;
import java.io.IOException;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.*;
import java.util.concurrent.atomic.AtomicReference;

/** An Agent scanner can deliver assigned local batches and read scheduling capacity only. */
@Service
public class AgentRelayService {
    private final DistributedConfig config;
    private final NodeConfigService nodes;
    private final LocalParentTaskService parents;
    private final ObjectMapper mapper;
    private final RestTemplate http;
    @Autowired private MasterTaskBandwidthService bandwidth;
    @Autowired private LocalTransferConfig localConfig;

    public AgentRelayService(DistributedConfig config, NodeConfigService nodes, LocalParentTaskService parents, ObjectMapper mapper) {
        this.config = config; this.nodes = nodes; this.parents = parents; this.mapper = mapper;
        SimpleClientHttpRequestFactory factory = new SimpleClientHttpRequestFactory();
        factory.setConnectTimeout(3000); factory.setReadTimeout(15000);
        http = new RestTemplate(factory);
    }

    public RpcResponse relay(String caller, RpcRelayRequest request) throws IOException {
        if (caller == null || request == null || !config.isMaster()) throw new SecurityException("Agent relay is not available");
        AgentRpcPolicy.requireAllowed(request.getMethod(), request.getPath());
        List<String> members = config.getWorkerIps();
        if (members == null || !members.contains(caller) || !members.contains(request.getAddress()))
            throw new SecurityException("Agent relay target is not a cluster member");
        byte[] body = AgentRpcPolicy.decode(request.getBody());
        Integer parentId = null;
        if ("GET".equals(request.getMethod()) && "/api/worker/worker-status".equals(request.getPath())) {
            if (body.length != 0) throw new IllegalArgumentException("Status request cannot contain a body");
        } else if ("POST".equals(request.getMethod()) && "/api/worker/local-transfers/batches".equals(request.getPath())) {
            LocalFileBatch batch = mapper.readValue(body, LocalFileBatch.class);
            LocalParentTaskRecord parent = batch == null ? null : parents.get(batch.getParentTaskId());
            if (parent == null || parents.isDeleted(batch.getParentTaskId()) || parents.isStopped(batch.getParentTaskId())
                    || !caller.equals(batch.getDiscoveryWorker()) || batch.getNodeBinding() == null
                    || !request.getAddress().equals(batch.getNodeBinding().getExecutionNodeIp())
                    || batch.getRequest() == null || !batch.getParentTaskId().equals(batch.getRequest().getParentTaskId())
                    || !sameTransfer(parent.getRequest(), batch.getRequest()))
                throw new SecurityException("Local batch is not authorized by its master task");
            validateAssignment(caller, request.getAddress(), parent.getRequest(), batch);
            // Runtime bandwidth may have changed after this scanner checkpoint was written.
            // Forward the Master's current policy, never any limit supplied by the Agent.
            LocalTransferRequest canonical = mapper.convertValue(parent.getRequest(), LocalTransferRequest.class);
            if (canonical.getTaskBandwidthScopeId() != null) {
                TaskBandwidthPolicy policy = bandwidth.desired(canonical.getTaskBandwidthScopeId());
                if (policy == null || !policy.getMembers().contains(caller) || !policy.getMembers().contains(request.getAddress()))
                    throw new SecurityException("Local batch bandwidth scope is not authorized");
                canonical.setTaskBandwidthLimitMbps(policy.getLimitMbps());
            }
            batch.setRequest(canonical);
            body = mapper.writeValueAsBytes(batch);
            parentId = batch.getParentTaskId();
        } else throw new SecurityException("Agent relay operation is not permitted");
        final byte[] forwarded = body;
        if (parentId != null) {
            AtomicReference<RpcResponse> result = new AtomicReference<>();
            if (!parents.whileActive(parentId, () -> result.set(send(request, forwarded))))
                throw new SecurityException("Master task has stopped");
            return result.get();
        }
        return send(request, forwarded);
    }

    private RpcResponse send(RpcRelayRequest request, byte[] body) {
        HttpHeaders headers = new HttpHeaders();
        headers.setContentType(MediaType.APPLICATION_JSON);
        headers.set("X-Internal-Token", config.getInternalToken());
        ResponseEntity<byte[]> response = http.exchange("http://" + request.getAddress() + ":" + config.getPort() + request.getPath(),
                HttpMethod.valueOf(request.getMethod()), new HttpEntity<>(body, headers), byte[].class);
        AgentRpcPolicy.requireSize(response.getBody());
        Map<String, String> resultHeaders = new LinkedHashMap<>();
        resultHeaders.put("Content-Type", response.getHeaders().getFirst(HttpHeaders.CONTENT_TYPE));
        return new RpcResponse(response.getStatusCodeValue(), response.getBody(), AgentRpcPolicy.responseHeaders(resultHeaders));
    }

    private boolean sameTransfer(LocalTransferRequest saved, LocalTransferRequest incoming) {
        if (saved == null || incoming == null || saved.getDirection() != TransferDirection.LOCAL_TO_OSS) return false;
        ObjectNode expected = mapper.valueToTree(saved), received = mapper.valueToTree(incoming);
        expected.remove("taskBandwidthLimitMbps"); received.remove("taskBandwidthLimitMbps");
        return expected.equals(received);
    }

    private void validateAssignment(String caller, String target, LocalTransferRequest saved, LocalFileBatch batch) {
        List<LocalNodeBinding> bindings = saved.getSourceEndpoint() == null ? null : saved.getSourceEndpoint().getNodeBindings();
        if (bindings == null || bindings.isEmpty()
                || !caller.equals(bindings.get(Math.floorMod(batch.getParentTaskId(), bindings.size())).getExecutionNodeIp()))
            throw new SecurityException("Only the task's assigned discovery Agent can deliver local batches");
        LocalNodeBinding targetBinding = null;
        for (LocalNodeBinding binding : bindings) if (target.equals(binding.getExecutionNodeIp())) targetBinding = binding;
        if (targetBinding == null || !targetBinding.equals(batch.getNodeBinding())
                || batch.getShardIndex() == null || batch.getShardIndex() < 0
                || !Objects.equals(batch.getShardId(), batch.getParentTaskId() + "-local-" + batch.getShardIndex())
                || !Integer.valueOf(0).equals(batch.getAttempt()) || batch.getEntries() == null)
            throw new SecurityException("Local batch assignment is invalid");
        int configured = localConfig == null ? 1000 : Math.max(1, localConfig.getBatch().getMaxItems());
        if (batch.getEntries().size() > Math.min(100000, configured)) throw new SecurityException("Local batch item limit exceeded");
        Set<String> keys = new HashSet<>();
        for (LocalFileEntry entry : batch.getEntries()) {
            if (entry == null || entry.getRelativeKey() == null || entry.getRelativeKey().isEmpty()
                    || entry.getRelativeKey().length() > 4096 || entry.getRelativeKey().indexOf('\0') >= 0
                    || entry.getRelativeKey().indexOf('\\') >= 0 || entry.getSize() == null || entry.getSize() < 0
                    || entry.getLastModified() == null || entry.getLastModified() < 0)
                throw new SecurityException("Local batch contains invalid file metadata");
            Path relative = Paths.get(entry.getRelativeKey());
            if (relative.isAbsolute() || relative.normalize().startsWith("..")
                    || !relative.normalize().toString().equals(entry.getRelativeKey()) || !keys.add(entry.getRelativeKey()))
                throw new SecurityException("Local batch key is outside its assigned source root");
        }
    }
}
