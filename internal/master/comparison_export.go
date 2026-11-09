package master

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/comparison"
)

// Export timing: variables only so that tests can shorten them.
var (
	// comparisonExportCallTimeout bounds one page request to the Agent.
	comparisonExportCallTimeout = 30 * time.Second
	// comparisonExportRetryDelays are the waits before repeating a page
	// request that failed transiently; their count is the number of retries.
	comparisonExportRetryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
)

const comparisonCSVEscaping = "Strings starting with apostrophe, =, +, -, @, TAB, CR or LF gain one leading apostrophe. Remove that added apostrophe after CSV parsing to recover the original value. Values containing CR or LF are quoted with those bytes kept, but some CSV readers turn an embedded CR LF into LF; the JSON Lines export holds every key exactly."

// comparisonResultsUnavailable is the Agent's Progress.Error for a
// comparison whose stored rows are gone from its node.
const comparisonResultsUnavailable = "Comparison results are unavailable on this node"

var comparisonDifferencesHeader = []string{"number", "key", "status", "sourceKey", "destinationKey", "sourceSize", "destinationSize", "sourceSha256", "destinationSha256", "reasons", "error", "errorDetail"}

// A comparisonTicket lets a browser download one export through a plain form
// POST. It is single-use, short-lived and subordinate to the login session
// that requested it, like the task report tickets.
type comparisonTicket struct {
	id       string
	format   string
	statuses []string
	session  string
	expires  time.Time
}

type comparisonTickets struct {
	mu     sync.Mutex
	issued map[string]comparisonTicket
}

const comparisonMaxTickets = 2048

func comparisonExportQuery(w http.ResponseWriter, r *http.Request) (string, []string, bool) {
	query := r.URL.Query()
	format := strings.ToLower(strings.TrimSpace(query.Get("format")))
	if format == "" {
		format = "zip"
	}
	if format != "zip" && format != "jsonl" {
		fail(w, 400, "Export format must be zip or jsonl")
		return "", nil, false
	}
	statuses, ok := comparisonStatuses(query["status"])
	if !ok {
		fail(w, 400, "Unknown comparison result status")
		return "", nil, false
	}
	return format, statuses, true
}

func (c *comparisonAPI) export(w http.ResponseWriter, r *http.Request) {
	format, statuses, ok := comparisonExportQuery(w, r)
	if !ok {
		return
	}
	c.streamExport(w, r, r.PathValue("id"), format, statuses)
}

func (c *comparisonAPI) issueExportTicket(w http.ResponseWriter, r *http.Request) {
	format, statuses, ok := comparisonExportQuery(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, ok := c.comparisonMember(w, id, false); !ok {
		return
	}
	value, err := randomToken()
	if err != nil {
		fail(w, 500, "Cannot create download ticket")
		return
	}
	c.tickets.mu.Lock()
	now := c.now()
	for key, ticket := range c.tickets.issued {
		if !now.Before(ticket.expires) {
			delete(c.tickets.issued, key)
		}
	}
	if len(c.tickets.issued) >= comparisonMaxTickets {
		c.tickets.mu.Unlock()
		fail(w, 429, "Too many unused download tickets")
		return
	}
	c.tickets.issued[tokenKey(value)] = comparisonTicket{id: id, format: format, statuses: statuses, session: bearer(r), expires: now.Add(2 * time.Minute)}
	c.tickets.mu.Unlock()
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]string{"ticket": value}})
}

func (c *comparisonAPI) consumeExportTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	c.tickets.mu.Lock()
	ticket, found := c.tickets.issued[tokenKey(values[0])]
	delete(c.tickets.issued, tokenKey(values[0]))
	c.tickets.mu.Unlock()
	if !found || ticket.id != id || !c.now().Before(ticket.expires) {
		fail(w, 401, "Invalid or expired download ticket")
		return
	}
	// Logging out or changing the account password revokes even an unused
	// download ticket.
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+ticket.session)
	user, valid, err := c.server.authenticated(clone)
	if err != nil {
		fail(w, 500, "Cannot validate download session")
		return
	}
	if !valid || user.Role != "admin" || user.MustChangePassword {
		fail(w, 401, "Sign in required")
		return
	}
	c.streamExport(w, r, id, ticket.format, ticket.statuses)
}

// streamExport validates the comparison with its first page while errors can
// still be JSON, then streams the file. A later failure resets the connection
// instead of ending a truncated file as if it were complete.
func (c *comparisonAPI) streamExport(w http.ResponseWriter, r *http.Request, id, format string, statuses []string) {
	member, ok := c.comparisonMember(w, id, false)
	if !ok {
		return
	}
	ctx := r.Context()
	source := &comparisonRowSource{api: c, session: member.Session(), id: id}
	var err error
	if source.protocol, err = c.comparisonProtocol(ctx, source.session); err != nil {
		fail(w, 502, err.Error())
		return
	}
	first, err := source.page(ctx, 0, statuses)
	if err != nil {
		c.agentFailed(w, id, err)
		return
	}
	if !validComparisonPage(first, 0) {
		fail(w, 502, errComparisonPage.Error())
		return
	}
	source.snapshot = first.Progress
	comparisonTruncation(&source.snapshot)
	name := "comparison-" + comparisonFileName(id)
	w.Header().Set("Cache-Control", "no-store")
	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.jsonl"`)
	} else {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.zip"`)
	}
	// Start the download now: a browser shows it at once, and a client never
	// mistakes a later reset for a connection that failed before the reply.
	w.WriteHeader(http.StatusOK)
	_ = http.NewResponseController(w).Flush()
	if format == "jsonl" {
		err = source.writeJSONL(ctx, w, statuses, &first)
	} else {
		err = c.writeComparisonZIP(ctx, w, source, &first, member, statuses)
	}
	if err != nil {
		panic(http.ErrAbortHandler)
	}
}

// comparisonFileName keeps a download name to characters that need no
// quoting. Master-created IDs already are.
func comparisonFileName(id string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '_'
	}, id)
}

// comparisonRowSource reads the stored rows of one comparison for an export.
type comparisonRowSource struct {
	api      *comparisonAPI
	session  agenttransport.Session
	id       string
	protocol int
	// snapshot is the Progress returned with the first page. Rows numbered
	// after its Rows were stored later and are left out, so all parts of an
	// export describe the same moment even while the comparison runs.
	snapshot comparison.Progress
}

// page reads one page, repeating a transiently failed request after
// comparisonExportRetryDelays.
func (s *comparisonRowSource) page(ctx context.Context, after int64, statuses []string) (comparison.Page, error) {
	for attempt := 0; ; attempt++ {
		reply, err := s.api.fetchComparisonRows(ctx, s.session, &s.protocol, comparison.RowsQuery{ID: s.id, After: after, Limit: comparison.MaxPageRows, Statuses: statuses, MaxBytes: comparison.MaxPageBytes}, comparisonExportCallTimeout)
		if err == nil {
			return reply, nil
		}
		if ctx.Err() != nil {
			return reply, ctx.Err()
		}
		if attempt >= len(comparisonExportRetryDelays) || !comparisonTransient(err) {
			return reply, err
		}
		timer := time.NewTimer(comparisonExportRetryDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return reply, ctx.Err()
		case <-timer.C:
		}
	}
}

// comparisonTransient reports a failed Agent call worth repeating: a timeout,
// a full command queue or an Agent-side server error.
func comparisonTransient(err error) bool {
	var problem *AgentError
	if errors.As(err, &problem) {
		switch problem.Status {
		case 408, 429, 500, 502, 503, 504:
			return true
		}
		return false
	}
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, agenttransport.ErrQueueFull)
}

// walk visits, in row order, every stored row of the snapshot whose status is
// in statuses (all when empty). first, when set, is the already fetched page
// after row 0 for the same statuses.
func (s *comparisonRowSource) walk(ctx context.Context, statuses []string, first *comparison.Page, visit func(comparison.Row) error) error {
	filter := comparisonStatusSet(statuses)
	bound := s.snapshot.Rows
	var cursor int64
	for {
		var page comparison.Page
		if first != nil {
			page, first = *first, nil
		} else {
			var err error
			if page, err = s.page(ctx, cursor, statuses); err != nil {
				return err
			}
			if !validComparisonPage(page, cursor) {
				return errComparisonPage
			}
		}
		for _, row := range page.Items {
			if row.Number > bound {
				return nil
			}
			if filter != nil && !filter[row.Status] {
				continue
			}
			if err := visit(row); err != nil {
				return err
			}
		}
		if page.NextAfter == 0 || page.NextAfter >= bound {
			return nil
		}
		cursor = page.NextAfter
	}
}

func (s *comparisonRowSource) writeJSONL(ctx context.Context, output io.Writer, statuses []string, first *comparison.Page) error {
	buffered := bufio.NewWriterSize(output, 64<<10)
	encoder := json.NewEncoder(buffered)
	encoder.SetEscapeHTML(false)
	if err := s.walk(ctx, statuses, first, func(row comparison.Row) error { return encoder.Encode(row) }); err != nil {
		return err
	}
	return buffered.Flush()
}

// importableKey reports whether import-inventory reads key back as the same
// object key: 1..1024 bytes of UTF-8 without NUL or line breaks. It skips a
// line that is only whitespace, so such a key would be lost silently.
func importableKey(key string) bool {
	return len(key) <= 1024 && utf8.ValidString(key) && !strings.ContainsAny(key, "\x00\r\n") && strings.TrimSpace(key) != ""
}

type comparisonExportEndpoint struct {
	Connection string `json:"connection"`
	Bucket     string `json:"bucket"`
	Prefix     string `json:"prefix"`
}

type comparisonExportCounts struct {
	Compared     int64 `json:"compared"`
	Matched      int64 `json:"matched"`
	Different    int64 `json:"different"`
	ETagMismatch int64 `json:"etagMismatch"`
	Missing      int64 `json:"missing"`
	Extra        int64 `json:"extra"`
	Changed      int64 `json:"changed"`
	Errors       int64 `json:"errors"`
	BothMissing  int64 `json:"bothMissing"`
}

type comparisonExportNode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// comparisonExportSummary is summary.json in an export ZIP.
type comparisonExportSummary struct {
	ComparisonID       string                   `json:"comparisonId"`
	Node               comparisonExportNode     `json:"node"`
	Status             string                   `json:"status"`
	Mode               string                   `json:"mode"`
	Scope              string                   `json:"scope"`
	Source             comparisonExportEndpoint `json:"source"`
	Destination        comparisonExportEndpoint `json:"destination"`
	Counts             comparisonExportCounts   `json:"counts"`
	StoredRows         int64                    `json:"storedRows"`
	ExportedRows       int64                    `json:"exportedRows"`
	StatusFilter       []string                 `json:"statusFilter"`
	Complete           bool                     `json:"complete"`
	IncompleteReasons  []string                 `json:"incompleteReasons"`
	TruncatedReason    string                   `json:"truncatedReason"`
	LastStoredKey      string                   `json:"lastStoredKey"`
	RetryStatuses      []string                 `json:"retryStatuses"`
	RetryKeys          int64                    `json:"retryKeys"`
	RetryKeysOmitted   int64                    `json:"retryKeysOmitted"`
	RetryPrefixesMatch bool                     `json:"retryPrefixesMatch"`
	CSVEscaping        string                   `json:"csvEscaping"`
	StartedAt          time.Time                `json:"startedAt"`
	EndedAt            *time.Time               `json:"endedAt"`
	GeneratedAt        time.Time                `json:"generatedAt"`
}

// comparisonStatusCount is the number of compared objects whose result is
// one of statuses.
func comparisonStatusCount(p comparison.Progress, statuses []string) int64 {
	var total int64
	for _, status := range statuses {
		switch status {
		case comparison.StatusDifferent:
			total += p.Different
		case comparison.StatusETagMismatch:
			total += p.ETagMismatch
		case comparison.StatusMissingTarget:
			total += p.Missing
		case comparison.StatusExtraTarget:
			total += p.Extra
		case comparison.StatusChanged:
			total += p.Changed
		case comparison.StatusError:
			total += p.Errors
		case comparison.StatusBothMissing:
			total += p.BothMissing
		}
	}
	return total
}

func (c *comparisonAPI) writeComparisonZIP(ctx context.Context, output io.Writer, source *comparisonRowSource, first *comparison.Page, member cluster.Member, statuses []string) error {
	buffered := bufio.NewWriterSize(output, 64<<10)
	zipper := zip.NewWriter(buffered)
	entry, err := zipper.Create("differences.csv")
	if err != nil {
		return err
	}
	// Unlike task reports, keep CR and LF inside values byte for byte: keys
	// may contain them, and UseCRLF would drop or rewrite them.
	differences := csv.NewWriter(entry)
	if err := differences.Write(comparisonDifferencesHeader); err != nil {
		return err
	}
	var exported int64
	if err := source.walk(ctx, statuses, first, func(row comparison.Row) error {
		exported++
		return reportCSV(differences, row.Number, row.Key, row.Status, row.SourceKey, row.DestinationKey, row.SourceSize, row.DestinationSize, row.SourceSHA256, row.DestinationSHA256, strings.Join(row.Reasons, ";"), row.Error, row.ErrorDetail)
	}); err != nil {
		return err
	}
	if err := flushReport(differences); err != nil {
		return err
	}
	entry, err = zipper.Create("retry-source-keys.incr")
	if err != nil {
		return err
	}
	keys := bufio.NewWriter(entry)
	var retryKeys, omitted int64
	// A second pass lists the keys to transfer again whatever the export's
	// own status filter is.
	if err := source.walk(ctx, comparison.RetryStatuses, nil, func(row comparison.Row) error {
		if !importableKey(row.SourceKey) {
			omitted++
			return nil
		}
		retryKeys++
		_, err := keys.WriteString(row.SourceKey + "\n")
		return err
	}); err != nil {
		return err
	}
	if err := keys.Flush(); err != nil {
		return err
	}
	p := source.snapshot
	summary := comparisonExportSummary{
		ComparisonID: p.ID,
		Node:         comparisonExportNode{ID: member.ID, Name: c.server.nodeDisplaySnapshot().lookup(member.ID, member.Address).DisplayLabel},
		Status:       p.Status,
		Mode:         p.Mode,
		Scope:        p.Scope,
		Source:       comparisonExportEndpoint{Connection: p.Source.Ref, Bucket: p.Source.Bucket, Prefix: p.SourcePrefix},
		Destination:  comparisonExportEndpoint{Connection: p.Destination.Ref, Bucket: p.Destination.Bucket, Prefix: p.DestinationPrefix},
		Counts: comparisonExportCounts{
			Compared: p.Compared, Matched: p.Matched, Different: p.Different, ETagMismatch: p.ETagMismatch,
			Missing: p.Missing, Extra: p.Extra, Changed: p.Changed, Errors: p.Errors, BothMissing: p.BothMissing,
		},
		StoredRows:         p.Rows,
		ExportedRows:       exported,
		StatusFilter:       append([]string{}, statuses...),
		IncompleteReasons:  []string{},
		TruncatedReason:    p.TruncatedReason,
		LastStoredKey:      p.LastStoredKey,
		RetryStatuses:      append([]string{}, comparison.RetryStatuses...),
		RetryKeys:          retryKeys,
		RetryKeysOmitted:   omitted,
		RetryPrefixesMatch: p.SourcePrefix == p.DestinationPrefix,
		CSVEscaping:        comparisonCSVEscaping,
		StartedAt:          p.StartedAt.UTC(),
		GeneratedAt:        c.now().UTC(),
	}
	if !p.EndedAt.IsZero() {
		ended := p.EndedAt.UTC()
		summary.EndedAt = &ended
	}
	if p.Status != comparison.StateCompleted {
		summary.IncompleteReasons = append(summary.IncompleteReasons, "NOT_COMPLETED:"+p.Status)
	}
	if p.RowsTruncated {
		summary.IncompleteReasons = append(summary.IncompleteReasons, "ROWS_TRUNCATED:"+p.TruncatedReason)
	}
	if p.Error == comparisonResultsUnavailable {
		summary.IncompleteReasons = append(summary.IncompleteReasons, "RESULTS_UNAVAILABLE")
	}
	// Results of Agents before comparison.Protocol counted BOTH_MISSING rows
	// as errors, so only current results are checked against the counts.
	current := p.ResultsProtocol >= comparison.Protocol && !p.RowsTruncated
	if p.Rows != p.Compared-p.Matched || len(statuses) == 0 && exported != p.Rows || current && len(statuses) > 0 && exported != comparisonStatusCount(p, statuses) {
		summary.IncompleteReasons = append(summary.IncompleteReasons, "ROW_COUNT_MISMATCH")
	}
	if current && retryKeys+omitted != p.Missing+p.Different+p.ETagMismatch {
		summary.IncompleteReasons = append(summary.IncompleteReasons, "RETRY_KEYS_MISMATCH")
	}
	summary.Complete = len(summary.IncompleteReasons) == 0
	entry, err = zipper.Create("summary.json")
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(entry)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(summary); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := zipper.Close(); err != nil {
		return err
	}
	return buffered.Flush()
}
