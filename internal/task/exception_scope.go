package task

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// UnfinishedNodes lists the nodes that own unfinished children of the active
// tasks a failure query covers. Only those nodes can still report failures
// that the exception index does not hold yet: the index is written with each
// observation, a stopping task drains every node before it ends, and a node
// that is idle or was removed owns no unfinished child.
func (r *Repository) UnfinishedNodes(ctx context.Context, filter ExceptionFilter) ([]Node, error) {
	found := map[string]Node{}
	visit := func(tx *state.Tx, p Parent) error {
		if p.Deleted || p.Purged || Ended(p.Status) {
			return nil
		}
		if err := ensureScheduling(tx, &p); err != nil {
			return err
		}
		nodes := make(map[string]Node, len(p.Spec.Nodes))
		for _, node := range p.Spec.Nodes {
			nodes[node.ID] = node
		}
		for id, load := range p.Scheduling.Nodes {
			node, known := nodes[id]
			if load.Children <= 0 || !known {
				continue
			}
			// The probe carries what an exception record of this node would.
			probe := Exception{ParentID: p.ID, Direction: p.Spec.Direction, Node: node.Address, Source: p.Spec.Source, Destination: p.Spec.Destination}
			if probe.Source.Kind == "LOCAL" {
				probe.Source.Root = node.LocalRoot
			}
			if probe.Destination.Kind == "LOCAL" {
				probe.Destination.Root = node.LocalRoot
			}
			if exceptionMatches(probe, filter) {
				found[node.ID+"\x00"+node.EnrollmentID] = node
			}
		}
		return nil
	}
	err := r.store.View(func(tx *state.Tx) error {
		if filter.ParentID > 0 {
			p, err := get(tx, filter.ParentID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			return visit(tx, p)
		}
		after := ""
		for {
			count := 0
			err := tx.Scan("master-task-active", "", after, 128, func(k string, raw json.RawMessage) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				after, count = k, count+1
				var id int64
				if err := json.Unmarshal(raw, &id); err != nil {
					return err
				}
				p, err := get(tx, id)
				if err != nil {
					return err
				}
				return visit(tx, p)
			})
			if err != nil || count < 128 {
				return err
			}
		}
	})
	if err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(found))
	for _, node := range found {
		nodes = append(nodes, node)
	}
	return nodes, nil
}
