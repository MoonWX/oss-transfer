package master

import (
	"archive/zip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type reportTicket struct {
	parentID int64
	session  string
	expires  time.Time
}
type reportAPI struct {
	server  *Server
	mu      sync.Mutex
	tickets map[string]reportTicket
}

func installReportRoutes(s *Server) {
	a := &reportAPI{server: s, tickets: make(map[string]reportTicket)}
	s.mux.HandleFunc("GET /api/master/task-reports/{id}", s.adminOnly(a.download))
	s.mux.HandleFunc("GET /api/master/task-reports/{id}/download-ticket", s.adminOnly(a.issueTicket))
	s.mux.HandleFunc("POST /api/master/task-reports/{id}/download", a.consumeTicket)
	s.mux.HandleFunc("GET /api/master/local-transfers/report", s.adminOnly(a.downloadLocal))
}

func (a *reportAPI) exportable(w http.ResponseWriter, r *http.Request) (task.Parent, bool) {
	id, ok := taskPathID(w, r)
	if !ok {
		return task.Parent{}, false
	}
	p, err := a.server.tasks.repo.Get(id)
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			fail(w, 404, "Task not found")
		} else {
			fail(w, 500, "Cannot read task")
		}
		return p, false
	}
	if p.Purged {
		fail(w, 404, "Task not found")
		return p, false
	}
	if !task.Ended(p.Status) {
		fail(w, 409, "任务尚未结束或仍在停止中，请完全停止后再导出报告")
		return p, false
	}
	return p, true
}

func (a *reportAPI) issueTicket(w http.ResponseWriter, r *http.Request) {
	p, ok := a.exportable(w, r)
	if !ok {
		return
	}
	value, err := randomToken()
	if err != nil {
		fail(w, 500, "Cannot create download ticket")
		return
	}
	a.mu.Lock()
	now := a.server.options.Now()
	for key, ticket := range a.tickets {
		if !now.Before(ticket.expires) {
			delete(a.tickets, key)
		}
	}
	if len(a.tickets) >= 2048 {
		a.mu.Unlock()
		fail(w, 429, "Too many unused download tickets")
		return
	}
	a.tickets[tokenKey(value)] = reportTicket{p.ID, bearer(r), now.Add(2 * time.Minute)}
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]string{"ticket": value}})
}

func (a *reportAPI) consumeTicket(w http.ResponseWriter, r *http.Request) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		fail(w, 400, "Invalid download form")
		return
	}
	values := r.PostForm["ticket"]
	if len(values) != 1 || len(values[0]) != 43 {
		fail(w, 401, "Invalid or expired download ticket")
		return
	}
	a.mu.Lock()
	ticket, found := a.tickets[tokenKey(values[0])]
	delete(a.tickets, tokenKey(values[0]))
	a.mu.Unlock()
	if !found || ticket.parentID != id || !a.server.options.Now().Before(ticket.expires) {
		fail(w, 401, "Invalid or expired download ticket")
		return
	}
	// A ticket is subordinate to the original login session. Logging out or
	// changing the account password revokes even an unused download ticket.
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+ticket.session)
	user, valid, err := a.server.authenticated(clone)
	if err != nil {
		fail(w, 500, "Cannot validate download session")
		return
	}
	if !valid || user.Role != "admin" {
		fail(w, 401, "Sign in required")
		return
	}
	a.download(w, r)
}

func (a *reportAPI) download(w http.ResponseWriter, r *http.Request) {
	p, ok := a.exportable(w, r)
	if !ok {
		return
	}
	a.stream(w, r, []task.Parent{p}, fmt.Sprintf("task-%d-report.zip", p.ID))
}
func (a *reportAPI) downloadLocal(w http.ResponseWriter, r *http.Request) {
	all, truncated, err := a.server.allTaskParents()
	if err != nil {
		fail(w, 500, "Cannot read local tasks")
		return
	}
	if truncated {
		fail(w, 409, "Use single-task reports for this task history")
		return
	}
	parents := []task.Parent{}
	for _, p := range all {
		if p.Spec.Direction == "OSS_TO_OSS" || p.Deleted || p.Purged {
			continue
		}
		if !task.Ended(p.Status) {
			fail(w, 409, "Stop or finish all local transfers before exporting")
			return
		}
		parents = append(parents, p)
	}
	a.stream(w, r, parents, "local-transfer-report.zip")
}

func (a *reportAPI) stream(w http.ResponseWriter, r *http.Request, parents []task.Parent, filename string) {
	// Only an exception index is spooled on the Master. It contains metadata,
	// never source/target data, and is removed on completion or cancellation.
	exceptions, err := os.CreateTemp(a.server.options.DataDirectory, ".report-exceptions-*.csv")
	if err != nil {
		fail(w, 500, "Cannot prepare report")
		return
	}
	defer os.Remove(exceptions.Name())
	defer exceptions.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := a.server.writeTaskReport(r.Context(), w, exceptions, parents); err != nil {
		// Headers may already be sent. Reset the connection instead of appending
		// JSON to a ZIP or presenting a truncated transfer as successful.
		panic(http.ErrAbortHandler)
	}
}

var reportFileHeader = []string{"parentTaskId", "childTaskId", "direction", "logicalKey", "sourceS3Alias", "sourceBucket", "sourceKey", "sourcePath", "destinationS3Alias", "destinationBucket", "destinationKey", "destinationPath", "size", "node", "result", "reasonCode", "errorMessage", "discoveredAt", "completedAt", "ledgerObservedAt", "targetCommitted"}
var reportErrorHeader = []string{"parentTaskId", "childTaskId", "direction", "logicalKey", "size", "node", "reasonCode", "errorMessage", "timestamp"}
var reportSummaryHeader = []string{"rowType", "parentTaskId", "childTaskId", "direction", "node", "status", "sourceEndpoint", "destinationEndpoint", "sourcePrefix", "startKey", "endKey", "objectKey", "targetRelativePath", "dateAfter", "dateBefore", "overwrite", "conflictPolicy", "distributionMode", "scannedCount", "scannedBytes", "successCount", "successBytes", "skippedCount", "skippedBytes", "failedCount", "failedBytes", "createTime", "startTime", "endTime", "keyMappingMode", "keyMappingSourcePrefix", "keyMappingTargetPrefix", "keyMappingTargetKey", "requestLimitScope", "listQpsLimit", "headQpsLimit", "putQpsLimit", "bandwidthBitsPerSecond"}

type reportCoverage struct {
	LegacyHistorical     bool              `json:"legacyHistorical,omitempty"`
	LegacyOriginalStatus string            `json:"legacyOriginalStatus,omitempty"`
	ParentTaskID         int64             `json:"parentTaskId"`
	Complete             bool              `json:"complete"`
	Status               string            `json:"status"`
	ScanComplete         bool              `json:"scanComplete"`
	Stable               bool              `json:"stableSnapshot"`
	CountsReconciled     bool              `json:"countsReconciled"`
	FileRows             int64             `json:"fileRows"`
	ResultCounts         map[string]int64  `json:"resultCounts"`
	ResultBytes          map[string]int64  `json:"resultBytes"`
	PartialReasons       []string          `json:"partialReasons"`
	NodeIssues           map[string]string `json:"nodeIssues"`
}

func newCoverage(p task.Parent) reportCoverage {
	return reportCoverage{ParentTaskID: p.ID, Status: p.Status, ScanComplete: p.ScanComplete, Stable: true, ResultCounts: map[string]int64{"SUCCESS": 0, "SKIPPED": 0, "FAILED": 0, "UNPROCESSED": 0, "UNKNOWN": 0}, ResultBytes: map[string]int64{}, PartialReasons: []string{}, NodeIssues: map[string]string{}}
}
func csvValue(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case time.Time:
		if v.IsZero() {
			return ""
		}
		return v.UTC().Format(time.RFC3339Nano)
	case *time.Time:
		if v == nil {
			return ""
		}
		return csvValue(*v)
	case string:
		if len(v) > 0 && strings.ContainsRune("'=+-@\t\r\n", rune(v[0])) {
			return "'" + v
		}
		return v
	default:
		return fmt.Sprint(value)
	}
}
func reportCSV(writer *csv.Writer, values ...any) error {
	row := make([]string, len(values))
	for i, v := range values {
		row[i] = csvValue(v)
	}
	return writer.Write(row)
}
func reportWriter(zipper *zip.Writer, name string) (*csv.Writer, error) {
	entry, err := zipper.Create(name)
	if err != nil {
		return nil, err
	}
	writer := csv.NewWriter(entry)
	writer.UseCRLF = true
	return writer, nil
}
func flushReport(writer *csv.Writer) error { writer.Flush(); return writer.Error() }
func reportEndpoint(ep execution.Endpoint) string {
	if ep.Kind == "LOCAL" {
		return "LOCAL:" + ep.Root
	}
	return "OSS:" + ep.Ref + "/" + ep.Bucket
}

func reportRequestLimits(limits *execution.RequestLimits) []any {
	if limits == nil {
		return []any{"", "", "", "", ""}
	}
	scope := limits.Scope
	if scope == "" {
		scope = "CLUSTER"
	}
	decimal := func(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
	return []any{scope, decimal(limits.ListQPS), decimal(limits.HeadQPS), decimal(limits.PutQPS), decimal(limits.BandwidthBitsPerSecond)}
}

func summaryCSV(writer *csv.Writer, p task.Parent, c *task.Child) error {
	rowType, childID, node, status := "TASK", "", "", p.Status
	src, dst := p.Spec.Source, p.Spec.Destination
	counters := p.Counters
	scanned, scannedBytes := max(p.ScannedObjects, p.DispatchedObjects), max(p.ScannedBytes, p.DispatchedBytes, p.Counters.TotalBytes)
	created := any(p.CreatedAt)
	started, finished := any(p.StartedAt), any(p.FinishedAt)
	if c != nil {
		rowType, childID, node, status = "CHILD_TASK", c.Job.ChildID, c.Node.Address, "PENDING"
		src, dst = c.Job.Source, c.Job.Destination
		counters = execution.Counters{}
		scanned = int64(len(c.Job.Items))
		scannedBytes = 0
		for _, item := range c.Job.Items {
			scannedBytes += item.Source.Size
		}
		created, started, finished = nil, nil, nil
		if c.Record != nil {
			status = string(c.Record.Status)
			counters = c.Record.Counters
			scannedBytes = max(scannedBytes, counters.TotalBytes)
		}
	}
	targetRelative := ""
	if p.Spec.Direction == "OSS_TO_LOCAL" {
		targetRelative = p.Spec.TargetKey
	}
	values := []any{rowType, p.ID, childID, p.Spec.Direction, node, status, reportEndpoint(src), reportEndpoint(dst), p.Spec.Prefix, p.Spec.StartKey, p.Spec.EndKey, p.Spec.ObjectKey, targetRelative, p.Spec.ModifiedFrom, p.Spec.ModifiedUntil, p.Spec.ConflictPolicy == "OVERWRITE", p.Spec.ConflictPolicy, p.Spec.DistributionMode, scanned, scannedBytes, counters.Succeeded, counters.SucceededBytes, counters.Skipped, counters.SkippedBytes, counters.Failed, counters.FailedBytes, created, started, finished, p.Spec.MappingMode, p.Spec.ReplaceSourcePrefix, p.Spec.TargetPrefix, p.Spec.TargetKey}
	return reportCSV(writer, append(values, reportRequestLimits(p.Spec.RequestLimits)...)...)
}

func (s *Server) walkReportChildren(ctx context.Context, p task.Parent, visit func(task.Child) error) error {
	after := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		children, err := s.tasks.repo.Children(p.ID, after, 128)
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := visit(child); err != nil {
				return err
			}
			after = child.Job.Number
		}
		if len(children) < 128 {
			return nil
		}
	}
}

func (s *Server) writeTaskReport(ctx context.Context, output io.Writer, exceptions *os.File, parents []task.Parent) error {
	zipper := zip.NewWriter(output)
	summary, err := reportWriter(zipper, "task-summary.csv")
	if err != nil {
		return err
	}
	if err := summary.Write(reportSummaryHeader); err != nil {
		return err
	}
	for _, p := range parents {
		if err := summaryCSV(summary, p, nil); err != nil {
			return err
		}
		if task.Historical(p) {
			if err := s.walkLegacyChildren(ctx, p, func(c task.LegacyChild) error { return legacySummaryCSV(summary, p, c) }); err != nil {
				return err
			}
			continue
		}
		if err := s.walkReportChildren(ctx, p, func(c task.Child) error { return summaryCSV(summary, p, &c) }); err != nil {
			return err
		}
	}
	if err := flushReport(summary); err != nil {
		return err
	}
	files, err := reportWriter(zipper, "file-results.csv")
	if err != nil {
		return err
	}
	if err := files.Write(reportFileHeader); err != nil {
		return err
	}
	issues := csv.NewWriter(exceptions)
	issues.UseCRLF = true
	if err := issues.Write(reportErrorHeader); err != nil {
		return err
	}
	onlineMembers, err := s.onlineAgents()
	if err != nil {
		return err
	}
	online := map[string]string{}
	for _, member := range onlineMembers {
		online[member.ID] = member.EnrollmentID
	}
	coverages := make([]reportCoverage, 0, len(parents))
	for _, p := range parents {
		if task.Historical(p) {
			coverage, err := s.writeLegacyReport(ctx, p, files, issues)
			if err != nil {
				return err
			}
			coverages = append(coverages, coverage)
			continue
		}
		coverage := newCoverage(p)
		for _, node := range p.Spec.Nodes {
			if online[node.ID] != node.EnrollmentID {
				coverage.NodeIssues[node.Address] = "execution Agent is unavailable in its recorded enrollment"
			}
		}
		var resolvedBytes int64
		err := s.walkReportChildren(ctx, p, func(child task.Child) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			resolved := map[string]*transferstore.Object{}
			if child.Record != nil {
				for _, result := range child.Record.Results {
					if result.ResolvedSource != nil {
						resolved[result.ID] = result.ResolvedSource
						resolvedBytes += result.ResolvedSource.Size
					}
				}
			}
			items, observed, readErr := s.reportChild(ctx, p, child, online, coverage.NodeIssues)
			if readErr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				coverage.NodeIssues[child.Node.Address] = readErr.Error()
				for _, item := range child.Job.Items {
					if err := fileCSV(files, issues, p, child, item, execution.ItemResult{ID: item.ID, Error: readErr.Error(), ResolvedSource: resolved[item.ID]}, "UNKNOWN", "LEDGER_UNAVAILABLE", time.Time{}); err != nil {
						return err
					}
					coverage.FileRows++
					coverage.ResultCounts["UNKNOWN"]++
					coverage.ResultBytes["UNKNOWN"] += execution.EffectiveSource(item, execution.ItemResult{ResolvedSource: resolved[item.ID]}).Size
				}
				return nil
			}
			for _, entry := range items {
				result, reason := string(entry.Result.Status), ""
				switch entry.Result.Status {
				case execution.ItemPending:
					result = "UNPROCESSED"
					reason = "NOT_COMPLETED"
				case execution.ItemFailed:
					reason = "TRANSFER_FAILED"
				case execution.ItemSkipped:
					reason = "DESTINATION_EXISTS"
				}
				if err := fileCSV(files, issues, p, child, entry.Item, entry.Result, result, reason, observed); err != nil {
					return err
				}
				coverage.FileRows++
				coverage.ResultCounts[result]++
				coverage.ResultBytes[result] += execution.EffectiveSource(entry.Item, entry.Result).Size
			}
			return nil
		})
		if err != nil {
			return err
		}
		current, err := s.tasks.repo.Get(p.ID)
		if err != nil {
			return err
		}
		coverage.Stable = current.Generation == p.Generation && current.Status == p.Status && current.Deleted == p.Deleted && current.Purged == p.Purged && current.ChildCount == p.ChildCount && current.ScanComplete == p.ScanComplete && current.Counters == p.Counters
		coverage.CountsReconciled = coverage.ResultCounts["UNKNOWN"] == 0 && coverage.FileRows == p.DispatchedObjects && coverage.ResultCounts["SUCCESS"] == p.Counters.Succeeded && coverage.ResultCounts["SKIPPED"] == p.Counters.Skipped && coverage.ResultCounts["FAILED"] == p.Counters.Failed && coverage.ResultBytes["SUCCESS"] == p.Counters.SucceededBytes && coverage.ResultBytes["SKIPPED"] == p.Counters.SkippedBytes && coverage.ResultBytes["FAILED"] == p.Counters.FailedBytes
		var fileBytes int64
		for _, bytes := range coverage.ResultBytes {
			fileBytes += bytes
		}
		coverage.CountsReconciled = coverage.CountsReconciled && fileBytes == p.DispatchedBytes+resolvedBytes && (!p.ScanComplete || (coverage.FileRows == p.ScannedObjects && fileBytes == p.ScannedBytes+resolvedBytes))
		if !p.ScanComplete {
			coverage.PartialReasons = append(coverage.PartialReasons, "SCAN_INCOMPLETE")
		}
		if p.Status == "STOPPED" {
			coverage.PartialReasons = append(coverage.PartialReasons, "TASK_STOPPED")
		}
		if coverage.ResultCounts["UNPROCESSED"] > 0 {
			coverage.PartialReasons = append(coverage.PartialReasons, "UNPROCESSED_OBJECTS")
		}
		if len(coverage.NodeIssues) > 0 {
			coverage.PartialReasons = append(coverage.PartialReasons, "NODE_REPORT_INCOMPLETE")
		}
		if !coverage.Stable {
			coverage.PartialReasons = append(coverage.PartialReasons, "STATE_CHANGED_DURING_EXPORT")
		}
		if !coverage.CountsReconciled {
			coverage.PartialReasons = append(coverage.PartialReasons, "COUNTS_DO_NOT_RECONCILE")
		}
		if p.Error != "" {
			coverage.PartialReasons = append(coverage.PartialReasons, "TASK_ERROR")
			if err := reportCSV(issues, p.ID, "", p.Spec.Direction, "", "", "", "TASK_ERROR", p.Error, s.options.Now()); err != nil {
				return err
			}
		}
		for node, message := range coverage.NodeIssues {
			if err := reportCSV(issues, p.ID, "", p.Spec.Direction, "", "", node, "NODE_REPORT_INCOMPLETE", message, s.options.Now()); err != nil {
				return err
			}
		}
		coverage.Complete = len(coverage.PartialReasons) == 0
		coverages = append(coverages, coverage)
	}
	if err := flushReport(files); err != nil {
		return err
	}
	if err := flushReport(issues); err != nil {
		return err
	}
	if _, err := exceptions.Seek(0, io.SeekStart); err != nil {
		return err
	}
	entry, err := zipper.Create("exception-details.csv")
	if err != nil {
		return err
	}
	if _, err := io.Copy(entry, exceptions); err != nil {
		return err
	}
	complete := true
	unreachable := map[string]bool{}
	for _, coverage := range coverages {
		complete = complete && coverage.Complete
		for node := range coverage.NodeIssues {
			unreachable[node] = true
		}
	}
	unreachableNodes := make([]string, 0, len(unreachable))
	for node := range unreachable {
		unreachableNodes = append(unreachableNodes, node)
	}
	sort.Strings(unreachableNodes)
	metadata := map[string]any{"schemaVersion": 3, "generatedAt": s.options.Now().UTC(), "complete": complete, "contentVerified": false, "tasks": coverages, "unreachableNodes": unreachableNodes, "completenessMeaning": "Complete covers the task's discovered objects, not an independent inventory or checksum verification.", "timestampMeaning": "ledgerObservedAt is the child ledger update time. Per-object discovery and completion timestamps are not recorded.", "csvStringEscaping": "Strings starting with apostrophe, =, +, -, @, TAB, CR or LF gain one leading apostrophe. Remove that added apostrophe after CSV parsing to recover the original value."}
	for _, coverage := range coverages {
		if coverage.LegacyHistorical {
			metadata["containsLegacyHistory"] = true
			metadata["timestampMeaning"] = "Legacy rows retain recorded discovery/completion timestamps when available. Native Go rows use ledgerObservedAt. Empty fields were not recorded."
			metadata["legacyContentMeaning"] = "V4.0.x object results are retained historical records. Their source and destination contents have not been reverified by this export."
			break
		}
	}
	if len(coverages) == 1 {
		c := coverages[0]
		metadata["parentTaskId"] = c.ParentTaskID
		metadata["resultCounts"] = c.ResultCounts
		metadata["resultBytes"] = c.ResultBytes
		metadata["partialReasons"] = c.PartialReasons
		metadata["fileRows"] = c.FileRows
		metadata["scanComplete"] = c.ScanComplete
		metadata["stableSnapshot"] = c.Stable
		metadata["countsReconciled"] = c.CountsReconciled
	}
	entry, err = zipper.Create("report-metadata.json")
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(entry)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(metadata); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return zipper.Close()
}

func (s *Server) reportChild(ctx context.Context, p task.Parent, child task.Child, online map[string]string, nodeIssues map[string]string) ([]agentservice.ReportItem, time.Time, error) {
	if previous := nodeIssues[child.Node.Address]; previous != "" {
		return nil, time.Time{}, errors.New(previous)
	}
	if online[child.Node.ID] != child.Node.EnrollmentID {
		return nil, time.Time{}, errors.New("execution Agent is unavailable in its recorded enrollment")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var page agentservice.ReportPage
	err := s.agentCall(ctx, nodeSession(child.Node), "report_page", false, agentservice.ReportRequest{ParentID: p.ID, Generation: p.Generation, ChildID: child.Job.ChildID, Limit: 128}, &page)
	if err != nil {
		return nil, time.Time{}, err
	}
	if page.ParentID != p.ID || page.Generation != p.Generation || page.ChildID != child.Job.ChildID || page.JobDigest != agentservice.ReportJobDigest(child.Job) || page.Total != len(child.Job.Items) || page.Offset != 0 || !page.Done || page.NextOffset != len(child.Job.Items) || len(page.Items) != len(child.Job.Items) {
		return nil, time.Time{}, errors.New("Agent returned a different or incomplete child ledger")
	}
	if page.Status != execution.Completed && page.Status != execution.Failed && page.Status != execution.PartialFailed && page.Status != execution.Stopped {
		return nil, time.Time{}, errors.New("Agent child is not ended")
	}
	for index, entry := range page.Items {
		if !reflect.DeepEqual(entry.Item, child.Job.Items[index]) || entry.Result.ID != entry.Item.ID {
			return nil, time.Time{}, errors.New("Agent returned an unrelated object result")
		}
		switch entry.Result.Status {
		case execution.ItemSucceeded, execution.ItemSkipped, execution.ItemFailed, execution.ItemPending:
		default:
			return nil, time.Time{}, errors.New("Agent returned an unknown object status")
		}
	}
	record := execution.Record{Job: child.Job}
	for _, entry := range page.Items {
		record.Results = append(record.Results, entry.Result)
	}
	if err := record.ValidateResolvedSources(p.Spec.InventoryID != "", child.Record); err != nil {
		return nil, time.Time{}, err
	}
	return page.Items, page.ObservedAt, nil
}

func fileCSV(files, issues *csv.Writer, p task.Parent, child task.Child, item execution.Item, result execution.ItemResult, status, reason string, observed time.Time) error {
	item.Source = execution.EffectiveSource(item, result)
	if result.Reconciliation != "" {
		if reason != "" {
			reason += ";"
		}
		reason += result.Reconciliation
	}
	src, dst := child.Job.Source, child.Job.Destination
	sourceKey, sourcePath, destinationKey, destinationPath := item.Source.Key, "", item.DestinationKey, ""
	if src.Kind == "LOCAL" {
		sourcePath = joinAgentPath(src.Root, sourceKey)
		sourceKey = ""
	}
	if dst.Kind == "LOCAL" {
		destinationPath = joinAgentPath(dst.Root, destinationKey)
		destinationKey = ""
	}
	if err := reportCSV(files, p.ID, child.Job.ChildID, p.Spec.Direction, item.Source.Key, src.Ref, src.Bucket, sourceKey, sourcePath, dst.Ref, dst.Bucket, destinationKey, destinationPath, item.Source.Size, child.Node.Address, status, reason, result.Error, nil, nil, observed, strconv.FormatBool(result.TargetCommitted)); err != nil {
		return err
	}
	if status == "FAILED" || status == "UNKNOWN" {
		return reportCSV(issues, p.ID, child.Job.ChildID, p.Spec.Direction, item.Source.Key, item.Source.Size, child.Node.Address, reason, result.Error, observed)
	}
	return nil
}
