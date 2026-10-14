package legacyimport

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

func addBlocker(g *oldGroup, code string) {
	for _, old := range g.Blockers {
		if old == code {
			return
		}
	}
	g.Blockers = append(g.Blockers, code)
}
func oldTime(ms *int64) *time.Time {
	if ms == nil || *ms <= 0 {
		return nil
	}
	t := time.UnixMilli(*ms).UTC()
	return &t
}
func knownFinished(status string) bool {
	return status == "COMPLETED" || status == "PARTIAL_FAILED" || status == "FAILED"
}

func assignLegacyNumbers(g *oldGroup) error {
	creation := map[int64]int{}
	if g.Creation != nil {
		for i, id := range g.Creation.TaskIDs {
			if _, ok := creation[id]; !ok {
				creation[id] = i + 1
			}
		}
	}
	pattern := regexp.MustCompile(`^(?:` + strconv.FormatInt(g.ID, 10) + `-(?:oss|local)|batch|oss-.+)-(\d+)$`)
	occupied := map[int]bool{}
	for _, child := range g.Children {
		if child.Value.ShardIndex != nil && *child.Value.ShardIndex >= 0 {
			child.Number = *child.Value.ShardIndex + 1
		} else if matched := pattern.FindStringSubmatch(child.Value.ShardID); matched != nil {
			n, err := strconv.ParseInt(matched[1], 10, 32)
			if err == nil {
				child.Number = int(n) + 1
			}
		}
		if child.Number == 0 {
			child.Number = creation[child.Value.TaskIndex]
		}
		if child.Number > 0 {
			if occupied[child.Number] {
				return errors.New("multiple legacy records claim the same public child number; review node copies before import")
			}
			occupied[child.Number] = true
		}
	}
	sort.Slice(g.Children, func(i, j int) bool {
		a, b := g.Children[i], g.Children[j]
		if a.Number > 0 && b.Number > 0 {
			return a.Number < b.Number
		}
		if a.Number > 0 {
			return true
		}
		if b.Number > 0 {
			return false
		}
		aKey, bKey := a.Value.ShardID, b.Value.ShardID
		if aKey == "" {
			aKey = strconv.FormatInt(a.Value.TaskIndex, 10)
		}
		if bKey == "" {
			bKey = strconv.FormatInt(b.Value.TaskIndex, 10)
		}
		if aKey == bKey {
			return a.Node < b.Node
		}
		return naturalLess(aKey, bKey)
	})
	next := 1
	for _, child := range g.Children {
		if child.Number != 0 {
			continue
		}
		for occupied[next] {
			next++
		}
		child.Number = next
		occupied[next] = true
		next++
	}
	return nil
}
func naturalLess(a, b string) bool {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] >= '0' && a[i] <= '9' && b[j] >= '0' && b[j] <= '9' {
			ia, jb := i, j
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			na, nb := strings.TrimLeft(a[ia:i], "0"), strings.TrimLeft(b[jb:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
		} else {
			aa, bb := strings.ToLower(a[i:i+1]), strings.ToLower(b[j:j+1])
			if aa != bb {
				return aa < bb
			}
			i++
			j++
		}
	}
	return len(a)-i < len(b)-j
}
func readDate(value, zone string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return &parsed, nil
	}
	if zone == "" {
		return nil, errors.New("LEGACY_DATE_TIMEZONE_REQUIRED")
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, errors.New("LEGACY_DATE_TIMEZONE_INVALID")
	}
	parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value, loc)
	if err != nil {
		return nil, errors.New("LEGACY_DATE_FILTER_INVALID")
	}
	return &parsed, nil
}
func oldStore(ep *oldEndpoint, alias, bucket, kind string) execution.Endpoint {
	if ep != nil {
		if ep.Type == "LOCAL" {
			return execution.Endpoint{Kind: "LOCAL", Root: ep.LocalPath}
		}
		if ep.S3Alias != "" {
			alias = ep.S3Alias
		}
		if ep.Bucket != "" {
			bucket = ep.Bucket
		}
	}
	return execution.Endpoint{Kind: kind, Ref: alias, Bucket: bucket}
}
func (p *Prepared) mapSpec(g *oldGroup, request oldRequest, options TaskImportOptions) task.Spec {
	direction := request.Direction
	if direction == "" {
		direction = "OSS_TO_OSS"
	}
	spec := task.Spec{Direction: direction, ConflictPolicy: request.ConflictPolicy, DistributionMode: strings.ToUpper(request.DistributionMode), Prefix: request.Prefix, ObjectKey: request.ObjectKey, StartKey: request.StartKey, EndKey: request.EndKey, Marker: request.Marker}
	if spec.ConflictPolicy == "" {
		spec.ConflictPolicy = "SKIP"
		if request.Overwrite != nil && *request.Overwrite {
			spec.ConflictPolicy = "OVERWRITE"
		}
	}
	if spec.ConflictPolicy != "SKIP" && spec.ConflictPolicy != "OVERWRITE" {
		addBlocker(g, "CONFLICT_POLICY_UNSUPPORTED")
	}
	if spec.DistributionMode == "" {
		spec.DistributionMode = "AFFINITY"
	}
	if spec.DistributionMode != "BALANCED" && spec.DistributionMode != "AFFINITY" {
		addBlocker(g, "DISTRIBUTION_MODE_UNSUPPORTED")
	}
	spec.Source = oldStore(request.SourceEndpoint, request.SourceS3Alias, request.SourceBucket, "S3")
	spec.Destination = oldStore(request.DestinationEndpoint, request.DestS3Alias, request.DestBucket, "S3")
	if request.SourceEndpoint != nil {
		if request.SourceEndpoint.ObjectKey != "" {
			spec.ObjectKey = request.SourceEndpoint.ObjectKey
		}
		if request.SourceEndpoint.Prefix != "" {
			spec.Prefix = request.SourceEndpoint.Prefix
		}
		if request.SourceEndpoint.PrefixFilterEnabled != nil && !*request.SourceEndpoint.PrefixFilterEnabled {
			spec.Prefix = ""
		}
	}
	var local *oldEndpoint
	switch direction {
	case "OSS_TO_LOCAL":
		local = request.DestinationEndpoint
		spec.Destination.Kind = "LOCAL"
		spec.Destination.Ref, spec.Destination.Bucket = "", ""
		spec.TargetKey = request.TargetRelativePath
	case "LOCAL_TO_OSS":
		local = request.SourceEndpoint
		spec.Source.Kind = "LOCAL"
		spec.Source.Ref, spec.Source.Bucket = "", ""
		if request.DestinationEndpoint != nil {
			spec.TargetPrefix = request.DestinationEndpoint.Prefix
			spec.TargetKey = request.DestinationEndpoint.ObjectKey
		}
		addBlocker(g, "LOCAL_SOURCE_KIND_REQUIRES_PROBE")
	case "OSS_TO_OSS":
	default:
		addBlocker(g, "DIRECTION_UNSUPPORTED")
	}
	if request.ListQPSLimit != nil || request.HeadQPSLimit != nil || request.PutQPSLimit != nil || request.BandwidthBitsPerSecond != nil {
		spec.RequestLimits = &execution.RequestLimits{Scope: "PER_NODE"}
		if request.ListQPSLimit != nil {
			spec.RequestLimits.ListQPS = *request.ListQPSLimit
		}
		if request.HeadQPSLimit != nil {
			spec.RequestLimits.HeadQPS = *request.HeadQPSLimit
		}
		if request.PutQPSLimit != nil {
			spec.RequestLimits.PutQPS = *request.PutQPSLimit
		}
		if request.BandwidthBitsPerSecond != nil {
			spec.RequestLimits.BandwidthBitsPerSecond = *request.BandwidthBitsPerSecond
		}
		if err := execution.ValidateRequestLimits(spec.RequestLimits); err != nil {
			addBlocker(g, "LEGACY_TASK_REQUEST_LIMITS_INVALID")
		}
	}
	if request.TaskBandwidthLimitMbps != nil {
		spec.BandwidthMbps = *request.TaskBandwidthLimitMbps
		if math.IsNaN(spec.BandwidthMbps) || math.IsInf(spec.BandwidthMbps, 0) || spec.BandwidthMbps < 0.1 || spec.BandwidthMbps > 1000000 {
			addBlocker(g, "BANDWIDTH_POLICY_INVALID")
		}
	}
	if mapping := request.KeyMapping; mapping != nil {
		spec.MappingMode = mapping.Mode
		spec.ReplaceSourcePrefix = mapping.SourcePrefix
		spec.TargetPrefix = mapping.TargetPrefix
		spec.TargetKey = mapping.TargetKey
		switch mapping.Mode {
		case "PRESERVE", "ADD_PREFIX", "REPLACE_PREFIX", "EXACT_KEY":
		default:
			addBlocker(g, "KEY_MAPPING_UNSUPPORTED")
		}
	}
	if date, err := readDate(request.DateAfter, options.LegacyTimezone); err != nil {
		addBlocker(g, err.Error())
	} else {
		spec.ModifiedFrom = date
	}
	if date, err := readDate(request.DateBefore, options.LegacyTimezone); err != nil {
		addBlocker(g, err.Error())
	} else {
		spec.ModifiedUntil = date
	}
	if spec.ModifiedFrom != nil && spec.ModifiedUntil != nil && spec.ModifiedFrom.After(*spec.ModifiedUntil) {
		addBlocker(g, "DATE_FILTER_ORDER_INVALID")
	}
	nodes := append([]string(nil), request.EligibleWorkerNodes...)
	roots := map[string]string{}
	bindings := request.NodeBindings
	if local != nil && len(local.NodeBindings) > 0 {
		bindings = local.NodeBindings
	}
	if local != nil && local.ExecutionNodeIP != "" {
		nodes = append(nodes, local.ExecutionNodeIP)
		roots[local.ExecutionNodeIP] = local.LocalPath
	}
	if request.ExecutionNodeIP != "" {
		nodes = append(nodes, request.ExecutionNodeIP)
		roots[request.ExecutionNodeIP] = request.LocalPath
	}
	for _, binding := range bindings {
		nodes = append(nodes, binding.ExecutionNodeIP)
		roots[binding.ExecutionNodeIP] = binding.LocalPath
	}
	nodes = append(nodes, request.ParticipatingWorkerNodes...)
	if g.Creation != nil {
		nodes = append(nodes, g.Creation.ParticipatingWorkerNodes...)
	}
	for _, child := range g.Children {
		nodes = append(nodes, child.Node)
	}
	if len(nodes) == 0 && g.SourceNode != "" {
		nodes = append(nodes, g.SourceNode)
		addBlocker(g, "EXECUTION_NODES_REQUIRE_REVIEW")
	}
	identities := map[string]NodeIdentity{}
	for _, id := range p.Manifest.Nodes {
		identities[id.LegacyNode] = id
	}
	seen := map[string]bool{}
	for _, node := range nodes {
		if seen[node] {
			continue
		}
		seen[node] = true
		identity, ok := identities[node]
		if !ok {
			addBlocker(g, "EXECUTION_NODE_NOT_MAPPED")
			continue
		}
		if identity.Revoked {
			addBlocker(g, "EXECUTION_NODE_REVOKED")
		}
		spec.Nodes = append(spec.Nodes, task.Node{ID: identity.InstanceID, EnrollmentID: identity.EnrollmentID, Address: identity.Address, LocalRoot: roots[node]})
	}
	// Persisted order controls bandwidth remainder allocation. Sort identities
	// deterministically instead of inheriting map iteration order.
	sort.Slice(spec.Nodes, func(i, j int) bool { return spec.Nodes[i].ID < spec.Nodes[j].ID })
	if len(spec.Nodes) == 0 {
		addBlocker(g, "EXECUTION_NODES_MISSING")
	}
	if local != nil {
		root := local.LocalPath
		if root == "" && len(bindings) > 0 {
			root = bindings[0].LocalPath
		}
		if root == "" {
			root = request.LocalPath
		}
		if direction == "LOCAL_TO_OSS" {
			spec.Source.Root = root
		} else {
			spec.Destination.Root = root
		}
		for i := range spec.Nodes {
			if spec.Nodes[i].LocalRoot == "" {
				spec.Nodes[i].LocalRoot = root
			}
		}
		if root == "" {
			addBlocker(g, "LOCAL_ROOT_MISSING")
		}
	} else if direction != "OSS_TO_OSS" {
		addBlocker(g, "LOCAL_ENDPOINT_MISSING")
	}
	if direction == "LOCAL_TO_OSS" && len(spec.Nodes) > 1 {
		spec.DistributionMode = "BALANCED"
		addBlocker(g, "SHARED_LOCAL_SOURCE_REQUIRES_VERIFICATION")
	}
	aliases := map[string]bool{}
	for _, plan := range p.plans {
		if plan.Header.Role == "master" {
			for _, connection := range plan.Connections {
				aliases[connection.Alias] = true
			}
		}
	}
	for _, ep := range []execution.Endpoint{spec.Source, spec.Destination} {
		if ep.Kind == "S3" {
			if ep.Ref == "" {
				addBlocker(g, "DEFAULT_S3_CONNECTION_REQUIRES_MAPPING")
			} else if !aliases[ep.Ref] {
				addBlocker(g, "STORAGE_CONNECTION_NOT_IMPORTED")
			}
			if ep.Bucket == "" {
				addBlocker(g, "BUCKET_MISSING")
			}
		}
	}
	return spec
}
func (p *Prepared) convertParent(c *taskCatalogue, g *oldGroup, options TaskImportOptions) (convertedParent, error) {
	if err := assignLegacyNumbers(g); err != nil {
		return convertedParent{}, err
	}
	row := task.LegacyParentImport{Parent: task.Parent{ID: g.ID, Generation: 1, BudgetRevision: 1, Status: "STOPPED", Deleted: g.Deleted, Purged: g.Purged}, Children: []task.LegacyChild{}}
	parent := &row.Parent
	var request oldRequest
	original := "UNKNOWN"
	switch {
	case g.Creation != nil:
		if g.Local != nil {
			return convertedParent{}, errors.New("public ID is both OSS and local parent")
		}
		creation := g.Creation
		original = creation.Status
		parent.CreatedAt = time.UnixMilli(creation.CreatedAt).UTC()
		if creation.Request != nil {
			request = *creation.Request
		} else {
			addBlocker(g, "CREATION_REQUEST_MISSING")
		}
		parent.ScanComplete = creation.ScanComplete != nil && *creation.ScanComplete
		if len(creation.PendingDispatches) > 0 {
			addBlocker(g, "PENDING_DISPATCH_REQUIRES_RECONCILIATION")
		}
		present := map[int64]bool{}
		for _, child := range g.Children {
			present[child.Value.TaskIndex] = true
		}
		for _, id := range creation.TaskIDs {
			if !present[id] {
				addBlocker(g, "EXECUTION_RECORD_MISSING")
			}
		}
	case g.Local != nil:
		local := g.Local
		original = local.Status
		if local.Request != nil {
			request = *local.Request
		} else {
			addBlocker(g, "LOCAL_REQUEST_MISSING")
		}
		if created := oldTime(local.CreateTime); created != nil {
			parent.CreatedAt = *created
		}
		parent.StartedAt = oldTime(local.StartTime)
		parent.FinishedAt = oldTime(local.EndTime)
		parent.ScanComplete = local.OrchestrationComplete != nil && *local.OrchestrationComplete
		parent.Error = local.ErrorMessage
	case g.Snapshot != nil:
		snapshot := g.Snapshot
		request = snapshot.oldRequest
		original = snapshot.Status
		if created := oldTime(snapshot.CreateTime); created != nil {
			parent.CreatedAt = *created
		}
		parent.StartedAt = oldTime(snapshot.StartTime)
		parent.FinishedAt = oldTime(snapshot.EndTime)
		parent.ScanComplete = snapshot.ScanComplete != nil && *snapshot.ScanComplete
	case len(g.Children) > 0:
		child := g.Children[0]
		request = child.Value.oldRequest
		original = child.Value.Status
		parent.ScanComplete = child.Value.ScanComplete != nil && *child.Value.ScanComplete
	default:
		original = "PURGED"
		if !g.Purged {
			addBlocker(g, "PARENT_METADATA_MISSING")
		}
	}
	parent.Spec = p.mapSpec(g, request, options)
	allCompleted := len(g.Children) > 0
	allFinished := len(g.Children) > 0
	seenDeleted := 0
	nodes := map[string]bool{}
	for _, child := range g.Children {
		source, err := p.sourceRecord(child.Node, child.Key)
		if err != nil {
			return convertedParent{}, err
		}
		row.Children = append(row.Children, task.LegacyChild{ParentID: g.ID, Number: child.Number, Node: child.Node, TaskID: child.Value.TaskIndex, Key: child.Key, Raw: source.Value})
		value := child.Value
		nodes[child.Node] = true
		if child.Deleted {
			seenDeleted++
		}
		if value.Status != "COMPLETED" {
			allCompleted = false
		}
		if !knownFinished(value.Status) {
			allFinished = false
		} else {
			parent.FinishedChildren++
			if value.Status != "COMPLETED" {
				parent.FailedChildren++
			}
		}
		for _, n := range []int64{value.ListObjectCount, value.ListObjectSize, value.PutObjectCount, value.PutObjectSize, value.SkippedObjectCount, value.SkippedObjectSize, value.FailedObjectCount, value.FailedObjectSize} {
			if n < 0 {
				return convertedParent{}, errors.New("negative historical object counter")
			}
		}
		sums := []struct {
			dst   *int64
			value int64
		}{{&parent.ScannedObjects, value.ListObjectCount}, {&parent.ScannedBytes, value.ListObjectSize}, {&parent.Counters.Succeeded, value.PutObjectCount}, {&parent.Counters.SucceededBytes, value.PutObjectSize}, {&parent.Counters.Skipped, value.SkippedObjectCount}, {&parent.Counters.SkippedBytes, value.SkippedObjectSize}, {&parent.Counters.Failed, value.FailedObjectCount}, {&parent.Counters.FailedBytes, value.FailedObjectSize}}
		for _, sum := range sums {
			if *sum.dst > math.MaxInt64-sum.value {
				return convertedParent{}, errors.New("historical object counters overflow")
			}
			*sum.dst += sum.value
		}
		if created := oldTime(value.CreateTime); created != nil && (parent.CreatedAt.IsZero() || created.Before(parent.CreatedAt)) {
			parent.CreatedAt = *created
		}
		if started := oldTime(value.StartTime); started != nil && (parent.StartedAt == nil || started.Before(*parent.StartedAt)) {
			parent.StartedAt = started
		}
		if ended := oldTime(value.EndTime); ended != nil && (parent.FinishedAt == nil || ended.After(*parent.FinishedAt)) {
			parent.FinishedAt = ended
		}
		if manifest := value.InventoryManifestID; manifest != "" {
			c.Inventories[child.Node+"\x00"+manifest] = appendUniqueID(c.Inventories[child.Node+"\x00"+manifest], g.ID)
			if !p.plans[child.Node].manifests[manifest] {
				addBlocker(g, "INVENTORY_MANIFEST_NOT_READY")
			}
		}
	}
	parent.ChildCount = len(row.Children)
	parent.Counters.Total = parent.ScannedObjects
	parent.Counters.TotalBytes = parent.ScannedBytes
	if seenDeleted > 0 {
		if seenDeleted == len(g.Children) && g.Creation == nil && g.Local == nil {
			parent.Deleted = true
		} else if !parent.Deleted {
			addBlocker(g, "INCONSISTENT_RECYCLE_STATE")
		}
	}
	if g.Creation != nil {
		for _, code := range g.Blockers {
			if code == "EXECUTION_RECORD_MISSING" || code == "PENDING_DISPATCH_REQUIRES_RECONCILIATION" || code == "EXECUTION_PURGE_FENCE_REQUIRES_REVIEW" {
				allCompleted = false
				allFinished = false
			}
		}
	}
	if parent.ScanComplete && allCompleted {
		parent.Status = "COMPLETED"
	} else if parent.ScanComplete && allFinished {
		parent.Status = "PARTIAL_FAILED"
		if parent.Counters.Succeeded+parent.Counters.Skipped == 0 {
			parent.Status = "FAILED"
		}
	}
	if g.Creation != nil && g.Creation.Stopped {
		parent.Status = "STOPPED"
	}
	if g.Snapshot != nil && knownFinished(g.Snapshot.Status) && len(g.Children) == 0 {
		parent.Status = g.Snapshot.Status
		addBlocker(g, "HISTORICAL_CHILD_RECORDS_MISSING")
	}
	if g.Local != nil && g.Local.Status == "STOPPED" && !(g.Deleted && g.Snapshot != nil && g.Snapshot.Status == "COMPLETED" && allCompleted) {
		parent.Status = "STOPPED"
	}
	if !parent.ScanComplete {
		addBlocker(g, "DISCOVERY_REQUIRES_RECONCILIATION")
	}
	if parent.Status != "COMPLETED" {
		addBlocker(g, "OBJECT_HISTORY_REQUIRES_RECONCILIATION")
	}
	if parent.Purged {
		parent.Status = "STOPPED"
	}
	scope := request.TaskBandwidthScopeID
	if scope == "" && g.Creation != nil {
		scope = "creation:" + g.Creation.ID
	}
	if scope == "" && g.Local != nil {
		scope = fmt.Sprintf("local:%d", g.ID)
	}
	if scope != "" {
		c.ScopedParents[scope] = appendUniqueID(c.ScopedParents[scope], g.ID)
		if policy, ok := c.Policies[scope]; ok {
			if policy.LimitMbps == nil {
				parent.Spec.BandwidthMbps = 0
			} else {
				parent.Spec.BandwidthMbps = *policy.LimitMbps
			}
			if len(policy.Members) == 0 {
				addBlocker(g, "BANDWIDTH_MEMBERS_MISSING")
			}
		}
	}
	sort.Strings(g.Blockers)
	sourceNodes := make([]string, 0, len(nodes))
	for node := range nodes {
		sourceNodes = append(sourceNodes, node)
	}
	sort.Strings(sourceNodes)
	parent.Legacy = &task.LegacyProvenance{MigrationID: p.Manifest.MigrationID, OriginalStatus: original, NeedsReconciliation: parent.Status != "COMPLETED", Blockers: append([]string(nil), g.Blockers...), SourceNodes: sourceNodes, ScopeID: scope}
	return convertedParent{row, g}, nil
}
func appendUniqueID(ids []int64, id int64) []int64 {
	for _, old := range ids {
		if old == id {
			return ids
		}
	}
	return append(ids, id)
}
