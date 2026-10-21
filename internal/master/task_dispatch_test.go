package master

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Use real Master/Agent repositories and the command broker; script discovery
// metadata and one delivery failure, without starting an unrelated S3 fixture.
func TestDispatchPagePersistsBeforeDeliveryAndRecoversAnInterruptedPage(t *testing.T) {
	for _, failDelivery := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%v", failDelivery), func(t *testing.T) {
			testDispatchPage(t, failDelivery, "")
		})
	}
}

func TestDispatchPagePersistsValidatedPrefixWhenCollectionFails(t *testing.T) {
	for _, fault := range []string{"rpc", "deadline", "stop-fence"} {
		t.Run(fault, func(t *testing.T) { testDispatchPage(t, false, fault) })
	}
}

func testDispatchPage(t *testing.T, failDelivery bool, scanFault string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "master.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	agentDB, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer agentDB.Close()
	jobs, err := execution.NewRepository(agentDB, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	broker := agenttransport.NewBroker(agenttransport.BrokerOptions{})
	defer broker.Close()
	node := task.Node{ID: "agent", EnrollmentID: "enrollment", Address: "agent.example"}
	if err := broker.Activate(nodeSession(node)); err != nil {
		t.Fatal(err)
	}
	server := &Server{store: db, agents: &agentControl{broker: broker}}
	runtime := newTaskRuntime(server)
	defer runtime.close()
	spec := task.Spec{Direction: "OSS_TO_OSS", Source: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: []task.Node{node}, ConflictPolicy: "SKIP", DistributionMode: "BALANCED"}
	p, _, err := runtime.repo.Create(spec, "dispatch-page")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mu sync.Mutex
	reads, acknowledgements, acknowledgedThrough := 0, 0, int64(0)
	failedDelivery, failedScan := false, false
	dispatchCtx := ctx
	serveDone := make(chan error, 1)
	go func() {
		for {
			command, err := broker.Poll(ctx, nodeSession(node))
			if err != nil {
				serveDone <- err
				return
			}
			if err := broker.Confirm(nodeSession(node), agenttransport.Receipt{RequestID: command.RequestID, DeliveryID: command.DeliveryID}); err != nil {
				serveDone <- err
				return
			}
			status := 200
			var response any = map[string]any{}
			switch command.Operation {
			case "scan_batch":
				var control agentservice.ScanRequest
				err = json.Unmarshal(command.Payload, &control)
				mu.Lock()
				reads++
				mu.Unlock()
				if scanFault != "" && !failedScan && control.Number == 3 {
					failedScan = true
					if scanFault == "deadline" {
						<-dispatchCtx.Done()
						continue // An expired command cannot return a result.
					}
					if scanFault == "stop-fence" {
						_, err = runtime.repo.Stop(p.ID)
					}
					status = 503
					response = map[string]string{"message": "injected scan interruption"}
					break
				}
				found := control.Number < 17
				response = map[string]any{"found": found, "batch": discovery.Batch{ParentID: p.ID, Generation: p.Generation, Number: control.Number + 1, Objects: []transferstore.Object{{Key: fmt.Sprintf("object-%d", control.Number+1), Size: 1024}}}}
			case "child_accept":
				var request agentservice.ChildAcceptRequest
				err = json.Unmarshal(command.Payload, &request)
				if err == nil {
					var saved task.Parent
					saved, err = runtime.repo.Get(p.ID)
					if err == nil && (request.Job.Number == 1 && saved.DispatchedThrough < 1 || request.Job.Number > 1 && saved.DispatchedThrough != 17 && !(scanFault != "" && saved.DispatchedThrough == 3)) {
						err = errors.New("Agent received a job before its complete assignment page was persisted")
					}
				}
				if err == nil && failDelivery && request.Job.Number == 3 && !failedDelivery {
					failedDelivery, status = true, 503
					response = map[string]string{"message": "injected temporary unavailability"}
				} else if err == nil {
					_, err = jobs.Accept(ctx, request.Job)
				}
			case "child_get":
				var request map[string]string
				err = json.Unmarshal(command.Payload, &request)
				if err == nil {
					response, err = jobs.Get(ctx, request["childId"])
					if errors.Is(err, execution.ErrNotFound) {
						status, err = 404, nil
						response = map[string]string{"message": "not accepted"}
					}
				}
			case "scan_ack":
				var control agentservice.ScanRequest
				err = json.Unmarshal(command.Payload, &control)
				mu.Lock()
				acknowledgements++
				acknowledgedThrough = control.Number
				mu.Unlock()
			default:
				err = fmt.Errorf("unexpected operation %s", command.Operation)
			}
			if err != nil {
				serveDone <- err
				return
			}
			payload, err := json.Marshal(response)
			if err == nil {
				err = broker.Complete(nodeSession(node), agenttransport.Result{RequestID: command.RequestID, DeliveryID: command.DeliveryID, Response: agenttransport.Response{Status: status, Payload: payload}})
			}
			if err != nil {
				serveDone <- err
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-serveDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	control := agentservice.ScanRequest{ParentID: p.ID, Generation: p.Generation}
	if err := runtime.dispatch(ctx, p, control); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	firstRead, firstACK, firstThrough := reads, acknowledgements, acknowledgedThrough
	mu.Unlock()
	if firstRead != 1 || firstACK != 1 || firstThrough != 1 {
		t.Fatalf("first transfer waited for a full page: reads=%d acknowledgements=%d through=%d", firstRead, firstACK, firstThrough)
	}
	p, _ = runtime.repo.Get(p.ID)
	if scanFault == "deadline" {
		var endDispatch context.CancelFunc
		dispatchCtx, endDispatch = context.WithTimeout(ctx, time.Second)
		defer endDispatch()
	}
	err = runtime.dispatch(dispatchCtx, p, control)
	if failDelivery && !agentStatus(err, 503) {
		t.Fatal("injected partial-delivery failure was ignored", err)
	}
	if !failDelivery && scanFault == "" && err != nil {
		t.Fatal(err)
	}
	if scanFault == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("scan deadline was ignored", err)
	}
	if (scanFault == "rpc" || scanFault == "stop-fence") && !agentStatus(err, 503) {
		t.Fatal("scan failure was ignored", err)
	}
	if scanFault == "stop-fence" && !errors.Is(err, task.ErrState) {
		t.Fatal("prefix persistence crossed a stop fence", err)
	}
	mu.Lock()
	acksAfterPage, throughAfterPage := acknowledgements, acknowledgedThrough
	mu.Unlock()
	if (failDelivery || scanFault != "") && (acksAfterPage != 1 || throughAfterPage != 1) {
		t.Fatal("partially delivered page was acknowledged as complete")
	}
	if !failDelivery && scanFault == "" && (acksAfterPage != 2 || throughAfterPage != 17) {
		t.Fatalf("sixteen-child page should send one cumulative ACK: calls=%d through=%d", acksAfterPage, throughAfterPage)
	}
	// Reopen only the Master's durable state. The peer retains the accepted
	// jobs, including only the first two when delivery was interrupted.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	server.store, runtime.repo = db, task.New(db)
	p, err = runtime.repo.Get(p.ID)
	wantChildren := 17
	if scanFault != "" {
		wantChildren = 3
	}
	if scanFault == "stop-fence" {
		wantChildren = 1
	}
	if err != nil || p.DispatchedThrough != wantChildren || p.ChildCount != wantChildren {
		t.Fatalf("interrupted dispatch lost assignments or crossed a fence: %+v %v", p, err)
	}
	if scanFault != "" {
		for number := 2; number <= 3; number++ {
			if _, err := jobs.Get(ctx, fmt.Sprintf("%d-%d", p.ID, number)); !errors.Is(err, execution.ErrNotFound) {
				t.Fatalf("collection error still delivered child %d: %v", number, err)
			}
		}
	}
	if scanFault == "stop-fence" {
		if p.Status != "STOPPING" {
			t.Fatal("stop fence was lost", p.Status)
		}
		return
	}
	if err := runtime.observe(ctx, p, 64); err != nil {
		t.Fatal(err)
	}
	for number := 1; number <= wantChildren; number++ {
		if _, err := jobs.Get(ctx, fmt.Sprintf("%d-%d", p.ID, number)); err != nil {
			t.Fatalf("assigned child %d was not recovered: %v", number, err)
		}
	}
	if scanFault != "" {
		if err := runtime.dispatch(ctx, p, control); err != nil {
			t.Fatal("next collection could not advance beyond its saved prefix", err)
		}
		p, err = runtime.repo.Get(p.ID)
		if err != nil || p.DispatchedThrough != 17 {
			t.Fatal("next collection did not make progress", p.DispatchedThrough, err)
		}
	}
}
