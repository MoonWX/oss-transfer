package master

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/ratelimit"
	"github.com/MoonWX/oss-transfer/internal/task"
)

type taskBinding struct {
	ExecutionNodeIP string `json:"executionNodeIp"`
	LocalPath       string `json:"localPath"`
}
type taskEndpoint struct {
	Type                string        `json:"type"`
	S3Alias             string        `json:"s3Alias,omitempty"`
	Bucket              string        `json:"bucket,omitempty"`
	Prefix              string        `json:"prefix,omitempty"`
	PrefixFilterEnabled bool          `json:"prefixFilterEnabled,omitempty"`
	ObjectKey           string        `json:"objectKey,omitempty"`
	LocalPath           string        `json:"localPath,omitempty"`
	NodeBindings        []taskBinding `json:"nodeBindings,omitempty"`
}
type taskMapping struct {
	Mode         string `json:"mode"`
	SourcePrefix string `json:"sourcePrefix,omitempty"`
	TargetPrefix string `json:"targetPrefix,omitempty"`
	TargetKey    string `json:"targetKey,omitempty"`
}
type taskCreateRequest struct {
	RequestLimits          *execution.RequestLimits `json:"requestLimits,omitempty"`
	Direction              string                   `json:"direction"`
	SourceS3Alias          string                   `json:"sourceS3Alias"`
	SourceBucket           string                   `json:"sourceBucket"`
	DestS3Alias            string                   `json:"destS3Alias"`
	DestBucket             string                   `json:"destBucket"`
	SourceEndpoint         *taskEndpoint            `json:"sourceEndpoint"`
	DestinationEndpoint    *taskEndpoint            `json:"destinationEndpoint"`
	Prefix                 string                   `json:"prefix"`
	ObjectKey              string                   `json:"objectKey"`
	KeyMapping             *taskMapping             `json:"keyMapping"`
	Overwrite              *bool                    `json:"overwrite"`
	ConflictPolicy         string                   `json:"conflictPolicy"`
	EligibleWorkerNodes    []string                 `json:"eligibleWorkerNodes"`
	WorkerNode             string                   `json:"workerNode"`
	ExecutionNodeIP        string                   `json:"executionNodeIp"`
	DistributionMode       string                   `json:"distributionMode"`
	DateAfter              string                   `json:"dateAfter"`
	DateBefore             string                   `json:"dateBefore"`
	TaskBandwidthLimitMbps *float64                 `json:"taskBandwidthLimitMbps"`
	TargetRelativePath     string                   `json:"targetRelativePath"`
	StartKey               string                   `json:"startKey"`
	EndKey                 string                   `json:"endKey"`
	Marker                 string                   `json:"marker"`
}
type bindingCheck struct {
	taskBinding
	NodeDisplay
	Valid    bool   `json:"valid"`
	PathType string `json:"pathType,omitempty"`
	Code     string `json:"code,omitempty"`
	Message  string `json:"message,omitempty"`
}

func installTaskRoutes(s *Server) error {
	installInventoryRoutes(s)
	if err := s.tasks.repo.EnsureListingIndex(); err != nil {
		return err
	}
	for _, kind := range []string{"add-buckets", "import", "local-transfers"} {
		s.mux.HandleFunc("POST /api/master/"+kind, s.adminOnly(s.createTaskHTTP(kind)))
	}
	s.mux.HandleFunc("POST /api/master/local-transfers/validate", s.adminOnly(s.validateTaskHTTP))
	s.mux.HandleFunc("GET /api/master/status", s.adminOnly(s.taskStatusHTTP))
	s.mux.HandleFunc("GET /api/master/tasks/{id}", s.adminOnly(s.taskDetailHTTP))
	s.mux.HandleFunc("GET /api/master/tasks/{id}/progress", s.adminOnly(s.taskProgressHTTP))
	s.mux.HandleFunc("GET /api/master/local-transfers", s.adminOnly(s.taskStatusHTTP))
	s.mux.HandleFunc("GET /api/master/tasks/{id}/children", s.adminOnly(s.taskChildrenHTTP))
	s.mux.HandleFunc("GET /api/master/tasks/{id}/children/{number}", s.adminOnly(s.taskChildrenHTTP))
	s.mux.HandleFunc("POST /api/master/tasks/{id}/children/{number}/recover", s.adminOnly(s.recoverChildHTTP))
	s.mux.HandleFunc("GET /api/master/deleted-tasks", s.adminOnly(s.deletedTasksHTTP))
	s.mux.HandleFunc("POST /api/master/cleanup-status", s.adminOnly(s.cleanupStatusHTTP))
	for route, action := range map[string]string{"start": "resume", "stop": "stop", "resume": "resume", "retry": "retry", "retry-task": "retry", "clear": "clear", "restore": "restore", "permanent-delete": "permanent-delete", "local-transfers/stop": "stop", "local-transfers/resume": "resume", "local-transfers/retry-failures": "retry"} {
		s.mux.HandleFunc("POST /api/master/"+route, s.adminOnly(s.controlTaskHTTP(action)))
	}
	s.mux.HandleFunc("GET /api/master/tasks/{id}/bandwidth-limit", s.adminOnly(s.taskBandwidthHTTP))
	s.mux.HandleFunc("PUT /api/master/tasks/{id}/bandwidth-limit", s.adminOnly(s.taskBandwidthHTTP))
	return nil
}

func readTaskRows(w http.ResponseWriter, r *http.Request) ([]taskCreateRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	var rawRows []json.RawMessage
	if err := decoder.Decode(&rawRows); err != nil || len(rawRows) == 0 || len(rawRows) > 128 {
		fail(w, 400, "Expected an array containing 1 to 128 transfer requests")
		return nil, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		fail(w, 400, "Expected one JSON array")
		return nil, false
	}
	rows := make([]taskCreateRequest, len(rawRows))
	for i, raw := range rawRows {
		if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			fail(w, 400, "Each transfer request must be a JSON object")
			return nil, false
		}
		localImport := r.URL.Path == "/api/master/local-transfers" || r.URL.Path == "/api/master/local-transfers/validate"
		if r.URL.Path == "/api/master/import" || localImport {
			// Legacy exports include counters and execution IDs. They are not
			// evidence that an object exists at this task's current destination.
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				fail(w, 400, "Invalid imported task")
				return nil, false
			}
			_, exportedLocal := fields["taskIndex"]
			if localImport && exportedLocal {
				if err := normalizeLocalTaskExport(fields); err != nil {
					fail(w, 400, fmt.Sprintf(localize(w, "Invalid imported task %d: %s"), i+1, localize(w, err.Error())))
					return nil, false
				}
			}
			for _, name := range strings.Fields("taskIndex status listObjectCount listObjectSize putObjectCount putObjectSize failedObjectCount failedObjectSize skippedObjectCount skippedObjectSize createTime startTime endTime scanComplete currentSpeedBytesPerSecond averageSpeedBytesPerSecond averageSpeedIncludesInFlight parentTaskId shardId shardIndex attempt autoStart storageId participatingWorkerNodes taskBandwidthScopeId errorCode errorMessage duration activeDurationMillis activeStartedAt durationEstimated durationIncomplete childTaskId subtaskNumber ossTaskGroup creationStatus childCount partial generation observedAt executionNodes executionNodeName executionNodeAddress executionNodeLabel workerNodeLabel executionNodeIdentityOnly") {
				delete(fields, name)
			}
			for _, name := range []string{"inventoryManifestId", "localFileEntries", "localPath", "nodeBindings", "listQpsLimit", "headQpsLimit", "putQpsLimit", "bandwidthBitsPerSecond"} {
				if value, ok := fields[name]; ok {
					if string(value) != "null" && string(value) != "0" && string(value) != "\"\"" && string(value) != "[]" {
						fail(w, 400, fmt.Sprintf(localize(w, "Import requires an explicit replacement for %s"), name))
						return nil, false
					}
					delete(fields, name)
				}
			}
			raw, _ = json.Marshal(fields)
		}
		rowDecoder := json.NewDecoder(bytes.NewReader(raw))
		rowDecoder.DisallowUnknownFields()
		if err := rowDecoder.Decode(&rows[i]); err != nil {
			fail(w, 400, fmt.Sprintf(localize(w, "Invalid task %d: %s"), i+1, err.Error()))
			return nil, false
		}
	}
	return rows, true
}

// A task exported by the list page includes both display aliases and the
// original endpoints. Accept those redundant fields only when they agree;
// changing a destination in one place must not silently select the other.
func normalizeLocalTaskExport(fields map[string]json.RawMessage) error {
	var row taskCreateRequest
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &row); err != nil {
		return err
	}
	if row.SourceEndpoint == nil || row.DestinationEndpoint == nil {
		return errors.New("sourceEndpoint and destinationEndpoint are required")
	}
	local := row.DestinationEndpoint
	if row.Direction == "LOCAL_TO_OSS" {
		local = row.SourceEndpoint
	} else if row.Direction != "OSS_TO_LOCAL" {
		return errors.New("local-transfers requires a local endpoint")
	}
	for _, value := range []struct{ name, top, nested string }{
		{"sourceS3Alias", row.SourceS3Alias, row.SourceEndpoint.S3Alias},
		{"sourceBucket", row.SourceBucket, row.SourceEndpoint.Bucket},
		{"destS3Alias", row.DestS3Alias, row.DestinationEndpoint.S3Alias},
		{"destBucket", row.DestBucket, row.DestinationEndpoint.Bucket},
	} {
		if value.top != "" && value.top != value.nested {
			return fmt.Errorf("conflicting %s fields", value.name)
		}
		delete(fields, value.name)
	}
	if value, ok := fields["nodeBindings"]; ok {
		var bindings []taskBinding
		if err := json.Unmarshal(value, &bindings); err != nil {
			return err
		}
		if len(bindings) != 0 {
			if len(bindings) != len(local.NodeBindings) {
				return errors.New("conflicting local node bindings")
			}
			for i := range bindings {
				if bindings[i] != local.NodeBindings[i] {
					return errors.New("conflicting local node bindings")
				}
			}
		}
		delete(fields, "nodeBindings")
	}
	expected := taskMapping{Mode: "PRESERVE"}
	if row.Direction == "LOCAL_TO_OSS" {
		expected.TargetPrefix = row.DestinationEndpoint.Prefix
		expected.TargetKey = row.DestinationEndpoint.ObjectKey
		if expected.TargetPrefix != "" {
			expected.Mode = "ADD_PREFIX"
		}
		if expected.TargetKey != "" {
			expected.Mode = "EXACT_KEY"
		}
		// A resolved single-file upload also exposes its basename as objectKey.
		// The actual source selector remains the full local path on each node.
		if row.ObjectKey != "" {
			if len(local.NodeBindings) == 0 {
				return errors.New("single-file export requires local node bindings")
			}
			for _, binding := range local.NodeBindings {
				_, name := splitAgentFile(binding.LocalPath)
				if name != row.ObjectKey {
					return errors.New("objectKey conflicts with local source path")
				}
			}
			delete(fields, "objectKey")
		}
	} else if row.TargetRelativePath != "" {
		expected.Mode, expected.TargetKey = "EXACT_KEY", row.TargetRelativePath
	}
	if row.KeyMapping != nil && *row.KeyMapping != expected {
		return errors.New("keyMapping conflicts with local transfer endpoints")
	}
	delete(fields, "keyMapping")
	return nil
}

func taskDate(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return &parsed, nil
		}
	}
	return nil, errors.New("invalid modification time")
}
func taskBandwidthValue(value *float64) (float64, error) {
	if value == nil {
		return 0, nil
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0.1 || *value > 1000000 {
		return 0, errors.New("bandwidth must be 0.1 to 1,000,000 Mbps, or null")
	}
	return *value, nil
}
func mergeTaskString(top, nested, field string) (string, error) {
	if top != "" && nested != "" && top != nested {
		return "", fmt.Errorf("conflicting %s fields", field)
	}
	if top != "" {
		return top, nil
	}
	return nested, nil
}
func ossEndpoint(alias, bucket string) (execution.Endpoint, error) {
	if alias == "" || bucket == "" || strings.ContainsAny(alias+bucket, "\x00\r\n") || strings.Contains(bucket, "/") {
		return execution.Endpoint{}, errors.New("storage connection and bucket are required")
	}
	return execution.Endpoint{Kind: "S3", Ref: alias, Bucket: bucket}, nil
}

// normalizeTaskRequest has no side effects. Object keys and prefixes are never
// trimmed, cleaned, path.Join'ed or otherwise rewritten by this adapter.
func normalizeTaskRequest(row taskCreateRequest, kind string) (task.Spec, []taskBinding, []string, error) {
	spec := task.Spec{Direction: row.Direction, ConflictPolicy: "SKIP", DistributionMode: strings.ToUpper(row.DistributionMode), Prefix: row.Prefix, ObjectKey: row.ObjectKey, StartKey: row.StartKey, EndKey: row.EndKey, Marker: row.Marker, MappingMode: "PRESERVE"}
	if spec.Direction == "" {
		spec.Direction = "OSS_TO_OSS"
	}
	if spec.DistributionMode == "" {
		spec.DistributionMode = "AFFINITY"
	}
	if spec.DistributionMode != "AFFINITY" && spec.DistributionMode != "BALANCED" {
		return spec, nil, nil, errors.New("invalid distribution mode")
	}
	if row.Overwrite != nil && *row.Overwrite {
		spec.ConflictPolicy = "OVERWRITE"
	}
	if row.ConflictPolicy != "" {
		if row.ConflictPolicy != "SKIP" && row.ConflictPolicy != "OVERWRITE" {
			return spec, nil, nil, errors.New("invalid conflict policy")
		}
		if row.Overwrite != nil && (*row.Overwrite != (row.ConflictPolicy == "OVERWRITE")) {
			return spec, nil, nil, errors.New("overwrite and conflictPolicy disagree")
		}
		spec.ConflictPolicy = row.ConflictPolicy
	}
	if err := execution.ValidateRequestLimits(row.RequestLimits); err != nil {
		return spec, nil, nil, err
	}
	if row.RequestLimits != nil {
		copy := *row.RequestLimits
		if copy.Scope == "" {
			copy.Scope = "CLUSTER"
		}
		spec.RequestLimits = &copy
	}
	var err error
	spec.BandwidthMbps, err = taskBandwidthValue(row.TaskBandwidthLimitMbps)
	if err != nil {
		return spec, nil, nil, err
	}
	spec.ModifiedFrom, err = taskDate(row.DateAfter)
	if err != nil {
		return spec, nil, nil, err
	}
	spec.ModifiedUntil, err = taskDate(row.DateBefore)
	if err != nil {
		return spec, nil, nil, err
	}
	if spec.ModifiedFrom != nil && spec.ModifiedUntil != nil && !spec.ModifiedFrom.Before(*spec.ModifiedUntil) {
		return spec, nil, nil, errors.New("dateAfter must precede dateBefore")
	}
	nodes := append([]string(nil), row.EligibleWorkerNodes...)
	worker, err := mergeTaskString(row.WorkerNode, row.ExecutionNodeIP, "worker node")
	if err != nil {
		return spec, nil, nil, err
	}
	if worker != "" {
		if len(nodes) > 0 && (len(nodes) != 1 || nodes[0] != worker) {
			return spec, nil, nil, errors.New("workerNode conflicts with eligibleWorkerNodes")
		}
		nodes = []string{worker}
	}
	if spec.Direction == "OSS_TO_OSS" {
		if kind == "local-transfers" {
			return spec, nil, nil, errors.New("local-transfers requires a local endpoint")
		}
		srcAlias, srcBucket, dstAlias, dstBucket := row.SourceS3Alias, row.SourceBucket, row.DestS3Alias, row.DestBucket
		if ep := row.SourceEndpoint; ep != nil {
			if ep.Type != "" && ep.Type != "OSS" {
				return spec, nil, nil, errors.New("source endpoint must be OSS")
			}
			for _, part := range []struct {
				to          *string
				value, name string
			}{{&srcAlias, ep.S3Alias, "source connection"}, {&srcBucket, ep.Bucket, "source bucket"}, {&spec.ObjectKey, ep.ObjectKey, "object key"}} {
				*part.to, err = mergeTaskString(*part.to, part.value, part.name)
				if err != nil {
					return spec, nil, nil, err
				}
			}
			if kind == "import" && row.Prefix == "" && ep.Prefix != "" && !ep.PrefixFilterEnabled {
				return spec, nil, nil, errors.New("imported sourceEndpoint.prefix requires prefixFilterEnabled:true")
			}
			spec.Prefix, err = mergeTaskString(spec.Prefix, ep.Prefix, "source prefix")
			if err != nil {
				return spec, nil, nil, err
			}
		}
		if ep := row.DestinationEndpoint; ep != nil {
			if ep.Type != "" && ep.Type != "OSS" {
				return spec, nil, nil, errors.New("destination endpoint must be OSS")
			}
			if ep.Prefix != "" || ep.ObjectKey != "" {
				return spec, nil, nil, errors.New("OSS destination naming must use keyMapping")
			}
			dstAlias, err = mergeTaskString(dstAlias, ep.S3Alias, "destination connection")
			if err != nil {
				return spec, nil, nil, err
			}
			dstBucket, err = mergeTaskString(dstBucket, ep.Bucket, "destination bucket")
			if err != nil {
				return spec, nil, nil, err
			}
		}
		spec.Source, err = ossEndpoint(srcAlias, srcBucket)
		if err != nil {
			return spec, nil, nil, err
		}
		spec.Destination, err = ossEndpoint(dstAlias, dstBucket)
		if err != nil {
			return spec, nil, nil, err
		}
		if row.TargetRelativePath != "" {
			return spec, nil, nil, errors.New("targetRelativePath only applies to a local destination")
		}
		if err := applyTaskMapping(&spec, row.KeyMapping); err != nil {
			return spec, nil, nil, err
		}
	} else {
		if kind != "local-transfers" {
			return spec, nil, nil, errors.New("local transfers require the local-transfers endpoint")
		}
		if row.SourceEndpoint == nil || row.DestinationEndpoint == nil {
			return spec, nil, nil, errors.New("sourceEndpoint and destinationEndpoint are required")
		}
		if row.KeyMapping != nil {
			return spec, nil, nil, errors.New("local transfers use destination prefix/objectKey or targetRelativePath")
		}
		var local, oss *taskEndpoint
		switch spec.Direction {
		case "LOCAL_TO_OSS":
			local, oss = row.SourceEndpoint, row.DestinationEndpoint
		case "OSS_TO_LOCAL":
			local, oss = row.DestinationEndpoint, row.SourceEndpoint
		default:
			return spec, nil, nil, errors.New("invalid direction")
		}
		if local.Type != "LOCAL" || oss.Type != "OSS" {
			return spec, nil, nil, errors.New("endpoint types do not match direction")
		}
		endpoint, err := ossEndpoint(oss.S3Alias, oss.Bucket)
		if err != nil {
			return spec, nil, nil, err
		}
		bindings := append([]taskBinding(nil), local.NodeBindings...)
		if len(bindings) == 0 && local.LocalPath != "" && worker != "" {
			bindings = []taskBinding{{ExecutionNodeIP: worker, LocalPath: local.LocalPath}}
		}
		if len(bindings) == 0 {
			return spec, nil, nil, errors.New("local node bindings are required")
		}
		if local.Prefix != "" || local.ObjectKey != "" || local.S3Alias != "" || local.Bucket != "" || oss.LocalPath != "" || len(oss.NodeBindings) != 0 {
			return spec, nil, nil, errors.New("endpoint fields do not match their storage type")
		}
		if len(nodes) > 0 {
			selected := make(map[string]bool, len(bindings))
			for _, binding := range bindings {
				selected[binding.ExecutionNodeIP] = true
			}
			if len(nodes) != len(bindings) {
				return spec, nil, nil, errors.New("execution node selection conflicts with local bindings")
			}
			for _, node := range nodes {
				if !selected[node] {
					return spec, nil, nil, errors.New("execution node selection conflicts with local bindings")
				}
				delete(selected, node)
			}
		}
		if row.SourceS3Alias != "" || row.SourceBucket != "" || row.DestS3Alias != "" || row.DestBucket != "" {
			return spec, nil, nil, errors.New("local transfers select storage through their endpoints")
		}
		if row.StartKey != "" || row.EndKey != "" || row.Marker != "" {
			return spec, nil, nil, errors.New("local transfers do not accept imported key ranges or markers")
		}
		if spec.Direction == "LOCAL_TO_OSS" {
			spec.Source = execution.Endpoint{Kind: "LOCAL"}
			spec.Destination = endpoint
			spec.TargetPrefix = oss.Prefix
			spec.TargetKey = oss.ObjectKey
			if spec.TargetPrefix != "" {
				spec.MappingMode = "ADD_PREFIX"
			}
			if spec.TargetKey != "" {
				spec.MappingMode = "EXACT_KEY"
			}
			if row.Prefix != "" || row.ObjectKey != "" || row.TargetRelativePath != "" {
				return spec, nil, nil, errors.New("upload source is selected by its local path")
			}
			if len(bindings) > 1 {
				spec.DistributionMode = "BALANCED"
			} else {
				spec.DistributionMode = "AFFINITY"
			}
		} else {
			spec.Source = endpoint
			spec.Destination = execution.Endpoint{Kind: "LOCAL"}
			spec.Prefix, err = mergeTaskString(spec.Prefix, oss.Prefix, "source prefix")
			if err != nil {
				return spec, nil, nil, err
			}
			spec.ObjectKey, err = mergeTaskString(spec.ObjectKey, oss.ObjectKey, "source object")
			if err != nil {
				return spec, nil, nil, err
			}
			spec.TargetKey = row.TargetRelativePath
			if spec.TargetKey != "" {
				if spec.ObjectKey == "" {
					return spec, nil, nil, errors.New("targetRelativePath requires a single source object")
				}
				if !safeRelative(spec.TargetKey) {
					return spec, nil, nil, errors.New("targetRelativePath must stay inside the local root")
				}
				spec.MappingMode = "EXACT_KEY"
			}
		}
		if spec.Prefix != "" && spec.ObjectKey != "" {
			return spec, nil, nil, errors.New("prefix and objectKey are mutually exclusive")
		}
		if spec.TargetPrefix != "" && spec.TargetKey != "" {
			return spec, nil, nil, errors.New("target prefix and key are mutually exclusive")
		}
		return spec, bindings, nil, nil
	}
	if spec.Prefix != "" && spec.ObjectKey != "" {
		return spec, nil, nil, errors.New("prefix and objectKey are mutually exclusive")
	}
	if spec.Source == spec.Destination {
		return spec, nil, nil, errors.New("source and destination buckets must differ")
	}
	if spec.ObjectKey != "" && (spec.StartKey != "" || spec.EndKey != "") {
		return spec, nil, nil, errors.New("a single source object cannot have a key range")
	}
	if spec.EndKey != "" && spec.StartKey >= spec.EndKey {
		return spec, nil, nil, errors.New("invalid source key range")
	}
	return spec, nil, nodes, nil
}

func applyTaskMapping(spec *task.Spec, mapping *taskMapping) error {
	if mapping == nil {
		return nil
	}
	spec.MappingMode = mapping.Mode
	switch mapping.Mode {
	case "PRESERVE":
		if mapping.SourcePrefix != "" || mapping.TargetPrefix != "" || mapping.TargetKey != "" {
			return errors.New("PRESERVE does not accept mapping fields")
		}
	case "ADD_PREFIX":
		if mapping.TargetPrefix == "" || mapping.SourcePrefix != "" || mapping.TargetKey != "" {
			return errors.New("ADD_PREFIX requires only targetPrefix")
		}
		spec.TargetPrefix = mapping.TargetPrefix
	case "REPLACE_PREFIX":
		if spec.Prefix == "" || mapping.SourcePrefix != spec.Prefix || mapping.TargetKey != "" {
			return errors.New("REPLACE_PREFIX sourcePrefix must equal the source filter prefix")
		}
		spec.ReplaceSourcePrefix = mapping.SourcePrefix
		spec.TargetPrefix = mapping.TargetPrefix
	case "EXACT_KEY":
		if spec.ObjectKey == "" || mapping.TargetKey == "" || mapping.SourcePrefix != "" || mapping.TargetPrefix != "" {
			return errors.New("EXACT_KEY requires one source object and targetKey")
		}
		spec.TargetKey = mapping.TargetKey
	default:
		return errors.New("unsupported key mapping mode")
	}
	return nil
}
func safeRelative(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\\x00") || len(value) > 1 && value[1] == ':' {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == "." || part == "" {
			return false
		}
	}
	return true
}
func absoluteAgentPath(value string) bool {
	return strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\\\") || (len(value) >= 3 && value[1] == ':' && (value[2] == '/' || value[2] == '\\'))
}
func splitAgentFile(value string) (string, string) {
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		return path.Dir(value), path.Base(value)
	}
	// path.Dir is POSIX-only: it strips the second leading UNC slash and
	// changes a drive-root parent from C:/ to drive-relative C:.
	normalized := strings.ReplaceAll(value, "\\", "/")
	separator := strings.LastIndexByte(normalized, '/')
	if separator < 0 {
		return ".", normalized
	}
	dir, base := normalized[:separator], normalized[separator+1:]
	if len(dir) == 2 && dir[1] == ':' {
		dir += "/"
	}
	if strings.Contains(value, "\\") {
		dir = strings.ReplaceAll(dir, "/", "\\")
	}
	return dir, base
}
func joinAgentPath(root, key string) string {
	sep := "/"
	if strings.Contains(root, "\\") && !strings.HasPrefix(root, "/") {
		sep = "\\"
	}
	return strings.TrimRight(root, "/\\") + sep + strings.ReplaceAll(key, "/", sep)
}

func (s *Server) resolveTaskSpec(ctx context.Context, row taskCreateRequest, kind string, sets ...nodeDisplaySet) (task.Spec, []bindingCheck, error) {
	spec, bindings, requested, err := normalizeTaskRequest(row, kind)
	if err != nil {
		return spec, nil, err
	}
	members, err := s.onlineAgents()
	if err != nil {
		return spec, nil, err
	}
	var displays nodeDisplaySet
	if len(sets) > 0 {
		displays = sets[0]
	} else {
		displays = s.nodeDisplaySnapshot()
	}
	byAddress := map[string]cluster.Member{}
	for _, member := range members {
		byAddress[member.Address] = member
		byAddress[member.ID] = member
	}
	seen := map[string]bool{}
	if len(bindings) == 0 {
		if len(requested) == 0 {
			for _, member := range members {
				requested = append(requested, member.Address)
			}
		}
		if len(requested) == 0 {
			return spec, nil, errors.New("no online execution Agent")
		}
		for _, address := range requested {
			member, ok := byAddress[address]
			if !ok {
				return spec, nil, fmt.Errorf(localizeContext(ctx, "execution Agent %q is unavailable"), displays.lookup(address, address).DisplayLabel)
			}
			if seen[member.ID] {
				return spec, nil, errors.New("duplicate execution Agent")
			}
			seen[member.ID] = true
			spec.Nodes = append(spec.Nodes, task.Node{ID: member.ID, EnrollmentID: member.EnrollmentID, Address: member.Address})
		}
		return spec, nil, nil
	}
	if len(bindings) > 1024 {
		return spec, nil, errors.New("too many local node bindings")
	}
	checks := make([]bindingCheck, 0, len(bindings))
	var firstErr error
	for _, binding := range bindings {
		check := bindingCheck{taskBinding: binding, NodeDisplay: displays.lookup(binding.ExecutionNodeIP, binding.ExecutionNodeIP)}
		member, ok := byAddress[binding.ExecutionNodeIP]
		if !ok {
			check.Message = localizeContext(ctx, "执行节点未连接")
		} else if seen[member.ID] {
			check.Message = localizeContext(ctx, "执行节点重复")
		} else if !absoluteAgentPath(binding.LocalPath) || strings.ContainsRune(binding.LocalPath, 0) {
			check.Message = localizeContext(ctx, "本地路径必须为绝对路径")
		} else {
			seen[member.ID] = true
			var probe struct {
				PathType string `json:"pathType"`
				Exists   bool   `json:"exists"`
			}
			probeErr := s.agentCall(ctx, member.Session(), "local_path", false, agentservice.PathRequest{Path: binding.LocalPath}, &probe)
			check.PathType = probe.PathType
			if probeErr != nil {
				check.Message = probeErr.Error()
			} else if spec.Direction == "LOCAL_TO_OSS" && probe.PathType == "MISSING" {
				check.Message = localizeContext(ctx, "源路径不存在")
			} else if spec.Direction == "LOCAL_TO_OSS" && probe.PathType == "FILE" && len(bindings) > 1 {
				check.Code = "SINGLE_FILE_REQUIRES_ONE_NODE"
				check.Message = localizeContext(ctx, "单文件上传只能选择一个节点")
			} else if spec.Direction == "LOCAL_TO_OSS" && probe.PathType == "DIRECTORY" && spec.TargetKey != "" {
				check.Message = localizeContext(ctx, "目录上传不能指定单个目标 Key")
			} else if spec.Direction == "OSS_TO_LOCAL" && probe.PathType == "FILE" {
				check.Message = localizeContext(ctx, "下载目标必须是目录")
			} else if probe.PathType != "FILE" && probe.PathType != "DIRECTORY" && probe.PathType != "MISSING" {
				check.Message = localizeContext(ctx, "节点未返回有效路径类型")
			} else {
				check.Valid = true
				root := binding.LocalPath
				if spec.Direction == "LOCAL_TO_OSS" && probe.PathType == "FILE" {
					root, spec.ObjectKey = splitAgentFile(binding.LocalPath)
				}
				if probe.PathType == "MISSING" {
					check.Message = localizeContext(ctx, "目标目录将在任务开始时创建")
				}
				spec.Nodes = append(spec.Nodes, task.Node{ID: member.ID, EnrollmentID: member.EnrollmentID, Address: member.Address, LocalRoot: root})
			}
		}
		if !check.Valid && firstErr == nil {
			firstErr = fmt.Errorf("%s: %s", check.DisplayLabel, check.Message)
		}
		checks = append(checks, check)
	}
	if firstErr == nil {
		firstErr = s.validateSharedBindings(ctx, &spec, checks)
	} else {
		invalidateUnverifiedSharedBindings(ctx, checks)
	}
	if len(spec.Nodes) > 0 {
		if spec.Direction == "LOCAL_TO_OSS" {
			spec.Source.Root = spec.Nodes[0].LocalRoot
		} else {
			spec.Destination.Root = spec.Nodes[0].LocalRoot
		}
	}
	return spec, checks, firstErr
}

func (s *Server) createTaskHTTP(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rows, ok := readTaskRows(w, r)
		if !ok {
			return
		}
		requestKey := r.Header.Get("Idempotency-Key")
		if len(requestKey) > 220 {
			fail(w, 400, "Idempotency-Key is too long")
			return
		}
		if requestKey == "" {
			var err error
			requestKey, err = cluster.Token()
			if err != nil {
				fail(w, 500, "Cannot allocate request identity")
				return
			}
		}
		ids := []int64{}
		problems := []string{}
		acceptedBindings := []map[string]any{}
		displays := s.nodeDisplaySnapshot()
		for i, row := range rows {
			spec, checks, err := s.resolveTaskSpec(r.Context(), row, kind, displays)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%d: %s", i+1, localize(w, err.Error())))
				continue
			}
			created, err := s.createTransfer(r.Context(), spec, fmt.Sprintf("%s:%d", requestKey, i))
			if created.ID > 0 && !errors.Is(err, task.ErrConflict) {
				ids = append(ids, created.ID)
				acceptedBindings = append(acceptedBindings, map[string]any{"parentTaskId": created.ID, "distributionMode": strings.ToLower(spec.DistributionMode), "nodeBindings": checks})
			}
			if err != nil {
				problems = append(problems, fmt.Sprintf("%d: %s", i+1, localize(w, err.Error())))
			}
		}
		result := map[string]any{"success": true, "partial": len(problems) > 0, "taskIds": ids, "rejectedCount": len(rows) - len(ids), "validationErrors": problems}
		if kind == "local-transfers" {
			result["acceptedCount"] = len(ids)
			result["parentTaskIds"] = ids
			result["acceptedNodeBindings"] = acceptedBindings
			writeJSON(w, 200, map[string]any{"success": true, "data": result})
		} else {
			count := "addedCount"
			if kind == "import" {
				count = "importedCount"
			}
			result[count] = len(ids)
			writeJSON(w, 200, result)
		}
	}
}
func (s *Server) validateTaskHTTP(w http.ResponseWriter, r *http.Request) {
	rows, ok := readTaskRows(w, r)
	if !ok {
		return
	}
	checks := []bindingCheck{}
	displays := s.nodeDisplaySnapshot()
	valid := true
	problems := []string{}
	for _, row := range rows {
		_, items, err := s.resolveTaskSpec(r.Context(), row, "local-transfers", displays)
		checks = append(checks, items...)
		if err != nil {
			valid = false
			problems = append(problems, localize(w, err.Error()))
		}
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"valid": valid, "nodeBindings": checks, "validationErrors": problems}})
}

func taskDTO(p task.Parent) map[string]any {
	spec := p.Spec
	nodes := []string{}
	bindings := []taskBinding{}
	for _, node := range spec.Nodes {
		nodes = append(nodes, node.Address)
		if node.LocalRoot != "" {
			local := node.LocalRoot
			if spec.Direction == "LOCAL_TO_OSS" && spec.ObjectKey != "" {
				local = joinAgentPath(local, spec.ObjectKey)
			}
			bindings = append(bindings, taskBinding{node.Address, local})
		}
	}
	endpoint := func(ep execution.Endpoint, source bool) taskEndpoint {
		if ep.Kind == "LOCAL" {
			local := ep.Root
			if source && spec.ObjectKey != "" {
				local = joinAgentPath(local, spec.ObjectKey)
			}
			return taskEndpoint{Type: "LOCAL", LocalPath: local, NodeBindings: bindings}
		}
		out := taskEndpoint{Type: "OSS", S3Alias: ep.Ref, Bucket: ep.Bucket}
		if source {
			out.Prefix = spec.Prefix
			out.PrefixFilterEnabled = spec.Prefix != ""
			out.ObjectKey = spec.ObjectKey
		} else if spec.Direction == "LOCAL_TO_OSS" {
			out.Prefix = spec.TargetPrefix
			out.ObjectKey = spec.TargetKey
		}
		return out
	}
	row := map[string]any{"taskIndex": p.ID, "parentTaskId": p.ID, "generation": p.Generation, "direction": spec.Direction, "status": p.Status, "autoStart": true, "sourceS3Alias": spec.Source.Ref, "sourceBucket": spec.Source.Bucket, "destS3Alias": spec.Destination.Ref, "destBucket": spec.Destination.Bucket, "sourceEndpoint": endpoint(spec.Source, true), "destinationEndpoint": endpoint(spec.Destination, false), "eligibleWorkerNodes": nodes, "participatingWorkerNodes": nodes, "nodeBindings": bindings, "conflictPolicy": spec.ConflictPolicy, "overwrite": spec.ConflictPolicy == "OVERWRITE", "distributionMode": strings.ToLower(spec.DistributionMode), "prefix": spec.Prefix, "objectKey": spec.ObjectKey, "startKey": spec.StartKey, "endKey": spec.EndKey, "scanComplete": p.ScanComplete, "listObjectCount": max(p.ScannedObjects, p.Counters.Total), "listObjectSize": max(p.ScannedBytes, p.Counters.TotalBytes), "putObjectCount": p.Counters.Succeeded, "putObjectSize": p.Counters.SucceededBytes, "skippedObjectCount": p.Counters.Skipped, "skippedObjectSize": p.Counters.SkippedBytes, "failedObjectCount": p.Counters.Failed, "failedObjectSize": p.Counters.FailedBytes, "createTime": p.CreatedAt.UnixMilli(), "errorMessage": p.Error, "childCount": p.ChildCount, "partial": false}
	if spec.InventoryID != "" {
		row["inventoryManifestId"] = spec.InventoryID
	}
	if p.Cleanup != nil {
		row["cleanup"] = cleanupDTO(p.Cleanup)
	}
	if spec.RequestLimits != nil {
		row["requestLimits"] = spec.RequestLimits
	}
	row["listObjectCount"] = max(p.ScannedObjects, p.DispatchedObjects, p.Counters.Total)
	row["listObjectSize"] = max(p.ScannedBytes, p.DispatchedBytes, p.Counters.TotalBytes)
	if p.RuntimeError != "" {
		row["errorMessage"] = strings.TrimSpace(p.Error + "\n" + p.RuntimeError)
	}
	if spec.Direction == "OSS_TO_LOCAL" {
		row["targetRelativePath"] = spec.TargetKey
	}
	if spec.MappingMode != "" {
		row["keyMapping"] = taskMapping{spec.MappingMode, spec.ReplaceSourcePrefix, spec.TargetPrefix, spec.TargetKey}
	}
	if spec.ModifiedFrom != nil {
		row["dateAfter"] = spec.ModifiedFrom.Format(time.RFC3339)
	}
	if spec.ModifiedUntil != nil {
		row["dateBefore"] = spec.ModifiedUntil.Format(time.RFC3339)
	}
	if spec.BandwidthMbps > 0 {
		row["taskBandwidthLimitMbps"] = spec.BandwidthMbps
	} else {
		row["taskBandwidthLimitMbps"] = nil
	}
	executionTimeDTO(row, p.ExecutionTime(), p.StartedAt, p.FinishedAt, p.Counters.SucceededBytes, time.Now())
	if p.ResumeTiming != nil || (p.Status == "RESUMING" && p.ActiveTime == nil && p.FinishedAt == nil) {
		delete(row, "duration")
		delete(row, "averageSpeedBytesPerSecond")
		row["durationIncomplete"] = true
	}
	if spec.Direction == "OSS_TO_OSS" {
		row["ossTaskGroup"] = true
		if p.ScanComplete {
			row["creationStatus"] = "COMPLETED"
		} else {
			row["creationStatus"] = p.Status
		}
	}
	if task.Historical(p) {
		row["legacyHistorical"] = true
		if p.CreatedAt.IsZero() {
			row["createTime"] = nil
		}
		if p.FinishedAt == nil {
			delete(row, "duration")
			delete(row, "averageSpeedBytesPerSecond")
		}
		row["contentVerified"] = false
		row["legacyOriginalStatus"] = p.Legacy.OriginalStatus
		row["migrationRecoveryRequired"] = p.Legacy.NeedsReconciliation
		row["autoStart"] = false
	}
	return row
}
func childTaskDTO(p task.Parent, c task.Child) map[string]any {
	// A child's endpoints belong to its assigned node, whose local root can
	// differ from the node that scanned the shared source.
	p.Spec.Source, p.Spec.Destination = c.Job.Source, c.Job.Destination
	p.Spec.Nodes = []task.Node{c.Node}
	row := taskDTO(p)
	row["taskIndex"] = p.ID
	row["childTaskId"] = c.Job.ChildID
	row["shardId"] = c.Job.ChildID
	row["shardIndex"] = c.Job.Number - 1
	row["subtaskNumber"] = c.Job.Number
	row["executionNodeIp"] = c.Node.Address
	row["workerNode"] = c.Node.Address
	row["ossTaskGroup"] = false
	delete(row, "creationStatus")
	row["scanComplete"] = c.Job.DiscoveryComplete
	row["status"] = "PENDING"
	row["listObjectCount"] = len(c.Job.Items)
	var size int64
	for _, item := range c.Job.Items {
		size += item.Source.Size
	}
	row["listObjectSize"] = size
	for _, name := range []string{"putObjectCount", "putObjectSize", "skippedObjectCount", "skippedObjectSize", "failedObjectCount", "failedObjectSize"} {
		row[name] = int64(0)
	}
	delete(row, "startTime")
	delete(row, "endTime")
	delete(row, "duration")
	delete(row, "activeDurationMillis")
	delete(row, "activeStartedAt")
	delete(row, "durationEstimated")
	delete(row, "durationIncomplete")
	delete(row, "averageSpeedBytesPerSecond")
	row["errorMessage"] = ""
	if c.Node.LocalRoot != "" {
		row["localPath"] = c.Node.LocalRoot
		if p.Spec.Direction == "LOCAL_TO_OSS" && p.Spec.ObjectKey != "" {
			row["localPath"] = joinAgentPath(c.Node.LocalRoot, p.Spec.ObjectKey)
		}
	}
	if len(c.Job.Items) > 0 {
		row["startKey"] = c.Job.Items[0].Source.Key
		row["endKey"] = c.Job.Items[len(c.Job.Items)-1].Source.Key
	}
	if c.Record != nil {
		rec := c.Record
		row["status"] = string(rec.Status)
		if rec.Status == execution.Waiting {
			row["status"] = "PENDING"
		}
		row["listObjectSize"] = max(size, rec.Counters.TotalBytes)
		row["putObjectCount"] = rec.Counters.Succeeded
		row["putObjectSize"] = rec.Counters.SucceededBytes
		row["skippedObjectCount"] = rec.Counters.Skipped
		row["skippedObjectSize"] = rec.Counters.SkippedBytes
		row["failedObjectCount"] = rec.Counters.Failed
		row["failedObjectSize"] = rec.Counters.FailedBytes
		row["errorMessage"] = rec.Error
		row["attempt"] = rec.Attempt
		row["observedAt"] = rec.UpdatedAt.UnixMilli()
		if !rec.CreatedAt.IsZero() {
			row["createTime"] = rec.CreatedAt.UnixMilli()
		}
		executionTimeDTO(row, rec.ExecutionTime(), rec.StartedAt, rec.FinishedAt, rec.Counters.SucceededBytes, time.Now())
	}
	return row
}

func (s *Server) allTaskParents() ([]task.Parent, bool, error) {
	parents, err := s.tasks.repo.Recent(0, 10001, false)
	if len(parents) > 10000 {
		return parents[:10000], true, err
	}
	return parents, false, err
}
func (s *Server) taskListPage(w http.ResponseWriter, r *http.Request, deleted bool) ([]task.Parent, int64, bool) {
	before, limit := int64(0), 10000
	if raw := r.URL.Query().Get("beforeId"); raw != "" {
		var err error
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 1 {
			fail(w, 400, "beforeId must be a positive task ID")
			return nil, 0, false
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 10000 {
			fail(w, 400, "Task page limit must be between 1 and 10000")
			return nil, 0, false
		}
	}
	parents, err := s.tasks.repo.Recent(before, limit+1, deleted)
	if err != nil {
		fail(w, 500, "Cannot read task history")
		return nil, 0, false
	}
	var next int64
	if len(parents) > limit {
		parents = parents[:limit]
		next = parents[len(parents)-1].ID
	}
	return parents, next, true
}

// Old clients use PENDING/LIST_DONE; filtering groups them without rewriting records.
func taskStatusGroup(status string) string {
	switch status {
	case "PENDING":
		return "WAITING"
	case "LIST_DONE":
		return "RUNNING"
	default:
		return status
	}
}
func (s *Server) taskStatusHTTP(w http.ResponseWriter, r *http.Request) {
	parents, next, ok := s.taskListPage(w, r, false)
	if !ok {
		return
	}
	truncated := next > 0
	partial := truncated && !r.URL.Query().Has("limit") && !r.URL.Query().Has("beforeId")
	rows := []map[string]any{}
	result := map[string]any{"success": true, "partial": partial, "pageComplete": true, "hasMore": truncated, "tasks": rows, "pendingTasks": 0, "runningTasks": 0, "listDoneTasks": 0, "completedTasks": 0, "failedTasks": 0, "partialFailedTasks": 0, "stoppedTasks": 0, "totalPutObjectCount": int64(0), "totalPutObjectSize": int64(0), "totalFailedObjectCount": int64(0)}
	q := r.URL.Query()
	displays := s.nodeDisplaySnapshot()
	for _, p := range parents {
		if p.Deleted || p.Purged {
			continue
		}
		if q.Get("status") != "" && taskStatusGroup(q.Get("status")) != taskStatusGroup(p.Status) {
			continue
		}
		if q.Get("sourceBucket") != "" && q.Get("sourceBucket") != p.Spec.Source.Bucket {
			continue
		}
		if q.Get("destBucket") != "" && q.Get("destBucket") != p.Spec.Destination.Bucket {
			continue
		}
		if q.Get("workerNode") != "" {
			found := false
			for _, node := range p.Spec.Nodes {
				found = found || node.Address == q.Get("workerNode")
			}
			if !found {
				continue
			}
		}
		if r.URL.Path == "/api/master/local-transfers" && p.Spec.Direction == "OSS_TO_OSS" {
			continue
		}
		rows = append(rows, s.taskView(p, displays))
		metric := map[string]string{"RUNNING": "runningTasks", "WAITING": "pendingTasks", "COMPLETED": "completedTasks", "FAILED": "failedTasks", "PARTIAL_FAILED": "partialFailedTasks", "STOPPED": "stoppedTasks"}[taskStatusGroup(p.Status)]
		if metric != "" {
			result[metric] = result[metric].(int) + 1
		}
		result["totalPutObjectCount"] = result["totalPutObjectCount"].(int64) + p.Counters.Succeeded
		result["totalPutObjectSize"] = result["totalPutObjectSize"].(int64) + p.Counters.SucceededBytes
		result["totalFailedObjectCount"] = result["totalFailedObjectCount"].(int64) + p.Counters.Failed
	}
	result["nodeDisplays"] = displays.byAddress
	result["tasks"] = rows
	result["totalTasks"] = len(rows)
	if truncated {
		result["nextBeforeId"] = next
		result["message"] = localize(w, "Showing recent tasks; load an earlier page for more history")
	}
	result["truncated"] = truncated
	writeJSON(w, 200, result)
}

func (s *Server) taskDetailHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	p, err := s.tasks.repo.Get(id)
	if err != nil {
		if errors.Is(err, task.ErrNotFound) {
			fail(w, 404, "Task not found")
		} else {
			fail(w, 500, "Cannot read task")
		}
		return
	}
	if p.Deleted || p.Purged {
		fail(w, 404, "Task not found")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": s.taskView(p)})
}
func taskPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		fail(w, 400, "Invalid task ID")
		return 0, false
	}
	return id, true
}
func (s *Server) taskChildrenHTTP(w http.ResponseWriter, r *http.Request) {
	displays := s.nodeDisplaySnapshot()
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	parent, err := s.tasks.repo.Get(id)
	if err != nil && !errors.Is(err, task.ErrNotFound) {
		fail(w, 500, "Cannot read task")
		return
	}
	if err != nil || parent.Purged || parent.Deleted {
		fail(w, 404, "Task not found")
		return
	}
	offset, limit := 0, 25
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, err = strconv.Atoi(raw)
		if err != nil || offset < 0 {
			fail(w, 400, "Invalid child offset")
			return
		}
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			fail(w, 400, "Child page size must be 1 to 200")
			return
		}
	}
	if raw := r.PathValue("number"); raw != "" {
		number, err := strconv.Atoi(raw)
		if err != nil || number < 1 {
			fail(w, 400, "Invalid child number")
			return
		}
		offset = number - 1
		limit = 1
	}
	active := r.URL.Query().Get("view") == "active" && r.PathValue("number") == ""
	priorityOrder := r.PathValue("number") == "" && r.URL.Query().Get("order") == "priority"
	cursor := r.URL.Query().Get("cursor")
	var known []int
	validCursor := task.ValidAllChildOrderCursor
	if active {
		validCursor = task.ValidChildOrderCursor
	}
	if priorityOrder && cursor != "" && !validCursor(cursor) {
		fail(w, 400, "Invalid child cursor")
		return
	}
	if raw := r.URL.Query().Get("known"); priorityOrder && raw != "" {
		parts := strings.Split(raw, ",")
		if len(parts) > 200 {
			fail(w, 400, "Too many visible child tasks")
			return
		}
		seen := map[int]bool{}
		for _, part := range parts {
			number, err := strconv.Atoi(part)
			if err != nil || number < 1 || seen[number] {
				fail(w, 400, "Invalid visible child task")
				return
			}
			known, seen[number] = append(known, number), true
		}
	}
	if view := r.URL.Query().Get("view"); view != "" && view != "all" && view != "active" {
		fail(w, 400, "Invalid child view")
		return
	}
	if active && task.Ended(parent.Status) {
		writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"parentTaskId": id, "offset": offset, "limit": limit, "totalCount": 0, "items": []any{}, "hasMore": false, "nextOffset": offset, "partial": false, "cached": true}})
		return
	}
	if parent.Legacy != nil {
		children, err := s.tasks.repo.LegacyChildrenPage(id, offset, limit)
		if r.PathValue("number") != "" {
			children, err = s.tasks.repo.LegacyChildren(id, offset, limit)
		}
		if err != nil {
			fail(w, 500, "Cannot read historical child tasks")
			return
		}
		items := []map[string]any{}
		for _, child := range children {
			if selected := r.URL.Query().Get("executionNodeIp"); selected != "" && selected != child.Node {
				continue
			}
			if r.PathValue("number") != "" && child.Number != offset+1 {
				continue
			}
			row, err := legacyChildDTO(parent, child)
			if err != nil {
				fail(w, 500, "Cannot read historical child metadata")
				return
			}
			addExecutionNodeDisplay(row, displays.lookup("", child.Node))
			items = append(items, row)
		}
		writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"parentTaskId": id, "offset": offset, "limit": limit, "totalCount": parent.ChildCount, "items": items, "partial": false, "cached": true, "legacyHistorical": true, "hasMore": offset+len(children) < parent.ChildCount, "nextOffset": offset + len(children)}})
		return
	}
	fetch := s.tasks.repo.Children
	if active {
		fetch = s.tasks.repo.Outstanding
	}
	var children []task.Child
	var updates []task.Child
	ready := true
	if priorityOrder {
		if active {
			children, updates, err = s.tasks.repo.ActiveChildren(id, cursor, limit+1, known)
		} else {
			children, updates, ready, err = s.tasks.repo.PriorityChildren(r.Context(), id, cursor, limit+1, known)
		}
	} else {
		children, err = fetch(id, offset, limit+1)
	}
	if err != nil {
		fail(w, 500, "Cannot read child tasks")
		return
	}
	if !ready {
		writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"parentTaskId": id, "offset": offset, "limit": limit, "totalCount": parent.ChildCount, "items": []any{}, "updates": []any{}, "hasMore": false, "nextOffset": offset, "partial": true, "cached": true, "order": "priority", "cursor": cursor, "nextCursor": cursor, "indexing": true}})
		return
	}
	hasMore := len(children) > limit
	if hasMore {
		children = children[:limit]
	}
	nextOffset := offset
	total := parent.ChildCount
	if active {
		total = max(0, parent.ChildCount-parent.FinishedChildren)
	}
	items := []map[string]any{}
	nextCursor := cursor
	for _, child := range children {
		nextOffset = child.Job.Number
		if active {
			nextCursor = task.ChildOrderCursor(child)
		} else {
			nextCursor = task.AllChildOrderCursor(child)
		}
		if selected := r.URL.Query().Get("executionNodeIp"); selected != "" && selected != child.Node.Address {
			continue
		}
		items = append(items, s.childView(parent, child, displays))
	}
	// observedAt belongs to each persisted Agent record. Generating a current
	// snapshotAt here would make offline observations appear fresh.
	data := map[string]any{"parentTaskId": id, "offset": offset, "limit": limit, "totalCount": total, "items": items, "hasMore": hasMore, "nextOffset": nextOffset, "partial": false, "cached": true}
	if priorityOrder {
		data["order"], data["cursor"], data["nextCursor"] = "priority", cursor, nextCursor
		// The offset identifies a loaded UI window; only the composite cursor
		// determines which children follow it in the current priority order.
		data["nextOffset"] = offset + len(children)
		updated := []map[string]any{}
		for _, child := range updates {
			updated = append(updated, s.childView(parent, child, displays))
		}
		data["updates"] = updated
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": data})
}
func (s *Server) controlTaskHTTP(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			TaskIDs             []int64          `json:"taskIds"`
			ExpectedGenerations map[int64]uint64 `json:"expectedGenerations,omitempty"`
		}
		if !readJSON(w, r, &request) {
			return
		}
		if len(request.TaskIDs) == 0 {
			fail(w, 400, "Select at least one task")
			return
		}
		seen := map[int64]bool{}
		for _, id := range request.TaskIDs {
			if id < 1 || seen[id] {
				fail(w, 400, "Invalid or duplicate task IDs")
				return
			}
			seen[id] = true
		}
		resuming := action == "resume" || action == "retry"
		if request.ExpectedGenerations != nil {
			if !resuming || len(request.ExpectedGenerations) != len(request.TaskIDs) {
				fail(w, 400, "Expected generations must match the selected tasks")
				return
			}
			for id, generation := range request.ExpectedGenerations {
				if !seen[id] || generation == 0 {
					fail(w, 400, "Expected generations must match the selected tasks")
					return
				}
			}
		}
		displays := s.nodeDisplaySnapshot()
		outcomes := s.runTaskControls(r.Context(), request.TaskIDs, func(ctx context.Context, id int64) (task.Parent, bool, error) {
			if action == "permanent-delete" {
				return s.requestTaskPurge(ctx, id)
			}
			if action == "stop" {
				return s.requestTaskStop(ctx, id)
			}
			if resuming {
				var expected *uint64
				if request.ExpectedGenerations != nil {
					value := request.ExpectedGenerations[id]
					expected = &value
				}
				return s.requestTaskResume(ctx, id, expected)
			}
			p, err := s.controlTask(ctx, id, action)
			return p, true, err
		})
		accepted, failed, ignored := 0, 0, 0
		details := make([]map[string]any, 0, len(request.TaskIDs))
		for index, id := range request.TaskIDs {
			outcome := outcomes[index]
			p, admitted, err := outcome.parent, outcome.admitted, outcome.err
			item := map[string]any{"taskId": id, "status": p.Status, "generation": p.Generation, "success": err == nil, "accepted": err == nil && admitted, "ignored": err == nil && !admitted}
			if action == "permanent-delete" && err == nil {
				item["purged"] = p.Purged
				if p.Cleanup != nil {
					item["cleanup"] = cleanupDTO(p.Cleanup)
				}
			}
			if err != nil {
				failed++
				item["message"] = err.Error()
			} else if !admitted {
				ignored++
			} else {
				accepted++
				if action == "permanent-delete" {
					retained := []string{}
					retainedLabels := []string{}
					for _, node := range p.Spec.Nodes {
						if retired, _ := s.agents.registry.IsRevoked(nodeSession(node)); retired {
							retained = append(retained, node.Address)
							retainedLabels = append(retainedLabels, displays.lookup(node.ID, node.Address).DisplayLabel)
						}
					}
					if len(retained) > 0 {
						item["retainedAgentHistory"] = retained
						item["message"] = localize(w, "已受理清理；已移除节点的本地历史保留：") + strings.Join(retainedLabels, "、")
					}
				}
			}
			details = append(details, item)
		}
		result := map[string]any{"success": true, "accepted": accepted, "failed": failed, "ignored": ignored, "partial": failed > 0, "details": details}
		if strings.HasPrefix(r.URL.Path, "/api/master/local-transfers/") {
			writeJSON(w, 200, map[string]any{"success": true, "data": result})
		} else {
			writeJSON(w, 200, result)
		}
	}
}

func (s *Server) deletedTasksHTTP(w http.ResponseWriter, r *http.Request) {
	displays := s.nodeDisplaySnapshot()
	parents, next, ok := s.taskListPage(w, r, true)
	if !ok {
		return
	}
	truncated := next > 0
	partial := truncated && !r.URL.Query().Has("limit") && !r.URL.Query().Has("beforeId")
	members, err := s.agents.registry.Members()
	if err != nil {
		fail(w, 500, "Cannot read Agent membership")
		return
	}
	groups := map[string][]map[string]any{}
	queried := []string{}
	for _, node := range members {
		groups[node.Address] = []map[string]any{}
		queried = append(queried, node.Address)
	}
	total := 0
	for _, p := range parents {
		if !p.Deleted || p.Purged {
			continue
		}
		address := "master"
		if len(p.Spec.Nodes) > 0 {
			address = p.Spec.Nodes[0].Address
		}
		if _, ok := groups[address]; !ok {
			groups[address] = []map[string]any{}
			queried = append(queried, address)
		}
		groups[address] = append(groups[address], s.taskView(p, displays))
		total++
	}
	result := map[string]any{"success": true, "deletedTasks": groups, "nodeDisplays": displays.byAddress, "totalCount": total, "queriedWorkers": queried, "complete": !partial, "partial": partial, "pageComplete": true, "hasMore": truncated, "truncated": truncated, "failedWorkers": []string{}, "unreachableWorkers": []string{}}
	if next > 0 {
		result["nextBeforeId"] = next
	}
	writeJSON(w, 200, result)
}

func (s *Server) taskBandwidthHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := taskPathID(w, r)
	if !ok {
		return
	}
	parent, err := s.tasks.repo.Get(id)
	if err != nil && !errors.Is(err, task.ErrNotFound) {
		fail(w, 500, "Cannot read task")
		return
	}
	if err != nil || parent.Deleted || parent.Purged {
		fail(w, 404, "Task not found")
		return
	}
	if r.Method == http.MethodPut {
		var request struct {
			LimitMbps          json.RawMessage          `json:"limitMbps"`
			RequestLimits      json.RawMessage          `json:"requestLimits"`
			RequestLimitsPatch *task.RequestLimitsPatch `json:"requestLimitsPatch"`
		}
		if !readJSON(w, r, &request) {
			return
		}
		var limit *float64
		if len(request.LimitMbps) > 0 {
			var value *float64
			if err := json.Unmarshal(request.LimitMbps, &value); err != nil {
				fail(w, 400, "Invalid bandwidth limit")
				return
			}
			parsed, err := taskBandwidthValue(value)
			if err != nil {
				fail(w, 400, err.Error())
				return
			}
			limit = &parsed
		}
		var limits *execution.RequestLimits
		replaceRequests := len(request.RequestLimits) > 0
		if replaceRequests {
			decoder := json.NewDecoder(bytes.NewReader(request.RequestLimits))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&limits); err != nil {
				fail(w, 400, "Invalid request limits")
				return
			}
			if err := execution.ValidateRequestLimits(limits); err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
		if request.RequestLimitsPatch != nil {
			patch := request.RequestLimitsPatch
			if replaceRequests {
				fail(w, 400, "Cannot replace and patch request limits together")
				return
			}
			value := execution.RequestLimits{}
			if patch.ListQPS != nil {
				value.ListQPS = *patch.ListQPS
			}
			if patch.HeadQPS != nil {
				value.HeadQPS = *patch.HeadQPS
			}
			if patch.PutQPS != nil {
				value.PutQPS = *patch.PutQPS
			}
			if err := execution.ValidateRequestLimits(&value); err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
		if limit == nil && !replaceRequests && request.RequestLimitsPatch == nil {
			fail(w, 400, "No task limits provided")
			return
		}
		if task.Ended(parent.Status) {
			fail(w, 409, "Bandwidth cannot be changed after the task ends")
			return
		}
		parent, err = s.changeTaskLimits(r.Context(), id, limit, limits, replaceRequests, request.RequestLimitsPatch)
		if err != nil {
			fail(w, 409, err.Error())
			return
		}
	}
	var value any
	if parent.Spec.BandwidthMbps > 0 {
		value = parent.Spec.BandwidthMbps
	}
	members, err := s.onlineAgents()
	if err != nil {
		fail(w, 500, "Cannot read execution Agents")
		return
	}
	online := make(map[string]string, len(members))
	for _, member := range members {
		online[member.ID] = member.EnrollmentID
	}
	displays := s.nodeDisplaySnapshot()
	activeNodes := parent.ActiveNodes()
	nodes := make([]map[string]any, len(activeNodes))
	// All lookups share one deadline, and a bounded set of workers. A dead
	// Agent must not hold the UI open for thirty seconds per selected node.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	indexes := make(chan int, len(nodes))
	for i := range nodes {
		indexes <- i
	}
	close(indexes)
	var workers sync.WaitGroup
	for i := 0; i < min(8, len(nodes)); i++ {
		workers.Go(func() {
			for index := range indexes {
				node := activeNodes[index]
				if online[node.ID] != node.EnrollmentID {
					nodes[index] = map[string]any{"address": node.Address, "applied": false, "message": localize(w, "执行节点未连接")}
					addNodeDisplay(nodes[index], displays.lookup(node.ID, node.Address))
					continue
				}
				var policy ratelimit.ParentLimit
				err := s.agentCall(ctx, cluster.Member{ID: node.ID, EnrollmentID: node.EnrollmentID}.Session(), "bandwidth_parent_status", false, map[string]any{"parentId": parent.ID}, &policy)
				expected := parentPolicy(parent, index)
				matches := err == nil && policy.Generation == expected.Generation && policy.Revision == expected.Revision && policy.BandwidthBytesPerSecond == expected.BandwidthBytesPerSecond && policy.ListQPS == expected.ListQPS && policy.HeadQPS == expected.HeadQPS && policy.PutQPS == expected.PutQPS && policy.Stopped == expected.Stopped
				item := map[string]any{"address": node.Address, "applied": matches}
				addNodeDisplay(item, displays.lookup(node.ID, node.Address))
				if err != nil {
					item["message"] = err.Error()
				}
				nodes[index] = item
			}
		})
	}
	workers.Wait()
	applied := len(nodes) > 0
	for _, node := range nodes {
		applied = applied && node["applied"] == true
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": map[string]any{"limitMbps": value, "requestLimits": parent.Spec.RequestLimits, "applied": applied, "sharedTaskCount": 1, "canEdit": !task.Ended(parent.Status), "nodes": nodes}})
}
