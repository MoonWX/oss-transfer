package agentservice

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

// comparisonS3 serves LIST (one page) and HEAD/GET for "bucket/key" objects.
type comparisonS3 struct {
	server  *httptest.Server
	objects map[string]string
	mu      sync.Mutex
	gate    chan struct{} // when set, LIST waits for it or the request's end
	deny    atomic.Bool   // LIST answers 403 AccessDenied
}

func newComparisonS3(t *testing.T, objects map[string]string) *comparisonS3 {
	f := &comparisonS3{objects: objects}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() {
		f.release()
		f.server.Close()
	})
	return f
}

func (f *comparisonS3) block() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = make(chan struct{})
}

func (f *comparisonS3) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate != nil {
		close(f.gate)
		f.gate = nil
	}
}

func (f *comparisonS3) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if r.Method == http.MethodGet && key == "" {
		if f.deny.Load() {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>Access Denied</Message><RequestId>FIXTURE</RequestId></Error>`)
			return
		}
		f.mu.Lock()
		gate := f.gate
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		type entry struct {
			Key          string `xml:"Key"`
			Size         int    `xml:"Size"`
			ETag         string `xml:"ETag"`
			LastModified string `xml:"LastModified"`
		}
		var result struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Name     string   `xml:"Name"`
			Contents []entry  `xml:"Contents"`
		}
		result.Name = bucket
		prefix, after := r.URL.Query().Get("prefix"), r.URL.Query().Get("start-after")
		for name, data := range f.objects {
			if object, ok := strings.CutPrefix(name, bucket+"/"); ok && strings.HasPrefix(object, prefix) && object > after {
				result.Contents = append(result.Contents, entry{object, len(data), `"fixture"`, "2026-01-01T00:00:00Z"})
			}
		}
		sort.Slice(result.Contents, func(i, j int) bool { return result.Contents[i].Key < result.Contents[j].Key })
		w.Header().Set("Content-Type", "application/xml")
		_ = xml.NewEncoder(w).Encode(result)
		return
	}
	data, found := f.objects[bucket+"/"+key]
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("ETag", `"fixture"`)
	w.Header().Set("Last-Modified", "Thu, 01 Jan 2026 00:00:00 GMT")
	w.Header().Set("Content-Type", "application/octet-stream")
	if r.Method == http.MethodGet {
		_, _ = w.Write([]byte(data))
	}
}

func comparisonSnapshotAt(revision uint64, endpoint string) storageconfig.Snapshot {
	pathStyle := true
	return storageconfig.Snapshot{Revision: revision, Connections: []storageconfig.Config{{Alias: "test", Endpoint: endpoint, AccessKey: "fixture-ak", SecretKey: "fixture-sk", Region: "test", PathStyleEnable: &pathStyle}}}
}

func openComparisonService(t *testing.T, db *state.Store, directory string, updater softwareupdate.Controller) *Service {
	t.Helper()
	s, err := New(db, Options{SpoolDirectory: t.TempDir(), Updater: updater, Comparison: comparison.Options{Directory: directory, Logf: func(string, ...any) {}, FreeBytes: func(string) (uint64, error) { return 1 << 50, nil }}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func comparisonCall(t *testing.T, s *Service, operation string, payload any, mutating bool) (int, map[string]any) {
	t.Helper()
	raw, ok := payload.(string)
	if !ok {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		raw = string(data)
	}
	reply, err := s.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: operation, Payload: json.RawMessage(raw), Mutating: mutating}})
	if err != nil {
		t.Fatal(operation, err)
	}
	var body map[string]any
	if len(reply.Payload) > 0 {
		if err := json.Unmarshal(reply.Payload, &body); err != nil {
			t.Fatal(operation, string(reply.Payload), err)
		}
	}
	return reply.Status, body
}

func comparisonRequestFor(id string, revision uint64) comparison.Request {
	return comparison.Request{ID: id, StorageRevision: revision, Source: execution.Endpoint{Kind: "S3", Ref: "test", Bucket: "source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "test", Bucket: "target"}, SourcePrefix: "2026/", DestinationPrefix: "backup/", Mode: "metadata", Scope: "prefix"}
}

func waitComparison(t *testing.T, s *Service, id string, done func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute) // hang guard only
	for {
		status, body := comparisonCall(t, s, "comparison_get", map[string]any{"id": id, "limit": 10}, false)
		if status != 200 {
			t.Fatal(id, status, body)
		}
		if done(body) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("comparison %s stuck: %+v", id, body)
		}
		time.Sleep(time.Millisecond)
	}
}

func hasStatus(statuses ...string) func(map[string]any) bool {
	return func(body map[string]any) bool {
		for _, status := range statuses {
			if body["status"] == status {
				return true
			}
		}
		return false
	}
}

var comparisonFixtureObjects = map[string]string{"source/2026/a": "AAAA", "source/2026/b": "same", "target/backup/b": "same", "target/backup/c": "x"}

func TestComparisonCommandsOverTheAgentChannel(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s3 := newComparisonS3(t, comparisonFixtureObjects)
	s := openComparisonService(t, db, filepath.Join(t.TempDir(), "comparisons"), nil)
	if s.LeaseCapabilities()["comparisonProtocol"] != comparison.Protocol {
		t.Fatalf("capability missing: %+v", s.LeaseCapabilities())
	}
	if _, err := s.Configs.ApplySnapshot(comparisonSnapshotAt(1, s3.server.URL)); err != nil {
		t.Fatal(err)
	}
	expect := func(status int, body map[string]any, wantStatus int, wantMessage string) {
		t.Helper()
		if status != wantStatus || wantMessage != "" && body["message"] != wantMessage {
			t.Fatalf("got %d %+v, want %d %q", status, body, wantStatus, wantMessage)
		}
	}
	first := comparisonRequestFor("cmp-1", 1)
	status, body := comparisonCall(t, s, "comparison_start", first, false)
	expect(status, body, 400, "operation requires a mutating command")
	stale := comparisonRequestFor("cmp-1", 2)
	status, body = comparisonCall(t, s, "comparison_start", stale, true)
	expect(status, body, 409, "")
	invalid := first
	invalid.Mode = "checksum"
	status, body = comparisonCall(t, s, "comparison_start", invalid, true)
	expect(status, body, 400, "")
	status, body = comparisonCall(t, s, "comparison_start", `{"id":"cmp-1","unknown":true}`, true)
	expect(status, body, 400, "")
	status, body = comparisonCall(t, s, "comparison_start", first, true)
	expect(status, body, 200, "")
	done := waitComparison(t, s, "cmp-1", hasStatus(comparison.StateCompleted))
	if done["compared"] != float64(3) || done["matched"] != float64(1) || done["missing"] != float64(1) || done["extra"] != float64(1) || done["rows"] != float64(2) || done["resultsProtocol"] != float64(comparison.Protocol) {
		t.Fatalf("wrong comparison: %+v", done)
	}
	if encoded, _ := json.Marshal(done); strings.Contains(string(encoded), "fixture-sk") || strings.Contains(string(encoded), s3.server.URL) {
		t.Fatal("comparison disclosed connection settings")
	}
	status, body = comparisonCall(t, s, "comparison_rows", comparison.RowsQuery{ID: "cmp-1", Limit: 10, Statuses: []string{comparison.StatusExtraTarget}}, false)
	if items, _ := body["items"].([]any); status != 200 || len(items) != 1 || items[0].(map[string]any)["key"] != "c" {
		t.Fatalf("filtered rows: %d %+v", status, body)
	}
	status, body = comparisonCall(t, s, "comparison_rows", comparison.RowsQuery{ID: "cmp-1", Limit: 10, Statuses: []string{"NOPE"}}, false)
	expect(status, body, 400, "")
	status, body = comparisonCall(t, s, "comparison_rows", comparison.RowsQuery{ID: "missing", Limit: 10}, false)
	expect(status, body, 404, "Comparison not found")
	status, body = comparisonCall(t, s, "comparison_get", map[string]any{"id": "cmp-1", "limit": 0}, false)
	expect(status, body, 400, "")
	for _, payload := range []string{`{}`, ``} {
		status, body = comparisonCall(t, s, "comparison_list", payload, false)
		if items, _ := body["items"].([]any); status != 200 || len(items) != 1 || items[0].(map[string]any)["id"] != "cmp-1" {
			t.Fatalf("list: %d %+v", status, body)
		}
	}
	status, body = comparisonCall(t, s, "comparison_list", `{"id":"x"}`, false)
	expect(status, body, 400, "")
	resume := comparison.ResumeRequest{ID: "cmp-1", StorageRevision: 1}
	status, body = comparisonCall(t, s, "comparison_resume", resume, false)
	expect(status, body, 400, "operation requires a mutating command")
	status, body = comparisonCall(t, s, "comparison_resume", resume, true)
	expect(status, body, 409, "Comparison cannot be resumed")
	status, body = comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{ID: "missing", StorageRevision: 1}, true)
	expect(status, body, 404, "Comparison not found")
	status, body = comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{ID: "cmp-1", StorageRevision: 7}, true)
	expect(status, body, 409, storageconfig.ErrStaleRevision.Error())
	status, body = comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{StorageRevision: 1}, true)
	expect(status, body, 400, "")

	// A running comparison: admission, deletion and cancellation.
	s3.block()
	second := comparisonRequestFor("cmp-2", 1)
	status, body = comparisonCall(t, s, "comparison_start", second, true)
	if status != 200 || body["status"] != comparison.StateRunning {
		t.Fatal(status, body)
	}
	status, body = comparisonCall(t, s, "comparison_start", comparisonRequestFor("cmp-3", 1), true)
	expect(status, body, 429, "Another comparison is running on this node")
	status, body = comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{ID: "cmp-2", StorageRevision: 1}, true)
	if status != 200 || body["status"] != comparison.StateRunning {
		t.Fatalf("replayed resume of the running comparison: %d %+v", status, body)
	}
	status, body = comparisonCall(t, s, "comparison_delete", map[string]any{"id": "cmp-2"}, true)
	expect(status, body, 409, "Comparison is running; cancel it before deleting")
	status, body = comparisonCall(t, s, "comparison_cancel", map[string]any{"id": "cmp-2"}, true)
	if status != 200 || body["status"] != comparison.StateCanceling {
		t.Fatal(status, body)
	}
	canceled := waitComparison(t, s, "cmp-2", hasStatus(comparison.StateCanceled))
	if canceled["resumable"] != true {
		t.Fatalf("canceled comparison not resumable: %+v", canceled)
	}
	s3.release()
	status, body = comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{ID: "cmp-2", StorageRevision: 1}, true)
	if status != 200 || body["status"] != comparison.StateRunning || body["resumes"] != float64(1) {
		t.Fatal(status, body)
	}
	resumed := waitComparison(t, s, "cmp-2", hasStatus(comparison.StateCompleted))
	if resumed["compared"] != float64(3) || resumed["rows"] != float64(2) {
		t.Fatalf("resumed comparison: %+v", resumed)
	}
	status, body = comparisonCall(t, s, "comparison_delete", map[string]any{"id": "cmp-1"}, true)
	if status != 200 || body["success"] != true {
		t.Fatal(status, body)
	}
	status, body = comparisonCall(t, s, "comparison_get", map[string]any{"id": "cmp-1", "limit": 1}, false)
	expect(status, body, 404, "Comparison not found")
	status, body = comparisonCall(t, s, "comparison_delete", map[string]any{"id": "cmp-1"}, true)
	expect(status, body, 200, "")

	// A failed LIST is reported with the fixed sentence and can be resumed.
	s3.deny.Store(true)
	status, body = comparisonCall(t, s, "comparison_start", comparisonRequestFor("cmp-4", 1), true)
	expect(status, body, 200, "")
	failed := waitComparison(t, s, "cmp-4", hasStatus(comparison.StateFailed))
	if failed["error"] != "Comparison could not finish; check storage access and retry." || failed["resumable"] != true {
		t.Fatalf("failed comparison: %+v", failed)
	}
	s3.deny.Store(false)
	status, body = comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{ID: "cmp-4", StorageRevision: 1}, true)
	expect(status, body, 200, "")
	if finished := waitComparison(t, s, "cmp-4", hasStatus(comparison.StateCompleted)); finished["compared"] != float64(3) || finished["error"] != nil || finished["errorDetail"] != nil {
		t.Fatalf("resumed after failure: %+v", finished)
	}
	if !s.ReplaySafe("comparison_resume") || !s.ReplaySafe("comparison_start") {
		t.Fatal("comparison commands must reconcile lost replies by ID")
	}
}

func TestComparisonAutoResumeRequiresTheSameStorageRevision(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	directory := filepath.Join(t.TempDir(), "comparisons")
	s3 := newComparisonS3(t, comparisonFixtureObjects)
	interrupt := func(id string, revision uint64) {
		t.Helper()
		s := openComparisonService(t, db, directory, nil)
		if _, err := s.Configs.ApplySnapshot(comparisonSnapshotAt(revision, s3.server.URL)); err != nil {
			t.Fatal(err)
		}
		s3.block()
		if status, body := comparisonCall(t, s, "comparison_start", comparisonRequestFor(id, revision), true); status != 200 {
			t.Fatal(status, body)
		}
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		s3.release()
	}
	interrupt("same", 1)
	s := openComparisonService(t, db, directory, nil)
	if done := waitComparison(t, s, "same", hasStatus(comparison.StateCompleted, comparison.StateFailed)); done["status"] != comparison.StateCompleted || done["resumes"] != float64(1) || done["compared"] != float64(3) {
		t.Fatalf("not resumed after restart: %+v", done)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	interrupt("changed", 1)
	// Storage connections change while the Agent is stopped.
	if _, err := storageconfig.NewAgent(db).ApplySnapshot(comparisonSnapshotAt(2, s3.server.URL)); err != nil {
		t.Fatal(err)
	}
	s = openComparisonService(t, db, directory, nil)
	status, body := comparisonCall(t, s, "comparison_get", map[string]any{"id": "changed", "limit": 1}, false)
	if status != 200 || body["status"] != comparison.StateInterrupted || body["errorDetail"] != errComparisonStorageChanged.Error() || body["resumable"] != true {
		t.Fatalf("changed storage resumed automatically: %d %+v", status, body)
	}
	status, body = comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{ID: "changed", StorageRevision: 2}, true)
	if status != 200 {
		t.Fatal(status, body)
	}
	if done := waitComparison(t, s, "changed", hasStatus(comparison.StateCompleted, comparison.StateFailed)); done["status"] != comparison.StateCompleted || done["compared"] != float64(3) {
		t.Fatalf("manual resume: %+v", done)
	}
}

func TestComparisonResumeIsFencedDuringUpdates(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	updater := &updateController{status: softwareupdate.Status{Phase: "DOWNLOADING"}}
	s := openComparisonService(t, db, filepath.Join(t.TempDir(), "comparisons"), updater)
	if !softwareupdate.StartsWork("comparison_resume") {
		t.Fatal("comparison_resume does not start work")
	}
	status, body := comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{ID: "any", StorageRevision: 1}, true)
	if status != 409 || body["code"] != "updating" {
		t.Fatalf("resume during an update: %d %+v", status, body)
	}
	for _, operation := range []string{"comparison_list", "comparison_get", "comparison_rows"} {
		if status, _ := comparisonCall(t, s, operation, map[string]any{"id": "any", "limit": 1}, false); status == 409 {
			t.Fatalf("%s fenced during an update", operation)
		}
	}
}

func TestComparisonListFitsTheAgentChannel(t *testing.T) {
	small := []comparison.Progress{{Request: comparisonRequestFor("a", 1), Checkpoint: "k", LastStoredKey: "k", ErrorDetail: "detail"}}
	if got := fitComparisonList(small); got.Items[0].Checkpoint != "k" || got.Items[0].ErrorDetail != "detail" {
		t.Fatalf("small list trimmed: %+v", got)
	}
	long := strings.Repeat("<", 1024) // JSON escapes each as <
	var items []comparison.Progress
	for i := 0; i < comparison.DefaultMaxRetained; i++ {
		items = append(items, comparison.Progress{Request: comparisonRequestFor(fmt.Sprintf("cmp-%03d", i), 1), Status: comparison.StateFailed, Checkpoint: long, LastStoredKey: long, ErrorDetail: long[:512]})
	}
	got := fitComparisonList(items)
	encoded, err := json.Marshal(got)
	if err != nil || len(encoded) > comparisonListBudget || len(got.Items) != len(items) {
		t.Fatalf("list of %d bytes, %d items: %v", len(encoded), len(got.Items), err)
	}
	if got.Items[0].ID != "cmp-000" || got.Items[0].Status != comparison.StateFailed || got.Items[0].Checkpoint != "" || items[0].Checkpoint != long {
		t.Fatalf("trimmed wrong fields: %+v", got.Items[0])
	}
}
