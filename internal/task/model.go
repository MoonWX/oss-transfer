// Package task owns Master task metadata. It never opens a source or destination.
package task

import (
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
)

var (
	ErrNotFound       = errors.New("task not found")
	ErrConflict       = errors.New("task idempotency conflict")
	ErrGeneration     = errors.New("task generation changed")
	ErrState          = errors.New("task state does not allow this operation")
	ErrLegacyRecovery = errors.New("legacy task requires migration reconciliation before resuming")
)

type Node struct {
	ID           string `json:"id"`
	EnrollmentID string `json:"enrollmentId"`
	Address      string `json:"address"`
	LocalRoot    string `json:"localRoot,omitempty"`
}

type Spec struct {
	InventoryID         string                   `json:"inventoryId,omitempty"`
	RequestLimits       *execution.RequestLimits `json:"requestLimits,omitempty"`
	Direction           string                   `json:"direction"`
	Source              execution.Endpoint       `json:"source"`
	Destination         execution.Endpoint       `json:"destination"`
	Nodes               []Node                   `json:"nodes"`
	Prefix              string                   `json:"prefix,omitempty"`
	ObjectKey           string                   `json:"objectKey,omitempty"`
	TargetPrefix        string                   `json:"targetPrefix,omitempty"`
	TargetKey           string                   `json:"targetKey,omitempty"`
	MappingMode         string                   `json:"mappingMode,omitempty"`
	ReplaceSourcePrefix string                   `json:"replaceSourcePrefix,omitempty"`
	StartKey            string                   `json:"startKey,omitempty"`
	EndKey              string                   `json:"endKey,omitempty"`
	Marker              string                   `json:"marker,omitempty"`
	SharedLocalSource   bool                     `json:"sharedLocalSource,omitempty"`
	ModifiedFrom        *time.Time               `json:"modifiedFrom,omitempty"`
	ModifiedUntil       *time.Time               `json:"modifiedUntil,omitempty"`
	ConflictPolicy      string                   `json:"conflictPolicy"`
	DistributionMode    string                   `json:"distributionMode"`
	BandwidthMbps       float64                  `json:"bandwidthMbps"`
}

type Parent struct {
	ActiveTime   *execution.ActiveTime `json:"activeTime,omitempty"`
	ResumeTiming *ResumeTiming         `json:"resumeTiming,omitempty"`
	// ResumeAfter is a durable cursor while old child assignments are prepared.
	ResumeAfter       *int               `json:"resumeAfter,omitempty"`
	ID                int64              `json:"id"`
	Spec              Spec               `json:"spec"`
	Generation        uint64             `json:"generation"`
	BudgetRevision    uint64             `json:"budgetRevision"`
	Status            string             `json:"status"`
	CreatedAt         time.Time          `json:"createdAt"`
	StartedAt         *time.Time         `json:"startedAt,omitempty"`
	FinishedAt        *time.Time         `json:"finishedAt,omitempty"`
	ScannedObjects    int64              `json:"scannedObjects"`
	ScannedBytes      int64              `json:"scannedBytes"`
	ScanComplete      bool               `json:"scanComplete"`
	ScannedBatches    int                `json:"scannedBatches"`
	DispatchedThrough int                `json:"dispatchedThrough"`
	DispatchedObjects int64              `json:"dispatchedObjects"`
	DispatchedBytes   int64              `json:"dispatchedBytes"`
	ChildCount        int                `json:"childCount"`
	FinishedChildren  int                `json:"finishedChildren"`
	FailedChildren    int                `json:"failedChildren"`
	Counters          execution.Counters `json:"counters"`
	Error             string             `json:"error,omitempty"`
	RuntimeError      string             `json:"runtimeError,omitempty"`
	Deleted           bool               `json:"deleted"`
	Purged            bool               `json:"purged"`
	Fingerprint       string             `json:"fingerprint"`
	Legacy            *LegacyProvenance  `json:"legacy,omitempty"`
	Scheduling        *SchedulingState   `json:"scheduling,omitempty"`
	ChildOrder        *ChildOrderState   `json:"childOrder,omitempty"`
}

type Child struct {
	Job      execution.Job     `json:"job"`
	Node     Node              `json:"node"`
	Accepted bool              `json:"accepted"`
	Record   *execution.Record `json:"record,omitempty"`
}

func Ended(status string) bool {
	return status == "STOPPED" || status == "COMPLETED" || status == "FAILED" || status == "PARTIAL_FAILED"
}
