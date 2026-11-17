package master

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// States of a task's live scan activity.
const (
	scanActivityLive        = "LIVE"        // the scanner's latest reply, younger than scanActivityMaxAge
	scanActivityStale       = "STALE"       // older, or the latest call failed; the values are kept
	scanActivityUnsupported = "UNSUPPORTED" // the scanner reports no scan protocol
)

// scanActivityMaxAge is how long a reply counts as live, the age of the task
// rates.
const scanActivityMaxAge = taskRateMaxAge

// scanSample is the latest scan_start reply of a task's step: its scanner's
// protocol and what the scanning attempt was doing as it replied.
type scanSample struct {
	generation uint64
	epoch      uint64
	attempt    string
	receivedAt time.Time
	protocol   int
	activity   *transferstore.ScanActivitySnapshot
	failed     bool // the latest call failed; the values are the last reply's
}

// recordScan keeps a step's scan_start reply beside the task rates. A failed
// call keeps the previous reply's values, marked as no longer current. A
// reply of another generation, epoch or attempt replaces the sample.
func (t *taskRuntime) recordScan(p task.Parent, reply agentservice.ScanReply, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.scans == nil {
		t.scans = map[int64]scanSample{}
	}
	if err != nil {
		if sample, ok := t.scans[p.ID]; ok {
			sample.failed = true
			t.scans[p.ID] = sample
		}
		return
	}
	t.scans[p.ID] = scanSample{generation: p.Generation, epoch: p.ScannerEpoch(), attempt: reply.Attempt,
		receivedAt: t.server.options.Now(), protocol: reply.ScanProtocol, activity: reply.Activity}
}

func (t *taskRuntime) scanSample(p task.Parent) (scanSample, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	sample, ok := t.scans[p.ID]
	return sample, ok && sample.generation == p.Generation && sample.epoch == p.ScannerEpoch()
}

func (t *taskRuntime) forgetScans(active map[int64]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id := range t.scans {
		if !active[id] {
			delete(t.scans, id)
		}
	}
}

// scanActivityView is what the scanner of a running task with an incomplete
// scan last reported, or nil when there is nothing to show: no reply yet, or
// a scanner whose source reports no activity. A scanner that predates the
// scan protocol is UNSUPPORTED for a scan that a current Agent reports on,
// never a scan that has done nothing; for any other scan an upgrade would
// show nothing more, so nothing is shown.
func (s *Server) scanActivityView(p task.Parent) map[string]any {
	if p.Status != "RUNNING" || p.ScanComplete {
		return nil
	}
	sample, ok := s.tasks.scanSample(p)
	if !ok {
		return nil
	}
	if sample.protocol < 1 {
		if !scanReportsActivity(p) {
			return nil
		}
		return map[string]any{"state": scanActivityUnsupported}
	}
	activity := sample.activity
	if activity == nil {
		return nil
	}
	age := s.options.Now().Sub(sample.receivedAt)
	state := scanActivityLive
	if sample.failed || age > scanActivityMaxAge {
		state = scanActivityStale
	}
	view := map[string]any{"state": state, "observedAt": sample.receivedAt.UnixMilli(), "ageMillis": max(0, age.Milliseconds()),
		"mode": activity.Mode, "namesRead": activity.NamesRead, "examined": activity.Examined,
		"alreadyFound": activity.AlreadyFound, "vanished": activity.Vanished, "idleMillis": activity.IdleMillis,
		"oldestCallMillis": activity.OldestCallMillis, "waitingFor": activity.WaitingFor, "lookupMeanMillis": nil, "directory": nil}
	if activity.LookupMeanMillis != nil {
		view["lookupMeanMillis"] = *activity.LookupMeanMillis
	}
	if d := activity.Directory; d != nil {
		view["directory"] = map[string]any{"key": d.Key, "reading": d.Reading, "names": d.Names, "position": d.Position, "examined": d.Examined, "elapsedMillis": d.ElapsedMillis}
	}
	return view
}

// scanReportsActivity reports whether a current Agent reports live progress
// for p's scan: that of a local directory, not of object storage, an
// inventory or a single key.
func scanReportsActivity(p task.Parent) bool {
	return p.Spec.Source.Kind == "LOCAL" && p.Spec.InventoryID == "" && p.Spec.ObjectKey == ""
}

// scanProtocol asks a node which scan protocol its Agent speaks; zero for an
// Agent that predates it.
func (t *taskRuntime) scanProtocol(ctx context.Context, node task.Node) (int, error) {
	if t.scanQuery != nil {
		return t.scanQuery(ctx, node)
	}
	var caps struct {
		ScanProtocol int `json:"scanProtocol"`
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := t.server.agentCall(callCtx, nodeSession(node), "capabilities", false, nil, &caps)
	return caps.ScanProtocol, err
}

// scanUpgradeWait records whether a fenced scanner's checkpoint waits for a
// member that speaks its scan protocol, for the node view.
func (t *taskRuntime) scanUpgradeWait(p task.Parent, waiting bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.upgradeWaits == nil {
		t.upgradeWaits = map[int64]uint64{}
	}
	if waiting {
		t.upgradeWaits[p.ID] = p.Generation
	} else {
		delete(t.upgradeWaits, p.ID)
	}
}

func (t *taskRuntime) waitsForScanUpgrade(p task.Parent) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	generation, ok := t.upgradeWaits[p.ID]
	return ok && generation == p.Generation && scanPhase(p) == scanPhaseWaiting
}

// scannerWait is what a running task waits for while its scanner is not
// available: an upgraded member, when none of the others can resume the scan
// checkpoint (the fenced scanner cannot rejoin while its scan is incomplete),
// or else the scanner's reconnect.
func (t *taskRuntime) scannerWait(p task.Parent) error {
	if t.waitsForScanUpgrade(p) {
		return errors.New("Waiting for an upgraded Agent to continue the scan; the remaining nodes are too old to resume it")
	}
	scanning := p.ScannerNode()
	return fmt.Errorf("Waiting for scanning node %s to reconnect; existing child tasks remain assigned to their original nodes", t.server.nodeDisplaySnapshot().lookup(scanning.ID, scanning.Address).DisplayLabel)
}

// requiredScanProtocol is the protocol an Agent must speak to take over from
// the Master's scan checkpoint.
func requiredScanProtocol(p task.Parent) int {
	if p.ScanCheckpoint == nil {
		return 0
	}
	return p.ScanCheckpoint.RequiresScanProtocol
}
