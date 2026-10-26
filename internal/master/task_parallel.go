package master

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/task"
)

// Preserve each node's child order, but let independent nodes communicate in
// parallel. This does not change durable assignments or Agent execution limits.
func groupChildrenByNode(children []task.Child) [][]task.Child {
	var groups [][]task.Child
	indices := make(map[string]int)
	for _, child := range children {
		index, exists := indices[child.Node.ID]
		if !exists {
			index = len(groups)
			indices[child.Node.ID] = index
			groups = append(groups, nil)
		}
		groups[index] = append(groups[index], child)
	}
	return groups
}

func parallelChildNodes(ctx context.Context, groups [][]task.Child, visit func(int, []task.Child) error) error {
	problems := make([]error, len(groups))
	jobs := make(chan int, len(groups))
	for index := range groups {
		jobs <- index
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(8, len(groups)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if err := ctx.Err(); err != nil {
					problems[index] = err
					continue
				}
				problems[index] = visit(index, groups[index])
			}
		}()
	}
	workers.Wait()
	return errors.Join(append(problems, ctx.Err())...)
}

func (t *taskRuntime) deliverChildren(ctx context.Context, p task.Parent, children []task.Child) error {
	return parallelChildNodes(ctx, groupChildrenByNode(children), func(_ int, group []task.Child) error {
		for _, child := range group {
			callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := t.server.agentCall(callCtx, nodeSession(child.Node), "child_accept", true, childAcceptance(p, child), nil)
			cancel()
			if err != nil {
				return err
			}
		}
		return nil
	})
}
