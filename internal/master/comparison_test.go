package master

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// This crosses the real API, reverse Agent channel, S3 SDK LIST/HEAD/GET and
// durable comparison results. The local HTTP fixture supplies only S3 storage.
func TestComparisonHTTPAgentStreamingAndCredentialBoundary(t *testing.T) {
	objects := map[string]string{"source/2026/a": "AAAA", "target/backup/a": "BBBB", "source/2026/missing": "source", "target/backup/extra": "target", "source/2026/a +?中文": "same", "target/backup/a +?中文": "same"}
	var downloads atomic.Int64
	var blockLists atomic.Bool
	listStarted, releaseLists := make(chan struct{}, 1), make(chan struct{})
	var release sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		bucket, object, _ := strings.Cut(key, "/")
		if r.Method == "GET" && object == "" {
			if blockLists.Load() {
				select {
				case listStarted <- struct{}{}:
				default:
				}
				select {
				case <-releaseLists:
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
					if strings.HasPrefix(relative, r.URL.Query().Get("prefix")) {
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
			downloads.Add(1)
			_, _ = w.Write([]byte(data))
		}
	}))
	defer func() {
		release.Do(func() { close(releaseLists) })
		server.Close()
	}()
	f := newAgentHTTPFixture(t)
	agent, _ := taskAPIAgent(t, f)
	code, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/s3-config", f.admin, map[string]any{"alias": "test", "endpoint": server.URL, "accessKey": "fixture-private-ak", "secretKey": "fixture-private-sk", "region": "test", "pathStyleEnable": true, "validate": false})
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
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/comparisons/%s?limit=2", f.http.URL, id), f.admin, nil)
		if code != 200 {
			t.Fatal(code, body)
		}
		if body["status"] != "RUNNING" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("comparison did not finish", body)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if body["status"] != "COMPLETED" || body["different"] != float64(1) || body["missing"] != float64(1) || body["extra"] != float64(1) || body["matched"] != float64(1) || downloads.Load() != 4 {
		t.Fatalf("wrong real S3 comparison: %+v downloads=%d", body, downloads.Load())
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
	blockLists.Store(true)
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons", f.admin, create)
	if code != 202 {
		t.Fatal(code, body)
	}
	select {
	case <-listStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("comparison did not start its S3 scan")
	}
	code, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/comparisons", f.admin, create)
	if code != 503 || countAssignments() != 1 {
		t.Fatal("busy Agent retained a mapping for work it rejected", code, body)
	}
	release.Do(func() { close(releaseLists) })
}
