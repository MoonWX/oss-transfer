package master

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func TestTaskRateAPIUsesAllAgentsAndRejectsStaleCache(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, node := taskAPIAgent(t, f)
	parent := task.Parent{ID: 9, Generation: 1, Status: "RUNNING", Spec: task.Spec{Nodes: []task.Node{node}}}
	reader, err := service.Rates.Decorator(func(_ context.Context, _ execution.Job, _ execution.Item, r io.ReadCloser) (io.ReadCloser, error) {
		return r, nil
	})(context.Background(), execution.Job{ParentID: 9, Generation: 1, ChildID: "9-1"}, execution.Item{}, io.NopCloser(strings.NewReader(strings.Repeat("x", 1000))))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, reader)
	reader.Close()
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)) + 10*time.Millisecond)
	f.master.tasks.sampleRates(context.Background(), parent)
	row := f.master.taskView(parent)
	if row["currentSpeedBytesPerSecond"] != float64(200) {
		t.Fatal(row)
	}
	if rate, ok := f.master.tasks.rate(parent, "9-1"); !ok || rate != 200 {
		t.Fatal(rate, ok)
	}
	parent.Generation = 2
	if f.master.taskView(parent)["currentSpeedBytesPerSecond"] != nil {
		t.Fatal("previous generation reused")
	}
	parent.Generation = 1
	offline := task.Node{ID: "offline", EnrollmentID: "offline", Address: "offline.example"}
	if err := f.master.agents.broker.Activate(nodeSession(offline)); err != nil {
		t.Fatal(err)
	}
	parent.Spec.Nodes = append(parent.Spec.Nodes, offline)
	f.master.tasks.mu.Lock()
	delete(f.master.tasks.rates, parent.ID)
	f.master.tasks.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	f.master.tasks.sampleRates(ctx, parent)
	if f.master.taskView(parent)["currentSpeedBytesPerSecond"] != nil {
		t.Fatal("partial rate presented as aggregate")
	}
	parent.Status = "STOPPED"
	if f.master.taskView(parent)["currentSpeedBytesPerSecond"] != float64(0) {
		t.Fatal("stopped task retained speed")
	}
	parent.Status = "RUNNING"
	f.master.tasks.mu.Lock()
	f.master.tasks.rates[parent.ID] = taskRateCache{generation: 1, valid: true, total: 123, observed: time.Now().Add(-11 * time.Second)}
	f.master.tasks.mu.Unlock()
	if f.master.taskView(parent)["currentSpeedBytesPerSecond"] != nil {
		t.Fatal("expired rate presented as current")
	}
}

func TestTaskRateRefreshKeepsLastCompleteSampleUntilCollectionFails(t *testing.T) {
	f := newAgentHTTPFixture(t)
	node := task.Node{ID: "delayed", EnrollmentID: "delayed", Address: "delayed"}
	if err := f.master.agents.broker.Activate(nodeSession(node)); err != nil {
		t.Fatal(err)
	}
	parent := task.Parent{ID: 41, Generation: 1, Status: "RUNNING", Spec: task.Spec{Nodes: []task.Node{node}}}
	f.master.tasks.mu.Lock()
	f.master.tasks.rates[parent.ID] = taskRateCache{generation: 1, valid: true, total: 123, observed: time.Now(), attempted: time.Now().Add(-2 * time.Second)}
	f.master.tasks.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { f.master.tasks.sampleRates(ctx, parent); close(done) }()
	poll, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := f.master.agents.broker.Poll(poll, nodeSession(node)); err != nil {
		t.Fatal(err)
	}
	if got := f.master.taskView(parent)["currentSpeedBytesPerSecond"]; got != float64(123) {
		t.Fatalf("in-flight refresh hid complete sample: %v", got)
	}
	cancel()
	<-done
	if got := f.master.taskView(parent)["currentSpeedBytesPerSecond"]; got != nil {
		t.Fatalf("failed collection retained speed: %v", got)
	}
}
