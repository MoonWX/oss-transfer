package master

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
)

var (
	errChildParentInactive = errors.New("Resume the parent task before recovering a child")
	errChildNotRecoverable = errors.New("This child is no longer waiting for recovery")
	errChildAgentUpgrade   = errors.New("Upgrade this Agent to recover individual child tasks")
)

func (s *Server) recoverChildHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || number < 1 {
		fail(w, http.StatusBadRequest, "Invalid child number")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	var parent task.Parent
	var child task.Child
	var outcome string
	err = s.tasks.exclusive(ctx, id, func() error {
		var recoverErr error
		parent, child, outcome, recoverErr = s.recoverChild(ctx, id, number)
		return recoverErr
	})
	if err != nil {
		if r.Context().Err() != nil {
			// The Agent may already have admitted the attempt. A disconnected
			// caller can retry safely and observe ALREADY_RUNNING.
			return
		}
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			fail(w, http.StatusGatewayTimeout, "Recovery confirmation timed out; refresh the child status")
		case errors.Is(err, task.ErrNotFound):
			fail(w, http.StatusNotFound, "Child task not found")
		case errors.Is(err, errChildParentInactive), errors.Is(err, errChildNotRecoverable):
			fail(w, http.StatusConflict, err.Error())
		case errors.Is(err, errChildAgentUpgrade):
			fail(w, http.StatusConflict, errChildAgentUpgrade.Error())
		default:
			fail(w, http.StatusConflict, fmt.Sprintf(localizeContext(ctx, "Cannot recover child task: %s"), err.Error()))
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": map[string]any{"outcome": outcome, "child": s.childView(parent, child)}})
}

// recoverChild runs under the parent's coordinator lock. It never creates a
// child, changes its generation, or resumes a stopped parent as a side effect.
func (s *Server) recoverChild(ctx context.Context, id int64, number int) (parent task.Parent, child task.Child, outcome string, err error) {
	parent, err = s.tasks.repo.Get(id)
	if err != nil {
		return
	}
	if parent.Deleted || parent.Purged {
		err = task.ErrNotFound
		return
	}
	if parent.Status != "RUNNING" || task.Historical(parent) {
		err = errChildParentInactive
		return
	}
	children, readErr := s.tasks.repo.Children(id, number-1, 1)
	if readErr != nil {
		err = readErr
		return
	}
	if len(children) != 1 || children[0].Job.Number != number {
		err = task.ErrNotFound
		return
	}
	child = children[0]
	if err = s.checkTaskEnrollments(parent); err != nil {
		return
	}
	var record execution.Record
	err = s.agentCall(ctx, nodeSession(child.Node), "child_get", false, map[string]string{"childId": child.Job.ChildID}, &record)
	if err != nil {
		return
	}
	// Observe validates the complete immutable job, not only the displayed ID.
	if err = s.tasks.repo.Observe(id, parent.Generation, number, record); err != nil {
		return
	}
	child.Record = &record
	if record.Status == execution.Running {
		outcome = "ALREADY_RUNNING"
		return
	}
	if record.Status != execution.Recoverable {
		err = errChildNotRecoverable
		return
	}
	snapshot, readErr := storageconfig.NewMaster(s.store).Snapshot()
	if readErr != nil {
		err = readErr
		return
	}
	for _, node := range parent.Spec.Nodes {
		if err = s.syncStorage(ctx, cluster.Member{ID: node.ID, EnrollmentID: node.EnrollmentID}, snapshot); err != nil {
			return
		}
	}
	if err = s.tasks.policies(ctx, parent); err != nil {
		return
	}
	// A manual retry also rechecks a mount which may have changed without an
	// Agent restart; do not reuse this process's previous proof.
	s.tasks.mu.Lock()
	delete(s.tasks.sharedProofs, id)
	s.tasks.mu.Unlock()
	if err = s.tasks.ensureSharedSource(ctx, parent); err != nil {
		return
	}
	if parent.Spec.Destination.Kind == "LOCAL" && len(parent.Spec.Nodes) > 1 {
		for writer := range parent.Spec.Nodes {
			if _, _, err = s.proveSharedDirectory(ctx, parent.Spec.Nodes, writer); err != nil {
				return
			}
		}
	}
	current, readErr := s.tasks.repo.Get(id)
	if readErr != nil {
		err = readErr
		return
	}
	if current.Status != "RUNNING" || current.Deleted || current.Purged || current.Generation != parent.Generation {
		err = errChildParentInactive
		return
	}
	var result agentservice.ChildRecoverResult
	err = s.agentCall(ctx, nodeSession(child.Node), "child_recover", true, agentservice.ChildRecoverRequest{ParentID: id, ChildID: child.Job.ChildID, Generation: parent.Generation}, &result)
	if err != nil {
		var agentErr *AgentError
		if errors.As(err, &agentErr) {
			if agentErr.Message == "unknown_operation" || agentErr.Message == "unsupported_operation" {
				err = errChildAgentUpgrade
			} else if agentErr.Message == execution.ErrNotRecoverable.Error() {
				err = errChildNotRecoverable
			}
		}
		return
	}
	if result.Outcome != "STARTED" && result.Outcome != "ALREADY_RUNNING" && result.Outcome != "WAITING_CAPACITY" {
		err = errors.New("Agent returned an invalid recovery outcome")
		return
	}
	if err = s.tasks.repo.Observe(id, parent.Generation, number, result.Record); err != nil {
		return
	}
	child.Record = &result.Record
	outcome = result.Outcome
	return
}
