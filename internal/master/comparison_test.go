package master

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// comparisonS3 serves ListObjectsV2 (two keys per page), HEAD and GET for
// fixed objects. LIST requests can be held to keep a comparison running.
type comparisonS3 struct {
	*httptest.Server
	downloads    atomic.Int64
	blockLists   atomic.Bool
	listStarted  chan struct{}
	releaseLists chan struct{}
	release      sync.Once
}

func newComparisonS3(t *testing.T, objects map[string]string) *comparisonS3 {
	t.Helper()
	s := &comparisonS3{listStarted: make(chan struct{}, 1), releaseLists: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		bucket, object, _ := strings.Cut(key, "/")
		if r.Method == "GET" && object == "" {
			if s.blockLists.Load() {
				select {
				case s.listStarted <- struct{}{}:
				default:
				}
				select {
				case <-s.releaseLists:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/xml")
			type entry struct {
				Key          string `xml:"Key"`
				Size         int    `xml:"Size"`
				ETag         string `xml:"ETag"`
				LastModified string `xml:"LastModified"`
			}
			var result struct {
				XMLName               xml.Name `xml:"ListBucketResult"`
				Name                  string   `xml:"Name"`
				IsTruncated           bool     `xml:"IsTruncated"`
				NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
				Contents              []entry  `xml:"Contents"`
			}
			result.Name = bucket
			for k, v := range objects {
				if strings.HasPrefix(k, bucket+"/") {
					relative := strings.TrimPrefix(k, bucket+"/")
					if strings.HasPrefix(relative, r.URL.Query().Get("prefix")) && relative > r.URL.Query().Get("start-after") {
						result.Contents = append(result.Contents, entry{relative, len(v), `"opaque"`, "2026-01-01T00:00:00Z"})
					}
				}
			}
			sort.Slice(result.Contents, func(i, j int) bool { return result.Contents[i].Key < result.Contents[j].Key })
			start, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))
			end := min(start+2, len(result.Contents))
			if end < len(result.Contents) {
				result.IsTruncated = true
				result.NextContinuationToken = strconv.Itoa(end)
			}
			result.Contents = result.Contents[start:end]
			_ = xml.NewEncoder(w).Encode(result)
			return
		}
		data, found := objects[key]
		if !found {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("ETag", `"opaque"`)
		w.Header().Set("Last-Modified", "Thu, 01 Jan 2026 00:00:00 GMT")
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Method == "GET" {
			s.downloads.Add(1)
			_, _ = w.Write([]byte(data))
		}
	}))
	t.Cleanup(func() {
		s.unblock()
		s.Close()
	})
	return s
}

// unblock lets held and future LIST requests proceed.
func (s *comparisonS3) unblock() {
	s.blockLists.Store(false)
	s.release.Do(func() { close(s.releaseLists) })
}

// comparisonObjects holds one different, one equal, one missing and one
// extra object under source/2026/ and target/backup/.
func comparisonObjects() map[string]string {
	return map[string]string{"source/2026/a": "AAAA", "target/backup/a": "BBBB", "source/2026/missing": "source", "target/backup/extra": "target", "source/2026/a +?中文": "same", "target/backup/a +?中文": "same"}
}

// waitComparison polls a comparison until its status leaves the given ones.
func waitComparison(t *testing.T, f *agentHTTPFixture, id string, while ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute) // hang guard only
	for {
		code, body := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/comparisons/%s?limit=2", f.http.URL, id), f.admin, nil)
		if code != 200 {
			t.Fatal(code, body)
		}
		if !slices.Contains(while, body["status"].(string)) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatal("comparison did not leave", while, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func listedComparison(t *testing.T, f *agentHTTPFixture, id string) (map[string]any, map[string]any) {
	t.Helper()
	code, body := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/comparisons", f.admin, nil)
	if code != 200 {
		t.Fatal(code, body)
	}
	for _, raw := range body["items"].([]any) {
		if item := raw.(map[string]any); item["id"] == id {
			return item, body
		}
	}
	t.Fatal("comparison is not listed", id, body)
	return nil, nil
}

// checkComparisonExports checks the ZIP and JSONL exports of the comparison
// of comparisonObjects.
func checkComparisonExports(t *testing.T, f *agentHTTPFixture, id string) {
	t.Helper()
	names, files := comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/"+id+"/export", f.admin, ""))
	rows := differenceRows(t, files["differences.csv"])
	if !slices.Equal(names, []string{"differences.csv", "retry-source-keys.incr", "summary.json"}) || len(rows) != 3 {
		t.Fatal("export entries", names, rows)
	}
	statuses := []string{rows[0]["status"], rows[1]["status"], rows[2]["status"]}
	if !slices.Equal(statuses, []string{"DIFFERENT", "EXTRA_TARGET", "MISSING_TARGET"}) || rows[0]["sourceSha256"] == "" || rows[0]["sourceSha256"] == rows[0]["destinationSha256"] {
		t.Fatal("exported rows", rows)
	}
	if string(files["retry-source-keys.incr"]) != "2026/a\n2026/missing\n" {
		t.Fatalf("retry keys %q", files["retry-source-keys.incr"])
	}
	summary := exportSummary(t, files["summary.json"])
	if !summary.Complete || summary.Status != "COMPLETED" || summary.ExportedRows != 3 || summary.StoredRows != 3 || summary.Counts.Compared != 4 || summary.Counts.Matched != 1 || summary.RetryKeys != 2 || summary.RetryPrefixesMatch || summary.Source != (comparisonExportEndpoint{Connection: "test", Bucket: "source", Prefix: "2026/"}) {
		t.Fatalf("summary %+v", summary)
	}
	download := mustDownloadComparison(t, f, "GET", "/api/master/comparisons/"+id+"/export?format=jsonl&status=EXTRA_TARGET", f.admin, "")
	var row comparison.Row
	if download.status != 200 || json.Unmarshal(download.body, &row) != nil || row.DestinationKey != "backup/extra" || strings.Count(string(download.body), "\n") != 1 {
		t.Fatal("JSONL export", download.status, string(download.body))
	}
}

// This crosses the real API, reverse Agent channel, S3 SDK LIST/HEAD/GET and
// durable comparison results. The local HTTP fixture supplies only S3 storage.
// The bare Agent service does not answer "capabilities", so the Master uses
// the operations of Agents before comparison.Protocol here.
func TestComparisonHTTPAgentStreamingAndCredentialBoundary(t *testing.T) {
	s3 := newComparisonS3(t, comparisonObjects())
	f := newAgentHTTPFixture(t)
	agent, _ := taskAPIAgent(t, f)
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, map[string]any{"alias": "test", "endpoint": s3.URL, "accessKey": "fixture-private-ak", "secretKey": "fixture-private-sk", "region": "test", "pathStyleEnable": true, "validate": false})
	if code != 200 {
		t.Fatal(code, body)
	}
	create := map[string]any{"sourceBucket": "test/source", "destBucket": "test/target", "sourcePrefix": "2026/", "destPrefix": "backup/", "scope": "prefix", "mode": "content"}
	if code, _ = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons", "", create); code != 401 {
		t.Fatal("comparison did not require administrator", code)
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons", f.admin, create)
	if code != 202 {
		t.Fatal(code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatal("comparison ID missing")
	}
	body = waitComparison(t, f, id, "RUNNING", "CANCELING")
	if body["status"] != "COMPLETED" || body["different"] != float64(1) || body["missing"] != float64(1) || body["extra"] != float64(1) || body["matched"] != float64(1) || s3.downloads.Load() != 4 {
		t.Fatalf("wrong real S3 comparison: %+v downloads=%d", body, s3.downloads.Load())
	}
	if len(body["items"].([]any)) != 2 || body["nextAfter"] != float64(2) {
		t.Fatal("pagination missing", body)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "fixture-private") {
		t.Fatal("comparison disclosed credentials")
	}
	code, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/comparisons/%s?after=2&limit=2", f.http.URL, id), f.admin, nil)
	if code != 200 || len(body["items"].([]any)) != 1 {
		t.Fatal("next page lost", code, body)
	}
	// The Master filters rows of Agents without comparison_rows.
	code, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/comparisons/%s?status=MISSING_TARGET", f.http.URL, id), f.admin, nil)
	if items := body["items"].([]any); code != 200 || len(items) != 1 || items[0].(map[string]any)["sourceKey"] != "2026/missing" || body["nextAfter"] != nil {
		t.Fatal("filtered page", code, body)
	}
	item, list := listedComparison(t, f, id)
	if item["status"] != "COMPLETED" || item["nodeName"] != "QA node (127.0.0.1)" || item["compared"] != float64(4) || list["complete"] != true {
		t.Fatal("list", list)
	}
	checkComparisonExports(t, f, id)
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons/"+id+"/resume", f.admin, nil)
	if code != 409 || body["message"] != "Update this node to resume comparisons" {
		t.Fatal("resume on an Agent without the protocol", code, body)
	}
	// Agent retention removes the result independently of the Master mapping.
	if err := agent.Comparisons.Delete(id); err != nil {
		t.Fatal(err)
	}
	code, _ = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/comparisons/"+id, f.admin, nil)
	if code != 404 {
		t.Fatal("expired result was not reported", code)
	}
	countAssignments := func() int {
		t.Helper()
		count := 0
		if err := f.db.View(func(tx *state.Tx) error {
			return tx.Scan("comparison_assignments", "", "", 10, func(_ string, _ json.RawMessage) error { count++; return nil })
		}); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if countAssignments() != 0 {
		t.Fatal("expired result left its Master mapping")
	}
	s3.blockLists.Store(true)
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons", f.admin, create)
	if code != 202 {
		t.Fatal(code, body)
	}
	running := body["id"].(string)
	select {
	case <-s3.listStarted:
	case <-time.After(30 * time.Second): // hang guard only
		t.Fatal("comparison did not start its S3 scan")
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons", f.admin, create)
	if code != 503 || countAssignments() != 1 {
		t.Fatal("busy Agent retained a mapping for work it rejected", code, body)
	}
	code, body = request(t, f.http.Client(), "DELETE", f.http.URL+"/api/master/comparisons/"+running, f.admin, nil)
	if code != 409 || body["message"] != "Comparison is running; cancel it before deleting" || countAssignments() != 1 {
		t.Fatal("running comparison delete", code, body)
	}
	if item, _ := listedComparison(t, f, running); item["status"] != "RUNNING" {
		t.Fatal("running comparison list item", item)
	}
	s3.unblock()
}

// recordingComparisonAgent is the real Agent service answering
// "capabilities" as the Agent process does, by merging LeaseCapabilities.
type recordingComparisonAgent struct {
	*agentservice.Service
	mu         sync.Mutex
	operations []string
}

func (a *recordingComparisonAgent) Execute(ctx context.Context, c agenttransport.Command) (agenttransport.Response, error) {
	a.mu.Lock()
	a.operations = append(a.operations, c.Operation)
	a.mu.Unlock()
	if c.Operation == "capabilities" {
		raw, err := json.Marshal(a.LeaseCapabilities())
		return agenttransport.Response{Status: 200, Payload: raw}, err
	}
	return a.Service.Execute(ctx, c)
}

func (a *recordingComparisonAgent) used(operation string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Contains(a.operations, operation)
}

// This needs Agents with comparison.Protocol (file-backed results with
// comparison_rows, comparison_list and comparison_resume): list, filtered
// pages and export use the newer operations, and a canceled comparison is
// resumed to the same result as an uninterrupted one.
func TestComparisonHTTPRealAgentProtocolListExportAndResume(t *testing.T) {
	s3 := newComparisonS3(t, comparisonObjects())
	f := newAgentHTTPFixture(t)
	credential := agentCredential(t)
	code, session := f.join(t, f.invite(t, "protocol.example"), "protocol-agent", credential)
	if code != 200 {
		t.Fatal(code)
	}
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentservice.New(db, agentservice.Options{})
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	agent := &recordingComparisonAgent{Service: service}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- agenttransport.NewRunner(agentClient(t, f, credential), agent, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond}).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second): // hang guard only
			t.Error("Agent runner did not stop")
		}
		_ = service.Close(context.Background())
		_ = db.Close()
	})
	waitOnline(t, f, nodeFromSession(session))
	protocol, err := newComparisonAPI(f.master).comparisonProtocol(context.Background(), session)
	if err != nil || protocol < comparison.Protocol {
		t.Fatalf("the Agent advertises comparisonProtocol %d (%v); this test needs Agents with comparison.Protocol %d", protocol, err, comparison.Protocol)
	}
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, map[string]any{"alias": "test", "endpoint": s3.URL, "accessKey": "fixture-private-ak", "secretKey": "fixture-private-sk", "region": "test", "pathStyleEnable": true, "validate": false})
	if code != 200 {
		t.Fatal(code, body)
	}
	create := map[string]any{"sourceBucket": "test/source", "destBucket": "test/target", "sourcePrefix": "2026/", "destPrefix": "backup/", "scope": "prefix", "mode": "content"}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons", f.admin, create)
	if code != 202 || body["resultsProtocol"] != float64(comparison.Protocol) {
		t.Fatal(code, body)
	}
	id := body["id"].(string)
	body = waitComparison(t, f, id, "RUNNING", "CANCELING")
	if body["status"] != "COMPLETED" || body["different"] != float64(1) || body["missing"] != float64(1) || body["extra"] != float64(1) || body["matched"] != float64(1) || body["rows"] != float64(3) {
		t.Fatal("comparison result", body)
	}
	code, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/comparisons/%s?status=MISSING_TARGET,DIFFERENT&limit=1", f.http.URL, id), f.admin, nil)
	items := body["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["status"] != "DIFFERENT" || body["nextAfter"] == nil {
		t.Fatal("filtered page", code, body)
	}
	code, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/comparisons/%s?status=MISSING_TARGET,DIFFERENT&after=%v", f.http.URL, id, body["nextAfter"]), f.admin, nil)
	items = body["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["sourceKey"] != "2026/missing" || body["nextAfter"] != nil {
		t.Fatal("filtered continuation", code, body)
	}
	if item, list := listedComparison(t, f, id); item["status"] != "COMPLETED" || item["resultsProtocol"] != float64(comparison.Protocol) || list["complete"] != true {
		t.Fatal("list", list)
	}
	checkComparisonExports(t, f, id)
	for _, operation := range []string{"comparison_rows", "comparison_list"} {
		if !agent.used(operation) {
			t.Fatal("the Master did not use", operation)
		}
	}
	// A canceled comparison resumes from its checkpoint to the same result.
	s3.blockLists.Store(true)
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons", f.admin, create)
	if code != 202 {
		t.Fatal(code, body)
	}
	resumed := body["id"].(string)
	select {
	case <-s3.listStarted:
	case <-time.After(30 * time.Second): // hang guard only
		t.Fatal("comparison did not start its S3 scan")
	}
	if code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons/"+resumed+"/cancel", f.admin, nil); code != 200 {
		t.Fatal(code, body)
	}
	body = waitComparison(t, f, resumed, "RUNNING", "CANCELING")
	if body["status"] != "CANCELED" || body["resumable"] != true {
		t.Fatal("canceled comparison", body)
	}
	s3.unblock()
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons/"+resumed+"/resume", f.admin, nil)
	if code != 202 || body["status"] != "RUNNING" || body["resumes"] != float64(1) {
		t.Fatal("resume", code, body)
	}
	body = waitComparison(t, f, resumed, "RUNNING", "CANCELING")
	if body["status"] != "COMPLETED" || body["compared"] != float64(4) || body["different"] != float64(1) || body["missing"] != float64(1) || body["extra"] != float64(1) || body["matched"] != float64(1) || body["rows"] != float64(3) {
		t.Fatal("resumed comparison differs from an uninterrupted one", body)
	}
	checkComparisonExports(t, f, resumed)
}

func nodeFromSession(session agenttransport.Session) task.Node {
	return task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID}
}
