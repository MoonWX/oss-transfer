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
type Options struct {
	ListQPS, PutQPS, HeadQPS, BandwidthBitsPerSecond float64
	UseZeroDefaults                                  bool
	Threshold                                        int
	ReduceFactor, IncreaseFactor, MinQPS             float64
	RecoveryInterval                                 time.Duration
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
	ID                      string          `json:"id"`
	ProcessID               string          `json:"processId"`
	Sequence                uint64          `json:"sequence"`
	Type                    string          `json:"type"`
	Value                   float64         `json:"value"`
	PreviousValue           float64         `json:"previousValue"`
	ManualTarget            float64         `json:"manualTarget"`
	PreviousManualTarget    float64         `json:"previousManualTarget"`
	Manual                  bool            `json:"manual"`
	Timestamp               int64           `json:"timestamp"`
	ReasonCode              string          `json:"reasonCode"`
	IncidentID              string          `json:"incidentId,omitempty"`
	DetailsAvailable        bool            `json:"detailsAvailable"`
	Consecutive503Count     int             `json:"consecutive503Count,omitempty"`
	Consecutive503Threshold int             `json:"consecutive503Threshold,omitempty"`
	Factor                  float64         `json:"factor,omitempty"`
	MinQPS                  float64         `json:"minQps,omitempty"`
	IntervalMS              int64           `json:"intervalMs"`
	IncreaseFactor          float64         `json:"increaseFactor"`
	TriggerRequest          *RequestContext `json:"triggerRequest,omitempty"`
}

type persisted struct {
	Overrides map[string]float64    `json:"overrides"`
	Effective map[string]float64    `json:"effective"`
	Last      map[string]Adjustment `json:"last"`
	Incidents map[string]string     `json:"incidents"`
	Roots     []string              `json:"advertisedLocalRoots"`
	Sequence  uint64                `json:"sequence"`
}

type Config struct {
	AdvertisedLocalRoots []string `json:"advertisedLocalRoots"`
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
