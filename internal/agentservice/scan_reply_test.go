package agentservice

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// pausedLocalFactory opens LOCAL sources whose scan stops after its first
// page until resumed.
type pausedLocalFactory struct {
	paused, resume chan struct{}
}

type pausedLocal struct {
	*transferstore.Local
	factory pausedLocalFactory
}

func (f pausedLocalFactory) OpenEndpoint(_ context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	local, err := transferstore.NewLocal(endpoint.Root)
	if err != nil {
		return nil, err
	}
	return &pausedLocal{Local: local, factory: f}, nil
}

func (s *pausedLocal) ScanPages(ctx context.Context, options transferstore.ListOptions, emit func(transferstore.Page) error) error {
	first := true
	return s.Local.ScanPages(ctx, options, func(page transferstore.Page) error {
		if err := emit(page); err != nil {
			return err
		}
		if first {
			first = false
			close(s.factory.paused)
			select {
			case <-s.factory.resume:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	})
}

// scan_start and scan_status reply with the scan protocol and, while the
// attempt runs, its activity; every scan reply keeps the release protocol.
func TestScanRepliesCarryTheScanProtocolAndActivity(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	factory := pausedLocalFactory{paused: make(chan struct{}), resume: make(chan struct{})}
	scanner, err := discovery.New(db, factory, discovery.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute) // hang guard only
	defer cancel()
	service := &Service{Scanner: scanner}
	call := func(operation string, request any) map[string]json.RawMessage {
		t.Helper()
		payload, _ := json.Marshal(request)
		reply, err := service.Execute(ctx, agenttransport.Command{Request: agenttransport.Request{Operation: operation, Mutating: true, Payload: payload}})
		if err != nil || reply.Status != 200 {
			t.Fatalf("%s: %+v %v", operation, reply, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(reply.Payload, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}
	spec := discovery.Spec{ParentID: 1, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: root}}
	call("scan_start", spec)
	<-factory.paused
	status := ScanRequest{ParentID: 1, Generation: 1}
	for _, operation := range []string{"scan_start", "scan_status"} {
		request := any(status)
		if operation == "scan_start" {
			request = spec
		}
		fields := call(operation, request)
		var activity transferstore.ScanActivitySnapshot
		if string(fields["releaseProtocol"]) != "1" || string(fields["scanProtocol"]) != strconv.Itoa(discovery.ScanProtocol) || json.Unmarshal(fields["activity"], &activity) != nil ||
			activity.Mode != transferstore.ScanModeSorted || activity.NamesRead != 3 || activity.Examined != 3 || string(fields["status"]) != `"SCANNING"` {
			t.Fatalf("%s reply while scanning: %s", operation, fields)
		}
	}
	close(factory.resume)
	for {
		fields := call("scan_status", status)
		if string(fields["status"]) == `"COMPLETED"` {
			if _, found := fields["activity"]; found || string(fields["scanProtocol"]) != strconv.Itoa(discovery.ScanProtocol) || string(fields["releaseProtocol"]) != "1" {
				t.Fatalf("reply after the scan: %s", fields)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if fields := call("scan_ack", ScanRequest{ParentID: 1, Generation: 1, Number: 1}); string(fields["releaseProtocol"]) != "1" {
		t.Fatalf("acknowledgement lost the release protocol: %s", fields)
	}
}
