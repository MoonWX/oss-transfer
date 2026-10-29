package master

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

const cleanupTaskConcurrency = 2
const cleanupNodeConcurrency = 4

// Late lease/proof replies must not recreate metadata behind a committed
// cleanup cursor. Check in the same transaction as each auxiliary write.
func allowTaskMetadataWrite(tx *state.Tx, id int64) error {
	var p task.Parent
	found, err := tx.Get("master-tasks", fmt.Sprintf("%020d", id), &p)
	if err != nil {
		return err
	}
	if !found {
		return task.ErrNotFound
	}
	if p.Purged || p.Cleanup != nil {
		return task.ErrState
	}
	return nil
}

type cleanupRetry struct {
	next     time.Time
	attempts int
}

type cleanupNodeKey struct {
	parent int64
	node   string
}

// The queue and acknowledgements live in the repository. These maps only limit
// concurrent work and polling; losing them on restart cannot lose cleanup work.
type taskCleanupRuntime struct {
	tasks       *taskRuntime
	wake        chan struct{}
	nodeSlots   chan struct{}
	mu          sync.Mutex
	active      map[int64]bool
	retry       map[int64]map[string]cleanupRetry
	nodeTimeout time.Duration
}

func newTaskCleanupRuntime(t *taskRuntime) *taskCleanupRuntime {
	return &taskCleanupRuntime{tasks: t, wake: make(chan struct{}, 1), nodeSlots: make(chan struct{}, cleanupNodeConcurrency), active: map[int64]bool{}, retry: map[int64]map[string]cleanupRetry{}, nodeTimeout: 5 * time.Second}
}

func (s *Server) requestTaskPurge(ctx context.Context, id int64) (p task.Parent, admitted bool, err error) {
	err = s.tasks.exclusive(ctx, id, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, admitted, err = s.tasks.repo.RequestPurge(id)
		return err
	})
	if err == nil {
		select {
		case s.tasks.cleanup.wake <- struct{}{}:
		default:
		}
	}
	return
}

func (c *taskCleanupRuntime) loop() {
	defer c.tasks.workers.Done()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var after int64
	for {
		select {
		case <-c.tasks.ctx.Done():
			return
		case <-ticker.C:
		case <-c.wake:
		}
		parents, err := c.tasks.repo.PendingPurges(after, 32)
		if err != nil {
			continue
		}
		if len(parents) == 0 {
			after = 0
			continue
		}
		for index, p := range parents {
			c.mu.Lock()
			if len(c.active) >= cleanupTaskConcurrency {
				c.mu.Unlock()
				break
			}
			// Resume after the last considered parent, not the fetched page.
			// Otherwise two waiting tasks at the front of a short page can
			// repeatedly take both slots and starve every later deletion.
			after = p.ID
			if index == len(parents)-1 && len(parents) < 32 {
				after = 0
			}
			if c.active[p.ID] || time.Now().Before(c.retry[p.ID][""].next) {
				c.mu.Unlock()
				continue
			}
			c.active[p.ID] = true
			c.mu.Unlock()
			c.tasks.workers.Add(1)
			go func(p task.Parent) {
				defer c.tasks.workers.Done()
				continueLocal := false
				defer func() {
					if continueLocal {
						// Yield between local write transactions without adding the
						// network polling interval to every metadata page.
						timer := time.NewTimer(10 * time.Millisecond)
						select {
						case <-timer.C:
						case <-c.tasks.ctx.Done():
						}
						timer.Stop()
					}
					c.mu.Lock()
					delete(c.active, p.ID)
					c.mu.Unlock()
					if continueLocal {
						select {
						case c.wake <- struct{}{}:
						default:
						}
					}
				}()
				ctx, cancel := context.WithTimeout(c.tasks.ctx, 40*time.Second)
				defer cancel()
				err := c.step(ctx, p.ID)
				continueLocal = err == nil && p.Cleanup.Phase == "MASTER"
				if err != nil && c.tasks.ctx.Err() == nil {
					c.schedule(cleanupNodeKey{parent: p.ID}, true)
					_, _ = c.tasks.repo.UpdatePurge(p.ID, func(progress *task.CleanupProgress) error {
						progress.Status = "WAITING"
						progress.Error = cleanupError(err)
						return nil
					})
				}
			}(p)
		}
	}
}

func cleanupError(err error) string {
	// Keep corrupt/remote diagnostics bounded before putting them in progress.
	message := strings.TrimSpace(err.Error())
	if len(message) > 1024 {
		message = message[:1024]
	}
	return message
}

func (c *taskCleanupRuntime) schedule(key cleanupNodeKey, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retry[key.parent] == nil {
		c.retry[key.parent] = map[string]cleanupRetry{}
	}
	r := c.retry[key.parent][key.node]
	if failed {
		r.attempts = min(r.attempts+1, 5)
		r.next = time.Now().Add(time.Second * time.Duration(1<<r.attempts))
	} else {
		r.attempts = 0
		r.next = time.Now().Add(time.Second)
	}
	c.retry[key.parent][key.node] = r
}

func (c *taskCleanupRuntime) forget(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.retry, id)
}

type agentCleanupProgress struct {
	Done           bool   `json:"done"`
	Phase          string `json:"phase"`
	RecordsDeleted int64  `json:"recordsDeleted"`
	Error          string `json:"error,omitempty"`
}

type cleanupNodeResult struct {
	node    task.Node
	skipped bool
	status  agentCleanupProgress
	err     error
}

func (c *taskCleanupRuntime) cleanNode(ctx context.Context, p task.Parent, node task.Node) cleanupNodeResult {
	result := cleanupNodeResult{node: node}
	select {
	case c.nodeSlots <- struct{}{}:
		defer func() { <-c.nodeSlots }()
	case <-ctx.Done():
		result.err = ctx.Err()
		return result
	}
	s := c.tasks.server
	result.skipped, result.err = s.agents.registry.IsRevoked(nodeSession(node))
	if result.err != nil || result.skipped {
		result.status.Done = result.skipped
		return result
	}
	request := agentservice.ParentRequest{ParentID: p.ID, Generation: p.Generation}
	call, cancel := context.WithTimeout(ctx, c.nodeTimeout)
	result.err = s.agentCall(call, nodeSession(node), "parent_cleanup", true, request, &result.status)
	cancel()
	if agentStatus(result.err, http.StatusNotFound) {
		// An older Agent still benefits from durable admission and automatic
		// retry. Only an explicit unsupported-operation response permits this
		// fallback; a lost reply must not change the cleanup protocol.
		result.err = s.agentCall(ctx, nodeSession(node), "parent_delete", true, request, nil)
		result.status.Done = result.err == nil
	}
	if result.err == nil && result.status.Error != "" {
		result.err = errors.New(result.status.Error)
	}
	if result.err == nil && result.status.RecordsDeleted < 0 {
		result.err = errors.New("invalid Agent cleanup progress")
	}
	return result
}

func (c *taskCleanupRuntime) step(ctx context.Context, id int64) error {
	p, err := c.tasks.repo.Get(id)
	if err != nil {
		return err
	}
	if p.Purged || p.Cleanup == nil {
		c.forget(id)
		return nil
	}
	if p.Cleanup.Phase == "MASTER" {
		_, done, err := c.tasks.repo.PurgePage(ctx, id)
		if done {
			c.forget(id)
		}
		return err
	}
	var nodes []task.Node
	c.mu.Lock()
	for _, node := range p.Spec.Nodes {
		if p.Cleanup.Nodes[node.ID].Done || time.Now().Before(c.retry[id][node.ID].next) {
			continue
		}
		nodes = append(nodes, node)
		if len(nodes) == cleanupNodeConcurrency {
			break
		}
	}
	c.mu.Unlock()
	if len(nodes) == 0 {
		if p.Cleanup.CompletedNodes == p.Cleanup.TotalNodes {
			_, err := c.tasks.repo.UpdatePurge(id, func(progress *task.CleanupProgress) error {
				progress.Phase, progress.Status, progress.Error = "MASTER", "RUNNING", ""
				return nil
			})
			return err
		}
		return nil
	}
	results := make(chan cleanupNodeResult, len(nodes))
	for _, node := range nodes {
		go func(node task.Node) { results <- c.cleanNode(ctx, p, node) }(node)
	}
	collected := make([]cleanupNodeResult, 0, len(nodes))
	for range nodes {
		result := <-results
		c.schedule(cleanupNodeKey{id, result.node.ID}, result.err != nil)
		collected = append(collected, result)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	_, err = c.tasks.repo.UpdatePurge(id, func(progress *task.CleanupProgress) error {
		for _, result := range collected {
			previous := progress.Nodes[result.node.ID]
			previous.RecordsDeleted = max(previous.RecordsDeleted, result.status.RecordsDeleted)
			previous.Done = result.err == nil && result.status.Done
			previous.Skipped = result.skipped
			previous.Error = ""
			if result.err != nil {
				previous.Error = cleanupError(result.err)
			}
			progress.Nodes[result.node.ID] = previous
		}
		progress.Status, progress.Error = "RUNNING", ""
		allDone := true
		for _, node := range p.Spec.Nodes {
			state := progress.Nodes[node.ID]
			allDone = allDone && state.Done
			if !state.Done && state.Error != "" && progress.Error == "" {
				progress.Status = "WAITING"
				progress.Error = state.Error
			}
		}
		if allDone {
			progress.Phase = "MASTER"
			progress.Status, progress.Error = "RUNNING", ""
		}
		return nil
	})
	return err
}

func cleanupDTO(progress *task.CleanupProgress) map[string]any {
	if progress == nil {
		return nil
	}
	row := map[string]any{"status": progress.Status, "phase": progress.Phase, "recordsDeleted": progress.RecordsDeleted,
		"completedNodes": progress.CompletedNodes, "totalNodes": progress.TotalNodes, "updatedAt": progress.UpdatedAt}
	if progress.Error != "" {
		row["error"] = progress.Error
	}
	return row
}

func (s *Server) cleanupStatusHTTP(w http.ResponseWriter, r *http.Request) {
	var input struct {
		TaskIDs []int64 `json:"taskIds"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	if len(input.TaskIDs) == 0 || len(input.TaskIDs) > 128 {
		fail(w, http.StatusBadRequest, "Select between 1 and 128 tasks per cleanup status page")
		return
	}
	seen := make(map[int64]bool, len(input.TaskIDs))
	rows := make([]map[string]any, 0, len(input.TaskIDs))
	for _, id := range input.TaskIDs {
		if id < 1 || seen[id] {
			fail(w, http.StatusBadRequest, "Invalid or duplicate task IDs")
			return
		}
		seen[id] = true
		p, err := s.tasks.repo.Get(id)
		if err != nil && !errors.Is(err, task.ErrNotFound) {
			fail(w, http.StatusInternalServerError, "Cannot read cleanup progress")
			return
		}
		row := map[string]any{"taskId": id, "purged": p.Purged}
		if errors.Is(err, task.ErrNotFound) || !p.Deleted {
			row["missing"] = true
		} else if p.Cleanup != nil {
			row["cleanup"] = cleanupDTO(p.Cleanup)
		}
		rows = append(rows, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "tasks": rows})
}
