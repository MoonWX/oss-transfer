package master

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// A stop discards the bytes of objects in transfer. The task row says how many
// objects are in transfer and how much of them was sent, from the same
// samples as the live bytes, so a stop confirmation can name what restarts.
// Objects an Agent completed but the Master has not credited yet are not in
// transfer: they keep their results.
func TestTaskRowCountsObjectsInFlight(t *testing.T) {
	f, p := progressMasterFixture(t, 1)
	first := assignProgressChild(t, f, p, 1, []transferstore.Object{{Key: "a", Size: 8 << 30}, {Key: "b", Size: 8 << 30}})
	second := assignProgressChild(t, f, p, 2, []transferstore.Object{{Key: "c", Size: 1 << 20}, {Key: "d", Size: 4 << 30}})
	completed := childProgressSample(second, 1, 1<<30)
	completed.SucceededBytes = 1 << 20 // "c" finished on the Agent, not yet observed
	serveProgressAgent(t, f, first.Node, func(command agenttransport.Command) (any, error) {
		if command.Operation != "task_metrics" {
			return nil, fmt.Errorf("unexpected operation %s", command.Operation)
		}
		return progressMetrics(p, childProgressSample(first, 2, 3<<30), completed), nil
	})
	f.master.tasks.sampleRates(context.Background(), p)
	row := f.master.taskView(p)
	if row["liveProgress"] != "LIVE" || row["inFlightObjectCount"] != 3 || row["inFlightObjectBytes"] != int64(4<<30) {
		t.Fatalf("objects in flight: %v %v %v", row["liveProgress"], row["inFlightObjectCount"], row["inFlightObjectBytes"])
	}
	if row["inFlightBytes"] != int64(4<<30+1<<20) {
		t.Fatalf("uncredited bytes changed meaning: %v", row["inFlightBytes"])
	}
	// Child rows carry no such count.
	if child := f.master.childView(p, first); child["inFlightObjectCount"] != nil {
		t.Fatalf("child row: %v", child["inFlightObjectCount"])
	}
	// A task exported with these fields can be imported again: like the other
	// progress fields they are dropped, not rejected as unknown.
	base, _ := json.Marshal(ossTaskRequest())
	var exported map[string]any
	_ = json.Unmarshal(base, &exported)
	exported["inFlightObjectCount"], exported["inFlightObjectBytes"] = row["inFlightObjectCount"], row["inFlightObjectBytes"]
	encoded, _ := json.Marshal([]any{exported})
	response := httptest.NewRecorder()
	if rows, ok := readTaskRows(response, httptest.NewRequest("POST", "/api/master/import", bytes.NewReader(encoded))); !ok || len(rows) != 1 {
		t.Fatal("an exported row with objects in flight was rejected", response.Body.String())
	}
}
