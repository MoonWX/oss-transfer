package execution

import "time"

// ActiveTime stores closed execution intervals separately from the current one.
// Keeping the open interval's origin makes checkpoint/restart replay idempotent.
type ActiveTime struct {
	Estimated     bool       `json:"estimated,omitempty"`
	Incomplete    bool       `json:"incomplete,omitempty"`
	ElapsedMillis int64      `json:"elapsedMillis"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
}

// LegacyActiveTime preserves the only interval older records can prove. Past
// retry pauses cannot be reconstructed from a first start and a final end.
func LegacyActiveTime(saved *ActiveTime, started, finished *time.Time, checkpoint time.Time, active bool) ActiveTime {
	if saved != nil {
		return *saved
	}
	var result ActiveTime
	if started == nil {
		return result
	}
	result.Estimated = true
	if finished != nil {
		result.ElapsedMillis = max(int64(0), finished.Sub(*started).Milliseconds())
	} else if active {
		result.StartedAt = started
	} else if !checkpoint.IsZero() {
		result.ElapsedMillis = max(int64(0), checkpoint.Sub(*started).Milliseconds())
	}
	return result
}

func (a *ActiveTime) Transition(active bool, at time.Time) {
	if active {
		if a.StartedAt == nil {
			at = at.UTC()
			a.StartedAt = &at
		}
		return
	}
	if a.StartedAt != nil {
		a.ElapsedMillis += max(int64(0), at.Sub(*a.StartedAt).Milliseconds())
		a.StartedAt = nil
	}
}

func (a ActiveTime) Duration(at time.Time) time.Duration {
	elapsed := max(int64(0), a.ElapsedMillis)
	if a.StartedAt != nil {
		elapsed += max(int64(0), at.Sub(*a.StartedAt).Milliseconds())
	}
	return time.Duration(elapsed) * time.Millisecond
}

func (r Record) ExecutionTime() ActiveTime {
	return LegacyActiveTime(r.ActiveTime, r.StartedAt, r.FinishedAt, r.UpdatedAt, r.Status == Running)
}
