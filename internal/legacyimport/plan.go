package legacyimport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func planEndpoint(value string) string {
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "(invalid endpoint)"
	}
	return u.Scheme + "://" + u.Host
}

func category(key string) string {
	for _, match := range []struct{ prefix, category string }{
		{"user:", "users"}, {"s3_config_", "connections"}, {"node_config", "node-settings"},
		{"agent:identity", "agent-identity"}, {"agent:member:", "agent-membership"}, {"agent:invite:", "agent-invitations"}, {"agent:storage-version", "agent-membership"},
		{"rate_limit:manual:", "node-rate-targets"}, {"rate_adjustment:", "node-rate-history"}, {"rate_event:", "node-rate-history"}, {"rate_incident", "node-rate-history"},
		{"task_bandwidth:", "task-bandwidth"}, {"task-bandwidth:policy:", "task-bandwidth"},
		{"task:purged:", "purge-fences"}, {"report:purged:", "purge-fences"}, {"local:parent:purged:", "purge-fences"},
		{"task:deleted:", "recycle-bin"}, {"local:parent:deleted:", "recycle-bin"}, {"task:id:list:deleted", "recycle-bin"},
		{"task_creation:", "task-catalogue"}, {"task_creation_ids", "task-catalogue"}, {"local:parent:", "task-catalogue"}, {"task:id:", "task-catalogue"}, {"task:", "tasks"},
		{"completed:", "completion-ledger"}, {"file-report:", "file-report-ledger"}, {"inventory:", "inventory"},
		{"local:explore:", "discovery"}, {"local:discovery:", "discovery"}, {"local:batch-assignment:", "discovery"}, {"local:shard-assignment:", "discovery"}, {"local:stop:", "stop-fences"},
		{"failed:", "exceptions"}, {"fallback:", "exceptions"}, {"local-temp:", "temporary-files"},
	} {
		if strings.HasPrefix(key, match.prefix) {
			return match.category
		}
	}
	return "unknown"
}

// Values remain in the original export. Only the small, non-secret mapping
// facts below are retained in memory or printed by the inspection command.
func (p *Plan) observe(key, raw []byte) error {
	if !utf8.Valid(key) || !utf8.Valid(raw) || !json.Valid(raw) {
		return invalid("key is not UTF-8 or value is not UTF-8 JSON; raw export must be reviewed")
	}
	k := string(key)
	c := category(k)
	p.Categories[c]++
	if c == "unknown" {
		p.unknown++
	}
	if len(p.Users)+len(p.Connections)+len(p.Tasks)+len(p.manifests) >= maxFacts {
		return invalid("mapping facts exceed inspection capacity")
	}
	switch {
	case c == "users":
		var row struct{ Username, PasswordHash, Role string }
		if json.Unmarshal(raw, &row) != nil || row.Username == "" || k != "user:"+row.Username {
			return invalid("malformed account record")
		}
		kind := "unsupported"
		parts := strings.Split(row.PasswordHash, ":")
		if len(parts) == 2 {
			salt, e1 := hex.DecodeString(parts[0])
			hash, e2 := hex.DecodeString(parts[1])
			if e1 == nil && e2 == nil && len(salt) == 16 && len(hash) == 32 {
				kind = "V4_SHA256"
			}
		}
		p.Users = append(p.Users, UserPlan{row.Username, row.Role, kind})
		if kind == "unsupported" {
			p.issue("UNSUPPORTED_PASSWORD_HASH", k, "Do not reset or replace this account while importing.")
		}
		if row.Role != "admin" && row.Role != "user" {
			p.issue("UNSUPPORTED_ROLE", k, "Do not promote an unknown account role to administrator.")
		}
	case c == "connections":
		var row struct {
			Alias, Endpoint, AccessKey, SecretKey string
			UseV2SignerType, PathStyleEnable      *bool
		}
		if json.Unmarshal(raw, &row) != nil || row.Alias == "" || k != "s3_config_"+row.Alias || row.Endpoint == "" || row.AccessKey == "" || row.SecretKey == "" {
			return invalid("malformed storage connection")
		}
		signature := "v4"
		if row.UseV2SignerType != nil && *row.UseV2SignerType {
			signature = "v2"
		}
		normalized, _ := json.Marshal(row)
		h := sha256.Sum256(normalized)
		p.Connections = append(p.Connections, ConnectionPlan{Alias: row.Alias, Endpoint: planEndpoint(row.Endpoint), Region: "default", Signature: signature, PathStyle: row.PathStyleEnable != nil && *row.PathStyleEnable, fingerprint: hex.EncodeToString(h[:])})
	case k == "node_config":
		var row struct {
			LocalIP   string   `json:"localIp"`
			WorkerIPs []string `json:"workerIps"`
		}
		if json.Unmarshal(raw, &row) != nil {
			return invalid("malformed node configuration")
		}
		if row.LocalIP != "" && row.LocalIP != p.Header.Node {
			p.issue("NODE_IDENTITY_MISMATCH", "node_config", "The export label differs from the stored node identity.")
		}
		if p.Header.Role == "master" {
			p.ExpectedNodes = append([]string(nil), row.WorkerIPs...)
		}
	case k == "agent:identity":
		var row struct {
			Address, InstanceID, Credential string
			Joined                          bool
		}
		if json.Unmarshal(raw, &row) != nil || row.Address == "" || row.InstanceID == "" || row.Credential == "" {
			return invalid("malformed Agent identity")
		}
		if p.Header.Role != "agent" || row.Address != p.Header.Node {
			p.issue("NODE_IDENTITY_MISMATCH", k, "The saved Agent identity must match the Master membership and this export label.")
		}
		if !row.Joined {
			p.issue("PENDING_ENROLLMENT", k, "An unfinished V4.0.x enrollment is not an authenticated Go Agent identity.")
		}
	case c == "tasks" || strings.HasPrefix(k, "task:deleted:"):
		var row struct {
			TaskIndex                              int64  `json:"taskIndex"`
			ParentTaskID                           *int64 `json:"parentTaskId"`
			Status, Direction, InventoryManifestID string
			LocalFileEntries                       []json.RawMessage
		}
		if json.Unmarshal(raw, &row) != nil {
			return invalid("malformed task record")
		}
		idText := strings.TrimPrefix(strings.TrimPrefix(k, "task:deleted:"), "task:")
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil || strconv.FormatInt(id, 10) != idText || row.TaskIndex != id {
			return invalid("task key and identity differ")
		}
		direction := row.Direction
		if direction == "" {
			direction = "OSS_TO_OSS"
		}
		disposition := "PRESERVE_HISTORY_REQUIRE_RECOVERY_REVIEW"
		if row.Status == "COMPLETED" {
			disposition = "PRESERVE_HISTORICAL_RESULT"
		}
		p.Tasks = append(p.Tasks, TaskPlan{k, id, row.ParentTaskID, row.Status, direction, strings.HasPrefix(k, "task:deleted:"), disposition, row.InventoryManifestID, len(row.LocalFileEntries)})
	case strings.HasPrefix(k, "inventory:") && strings.HasSuffix(k, ":ready"):
		var ready bool
		if json.Unmarshal(raw, &ready) != nil {
			return invalid("malformed inventory readiness marker")
		}
		p.manifests[strings.TrimSuffix(strings.TrimPrefix(k, "inventory:"), ":ready")] = ready
	}
	return nil
}

type ClusterPlan struct {
	ReadyToImport bool    `json:"readyToImport"`
	Nodes         []Plan  `json:"nodes"`
	Issues        []Issue `json:"issues"`
}

// Combine checks cluster coverage, not source/target file contents. The result
// remains a plan even when all snapshots are structurally complete.
func Combine(plans []Plan) (ClusterPlan, error) {
	result := ClusterPlan{Nodes: plans, Issues: []Issue{}}
	byNode := map[string]Plan{}
	var master *Plan
	for i := range plans {
		p := &plans[i]
		if _, exists := byNode[p.Header.Node]; exists {
			return ClusterPlan{}, invalid("duplicate node snapshot")
		}
		byNode[p.Header.Node] = *p
		if p.Header.Role == "master" {
			if master != nil {
				return ClusterPlan{}, invalid("multiple Master snapshots")
			}
			master = p
		}
	}
	if master == nil {
		return ClusterPlan{}, invalid("one Master snapshot is required")
	}
	for _, node := range master.ExpectedNodes {
		if _, exists := byNode[node]; !exists {
			result.Issues = append(result.Issues, Issue{"NODE_SNAPSHOT_MISSING", node, "This execution node's task ledger and checkpoints have not been exported."})
		}
	}
	masterConnections := map[string]ConnectionPlan{}
	for _, c := range master.Connections {
		masterConnections[c.Alias] = c
	}
	for _, p := range plans {
		for _, c := range p.Connections {
			if authoritative, ok := masterConnections[c.Alias]; !ok || authoritative.fingerprint != c.fingerprint {
				result.Issues = append(result.Issues, Issue{"CONNECTION_DRIFT", p.Header.Node + "/" + c.Alias, "This node's connection differs from the Master snapshot; do not silently overwrite it."})
			}
		}
	}
	sort.Slice(result.Nodes, func(i, j int) bool { return result.Nodes[i].Header.Node < result.Nodes[j].Header.Node })
	result.Issues = append(result.Issues, Issue{"RUNTIME_CONVERSION_PENDING", "", "Snapshots are preserved for conversion review; no task is executable from this plan."})
	return result, nil
}
