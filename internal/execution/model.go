// Package execution runs durable, bounded child jobs on an Agent. Object writes
// and ledger commits are separate transactions: recovery can repeat a write.
package execution

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

var (
	ErrNotFound       = errors.New("job not found")
	ErrGeneration     = errors.New("stale or unregistered generation")
	ErrConflict       = errors.New("child ID already belongs to different work")
	ErrStopped        = errors.New("job or parent is stopped")
	ErrNotRecoverable = errors.New("child task is no longer waiting for recovery")
	ErrDeleted        = errors.New("parent is deleted")
	ErrCapacity       = errors.New("pending job capacity reached")
	ErrAttempt        = errors.New("attempt is no longer current")
	ErrRunning        = errors.New("previous attempt has not exited")
	ErrClosed         = errors.New("execution engine is closed")
)

type Status string

const (
	Waiting       Status = "WAITING"
	Running       Status = "RUNNING"
	Recoverable   Status = "RECOVERABLE"
	Stopped       Status = "STOPPED"
	Completed     Status = "COMPLETED"
	Failed        Status = "FAILED"
	PartialFailed Status = "PARTIAL_FAILED"
	Deleted       Status = "DELETED"
	Retired       Status = "RETIRED"
)

type ItemStatus string

const (
	ItemPending   ItemStatus = "PENDING"
	ItemSucceeded ItemStatus = "SUCCESS"
	ItemSkipped   ItemStatus = "SKIPPED"
	ItemFailed    ItemStatus = "FAILED"
)

// Ref identifies a separately provisioned storage connection; it is not a secret.
type Endpoint struct {
	Kind   string `json:"kind"`
	Ref    string `json:"ref,omitempty"`
	Root   string `json:"root,omitempty"`
	Bucket string `json:"bucket,omitempty"`
}

type Item struct {
	ID             string               `json:"id"`
	Source         transferstore.Object `json:"source"`
	DestinationKey string               `json:"destinationKey"`
	Legacy         *LegacyCandidate     `json:"legacy,omitempty"`
}

// Items are immutable and limited to 128. DiscoveryComplete can be set later by
// Seal; an empty queue alone never completes an unsealed job.
type Job struct {
	RequestLimits  *RequestLimits `json:"requestLimits,omitempty"`
	ChildID        string         `json:"childId"`
	ParentID       int64          `json:"parentId"`
	Number         int            `json:"number"`
	Generation     uint64         `json:"generation"`
	Assignment     uint64         `json:"assignment,omitempty"`
	OwnerEpoch     uint64         `json:"ownerEpoch,omitempty"`
	Source         Endpoint       `json:"source"`
	Destination    Endpoint       `json:"destination"`
	ConflictPolicy string         `json:"conflictPolicy"`
	// SharedLocalSource is only scheduled after Agent-local shared-root proof.
	// Local device/inode identity differs between mounts on different nodes.
	SharedLocalSource      bool   `json:"sharedLocalSource,omitempty"`
	SharedLocalDestination bool   `json:"sharedLocalDestination,omitempty"`
	Items                  []Item `json:"items"`
	DiscoveryComplete      bool   `json:"discoveryComplete"`
}

type ItemResult struct {
	// ResolvedSource is pinned durably before reading an inventory key that was
	// absent during discovery. Retries retain this first observed identity.
	ResolvedSource *transferstore.Object `json:"resolvedSource,omitempty"`
	ID             string                `json:"id"`
	Status         ItemStatus            `json:"status"`
	Target         transferstore.Object  `json:"target"`
	Error          string                `json:"error,omitempty"`
	// A write may have published the target before its durability check failed.
	TargetCommitted bool   `json:"targetCommitted,omitempty"`
	Reconciliation  string `json:"reconciliation,omitempty"`
}

type Counters struct {
	Total          int64 `json:"total"`
	TotalBytes     int64 `json:"totalBytes"`
	Succeeded      int64 `json:"succeeded"`
	SucceededBytes int64 `json:"succeededBytes"`
	Skipped        int64 `json:"skipped"`
	SkippedBytes   int64 `json:"skippedBytes"`
	Failed         int64 `json:"failed"`
	FailedBytes    int64 `json:"failedBytes"`
}

type Record struct {
	ReleasedAssignment *uint64      `json:"releasedAssignment,omitempty"`
	ActiveTime         *ActiveTime  `json:"activeTime,omitempty"`
	InventorySource    bool         `json:"inventorySource,omitempty"`
	Job                Job          `json:"job"`
	Status             Status       `json:"status"`
	Attempt            uint64       `json:"attempt"`
	Results            []ItemResult `json:"results"`
	Counters           Counters     `json:"counters"`
	Error              string       `json:"error,omitempty"`
	UpdatedAt          time.Time    `json:"updatedAt"`
	CreatedAt          time.Time    `json:"createdAt"`
	StartedAt          *time.Time   `json:"startedAt,omitempty"`
	FinishedAt         *time.Time   `json:"finishedAt,omitempty"`
	Fingerprint        string       `json:"fingerprint"`
}

type ParentFence struct {
	StopJobsFenced  bool   `json:"stopJobsFenced,omitempty"`
	CleanupDone     bool   `json:"cleanupDone,omitempty"`
	CleanupDeleted  int64  `json:"cleanupDeleted,omitempty"`
	CleanupAfter    string `json:"cleanupAfter,omitempty"`
	CleanupError    string `json:"cleanupError,omitempty"`
	RetryFailed     bool   `json:"retryFailed,omitempty"`
	InventorySource bool   `json:"inventorySource,omitempty"`
	ParentID        int64  `json:"parentId"`
	Generation      uint64 `json:"generation"`
	Stopped         bool   `json:"stopped"`
	Deleted         bool   `json:"deleted"`
}

// Each Open owns two independent handles, closed after all attempt workers exit.
// Implementations must honor context cancellation during Open and I/O.
type StoreFactory interface {
	Open(context.Context, Job) (source, destination transferstore.Store, err error)
}
type StoreFactoryFunc func(context.Context, Job) (transferstore.Store, transferstore.Store, error)

func (f StoreFactoryFunc) Open(ctx context.Context, j Job) (transferstore.Store, transferstore.Store, error) {
	return f(ctx, j)
}

type ReaderDecorator func(context.Context, Job, Item, io.ReadCloser) (io.ReadCloser, error)

type Options struct {
	JobContext        func(Job) (context.Context, error)
	ParentContext     func(int64, uint64) (context.Context, error)
	MaxConcurrentJobs int
	WorkersPerJob     int
	QueueSize         int
	CommitBatchSize   int
	CommitInterval    time.Duration
	DecorateReader    ReaderDecorator
}

func terminal(s Status) bool {
	return s == Completed || s == Failed || s == PartialFailed || s == Deleted || s == Retired
}
