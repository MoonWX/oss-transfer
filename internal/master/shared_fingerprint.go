package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// sharedFingerprintBudget bounds sampling and comparing files in one precheck
// or one task step. A task step has 20 seconds in all, so a precheck passes
// only what a step can repeat, and a slow mount fails with an explanation
// instead of a step that silently runs out of time.
const sharedFingerprintBudget = 15 * time.Second

func agentLacksOperation(err error) bool {
	var agentErr *AgentError
	return errors.As(err, &agentErr) && (agentErr.Message == "unknown_operation" || agentErr.Message == "unsupported_operation")
}

// unwritableFailure reports whether a shared probe write failed only because
// the Agent cannot write the root: no write permission, a read-only file
// system, or no space left. Agents that predate the storage_not_writable code
// send only the operating system's text; recognizing it lets the fingerprint
// step ask for their upgrade instead of reporting a write failure the user
// cannot fix.
func unwritableFailure(err error) bool {
	var agentErr *AgentError
	if !errors.As(err, &agentErr) {
		return false
	}
	if agentErr.Code == agentservice.UnwritableStorageCode {
		return true
	}
	message := strings.ToLower(agentErr.Message)
	for _, text := range []string{"read-only file system", "permission denied", "operation not permitted", "access is denied", "write-protected", "write protected", "no space left on device", "disk quota exceeded", "not enough space on the disk", "the disk is full"} {
		if strings.Contains(message, text) {
			return true
		}
	}
	return false
}

func (s *Server) nodeLabel(node task.Node) string {
	return s.nodeDisplaySnapshot().lookup(node.ID, node.Address).DisplayLabel
}

func (s *Server) nodeLabels(nodes []task.Node) string {
	labels := make([]string, len(nodes))
	for i, node := range nodes {
		labels[i] = s.nodeLabel(node)
	}
	return strings.Join(labels, ", ")
}

// fingerprintPortable reports whether a sample must use names that a Windows
// Agent can open: one takes part, or an Agent does not say which operating
// system it runs.
func (s *Server) fingerprintPortable(ctx context.Context, nodes []task.Node) bool {
	for _, node := range nodes {
		caps, err := s.tasks.capabilities(ctx, node)
		if err != nil || caps.OS == "" || caps.OS == "windows" {
			return true
		}
	}
	return false
}

func (s *Server) sampleSharedFingerprints(ctx context.Context, node task.Node, portable bool) ([]transferstore.SharedFingerprint, error) {
	var result agentservice.SharedFingerprintSampleResponse
	err := s.agentCall(ctx, nodeSession(node), "shared_fingerprint_sample", false, agentservice.SharedFingerprintSampleRequest{Root: node.LocalRoot, Limit: transferstore.SharedFingerprintMaxFiles, Portable: portable}, &result)
	if agentLacksOperation(err) {
		return nil, fmt.Errorf(localizeContext(ctx, "Upgrade Agent %s to compare files in a shared directory that no node can write"), s.nodeLabel(node))
	}
	if err == nil {
		// The sample is forwarded to other Agents; it must pass their limits.
		err = transferstore.ValidateSharedFingerprints(result.Fingerprints)
	}
	if err != nil {
		return nil, fmt.Errorf(localizeContext(ctx, "Node %s could not sample files in the shared directory: %s"), s.nodeLabel(node), err.Error())
	}
	if result.Fingerprints == nil {
		result.Fingerprints = []transferstore.SharedFingerprint{}
	}
	return result.Fingerprints, nil
}

// fingerprintCheck is one node's comparison with a sample.
type fingerprintCheck struct {
	node task.Node
	// differences lists the sampled paths this node sees differently.
	differences []transferstore.SharedFingerprintDifference
	// err means the comparison could not be completed on this node.
	err error
}

func (c fingerprintCheck) matched() bool { return c.err == nil && len(c.differences) == 0 }

// compareSharedFingerprints has every node recompute the sample, a few nodes
// at a time. With a parent and generation each Agent that matches also
// records its proof for that task generation; one that does not loses it.
func (s *Server) compareSharedFingerprints(ctx context.Context, nodes []task.Node, parentID int64, generation uint64, sample []transferstore.SharedFingerprint) []fingerprintCheck {
	checks := make([]fingerprintCheck, len(nodes))
	slots := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for i, node := range nodes {
		checks[i].node = node
		wait.Add(1)
		go func() {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			checks[i].differences, checks[i].err = s.verifySharedFingerprints(ctx, node, parentID, generation, sample)
		}()
	}
	wait.Wait()
	return checks
}

func (s *Server) verifySharedFingerprints(ctx context.Context, node task.Node, parentID int64, generation uint64, sample []transferstore.SharedFingerprint) ([]transferstore.SharedFingerprintDifference, error) {
	request := agentservice.SharedFingerprintVerifyRequest{ParentID: parentID, Generation: generation, Root: node.LocalRoot, Fingerprints: sample}
	if request.Fingerprints == nil {
		request.Fingerprints = []transferstore.SharedFingerprint{}
	}
	err := s.agentCall(ctx, nodeSession(node), "shared_fingerprint_verify", true, request, nil)
	if err == nil {
		return nil, nil
	}
	if agentLacksOperation(err) {
		return nil, fmt.Errorf(localizeContext(ctx, "Upgrade Agent %s to compare files in a shared directory that no node can write"), s.nodeLabel(node))
	}
	var agentErr *AgentError
	if errors.As(err, &agentErr) && agentErr.Code == agentservice.SharedFingerprintMismatchCode {
		var mismatch agentservice.SharedFingerprintMismatch
		if json.Unmarshal(agentErr.Payload, &mismatch) == nil && len(mismatch.Differences) > 0 {
			return mismatch.Differences, nil
		}
	}
	return nil, fmt.Errorf(localizeContext(ctx, "Could not complete the comparison on node %s: %s"), s.nodeLabel(node), err.Error())
}

// fingerprintFailure explains why a node did not match a sample read on
// sampler, naming the file and both nodes so the user knows where to look.
func (s *Server) fingerprintFailure(ctx context.Context, sampler task.Node, check fingerprintCheck) error {
	if check.err != nil {
		return check.err
	}
	difference := check.differences[0]
	node, reference := s.nodeLabel(check.node), s.nodeLabel(sampler)
	if check.node.ID == sampler.ID && difference.Status != transferstore.SharedFingerprintUnreadable {
		return fmt.Errorf(localizeContext(ctx, "The shared directory on node %s changed during the check (%q); run the check again"), node, difference.Path)
	}
	switch difference.Status {
	case transferstore.SharedFingerprintUnexpected:
		return fmt.Errorf(localizeContext(ctx, "Node %s found no file in the shared directory, but node %s found %q; an empty directory often means the share is not mounted on node %s"), reference, node, difference.Path, reference)
	case transferstore.SharedFingerprintMissing:
		return fmt.Errorf(localizeContext(ctx, "Sampled file %q, read on node %s, is missing on node %s"), difference.Path, reference, node)
	case transferstore.SharedFingerprintChanged:
		return fmt.Errorf(localizeContext(ctx, "Nodes %s and %s read different data for sampled file %q (its size or its first or last 64 KiB differ)"), reference, node, difference.Path)
	}
	return fmt.Errorf(localizeContext(ctx, "Node %s could not read sampled file %q: %s"), node, difference.Path, difference.Message)
}

// proveSharedFingerprints samples files through nodes[0] and has every node,
// the sampler included, compare them within sharedFingerprintBudget. With a
// parent and generation each Agent also records its proof for that task
// generation. On failure, involved lists the indexes of the nodes that the
// error names.
func (s *Server) proveSharedFingerprints(ctx context.Context, nodes []task.Node, parentID int64, generation uint64) (sample []transferstore.SharedFingerprint, involved []int, err error) {
	ctx, cancel := context.WithTimeout(ctx, sharedFingerprintBudget)
	defer cancel()
	sample, err = s.sampleSharedFingerprints(ctx, nodes[0], s.fingerprintPortable(ctx, nodes))
	if err != nil {
		return nil, []int{0}, fingerprintTimeout(ctx, err)
	}
	for i, check := range s.compareSharedFingerprints(ctx, nodes, parentID, generation, sample) {
		if check.matched() {
			continue
		}
		involved = []int{i}
		if check.err == nil && i != 0 {
			involved = []int{0, i}
		}
		return nil, involved, fingerprintTimeout(ctx, s.fingerprintFailure(ctx, nodes[0], check))
	}
	return sample, nil, nil
}

// fingerprintTimeout says that the comparison ran out of time, which a slow
// mount causes, rather than leaving only "context deadline exceeded".
func fingerprintTimeout(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf(localizeContext(ctx, "Sampling and comparing files in the shared directory did not finish within %d seconds: %s"), int(sharedFingerprintBudget/time.Second), err.Error())
	}
	return err
}

// validateSharedSourceByFingerprints is the preflight for an upload source
// that no node could write the test file to.
func (s *Server) validateSharedSourceByFingerprints(ctx context.Context, spec *task.Spec, checks []bindingCheck) error {
	sample, involved, err := s.proveSharedFingerprints(ctx, spec.Nodes, 0, 0)
	if err != nil {
		for i := range checks {
			checks[i].Valid = false
			checks[i].Code = "SHARED_STORAGE_FINGERPRINT_FAILED"
			checks[i].Message = localizeContext(ctx, "Shared storage verification did not pass")
		}
		for _, i := range involved {
			checks[i].Message = err.Error()
		}
		return err
	}
	message := fmt.Sprintf(localizeContext(ctx, "No node could write the test file (no write permission, read-only or full storage), so sampled files were compared instead: %d files have the same size and the same first and last 64 KiB on every node. This is weaker than the test file: separate copies holding identical files also pass."), len(sample))
	if len(sample) == 0 {
		message = localizeContext(ctx, "No node could write the test file (no write permission, read-only or full storage), and a complete listing of the shared directory found no file on any node, so there was nothing to compare.")
	}
	for i := range checks {
		checks[i].Code = "SHARED_STORAGE_FINGERPRINT_VERIFIED"
		checks[i].Message = message
	}
	return nil
}

// sharedSkipUnsupported finds the first node whose check spec skips and whose
// Agent cannot record a skipped shared check: a task whose check was skipped
// could never start there. It returns the node's index in spec.Nodes.
func (s *Server) sharedSkipUnsupported(ctx context.Context, spec task.Spec) (int, error) {
	for i, node := range spec.Nodes {
		if spec.SharedCheckSkippedNodesOnly && !node.SharedCheckSkipped {
			continue
		}
		caps, err := s.tasks.capabilities(ctx, node)
		if err != nil {
			return i, capabilitiesUnreadable(ctx, s.nodeLabel(node), err)
		}
		if !sharedSkipCapable(caps) {
			return i, fmt.Errorf(localizeContext(ctx, sharedSkipUpgradeMessage), s.nodeLabel(node))
		}
	}
	return -1, nil
}

// sharedSkipUpgradeMessage names an Agent that cannot run a task whose shared
// directory check was skipped.
const sharedSkipUpgradeMessage = "Upgrade Agent %s to run a task whose shared directory check was skipped"

// skippedSharedNodes splits nodes into those whose shared directory check the
// operator skipped (Parent.SharedCheckSkippedFor), which get their proof
// without a check, and the others, which are verified.
func skippedSharedNodes(p task.Parent, nodes []task.Node) (skipped, verified []task.Node) {
	for _, node := range nodes {
		if p.SharedCheckSkippedFor(node.ID) {
			skipped = append(skipped, node)
		} else {
			verified = append(verified, node)
		}
	}
	return skipped, verified
}

// grantSkippedSharedCheck gives each Agent the shared-storage proof that its
// scheduler requires, without any check, for nodes whose check an operator
// explicitly skipped, at creation or in a node change. Each grant to an Agent
// process that held no proof is logged, so the log records every node that
// starts work unverified, once per Agent process rather than on every step.
func (s *Server) grantSkippedSharedCheck(ctx context.Context, p task.Parent, nodes []task.Node) error {
	for _, node := range nodes {
		var result agentservice.SharedProofSkipResponse
		err := s.agentCall(ctx, nodeSession(node), "shared_proof_skip", true, agentservice.SharedProofSkipRequest{ParentID: p.ID, Generation: p.Generation, Root: node.LocalRoot}, &result)
		if agentLacksOperation(err) {
			return fmt.Errorf(localizeContext(ctx, sharedSkipUpgradeMessage), s.nodeLabel(node))
		}
		if err != nil {
			return err
		}
		if result.Granted {
			log.Printf("task %d generation %d: Agent %s works in %s without a shared directory check, which the operator chose to skip; it was not verified to see the same storage as the other task nodes", p.ID, p.Generation, s.nodeLabel(node), node.LocalRoot)
		}
	}
	return nil
}
