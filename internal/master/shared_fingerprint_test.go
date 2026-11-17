package master

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// readOnlyAgent is a real Agent whose shared root refuses writes, as a
// read-only NFS mount or an account without write permission would. Root
// chmod cannot simulate this when tests run as root, so the write is refused
// at the command boundary with the error the Agent would send.
type readOnlyAgent struct {
	*agentservice.Service
	// write is how a probe write fails: "read-only" (current Agents),
	// "legacy" (an Agent that predates fingerprints and their codes) or
	// "io" (a failure that is not about permissions).
	write        string
	fingerprints *atomic.Int64
}

func (a readOnlyAgent) Execute(ctx context.Context, command agenttransport.Command) (agenttransport.Response, error) {
	refuse := func() (agenttransport.Response, error) {
		switch a.write {
		case "legacy":
			return agenttransport.Response{Status: 500, Payload: json.RawMessage(`{"code":"operation_failed","message":"open /shared/.oss-transfer-shared-probe-x.probe: read-only file system"}`)}, nil
		case "io":
			return agenttransport.Response{Status: 500, Payload: json.RawMessage(`{"code":"operation_failed","message":"open /shared/.oss-transfer-shared-probe-x.probe: input/output error"}`)}, nil
		}
		return agenttransport.Response{Status: 403, Payload: json.RawMessage(`{"code":"read_only_storage","message":"open /shared/.oss-transfer-shared-probe-x.probe: permission denied"}`)}, nil
	}
	switch command.Operation {
	case "capabilities":
		// The Agent binary answers this outside the service; the runtime
		// needs the boot identity to tie proofs to this process.
		capabilities := a.LeaseCapabilities()
		capabilities["bootId"] = capabilities["leaseBootId"]
		raw, _ := json.Marshal(capabilities)
		return agenttransport.Response{Status: 200, Payload: raw}, nil
	case "preflight_probe":
		var request agentservice.PreflightProbeRequest
		if err := json.Unmarshal(command.Payload, &request); err == nil && request.Action == "write" {
			return refuse()
		}
	case "shared_probe_ensure":
		return refuse()
	case "shared_fingerprint_sample", "shared_fingerprint_verify", "shared_proof_skip":
		a.fingerprints.Add(1)
		if a.write == "legacy" {
			return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
		}
	}
	return a.Service.Execute(ctx, command)
}

type sharedTestAgent struct {
	node    task.Node
	service *agentservice.Service
}

func readOnlyTestAgents(t *testing.T, f *agentHTTPFixture, write string, calls *atomic.Int64) []sharedTestAgent {
	t.Helper()
	agents := make([]sharedTestAgent, 2)
	for i := range agents {
		agents[i].node = connectTestAgent(t, f, i+1, func(service *agentservice.Service) agenttransport.Executor {
			agents[i].service = service
			return readOnlyAgent{Service: service, write: write, fingerprints: calls}
		})
	}
	return agents
}

func sharedTree(t *testing.T, root string, variant string) {
	t.Helper()
	for i := range 24 {
		name := filepath.Join(root, fmt.Sprintf("part-%d", i%4), fmt.Sprintf("object-%d.bin", i))
		if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(fmt.Sprintf("object %d %s", i, variant)), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func treeEntries(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	if err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		names = append(names, path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return names
}

func validateRows(t *testing.T, f *agentHTTPFixture, row taskCreateRequest) map[string]any {
	t.Helper()
	code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers/validate", f.admin, []taskCreateRequest{row})
	if code != 200 {
		t.Fatal(code, response)
	}
	return response["data"].(map[string]any)
}

func uploadRow(bindings ...taskBinding) taskCreateRequest {
	return taskCreateRequest{Direction: "LOCAL_TO_OSS",
		SourceEndpoint:      &taskEndpoint{Type: "LOCAL", NodeBindings: bindings},
		DestinationEndpoint: &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "destination"}}
}

func bindingCodes(data map[string]any) []string {
	var codes []string
	for _, raw := range data["nodeBindings"].([]any) {
		codes = append(codes, fmt.Sprint(raw.(map[string]any)["code"]))
	}
	return codes
}

func TestReadOnlySharedSourcePreflightComparesSampledFiles(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := readOnlyTestAgents(t, f, "read-only", &calls)
	a, b := agents[0].node, agents[1].node
	shared, copied, edited := t.TempDir(), t.TempDir(), t.TempDir()
	sharedTree(t, shared, "v1")
	sharedTree(t, edited, "v2") // Same paths, every file different.
	before := treeEntries(t, shared)

	data := validateRows(t, f, uploadRow(taskBinding{a.Address, shared}, taskBinding{b.Address, shared}))
	if data["valid"] != true || calls.Load() == 0 {
		t.Fatal("read-only shared source did not fall back to fingerprints", data)
	}
	for _, code := range bindingCodes(data) {
		if code != "SHARED_STORAGE_FINGERPRINT_VERIFIED" {
			t.Fatal("fingerprint verification was not reported", data)
		}
	}
	if after := treeEntries(t, shared); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatal("read-only verification changed the source", after)
	}

	data = validateRows(t, f, uploadRow(taskBinding{a.Address, shared}, taskBinding{b.Address, edited}))
	encoded, _ := json.Marshal(data)
	if data["valid"] != false || !strings.Contains(string(encoded), "does not see the same files in the shared directory") {
		t.Fatal("different directories passed fingerprint verification", string(encoded))
	}

	// Nothing to transfer on either node is not a reason to refuse.
	sharedTree(t, copied, "v1")
	empty, alsoEmpty := t.TempDir(), t.TempDir()
	if data := validateRows(t, f, uploadRow(taskBinding{a.Address, empty}, taskBinding{b.Address, alsoEmpty})); data["valid"] != true {
		t.Fatal("empty read-only roots were refused", data)
	}
	if data := validateRows(t, f, uploadRow(taskBinding{a.Address, empty}, taskBinding{b.Address, copied})); data["valid"] != false {
		t.Fatal("an empty root matched a populated one", data)
	}

	// A download destination must be writable; no fallback applies.
	calls.Store(0)
	data = validateRows(t, f, taskCreateRequest{Direction: "OSS_TO_LOCAL",
		SourceEndpoint:      &taskEndpoint{Type: "OSS", S3Alias: "qa", Bucket: "source"},
		DestinationEndpoint: &taskEndpoint{Type: "LOCAL", NodeBindings: []taskBinding{{a.Address, shared}, {b.Address, shared}}}})
	if data["valid"] != false || calls.Load() != 0 {
		t.Fatal("a read-only download destination used the fingerprint fallback", data, calls.Load())
	}
}

func TestSharedPreflightFallsBackOnlyForPermissionErrors(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := readOnlyTestAgents(t, f, "io", &calls)
	shared := t.TempDir()
	sharedTree(t, shared, "v1")
	data := validateRows(t, f, uploadRow(taskBinding{agents[0].node.Address, shared}, taskBinding{agents[1].node.Address, shared}))
	encoded, _ := json.Marshal(data)
	if data["valid"] != false || calls.Load() != 0 || !strings.Contains(string(encoded), "needs one writable node") {
		t.Fatal("an I/O error was treated as a read-only source", string(encoded), calls.Load())
	}
	for _, code := range bindingCodes(data) {
		if code != "SHARED_STORAGE_PROBE_FAILED" {
			t.Fatal("unexpected binding code", data)
		}
	}
}

func TestReadOnlySharedPreflightAsksToUpgradeOldAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := readOnlyTestAgents(t, f, "legacy", &calls)
	if _, err := f.master.agents.registry.UpdateLabel(agents[0].node.ID, "Legacy worker"); err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	sharedTree(t, shared, "v1")
	data := validateRows(t, f, uploadRow(taskBinding{agents[0].node.Address, shared}, taskBinding{agents[1].node.Address, shared}))
	encoded, _ := json.Marshal(data)
	if data["valid"] != false || !strings.Contains(string(encoded), "Upgrade Agent Legacy worker (127.0.0.1) to verify a read-only shared directory") {
		t.Fatal("an old Agent did not produce an upgrade request", string(encoded))
	}
	if localizeMessage("zh-CN", "Upgrade Agent %s to verify a read-only shared directory") == "Upgrade Agent %s to verify a read-only shared directory" {
		t.Fatal("upgrade message is not translated")
	}
}

func TestReadOnlyWriteFailureClassification(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{&AgentError{Status: 403, Code: agentservice.ReadOnlyStorageCode, Message: "anything"}, true},
		{&AgentError{Status: 500, Message: "open /x: read-only file system"}, true},
		{&AgentError{Status: 500, Message: "open /x: Permission denied"}, true},
		{&AgentError{Status: 500, Message: "open /x: Access is denied."}, true},
		{&AgentError{Status: 500, Message: "open /x: input/output error"}, false},
		{&AgentError{Status: 404, Message: "unknown_operation"}, false},
		{fmt.Errorf("transport: permission denied"), false},
	} {
		if got := readOnlyWriteFailure(test.err); got != test.want {
			t.Fatalf("%v: got %t", test.err, got)
		}
	}
}

func sharedSourceParent(t *testing.T, f *agentHTTPFixture, roots ...string) (task.Parent, []sharedTestAgent) {
	t.Helper()
	var calls atomic.Int64
	agents := readOnlyTestAgents(t, f, "read-only", &calls)
	for i := range agents {
		agents[i].node.LocalRoot = roots[i]
	}
	p := takeoverParent(t, f, agents[0].node, agents[1].node)
	p.Spec.Direction = "LOCAL_TO_OSS"
	p.Spec.Source = execution.Endpoint{Kind: "LOCAL", Root: roots[0]}
	p.Spec.SharedLocalSource = true
	return p, agents
}

func TestReadOnlySharedSourceRuntimeGrantsAgentProofs(t *testing.T) {
	f := newAgentHTTPFixture(t)
	shared := t.TempDir()
	sharedTree(t, shared, "v1")
	p, agents := sharedSourceParent(t, f, shared, shared)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := f.master.tasks.ensureSharedSource(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if !agent.service.Shared.Verified(p.ID, p.Generation, shared) {
			t.Fatal("a read-only source did not grant the Agent's shared proof")
		}
	}

	f2 := newAgentHTTPFixture(t)
	other := t.TempDir()
	sharedTree(t, other, "v2")
	p, agents = sharedSourceParent(t, f2, shared, other)
	if err := f2.master.tasks.ensureSharedSource(ctx, p); err == nil || !strings.Contains(err.Error(), "does not see the same files") {
		t.Fatal("different read-only directories passed the runtime check", err)
	}
	if agents[1].service.Shared.Verified(p.ID, p.Generation, other) {
		t.Fatal("a mismatched Agent was granted a shared proof")
	}
}

func TestReadOnlyTakeoverComparesTheOriginalSample(t *testing.T) {
	f := newAgentHTTPFixture(t)
	shared := t.TempDir()
	sharedTree(t, shared, "v1")
	p, agents := sharedSourceParent(t, f, shared, shared)
	a, b := agents[0].node, agents[1].node
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := f.master.tasks.verifyTaskStorage(ctx, p, p.Spec.Nodes, true); err != nil {
		t.Fatal(err)
	}
	if entries := treeEntries(t, shared); len(entries) != 29 { // root, 4 directories, 24 files
		t.Fatal("takeover setup wrote into a read-only source", entries)
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{b}, false); err != nil {
		t.Fatal("the original directory failed takeover verification", err)
	}
	wrong := t.TempDir()
	sharedTree(t, wrong, "v2")
	moved := b
	moved.LocalRoot = wrong
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{moved}, false); err == nil {
		t.Fatal("takeover accepted a different directory")
	}

	// Edited source files: one node alone cannot tell an edit from another
	// directory, but two nodes that agree on a fresh sample replace it.
	sharedTree(t, shared, "edited, longer")
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{b}, false); err == nil {
		t.Fatal("a single node replaced the sample")
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{a, b}, false); err != nil {
		t.Fatal("two agreeing nodes could not replace an edited sample", err)
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{b}, false); err != nil {
		t.Fatal("the replaced sample was not kept", err)
	}
	// Missing files look like an unmounted share and are never accepted.
	if err := os.RemoveAll(filepath.Join(shared, "part-0")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(shared, "part-1")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(shared, "part-2")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(shared, "part-3")); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{a, b}, false); err == nil {
		t.Fatal("missing sampled files were accepted")
	}
}
