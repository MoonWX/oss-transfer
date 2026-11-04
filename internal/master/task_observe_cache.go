package master

import (
	"time"

	"github.com/MoonWX/oss-transfer/internal/task"
)

// Observation cache (DB-2).
//
// Agents that advertise agentservice.ChildPollProtocol return a fingerprint of
// each stored record, and children_poll answers notModified when the Master
// sends the fingerprint of the record it already stored. The Master remembers,
// in memory only, the fingerprint of each record it fully processed: the
// record passed the Generation/Assignment checks in observeOne and the
// identity, job and ValidateResolvedSources checks in ObserveBatch, and that
// batch committed. An entry is used only while the Master's stored child still
// describes that same record (attempt, update time, generation, assignment,
// node), so any change on either side, a Master restart, or a resumed
// generation falls back to a full read.
//
// Children answering notModified are polled less often (observeBackoff), and
// when every outstanding child of a task is quiet the whole observe pass is
// skipped until the earliest child is due again.

const (
	observePollInterval = 250 * time.Millisecond
	observePollMax      = 2 * time.Second
)

// observeBackoff is the delay after quiet consecutive notModified answers.
func observeBackoff(quiet int) time.Duration {
	if quiet <= 0 {
		return 0
	}
	if quiet >= 4 {
		return observePollMax
	}
	return min(observePollMax, observePollInterval<<quiet)
}

type observeEntry struct {
	fingerprint      string
	nodeID           string
	enrollmentID     string
	parentGeneration uint64
	jobGeneration    uint64
	assignment       uint64
	attempt          uint64
	updatedAt        time.Time
	quiet            int
	nextPoll         time.Time
}

type taskObserveState struct {
	generation uint64
	entries    map[int]observeEntry
	// quietUntil skips the next observe passes: set only after a pass that
	// covered every outstanding child with a valid entry.
	quietUntil time.Time
}

func (e observeEntry) matches(p task.Parent, head task.OutstandingChild) bool {
	c := head.Child
	return e.fingerprint != "" && head.HasRecord && c.Accepted && !p.HandoffPending(c.Job.Number) &&
		e.nodeID == c.Node.ID && e.enrollmentID == c.Node.EnrollmentID &&
		e.parentGeneration == p.Generation && head.RecordGeneration == p.Generation &&
		e.jobGeneration == c.Job.Generation && e.assignment == c.Job.Assignment &&
		e.attempt == head.RecordAttempt && e.updatedAt.Equal(head.RecordUpdatedAt)
}

func (t *taskRuntime) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// observeStateLocked returns the task's cache, discarding it when the parent
// generation changed. t.mu must be held.
func (t *taskRuntime) observeStateLocked(p task.Parent) *taskObserveState {
	if t.observeCache == nil {
		t.observeCache = map[int64]*taskObserveState{}
	}
	state := t.observeCache[p.ID]
	if state == nil || state.generation != p.Generation {
		state = &taskObserveState{generation: p.Generation, entries: map[int]observeEntry{}}
		t.observeCache[p.ID] = state
	}
	return state
}

// observeQuiet reports whether the task's last pass found every outstanding
// child unchanged and none is due yet.
func (t *taskRuntime) observeQuiet(p task.Parent) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.observeCache[p.ID]
	return state != nil && state.generation == p.Generation && t.clock().Before(state.quietUntil)
}

// wakeObserve makes the next step observe the task, e.g. after new children
// were assigned.
func (t *taskRuntime) wakeObserve(id int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if state := t.observeCache[id]; state != nil {
		state.quietUntil = time.Time{}
	}
}

func (t *taskRuntime) forgetObserve(id int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.observeCache, id)
}

// observePlan selects the children due for observation in one outstanding
// page and the fingerprints to send for them. covered is (after, through];
// through is math.MaxInt when the page reached the end of the outstanding set.
func (t *taskRuntime) observePlan(p task.Parent, heads []task.OutstandingChild, after, through int) (due []task.Child, known map[int]string) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.observeStateLocked(p)
	present := make(map[int]bool, len(heads))
	for _, head := range heads {
		present[head.Child.Job.Number] = true
	}
	for number := range state.entries {
		if number > after && number <= through && !present[number] {
			delete(state.entries, number)
		}
	}
	known = map[int]string{}
	for _, head := range heads {
		number := head.Child.Job.Number
		entry, ok := state.entries[number]
		if ok && !entry.matches(p, head) {
			delete(state.entries, number)
			ok = false
		}
		if ok && now.Before(entry.nextPoll) {
			continue
		}
		due = append(due, head.Child)
		if ok {
			known[number] = entry.fingerprint
		}
	}
	return due, known
}

// observeRecorded updates the cache after a pass. Fingerprints of fully read
// records are recorded only when committed is true, i.e. ObserveBatch stored
// them. wholeSet means heads is the entire outstanding set.
func (t *taskRuntime) observeRecorded(p task.Parent, heads []task.OutstandingChild, result nodeObservation, committed, wholeSet bool) {
	now := t.clock()
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.observeStateLocked(p)
	byNumber := make(map[int]task.OutstandingChild, len(heads))
	for _, head := range heads {
		byNumber[head.Child.Job.Number] = head
	}
	for _, number := range result.notModified {
		if entry, ok := state.entries[number]; ok {
			entry.quiet++
			entry.nextPoll = now.Add(observeBackoff(entry.quiet))
			state.entries[number] = entry
		}
	}
	if committed {
		for _, observation := range result.observations {
			fingerprint := result.fingerprints[observation.Number]
			head, ok := byNumber[observation.Number]
			if fingerprint == "" || !ok {
				delete(state.entries, observation.Number)
				continue
			}
			record := observation.Record
			state.entries[observation.Number] = observeEntry{fingerprint: fingerprint, nodeID: head.Child.Node.ID, enrollmentID: head.Child.Node.EnrollmentID,
				parentGeneration: p.Generation, jobGeneration: record.Job.Generation, assignment: head.Child.Job.Assignment,
				attempt: record.Attempt, updatedAt: record.UpdatedAt, nextPoll: now}
		}
	} else {
		for _, observation := range result.observations {
			delete(state.entries, observation.Number)
		}
	}
	state.quietUntil = time.Time{}
	if !wholeSet || len(heads) == 0 {
		return
	}
	earliest := time.Time{}
	for _, head := range heads {
		entry, ok := state.entries[head.Child.Job.Number]
		if !ok || entry.quiet == 0 {
			return
		}
		if earliest.IsZero() || entry.nextPoll.Before(earliest) {
			earliest = entry.nextPoll
		}
	}
	state.quietUntil = earliest
}

func (t *taskRuntime) nodePolls(node task.Node) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.pollNodes[node.ID+"/"+node.EnrollmentID]
}

func (t *taskRuntime) setNodePolls(node task.Node, supported bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pollNodes == nil {
		t.pollNodes = map[string]bool{}
	}
	if supported {
		t.pollNodes[node.ID+"/"+node.EnrollmentID] = true
	} else {
		delete(t.pollNodes, node.ID+"/"+node.EnrollmentID)
	}
}
