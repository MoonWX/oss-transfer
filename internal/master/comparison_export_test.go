package master

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/comparison"
)

type comparisonDownload struct {
	status int
	header http.Header
	body   []byte
}

// downloadComparison performs an export request. A request whose response
// fails while it is read returns the error instead of a download.
func downloadComparison(t *testing.T, f *agentHTTPFixture, method, path, token, form string) (comparisonDownload, error) {
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
		return comparisonDownload{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return comparisonDownload{}, err
	}
	return comparisonDownload{response.StatusCode, response.Header, data}, nil
}

func mustDownloadComparison(t *testing.T, f *agentHTTPFixture, method, path, token, form string) comparisonDownload {
	t.Helper()
	download, err := downloadComparison(t, f, method, path, token, form)
	if err != nil {
		t.Fatal(err)
	}
	return download
}

func (d comparisonDownload) message(t *testing.T) string {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(d.body, &body); err != nil {
		t.Fatal("error reply is not JSON", d.status, string(d.body))
	}
	message, _ := body["message"].(string)
	return message
}

// comparisonArchive returns the entries of an export ZIP in their order.
func comparisonArchive(t *testing.T, d comparisonDownload) ([]string, map[string][]byte) {
	t.Helper()
	if d.status != 200 || d.header.Get("Content-Type") != "application/zip" || !strings.HasPrefix(d.header.Get("Content-Disposition"), "attachment; ") {
		t.Fatal("export is not a ZIP attachment", d.status, d.header, string(d.body))
	}
	archive, err := zip.NewReader(bytes.NewReader(d.body), int64(len(d.body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
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
		names = append(names, entry.Name)
		files[entry.Name] = content
	}
	return names, files
}

func differenceRows(t *testing.T, data []byte) []map[string]string {
	t.Helper()
	records, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(records[0], comparisonDifferencesHeader) {
		t.Fatal("differences.csv header", records[0])
	}
	rows := make([]map[string]string, 0, len(records)-1)
	for _, record := range records[1:] {
		row := map[string]string{}
		for i, name := range records[0] {
			row[name] = record[i]
		}
		rows = append(rows, row)
	}
	return rows
}

func exportSummary(t *testing.T, data []byte) comparisonExportSummary {
	t.Helper()
	var summary comparisonExportSummary
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&summary); err != nil {
		t.Fatal(err)
	}
	return summary
}

func TestComparisonExportZipKeepsMoreThanTenThousandRows(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "export-current", agent)
	if _, err := f.master.agents.registry.UpdateLabel(node.ID, "Export node"); err != nil {
		t.Fatal(err)
	}
	rows := fakeComparisonRows(10500)
	// Rows 1, 2, 3, 8, 9 and 10 have retry statuses (DIFFERENT, ETAG_MISMATCH,
	// MISSING_TARGET). import-inventory cannot read four of these keys back.
	rows[0].SourceKey = "src/line\nbreak"
	rows[1].SourceKey = "   "
	rows[2].SourceKey = "=HYPERLINK(\"http://example.invalid\")"
	rows[7].SourceKey = "src/nul\x00key"
	rows[8].SourceKey = `src/comma,"quoted"`
	rows[9].SourceKey = "src/" + strings.Repeat("k", 1021)
	p := completedComparison("cmp-big", rows)
	agent.put(p, rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-big"), time.Now())
	names, files := comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-big/export", f.admin, ""))
	if !slices.Equal(names, []string{"differences.csv", "retry-source-keys.incr", "summary.json"}) {
		t.Fatal("ZIP entries", names)
	}
	exported := differenceRows(t, files["differences.csv"])
	if len(exported) != len(rows) {
		t.Fatal("rows lost", len(exported))
	}
	for i, row := range exported {
		if row["number"] != fmt.Sprint(i+1) || row["status"] != rows[i].Status {
			t.Fatal("row order", i, row)
		}
	}
	if exported[2]["sourceKey"] != `'=HYPERLINK("http://example.invalid")` || exported[8]["sourceKey"] != `src/comma,"quoted"` || exported[0]["sourceKey"] != "src/line\nbreak" {
		t.Fatal("CSV escaping", exported[0], exported[2], exported[8])
	}
	if exported[0]["reasons"] != "size;contentType" || exported[1]["reasons"] != "etag" || exported[5]["error"] != "Cannot read object metadata" || exported[5]["errorDetail"] != "target HEAD: HTTP 403 AccessDenied (request ID 1A2B)" || exported[4]["sourceSize"] != "4" || exported[4]["destinationSize"] != "5" {
		t.Fatal("row fields", exported[0], exported[1], exported[4], exported[5])
	}
	var retry []string
	omitted := map[int]bool{0: true, 1: true, 7: true, 9: true}
	for i, row := range rows {
		if slices.Contains(comparison.RetryStatuses, row.Status) && !omitted[i] {
			retry = append(retry, row.SourceKey)
		}
	}
	if string(files["retry-source-keys.incr"]) != strings.Join(retry, "\n")+"\n" || len(retry) != 4496 {
		t.Fatal("retry keys", len(retry), strings.Count(string(files["retry-source-keys.incr"]), "\n"))
	}
	summary := exportSummary(t, files["summary.json"])
	generated := summary.GeneratedAt
	summary.GeneratedAt = time.Time{}
	ended := p.EndedAt
	want := comparisonExportSummary{
		ComparisonID: "cmp-big", Node: comparisonExportNode{ID: node.ID, Name: "Export node (127.0.0.1)"}, Status: "COMPLETED", Mode: "content", Scope: "prefix",
		Source:      comparisonExportEndpoint{Connection: "qa", Bucket: "source", Prefix: "src/"},
		Destination: comparisonExportEndpoint{Connection: "qa", Bucket: "target", Prefix: "dst/"},
		Counts:      comparisonExportCounts{Compared: p.Compared, Matched: 5, Different: p.Different, ETagMismatch: p.ETagMismatch, Missing: p.Missing, Extra: p.Extra, Changed: p.Changed, Errors: p.Errors, BothMissing: p.BothMissing},
		StoredRows:  10500, ExportedRows: 10500, StatusFilter: []string{}, Complete: true, IncompleteReasons: []string{},
		RetryStatuses: comparison.RetryStatuses, RetryKeys: 4496, RetryKeysOmitted: 4, RetryPrefixesMatch: false,
		CSVEscaping: comparisonCSVEscaping, StartedAt: p.StartedAt, EndedAt: &ended,
	}
	gotJSON, _ := json.Marshal(summary)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) || generated.IsZero() {
		t.Fatalf("summary\n got %s\nwant %s", gotJSON, wantJSON)
	}
	queries := rowsQueries(t, agent)
	var afters []int64
	for _, q := range queries {
		if q.Limit != comparison.MaxPageRows || q.MaxBytes != comparison.MaxPageBytes {
			t.Fatal("export page size", q)
		}
		if len(q.Statuses) == 0 {
			afters = append(afters, q.After)
		} else if !slices.Equal(q.Statuses, comparison.RetryStatuses) {
			t.Fatal("retry pass filter", q)
		}
	}
	if !slices.Equal(afters, []int64{0, 1000, 2000, 3000, 4000, 5000, 6000, 7000, 8000, 9000, 10000}) || len(queries) != 11+5 {
		t.Fatal("export pages", afters, len(queries))
	}
}

func TestComparisonExportFiltersLegacyRowsInMaster(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(false)
	node := comparisonTestNode(t, f, "export-legacy", agent)
	rows := fakeComparisonRows(450)
	p := completedComparison("cmp-legacy", rows)
	p.ResultsProtocol, p.ResultLimit, p.RowsTruncated = 0, comparison.LegacyStoredDifferences, true
	p.Compared, p.Missing = p.Compared+20000, p.Missing+20000
	agent.put(p, rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-legacy"), time.Now())
	_, files := comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-legacy/export?format=zip&status=MISSING_TARGET,DIFFERENT", f.admin, ""))
	exported := differenceRows(t, files["differences.csv"])
	var want []string
	for i, row := range rows {
		if row.Status == comparison.StatusDifferent || row.Status == comparison.StatusMissingTarget {
			want = append(want, fmt.Sprint(i+1))
		}
	}
	var got []string
	for _, row := range exported {
		got = append(got, row["number"])
	}
	if !slices.Equal(got, want) || len(got) != 129 {
		t.Fatal("Master-side filter", len(got), len(want))
	}
	if keys := strings.Count(string(files["retry-source-keys.incr"]), "\n"); keys != 194 {
		t.Fatal("retry keys must not follow the export filter", keys)
	}
	summary := exportSummary(t, files["summary.json"])
	if summary.Complete || !slices.Equal(summary.IncompleteReasons, []string{"ROWS_TRUNCATED:LEGACY_LIMIT", "ROW_COUNT_MISMATCH"}) || summary.TruncatedReason != comparison.TruncatedLegacyLimit || summary.ExportedRows != 129 || summary.StoredRows != 450 || !slices.Equal(summary.StatusFilter, []string{"DIFFERENT", "MISSING_TARGET"}) || summary.RetryKeys != 194 {
		t.Fatalf("legacy summary %+v", summary)
	}
	queries := legacyGetQueries(t, agent)
	if len(queries) != 6 || len(agent.calledWith("comparison_rows")) != 0 {
		t.Fatal("legacy export requests", queries)
	}
	for i, q := range queries {
		if q.Limit != comparison.MaxLegacyPageRows || q.After != []int64{0, 200, 400}[i%3] {
			t.Fatal("legacy export page", i, q)
		}
	}
}

func TestComparisonExportJSONLStopsAtFirstSnapshot(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	agent.scanBudget = 1000
	node := comparisonTestNode(t, f, "export-jsonl", agent)
	rows := fakeComparisonRows(1500)
	p := completedComparison("cmp-running", rows)
	p.Status, p.EndedAt = comparison.StateRunning, time.Time{}
	agent.put(p, rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-running"), time.Now())
	// The running comparison stores 500 more rows after the first page.
	var once sync.Once
	agent.setIntercept(func(c agenttransport.Command) (agenttransport.Response, bool) {
		var q comparison.RowsQuery
		if c.Operation == "comparison_rows" && json.Unmarshal(c.Payload, &q) == nil && q.After > 0 {
			once.Do(func() {
				agent.mu.Lock()
				defer agent.mu.Unlock()
				stored := agent.comparisons["cmp-running"]
				for i, row := range fakeComparisonRows(2000)[1500:] {
					row.Number = int64(1501 + i)
					stored.rows = append(stored.rows, row)
				}
				stored.progress.Rows = 2000
			})
		}
		return agenttransport.Response{}, false
	})
	download := mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-running/export?format=JSONL&status=ERROR", f.admin, "")
	if download.status != 200 || download.header.Get("Content-Type") != "application/x-ndjson" || download.header.Get("Content-Disposition") != `attachment; filename="comparison-cmp-running.jsonl"` {
		t.Fatal(download.status, download.header, string(download.body))
	}
	if !bytes.HasSuffix(download.body, []byte("\n")) {
		t.Fatal("JSONL does not end with a newline")
	}
	var numbers []int64
	for _, line := range strings.Split(strings.TrimSuffix(string(download.body), "\n"), "\n") {
		var row comparison.Row
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err, line)
		}
		if row.Status != comparison.StatusError || row.Error != "Cannot read object metadata" || row.ErrorDetail == "" {
			t.Fatal("exported row", row)
		}
		numbers = append(numbers, row.Number)
	}
	// ERROR rows are numbered 6, 13, ...; none after row 1500 of the snapshot.
	if len(numbers) != 214 || numbers[0] != 6 || numbers[len(numbers)-1] != 1497 {
		t.Fatal("snapshot bound", len(numbers), numbers[0], numbers[len(numbers)-1])
	}
	// The same export through the CSV keeps the snapshot's counts.
	agent.setIntercept(nil)
	_, files := comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-running/export", f.admin, ""))
	summary := exportSummary(t, files["summary.json"])
	if summary.Complete || !slices.Equal(summary.IncompleteReasons, []string{"NOT_COMPLETED:RUNNING", "ROW_COUNT_MISMATCH", "RETRY_KEYS_MISMATCH"}) || summary.EndedAt != nil || summary.ExportedRows != 2000 {
		t.Fatalf("running summary %+v", summary)
	}
}

func TestComparisonExportTicketsAreSingleUseAndBoundToSession(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "export-tickets", agent)
	rows := fakeComparisonRows(30)
	for _, id := range []string{"cmp-a", "cmp-b"} {
		agent.put(completedComparison(id, rows), rows)
		putComparisonAssignment(t, f, node, comparisonTestRequest(id), time.Now())
	}
	issue := func(query string) (int, string, string) {
		t.Helper()
		code, body := comparisonGet(t, f, "GET", "/api/master/comparisons/cmp-a/export-ticket"+query, "", nil)
		if code != 200 {
			message, _ := body["message"].(string)
			return code, "", message
		}
		if body["success"] != true {
			t.Fatal(body)
		}
		return code, body["data"].(map[string]any)["ticket"].(string), ""
	}
	for _, test := range []struct{ query, message string }{
		{"?format=csv", "Export format must be zip or jsonl"},
		{"?status=MATCH", "Unknown comparison result status"},
	} {
		if code, _, message := issue(test.query); code != 400 || message != test.message {
			t.Fatal(test.query, code, message)
		}
	}
	if code, _ := comparisonGet(t, f, "GET", "/api/master/comparisons/unknown/export-ticket", "", nil); code != 404 {
		t.Fatal("ticket for an unknown comparison", code)
	}
	_, ticket, _ := issue("?format=jsonl&status=EXTRA_TARGET")
	form := url.Values{"ticket": {ticket}}.Encode()
	// The ticket carries its format and filter; the request cannot change them.
	download := mustDownloadComparison(t, f, "POST", "/api/master/comparisons/cmp-a/export?format=zip&status=DIFFERENT", "", form)
	if download.status != 200 || download.header.Get("Content-Type") != "application/x-ndjson" || strings.Count(string(download.body), "\n") != 4 || !strings.Contains(string(download.body), `"status":"EXTRA_TARGET"`) {
		t.Fatal("ticket download", download.status, string(download.body))
	}
	if replay := mustDownloadComparison(t, f, "POST", "/api/master/comparisons/cmp-a/export", "", form); replay.status != 401 || replay.message(t) != "Invalid or expired download ticket" {
		t.Fatal("ticket replay accepted", replay.status)
	}
	_, ticket, _ = issue("")
	if other := mustDownloadComparison(t, f, "POST", "/api/master/comparisons/cmp-b/export", "", url.Values{"ticket": {ticket}}.Encode()); other.status != 401 {
		t.Fatal("ticket accepted for another comparison", other.status)
	}
	for _, malformed := range []string{"", "ticket=short", url.Values{"ticket": {ticket, ticket}}.Encode()} {
		if rejected := mustDownloadComparison(t, f, "POST", "/api/master/comparisons/cmp-a/export", "", malformed); rejected.status != 401 {
			t.Fatal("malformed ticket form", malformed, rejected.status)
		}
	}
	if code, _ := request(t, f.http.Client(), "GET", f.http.URL+"/api/master/comparisons/cmp-a/export-ticket", "", nil); code != 401 {
		t.Fatal("ticket issued without a session", code)
	}
	if unauthenticated := mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-a/export", "", ""); unauthenticated.status != 401 {
		t.Fatal("unauthenticated export", unauthenticated.status)
	}
	_, ticket, _ = issue("")
	_, _ = comparisonGet(t, f, "POST", "/api/auth/logout", "", nil)
	if revoked := mustDownloadComparison(t, f, "POST", "/api/master/comparisons/cmp-a/export", "", url.Values{"ticket": {ticket}}.Encode()); revoked.status != 401 || revoked.message(t) != "Sign in required" {
		t.Fatal("logout did not revoke the ticket", revoked.status)
	}
}

func TestComparisonExportTicketsExpireAndAreBounded(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "export-expiry", agent)
	rows := fakeComparisonRows(3)
	agent.put(completedComparison("cmp-expiry", rows), rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-expiry"), time.Now())
	// Handlers of a private instance run on the test goroutine with its clock.
	api := newComparisonAPI(f.master)
	now := time.Now()
	api.now = func() time.Time { return now }
	issue := func() (int, string) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/master/comparisons/cmp-expiry/export-ticket?format=jsonl", nil)
		req.Header.Set("Authorization", "Bearer "+f.admin)
		req.SetPathValue("id", "cmp-expiry")
		recorder := httptest.NewRecorder()
		api.issueExportTicket(recorder, req)
		var body struct {
			Data struct {
				Ticket string `json:"ticket"`
			} `json:"data"`
		}
		_ = json.Unmarshal(recorder.Body.Bytes(), &body)
		return recorder.Code, body.Data.Ticket
	}
	consume := func(ticket string) int {
		t.Helper()
		req := httptest.NewRequest("POST", "/api/master/comparisons/cmp-expiry/export", strings.NewReader(url.Values{"ticket": {ticket}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetPathValue("id", "cmp-expiry")
		recorder := httptest.NewRecorder()
		api.consumeExportTicket(recorder, req)
		return recorder.Code
	}
	_, ticket := issue()
	now = now.Add(2 * time.Minute)
	if code := consume(ticket); code != 401 {
		t.Fatal("expired ticket accepted", code)
	}
	_, ticket = issue()
	now = now.Add(2*time.Minute - time.Nanosecond)
	if code := consume(ticket); code != 200 {
		t.Fatal("unexpired ticket refused", code)
	}
	for i := range comparisonMaxTickets {
		if code, _ := issue(); code != 200 {
			t.Fatal("ticket", i, code)
		}
	}
	if code, _ := issue(); code != 429 {
		t.Fatal("unused tickets are not bounded", code)
	}
	now = now.Add(2 * time.Minute)
	if code, _ := issue(); code != 200 {
		t.Fatal("expired tickets still count", code)
	}
}

func TestComparisonExportRetriesTransientFailuresAndAbortsLateOnes(t *testing.T) {
	setComparisonTiming(t, &comparisonExportRetryDelays[0], time.Millisecond)
	setComparisonTiming(t, &comparisonExportRetryDelays[1], time.Millisecond)
	setComparisonTiming(t, &comparisonExportRetryDelays[2], time.Millisecond)
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "export-retry", agent)
	offline := offlineComparisonNode(t, f, "export-offline")
	rows := fakeComparisonRows(2500)
	agent.put(completedComparison("cmp-retry", rows), rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-retry"), time.Now())
	putComparisonAssignment(t, f, offline, comparisonTestRequest("cmp-offline"), time.Now())
	var mu sync.Mutex
	attempts := map[int64]int{}
	// failing answers the attempts at row cursor `after` with status until
	// `times` attempts have failed (times < 0: always).
	failing := func(after int64, status, times int) {
		mu.Lock()
		clear(attempts)
		mu.Unlock()
		agent.setIntercept(func(c agenttransport.Command) (agenttransport.Response, bool) {
			var q comparison.RowsQuery
			if c.Operation != "comparison_rows" || json.Unmarshal(c.Payload, &q) != nil || len(q.Statuses) > 0 {
				return agenttransport.Response{}, false
			}
			mu.Lock()
			attempts[q.After]++
			count := attempts[q.After]
			mu.Unlock()
			if q.After != after || times >= 0 && count > times {
				return agenttransport.Response{}, false
			}
			response, _ := fakeReply(status, map[string]string{"code": "operation_failed", "message": "injected failure"})
			return response, true
		})
	}
	attemptsAt := func(after int64) int {
		mu.Lock()
		defer mu.Unlock()
		return attempts[after]
	}
	// Two transient failures of a later page are repeated and the file is
	// complete.
	failing(1000, 503, 2)
	_, files := comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-retry/export", f.admin, ""))
	if len(differenceRows(t, files["differences.csv"])) != 2500 || !exportSummary(t, files["summary.json"]).Complete || attemptsAt(1000) != 3 {
		t.Fatal("transient failures were not retried", attemptsAt(1000))
	}
	// A page that keeps failing after the headers were sent aborts the
	// response instead of ending a truncated file.
	failing(1000, 503, -1)
	if download, err := downloadComparison(t, f, "GET", "/api/master/comparisons/cmp-retry/export", f.admin, ""); err == nil {
		t.Fatal("failed export ended as a complete response", download.status, len(download.body))
	}
	if attemptsAt(1000) != 1+len(comparisonExportRetryDelays) {
		t.Fatal("retries", attemptsAt(1000))
	}
	failing(2000, 400, -1)
	if download, err := downloadComparison(t, f, "GET", "/api/master/comparisons/cmp-retry/export?format=jsonl", f.admin, ""); err == nil {
		t.Fatal("failed JSONL export ended as a complete response", download.status, len(download.body))
	}
	if attemptsAt(2000) != 1 {
		t.Fatal("a non-transient failure was retried", attemptsAt(2000))
	}
	// Failures of the first page are reported before any file is sent.
	failing(0, 400, -1)
	if download := mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-retry/export", f.admin, ""); download.status != 502 || download.message(t) != "injected failure" || attemptsAt(0) != 1 {
		t.Fatal("first page failure", download.status, string(download.body), attemptsAt(0))
	}
	// The same for the second walk, which lists the keys to transfer again.
	retryAttempts := 0
	failingRetryPass := func(status, times int) {
		mu.Lock()
		retryAttempts = 0
		mu.Unlock()
		agent.setIntercept(func(c agenttransport.Command) (agenttransport.Response, bool) {
			var q comparison.RowsQuery
			if c.Operation != "comparison_rows" || json.Unmarshal(c.Payload, &q) != nil || !slices.Equal(q.Statuses, comparison.RetryStatuses) || q.After != 0 {
				return agenttransport.Response{}, false
			}
			mu.Lock()
			retryAttempts++
			count := retryAttempts
			mu.Unlock()
			if times >= 0 && count > times {
				return agenttransport.Response{}, false
			}
			response, _ := fakeReply(status, map[string]string{"code": "operation_failed", "message": "injected failure"})
			return response, true
		})
	}
	retryAttemptsMade := func() int {
		mu.Lock()
		defer mu.Unlock()
		return retryAttempts
	}
	failingRetryPass(503, 2)
	_, files = comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-retry/export", f.admin, ""))
	if summary := exportSummary(t, files["summary.json"]); !summary.Complete || summary.RetryKeys != 1072 || strings.Count(string(files["retry-source-keys.incr"]), "\n") != 1072 || retryAttemptsMade() != 3 {
		t.Fatalf("transient retry pass failures were not retried: %d %+v", retryAttemptsMade(), summary)
	}
	failingRetryPass(503, -1)
	if download, err := downloadComparison(t, f, "GET", "/api/master/comparisons/cmp-retry/export", f.admin, ""); err == nil {
		t.Fatal("failed retry pass ended as a complete response", download.status, len(download.body))
	}
	if retryAttemptsMade() != 1+len(comparisonExportRetryDelays) {
		t.Fatal("retry pass retries", retryAttemptsMade())
	}
	failingRetryPass(400, -1)
	if download, err := downloadComparison(t, f, "GET", "/api/master/comparisons/cmp-retry/export?status=EXTRA_TARGET", f.admin, ""); err == nil {
		t.Fatal("failed retry pass ended as a complete response", download.status, len(download.body))
	}
	if retryAttemptsMade() != 1 {
		t.Fatal("a non-transient retry pass failure was retried", retryAttemptsMade())
	}
	agent.setIntercept(nil)
	if download := mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-offline/export", f.admin, ""); download.status != 503 || download.message(t) != "Agent is offline" {
		t.Fatal("offline node", download.status, string(download.body))
	}
	agent.mu.Lock()
	delete(agent.comparisons, "cmp-retry")
	agent.mu.Unlock()
	if download := mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-retry/export", f.admin, ""); download.status != 404 || download.message(t) != "Comparison not found" || comparisonAssignmentExists(t, f, "cmp-retry") {
		t.Fatal("expired comparison", download.status, string(download.body))
	}
	for _, query := range []string{"?format=csv", "?status=MATCH"} {
		if download := mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-offline/export"+query, f.admin, ""); download.status != 400 {
			t.Fatal(query, download.status)
		}
	}
}

func TestComparisonImportableKeysMatchImportInventory(t *testing.T) {
	for key, want := range map[string]bool{
		"a": true, " leading and trailing ": true, "中文/키": true, strings.Repeat("k", 1024): true,
		"": false, "   ": false, "\t": false, strings.Repeat("k", 1025): false, "bad\xff": false,
		"nul\x00": false, "cr\r": false, "lf\n": false,
	} {
		if importableKey(key) != want {
			t.Fatalf("%q: want %v", key, want)
		}
	}
}

func TestComparisonExportKeepsLineBreaksInCSVKeys(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "export-line-breaks", agent)
	rows := fakeComparisonRows(10)
	// Rows 1, 2, 3 and 8 have retry statuses; import-inventory cannot read
	// these keys back, so the export is the only place that names them.
	keys := map[int]string{0: "src/a\rb", 1: "\rlead", 2: "src/a\nb", 7: "src/trail\r"}
	for i, key := range keys {
		rows[i].SourceKey, rows[i].Key = key, key
	}
	agent.put(completedComparison("cmp-breaks", rows), rows)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-breaks"), time.Now())
	_, files := comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-breaks/export", f.admin, ""))
	raw := string(files["differences.csv"])
	if !strings.Contains(raw, "src/a\rb") || !strings.Contains(raw, "src/a\nb") || strings.Contains(raw, "\r\n") {
		t.Fatalf("CR or LF inside keys was rewritten: %q", raw)
	}
	exported := differenceRows(t, files["differences.csv"])
	for i, key := range keys {
		for _, column := range []string{"key", "sourceKey"} {
			if got := strings.TrimPrefix(exported[i][column], "'"); got != key {
				t.Fatalf("row %d %s: got %q, want %q", i+1, column, got, key)
			}
		}
	}
	if summary := exportSummary(t, files["summary.json"]); !summary.Complete || summary.RetryKeys != 2 || summary.RetryKeysOmitted != 4 {
		t.Fatalf("summary %+v", summary)
	}
}

func TestComparisonExportReportsUnavailableResults(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "export-unavailable", agent)
	// The Agent keeps the counts of a comparison whose stored rows are gone
	// and serves empty pages.
	p := completedComparison("cmp-unavailable", fakeComparisonRows(14))
	p.Error, p.ErrorDetail = comparisonResultsUnavailable, "result file missing"
	agent.put(p, nil)
	putComparisonAssignment(t, f, node, comparisonTestRequest("cmp-unavailable"), time.Now())
	for query, reasons := range map[string][]string{
		"":                     {"RESULTS_UNAVAILABLE", "ROW_COUNT_MISMATCH", "RETRY_KEYS_MISMATCH"},
		"?status=DIFFERENT":    {"RESULTS_UNAVAILABLE", "ROW_COUNT_MISMATCH", "RETRY_KEYS_MISMATCH"},
		"?status=EXTRA_TARGET": {"RESULTS_UNAVAILABLE", "ROW_COUNT_MISMATCH", "RETRY_KEYS_MISMATCH"},
	} {
		_, files := comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/cmp-unavailable/export"+query, f.admin, ""))
		if summary := exportSummary(t, files["summary.json"]); summary.Complete || !slices.Equal(summary.IncompleteReasons, reasons) || summary.ExportedRows != 0 || summary.RetryKeys != 0 {
			t.Fatalf("%s: %+v", query, summary)
		}
	}
}

func TestComparisonExportChecksFilteredRowsAgainstCounts(t *testing.T) {
	f := newAgentHTTPFixture(t)
	agent := newFakeComparisonAgent(true)
	node := comparisonTestNode(t, f, "export-counts", agent)
	rows := fakeComparisonRows(70)
	agent.put(completedComparison("cmp-exact", rows), rows)
	// The counts name one more DIFFERENT and one fewer ERROR result than the
	// stored rows hold: the rows and the counts still add up.
	short := completedComparison("cmp-short", rows)
	short.Different, short.Errors = short.Different+1, short.Errors-1
	agent.put(short, rows)
	// Results of an older Agent counted BOTH_MISSING as errors.
	legacy := short
	legacy.ID, legacy.ResultsProtocol = "cmp-old-results", 0
	agent.put(legacy, rows)
	for _, id := range []string{"cmp-exact", "cmp-short", "cmp-old-results"} {
		putComparisonAssignment(t, f, node, comparisonTestRequest(id), time.Now())
	}
	for _, test := range []struct {
		id, query string
		reasons   []string
	}{
		{"cmp-exact", "?status=DIFFERENT,BOTH_MISSING", []string{}},
		{"cmp-exact", "?status=ERROR", []string{}},
		{"cmp-short", "", []string{"RETRY_KEYS_MISMATCH"}},
		{"cmp-short", "?status=DIFFERENT", []string{"ROW_COUNT_MISMATCH", "RETRY_KEYS_MISMATCH"}},
		{"cmp-short", "?status=DIFFERENT,ERROR", []string{"RETRY_KEYS_MISMATCH"}},
		{"cmp-short", "?status=EXTRA_TARGET", []string{"RETRY_KEYS_MISMATCH"}},
		{"cmp-old-results", "?status=DIFFERENT", []string{}},
	} {
		_, files := comparisonArchive(t, mustDownloadComparison(t, f, "GET", "/api/master/comparisons/"+test.id+"/export"+test.query, f.admin, ""))
		summary := exportSummary(t, files["summary.json"])
		if summary.Complete != (len(test.reasons) == 0) || !slices.Equal(summary.IncompleteReasons, test.reasons) {
			t.Fatalf("%s%s: %+v", test.id, test.query, summary)
		}
	}
}
