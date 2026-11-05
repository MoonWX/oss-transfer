package master

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// plannedStorage serves real S3 wire requests. Gates hold requests until the
// test opens them (or the request is cancelled), so every stage of a node
// change is observed without timing assumptions. Requests carry the Agent's
// label (see labelTakeoverAgent).
type plannedStorage struct {
	mu       sync.Mutex
	source   map[string][]byte
	target   map[string][]byte
	reads    map[string]int
	lists    []plannedList
	listGate map[string]chan struct{} // continuation pages, by Agent label
	getGate  chan struct{}            // source object reads, all Agents
}

type plannedList struct{ owner, token string }

func newPlannedStorage(objects int) *plannedStorage {
	s := &plannedStorage{source: map[string][]byte{}, target: map[string][]byte{}, reads: map[string]int{}, listGate: map[string]chan struct{}{}}
	for i := range objects {
		s.source[fmt.Sprintf("objects/%03d", i)] = bytes.Repeat([]byte(fmt.Sprintf("planned-%03d", i)), 8)
	}
	return s
}

func (s *plannedStorage) wait(r *http.Request, gate chan struct{}) bool {
	if gate == nil {
		return true
	}
	select {
	case <-gate:
		return true
	case <-r.Context().Done():
		return false
	}
}

func (s *plannedStorage) listed(owner, token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, list := range s.lists {
		if list.owner == owner && list.token == token {
			return true
		}
	}
	return false
}

func (s *plannedStorage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	owner := r.Header.Get("X-Test-Owner")
	if bucket == "source" && r.Method == http.MethodGet && key == "" {
		token := r.URL.Query().Get("continuation-token")
		start, _ := strconv.Atoi(token)
		s.mu.Lock()
		s.lists = append(s.lists, plannedList{owner, token})
		gate := s.listGate[owner]
		s.mu.Unlock()
		if start > 0 && !s.wait(r, gate) {
			return
		}
		type entry struct {
			Key, ETag    string
			Size         int
			LastModified string
		}
		result := struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			Name                  string
			IsTruncated           bool
			NextContinuationToken string  `xml:"NextContinuationToken,omitempty"`
			Contents              []entry `xml:"Contents"`
		}{Name: bucket}
		end := min(start+128, len(s.source))
		for number := start; number < end; number++ {
			name := fmt.Sprintf("objects/%03d", number)
			sum := md5.Sum(s.source[name])
			result.Contents = append(result.Contents, entry{Key: name, Size: len(s.source[name]), ETag: `"` + hex.EncodeToString(sum[:]) + `"`, LastModified: "2026-01-01T00:00:00Z"})
		}
		if end < len(s.source) {
			result.IsTruncated, result.NextContinuationToken = true, strconv.Itoa(end)
		}
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(result)
		return
	}
	if bucket == "target" && r.Method == http.MethodPut {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		s.mu.Lock()
		if _, exists := s.target[key]; exists && r.Header.Get("If-None-Match") == "*" {
			s.mu.Unlock()
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		s.target[key] = payload
		s.mu.Unlock()
		sum := md5.Sum(payload)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.WriteHeader(http.StatusOK)
		return
	}
	s.mu.Lock()
	gate := s.getGate
	s.mu.Unlock()
	if bucket == "source" && r.Method == http.MethodGet && !s.wait(r, gate) {
		return
	}
	s.mu.Lock()
	var payload []byte
	var found bool
	if bucket == "source" {
		payload, found = s.source[key]
		if r.Method == http.MethodGet {
			s.reads[key]++
		}
	} else if bucket == "target" {
		payload, found = s.target[key]
	}
	s.mu.Unlock()
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	sum := md5.Sum(payload)
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.Header().Set("Last-Modified", "Thu, 01 Jan 2026 00:00:00 GMT")
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.Method == http.MethodGet {
		_, _ = w.Write(payload)
	}
}

type plannedHarness struct {
	t       *testing.T
	f       *agentHTTPFixture
	storage *plannedStorage
	runtime *taskRuntime
	ctx     context.Context
	a, b    *takeoverAgent
}

func newPlannedHarness(t *testing.T, objects int) *plannedHarness {
	t.Helper()
	f := newAgentHTTPFixture(t)
	storage := newPlannedStorage(objects)
	s3 := httptest.NewServer(storage)
	t.Cleanup(s3.Close)
	a, b := takeoverTestAgent(t, f, "planned-a"), takeoverTestAgent(t, f, "planned-b")
	labelTakeoverAgent(a, "a")
	labelTakeoverAgent(b, "b")
	waitOnline(t, f, a.node, b.node)
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, map[string]any{
		"alias": "qa", "endpoint": s3.URL, "accessKey": "fixture-ak", "secretKey": "fixture-sk", "region": "test", "pathStyleEnable": true, "validate": false,
	})
	if code != http.StatusOK {
		t.Fatal("storage setup", code, body)
	}
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	// A hang guard for slow race-instrumented runners, not a timing assertion.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	return &plannedHarness{t: t, f: f, storage: storage, runtime: runtime, ctx: ctx, a: a, b: b}
}

// step runs reconciliation the way the Master's loop does until done reports
// true, and fails as soon as a task error has been recorded.
func (h *plannedHarness) step(id int64, what string, done func(task.Parent) bool) task.Parent {
	h.t.Helper()
	for {
		p, err := h.runtime.repo.Get(id)
		if err != nil {
			h.t.Fatal(err)
		}
		stepErr := h.runtime.recordStep(h.ctx, id, p.Generation)
		if p, err = h.runtime.repo.Get(id); err != nil {
			h.t.Fatal(err)
		}
		if p.RuntimeError != "" || p.Error != "" || p.Status == "FAILED" || p.Status == "PARTIAL_FAILED" {
			h.t.Fatalf("%s: task error recorded: status=%s runtime=%q error=%q", what, p.Status, p.RuntimeError, p.Error)
		}
		if done(p) {
			return p
		}
		if h.ctx.Err() != nil {
			h.t.Fatalf("%s: not reached (last step: %v): %+v", what, stepErr, p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (h *plannedHarness) view(id int64) map[string]any {
	h.t.Helper()
	status, body := request(h.t, h.f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/nodes", h.f.http.URL, id), h.f.admin, nil)
	if status != 200 {
		h.t.Fatal(status, body)
	}
	return body["data"].(map[string]any)
}

// scanners lists the members the node view marks as the scanner, and the phase.
func scanners(view map[string]any) ([]string, string) {
	var ids []string
	for _, row := range view["nodes"].([]any) {
		if node := row.(map[string]any); node["scanner"] == true {
			ids = append(ids, node["id"].(string))
		}
	}
	phase, _ := view["scan"].(map[string]any)["phase"].(string)
	return ids, phase
}

func (h *plannedHarness) change(id int64, body map[string]any) {
	h.t.Helper()
	if status, response := postNodes(h.t, h.f, id, body); status != 200 {
		h.t.Fatal(status, response)
	}
}

func (s *plannedStorage) block(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner == "" {
		s.getGate = make(chan struct{})
	} else {
		s.listGate[owner] = make(chan struct{})
	}
}

func (s *plannedStorage) open(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gate := s.getGate
	if owner != "" {
		gate = s.listGate[owner]
	}
	if gate != nil {
		close(gate)
	}
	if owner == "" {
		s.getGate = nil
	} else {
		delete(s.listGate, owner)
	}
}

func completed(p task.Parent) bool { return p.Status == "COMPLETED" }

// Removing the scanner after its scan has finished: the fenced node answers
// "permanently retired" to anything still addressed to it during that step.
// That reply is expected and must not become the task's error; the view shows
// the finished scan instead of a scanner.
func TestPlannedChangeRemovingFinishedScannerRecordsNoTaskError(t *testing.T) {
	h := newPlannedHarness(t, 4)
	h.storage.block("") // hold transfers so the task outlives the change
	p := takeoverParent(t, h.f, h.a.node, h.b.node)
	startTakeoverLeaseLoop(t, h.ctx, h.runtime)
	p = h.step(p.ID, "scan finished and assigned", func(p task.Parent) bool {
		return p.Failover != nil && p.Failover.Enabled && p.ScannedBatches >= 1 && scanFinished(p)
	})
	if p.ScannerNode().ID != h.a.node.ID {
		t.Fatal("unexpected scanner", p.ScannerNode())
	}
	if ids, phase := scanners(h.view(p.ID)); len(ids) != 0 || phase != scanPhaseComplete {
		t.Fatal("finished scan still shows a scanner", ids, phase)
	}
	h.change(p.ID, map[string]any{"requestId": "remove-scanner", "remove": []string{h.a.node.ID}, "mode": "now"})
	p = h.step(p.ID, "scanner removed", func(p task.Parent) bool { return p.NodeRemoved(h.a.node.ID) })
	// Further steps still address nothing to the removed node.
	for range 3 {
		p = h.step(p.ID, "after removal", func(task.Parent) bool { return true })
	}
	view := h.view(p.ID)
	if ids, phase := scanners(view); len(ids) != 0 || phase != scanPhaseComplete || view["scan"].(map[string]any)["complete"] != true {
		t.Fatal("removed scanner's finished scan is not shown as complete", ids, phase)
	}
	if nodes := view["nodes"].([]any); len(nodes) != 1 || nodes[0].(map[string]any)["id"] != h.b.node.ID {
		t.Fatal("removed scanner still listed", nodes)
	}
	h.storage.open("")
	p = h.step(p.ID, "completion", completed)
	if p.Counters.Succeeded != 4 {
		t.Fatal("counters", p.Counters)
	}
}

// Removing the scanner mid-scan moves the scan to another member, which
// continues from the Master's checkpoint instead of listing from the start.
// The node view labels the new scanner throughout, and no stage of the change
// is recorded as a task error.
func TestPlannedChangeRemovingScannerMidScanResumesFromCheckpoint(t *testing.T) {
	h := newPlannedHarness(t, 256)
	h.storage.block("a") // the original scanner stops after its first page
	h.storage.block("b") // and its replacement waits at its first page
	p := takeoverParent(t, h.f, h.a.node, h.b.node)
	startTakeoverLeaseLoop(t, h.ctx, h.runtime)
	p = h.step(p.ID, "first batch assigned while the scanner waits", func(p task.Parent) bool {
		return p.Failover != nil && p.Failover.Enabled && p.DispatchedThrough >= 1 && p.ScanCheckpoint != nil && h.storage.listed("a", "128")
	})
	if ids, phase := scanners(h.view(p.ID)); len(ids) != 1 || ids[0] != h.a.node.ID || phase != scanPhaseScanning {
		t.Fatal("scanning member not labelled", ids, phase)
	}
	h.change(p.ID, map[string]any{"requestId": "drain-scanner", "remove": []string{h.a.node.ID}, "mode": "drain"})
	p = h.step(p.ID, "scanner drained and removed", func(p task.Parent) bool { return p.NodeRemoved(h.a.node.ID) })
	if p.ScannerNode().ID != h.b.node.ID || p.Failover.ScannerThrough < 1 {
		t.Fatal("scan did not move to the remaining node at the checkpoint", p.ScannerNode(), p.Failover)
	}
	p = h.step(p.ID, "replacement continues the listing", func(task.Parent) bool { return h.storage.listed("b", "128") })
	if ids, phase := scanners(h.view(p.ID)); len(ids) != 1 || ids[0] != h.b.node.ID || (phase != scanPhaseScanning && phase != scanPhaseHandoff) {
		t.Fatal("new scanner not labelled", ids, phase)
	}
	if h.storage.listed("b", "") {
		t.Fatal("the replacement scanner listed the source from the start")
	}
	h.storage.open("b")
	h.storage.open("a")
	p = h.step(p.ID, "completion", completed)
	if p.ScannedObjects != 256 || p.Counters.Succeeded != 256 || p.Counters.Skipped != 0 {
		t.Fatal("counters", p.ScannedObjects, p.Counters)
	}
	h.storage.mu.Lock()
	for key := range h.storage.source {
		if h.storage.reads[key] != 1 {
			t.Errorf("%s read %d times", key, h.storage.reads[key])
		}
	}
	h.storage.mu.Unlock()
	if ids, phase := scanners(h.view(p.ID)); len(ids) != 0 || phase != scanPhaseComplete {
		t.Fatal("completed scan still shows a scanner", ids, phase)
	}
}

// Adding a node: the step that admits the joiner has not prepared it yet. It
// is not an unavailable node, and the join must not flash a task error.
func TestPlannedChangeJoinRecordsNoTaskError(t *testing.T) {
	h := newPlannedHarness(t, 4)
	h.storage.block("")
	p := takeoverParent(t, h.f, h.a.node)
	startTakeoverLeaseLoop(t, h.ctx, h.runtime)
	p = h.step(p.ID, "first child running", func(p task.Parent) bool { return p.DispatchedThrough >= 1 })
	h.change(p.ID, map[string]any{"requestId": "join-b", "add": []string{h.b.node.ID}})
	p = h.step(p.ID, "joined", func(p task.Parent) bool { return !p.NodeJoining(h.b.node.ID) && len(p.ActiveNodes()) == 2 })
	for range 3 {
		p = h.step(p.ID, "after join", func(task.Parent) bool { return true })
	}
	view := h.view(p.ID)
	for _, row := range view["nodes"].([]any) {
		if node := row.(map[string]any); node["state"] != "ACTIVE" || node["share"] == nil {
			t.Fatal("member not active with a share", node)
		}
	}
	h.storage.open("")
	h.step(p.ID, "completion", completed)
}

// setLeaseFence moves the Master's lease fence for a node, so a test decides
// when the node's lease counts as expired instead of waiting for it.
func (h *plannedHarness) setLeaseFence(p task.Parent, node task.Node, at time.Time) {
	h.t.Helper()
	e, err := h.runtime.leases.entry(p, node)
	if err != nil {
		h.t.Fatal(err)
	}
	e.mu.Lock()
	e.safeAfter = at
	e.mu.Unlock()
}

// An offline scanner is a real problem until the user removes it. From then
// on, waiting for its lease to expire and moving the scan are progress of the
// requested change: the node view shows them, the task records no error.
func TestPlannedChangeRemovingOfflineScannerReportsProgress(t *testing.T) {
	h := newPlannedHarness(t, 256)
	h.storage.block("a")
	h.storage.block("b")
	p := takeoverParent(t, h.f, h.a.node, h.b.node)
	p = h.step(p.ID, "first batch assigned while the scanner waits", func(p task.Parent) bool {
		return p.Failover != nil && p.Failover.Enabled && p.DispatchedThrough >= 1 && h.storage.listed("a", "128")
	})
	p = h.step(p.ID, "first batch finished", func(p task.Parent) bool { return p.FinishedChildren >= 1 })
	// However slow this runner is, the disconnected scanner's lease must not
	// expire before the removal below.
	h.setLeaseFence(p, h.a.node, time.Now().Add(time.Hour))
	h.a.cancel() // the scanner's control channel drops
	<-h.a.done
	h.a.done <- nil // for the cleanup registered by takeoverTestAgent
	// Its heartbeat record ages out after 75 seconds; skip that wait.
	h.f.master.agents.mu.Lock()
	delete(h.f.master.agents.lastSeen, h.a.node.ID)
	h.f.master.agents.mu.Unlock()
	// Unexpected: reported, one problem per line.
	if err := h.runtime.recordStep(h.ctx, p.ID, p.Generation); err == nil {
		current, _ := h.runtime.repo.Get(p.ID)
		t.Fatalf("an unexpected disconnect was not reported: %+v", current)
	}
	p, _ = h.runtime.repo.Get(p.ID)
	if p.RuntimeError != "Waiting for an unavailable node's execution lease to expire\nWaiting for the scanning node to reconnect; existing child tasks remain assigned to their original nodes" {
		t.Fatalf("unexpected disconnect problem: %q", p.RuntimeError)
	}
	h.change(p.ID, map[string]any{"requestId": "remove-offline-scanner", "remove": []string{h.a.node.ID}, "mode": "drain"})
	p = h.step(p.ID, "removal accepted", func(task.Parent) bool { return true })
	view := h.view(p.ID)
	var leaver map[string]any
	for _, row := range view["nodes"].([]any) {
		if node := row.(map[string]any); node["id"] == h.a.node.ID {
			leaver = node
		}
	}
	if leaver == nil || leaver["state"] != "DRAINING" || leaver["online"] != false || leaver["progress"] != "LEASE_EXPIRY" {
		t.Fatal("offline leaver's progress not shown", leaver)
	}
	h.setLeaseFence(p, h.a.node, time.Now().Add(-time.Second)) // its lease has expired
	p = h.step(p.ID, "offline scanner fenced and removed", func(p task.Parent) bool { return p.NodeRemoved(h.a.node.ID) })
	if p.ScannerNode().ID != h.b.node.ID {
		t.Fatal("scan did not move", p.ScannerNode())
	}
	p = h.step(p.ID, "replacement continues the listing", func(task.Parent) bool { return h.storage.listed("b", "128") })
	if ids, phase := scanners(h.view(p.ID)); len(ids) != 1 || ids[0] != h.b.node.ID || (phase != scanPhaseScanning && phase != scanPhaseHandoff) {
		t.Fatal("new scanner not labelled", ids, phase)
	}
	h.storage.open("b")
	h.storage.open("a")
	p = h.step(p.ID, "completion", completed)
	if p.Counters.Succeeded != 256 || h.storage.listed("b", "") {
		t.Fatal("scan restarted or objects missing", p.Counters)
	}
}

// Replies from a node the task removed on purpose are filtered; the same reply
// from a current member, or any other failure, is kept.
func TestWithoutPlannedProblemsKeepsRealProblems(t *testing.T) {
	removed, current := "node-removed", "node-current"
	p := task.Parent{Spec: task.Spec{Nodes: []task.Node{{ID: current}, {ID: removed}}}, Membership: &task.MembershipState{Removed: map[string]time.Time{removed: {}}}}
	retired := func(node string) error {
		return &AgentError{Status: 409, Message: agentservice.ErrLeaseRetired.Error(), NodeID: node}
	}
	real := errors.New("Waiting for an unavailable node's execution lease to expire")
	if err := withoutPlannedProblems(p, errors.Join(retired(removed), fmt.Errorf("scan: %w", retired(removed)))); err != nil {
		t.Fatal("planned retirement kept", err)
	}
	got := withoutPlannedProblems(p, errors.Join(real, retired(removed), retired(current)))
	if runtimeProblemMessage(got) != real.Error()+"\n"+agentservice.ErrLeaseRetired.Error() {
		t.Fatalf("real problems lost: %q", runtimeProblemMessage(got))
	}
	draining := p
	draining.Membership = &task.MembershipState{Draining: map[string]task.NodeDrain{current: {Mode: task.DrainModeNow}}}
	if withoutPlannedProblems(draining, retired(current)) != nil {
		t.Fatal("a fenced node being removed is a planned stage")
	}
	if runtimeProblemMessage(errors.Join(real, real, errors.New(" "))) != real.Error() {
		t.Fatal("repeated or blank lines kept")
	}
}

// A node leaving raises the budget revision; the remaining node receives its
// larger share with the next reconciliation step. Until then the share is
// still being delivered, which the limit status must not report as a fault.
func TestPlannedChangeBandwidthStatusWaitsForRaisedShare(t *testing.T) {
	f := newAgentHTTPFixture(t)
	a, b := takeoverTestAgent(t, f, "share-a"), takeoverTestAgent(t, f, "share-b")
	waitOnline(t, f, a.node, b.node)
	p := takeoverParent(t, f, a.node, b.node)
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	available := map[string]bool{a.node.ID: true, b.node.ID: true}
	p, err := runtime.reconcileFailover(ctx, p, available)
	if err != nil || !p.Failover.Enabled {
		t.Fatal(err)
	}
	if err := runtime.policies(ctx, p); err != nil {
		t.Fatal(err)
	}
	status := func() map[string]any {
		code, body := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/bandwidth-limit", f.http.URL, p.ID), f.admin, nil)
		if code != 200 {
			t.Fatal(code, body)
		}
		return body["data"].(map[string]any)
	}
	if data := status(); data["applied"] != true {
		t.Fatal("initial shares not applied", data)
	}
	if code, body := postNodes(t, f, p.ID, map[string]any{"requestId": "share-leave", "remove": []string{b.node.ID}}); code != 200 {
		t.Fatal(code, body)
	}
	p, _ = runtime.repo.Get(p.ID)
	if p, err = runtime.reconcileMembership(ctx, p, available); err != nil || !p.NodeRemoved(b.node.ID) {
		t.Fatal("node did not leave", err, p.Membership)
	}
	data := status()
	nodes := data["nodes"].([]any)
	if data["applied"] != false || data["pending"] != true || len(nodes) != 1 || nodes[0].(map[string]any)["pending"] != true {
		t.Fatal("raised share reported as a fault before delivery", data)
	}
	if err := runtime.policies(ctx, p); err != nil {
		t.Fatal(err)
	}
	if data := status(); data["applied"] != true || data["pending"] != false {
		t.Fatal("raised share not applied", data)
	}
}
