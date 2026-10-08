package com.transfer.tools.service;

import com.transfer.tools.model.*;
import com.transfer.tools.util.OssKeyMapper;
import org.springframework.stereotype.Service;

import java.util.ArrayList;
import java.util.HashSet;
import java.util.List;
import java.util.Set;

@Service
public class EndpointNormalizationService {

    public void normalize(BucketPair pair) {
        TransferDirection direction = pair.getDirection() == null
                ? TransferDirection.OSS_TO_OSS : pair.getDirection();
        pair.setDirection(direction);
        if (pair.getSourceEndpoint() == null) {
            pair.setSourceEndpoint(direction == TransferDirection.LOCAL_TO_OSS
                    ? local(pair.getExecutionNodeIp(), pair.getLocalPath())
                    : oss(pair.getSourceS3Alias(), pair.getSourceBucket(), pair.getObjectKey(), pair.getPrefix()));
        }
        if (pair.getDestinationEndpoint() == null) {
            pair.setDestinationEndpoint(direction == TransferDirection.OSS_TO_LOCAL
                    ? local(pair.getExecutionNodeIp(), pair.getLocalPath())
                    : oss(pair.getDestS3Alias(), pair.getDestBucket(), null, null));
        }
        if (pair.getConflictPolicy() == null) {
            pair.setConflictPolicy(Boolean.TRUE.equals(pair.getOverwrite())
                    ? ConflictPolicy.OVERWRITE : ConflictPolicy.SKIP);
        }
        if (pair.getOverwrite() == null) {
            pair.setOverwrite(pair.getConflictPolicy() == ConflictPolicy.OVERWRITE);
        }
        normalizeLocalEndpoint(pair.getSourceEndpoint(), false);
        normalizeLocalEndpoint(pair.getDestinationEndpoint(), false);
        pair.setDistributionMode(normalizeMode(pair.getDistributionMode(), localEndpoint(pair.getSourceEndpoint(), pair.getDestinationEndpoint())));
        validateDirection(direction, pair.getSourceEndpoint(), pair.getDestinationEndpoint());
        backfillLegacy(pair);
        if (direction == TransferDirection.OSS_TO_OSS) {
            validateSourceFields(pair.getSourceEndpoint(), pair.getObjectKey(), pair.getPrefix());
            OssKeyMapper.validate(pair.getKeyMapping(), pair.getPrefix(), pair.getObjectKey());
            pair.getSourceEndpoint().setObjectKey(pair.getObjectKey());
            pair.getSourceEndpoint().setPrefix(pair.getPrefix());
            if (present(pair.getPrefix())) pair.getSourceEndpoint().setPrefixFilterEnabled(true);
        } else if (pair.getKeyMapping() != null) {
            throw new TransferValidationException("INVALID_KEY_MAPPING", "keyMapping only applies to OSS_TO_OSS");
        }
        TransferEndpoint local = localEndpoint(pair.getSourceEndpoint(), pair.getDestinationEndpoint());
        if (local != null) {
            pair.setStorageId(local.getStorageId());
            pair.setNodeBindings(local.getNodeBindings());
            pair.setEligibleWorkerNodes(nodeIps(local));
        }
    }

    /** Import range boundaries remain exclusive; an explicit endpoint prefix is a real filter. */
    public void normalizeAndValidate(TaskImport task) {
        if (task == null) throw new TransferValidationException("INVALID_TASK", "task is required");
        TransferDirection direction = task.getDirection() == null ? TransferDirection.OSS_TO_OSS : task.getDirection();
        if (direction != TransferDirection.OSS_TO_OSS) {
            if (task.getKeyMapping() != null)
                throw new TransferValidationException("INVALID_KEY_MAPPING", "keyMapping only applies to OSS_TO_OSS");
            return;
        }
        TransferEndpoint endpoint = task.getSourceEndpoint();
        if (present(task.getPrefix())) {
            if (endpoint == null) {
                endpoint = oss(task.getSourceS3Alias(), task.getSourceBucket(), task.getObjectKey(), task.getPrefix());
                task.setSourceEndpoint(endpoint);
            } else {
                validateSourceFields(endpoint, task.getObjectKey(), task.getPrefix());
                endpoint.setPrefix(task.getPrefix());
            }
            endpoint.setPrefixFilterEnabled(true);
        }
        String prefix = endpoint == null ? null : endpoint.getPrefix();
        if (endpoint != null) {
            validateSourceFields(endpoint, task.getObjectKey(), prefix);
            if (!present(task.getObjectKey())) task.setObjectKey(endpoint.getObjectKey());
            if (task.getKeyMapping() != null && task.getKeyMapping().getMode() == OssKeyMapping.Mode.REPLACE_PREFIX)
                endpoint.setPrefixFilterEnabled(true);
        }
        OssKeyMapper.validate(task.getKeyMapping(), prefix, task.getObjectKey());
        if (endpoint == null && present(task.getObjectKey())) {
            task.setSourceEndpoint(oss(task.getSourceS3Alias(), task.getSourceBucket(), task.getObjectKey(), null));
        } else if (endpoint != null) endpoint.setObjectKey(task.getObjectKey());
    }

    private static void validateSourceFields(TransferEndpoint endpoint, String objectKey, String prefix) {
        if (present(endpoint.getObjectKey()) && present(objectKey) && !endpoint.getObjectKey().equals(objectKey))
            throw new TransferValidationException("INVALID_SOURCE_SELECTION", "Conflicting objectKey fields");
        if (present(endpoint.getPrefix()) && present(prefix) && !endpoint.getPrefix().equals(prefix))
            throw new TransferValidationException("INVALID_SOURCE_SELECTION", "Conflicting prefix fields");
    }

    private static boolean present(String value) { return value != null && !value.isEmpty(); }

    public void normalize(ListSubTask task) {
        TransferDirection direction = task.getDirection() == null
                ? TransferDirection.OSS_TO_OSS : task.getDirection();
        task.setDirection(direction);
        if (task.getSourceEndpoint() == null) {
            task.setSourceEndpoint(direction == TransferDirection.LOCAL_TO_OSS
                    ? local(task.getExecutionNodeIp(), task.getLocalPath())
                    : oss(task.getSourceS3Alias(), task.getSourceBucket(), task.getObjectKey(), task.getStartKey()));
        }
        if (task.getDestinationEndpoint() == null) {
            task.setDestinationEndpoint(direction == TransferDirection.OSS_TO_LOCAL
                    ? local(task.getExecutionNodeIp(), task.getLocalPath())
                    : oss(task.getDestS3Alias(), task.getDestBucket(), null, null));
        }
        if (task.getConflictPolicy() == null) {
            task.setConflictPolicy(Boolean.TRUE.equals(task.getOverwrite())
                    ? ConflictPolicy.OVERWRITE : ConflictPolicy.SKIP);
        }
        if (task.getOverwrite() == null) {
            task.setOverwrite(task.getConflictPolicy() == ConflictPolicy.OVERWRITE);
        }
        normalizeLocalEndpoint(task.getSourceEndpoint(), false);
        normalizeLocalEndpoint(task.getDestinationEndpoint(), false);
        TransferEndpoint local = localEndpoint(task.getSourceEndpoint(), task.getDestinationEndpoint());
        task.setDistributionMode(normalizeMode(task.getDistributionMode(), local));
        if (local != null) {
            if (task.getStorageId() == null) task.setStorageId(local.getStorageId());
            if (task.getNodeBindings() == null) task.setNodeBindings(local.getNodeBindings());
            if (task.getEligibleWorkerNodes() == null) task.setEligibleWorkerNodes(nodeIps(local));
        }
    }

    public void normalize(SubTaskStatus task) {
        TransferDirection direction = task.getDirection() == null
                ? TransferDirection.OSS_TO_OSS : task.getDirection();
        task.setDirection(direction);
        if (task.getSourceEndpoint() == null) {
            task.setSourceEndpoint(direction == TransferDirection.LOCAL_TO_OSS
                    ? local(task.getExecutionNodeIp(), task.getLocalPath())
                    : oss(task.getSourceS3Alias(), task.getSourceBucket(), task.getObjectKey(), task.getStartKey()));
        }
        if (task.getDestinationEndpoint() == null) {
            task.setDestinationEndpoint(direction == TransferDirection.OSS_TO_LOCAL
                    ? local(task.getExecutionNodeIp(), task.getLocalPath())
                    : oss(task.getDestS3Alias(), task.getDestBucket(), null, null));
        }
        if (task.getConflictPolicy() == null) {
            task.setConflictPolicy(Boolean.TRUE.equals(task.getOverwrite())
                    ? ConflictPolicy.OVERWRITE : ConflictPolicy.SKIP);
        }
        if (task.getOverwrite() == null) {
            task.setOverwrite(task.getConflictPolicy() == ConflictPolicy.OVERWRITE);
        }
        normalizeLocalEndpoint(task.getSourceEndpoint(), false);
        normalizeLocalEndpoint(task.getDestinationEndpoint(), false);
        TransferEndpoint local = localEndpoint(task.getSourceEndpoint(), task.getDestinationEndpoint());
        task.setDistributionMode(normalizeMode(task.getDistributionMode(), local));
        if (local != null) {
            if (task.getStorageId() == null) task.setStorageId(local.getStorageId());
            if (task.getNodeBindings() == null) task.setNodeBindings(local.getNodeBindings());
            if (task.getEligibleWorkerNodes() == null) task.setEligibleWorkerNodes(nodeIps(local));
        }
    }

    public LocalTransferRequest normalizeAndValidate(LocalTransferRequest request) {
        if (request == null || request.getDirection() == null) {
            throw new TransferValidationException("INVALID_DIRECTION", "direction is required");
        }
        if (request.getDirection() == TransferDirection.OSS_TO_OSS) {
            throw new TransferValidationException("INVALID_DIRECTION", "local transfer API only accepts OSS_TO_LOCAL or LOCAL_TO_OSS");
        }
        validateDirection(request.getDirection(), request.getSourceEndpoint(), request.getDestinationEndpoint());
        if (request.getConflictPolicy() == null) {
            request.setConflictPolicy(ConflictPolicy.SKIP);
        }
        validateEndpoint(request.getSourceEndpoint());
        validateEndpoint(request.getDestinationEndpoint());
        TransferEndpoint local = localEndpoint(request.getSourceEndpoint(), request.getDestinationEndpoint());
        request.setDistributionMode(normalizeMode(request.getDistributionMode(), local));
        TransferEndpoint oss = request.getDirection() == TransferDirection.OSS_TO_LOCAL
                ? request.getSourceEndpoint() : request.getDestinationEndpoint();
        if (isBlank(oss.getS3Alias()) || isBlank(oss.getBucket())) {
            throw new TransferValidationException("INVALID_ENDPOINT", "OSS endpoint requires s3Alias and bucket");
        }
        if (request.getDirection() == TransferDirection.OSS_TO_LOCAL
                && !isBlank(oss.getObjectKey()) && !isBlank(oss.getPrefix())) {
            throw new TransferValidationException("INVALID_SOURCE_SELECTION", "objectKey and prefix are mutually exclusive");
        }
        if (request.getDirection() == TransferDirection.OSS_TO_LOCAL
                && !isBlank(request.getTargetRelativePath()) && isBlank(oss.getObjectKey())) {
            throw new TransferValidationException("INVALID_SOURCE_SELECTION", "targetRelativePath is only valid for an exact objectKey");
        }
        return request;
    }

    private void backfillLegacy(BucketPair pair) {
        if (pair.getSourceEndpoint().getType() == EndpointType.OSS) {
            if (isBlank(pair.getSourceS3Alias())) pair.setSourceS3Alias(pair.getSourceEndpoint().getS3Alias());
            if (isBlank(pair.getSourceBucket())) pair.setSourceBucket(pair.getSourceEndpoint().getBucket());
            if (!present(pair.getPrefix())) pair.setPrefix(pair.getSourceEndpoint().getPrefix());
            if (!present(pair.getObjectKey())) pair.setObjectKey(pair.getSourceEndpoint().getObjectKey());
        }
        if (pair.getDestinationEndpoint().getType() == EndpointType.OSS) {
            if (isBlank(pair.getDestS3Alias())) pair.setDestS3Alias(pair.getDestinationEndpoint().getS3Alias());
            if (isBlank(pair.getDestBucket())) pair.setDestBucket(pair.getDestinationEndpoint().getBucket());
        }
        TransferEndpoint local = pair.getSourceEndpoint().getType() == EndpointType.LOCAL
                ? pair.getSourceEndpoint() : (pair.getDestinationEndpoint().getType() == EndpointType.LOCAL
                ? pair.getDestinationEndpoint() : null);
        if (local != null) {
            if (isBlank(pair.getLocalPath())) pair.setLocalPath(local.getLocalPath());
            if (isBlank(pair.getExecutionNodeIp())) pair.setExecutionNodeIp(local.getExecutionNodeIp());
        }
    }

    public void validateDirection(TransferDirection direction, TransferEndpoint source, TransferEndpoint destination) {
        if (direction == null) {
            throw new TransferValidationException("INVALID_DIRECTION", "direction is required");
        }
        if (source == null || destination == null || source.getType() == null || destination.getType() == null) {
            throw new TransferValidationException("INVALID_ENDPOINT", "sourceEndpoint and destinationEndpoint types are required");
        }
        if (source.getType() != direction.getSourceType() || destination.getType() != direction.getDestinationType()) {
            throw new TransferValidationException("INVALID_DIRECTION", "endpoint types do not match direction " + direction);
        }
    }

    private void validateEndpoint(TransferEndpoint endpoint) {
        if (endpoint.getType() == EndpointType.LOCAL) {
            normalizeLocalEndpoint(endpoint, true);
        }
    }

    private void normalizeLocalEndpoint(TransferEndpoint endpoint, boolean strict) {
        if (endpoint == null || endpoint.getType() != EndpointType.LOCAL) return;
        List<LocalNodeBinding> bindings = endpoint.getNodeBindings();
        if (bindings == null || bindings.isEmpty()) {
            if (isBlank(endpoint.getExecutionNodeIp()) || isBlank(endpoint.getLocalPath())) {
                if (strict) throw new TransferValidationException("INVALID_ENDPOINT",
                        "LOCAL endpoint requires nodeBindings or executionNodeIp/localPath");
                return;
            }
            bindings = new ArrayList<>();
            bindings.add(new LocalNodeBinding(endpoint.getExecutionNodeIp(), endpoint.getLocalPath()));
            endpoint.setNodeBindings(bindings);
        }
        Set<String> nodes = new HashSet<>();
        for (LocalNodeBinding binding : bindings) {
            if (binding == null || isBlank(binding.getExecutionNodeIp()) || isBlank(binding.getLocalPath())) {
                throw new TransferValidationException("INVALID_ENDPOINT", "each LOCAL node binding requires executionNodeIp and localPath");
            }
            if (!nodes.add(binding.getExecutionNodeIp())) {
                throw new TransferValidationException("DUPLICATE_NODE_BINDING", "duplicate LOCAL node binding: " + binding.getExecutionNodeIp());
            }
        }
        if (!isBlank(endpoint.getExecutionNodeIp()) || !isBlank(endpoint.getLocalPath())) {
            boolean matches = false;
            for (LocalNodeBinding binding : bindings) {
                if (equals(endpoint.getExecutionNodeIp(), binding.getExecutionNodeIp())
                        && equals(endpoint.getLocalPath(), binding.getLocalPath())) {
                    matches = true;
                    break;
                }
            }
            if (!matches && strict) {
                throw new TransferValidationException("INVALID_ENDPOINT", "legacy LOCAL fields conflict with nodeBindings");
            }
        }
        LocalNodeBinding first = bindings.get(0);
        endpoint.setExecutionNodeIp(first.getExecutionNodeIp());
        endpoint.setLocalPath(first.getLocalPath());
    }

    public List<LocalNodeBinding> localBindings(LocalTransferRequest request) {
        normalizeAndValidate(request);
        return new ArrayList<>(localEndpoint(request.getSourceEndpoint(), request.getDestinationEndpoint()).getNodeBindings());
    }

    public LocalNodeBinding bindingFor(TransferEndpoint endpoint, String worker) {
        normalizeLocalEndpoint(endpoint, true);
        for (LocalNodeBinding binding : endpoint.getNodeBindings()) {
            if (binding.getExecutionNodeIp().equals(worker)) return binding;
        }
        throw new TransferValidationException("SHARD_ASSIGNMENT_MISMATCH", "worker is not eligible for LOCAL endpoint: " + worker);
    }

    private TransferEndpoint localEndpoint(TransferEndpoint source, TransferEndpoint destination) {
        if (source != null && source.getType() == EndpointType.LOCAL) return source;
        if (destination != null && destination.getType() == EndpointType.LOCAL) return destination;
        return null;
    }

    private List<String> nodeIps(TransferEndpoint endpoint) {
        List<String> result = new ArrayList<>();
        if (endpoint != null && endpoint.getNodeBindings() != null) {
            for (LocalNodeBinding binding : endpoint.getNodeBindings()) result.add(binding.getExecutionNodeIp());
        }
        return result;
    }

    private String normalizeMode(String mode, TransferEndpoint local) {
        if (isBlank(mode) && local != null && local.getNodeBindings() != null
                && local.getNodeBindings().size() > 1) {
            return DistributionMode.BALANCED.getValue();
        }
        String normalized;
        try {
            normalized = DistributionMode.fromValue(mode).getValue();
        } catch (IllegalArgumentException e) {
            throw new TransferValidationException("INVALID_DISTRIBUTION_MODE", e.getMessage());
        }
        return normalized;
    }

    private boolean equals(String left, String right) {
        return left == null ? right == null : left.equals(right);
    }

    public TransferEndpoint oss(String alias, String bucket, String objectKey, String prefix) {
        return new TransferEndpoint(EndpointType.OSS, alias, bucket, objectKey, prefix, null, null);
    }

    public TransferEndpoint local(String node, String path) {
        return new TransferEndpoint(EndpointType.LOCAL, null, null, null, null, node, path);
    }

    private boolean isBlank(String value) {
        return value == null || value.trim().isEmpty();
    }
}
