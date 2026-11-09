package comparison

import (
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

// Protocol is advertised by Agents as the "comparisonProtocol" capability.
// Protocol 2 Agents keep every difference in files under the data directory
// and also serve comparison_rows, comparison_list and comparison_resume.
// Agents without the capability store at most LegacyStoredDifferences rows per
// comparison and accept only comparison_start/get/cancel/delete.
const Protocol = 2

// LegacyStoredDifferences is the fixed row limit of Agents before Protocol 2.
const LegacyStoredDifferences = 10000

// Comparison states.
const (
	StateRunning     = "RUNNING"
	StateCanceling   = "CANCELING"
	StateCanceled    = "CANCELED"
	StateInterrupted = "INTERRUPTED"
	StateFailed      = "FAILED"
	StateCompleted   = "COMPLETED"
)

// Row statuses. Every status except MATCH is stored as a difference row.
const (
	StatusMatch         = "MATCH"
	StatusDifferent     = "DIFFERENT"
	StatusETagMismatch  = "ETAG_MISMATCH"
	StatusMissingTarget = "MISSING_TARGET"
	StatusExtraTarget   = "EXTRA_TARGET"
	StatusChanged       = "CHANGED_DURING_COMPARE"
	StatusError         = "ERROR"
	StatusBothMissing   = "BOTH_MISSING"
)

// RowStatuses lists every stored row status in display order.
var RowStatuses = []string{StatusDifferent, StatusETagMismatch, StatusMissingTarget, StatusExtraTarget, StatusChanged, StatusError, StatusBothMissing}

// RetryStatuses are the statuses whose source keys an export lists for a
// repeated transfer.
var RetryStatuses = []string{StatusMissingTarget, StatusDifferent, StatusETagMismatch}

// Reasons name the compared properties that differ on a DIFFERENT or
// ETAG_MISMATCH row.
const (
	ReasonSize               = "size"
	ReasonContentType        = "contentType"
	ReasonContentEncoding    = "contentEncoding"
	ReasonContentDisposition = "contentDisposition"
	ReasonCacheControl       = "cacheControl"
	ReasonMetadata           = "metadata"
	ReasonSHA256             = "sha256"
	ReasonETag               = "etag"
)

// Reasons why a comparison stopped storing rows. Counts stay exact.
const (
	TruncatedLegacyLimit   = "LEGACY_LIMIT"
	TruncatedStorageBudget = "STORAGE_BUDGET"
	TruncatedLowDiskSpace  = "LOW_DISK_SPACE"
)

// Paging limits. A page holds at least one row when any row matches, even if
// that row alone exceeds MaxPageBytes.
const (
	MaxLegacyPageRows = 200
	MaxPageRows       = 1000
	MaxPageBytes      = 4 << 20
)

// Defaults for Options. A zero option means its default.
const (
	DefaultBudgetBytes      int64 = 2 << 30
	DefaultRetentionDays          = 30
	DefaultMinimumFreeBytes int64 = 1 << 30
	DefaultMaxRetained            = 500
	DefaultWorkers                = 32
	DefaultContentWorkers         = 8
)

var (
	ErrNotFound     = errors.New("comparison not found")
	ErrBusy         = errors.New("another comparison is running on this node")
	ErrRunning      = errors.New("comparison is running")
	ErrNotResumable = errors.New("comparison cannot be resumed")
)

type Request struct {
	ID                string             `json:"id"`
	StorageRevision   uint64             `json:"storageRevision"`
	Source            execution.Endpoint `json:"source"`
	Destination       execution.Endpoint `json:"destination"`
	SourcePrefix      string             `json:"sourcePrefix"`
	DestinationPrefix string             `json:"destinationPrefix"`
	Mode              string             `json:"mode"`
	Scope             string             `json:"scope"`
}

type Row struct {
	Number            int64  `json:"number"`
	Key               string `json:"key"`
	SourceKey         string `json:"sourceKey"`
	DestinationKey    string `json:"destinationKey"`
	Status            string `json:"status"`
	SourceSize        int64  `json:"sourceSize"`
	DestinationSize   int64  `json:"destinationSize"`
	SourceSHA256      string `json:"sourceSha256,omitempty"`
	DestinationSHA256 string `json:"destinationSha256,omitempty"`
	// Error is a fixed English sentence; the Master localizes it.
	Error string `json:"error,omitempty"`
	// Reasons lists Reason* values for DIFFERENT and ETAG_MISMATCH rows.
	Reasons []string `json:"reasons,omitempty"`
	// ErrorDetail is the specific, credential-free cause of an ERROR row, such
	// as "target HEAD: HTTP 403 AccessDenied (request ID 1A2B)".
	ErrorDetail string `json:"errorDetail,omitempty"`
}

type Progress struct {
	Request
	Status       string `json:"status"`
	Compared     int64  `json:"compared"`
	Matched      int64  `json:"matched"`
	Different    int64  `json:"different"`
	Missing      int64  `json:"missing"`
	Extra        int64  `json:"extra"`
	Errors       int64  `json:"errors"`
	Changed      int64  `json:"changed"`
	BothMissing  int64  `json:"bothMissing"`
	ETagMismatch int64  `json:"etagMismatch"`
	// Rows is the number of stored difference rows, numbered 1..Rows.
	Rows          int64 `json:"rows"`
	RowsTruncated bool  `json:"rowsTruncated"`
	// ResultLimit is LegacyStoredDifferences for results written by an Agent
	// before Protocol 2, and 0 (no fixed limit) otherwise.
	ResultLimit     int    `json:"resultLimit"`
	TruncatedReason string `json:"truncatedReason,omitempty"`
	// LastStoredKey is the relative key of the last stored row once
	// RowsTruncated is set.
	LastStoredKey string `json:"lastStoredKey,omitempty"`
	StoredBytes   int64  `json:"storedBytes"`
	// BytesRead counts object content read by content mode on both sides.
	BytesRead int64 `json:"bytesRead"`
	// Checkpoint is the relative key through which every object has been
	// compared and counted. A resumed comparison lists after it.
	Checkpoint    string `json:"checkpoint,omitempty"`
	ElapsedMillis int64  `json:"elapsedMillis"`
	Resumes       int    `json:"resumes,omitempty"`
	Resumable     bool   `json:"resumable"`
	// ResultsProtocol is Protocol for file-backed results, 0 for older ones.
	ResultsProtocol int       `json:"resultsProtocol,omitempty"`
	StartedAt       time.Time `json:"startedAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
	EndedAt         time.Time `json:"endedAt,omitzero"`
	ExpiresAt       time.Time `json:"expiresAt,omitzero"`
	// Error is a fixed English sentence; the Master localizes it.
	Error string `json:"error,omitempty"`
	// ErrorDetail is the specific, credential-free cause of a failure.
	ErrorDetail string `json:"errorDetail,omitempty"`
}

// Page is the reply to comparison_get and comparison_rows. NextAfter is the
// number of the last row examined when stored rows remain after it; continue
// with After = NextAfter. It is 0 when no stored row follows. A filtered page
// may hold fewer than Limit rows, or none, and still have a NextAfter.
type Page struct {
	Progress
	Items     []Row `json:"items"`
	NextAfter int64 `json:"nextAfter,omitempty"`
}

// RowsQuery is the comparison_rows request. Limit is 1..MaxPageRows.
// Statuses, when set, keeps only rows with those statuses. MaxBytes caps the
// encoded rows of one page (default and maximum MaxPageBytes).
type RowsQuery struct {
	ID       string   `json:"id"`
	After    int64    `json:"after"`
	Limit    int      `json:"limit"`
	Statuses []string `json:"statuses,omitempty"`
	MaxBytes int      `json:"maxBytes,omitempty"`
}

// ResumeRequest is the comparison_resume request.
type ResumeRequest struct {
	ID              string `json:"id"`
	StorageRevision uint64 `json:"storageRevision"`
}

// List is the comparison_list reply, newest first, without rows.
type List struct {
	Items []Progress `json:"items"`
}

// Options configures a Manager. Zero values select the defaults above.
type Options struct {
	// Directory holds file-backed results, normally <data directory>/comparisons.
	Directory        string
	BudgetBytes      int64
	RetentionDays    int
	MinimumFreeBytes int64
	MaxRetained      int
	Workers          int
	ContentWorkers   int
	// ResumeFactory returns the stores for automatically resuming r after a
	// restart, or an error when its storage revision is no longer current.
	ResumeFactory func(r Request) (execution.StoreFactory, error)
	Now           func() time.Time
	FreeBytes     func(directory string) (uint64, error)
	Logf          func(format string, args ...any)
}
