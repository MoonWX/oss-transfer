package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// failingWrites makes chosen destination writes fail with a permanent error
// until it is cleared, so a child can end with failed objects on demand.
type failingWrites struct {
	mu   sync.Mutex
	keys map[string]bool
}

func (f *failingWrites) set(keys ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = map[string]bool{}
	for _, key := range keys {
		f.keys[key] = true
	}
}
func (f *failingWrites) fails(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys[key]
}

type failingTarget struct {
	transferstore.Store
	failures *failingWrites
}

func (t failingTarget) Write(ctx context.Context, key string, input io.Reader, options transferstore.WriteOptions) (transferstore.WriteResult, error) {
	if t.failures.fails(key) {
		return transferstore.WriteResult{}, errors.New("injected permanent write failure")
	}
	return t.Store.Write(ctx, key, input, options)
}

// generationReport drives one task through the Master's durable task state and
// a real Agent ledger (Engine, filesystem stores, authenticated transport). The
// steps mirror what the coordinator does, called directly so that every stop,
// resume and retry happens at a chosen point.
type generationReport struct {
	t        *testing.T
	f        *agentHTTPFixture
	engine   *execution.Engine
	failures *failingWrites
	parent   task.Parent
	db       *state.Store
	source   string
}

func newGenerationReport(t *testing.T, keys ...string) *generationReport {
	t.Helper()
	f := newAgentHTTPFixture(t)
	credential := agentCredential(t)
	status, session := f.join(t, f.invite(t, "report.example"), "report-agent", credential)
	if status != 200 {
		t.Fatal(status)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sourceRoot, destinationRoot := t.TempDir(), t.TempDir()
	for _, key := range keys {
		if err := os.WriteFile(filepath.Join(sourceRoot, key), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	failures := &failingWrites{}
	engine, err := execution.NewEngine(repo, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		source, err := transferstore.NewLocal(sourceRoot)
		if err != nil {
			return nil, nil, err
		}
		target, err := transferstore.NewLocal(destinationRoot)
		if err != nil {
			source.Close()
			return nil, nil, err
		}
		return source, failingTarget{target, failures}, nil
	}), execution.Options{})
	if err != nil {
		t.Fatal(err)
	}
	service := &agentservice.Service{Jobs: repo, Engine: engine}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agenttransport.NewRunner(agentClient(t, f, credential), service, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond}).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = engine.Close(context.Background())
		_ = db.Close()
	})
	var node task.Node
	for deadline := time.Now().Add(2 * time.Minute); ; { // hang guard only
		members, err := f.master.onlineAgents()
		if err != nil {
			t.Fatal(err)
		}
		if len(members) == 1 {
			node = task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: members[0].Address}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("report Agent never connected")
		}
		time.Sleep(time.Millisecond)
	}
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{node}
	p, _, err := f.master.tasks.repo.Create(spec, "generation-report")
	if err != nil {
		t.Fatal(err)
	}
	return &generationReport{t: t, f: f, engine: engine, failures: failures, parent: p, db: db, source: sourceRoot}
}

func (g *generationReport) refresh() task.Parent {
	g.t.Helper()
	p, err := g.f.master.tasks.repo.Get(g.parent.ID)
	if err != nil {
		g.t.Fatal(err)
	}
	g.parent = p
	return p
}

// assign dispatches one child per key list and reports the scan complete.
func (g *generationReport) assign(children ...[]string) {
	g.t.Helper()
	objects := 0
	for n, keys := range children {
		var batch []transferstore.Object
		for _, key := range keys {
			batch = append(batch, transferstore.Object{Key: key, Size: 1})
		}
		objects += len(batch)
		if _, err := g.f.master.tasks.repo.Assign(g.parent.ID, g.parent.Generation, n+1, batch); err != nil {
			g.t.Fatal(err)
		}
	}
	if err := g.f.master.tasks.repo.ScanProgress(g.parent.ID, g.parent.Generation, int64(objects), int64(objects), len(children), true, ""); err != nil {
		g.t.Fatal(err)
	}
	g.refresh()
}

func (g *generationReport) child(number int) task.Child {
	g.t.Helper()
	children, err := g.f.master.tasks.repo.Children(g.parent.ID, number-1, 1)
	if err != nil || len(children) != 1 || children[0].Job.Number != number {
		g.t.Fatal("child not found", number, children, err)
	}
	return children[0]
}

func (g *generationReport) observe(number int) {
	g.t.Helper()
	record, err := g.engine.Get(context.Background(), g.child(number).Job.ChildID)
	if err != nil {
		g.t.Fatal(err)
	}
	if err := g.f.master.tasks.repo.Observe(g.parent.ID, g.parent.Generation, number, record); err != nil {
		g.t.Fatal(err)
	}
}

// run lets the Agent accept and run a child in the current generation, as the
// coordinator dispatches it, and reports the ended ledger to the Master.
func (g *generationReport) run(number int) execution.Record {
	g.t.Helper()
	ctx := context.Background()
	child := g.child(number)
	if _, err := g.engine.Accept(ctx, child.Job); err != nil {
		g.t.Fatal(err)
	}
	if err := g.engine.Start(ctx, child.Job.ChildID, g.parent.Generation); err != nil {
		g.t.Fatal(err)
	}
	record, err := g.engine.Wait(ctx, child.Job.ChildID)
	if err != nil {
		g.t.Fatal(err)
	}
	g.observe(number)
	return record
}

// accept admits a child without starting it, so a stop leaves it unprocessed.
func (g *generationReport) accept(number int) {
	g.t.Helper()
	if _, err := g.engine.Accept(context.Background(), g.child(number).Job); err != nil {
		g.t.Fatal(err)
	}
}

func (g *generationReport) stop() {
	g.t.Helper()
	if _, err := g.f.master.tasks.repo.Stop(g.parent.ID); err != nil {
		g.t.Fatal(err)
	}
	if err := g.engine.StopParent(context.Background(), g.parent.ID, g.parent.Generation); err != nil {
		g.t.Fatal(err)
	}
	if err := g.f.master.tasks.repo.Stopped(g.parent.ID, g.parent.Generation); err != nil {
		g.t.Fatal(err)
	}
	if p := g.refresh(); p.Status != "STOPPED" {
		g.t.Fatal("task did not stop", p.Status)
	}
}

// resume continues (or retries the failed objects of) an ended task: a new
// generation that readmits only the children that did not complete.
func (g *generationReport) resume() {
	g.t.Helper()
	expected := g.parent.Generation
	p, accepted, err := g.f.master.tasks.repo.RequestResume(g.parent.ID, &expected)
	if err != nil || !accepted || p.Generation != expected+1 {
		g.t.Fatal("resume refused", accepted, err)
	}
	for ready := false; !ready; {
		if ready, err = g.f.master.tasks.repo.PrepareResume(p.ID, p.Generation); err != nil {
			g.t.Fatal(err)
		}
	}
	if err := g.f.master.tasks.repo.Resumed(p.ID, p.Generation); err != nil {
		g.t.Fatal(err)
	}
	// The coordinator always resumes with failed objects reopened.
	if err := g.engine.ResumeParent(context.Background(), p.ID, p.Generation, true); err != nil {
		g.t.Fatal(err)
	}
	g.refresh()
}

func (g *generationReport) finish(want string) {
	g.t.Helper()
	if finished, err := g.f.master.tasks.repo.FinishIfReady(g.parent.ID, g.parent.Generation); err != nil || !finished {
		g.t.Fatal("task did not finish", finished, err)
	}
	if p := g.refresh(); p.Status != want {
		g.t.Fatal("unexpected final status", p.Status, want)
	}
}

type generationReportResult struct {
	meta       map[string]any
	rows       []map[string]string
	exceptions []map[string]string
}

func (g *generationReport) export() generationReportResult {
	g.t.Helper()
	status, files := fetchReport(g.t, g.f, "GET", fmt.Sprintf("/api/master/task-reports/%d", g.parent.ID), g.f.admin, "")
	if status != 200 {
		g.t.Fatal("report export failed", status)
	}
	var result generationReportResult
	if err := json.Unmarshal(files["report-metadata.json"], &result.meta); err != nil {
		g.t.Fatal(err)
	}
	result.rows = reportRows(g.t, files["file-results.csv"])
	result.exceptions = reportRows(g.t, files["exception-details.csv"])
	return result
}

// results maps each logical key to its reported result.
func (r generationReportResult) results() map[string]string {
	out := map[string]string{}
	for _, row := range r.rows {
		out[row["logicalKey"]] = row["result"]
	}
	return out
}

func (r generationReportResult) assertComplete(t *testing.T, keys ...string) {
	t.Helper()
	if r.meta["complete"] != true || r.meta["countsReconciled"] != true || len(r.meta["partialReasons"].([]any)) != 0 || len(r.meta["unreachableNodes"].([]any)) != 0 {
		t.Fatal("finished task report is not complete", r.meta)
	}
	results := r.results()
	if len(r.rows) != len(keys) || len(results) != len(keys) {
		t.Fatal("report rows do not match the task's objects", r.rows)
	}
	for _, key := range keys {
		if results[key] != "SUCCESS" {
			t.Fatal("object lost its final result", key, results)
		}
	}
	if len(r.exceptions) != 0 {
		t.Fatal("finished task report lists exceptions", r.exceptions)
	}
}

func (r generationReportResult) assertNoUnavailableLedger(t *testing.T) {
	t.Helper()
	for _, row := range r.rows {
		if row["result"] == "UNKNOWN" || row["reasonCode"] == "LEDGER_UNAVAILABLE" {
			t.Fatal("an available ledger was reported as unavailable", row)
		}
	}
	if len(r.meta["unreachableNodes"].([]any)) != 0 {
		t.Fatal("a reachable Agent was reported as unreachable", r.meta)
	}
}

// A task stopped and continued keeps the children that completed before the
// stop in their original generation. The report must still read their
// ledgers, not mark them UNKNOWN (LEDGER_UNAVAILABLE).
func TestReportAfterStopAndContinueIsComplete(t *testing.T) {
	g := newGenerationReport(t, "a", "b", "c")
	g.assign([]string{"a"}, []string{"b", "c"})
	if record := g.run(1); record.Status != execution.Completed {
		t.Fatal("first child did not complete", record.Status)
	}
	g.accept(2)
	g.stop()
	g.observe(2)

	stopped := g.export()
	stopped.assertNoUnavailableLedger(t)
	if stopped.meta["complete"] != false || stopped.results()["a"] != "SUCCESS" || stopped.results()["b"] != "UNPROCESSED" {
		t.Fatal("stopped report misstates progress", stopped.meta, stopped.rows)
	}

	// Continue, then stop again before the Agent readmits the unfinished child.
	// Its ledger is still at the generation it last ran in.
	g.resume()
	g.stop()
	again := g.export()
	again.assertNoUnavailableLedger(t)
	if again.results()["a"] != "SUCCESS" || again.results()["b"] != "UNPROCESSED" || again.results()["c"] != "UNPROCESSED" {
		t.Fatal("report after a second stop misstates progress", again.rows)
	}

	g.resume()
	if record := g.run(2); record.Status != execution.Completed || record.Job.Generation != g.parent.Generation {
		t.Fatal("continued child did not complete in the new generation", record.Status, record.Job.Generation)
	}
	if first := g.child(1); first.Job.Generation != 1 || g.parent.Generation != 3 {
		t.Fatal("fixture no longer spans generations", first.Job.Generation, g.parent.Generation)
	}
	g.finish("COMPLETED")
	g.export().assertComplete(t, "a", "b", "c")
}

// Retrying the failed objects of an ended task readmits only the child with
// failures in a new generation. The report must combine the earlier completed
// child with the retried one.
func TestReportAfterRetryingFailedObjectsIsComplete(t *testing.T) {
	g := newGenerationReport(t, "a", "b", "c")
	g.failures.set("b")
	g.assign([]string{"a"}, []string{"b", "c"})
	g.run(1)
	if record := g.run(2); record.Status != execution.PartialFailed {
		t.Fatal("fixture child did not partially fail", record.Status)
	}
	g.finish("PARTIAL_FAILED")
	failed := g.export()
	failed.assertNoUnavailableLedger(t)
	if failed.meta["complete"] != true || failed.results()["b"] != "FAILED" || len(failed.exceptions) != 1 {
		t.Fatal("failed object not reported", failed.meta, failed.rows, failed.exceptions)
	}

	g.failures.set()
	g.resume()
	if record := g.run(2); record.Status != execution.Completed {
		t.Fatal("retried child did not complete", record.Status, record.Results)
	}
	g.finish("COMPLETED")
	g.export().assertComplete(t, "a", "b", "c")
}

// An Agent that answers but cannot serve one child's ledger is still asked
// for its other children: one missing ledger must not turn every result of
// that node UNKNOWN.
func TestReportKeepsReadingAnAgentAfterOneChildLedgerIsMissing(t *testing.T) {
	g := newGenerationReport(t, "a", "b")
	g.assign([]string{"a"}, []string{"b"})
	// The Agent never received child 1 (its dispatch was not delivered).
	g.run(2)
	g.stop()
	report := g.export()
	results := report.results()
	if results["a"] != "UNKNOWN" || results["b"] != "SUCCESS" {
		t.Fatal("one missing ledger hid the node's other results", report.rows)
	}
	if report.meta["complete"] != false || len(report.meta["unreachableNodes"].([]any)) != 1 {
		t.Fatal("missing ledger not disclosed", report.meta)
	}
	// The codes stay machine-readable; the files also explain them in words.
	for _, row := range report.rows {
		if row["result"] == "UNKNOWN" && (row["reasonCode"] != "LEDGER_UNAVAILABLE" || !strings.HasPrefix(row["errorMessage"], "The object result could not be read from its execution node: ")) {
			t.Fatal("unknown result not explained", row)
		}
	}
	meanings, _ := report.meta["reasonCodeMeaning"].(map[string]any)
	for _, code := range []string{"LEDGER_UNAVAILABLE", "NODE_REPORT_INCOMPLETE", "TASK_STOPPED"} {
		if meanings[code] == nil || meanings[code] == "" {
			t.Fatal("report code has no explanation", code, meanings)
		}
	}
}

// An object that changed at its source fails; retrying it copies its current
// version, pinned with another size. The report of the completed task counts
// only the size difference of that pin, so it reconciles.
func TestReportAfterRetryingAChangedSourceIsComplete(t *testing.T) {
	g := newGenerationReport(t, "a", "b")
	if err := g.db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	g.assign([]string{"a"}, []string{"b"})
	if err := os.WriteFile(filepath.Join(g.source, "b"), []byte("a newer, larger version"), 0o600); err != nil {
		t.Fatal(err)
	}
	g.run(1)
	if record := g.run(2); record.Status != execution.Failed || record.Results[0].Reconciliation != execution.ReasonSourceChanged {
		t.Fatal("fixture child did not fail with a changed source", record.Status, record.Results)
	}
	g.finish("PARTIAL_FAILED")
	g.resume()
	ctx := context.Background()
	child := g.child(2)
	if _, err := g.engine.AcceptDelivery(ctx, child.Job, nil, execution.Delivery{SourceRefresh: true}); err != nil {
		t.Fatal(err)
	}
	if err := g.engine.Start(ctx, child.Job.ChildID, g.parent.Generation); err != nil {
		t.Fatal(err)
	}
	record, err := g.engine.Wait(ctx, child.Job.ChildID)
	if err != nil || record.Status != execution.Completed || record.Results[0].Reconciliation != execution.ReasonSourceRefreshed || record.Results[0].ResolvedSource == nil {
		t.Fatal("retried child did not copy the current version", record.Status, record.Results, err)
	}
	g.observe(2)
	g.finish("COMPLETED")
	g.export().assertComplete(t, "a", "b")
}
