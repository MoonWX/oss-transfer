// Package nodecontrol owns Agent-wide request limits and observable adjustments.
// Its request budgets are shared by every parent running on the same Agent.
package nodecontrol

import (
	"context"
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/ratelimit"
)

var (
	ErrInvalid  = errors.New("invalid node setting")
	ErrClosed   = errors.New("node controller is closed")
	ErrNotFound = errors.New("adjustment not found")
)

type ByteLimiter interface {
	Node() ratelimit.NodeLimit
	ApplyNode(context.Context, ratelimit.NodeLimit) error
}

// All bandwidth values exposed to the legacy UI are bits/second. The shared
// execution limiter uses bytes/second; conversion happens only at this boundary.
//
// Automatic adjustment counts throttling responses (HTTP 429/503 or a
// throttling error code) per request limit over ThrottleWindow. A limit is
// lowered once the window holds at least Threshold of them and they are at
// least ThrottleRatio of that limit's responses, then not again for one window.
type Options struct {
	ListQPS, PutQPS, HeadQPS, BandwidthBitsPerSecond float64
	// Local storage limits: file operations per second and bits per second
	// read from or written to local files. Zero, the default, is unlimited.
	LocalOpsPerSecond, LocalBandwidthBitsPerSecond float64
	UseZeroDefaults                                bool
	Threshold                                      int
	ThrottleRatio                                  float64
	ThrottleWindow                                 time.Duration
	ReduceFactor, IncreaseFactor, MinQPS           float64
	RecoveryInterval                               time.Duration
	// Logf writes service log lines, such as a saved setting that New
	// ignored. Nil discards them.
	Logf func(format string, args ...any)
}

type RequestContext struct {
	Generation        uint64 `json:"generation,omitempty"`
	PathStyle         bool   `json:"-"`
	Operation         string `json:"operation,omitempty"`
	Direction         string `json:"direction,omitempty"`
	Method            string `json:"method,omitempty"`
	TaskID            int64  `json:"taskId,omitempty"`
	ConnectionAlias   string `json:"connectionAlias,omitempty"`
	Bucket            string `json:"bucket,omitempty"`
	Key               string `json:"key,omitempty"`
	PartNumber        int    `json:"partNumber,omitempty"`
	StatusCode        int    `json:"statusCode,omitempty"`
	ErrorCode         string `json:"errorCode,omitempty"`
	RequestID         string `json:"requestId,omitempty"`
	ExtendedRequestID string `json:"extendedRequestId,omitempty"`
	Message           string `json:"message,omitempty"`
	ContextSource     string `json:"contextSource,omitempty"`
}

type Adjustment struct {
	ID                   string  `json:"id"`
	ProcessID            string  `json:"processId"`
	Sequence             uint64  `json:"sequence"`
	Type                 string  `json:"type"`
	Value                float64 `json:"value"`
	PreviousValue        float64 `json:"previousValue"`
	ManualTarget         float64 `json:"manualTarget"`
	PreviousManualTarget float64 `json:"previousManualTarget"`
	Manual               bool    `json:"manual"`
	Timestamp            int64   `json:"timestamp"`
	ReasonCode           string  `json:"reasonCode"`
	IncidentID           string  `json:"incidentId,omitempty"`
	DetailsAvailable     bool    `json:"detailsAvailable"`
	// Reductions saved before the throttle window record consecutive 503s.
	Consecutive503Count     int `json:"consecutive503Count,omitempty"`
	Consecutive503Threshold int `json:"consecutive503Threshold,omitempty"`
	// Evidence of a reduction: throttling and all responses of the limit in
	// the window, the rule's settings, and the peak admitted rate it started from.
	ThrottleCount     int64           `json:"throttleCount,omitempty"`
	ThrottleResponses int64           `json:"throttleResponses,omitempty"`
	ThrottleThreshold int             `json:"throttleThreshold,omitempty"`
	ThrottleRatio     float64         `json:"throttleRatio,omitempty"`
	ThrottleWindowMS  int64           `json:"throttleWindowMs,omitempty"`
	MeasuredQPS       float64         `json:"measuredQps,omitempty"`
	Factor            float64         `json:"factor,omitempty"`
	MinQPS            float64         `json:"minQps,omitempty"`
	IntervalMS        int64           `json:"intervalMs"`
	IncreaseFactor    float64         `json:"increaseFactor"`
	TriggerRequest    *RequestContext `json:"triggerRequest,omitempty"`
}

type persisted struct {
	Overrides map[string]float64 `json:"overrides"`
	// Saved local storage limits live apart from Overrides: an older Agent
	// rejects a saved rate it does not know, which would stop it starting
	// after a downgrade, but it ignores this field.
	LocalOverrides map[string]float64    `json:"localOverrides,omitempty"`
	Effective      map[string]float64    `json:"effective"`
	Last           map[string]Adjustment `json:"last"`
	Incidents      map[string]string     `json:"incidents"`
	Roots          []string              `json:"advertisedLocalRoots"`
	Sequence       uint64                `json:"sequence"`
	Concurrency    *ConcurrencyConfig    `json:"concurrency,omitempty"`
	// The saved number of concurrent scans lives apart from Concurrency:
	// UpdateConfig saves that structure whole, so a scan change kept there
	// would also save the default child concurrency as a choice. An older
	// Agent ignores this field and drops it when it saves its settings on
	// start, so its scan concurrency is the default again.
	ScanConcurrency *int `json:"scanConcurrency,omitempty"`
}

type Config struct {
	AdvertisedLocalRoots []string `json:"advertisedLocalRoots"`
	ConcurrencyConfig
	// MaxConcurrentScans is how many scans the node runs at once: the saved
	// value, else DefaultScanConcurrency.
	MaxConcurrentScans int `json:"maxConcurrentScans"`
}

type ConcurrencyConfig struct {
	MaxConcurrentChildren int `json:"maxConcurrentChildren"`
	ObjectsPerChild       int `json:"objectsPerChild"`
}

// ConfigChange preserves settings omitted by older clients and rename-only edits.
type ConfigChange struct {
	AdvertisedLocalRoots  *[]string `json:"advertisedLocalRoots,omitempty"`
	MaxConcurrentChildren *int      `json:"maxConcurrentChildren,omitempty"`
	ObjectsPerChild       *int      `json:"objectsPerChild,omitempty"`
	// An Agent older than scan concurrency rejects this field, so the Master
	// sends it only to Agents that advertise scanConcurrencyProtocol.
	MaxConcurrentScans *int `json:"maxConcurrentScans,omitempty"`
}

type Detail struct {
	Event            Adjustment   `json:"event"`
	TriggerEvent     *Adjustment  `json:"triggerEvent"`
	IncidentID       string       `json:"incidentId,omitempty"`
	ProcessRestarted bool         `json:"processRestarted"`
	IncidentActive   bool         `json:"incidentActive"`
	HistoryComplete  bool         `json:"historyComplete"`
	Events           []Adjustment `json:"events"`
	NextCursor       string       `json:"nextCursor,omitempty"`
	Done             bool         `json:"done"`
}
