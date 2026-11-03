package master

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/nodecontrol"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// One large object in flight must move byte progress and the migrated amount
// without crediting it durably or writing anything while views are polled.
func TestLiveTransferredBytesFollowLargeObjectWithoutDurableCredit(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	const large, small int64 = 4 << 30, 1 << 20
	c := assignProgressChild(t, f, p, 1, []transferstore.Object{{Key: "large", Size: large}, {Key: "small", Size: small}})
	record := *c.Record
	record.Counters.Succeeded, record.Counters.SucceededBytes = 1, small
	if err := f.master.tasks.repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	c.Record = &record
	if err := f.master.tasks.repo.ScanProgress(p.ID, p.Generation, 2, large+small, 1, true, ""); err != nil {
		t.Fatal(err)
	}
	sample := childProgressSample(c, 1, 1<<30)
	sample.SucceededBytes = small
	serveProgressAgent(t, f, c.Node, func(command agenttransport.Command) (any, error) {
		if command.Operation != "task_metrics" {
			return nil, fmt.Errorf("unexpected operation %s", command.Operation)
		}
		return progressMetrics(p, sample), nil
	})
	f.master.tasks.sampleRates(context.Background(), p)
	p, err := f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantPercent := math.Floor(float64(small+1<<30)/float64(large+small)*100*100) / 100
	for _, row := range []map[string]any{f.master.taskView(p), f.master.childView(p, c)} {
		if row["putObjectSize"] != small || row["transferredBytes"] != small+1<<30 || row["inFlightBytes"] != int64(1<<30) || row["liveProgress"] != "LIVE" {
			t.Fatalf("live bytes = %+v", row)
		}
		if row["progressBasis"] != "BYTES" || row["progressPercent"] != wantPercent {
			t.Fatalf("progress = %v %v, want BYTES %v", row["progressBasis"], row["progressPercent"], wantPercent)
		}
	}
	// Polling the overview and detail must only read the in-memory sample.
	before, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		status, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/status", f.admin, nil)
		if status != 200 || body["totalTransferredBytes"] != float64(small+1<<30) || body["totalPutObjectSize"] != float64(small) {
			t.Fatalf("overview totals: %d %v %v", status, body["totalTransferredBytes"], body["totalPutObjectSize"])
		}
		if status, _ := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d", f.http.URL, p.ID), f.admin, nil); status != 200 {
			t.Fatal("detail", status)
		}
	}
	if after, err := os.ReadFile(f.path); err != nil || !bytes.Equal(before, after) {
		t.Fatal("viewing live bytes wrote to the task database", err)
	}

	original, _ := f.master.tasks.rateSnapshot(p)
	setEntry := func(entry taskRateCache) {
		f.master.tasks.mu.Lock()
		f.master.tasks.rates[p.ID] = entry
		f.master.tasks.mu.Unlock()
	}
	stale := original
	stale.observed = time.Now().Add(-11 * time.Second)
	setEntry(stale)
	for _, row := range []map[string]any{f.master.taskView(p), f.master.childView(p, c)} {
		if row["transferredBytes"] != small || row["inFlightBytes"] != nil || row["liveProgress"] != "STALE" {
			t.Fatalf("stale sample was shown as live or zero: %+v", row)
		}
	}
	legacy := original
	legacy.progressSupported, legacy.progress = false, nil
	setEntry(legacy)
	if row := f.master.taskView(p); row["liveProgress"] != "UNSUPPORTED" || row["inFlightBytes"] != nil || row["transferredBytes"] != small {
		t.Fatalf("older Agent: %+v", row)
	}

	// The Agent credited the large object before the ledger did: no double count.
	setEntry(original)
	record.Counters.Succeeded, record.Counters.SucceededBytes = 2, large+small
	record.Status, record.UpdatedAt = execution.Completed, time.Now()
	finished := time.Now()
	record.FinishedAt = &finished
	if err := f.master.tasks.repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	c.Record = &record
	if ready, err := f.master.tasks.repo.FinishIfReady(p.ID, p.Generation); err != nil || !ready {
		t.Fatal("fixture did not finish", ready, err)
	}
	p, err = f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []map[string]any{f.master.taskView(p), f.master.childView(p, c)} {
		if row["transferredBytes"] != large+small || row["inFlightBytes"] != int64(0) || row["liveProgress"] != "NONE" || row["progressPercent"] != float64(100) {
			t.Fatalf("ended task kept live bytes: %+v", row)
		}
	}
}

// Another node's child with the same number must never lend its bytes.
func TestLiveTransferredBytesMatchOnlyTheCurrentChildAssignment(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	c := assignProgressChild(t, f, p, 1, []transferstore.Object{{Key: "large", Size: 1000}})
	sample := childProgressSample(c, 1, 400)
	sample.Assignment++
	serveProgressAgent(t, f, c.Node, func(agenttransport.Command) (any, error) { return progressMetrics(p, sample), nil })
	f.master.tasks.sampleRates(context.Background(), p)
	if row := f.master.childView(p, c); row["transferredBytes"] != int64(0) || row["inFlightBytes"] != int64(0) || row["liveProgress"] != "LIVE" {
		t.Fatalf("mismatched assignment lent bytes: %+v", row)
	}
}

func TestProgressPercentRules(t *testing.T) {
	row := func(status string, scanned bool, count, size, put, skipped, transferred, skippedSize int64) map[string]any {
		return map[string]any{"status": status, "scanComplete": scanned, "listObjectCount": count, "listObjectSize": size, "putObjectCount": put, "skippedObjectCount": skipped, "transferredBytes": transferred, "skippedObjectSize": skippedSize}
	}
	for _, tc := range []struct {
		name    string
		row     map[string]any
		percent any
		basis   string
	}{
		{"scanning", row("RUNNING", false, 10, 100, 1, 0, 50, 0), nil, "BYTES"},
		{"counts mid-update", row("RUNNING", true, 10, 100, 9, 2, 50, 0), nil, "BYTES"},
		{"bytes", row("RUNNING", true, 10, 1000, 1, 1, 400, 100), float64(50), "BYTES"},
		{"zero-byte objects pending", row("RUNNING", true, 10, 1000, 5, 0, 1000, 0), 99.9, "BYTES"},
		{"all handled", row("RUNNING", true, 10, 1000, 10, 0, 1000, 0), float64(100), "BYTES"},
		{"overlap capped", row("RUNNING", true, 2, 1000, 1, 1, 1000, 500), float64(100), "BYTES"},
		{"no sizes", row("RUNNING", true, 4, 0, 1, 0, 0, 0), float64(25), "COUNT"},
		{"empty", row("RUNNING", true, 0, 0, 0, 0, 0, 0), float64(0), "COUNT"},
		{"completed", row("COMPLETED", false, 0, 0, 0, 0, 0, 0), float64(100), "COUNT"},
		{"child int count", map[string]any{"status": "RUNNING", "scanComplete": true, "listObjectCount": 3, "listObjectSize": int64(0), "putObjectCount": int64(1), "skippedObjectCount": int64(0)}, 33.33, "COUNT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addProgressPercent(tc.row)
			if tc.row["progressPercent"] != tc.percent || tc.row["progressBasis"] != tc.basis {
				t.Fatalf("got %v %v, want %v %v", tc.row["progressPercent"], tc.row["progressBasis"], tc.percent, tc.basis)
			}
		})
	}
}

func requestMetrics(p task.Parent, rates nodecontrol.TaskRequestRates, limits nodecontrol.NodeRequestLimits) agentservice.TaskRateSnapshot {
	reply := progressMetrics(p)
	reply.Requests = &nodecontrol.TaskRequests{Rates: rates, NodeLimits: limits}
	return reply
}

func TestRequestRatesSumNodesAndNeverShowPartialSums(t *testing.T) {
	f, p := progressMasterFixture(t, 2)
	p.Spec.RequestLimits = &execution.RequestLimits{Scope: "CLUSTER", ListQPS: 100, PutQPS: 40}
	if _, err := f.master.tasks.repo.UpdateLimits(p.ID, nil, p.Spec.RequestLimits, true, nil); err != nil {
		t.Fatal(err)
	}
	p, err := f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacy := make(chan bool, 1)
	legacy <- false
	for index, node := range p.Spec.Nodes {
		serveProgressAgent(t, f, node, func(agenttransport.Command) (any, error) {
			if index == 1 {
				old := <-legacy
				legacy <- old
				if old {
					return progressMetrics(p), nil
				}
			}
			return requestMetrics(p, nodecontrol.TaskRequestRates{List: 1, Get: 10, Head: 2, Put: 5}, nodecontrol.NodeRequestLimits{List: 30, Head: 0, Put: float64(10 * (index + 1))}), nil
		})
	}
	f.master.tasks.sampleRates(context.Background(), p)
	view := f.master.taskView(p)["requestRates"].(map[string]any)
	if view["observed"] != (nodecontrol.TaskRequestRates{List: 2, Get: 20, Head: 4, Put: 10}) {
		t.Fatalf("observed = %+v", view["observed"])
	}
	if view["configured"] != (taskRequestLimitView{Scope: "CLUSTER", List: 100, Put: 40}) {
		t.Fatalf("configured = %+v", view["configured"])
	}
	// LIST/GET: task share only. HEAD: unlimited on both sides. PUT: min(20,10)+min(20,20).
	if view["effective"] != (taskRequestLimitView{List: 100, Head: 0, Put: 30}) {
		t.Fatalf("effective = %+v", view["effective"])
	}
	<-legacy
	legacy <- true
	entry, _ := f.master.tasks.rateSnapshot(p)
	entry.attempted = time.Now().Add(-2 * time.Second)
	f.master.tasks.mu.Lock()
	f.master.tasks.rates[p.ID] = entry
	f.master.tasks.mu.Unlock()
	f.master.tasks.sampleRates(context.Background(), p)
	view = f.master.taskView(p)["requestRates"].(map[string]any)
	if view["observed"] != nil || view["effective"] != nil || view["configured"] == nil {
		t.Fatalf("an older Agent produced a partial sum: %+v", view)
	}
}

func TestEffectiveRequestLimitsByScope(t *testing.T) {
	nodes := []task.Node{{ID: "a"}, {ID: "b"}}
	limits := map[string]nodecontrol.NodeRequestLimits{"a": {Head: 5, Put: 100}, "b": {Head: 50, Put: 0}}
	for _, tc := range []struct {
		name   string
		limits *execution.RequestLimits
		want   taskRequestLimitView
	}{
		{"cluster", &execution.RequestLimits{Scope: "CLUSTER", HeadQPS: 20, PutQPS: 60}, taskRequestLimitView{Head: 5 + 10, Put: 30 + 30}},
		{"per node", &execution.RequestLimits{Scope: "PER_NODE", HeadQPS: 20, PutQPS: 60}, taskRequestLimitView{Head: 5 + 20, Put: 60 + 60}},
		{"node unlimited and task unlimited", &execution.RequestLimits{Scope: "CLUSTER", HeadQPS: 20}, taskRequestLimitView{Head: 15, Put: 0}},
		{"no task limits", nil, taskRequestLimitView{Head: 55, Put: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := task.Parent{ID: 1, Generation: 1, Status: "RUNNING", Spec: task.Spec{Nodes: nodes, RequestLimits: tc.limits}}
			got, ok := effectiveRequestLimits(p, limits)
			if !ok || got != tc.want {
				t.Fatalf("got %+v %v, want %+v", got, ok, tc.want)
			}
		})
	}
	if _, ok := effectiveRequestLimits(task.Parent{Spec: task.Spec{Nodes: nodes}}, map[string]nodecontrol.NodeRequestLimits{"a": {}}); ok {
		t.Fatal("a node missing from the sample produced a bound")
	}
}
