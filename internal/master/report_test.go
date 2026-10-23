package master

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// This Agent has a real ledger and Engine but no automatic scheduler: its
// accepted jobs can be stopped deterministically before any object is opened.
func stoppedReportFixture(t *testing.T) (*agentHTTPFixture, task.Parent) {
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
	engine, err := execution.NewEngine(repo, execution.StoreFactoryFunc(func(context.Context, execution.Job) (transferstore.Store, transferstore.Store, error) {
		return nil, nil, errors.New("report fixture must never open object data")
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
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("report Agent did not exit")
		}
		_ = engine.Close(context.Background())
		_ = db.Close()
	})
	deadline := time.Now().Add(3 * time.Second)
	var node task.Node
	for {
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
	p, _, err := f.master.tasks.repo.Create(spec, "stopped-report")
	if err != nil {
		t.Fatal(err)
	}
	child, err := f.master.tasks.repo.Assign(p.ID, p.Generation, 1, []transferstore.Object{{Key: "=SUM(A1)", Size: 9}, {Key: "ordinary/file", Size: 11}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Accept(ctx, child.Job); err != nil {
		t.Fatal(err)
	}
	if err := engine.StopParent(ctx, p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	record, err := engine.Get(ctx, child.Job.ChildID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.Observe(p.ID, p.Generation, 1, record); err != nil {
		t.Fatal(err)
	}
	if _, err := f.master.tasks.repo.Stop(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.repo.Stopped(p.ID, p.Generation); err != nil {
		t.Fatal(err)
	}
	p, err = f.master.tasks.repo.Get(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, p
}

func fetchReport(t *testing.T, f *agentHTTPFixture, method, path, token, form string) (int, map[string][]byte) {
	t.Helper()
	req, err := http.NewRequest(method, f.http.URL+path, strings.NewReader(form))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if form != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	response, err := f.http.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 200 {
		return response.StatusCode, nil
	}
	if response.Header.Get("Content-Type") != "application/zip" || !strings.Contains(response.Header.Get("Content-Disposition"), "attachment;") {
		t.Fatal("download was not an attachment", response.Header)
	}
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal("incomplete ZIP", err)
	}
	files := map[string][]byte{}
	for _, entry := range archive.File {
		stream, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(stream)
		_ = stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name] = content
	}
	return response.StatusCode, files
}
func reportRows(t *testing.T, data []byte) []map[string]string {
	t.Helper()
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil || len(rows) == 0 {
		t.Fatal(err)
	}
	result := make([]map[string]string, 0, len(rows)-1)
	for _, row := range rows[1:] {
		values := map[string]string{}
		for i, key := range rows[0] {
			values[key] = row[i]
		}
		result = append(result, values)
	}
	return result
}

func TestReportHTTPStoppedLedgerIdentifiersTicketsAndUnavailableNode(t *testing.T) {
	f, p := stoppedReportFixture(t)
	path := fmt.Sprintf("/api/master/task-reports/%d", p.ID)
	status, files := fetchReport(t, f, "GET", path, f.admin, "")
	if status != 200 || len(files) != 4 {
		t.Fatal(status, len(files))
	}
	rows := reportRows(t, files["file-results.csv"])
	if len(rows) != 2 || rows[0]["parentTaskId"] != fmt.Sprint(p.ID) || rows[0]["childTaskId"] != fmt.Sprintf("%d-1", p.ID) || rows[0]["result"] != "UNPROCESSED" || rows[0]["logicalKey"] != "'=SUM(A1)" {
		t.Fatal("stopped ledger or CSV escaping incorrect", rows)
	}
	if rows[0]["node"] != "QA node (127.0.0.1)" || rows[0]["nodeId"] != p.Spec.Nodes[0].Address {
		t.Fatal("report lost the human-readable node or its stable identity", rows[0])
	}
	for _, summary := range reportRows(t, files["task-summary.csv"]) {
		if summary["rowType"] == "CHILD_TASK" && (summary["node"] != rows[0]["node"] || summary["nodeId"] != rows[0]["nodeId"]) {
			t.Fatal("summary and object report disagree about the node", summary)
		}
	}
	for _, key := range []string{"taskId", "shardId"} {
		if _, exists := rows[0][key]; exists {
			t.Fatal("internal ID leaked", key)
		}
	}
	if rows[0]["completedAt"] != "" || rows[0]["ledgerObservedAt"] == "" {
		t.Fatal("invented per-object completion timestamp", rows[0])
	}
	var meta map[string]any
	if err := json.Unmarshal(files["report-metadata.json"], &meta); err != nil {
		t.Fatal(err)
	}
	if meta["complete"] != false || meta["countsReconciled"] != true || meta["resultCounts"].(map[string]any)["UNPROCESSED"] != float64(2) {
		t.Fatal("stopped snapshot claimed full migration", meta)
	}
	status, body := request(t, f.http.Client(), "GET", f.http.URL+path+"/download-ticket", f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	ticket := body["data"].(map[string]any)["ticket"].(string)
	form := url.Values{"ticket": {ticket}}.Encode()
	if code, _ := fetchReport(t, f, "POST", path+"/download", "", form); code != 200 {
		t.Fatal("valid ticket refused", code)
	}
	if code, _ := fetchReport(t, f, "POST", path+"/download", "", form); code != 401 {
		t.Fatal("ticket replay accepted", code)
	}
	if code, _ := fetchReport(t, f, "GET", path, "", ""); code != 401 {
		t.Fatal("unauthenticated export", code)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+path+"/download-ticket", f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	second := body["data"].(map[string]any)["ticket"].(string)
	if code, _ := fetchReport(t, f, "POST", fmt.Sprintf("/api/master/task-reports/%d/download", p.ID+1), "", url.Values{"ticket": {second}}.Encode()); code != 401 {
		t.Fatal("ticket accepted for another task", code)
	}
	// Offline data is explicitly UNKNOWN; it must never be mislabeled as a
	// verified failure or an object that was definitely never processed.
	if code, _ := request(t, f.http.Client(), "DELETE", f.http.URL+"/api/master/nodes/"+url.PathEscape(p.Spec.Nodes[0].Address), f.admin, nil); code != 200 {
		t.Fatal("cannot revoke fixture Agent", code)
	}
	status, files = fetchReport(t, f, "GET", path, f.admin, "")
	if status != 200 {
		t.Fatal(status)
	}
	rows = reportRows(t, files["file-results.csv"])
	if rows[0]["result"] != "UNKNOWN" || rows[0]["reasonCode"] != "LEDGER_UNAVAILABLE" {
		t.Fatal("offline results invented", rows)
	}
	if err := json.Unmarshal(files["report-metadata.json"], &meta); err != nil {
		t.Fatal(err)
	}
	if meta["complete"] != false || len(meta["unreachableNodes"].([]any)) != 1 || meta["resultCounts"].(map[string]any)["UNKNOWN"] != float64(2) {
		t.Fatal("missing ledger not disclosed", meta)
	}
	for _, row := range reportRows(t, files["exception-details.csv"]) {
		if row["node"] != "QA node (127.0.0.1)" || row["nodeId"] == "" {
			t.Fatal("revoked node label missing from error report", row)
		}
	}
	if len(reportRows(t, files["exception-details.csv"])) != 3 {
		t.Fatal("offline object details absent")
	}
	entries, err := filepath.Glob(filepath.Join(filepath.Dir(f.path), ".report-exceptions-*"))
	if err != nil || len(entries) != 0 {
		t.Fatal("exception spool leaked", entries, err)
	}
	status, body = request(t, f.http.Client(), "GET", f.http.URL+path+"/download-ticket", f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	third := body["data"].(map[string]any)["ticket"].(string)
	request(t, f.http.Client(), "POST", f.http.URL+"/api/auth/logout", f.admin, nil)
	if code, _ := fetchReport(t, f, "POST", path+"/download", "", url.Values{"ticket": {third}}.Encode()); code != 401 {
		t.Fatal("logout did not revoke download ticket", code)
	}
}

func TestReportHTTPRejectsRunningAndRechecksStateAfterTicket(t *testing.T) {
	f, p := stoppedReportFixture(t)
	path := fmt.Sprintf("/api/master/task-reports/%d", p.ID)
	code, body := request(t, f.http.Client(), "GET", f.http.URL+path+"/download-ticket", f.admin, nil)
	if code != 200 {
		t.Fatal(code, body)
	}
	ticket := body["data"].(map[string]any)["ticket"].(string)
	if _, err := f.master.tasks.repo.Resume(p.ID); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{path, path + "/download-ticket"} {
		if code, _ := request(t, f.http.Client(), "GET", f.http.URL+endpoint, f.admin, nil); code != 409 {
			t.Fatal("active report allowed", endpoint, code)
		}
	}
	if code, _ := fetchReport(t, f, "POST", path+"/download", "", url.Values{"ticket": {ticket}}.Encode()); code != 409 {
		t.Fatal("ticket bypassed new active state", code)
	}
	if code, _ := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/task-reports/99999", f.admin, nil); code != 404 {
		t.Fatal(code)
	}
}

type failedReportWriter struct{}

func (failedReportWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestReportStopsOnWriterFailureAndCancellation(t *testing.T) {
	f, p := stoppedReportFixture(t)
	file, err := os.CreateTemp(t.TempDir(), "exceptions")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := f.master.writeTaskReport(context.Background(), failedReportWriter{}, file, []task.Parent{p}); err == nil {
		t.Fatal("disconnected download was accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.master.writeTaskReport(ctx, io.Discard, file, []task.Parent{p}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled report kept reading", err)
	}
}
