package legacyimport

import (
	"encoding/json"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type oldBinding struct {
	ExecutionNodeIP string `json:"executionNodeIp"`
	LocalPath       string `json:"localPath"`
}
type oldEndpoint struct {
	Type, S3Alias, Bucket, ObjectKey, Prefix, LocalPath string
	ExecutionNodeIP                                     string `json:"executionNodeIp"`
	PrefixFilterEnabled                                 *bool
	NodeBindings                                        []oldBinding
}
type oldKeyMapping struct{ Mode, SourcePrefix, TargetPrefix, TargetKey string }
type oldRequest struct {
	Direction, SourceBucket, DestBucket, SourceS3Alias, DestS3Alias   string
	SourceEndpoint, DestinationEndpoint                               *oldEndpoint
	ConflictPolicy, DistributionMode, DateAfter, DateBefore           string
	Prefix, ObjectKey, TargetRelativePath, LocalPath, ExecutionNodeIP string
	StartKey, EndKey, Marker                                          string
	Overwrite                                                         *bool
	EligibleWorkerNodes, ParticipatingWorkerNodes                     []string
	NodeBindings                                                      []oldBinding
	KeyMapping                                                        *oldKeyMapping
	ListQPSLimit, HeadQPSLimit, PutQPSLimit, BandwidthBitsPerSecond   *float64
	TaskBandwidthLimitMbps                                            *float64
	TaskBandwidthScopeID                                              string `json:"taskBandwidthScopeId"`
}
type oldTask struct {
	oldRequest
	TaskIndex                                                                                                                                  int64
	ParentTaskID                                                                                                                               *int64 `json:"parentTaskId"`
	ShardID                                                                                                                                    string `json:"shardId"`
	ShardIndex                                                                                                                                 *int
	Status                                                                                                                                     string
	ScanComplete                                                                                                                               *bool
	ListObjectCount, ListObjectSize, PutObjectCount, PutObjectSize, FailedObjectCount, FailedObjectSize, SkippedObjectCount, SkippedObjectSize int64
	CreateTime, StartTime, EndTime                                                                                                             *int64
	InventoryManifestID                                                                                                                        string `json:"inventoryManifestId"`
	ErrorCode, ErrorMessage                                                                                                                    string
}
type oldCreation struct {
	ID                       string
	PublicTaskID             *int64  `json:"publicTaskId"`
	TaskIDs                  []int64 `json:"taskIds"`
	Request                  *oldRequest
	Status, Message, Marker  string
	CreatedAt                int64
	ScanComplete, AutoStart  *bool
	Stopped, Deleted         bool
	Generation               uint64
	NextShardIndex           int
	PendingDispatches        []json.RawMessage
	ParticipatingWorkerNodes []string
}
type oldLocalParent struct {
	ParentTaskID                    int64 `json:"parentTaskId"`
	Request                         *oldRequest
	Status, ErrorCode, ErrorMessage string
	OrchestrationComplete           *bool
	CreateTime, StartTime, EndTime  *int64
}
type oldRawRecord struct {
	Node, Key string
	Value     []byte
}
type oldTaskRef struct {
	Node, Key string
	Value     oldTask
	Deleted   bool
	ParentID  int64
	Number    int
}
type oldGroup struct {
	ID                    int64
	Creation              *oldCreation
	Local                 *oldLocalParent
	Snapshot              *oldTask
	SourceNode, SourceKey string
	Children              []*oldTaskRef
	Deleted, Purged       bool
	Blockers              []string
}
type taskCatalogue struct {
	Groups              map[int64]*oldGroup
	Tasks               map[string]*oldTaskRef
	CreationTasks       map[int64]int64
	Creations           map[string]int64
	Inventories         map[string][]int64
	Fences              map[int64]bool
	AuthoritativeFences map[int64]bool
	ScopedParents       map[string][]int64
	Policies            map[string]oldBandwidth
	HighWater           uint64
}
type oldBandwidth struct {
	ScopeID   string `json:"scopeId"`
	LimitMbps *float64
	Members   []string
	Revision  uint64
}

type TaskImportOptions struct {
	// Required only for a saved date filter without a timezone. Use the original
	// JVM's effective timezone, not the machine on which conversion happens.
	LegacyTimezone string `json:"legacyTimezone,omitempty"`
}
type TaskConversion struct {
	MigrationID       string        `json:"migrationId"`
	Parents           int           `json:"parents"`
	Children          int           `json:"children"`
	HistoryRecords    int           `json:"historyRecords"`
	HighWater         uint64        `json:"highWater"`
	CatalogueComplete bool          `json:"catalogueComplete"`
	ReadyToRun        bool          `json:"readyToRun"`
	Blockers          []TaskBlocker `json:"blockers"`
}
type TaskBlocker struct {
	ParentID int64  `json:"parentId"`
	Code     string `json:"code"`
}

type convertedParent struct {
	Import task.LegacyParentImport
	Group  *oldGroup
}
