package master

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func ossTaskRequest() taskCreateRequest {
	return taskCreateRequest{SourceS3Alias: "qa", SourceBucket: "source", DestS3Alias: "qa", DestBucket: "destination", EligibleWorkerNodes: []string{"node.example"}}
}

func TestSplitAgentFilePreservesAbsoluteWindowsRoots(t *testing.T) {
	for _, test := range []struct{ input, root, name string }{
		{`C:\file.bin`, `C:\`, "file.bin"},
		{`C:/file.bin`, "C:/", "file.bin"},
		{`C:\folder\file.bin`, `C:\folder`, "file.bin"},
		{`\\server\share\file.bin`, `\\server\share`, "file.bin"},
		{`//server/share/file.bin`, "//server/share", "file.bin"},
		{"/file.bin", "/", "file.bin"},
		{"/folder/ spaced file.bin", "/folder", " spaced file.bin"},
	} {
		root, name := splitAgentFile(test.input)
		if root != test.root || name != test.name || !absoluteAgentPath(root) {
			t.Errorf("split %q = %q, %q; want %q, %q", test.input, root, name, test.root, test.name)
		}
	}
}

func TestTaskNormalizationPreservesObjectNamesAndRejectsAmbiguity(t *testing.T) {
	row := ossTaskRequest()
	row.Prefix = " leading//prefix /"
	row.KeyMapping = &taskMapping{Mode: "REPLACE_PREFIX", SourcePrefix: row.Prefix, TargetPrefix: " target// "}
	row.StartKey, row.EndKey, row.Marker = "a", "z", "m"
	row.DateAfter, row.DateBefore = "2026-09-01T00:00:00+08:00", "2026-10-01T00:00:00+08:00"
	spec, bindings, nodes, err := normalizeTaskRequest(row, "import")
	if err != nil || len(bindings) != 0 || !reflect.DeepEqual(nodes, row.EligibleWorkerNodes) {
		t.Fatalf("normalize: %+v %v %v %v", spec, bindings, nodes, err)
	}
	if spec.Prefix != row.Prefix || spec.ReplaceSourcePrefix != row.Prefix || spec.TargetPrefix != row.KeyMapping.TargetPrefix || spec.StartKey != "a" || spec.EndKey != "z" || spec.Marker != "m" || spec.ModifiedFrom.Hour() != 0 {
		t.Fatal("adapter rewrote keys, range or timezone", spec)
	}
	invalid := []struct {
		name string
		edit func(*taskCreateRequest)
	}{
		{"conflicting bucket", func(r *taskCreateRequest) { r.SourceEndpoint = &taskEndpoint{Type: "OSS", Bucket: "different"} }},
		{"prefix and object", func(r *taskCreateRequest) { r.Prefix, r.ObjectKey = "prefix/", "one" }},
		{"mapping without object", func(r *taskCreateRequest) { r.KeyMapping = &taskMapping{Mode: "EXACT_KEY", TargetKey: "dst"} }},
		{"replace wrong prefix", func(r *taskCreateRequest) {
			r.Prefix = "a/"
			r.KeyMapping = &taskMapping{Mode: "REPLACE_PREFIX", SourcePrefix: "b/"}
		}},
		{"same bucket", func(r *taskCreateRequest) { r.DestBucket = r.SourceBucket }},
		{"equal range", func(r *taskCreateRequest) { r.StartKey, r.EndKey = "a", "a" }},
		{"object with range", func(r *taskCreateRequest) { r.ObjectKey, r.StartKey = "a", "b" }},
		{"silent destination mapping", func(r *taskCreateRequest) { r.DestinationEndpoint = &taskEndpoint{Type: "OSS", Prefix: "important/"} }},
		{"node disagreement", func(r *taskCreateRequest) { r.WorkerNode = "other.example" }},
		{"invalid bandwidth", func(r *taskCreateRequest) { v := 0.0; r.TaskBandwidthLimitMbps = &v }},
		{"conflict disagreement", func(r *taskCreateRequest) { v := true; r.Overwrite = &v; r.ConflictPolicy = "SKIP" }},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			request := ossTaskRequest()
			tc.edit(&request)
			if _, _, _, err := normalizeTaskRequest(request, "add-buckets"); err == nil {
				t.Fatal("ambiguous or unsupported transfer accepted")
			}
		})
	}
}

func TestTaskRequestDecodingDoesNotImportProgressOrSilentlyDropOptions(t *testing.T) {
	base, _ := json.Marshal(ossTaskRequest())
	var exported map[string]any
	_ = json.Unmarshal(base, &exported)
	exported["taskIndex"], exported["putObjectCount"], exported["status"] = 900, 100000, "COMPLETED"
	exported["startKey"], exported["endKey"], exported["marker"] = "a", "z", "middle"
	exported["localPath"], exported["nodeBindings"] = nil, []any{}
	encoded, _ := json.Marshal([]any{exported})
	response := httptest.NewRecorder()
	rows, ok := readTaskRows(response, httptest.NewRequest("POST", "/api/master/import", bytes.NewReader(encoded)))
	if !ok || len(rows) != 1 || rows[0].Marker != "middle" || rows[0].StartKey != "a" {
		t.Fatal("legacy import rejected or source bounds dropped", response.Body.String())
	}
	for _, extra := range []map[string]any{{"taskBandwithLimitMbps": 5}, {"inventoryManifestId": "manifest-1"}, {"listQpsLimit": 10}, {"localFileEntries": []any{map[string]any{"key": "important"}}}} {
		var row map[string]any
		_ = json.Unmarshal(base, &row)
		for name, value := range extra {
			row[name] = value
		}
		encoded, _ := json.Marshal([]any{row})
		response := httptest.NewRecorder()
		if _, ok := readTaskRows(response, httptest.NewRequest("POST", "/api/master/import", bytes.NewReader(encoded))); ok || response.Code != 400 {
			t.Fatalf("ignored import option %v", extra)
		}
	}
	for _, input := range []string{"[]", "null", "{}", "[null]", string(encoded) + " {}"} {
		response := httptest.NewRecorder()
		if _, ok := readTaskRows(response, httptest.NewRequest("POST", "/api/master/add-buckets", strings.NewReader(input))); ok || response.Code != 400 {
			t.Fatalf("accepted malformed request %s", input)
		}
	}
}

func persistAPITask(t *testing.T, f *agentHTTPFixture, requestKey string) task.Parent {
	t.Helper()
	spec, _, _, err := normalizeTaskRequest(ossTaskRequest(), "add-buckets")
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes = []task.Node{{ID: "node", EnrollmentID: "enrollment", Address: "node.example"}}
	p, _, err := f.master.tasks.repo.Create(spec, requestKey)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTaskHTTPPaginationDetailsRecycleAndOfflineState(t *testing.T) {
	f := newAgentHTTPFixture(t)
	p := persistAPITask(t, f, "parent-a")
	observed := time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)
	for number := 1; number <= 31; number++ {
		c, err := f.master.tasks.repo.Assign(p.ID, p.Generation, number, []transferstore.Object{{Key: fmt.Sprintf("key-%03d", number), Size: int64(number)}})
		if err != nil {
			t.Fatal(err)
		}
		status := execution.Completed
		if number == 26 {
			status = execution.Recoverable
		}
		record := execution.Record{Job: c.Job, Status: status, Attempt: 1, UpdatedAt: observed, Counters: execution.Counters{Total: 1, TotalBytes: int64(number)}}
		if status == execution.Completed {
			record.Counters.Succeeded, record.Counters.SucceededBytes = 1, int64(number)
		}
		if err := f.master.tasks.repo.Observe(p.ID, p.Generation, number, record); err != nil {
			t.Fatal(err)
		}
	}
	status, body := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/children?offset=25&limit=5", f.http.URL, p.ID), f.admin, nil)
	data := body["data"].(map[string]any)
	items := data["items"].([]any)
	first := items[0].(map[string]any)
	if status != 200 || len(items) != 5 || data["totalCount"] != float64(31) || first["childTaskId"] != fmt.Sprintf("%d-26", p.ID) || first["status"] != "RECOVERABLE" || first["observedAt"] != float64(observed.UnixMilli()) {
		t.Fatalf("child page contract: %d %v", status, body)
	}
	if _, invented := data["snapshotAt"]; invented {
		t.Fatal("old Agent observation presented as a fresh snapshot")
	}
	if _, inherited := first["startTime"]; inherited {
		t.Fatal("child inherited parent timing")
	}
	status, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/children/31", f.http.URL, p.ID), f.admin, nil)
	if status != 200 || len(body["data"].(map[string]any)["items"].([]any)) != 1 {
		t.Fatal(status, body)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/status", f.admin, nil)
	row := body["tasks"].([]any)[0].(map[string]any)
	if status != 200 || row["listObjectCount"] != float64(31) || row["putObjectCount"] != float64(30) {
		t.Fatal("scanned count fell behind completed objects", body)
	}
	for _, endpoint := range []string{"/api/master/status", fmt.Sprintf("/api/master/tasks/%d/children", p.ID), "/api/master/deleted-tasks"} {
		if code, _ := request(t, f.http.Client(), "GET", f.http.URL+endpoint, "", nil); code != 401 {
			t.Fatalf("unauthenticated %s: %d", endpoint, code)
		}
	}
	for _, query := range []string{"offset=-1", "limit=201", "limit=0", "offset=abc"} {
		if code, _ := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/children?%s", f.http.URL, p.ID, query), f.admin, nil); code != 400 {
			t.Fatal("invalid page accepted", query, code)
		}
	}
	status, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/stop", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if status != 200 || body["partial"] != false || body["accepted"] != float64(1) {
		t.Fatal("offline stop intent was not accepted", status, body)
	}
	stored, err := f.master.tasks.repo.Get(p.ID)
	if err != nil || stored.Status != "STOPPING" {
		t.Fatal("offline stop intent was lost", stored.Status, err)
	}
	if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	status, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/clear", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if status != 200 || body["accepted"] != float64(1) {
		t.Fatal(status, body)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/deleted-tasks", f.admin, nil)
	if status != 200 || body["complete"] != true || body["totalCount"] != float64(1) || len(body["failedWorkers"].([]any)) != 0 || len(body["deletedTasks"].(map[string]any)["node.example"].([]any)) != 1 {
		t.Fatal("durable recycle bin treated offline Agent as missing data", status, body)
	}
	if code, _ := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d", f.http.URL, p.ID), f.admin, nil); code != 404 {
		t.Fatal("deleted task visible in active detail", code)
	}
	status, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/restore", f.admin, map[string]any{"taskIds": []int64{p.ID}})
	if status != 200 || body["accepted"] != float64(1) {
		t.Fatal(status, body)
	}
	if code, _ := request(t, f.http.Client(), "PUT", fmt.Sprintf("%s/api/master/tasks/%d/bandwidth-limit", f.http.URL, p.ID), f.admin, map[string]any{"limitMbps": 100}); code != 409 {
		t.Fatal("ended task bandwidth editable", code)
	}
	started := time.Now()
	status, body = request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/bandwidth-limit", f.http.URL, p.ID), f.admin, nil)
	if status != 200 || body["data"].(map[string]any)["applied"] != false || time.Since(started) > time.Second {
		t.Fatal("offline bandwidth status was slow or invented", status, body)
	}
}

// This fixture uses the real Agent service, HTTP reverse transport, and separate
// persistent databases. Only S3 I/O is outside this API-contract test; the cluster
// acceptance suite exercises actual object transfers against SeaweedFS.
func taskAPIAgent(t *testing.T, f *agentHTTPFixture) (*agentservice.Service, task.Node) {
	t.Helper()
	credential := agentCredential(t)
	status, session := f.join(t, f.invite(t, "node.example"), "api-agent", credential)
	if status != 200 {
		t.Fatal(status)
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
	ctx, cancel := context.WithCancel(context.Background())
	runner := agenttransport.NewRunner(agentClient(t, f, credential), service, nil, agenttransport.RunnerOptions{ReconnectDelay: time.Millisecond})
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Agent runner did not stop")
		}
		_ = service.Close(context.Background())
		_ = db.Close()
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		online, err := f.master.onlineAgents()
		if err != nil {
			t.Fatal(err)
		}
		if len(online) == 1 {
			return service, task.Node{ID: session.NodeID, EnrollmentID: session.EnrollmentID, Address: online[0].Address}
		}
		if time.Now().After(deadline) {
			t.Fatal("Agent never became online")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTaskHTTPRealAgentPrecheckCreationIdempotencyAndBandwidth(t *testing.T) {
	f := newAgentHTTPFixture(t)
	service, node := taskAPIAgent(t, f)
	_, err := storageconfig.NewMaster(f.db).Create(storageconfig.Config{Alias: "qa", Endpoint: "http://127.0.0.1:9", AccessKey: "fixture-ak", SecretKey: "fixture-sk"})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, " spaced file.txt")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	upload := taskCreateRequest{Direction: "LOCAL_TO_OSS", SourceEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{node.Address, file}}}, DestinationEndpoint: &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "destination", ObjectKey: " target/key "}}
	spec, checks, err := f.master.resolveTaskSpec(context.Background(), upload, "local-transfers")
	if err != nil || len(checks) != 1 || !checks[0].Valid || spec.Source.Root != dir || spec.ObjectKey != " spaced file.txt" || spec.TargetKey != " target/key " {
		t.Fatal("real file precheck lost path or target key", spec, checks, err)
	}
	requestBody := []taskCreateRequest{upload}
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, requestBody)
	if status != 200 || body["data"].(map[string]any)["valid"] != true {
		t.Fatal(status, body)
	}
	missing := filepath.Join(dir, "not-created-by-validation")
	download := taskCreateRequest{Direction: "OSS_TO_LOCAL", SourceEndpoint: &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source", ObjectKey: "one"}, DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{node.Address, missing}}}, TargetRelativePath: "nested/file"}
	status, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{download})
	if status != 200 || body["data"].(map[string]any)["valid"] != true {
		t.Fatal(status, body)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("precheck created a destination directory", err)
	}
	upload.SourceEndpoint.NodeBindings[0].LocalPath = missing
	status, body = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{upload})
	if status != 200 || body["data"].(map[string]any)["valid"] != false {
		t.Fatal("missing upload source accepted", status, body)
	}
	row := ossTaskRequest()
	row.EligibleWorkerNodes = []string{node.Address}
	post := func(row taskCreateRequest) map[string]any {
		encoded, _ := json.Marshal([]taskCreateRequest{row})
		req, _ := http.NewRequest("POST", f.http.URL+"/api/master/add-buckets", bytes.NewReader(encoded))
		req.Header.Set("Authorization", "Bearer "+f.admin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "lost-http-response")
		res, err := f.http.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != 200 {
			t.Fatal(res.StatusCode, result)
		}
		return result
	}
	first, replay := post(row), post(row)
	if !reflect.DeepEqual(first["taskIds"], replay["taskIds"]) || first["addedCount"] != float64(1) {
		t.Fatal("HTTP retry duplicated task", first, replay)
	}
	id := int64(first["taskIds"].([]any)[0].(float64))
	row.DestBucket = "another-destination"
	if conflict := post(row); conflict["rejectedCount"] != float64(1) || conflict["partial"] != true {
		t.Fatal("idempotency key silently rebound", conflict)
	}
	parents, _, err := f.master.allTaskParents()
	if err != nil || len(parents) != 1 {
		t.Fatal("creation retry persisted extra tasks", len(parents), err)
	}
	url := fmt.Sprintf("%s/api/master/tasks/%d/bandwidth-limit", f.http.URL, id)
	status, body = request(t, f.http.Client(), "PUT", url, f.admin, map[string]any{"limitMbps": 100})
	if status != 200 || body["data"].(map[string]any)["applied"] != true {
		t.Fatal("real Agent did not confirm bandwidth policy", status, body)
	}
	policy, err := service.Limits.Parent(id)
	if err != nil || policy.BandwidthBytesPerSecond != 12500000 {
		t.Fatal("Mbps conversion not applied to actual Agent", policy, err)
	}
	// A different durable revision must never be reported as the requested cap.
	if err := service.Limits.ApplyParent(context.Background(), ratelimit.ParentLimit{ParentID: id, Generation: policy.Generation, Revision: policy.Revision + 1, BandwidthBytesPerSecond: 1}); err != nil {
		t.Fatal(err)
	}
	status, body = request(t, f.http.Client(), "GET", url, f.admin, nil)
	if status != 200 || body["data"].(map[string]any)["applied"] != false {
		t.Fatal("mismatched Agent revision claimed applied", status, body)
	}
}

func TestLocalTaskExportCanBeValidatedAndImportedWithSameSelectors(t *testing.T) {
	f := newAgentHTTPFixture(t)
	_, node := taskAPIAgent(t, f)
	if _, err := storageconfig.NewMaster(f.db).Create(storageconfig.Config{Alias: "qa", Endpoint: "http://127.0.0.1:9", AccessKey: "fixture-ak", SecretKey: "fixture-sk"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, " spaced file.txt")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, single := range []bool{false, true} {
		for _, direction := range []string{"LOCAL_TO_OSS", "OSS_TO_LOCAL"} {
			t.Run(fmt.Sprintf("%s/single=%t", direction, single), func(t *testing.T) {
				local := &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{node.Address, dir}}}
				oss := &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "bucket", Prefix: " spaced//prefix /"}
				row := taskCreateRequest{Direction: direction, ConflictPolicy: "OVERWRITE", DistributionMode: "affinity", DateAfter: "2026-01-01T00:00:00Z"}
				if direction == "LOCAL_TO_OSS" {
					row.SourceEndpoint, row.DestinationEndpoint = local, oss
					if single {
						local.NodeBindings[0].LocalPath = file
						oss.Prefix, oss.ObjectKey = "", " exact//target "
					}
				} else {
					row.SourceEndpoint, row.DestinationEndpoint = oss, local
					if single {
						oss.Prefix, oss.ObjectKey = "", "source//key "
						row.TargetRelativePath = "nested/ output.txt"
					}
				}
				original, _, err := f.master.resolveTaskSpec(context.Background(), row, "local-transfers")
				if err != nil {
					t.Fatal(err)
				}
				exported := f.master.taskView(task.Parent{ID: 900, Spec: original, Status: "COMPLETED", ScanComplete: true, ChildCount: 7, Counters: execution.Counters{Succeeded: 500}})
				status, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []any{exported})
				if status != 200 || response["data"].(map[string]any)["valid"] != true {
					t.Fatal("export cannot be validated", status, response)
				}
				status, response = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers", f.admin, []any{exported})
				if status != 200 || response["data"].(map[string]any)["acceptedCount"] != float64(1) {
					t.Fatal("export cannot be imported", status, response)
				}
				id := int64(response["data"].(map[string]any)["parentTaskIds"].([]any)[0].(float64))
				created, err := f.master.tasks.repo.Get(id)
				if err != nil || !reflect.DeepEqual(created.Spec, original) || created.ID == 900 || created.Status != "RUNNING" || created.Counters.Succeeded != 0 || created.ChildCount != 0 || created.ScanComplete {
					t.Fatalf("import changed selectors or trusted exported progress: %+v; expected %+v; %v", created, original, err)
				}
			})
		}
	}
}

func TestLocalTaskExportRejectsConflictingSelectorsAndUnknownOptions(t *testing.T) {
	spec := task.Spec{Direction: "LOCAL_TO_OSS", Source: execution.Endpoint{Kind: "LOCAL", Root: "/source"}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "bucket"}, Nodes: []task.Node{{Address: "node", LocalRoot: "/source"}}, ObjectKey: "file.txt", MappingMode: "EXACT_KEY", TargetKey: "target"}
	for name, change := range map[string]func(map[string]any){
		"bucket":      func(row map[string]any) { row["destBucket"] = "another" },
		"mapping":     func(row map[string]any) { row["keyMapping"] = taskMapping{Mode: "EXACT_KEY", TargetKey: "another"} },
		"object":      func(row map[string]any) { row["objectKey"] = "another.txt" },
		"bindings":    func(row map[string]any) { row["nodeBindings"] = []taskBinding{{"node", "/different/file.txt"}} },
		"unknown":     func(row map[string]any) { row["taskBandwithLimitMbps"] = 5 },
		"unsupported": func(row map[string]any) { row["localFileEntries"] = []any{"important-file"} },
	} {
		t.Run(name, func(t *testing.T) {
			row := taskDTO(task.Parent{ID: 900, Spec: spec})
			change(row)
			encoded, _ := json.Marshal([]any{row})
			response := httptest.NewRecorder()
			if _, ok := readTaskRows(response, httptest.NewRequest("POST", "/api/master/local-transfers", bytes.NewReader(encoded))); ok || response.Code != 400 {
				t.Fatal("accepted conflicting export", response.Body.String())
			}
		})
	}
}
