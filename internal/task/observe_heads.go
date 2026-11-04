package task

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

// OutstandingChild is the part of an outstanding child the observe loop needs
// on every poll. Child carries the assignment identity (Job without Items),
// Node and Accepted; Child.Record is always nil. The Record* fields describe
// the last record the Master stored for this child.
type OutstandingChild struct {
	Child            Child
	HasRecord        bool
	RecordAttempt    uint64
	RecordUpdatedAt  time.Time
	RecordStatus     execution.Status
	RecordGeneration uint64
}

// childHead decodes only identity fields. encoding/json still scans the item
// list and results but allocates nothing for them, and the stored-job
// reference round trip done by Child.UnmarshalJSON is skipped.
type childHead struct {
	Job struct {
		ChildID                string `json:"childId"`
		ParentID               int64  `json:"parentId"`
		Number                 int    `json:"number"`
		Generation             uint64 `json:"generation"`
		Assignment             uint64 `json:"assignment"`
		OwnerEpoch             uint64 `json:"ownerEpoch"`
		SharedLocalSource      bool   `json:"sharedLocalSource"`
		SharedLocalDestination bool   `json:"sharedLocalDestination"`
		DiscoveryComplete      bool   `json:"discoveryComplete"`
	} `json:"job"`
	Node     Node `json:"node"`
	Accepted bool `json:"accepted"`
	Record   *struct {
		Attempt   uint64           `json:"attempt"`
		UpdatedAt time.Time        `json:"updatedAt"`
		Status    execution.Status `json:"status"`
		Job       *struct {
			Generation uint64 `json:"generation"`
		} `json:"job"`
	} `json:"record"`
}

// OutstandingHeads pages the same rows as Outstanding without decoding item
// lists or results.
func (r *Repository) OutstandingHeads(id int64, after int, limit int) (result []OutstandingChild, err error) {
	if limit < 1 || limit > 256 {
		return nil, errors.New("page size must be between 1 and 256")
	}
	err = r.store.View(func(tx *state.Tx) error {
		return tx.Scan("master-task-outstanding", key(id)+"/", childKey(id, after), limit, func(k string, _ json.RawMessage) error {
			var head childHead
			found, err := tx.Get("master-task-children", k, &head)
			if err != nil {
				return err
			}
			if !found {
				return ErrNotFound
			}
			job := head.Job
			out := OutstandingChild{Child: Child{Node: head.Node, Accepted: head.Accepted, Job: execution.Job{ChildID: job.ChildID, ParentID: job.ParentID, Number: job.Number, Generation: job.Generation, Assignment: job.Assignment, OwnerEpoch: job.OwnerEpoch, SharedLocalSource: job.SharedLocalSource, SharedLocalDestination: job.SharedLocalDestination, DiscoveryComplete: job.DiscoveryComplete}}}
			if head.Record != nil {
				out.HasRecord, out.RecordAttempt, out.RecordUpdatedAt, out.RecordStatus = true, head.Record.Attempt, head.Record.UpdatedAt, head.Record.Status
				// A stored job reference means the record's job equals Child.Job.
				out.RecordGeneration = job.Generation
				if head.Record.Job != nil {
					out.RecordGeneration = head.Record.Job.Generation
				}
			}
			result = append(result, out)
			return nil
		})
	})
	return
}

// Child reads one full child row.
func (r *Repository) Child(id int64, number int) (c Child, err error) {
	err = r.store.View(func(tx *state.Tx) error {
		found, err := tx.Get("master-task-children", childKey(id, number), &c)
		if err == nil && !found {
			err = ErrNotFound
		}
		return err
	})
	return
}
