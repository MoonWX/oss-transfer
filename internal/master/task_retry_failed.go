package master

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// Retrying the failed objects of a running task reopens its ended children
// (FAILED or PARTIAL_FAILED) in the current generation, each on its own node,
// while the rest of the task keeps transferring. Only failed objects run
// again; succeeded and skipped objects keep their ledger credit. A stopped or
// finished task keeps the ordinary retry, which resumes a new generation.
//
// The operation holds the task's coordinator lock (no reconciliation step
// runs meanwhile). For each batch it records the intent durably
// (task.BeginRetry), asks the Agent to reopen the observed ended attempt
// (child_retry_failed) and observes the result. A stop recorded before the
// intent refuses it; a stop recorded later is fenced by the stop step, which
// stops the reopened children with the rest of the generation.

const (
	retryFailedChildLimit  = 64 // children one request reopens
	retryFailedBatch       = 16 // children per Agent call
	retryFailedSearchPages = 8  // bounded exception-index pages per request
)

// Outcomes reported for each child.
const (
	retryChildQueued   = "QUEUED"
	retryChildRetrying = "ALREADY_RETRYING"
	retryChildSkipped  = "SKIPPED"
	retryChildFailed   = "FAILED"
)

var (
	errRetryFailedNotRunning = errors.New("Only a running task can retry failed objects without stopping; use Retry after the task stops or finishes")
	errRetryFailedBusy       = errors.New("Wait until the task's node change or takeover finishes before retrying its failed objects")
	errRetryAgentUpgrade     = errors.New("retry protocol unsupported")
)

type retryFailedChild struct {
	ChildTaskID   string `json:"childTaskId"`
	Number        int    `json:"subtaskNumber"`
	Node          string `json:"executionNodeIp"`
	FailedObjects int64  `json:"failedObjectCount"`
	Outcome       string `json:"outcome"`
	Message       string `json:"message,omitempty"`
}

type retryFailedResult struct {
	TaskID         int64              `json:"taskId"`
	Status         string             `json:"status"`
	Generation     uint64             `json:"generation"`
	Accepted       int                `json:"accepted"`
	Ignored        int                `json:"ignored"`
	Failed         int                `json:"failed"`
	RetriedObjects int64              `json:"retriedObjects"`
	HasMore        bool               `json:"hasMore"`
	Message        string             `json:"message,omitempty"`
	Children       []retryFailedChild `json:"children"`
}

func (s *Server) retryFailedHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	var request struct {
		ExpectedGeneration *uint64 `json:"expectedGeneration,omitempty"`
	}
	if r.ContentLength != 0 && !readJSON(w, r, &request) {
		return
	}
	if request.ExpectedGeneration != nil && *request.ExpectedGeneration == 0 {
		fail(w, http.StatusBadRequest, "Expected generations must match the selected tasks")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result, err := s.retryRunningFailures(ctx, id, request.ExpectedGeneration)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		switch {
		case errors.Is(err, task.ErrNotFound):
			fail(w, http.StatusNotFound, "Task not found")
		case errors.Is(err, errRetryFailedNotRunning), errors.Is(err, errRetryFailedBusy):
			fail(w, http.StatusConflict, err.Error())
		case errors.Is(err, context.DeadlineExceeded):
			fail(w, http.StatusGatewayTimeout, "Retry confirmation timed out; refresh the task status")
		default:
			fail(w, http.StatusConflict, fmt.Sprintf(localize(w, "Cannot retry failed objects: %s"), err.Error()))
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "data": result})
}

// retryRunningFailures serializes with reconciliation steps, other control
// operations and repeated requests for the same task.
func (s *Server) retryRunningFailures(ctx context.Context, id int64, expected *uint64) (result retryFailedResult, err error) {
	err = s.tasks.exclusive(ctx, id, func() error {
		var retryErr error
		result, retryErr = s.retryFailedInPlace(ctx, id, expected)
		return retryErr
	})
	return
}

// retryTaskControl is the bulk "retry" action for a running task. handled is
// false when the task is not running, so the ordinary retry (resume) applies.
func (s *Server) retryTaskControl(ctx context.Context, id int64, expected *uint64) (p task.Parent, result retryFailedResult, handled, admitted bool, err error) {
	p, err = s.tasks.repo.Get(id)
	if err != nil || p.Deleted || p.Purged || p.Status != "RUNNING" {
		return p, result, false, false, nil
	}
	result, err = s.retryRunningFailures(ctx, id, expected)
	if errors.Is(err, errRetryFailedNotRunning) {
		// It ended meanwhile: the ordinary retry resumes its failed objects.
		return p, result, false, false, nil
	}
	if current, readErr := s.tasks.repo.Get(id); readErr == nil {
		p = current
	}
	if err == nil && result.Accepted == 0 && result.Failed > 0 {
		err = errors.New(result.Message)
	}
	return p, result, true, err == nil && result.Accepted > 0, err
}

func (s *Server) retryFailedInPlace(ctx context.Context, id int64, expected *uint64) (retryFailedResult, error) {
	t := s.tasks
	// Each page reads a bounded part of the exception index; resolved records
	// of earlier failures can precede the first candidate.
	var p task.Parent
	var candidates []task.Child
	cursor := ""
	for range retryFailedSearchPages {
		var found []task.Child
		var err error
		p, found, cursor, err = t.repo.RetryCandidates(id, cursor, retryFailedChildLimit-len(candidates))
		if err != nil {
			return retryFailedResult{}, err
		}
		candidates = append(candidates, found...)
		if cursor == "" || len(candidates) == retryFailedChildLimit || p.Status != "RUNNING" {
			break
		}
	}
	result := retryFailedResult{TaskID: id, Status: p.Status, Generation: p.Generation, HasMore: cursor != "", Children: []retryFailedChild{}}
	if p.Deleted || p.Purged {
		return result, task.ErrNotFound
	}
	if expected != nil && *expected != p.Generation {
		result.Message = localizeContext(ctx, "The task changed; refresh and try again")
		return result, nil
	}
	if p.Status != "RUNNING" || task.Historical(p) {
		return result, errRetryFailedNotRunning
	}
	if p.ResumeAfter != nil || p.FailoverPending() {
		return result, errRetryFailedBusy
	}
	if len(candidates) == 0 {
		result.HasMore = false
		result.Message = localizeContext(ctx, "No finished child task has failed objects to retry; failed objects of running child tasks can be retried after those child tasks finish")
		return result, nil
	}
	members, err := s.agents.registry.Members()
	if err != nil {
		return result, err
	}
	enrolled := map[string]string{}
	for _, member := range members {
		enrolled[member.ID] = member.EnrollmentID
	}
	snapshot, err := storageconfig.NewMaster(s.store).Snapshot()
	if err != nil {
		return result, err
	}
	displays := s.nodeDisplaySnapshot()
	var order []string
	groups := map[string][]task.Child{}
	for _, child := range candidates {
		if groups[child.Node.ID] == nil {
			order = append(order, child.Node.ID)
		}
		groups[child.Node.ID] = append(groups[child.Node.ID], child)
	}
	available := map[string]bool{}
	halted := ""
	for _, nodeID := range order {
		children := groups[nodeID]
		node := children[0].Node
		name := displays.lookup(node.ID, node.Address).DisplayLabel
		reject := func(outcome, message string, rest []task.Child) {
			for _, child := range rest {
				result.Children = append(result.Children, retryChild(child, outcome, message))
			}
		}
		if halted != "" {
			reject(retryChildFailed, halted, children)
			continue
		}
		if !p.RetryNodeActive(node.ID) {
			reject(retryChildSkipped, fmt.Sprintf(localizeContext(ctx, "Agent %s is leaving this task; its failed objects are retried on the node that takes over its work"), name), children)
			continue
		}
		if s.softwareUpdateBlocksNode(node.ID) {
			reject(retryChildFailed, fmt.Sprintf(localizeContext(ctx, "Agent %s is updating; retry its failed objects after the update"), name), children)
			continue
		}
		if enrolled[node.ID] != node.EnrollmentID {
			reject(retryChildFailed, fmt.Sprintf(localizeContext(ctx, "Agent %s has been removed or re-enrolled; the original task cannot resume on that identity"), name), children)
			continue
		}
		if err := t.prepareRetryNode(ctx, p, node, snapshot); err != nil {
			message := fmt.Sprintf(localizeContext(ctx, "Agent %s is unavailable: %s"), name, err.Error())
			if errors.Is(err, errRetryAgentUpgrade) {
				message = fmt.Sprintf(localizeContext(ctx, "Upgrade Agent %s to retry failed objects while the task is running"), name)
			}
			reject(retryChildFailed, message, children)
			continue
		}
		available[node.ID] = true
		for start := 0; start < len(children); start += retryFailedBatch {
			batch := children[start:min(start+retryFailedBatch, len(children))]
			if halted != "" {
				reject(retryChildFailed, halted, batch)
				continue
			}
			outcomes, stop := t.retryFailedBatch(ctx, p, batch, name)
			result.Children = append(result.Children, outcomes...)
			halted = stop
		}
	}
	var messages []string
	for _, child := range result.Children {
		switch child.Outcome {
		case retryChildQueued:
			result.Accepted++
			result.RetriedObjects += child.FailedObjects
			continue
		case retryChildFailed:
			result.Failed++
		default:
			result.Ignored++
		}
		if child.Message != "" && len(messages) < 3 && !containsString(messages, child.Message) {
			messages = append(messages, child.Message)
		}
	}
	result.Message = strings.Join(messages, "; ")
	if result.Accepted > 0 {
		if current, err := t.repo.Get(id); err == nil {
			result.Status = current.Status
			// A reopened child makes its node busy; raise a released share now
			// rather than one reconciliation step later.
			if t.releasedNowBusy(current) {
				t.reallocateWoken(ctx, current, available)
			}
		}
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
	return result, nil
}

func retryChild(child task.Child, outcome, message string) retryFailedChild {
	value := retryFailedChild{ChildTaskID: child.Job.ChildID, Number: child.Job.Number, Node: child.Node.Address, Outcome: outcome, Message: message}
	if child.Record != nil {
		value.FailedObjects = child.Record.Counters.Failed
	}
	return value
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

// prepareRetryNode confirms the Agent supports the operation and holds the
// task's current storage configuration, limit share and (for takeover-capable
// tasks) execution lease before any child is reopened on it.
func (t *taskRuntime) prepareRetryNode(ctx context.Context, p task.Parent, node task.Node, snapshot storageconfig.Snapshot) error {
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var caps struct {
		RetryFailedProtocol int `json:"retryFailedProtocol"`
	}
	if err := t.server.agentCall(callCtx, nodeSession(node), "capabilities", false, nil, &caps); err != nil {
		return err
	}
	if caps.RetryFailedProtocol < agentservice.RetryFailedProtocol {
		return errRetryAgentUpgrade
	}
	if err := t.server.syncStorage(callCtx, cluster.Member{ID: node.ID, EnrollmentID: node.EnrollmentID}, snapshot); err != nil {
		return err
	}
	for index, member := range p.ActiveNodes() {
		if member.ID != node.ID {
			continue
		}
		err := t.deliverPolicy(callCtx, p, index, member)
		if staleLimit(err) {
			err = t.redeliverStale(callCtx, p, member, err)
		}
		if err != nil {
			return err
		}
	}
	if p.Failover != nil && p.Failover.Enabled {
		return t.leases.renew(ctx, p, node)
	}
	return nil
}

// retryFailedBatch reopens up to 16 children of one node. It returns a
// non-empty halt message when the task stopped running, which ends the
// operation for the remaining children.
func (t *taskRuntime) retryFailedBatch(ctx context.Context, p task.Parent, batch []task.Child, name string) ([]retryFailedChild, string) {
	notRunning := localizeContext(ctx, "The task is no longer running; the retry was not started")
	current, err := t.repo.Get(p.ID)
	if err != nil {
		return retryOutcomes(batch, retryChildFailed, err.Error()), err.Error()
	}
	if current.Status != "RUNNING" || current.Generation != p.Generation {
		return retryOutcomes(batch, retryChildFailed, notRunning), notRunning
	}
	marked, err := t.repo.BeginRetry(p.ID, p.Generation, batch)
	if errors.Is(err, task.ErrState) || errors.Is(err, task.ErrGeneration) {
		return retryOutcomes(batch, retryChildFailed, notRunning), notRunning
	}
	if err != nil {
		return retryOutcomes(batch, retryChildFailed, err.Error()), err.Error()
	}
	outcomes := make([]retryFailedChild, 0, len(batch))
	selected := map[int]bool{}
	for _, child := range marked {
		selected[child.Job.Number] = true
	}
	for _, child := range batch {
		if !selected[child.Job.Number] {
			outcomes = append(outcomes, retryChild(child, retryChildRetrying, localizeContext(ctx, "This child task is already retrying or has no failed objects left")))
		}
	}
	if len(marked) == 0 {
		return outcomes, ""
	}
	node := marked[0].Node
	request := agentservice.ChildRetryFailedRequest{ParentID: p.ID, Generation: p.Generation}
	// Only an Agent that advertises source refresh is told that this Master
	// accepts refreshed source pins; an older Agent would reject the field.
	if support, err := t.newRangedPass().supported(ctx, node); err == nil && support.refresh {
		request.SourceRefresh = true
	}
	for _, child := range marked {
		request.Children = append(request.Children, agentservice.ChildRetryTarget{ChildID: child.Job.ChildID, Attempt: child.Record.Attempt})
	}
	var reply agentservice.ChildRetryFailedResult
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	callErr := t.server.agentCall(callCtx, nodeSession(node), "child_retry_failed", true, request, &reply)
	cancel()
	// Observe whatever the reply said: a lost reply can hide a reopened child,
	// and an unchanged ended record restores the ledger now rather than at the
	// next poll. A failed read leaves both to the next reconciliation step.
	observed, _ := t.observeNode(ctx, p, marked, false)
	if len(observed) > 0 {
		_ = t.repo.ObserveBatch(p.ID, p.Generation, observed)
	}
	records := map[int]execution.Record{}
	for _, observation := range observed {
		records[observation.Number] = observation.Record
	}
	replies := map[string]agentservice.ChildRetryOutcome{}
	for _, value := range reply.Children {
		replies[value.ChildID] = value
	}
	var agentErr *AgentError
	upgrade := errors.As(callErr, &agentErr) && (agentErr.Message == "unknown_operation" || agentErr.Message == "unsupported_operation")
	retrying := localizeContext(ctx, "This child task is already retrying or has no failed objects left")
	for _, child := range marked {
		record, seen := records[child.Job.Number]
		value, replied := replies[child.Job.ChildID]
		// Within a generation only this operation advances an ended attempt.
		advanced := seen && record.Attempt > child.Record.Attempt
		switch {
		case replied && value.Outcome == agentservice.ChildRetryQueued, !replied && advanced:
			outcomes = append(outcomes, retryChild(child, retryChildQueued, ""))
		case replied && value.Outcome == agentservice.ChildRetryUnchanged && (advanced || seen && !retryEnded(record.Status)):
			outcomes = append(outcomes, retryChild(child, retryChildRetrying, retrying))
		case replied && value.Outcome == agentservice.ChildRetryCapacity:
			outcomes = append(outcomes, retryChild(child, retryChildFailed, fmt.Sprintf(localizeContext(ctx, "Agent %s has no free job capacity; retry later"), name)))
		case replied && value.Outcome == agentservice.ChildRetryRejected:
			outcomes = append(outcomes, retryChild(child, retryChildFailed, fmt.Sprintf(localizeContext(ctx, "Agent %s refused the retry: %s"), name, value.Error)))
		case upgrade:
			outcomes = append(outcomes, retryChild(child, retryChildFailed, fmt.Sprintf(localizeContext(ctx, "Upgrade Agent %s to retry failed objects while the task is running"), name)))
		case callErr != nil:
			outcomes = append(outcomes, retryChild(child, retryChildFailed, fmt.Sprintf(localizeContext(ctx, "Agent %s did not confirm the retry: %s"), name, callErr.Error())))
		default:
			outcomes = append(outcomes, retryChild(child, retryChildFailed, localizeContext(ctx, "The child task changed; refresh and try again")))
		}
	}
	return outcomes, ""
}

func retryOutcomes(children []task.Child, outcome, message string) []retryFailedChild {
	values := make([]retryFailedChild, 0, len(children))
	for _, child := range children {
		values = append(values, retryChild(child, outcome, message))
	}
	return values
}

func retryEnded(status execution.Status) bool {
	return status == execution.Completed || status == execution.Failed || status == execution.PartialFailed
}
