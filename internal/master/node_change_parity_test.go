package master

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// nodeChange posts a node change; an empty language sends no Accept-Language,
// so messages keep their source text.
func nodeChange(t *testing.T, f *agentHTTPFixture, id int64, language string, body map[string]any) (int, map[string]any) {
	t.Helper()
	status, _, result, err := localizedRequest(f.http.Client(), "POST", fmt.Sprintf("%s/api/master/tasks/%d/nodes", f.http.URL, id), f.admin, language, body)
	if err != nil {
		t.Fatal(err)
	}
	return status, result
}

// joinBody adds one node bound to root, with the extra fields.
func joinBody(node task.Node, root string, extra ...map[string]any) map[string]any {
	body := map[string]any{"add": []string{node.Address}, "bindings": []map[string]string{{"executionNodeIp": node.Address, "localPath": root}}}
	for _, fields := range extra {
		for key, value := range fields {
			body[key] = value
		}
	}
	return body
}

var (
	dryRunChange       = map[string]any{"dryRun": true}
	acknowledgedSkip   = map[string]any{"skipSharedCheck": true, "skipSharedCheckAcknowledged": true}
	skipCheckedMessage = "Shared directory check skipped as requested; the nodes were not verified to see the same storage"
)

// addedNode returns a node change's data and the result of the one node it adds.
func addedNode(t *testing.T, body map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	data, _ := body["data"].(map[string]any)
	checks, _ := data["nodeBindings"].([]any)
	if len(checks) != 1 {
		t.Fatal("expected the result of one added node", body)
	}
	return data, checks[0].(map[string]any)
}

func localTask(t *testing.T, f *agentHTTPFixture, key, direction, root string, skipped bool, nodes ...task.Node) task.Parent {
	t.Helper()
	spec := task.Spec{Direction: direction, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", SharedCheckSkipped: skipped}
	local := execution.Endpoint{Kind: "LOCAL", Root: root}
	if direction == "LOCAL_TO_OSS" {
		spec.Source, spec.Destination, spec.SharedLocalSource = local, execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, true
	} else {
		spec.Source, spec.Destination = execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, local
	}
	for _, node := range nodes {
		node.LocalRoot = root
		spec.Nodes = append(spec.Nodes, node)
	}
	p, _, err := f.master.tasks.repo.Create(spec, key)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func databaseSchema(t *testing.T, f *agentHTTPFixture) int {
	t.Helper()
	var format struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := f.master.store.View(func(tx *state.Tx) error { _, err := tx.Get("metadata", "format", &format); return err }); err != nil {
		t.Fatal(err)
	}
	return format.SchemaVersion
}

// A task created with the shared directory check skipped no longer passes that
// choice on to the nodes added later: they are verified, unless the change
// itself skips the check with the acknowledgement that creation requires.
func TestAddedNodesAreVerifiedUnlessTheChangeSkipsTheCheck(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "writable", "writable", "writable", "legacy")
	labelAgent(t, f, agents[3].node, "Legacy worker")
	shared, other := t.TempDir(), t.TempDir()
	p := localTask(t, f, "recorded-skip", "LOCAL_TO_OSS", shared, true, agents[0].node, agents[1].node)
	joiner := agents[2].node

	status, body := nodeChange(t, f, p.ID, "", joinBody(joiner, other, dryRunChange))
	data, check := addedNode(t, body)
	if status != 200 || data["valid"] != false || check["code"] != "SHARED_STORAGE_PROBE_FAILED" || !strings.Contains(fmt.Sprint(check["message"]), "could not read the shared test file") {
		t.Fatal("a node on another directory passed because the task's creator skipped the check", status, body)
	}
	status, body = nodeChange(t, f, p.ID, "", joinBody(joiner, shared, dryRunChange))
	if data, check := addedNode(t, body); status != 200 || data["valid"] != true || check["code"] != "SHARED_STORAGE_VERIFIED" {
		t.Fatal("the added node was not verified", status, body)
	}

	// Skipping needs the acknowledgement creation asks for, with its words.
	for _, extra := range []map[string]any{{"skipSharedCheck": true}, {"skipSharedCheck": true, "dryRun": true}} {
		status, body := nodeChange(t, f, p.ID, "", joinBody(joiner, other, extra))
		if status != 409 || body["code"] != "SHARED_CHECK_SKIP_UNACKNOWLEDGED" || body["message"] != skipSharedCheckUnacknowledged {
			t.Fatal("a skip without acknowledgement was accepted", extra, status, body)
		}
	}
	if status, body := nodeChange(t, f, p.ID, "zh-CN", joinBody(joiner, other, map[string]any{"skipSharedCheck": true})); status != 409 || body["message"] != localizeMessage("zh-CN", skipSharedCheckUnacknowledged) {
		t.Fatal("the refusal is not translated", status, body)
	}
	status, body = nodeChange(t, f, p.ID, "", joinBody(joiner, other, acknowledgedSkip, dryRunChange))
	if data, check := addedNode(t, body); status != 200 || data["valid"] != true || check["code"] != "SHARED_STORAGE_CHECK_SKIPPED" || check["message"] != skipCheckedMessage {
		t.Fatal("an acknowledged skip was not reported as such", status, body)
	}
	if entries, err := os.ReadDir(other); err != nil || len(entries) != 0 {
		t.Fatal("a skipped check wrote into the added directory", entries, err)
	}

	// Every node added to such a task must be able to record the skipped
	// check (the runtime grants it), whether or not this change skips it.
	for _, extra := range []map[string]any{dryRunChange, {"dryRun": true, "skipSharedCheck": true, "skipSharedCheckAcknowledged": true}} {
		status, body := nodeChange(t, f, p.ID, "zh-CN", joinBody(agents[3].node, shared, extra))
		if status != 409 || body["code"] != "AGENT_UPGRADE_REQUIRED" || body["message"] != "请先升级执行节点 Legacy worker (127.0.0.1)，才能运行已跳过共享目录校验的任务" {
			t.Fatal("an Agent that cannot record a skipped check was accepted", extra, status, body)
		}
	}

	commit := joinBody(joiner, other, acknowledgedSkip, map[string]any{"requestId": "join-unchecked"})
	status, body = nodeChange(t, f, p.ID, "", commit)
	if data, check := addedNode(t, body); status != 200 || data["accepted"] != true || data["replayed"] != false || check["code"] != "SHARED_STORAGE_CHECK_SKIPPED" {
		t.Fatal("the acknowledged change was refused", status, body)
	}
	if status, body = nodeChange(t, f, p.ID, "", commit); status != 200 || body["data"].(map[string]any)["replayed"] != true {
		t.Fatal("the retry did not replay", status, body)
	}
	if current, err := f.master.tasks.repo.Get(p.ID); err != nil || !current.NodeJoining(joiner.ID) || !current.Spec.SharedCheckSkipped {
		t.Fatal("the change was not recorded", err)
	}
}

// A change that adds nodes with the check skipped to a task that verified its
// directory records the skip for the nodes it adds only: the current members
// are neither asked to record a skipped check nor required to be reachable,
// since the runtime keeps verifying them. Each added node must be able to
// record it. A change that adds no local path ignores the choice.
func TestAChangeThatSkipsTheCheckCoversOnlyTheNodesItAdds(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "writable", "writable", "writable", "legacy")
	labelAgent(t, f, agents[3].node, "Legacy worker")
	a, b, joiner, legacy := agents[0].node, agents[1].node, agents[2].node, agents[3].node
	shared, other := t.TempDir(), t.TempDir()

	mixed := localTask(t, f, "mixed", "LOCAL_TO_OSS", shared, false, a, legacy)
	status, body := nodeChange(t, f, mixed.ID, "en-US", joinBody(joiner, other, acknowledgedSkip, dryRunChange))
	if data, check := addedNode(t, body); status != 200 || data["valid"] != true || check["code"] != "SHARED_STORAGE_CHECK_SKIPPED" {
		t.Fatal("a member that keeps its verification blocked the skip", status, body)
	}

	// Takeover fences an offline member, which keeps its verification when
	// it returns: the skip does not need it.
	ghost := task.Node{ID: "ghost", EnrollmentID: "ghost-enrollment", Address: "ghost.example"}
	fenced := localTask(t, f, "fenced", "LOCAL_TO_OSS", shared, false, a, ghost)
	if _, err := f.master.tasks.repo.EnableFailover(fenced.ID, fenced.Generation, false); err != nil {
		t.Fatal(err)
	}
	status, body = nodeChange(t, f, fenced.ID, "", joinBody(joiner, other, acknowledgedSkip, dryRunChange))
	if data, check := addedNode(t, body); status != 200 || data["valid"] != true || check["code"] != "SHARED_STORAGE_CHECK_SKIPPED" {
		t.Fatal("an offline member blocked the skip of a task with takeover", status, body)
	}

	verified := localTask(t, f, "verified", "LOCAL_TO_OSS", shared, false, a, b)
	status, body = nodeChange(t, f, verified.ID, "en-US", joinBody(legacy, other, acknowledgedSkip, dryRunChange))
	if status != 409 || body["code"] != "AGENT_UPGRADE_REQUIRED" || body["message"] != "Upgrade Agent Legacy worker (127.0.0.1) to run a task whose shared directory check was skipped" {
		t.Fatal("an added node that cannot record the skip was accepted", status, body)
	}
	status, body = nodeChange(t, f, verified.ID, "", joinBody(joiner, other, acknowledgedSkip))
	data, check := addedNode(t, body)
	if status != 200 || data["accepted"] != true || check["code"] != "SHARED_STORAGE_CHECK_SKIPPED" || data["view"].(map[string]any)["sharedCheckSkipped"] != true {
		t.Fatal("the skip was not accepted and shown", status, body)
	}
	current, err := f.master.tasks.repo.Get(verified.ID)
	if err != nil || !current.Spec.SharedCheckSkipped || !current.NodeJoining(joiner.ID) {
		t.Fatal("the skip was not recorded on the task", err)
	}
	if !current.SharedCheckSkippedFor(joiner.ID) || current.SharedCheckSkippedFor(a.ID) || current.SharedCheckSkippedFor(b.ID) {
		t.Fatal("the skip does not cover exactly the added node", current.Spec.Nodes)
	}
	if schema := databaseSchema(t, f); schema < state.SchemaSharedCheckSkipped {
		t.Fatal("the record does not raise the database format", schema)
	}

	buckets := takeoverParent(t, f, a, b)
	removal := localTask(t, f, "removal", "LOCAL_TO_OSS", shared, false, a, b)
	for _, change := range []struct {
		id   int64
		body map[string]any
	}{
		{buckets.ID, map[string]any{"add": []string{joiner.Address}}},
		{removal.ID, map[string]any{"remove": []string{b.Address}}},
	} {
		for key, value := range acknowledgedSkip {
			change.body[key] = value
		}
		if status, body := nodeChange(t, f, change.id, "", change.body); status != 200 || body["data"].(map[string]any)["accepted"] != true {
			t.Fatal("a change without an added local path was refused", status, body)
		}
		if current, err := f.master.tasks.repo.Get(change.id); err != nil || current.Spec.SharedCheckSkipped {
			t.Fatal("a change without an added local path recorded the skip", err)
		}
	}
}

// A node added with the check skipped joins without a check, while the
// runtime keeps verifying the task's other nodes against the original shared
// directory: a member that loses its mount is refused, not granted the skip.
func TestASkippedJoinKeepsVerifyingTheOtherNodes(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "writable", "writable")
	a, joiner := agents[0].node, agents[1].node
	var lost atomic.Bool
	b := overrideTestAgent(t, f, 3, func(command agenttransport.Command) (agenttransport.Response, bool) {
		if lost.Load() && command.Operation == "shared_probe_verify" {
			return agentReply(404, map[string]string{"code": "operation_failed", "message": "the shared directory is not mounted"})
		}
		return agenttransport.Response{}, false
	})
	labelAgent(t, f, b, "Member B")
	shared, other := t.TempDir(), t.TempDir()
	p := localTask(t, f, "skipped-join", "LOCAL_TO_OSS", shared, false, a, b)
	if status, body := nodeChange(t, f, p.ID, "", joinBody(joiner, other, acknowledgedSkip)); status != 200 {
		t.Fatal(status, body)
	}
	runtime := f.master.tasks
	runtime.leases.ttl = 30 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	everyNode := map[string]bool{a.ID: true, b.ID: true, joiner.ID: true}
	p, err := runtime.reconcileFailover(ctx, p, everyNode)
	if err != nil {
		t.Fatal(err)
	}
	if p, err = runtime.repo.Get(p.ID); err != nil || !p.Failover.Enabled || p.NodeJoining(joiner.ID) || p.NodeRetired(joiner.ID) {
		t.Fatal("the node added without a check did not join", err, p.Membership)
	}
	if !agents[1].service.Shared.Verified(p.ID, p.Generation, other) || !agents[0].service.Shared.Verified(p.ID, p.Generation, shared) {
		t.Fatal("a node did not get its proof")
	}
	if proof := storedProof(t, f, p.ID); !proof.Ready || proof.ReadOnly {
		t.Fatal("the members did not set up the marker of the shared directory", proof)
	}
	if entries, err := os.ReadDir(other); err != nil || len(entries) != 0 {
		t.Fatal("the node added without a check was asked to read the marker", entries, err)
	}

	lost.Store(true)
	_, err = runtime.reconcileFailover(ctx, p, everyNode)
	if err == nil || !strings.Contains(err.Error(), "Agent Member B (127.0.0.1) cannot verify the original shared directory") {
		t.Fatal("a member that lost its mount was not refused", err)
	}
}

// Adding a node to a download probes that the node can write its destination,
// as creation does: before the shared check and also when the check is
// skipped, with creation's code, message and fields. Path problems use
// creation's words too.
func TestAddingDownloadNodesProbesWritesAsCreationDoes(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "writable", "writable", "writable")
	joiner := agents[2].node
	var lockedProbes atomic.Int64
	locked := overrideTestAgent(t, f, 4, func(command agenttransport.Command) (agenttransport.Response, bool) {
		if command.Operation != "local_write_probe" {
			return agenttransport.Response{}, false
		}
		lockedProbes.Add(1)
		var request agentservice.PathRequest
		_ = json.Unmarshal(command.Payload, &request)
		uid := 4242
		return agentReply(200, agentservice.WriteProbe{Path: request.Path, ProbeDirectory: request.Path, ErrorCode: agentservice.WriteProbePermissionDenied, Error: "permission denied", EffectiveUID: &uid})
	})
	older := overrideTestAgent(t, f, 5, func(command agenttransport.Command) (agenttransport.Response, bool) {
		if command.Operation == "local_write_probe" {
			return agentReply(404, map[string]string{"code": "unknown_operation"})
		}
		return agenttransport.Response{}, false
	})
	odd := overrideTestAgent(t, f, 6, func(command agenttransport.Command) (agenttransport.Response, bool) {
		if command.Operation == "local_path" {
			return agentReply(200, map[string]string{"pathType": "SOCKET"})
		}
		return agenttransport.Response{}, false
	})
	shared := t.TempDir()
	download := localTask(t, f, "download", "OSS_TO_LOCAL", shared, false, agents[0].node, agents[1].node)

	status, body := nodeChange(t, f, download.ID, "", joinBody(joiner, shared, dryRunChange))
	data, check := addedNode(t, body)
	if status != 200 || data["valid"] != true || check["code"] != "SHARED_STORAGE_VERIFIED" || check["writeProbeDirectory"] != shared || check["freeBytes"] == nil {
		t.Fatal("a writable destination did not report the write probe", status, body)
	}
	if runtime.GOOS != "windows" && check["effectiveUid"] != float64(os.Geteuid()) {
		t.Fatal("the Agent's user is missing", check)
	}
	for _, extra := range []map[string]any{dryRunChange, {"dryRun": true, "skipSharedCheck": true, "skipSharedCheckAcknowledged": true}} {
		status, body := nodeChange(t, f, download.ID, "", joinBody(locked, shared, extra))
		data, check := addedNode(t, body)
		message := fmt.Sprint(check["message"])
		if status != 200 || data["valid"] != false || check["code"] != "LOCAL_PATH_NOT_WRITABLE" || check["effectiveUid"] != float64(4242) || check["writeProbeDirectory"] != shared || !strings.Contains(message, "UID 4242") || !strings.Contains(message, "chown 4242") {
			t.Fatal("an unwritable destination was not refused as at creation", extra, status, body)
		}
	}
	status, body = nodeChange(t, f, download.ID, "", joinBody(older, shared, dryRunChange))
	if data, check := addedNode(t, body); status != 200 || data["valid"] != true || check["code"] != "SHARED_STORAGE_VERIFIED" || check["freeBytes"] != nil {
		t.Fatal("an Agent from before the write probe was not accepted as at creation", status, body)
	}

	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(shared, "missing")
	upload := localTask(t, f, "upload", "LOCAL_TO_OSS", shared, false, agents[0].node, agents[1].node)
	for _, test := range []struct {
		task                    task.Parent
		node                    task.Node
		path, language          string
		code, message, pathType string
	}{
		{download, joiner, file, "en-US", "", "Download destination must be a directory", "FILE"},
		{download, joiner, missing, "zh-CN", "SHARED_DIRECTORY_REQUIRED", "共享目标目录不存在", "MISSING"},
		{download, odd, shared, "en-US", "", "Node returned an invalid path type", "SOCKET"},
		{upload, joiner, missing, "en-US", "", "Source path does not exist", "MISSING"},
		{upload, joiner, file, "en-US", "SHARED_DIRECTORY_REQUIRED", "Create the shared directory on all selected nodes before preflight", "FILE"},
	} {
		status, body := nodeChange(t, f, test.task.ID, test.language, joinBody(test.node, test.path, dryRunChange))
		data, check := addedNode(t, body)
		code, _ := check["code"].(string)
		if status != 200 || data["valid"] != false || code != test.code || check["message"] != test.message || check["pathType"] != test.pathType {
			t.Fatal("the added path was not reported with creation's words", test.path, status, body)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("the preflight created the destination", err)
	}
	// A join never creates the destination: a missing one is reported as
	// missing, not as a directory above it that the Agent cannot write.
	probes := lockedProbes.Load()
	for _, extra := range []map[string]any{dryRunChange, {"dryRun": true, "skipSharedCheck": true, "skipSharedCheckAcknowledged": true}} {
		status, body := nodeChange(t, f, download.ID, "zh-CN", joinBody(locked, missing, extra))
		data, check := addedNode(t, body)
		if status != 200 || data["valid"] != false || check["code"] != "SHARED_DIRECTORY_REQUIRED" || check["message"] != "共享目标目录不存在" || check["writeProbeDirectory"] != nil {
			t.Fatal("a missing destination was not reported as missing", extra, status, body)
		}
	}
	if lockedProbes.Load() != probes {
		t.Fatal("a missing destination was probed for writes")
	}
}

// A capabilities query that fails is reported as such, not as an offline
// Agent: when adding nodes and when creating a task.
func TestFailedCapabilityQueriesAreNotReportedAsOffline(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "writable", "writable")
	busy := overrideTestAgent(t, f, 3, func(command agenttransport.Command) (agenttransport.Response, bool) {
		if command.Operation == "capabilities" {
			return agentReply(503, map[string]string{"code": "busy", "message": "the Agent is busy"})
		}
		return agenttransport.Response{}, false
	})
	labelAgent(t, f, busy, "Busy worker")
	p := takeoverParent(t, f, agents[0].node, agents[1].node)
	for _, test := range []struct{ language, want string }{
		{"en-US", "Cannot read the version information of Agent Busy worker (127.0.0.1): the Agent is busy"},
		{"zh-CN", "无法读取执行节点 Busy worker (127.0.0.1) 的版本信息：the Agent is busy"},
	} {
		status, body := nodeChange(t, f, p.ID, test.language, map[string]any{"add": []string{busy.Address}})
		if status != 409 || body["code"] != "AGENT_VERSION_UNAVAILABLE" || body["message"] != test.want {
			t.Fatal("a failed capabilities query was not reported as such", status, body)
		}
	}
	shared := t.TempDir()
	row := taskCreateRequest{Direction: "OSS_TO_LOCAL", ConflictPolicy: execution.ConflictOverwriteChanged,
		SourceEndpoint:      &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"},
		DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{agents[0].node.Address, shared}, {busy.Address, shared}}}}
	status, _, response, err := localizedRequest(f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, "zh-CN", []taskCreateRequest{row})
	if err != nil || status != 200 || fmt.Sprint(response["data"].(map[string]any)["validationErrors"]) != "[1: 无法读取执行节点 Busy worker (127.0.0.1) 的版本信息：the Agent is busy]" {
		t.Fatal("creation did not report the failed capabilities query", status, response, err)
	}
}

// The commit of a node change re-checks, under the lock that removing an Agent
// takes, that each added Agent is still online under the enrollment that its
// preflight used, as creating a task does.
func TestNodeChangeCommitRechecksAddedAgentsUnderTheTopologyLock(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "writable", "writable", "writable")
	a, joiner, spare := agents[0].node, agents[1].node, agents[2].node
	labelAgent(t, f, joiner, "Joining worker")
	var target atomic.Pointer[string]
	removed := make(chan int, 1)
	remove := removeOnCapabilities(f, &target, removed)
	// In an OSS-to-OSS change, the last member's capabilities answer is the
	// last Agent call before the commit.
	last := overrideTestAgent(t, f, 4, remove)
	p := takeoverParent(t, f, a, last)
	target.Store(&joiner.Address)
	status, body := nodeChange(t, f, p.ID, "zh-CN", map[string]any{"add": []string{joiner.Address}})
	select {
	case code := <-removed:
		if code != http.StatusOK {
			t.Fatal("the Agent could not be removed while the change ran its preflight", code)
		}
	default:
		t.Fatal("the added Agent was not removed during the change")
	}
	if status != 409 || body["code"] != "AGENT_OFFLINE" || body["message"] != "执行节点 Joining worker (127.0.0.1) 未在线" {
		t.Fatal("a removed Agent was added", status, body)
	}
	if current, err := f.master.tasks.repo.Get(p.ID); err != nil || len(current.Spec.Nodes) != 2 || current.MembershipPending() {
		t.Fatal("the refused change was recorded", err)
	}
	// A committed join makes the Agent a member that cannot be removed.
	if status, body := nodeChange(t, f, p.ID, "", map[string]any{"add": []string{spare.Address}}); status != 200 || body["data"].(map[string]any)["accepted"] != true {
		t.Fatal(status, body)
	}
	if status, _ := request(t, f.http.Client(), "DELETE", f.http.URL+"/api/master/nodes/"+spare.Address, f.admin, nil); status != http.StatusConflict {
		t.Fatal("a joining Agent was removed", status)
	}
}
