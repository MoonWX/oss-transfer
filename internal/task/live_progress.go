package task

import (
	"errors"
	"math"
	"sort"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
)

const MaxProgressReports = 4096

// ProgressReport binds a volatile sample to the authenticated Agent session.
type ProgressReport struct {
	NodeID       string
	EnrollmentID string
	Child        execution.ChildProgress
}

type ProgressChild struct {
	Node     Node
	Progress execution.ChildProgress
	Bytes    int64
}

type ProgressProjection struct {
	Parent     Parent
	TotalBytes int64
	Children   []ProgressChild
}

// ProjectProgress adds only bytes that the durable counters do not yet credit.
// Parent and child counters share one read transaction, so an observation that
// commits during sampling cannot count a newly completed object twice. The
// projection is display-only: no volatile progress enters the recovery ledger.
func (r *Repository) ProjectProgress(parentID int64, generation uint64, reports []ProgressReport) (out ProgressProjection, err error) {
	if len(reports) > MaxProgressReports {
		return out, errors.New("too many live progress reports")
	}
	err = r.store.View(func(tx *state.Tx) error {
		p, err := get(tx, parentID)
		if err != nil {
			return err
		}
		if p.Generation != generation {
			return ErrGeneration
		}
		out.Parent, out.TotalBytes = p, p.Counters.SucceededBytes
		out.Children = []ProgressChild{}
		if p.Deleted || p.Purged || !executionActive(p.Status) {
			return nil
		}
		if out.TotalBytes < 0 {
			return ErrState
		}
		seen := make(map[int]bool, len(reports))
		for _, report := range reports {
			sample := report.Child
			if sample.Number < 1 || sample.ChildID == "" || sample.Generation != generation || sample.Attempt == 0 ||
				sample.SucceededBytes < 0 || sample.InFlightBytes < 0 || sample.ActiveObjects < 0 || sample.ActiveObjects > 128 ||
				sample.SucceededBytes > math.MaxInt64-sample.InFlightBytes || seen[sample.Number] {
				continue
			}
			var child Child
			found, err := tx.Get("master-task-children", childKey(parentID, sample.Number), &child)
			if err != nil {
				return err
			}
			if !found || child.Job.ParentID != parentID || child.Job.ChildID != sample.ChildID ||
				child.Job.Number != sample.Number || child.Job.Generation != generation || child.Job.Assignment != sample.Assignment ||
				child.Node.ID != report.NodeID || child.Node.EnrollmentID != report.EnrollmentID || report.NodeID == "" || report.EnrollmentID == "" {
				continue
			}
			var credited int64
			if record := child.Record; record != nil {
				credited = record.Counters.SucceededBytes
				if record.Job.Generation == generation {
					if record.Attempt > sample.Attempt || record.Attempt == sample.Attempt &&
						(record.Status != execution.Running && record.Status != execution.Waiting) {
						continue
					}
				}
			}
			// A sample taken before an object's result was committed can still
			// contain that object's live bytes. Discard it until the next sample.
			if credited < 0 || sample.SucceededBytes < credited {
				continue
			}
			bytes := sample.SucceededBytes + sample.InFlightBytes
			delta := bytes - credited
			if delta > math.MaxInt64-out.TotalBytes {
				continue
			}
			seen[sample.Number] = true
			out.TotalBytes += delta
			out.Children = append(out.Children, ProgressChild{Node: child.Node, Progress: sample, Bytes: bytes})
		}
		sort.Slice(out.Children, func(i, j int) bool { return out.Children[i].Progress.Number < out.Children[j].Progress.Number })
		return nil
	})
	return
}
