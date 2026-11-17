package master

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// proveSharedDirectory writes an unpredictable marker through one selected
// Agent, then reads its exact contents through every selected path. A successful
// stat of identically named paths on different machines is not such a proof.
func (s *Server) proveSharedDirectory(ctx context.Context, nodes []task.Node, writer int) (writeFailed bool, failedNode int, resultErr error) {
	displays := s.nodeDisplaySnapshot()
	nodeLabel := func(node task.Node) string { return displays.lookup(node.ID, node.Address).DisplayLabel }
	probe, err := transferstore.NewSharedProbe()
	if err != nil {
		return false, writer, err
	}
	call := func(ctx context.Context, node task.Node, action string) error {
		return s.agentCall(ctx, nodeSession(node), "preflight_probe", true, agentservice.PreflightProbeRequest{
			Root: node.LocalRoot, Action: action, Probe: probe,
		}, nil)
	}
	needsCleanup := true
	// A disconnected browser must not prevent cleanup. The strict marker name
	// and nonce mean this can never be used to remove a user-supplied filename.
	defer func() {
		if !needsCleanup {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := call(cleanupCtx, nodes[writer], "remove"); err != nil {
			writeFailed = false // Do not try another writer with unresolved cleanup.
			resultErr = errors.Join(resultErr, fmt.Errorf(localizeContext(ctx, "Probe cleanup failed on node %s: %s"), nodeLabel(nodes[writer]), err.Error()))
		}
	}()
	if err := call(ctx, nodes[writer], "write"); err != nil {
		if agentLacksOperation(err) {
			needsCleanup = false
			return false, writer, fmt.Errorf(localizeContext(ctx, "Upgrade Agent %s before verifying shared storage"), nodeLabel(nodes[writer]))
		}
		return true, writer, err
	}
	for i, node := range nodes {
		if err := call(ctx, node, "read"); err != nil {
			if agentLacksOperation(err) {
				return false, i, fmt.Errorf(localizeContext(ctx, "Upgrade Agent %s before verifying shared storage"), nodeLabel(node))
			}
			return false, i, fmt.Errorf(localizeContext(ctx, "Node %s could not read the shared test file: %s"), nodeLabel(node), err.Error())
		}
	}
	return false, -1, nil
}

// A successful path lookup is not a completed multi-node preflight. Preserve
// each failing node's reason and mark the other bindings as still unverified.
func invalidateUnverifiedSharedBindings(ctx context.Context, checks []bindingCheck) {
	if len(checks) < 2 {
		return
	}
	for i := range checks {
		if checks[i].Valid {
			checks[i].Valid = false
			checks[i].Code = "SHARED_STORAGE_NOT_VERIFIED"
			checks[i].Message = localizeContext(ctx, "Shared storage verification has not run because another node failed preflight")
		}
	}
}

func (s *Server) validateSharedBindings(ctx context.Context, spec *task.Spec, checks []bindingCheck) error {
	if len(spec.Nodes) < 2 {
		return nil
	}
	var pathErr error
	for i, check := range checks {
		if check.PathType != "DIRECTORY" {
			checks[i].Valid = false
			checks[i].Code = "SHARED_DIRECTORY_REQUIRED"
			checks[i].Message = localizeContext(ctx, "Create the shared directory on all selected nodes before preflight")
			if check.PathType == "MISSING" && spec.Direction == "OSS_TO_LOCAL" {
				checks[i].Message = localizeContext(ctx, "Shared destination directory does not exist")
			}
			if pathErr == nil {
				pathErr = errors.New(checks[i].Message)
			}
		}
	}
	if pathErr != nil {
		invalidateUnverifiedSharedBindings(ctx, checks)
		return pathErr
	}
	if spec.SharedCheckSkipped {
		// The operator acknowledged the risk: the directories exist, but
		// nothing proves that they are the same storage. Every Agent whose
		// check is skipped (in a node change, only the added ones) must still
		// be able to record the skipped check, or the task never starts there.
		if failed, err := s.sharedSkipUnsupported(ctx, *spec); err != nil {
			invalidateUnverifiedSharedBindings(ctx, checks)
			checks[failed].Code = "AGENT_UPGRADE_REQUIRED"
			checks[failed].Message = err.Error()
			return err
		}
		for i := range checks {
			checks[i].Code = "SHARED_STORAGE_CHECK_SKIPPED"
			checks[i].Message = localizeContext(ctx, "Shared directory check skipped as requested; the nodes were not verified to see the same storage")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	// An upload source that no node can write to is still a valid source.
	// Only then, and only when every write failed because the root cannot be
	// written (no permission, read-only or full), compare sampled files
	// instead; a download destination must always be writable.
	unwritable := spec.Direction == "LOCAL_TO_OSS"
	var otherWriteErr error
	otherWriter := -1
	for writer := range spec.Nodes {
		writeFailed, failedNode, err := s.proveSharedDirectory(ctx, spec.Nodes, writer)
		if err != nil {
			if writeFailed && !unwritableFailure(err) && otherWriteErr == nil {
				otherWriteErr, otherWriter = err, failedNode
			}
			unwritable = unwritable && writeFailed && unwritableFailure(err)
			if spec.Direction == "LOCAL_TO_OSS" && writeFailed && ctx.Err() == nil && writer+1 < len(spec.Nodes) {
				continue // A source may be mounted read-only on some readers.
			}
			if unwritable && ctx.Err() == nil {
				return s.validateSharedSourceByFingerprints(ctx, spec, checks)
			}
			message := err.Error()
			if writeFailed && spec.Direction == "LOCAL_TO_OSS" {
				// Show the failure that rules out comparing sampled files.
				if otherWriteErr != nil {
					message, failedNode = otherWriteErr.Error(), otherWriter
				}
				message = localizeContext(ctx, "No node could write the shared test file, and not every failure was a missing write permission, a read-only file system or full storage, so sampled files cannot be compared instead") + ": " + message
			}
			for i := range checks {
				checks[i].Valid = false
				checks[i].Code = "SHARED_STORAGE_PROBE_FAILED"
				if i == failedNode || failedNode < 0 {
					checks[i].Message = message
				} else {
					checks[i].Message = localizeContext(ctx, "Shared storage verification did not pass")
				}
			}
			return errors.New(message)
		}
		if spec.Direction == "LOCAL_TO_OSS" {
			// Runtime currently creates its proof from the first source node.
			// Prefer the verified writer while preserving every binding's path.
			if writer != 0 {
				spec.Nodes[0], spec.Nodes[writer] = spec.Nodes[writer], spec.Nodes[0]
			}
			break
		}
		// Every download node must be able to publish to the shared destination.
	}
	for i := range checks {
		checks[i].Code = "SHARED_STORAGE_VERIFIED"
		checks[i].Message = localizeContext(ctx, "Shared test file was written, read by every node, and removed")
	}
	return nil
}
