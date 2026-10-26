package task

import (
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

func executionActive(status string) bool { return status == "RUNNING" || status == "STOPPING" }

func (p Parent) ExecutionTime() execution.ActiveTime {
	return execution.LegacyActiveTime(p.ActiveTime, p.StartedAt, p.FinishedAt, time.Time{}, executionActive(p.Status))
}

// ResumeTiming reuses bounded resume preparation to recover the end of an old
// attempt whose parent end time was erased by an older release.
type ResumeTiming struct {
	LatestFinishedAt *time.Time `json:"latestFinishedAt,omitempty"`
}

func (p *Parent) captureResumeFinish(c Child) {
	if p.ResumeTiming == nil || c.Record == nil || c.Record.FinishedAt == nil || c.Record.Job.Generation >= p.Generation {
		return
	}
	end := *c.Record.FinishedAt
	if p.StartedAt == nil || end.Before(*p.StartedAt) {
		return
	}
	if p.ResumeTiming.LatestFinishedAt == nil || end.After(*p.ResumeTiming.LatestFinishedAt) {
		p.ResumeTiming.LatestFinishedAt = &end
	}
}

func (p *Parent) finishResumeTiming() {
	if p.ResumeTiming == nil {
		return
	}
	p.ActiveTime = &execution.ActiveTime{Incomplete: true}
	if p.StartedAt != nil && p.ResumeTiming.LatestFinishedAt != nil {
		p.ActiveTime = &execution.ActiveTime{Estimated: true, ElapsedMillis: max(int64(0), p.ResumeTiming.LatestFinishedAt.Sub(*p.StartedAt).Milliseconds())}
	}
	p.ResumeTiming = nil
}
