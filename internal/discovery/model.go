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
)

type Status string

const (
	Scanning    Status = "SCANNING"
	Stopped     Status = "STOPPED"
	Recoverable Status = "RECOVERABLE"
	Completed   Status = "COMPLETED"
	Failed      Status = "FAILED"
)

type Spec struct {
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
	Spec           Spec      `json:"spec"`
	Generation     uint64    `json:"generation"`
	Status         Status    `json:"status"`
	AckedThrough   int64     `json:"ackedThrough"`
	LastBatch      int64     `json:"lastBatch"`
	ScanComplete   bool      `json:"scanComplete"`
	TotalObjects   int64     `json:"totalObjects"`
	TotalBytes     int64     `json:"totalBytes"`
	MissingObjects int64     `json:"missingObjects,omitempty"`
	Error          string    `json:"error,omitempty"`
	Warning        string    `json:"warning,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt"`
	// Attempt and SeenObjects fence callbacks and reconcile source deletions.
	// They are durable bookkeeping, not a source cursor or completion checkpoint.
	Attempt     string `json:"attempt,omitempty"`
	SeenObjects int64  `json:"seenObjects"`
	// A completed scan never consults its deduplication index again. Cleanup is
	// resumable and independent of the retained, acknowledged batch history.
	ObjectsReleased bool `json:"objectsReleased,omitempty"`
	Deleted         bool `json:"deleted,omitempty"`
	Purged          bool `json:"purged,omitempty"`
}

type Batch struct {
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
	MaxConcurrentScans int
	MaxBatchObjects    int
	MaxBatchBytes      int64
}
