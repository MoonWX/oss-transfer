// Package discovery publishes durable, bounded batches of source object metadata.
// It is a restartable scan of a live source, not a point-in-time source snapshot.
package discovery

import (
	"context"
	"errors"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

var (
	ErrNotFound      = errors.New("discovery not found")
	ErrInvalid       = errors.New("invalid discovery request")
	ErrConflict      = errors.New("parent already has a different discovery specification")
	ErrGeneration    = errors.New("discovery generation is not current")
	ErrStopped       = errors.New("discovery is stopped")
	ErrRunning       = errors.New("stop discovery before resuming it")
	ErrCapacity      = errors.New("concurrent discovery capacity reached")
	ErrClosed        = errors.New("discovery scanner is closed")
	ErrDeleted       = errors.New("discovery was permanently deleted")
	ErrSourceChanged = errors.New("previously discovered source object changed")
	ErrCorrupt       = errors.New("discovery records are inconsistent")
	ErrBatchReleased = errors.New("discovery batch was acknowledged and released")
	ErrEpoch         = errors.New("discovery epoch is not current")
	ErrSeeding       = errors.New("discovery takeover manifest is incomplete")
)

type Status string

const (
	Scanning    Status = "SCANNING"
	Stopped     Status = "STOPPED"
	Recoverable Status = "RECOVERABLE"
	Completed   Status = "COMPLETED"
	Failed      Status = "FAILED"
	Seeding     Status = "SEEDING"
)

type Spec struct {
	Epoch            uint64             `json:"epoch,omitempty"`
	InventoryID      string             `json:"inventoryId,omitempty"`
	LegacyManifestID string             `json:"legacyManifestId,omitempty"`
	ParentID         int64              `json:"parentId"`
	Generation       uint64             `json:"generation"`
	Source           execution.Endpoint `json:"source"`
	Prefix           string             `json:"prefix,omitempty"`
	SingleKey        string             `json:"singleKey,omitempty"`
	StartKey         string             `json:"startKey,omitempty"` // Exclusive, UTF-8 byte order.
	EndKey           string             `json:"endKey,omitempty"`   // Inclusive, UTF-8 byte order.
	ModifiedFrom     time.Time          `json:"modifiedFrom,omitempty"`
	ModifiedUntil    time.Time          `json:"modifiedUntil,omitempty"`
}

type State struct {
	Checkpoint        *Checkpoint    `json:"checkpoint,omitempty"`
	CleanupPhase      int            `json:"cleanupPhase,omitempty"`
	CleanupAfter      string         `json:"cleanupAfter,omitempty"`
	CleanupDeleted    int64          `json:"cleanupDeleted,omitempty"`
	CleanupError      string         `json:"cleanupError,omitempty"`
	Epoch             uint64         `json:"epoch,omitempty"`
	Takeover          *TakeoverState `json:"takeover,omitempty"`
	Spec              Spec           `json:"spec"`
	Generation        uint64         `json:"generation"`
	Status            Status         `json:"status"`
	AckedThrough      int64          `json:"ackedThrough"`
	ReleasableThrough int64          `json:"releasableThrough,omitempty"`
	ReleaseProtocol   int            `json:"releaseProtocol,omitempty"`
	ReleasedThrough   int64          `json:"releasedThrough,omitempty"`
	LastBatch         int64          `json:"lastBatch"`
	ScanComplete      bool           `json:"scanComplete"`
	TotalObjects      int64          `json:"totalObjects"`
	TotalBytes        int64          `json:"totalBytes"`
	MissingObjects    int64          `json:"missingObjects,omitempty"`
	Error             string         `json:"error,omitempty"`
	Warning           string         `json:"warning,omitempty"`
	UpdatedAt         time.Time      `json:"updatedAt"`
	// Attempt fences callbacks from old runs; the checkpoint's stable Pass
	// identifies the traversal across process restarts and task generations.
	Attempt     string `json:"attempt,omitempty"`
	SeenObjects int64  `json:"seenObjects"`
	// A completed scan never consults its deduplication index again. Cleanup is
	// resumable and independent of releasing acknowledged outgoing batches.
	ObjectsReleased bool `json:"objectsReleased,omitempty"`
	Deleted         bool `json:"deleted,omitempty"`
	Purged          bool `json:"purged,omitempty"`
}

// Checkpoint advances only after its source page and output batches commit.
// SourceDone distinguishes a committed terminal page from an initial cursor.
// LegacyAfter is separate because imported manifests have a reconciliation
// phase after source enumeration. These cursors are private to this scanner;
// takeover replays Master's assigned work instead of inheriting scan-ahead.
type Checkpoint struct {
	Version     int    `json:"version"`
	Pass        string `json:"pass"`
	Cursor      string `json:"cursor,omitempty"`
	SourceDone  bool   `json:"sourceDone,omitempty"`
	LegacyAfter string `json:"legacyAfter,omitempty"`
	LegacyDone  bool   `json:"legacyDone,omitempty"`
}

type Batch struct {
	Epoch      uint64                               `json:"epoch,omitempty"`
	Legacy     map[string]execution.LegacyCandidate `json:"legacy,omitempty"`
	ParentID   int64                                `json:"parentId"`
	Generation uint64                               `json:"generation"`
	Number     int64                                `json:"number"`
	Objects    []transferstore.Object               `json:"objects"`
	Bytes      int64                                `json:"bytes"`
}

type EndpointFactory interface {
	OpenEndpoint(context.Context, execution.Endpoint) (transferstore.Store, error)
}

type Options struct {
	// ParentContext is a durable parent lease's lifetime, never an HTTP request.
	ParentContext      func(int64, uint64) (context.Context, error)
	MaxConcurrentScans int
	MaxBatchObjects    int
	MaxBatchBytes      int64
}
