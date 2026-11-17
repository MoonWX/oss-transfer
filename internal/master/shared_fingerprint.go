package master

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func agentLacksOperation(err error) bool {
	var agentErr *AgentError
	return errors.As(err, &agentErr) && (agentErr.Message == "unknown_operation" || agentErr.Message == "unsupported_operation")
}

// readOnlyWriteFailure reports whether a shared probe write failed only because
// the root is read-only or not writable by the Agent's account. Agents that
// predate the read_only_storage code send only the operating system's text;
// recognizing it lets the fingerprint step ask for their upgrade instead of
// reporting a write failure the user cannot fix.
func readOnlyWriteFailure(err error) bool {
	var agentErr *AgentError
	if !errors.As(err, &agentErr) {
		return false
	}
	if agentErr.Code == agentservice.ReadOnlyStorageCode {
		return true
	}
	message := strings.ToLower(agentErr.Message)
	for _, text := range []string{"read-only file system", "permission denied", "operation not permitted", "access is denied", "write-protected"} {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

func (s *Server) nodeLabel(node task.Node) string {
	return s.nodeDisplaySnapshot().lookup(node.ID, node.Address).DisplayLabel
}

// proveSharedFingerprints samples files through the first node and has every
// node, the sampler included, recompute them. With a parent and generation each
// Agent also records its proof for that task generation. It returns the
// sample, so takeover can later compare against the original directory.
func (s *Server) proveSharedFingerprints(ctx context.Context, nodes []task.Node, parentID int64, generation uint64) (sample []transferstore.SharedFingerprint, failedNode int, err error) {
	sample, err = s.sampleSharedFingerprints(ctx, nodes[0])
	if err != nil {
		return nil, 0, err
	}
	for i, node := range nodes {
		if err := s.verifySharedFingerprints(ctx, node, parentID, generation, sample); err != nil {
			return nil, i, err
		}
	}
	return sample, -1, nil
}

func (s *Server) sampleSharedFingerprints(ctx context.Context, node task.Node) ([]transferstore.SharedFingerprint, error) {
	var result agentservice.SharedFingerprintSampleResponse
	err := s.agentCall(ctx, nodeSession(node), "shared_fingerprint_sample", false, agentservice.SharedFingerprintSampleRequest{Root: node.LocalRoot, Limit: transferstore.SharedFingerprintMaxFiles}, &result)
	if agentLacksOperation(err) {
		return nil, fmt.Errorf(localizeContext(ctx, "Upgrade Agent %s to verify a read-only shared directory"), s.nodeLabel(node))
	}
	if err == nil {
		// The sample is forwarded to other Agents; it must pass their limits.
		err = transferstore.ValidateSharedFingerprints(result.Fingerprints)
	}
	if err != nil {
		return nil, fmt.Errorf(localizeContext(ctx, "Node %s could not sample files in the shared directory: %s"), s.nodeLabel(node), err.Error())
	}
	return result.Fingerprints, nil
}

func (s *Server) verifySharedFingerprints(ctx context.Context, node task.Node, parentID int64, generation uint64, sample []transferstore.SharedFingerprint) error {
	request := agentservice.SharedFingerprintVerifyRequest{ParentID: parentID, Generation: generation, Root: node.LocalRoot, Fingerprints: sample}
	if request.Fingerprints == nil {
		request.Fingerprints = []transferstore.SharedFingerprint{}
	}
	err := s.agentCall(ctx, nodeSession(node), "shared_fingerprint_verify", true, request, nil)
	if agentLacksOperation(err) {
		return fmt.Errorf(localizeContext(ctx, "Upgrade Agent %s to verify a read-only shared directory"), s.nodeLabel(node))
	}
	if err != nil {
		return explainedError{fmt.Sprintf(localizeContext(ctx, "Node %s does not see the same files in the shared directory: %s"), s.nodeLabel(node), err.Error()), err}
	}
	return nil
}

// explainedError shows a localized explanation but keeps the Agent's error
// reachable through errors.As.
type explainedError struct {
	message string
	cause   error
}

func (e explainedError) Error() string { return e.message }
func (e explainedError) Unwrap() error { return e.cause }

// sharedContentChanged reports whether a stored sample failed only because a
// sampled file still exists with other content, which an edit also causes.
func sharedContentChanged(err error) bool {
	var agentErr *AgentError
	return errors.As(err, &agentErr) && agentErr.Code == agentservice.SharedContentChangedCode
}

// validateReadOnlySharedSource is the preflight for an upload source that no
// node could write a probe to.
func (s *Server) validateReadOnlySharedSource(ctx context.Context, spec *task.Spec, checks []bindingCheck) error {
	sample, failedNode, err := s.proveSharedFingerprints(ctx, spec.Nodes, 0, 0)
	if err != nil {
		for i := range checks {
			checks[i].Valid = false
			checks[i].Code = "SHARED_STORAGE_PROBE_FAILED"
			checks[i].Message = localizeContext(ctx, "Shared storage verification did not pass")
			if i == failedNode {
				checks[i].Message = err.Error()
			}
		}
		return err
	}
	message := fmt.Sprintf(localizeContext(ctx, "The shared directory is read-only; %d sampled files have the same content on every node"), len(sample))
	if len(sample) == 0 {
		message = localizeContext(ctx, "The shared directory is read-only and every node sees it without files")
	}
	for i := range checks {
		checks[i].Code = "SHARED_STORAGE_FINGERPRINT_VERIFIED"
		checks[i].Message = message
	}
	return nil
}
