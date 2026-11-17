package master

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agentservice"
	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// readOnlyAgent is a real Agent whose shared root refuses writes, as a
// read-only NFS mount or an account without write permission would. Root
// chmod cannot simulate this when tests run as root, so the write is refused
// at the command boundary with the error the Agent would send.
type readOnlyAgent struct {
	*agentservice.Service
	// write is how a probe write fails: "read-only" (current Agents),
	// "legacy" (an Agent that predates fingerprints and their codes), "io" (a
	// failure that is not about permissions), "writable" (writes succeed) or
	// "verify-io" (writes refused, comparisons fail with an I/O error).
	write string
	// os is the operating system the Agent reports; empty means this one.
	os           string
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
		return agenttransport.Response{Status: 403, Payload: json.RawMessage(`{"code":"storage_not_writable","message":"open /shared/.oss-transfer-shared-probe-x.probe: permission denied"}`)}, nil
	}
	switch command.Operation {
	case "capabilities":
		// The Agent binary answers this outside the service; the runtime
		// needs the boot identity to tie proofs to this process.
		capabilities := a.LeaseCapabilities()
		capabilities["bootId"] = capabilities["leaseBootId"]
		capabilities["os"] = runtime.GOOS
		if a.os != "" {
			capabilities["os"] = a.os
		}
		if a.write == "legacy" {
			delete(capabilities, "sharedProofProtocol")
		}
		raw, _ := json.Marshal(capabilities)
		return agenttransport.Response{Status: 200, Payload: raw}, nil
	case "preflight_probe":
		var request agentservice.PreflightProbeRequest
		if err := json.Unmarshal(command.Payload, &request); err == nil && request.Action == "write" && a.write != "writable" {
			return refuse()
		}
	case "shared_probe_ensure":
		if a.write != "writable" {
			return refuse()
		}
	case "shared_fingerprint_sample", "shared_fingerprint_verify", "shared_proof_skip":
		a.fingerprints.Add(1)
		if a.write == "legacy" {
			return agenttransport.Response{Status: 404, Payload: json.RawMessage(`{"code":"unknown_operation"}`)}, nil
		}
		if a.write == "verify-io" && command.Operation == "shared_fingerprint_verify" {
			return agenttransport.Response{Status: 500, Payload: json.RawMessage(`{"code":"operation_failed","message":"open /shared: input/output error"}`)}, nil
		}
	}
	return a.Service.Execute(ctx, command)
}

type sharedTestAgent struct {
	node    task.Node
	service *agentservice.Service
}

// sharedTestAgents connects one Agent per mode; see readOnlyAgent.write.
func sharedTestAgents(t *testing.T, f *agentHTTPFixture, calls *atomic.Int64, modes ...string) []sharedTestAgent {
	t.Helper()
	agents := make([]sharedTestAgent, len(modes))
	for i := range agents {
		agents[i].node = connectTestAgent(t, f, i+1, func(service *agentservice.Service) agenttransport.Executor {
			agents[i].service = service
			return readOnlyAgent{Service: service, write: modes[i], fingerprints: calls}
		})
	}
	return agents
}

func readOnlyTestAgents(t *testing.T, f *agentHTTPFixture, write string, calls *atomic.Int64) []sharedTestAgent {
	t.Helper()
	return sharedTestAgents(t, f, calls, write, write)
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

// ensureStorage creates the storage connection that test rows name ("qa"),
// unless the test did: the preflight, like creation, refuses a row whose
// connection does not exist.
func ensureStorage(t *testing.T, f *agentHTTPFixture) {
	t.Helper()
	snapshot, err := storageconfig.NewMaster(f.db).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range snapshot.Connections {
		if config.Alias == "qa" {
			return
		}
	}
	createComparisonStorage(t, f)
}

// validateRows runs the preflight of one row whose storage connection exists,
// so that its node bindings decide.
func validateRows(t *testing.T, f *agentHTTPFixture, row taskCreateRequest) map[string]any {
	t.Helper()
	ensureStorage(t, f)
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

func bindingMessages(data map[string]any) []string {
	var messages []string
	for _, raw := range data["nodeBindings"].([]any) {
		messages = append(messages, fmt.Sprint(raw.(map[string]any)["message"]))
	}
	return messages
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
		t.Fatal("an unwritable shared source did not fall back to fingerprints", data)
	}
	for i, code := range bindingCodes(data) {
		// The message says exactly what was compared, and its limit.
		if message := bindingMessages(data)[i]; code != "SHARED_STORAGE_FINGERPRINT_VERIFIED" || !strings.Contains(message, "16 files have the same size and the same first and last 64 KiB") || !strings.Contains(message, "separate copies holding identical files also pass") {
			t.Fatal("fingerprint verification was not reported as what it is", data)
		}
	}
	if after := treeEntries(t, shared); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatal("read-only verification changed the source", after)
	}

	data = validateRows(t, f, uploadRow(taskBinding{a.Address, shared}, taskBinding{b.Address, edited}))
	encoded, _ := json.Marshal(data)
	if data["valid"] != false || !strings.Contains(string(encoded), "read different data for sampled file") {
		t.Fatal("different directories passed fingerprint verification", string(encoded))
	}
	// Both bindings carry the explanation: either node may have the wrong mount.
	for _, message := range bindingMessages(data) {
		if !strings.Contains(message, "read different data") {
			t.Fatal("a comparison failure is not explained on both bindings", data)
		}
	}

	// A complete listing with no file on either node: nothing to transfer.
	sharedTree(t, copied, "v1")
	empty, alsoEmpty := t.TempDir(), t.TempDir()
	if data := validateRows(t, f, uploadRow(taskBinding{a.Address, empty}, taskBinding{b.Address, alsoEmpty})); data["valid"] != true || !strings.Contains(bindingMessages(data)[0], "complete listing of the shared directory found no file") {
		t.Fatal("empty roots were refused or reported as compared", data)
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
	for _, key := range []string{
		"No node could write the test file (no write permission, read-only or full storage), so sampled files were compared instead: %d files have the same size and the same first and last 64 KiB on every node. This is weaker than the test file: separate copies holding identical files also pass.",
		"No node could write the test file (no write permission, read-only or full storage), and a complete listing of the shared directory found no file on any node, so there was nothing to compare.",
		"Nodes %s and %s read different data for sampled file %q (its size or its first or last 64 KiB differ)",
	} {
		if localizeMessage("zh-CN", key) == key || localizeMessage("ja-JP", key) == key {
			t.Fatal("not translated:", key)
		}
	}
}

// Files under ISO-timestamp directories are transferred by Unix Agents, so
// the comparison must use them: an empty or unmounted directory on the other
// node must never pass, whichever node samples.
func TestReadOnlyPreflightRefusesAnEmptyNodeAgainstColonNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file systems cannot hold ':' in names")
	}
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := readOnlyTestAgents(t, f, "read-only", &calls)
	a, b := agents[0].node, agents[1].node
	backups, empty := t.TempDir(), t.TempDir()
	writeFiles(t, backups, map[string]string{"2026-11-15T02:00:00/db.sql.gz": "monday", "2026-11-16T02:00:00/db.sql.gz": "tuesday"})

	data := validateRows(t, f, uploadRow(taskBinding{a.Address, backups}, taskBinding{b.Address, empty}))
	if data["valid"] != false || !strings.Contains(bindingMessages(data)[1], "is missing on node") {
		t.Fatal("an empty node matched files under colon names", data)
	}
	data = validateRows(t, f, uploadRow(taskBinding{b.Address, empty}, taskBinding{a.Address, backups}))
	messages := bindingMessages(data)
	if data["valid"] != false || !strings.Contains(messages[0], "found no file in the shared directory") || !strings.Contains(messages[0], "not mounted on node") || messages[0] != messages[1] {
		t.Fatal("an empty sampling node was not named as the likely unmounted one", data)
	}
	// With a Windows Agent taking part the names cannot be compared at all,
	// which is not the same as an empty directory either.
	windows := connectTestAgent(t, f, 3, func(service *agentservice.Service) agenttransport.Executor {
		return readOnlyAgent{Service: service, write: "read-only", os: "windows", fingerprints: &calls}
	})
	data = validateRows(t, f, uploadRow(taskBinding{a.Address, backups}, taskBinding{windows.Address, empty}))
	if data["valid"] != false || !strings.Contains(bindingMessages(data)[0], "names that cannot be compared") {
		t.Fatal("names a Windows Agent cannot open were sampled or treated as an empty directory", data)
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// A comparison that could not run is not evidence of different storage.
func TestReadOnlyPreflightReportsAComparisonThatCouldNotRun(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "read-only", "verify-io")
	shared := t.TempDir()
	sharedTree(t, shared, "v1")
	data := validateRows(t, f, uploadRow(taskBinding{agents[0].node.Address, shared}, taskBinding{agents[1].node.Address, shared}))
	encoded, _ := json.Marshal(data)
	if data["valid"] != false || !strings.Contains(bindingMessages(data)[1], "Could not complete the comparison on node") || !strings.Contains(bindingMessages(data)[1], "input/output error") || strings.Contains(string(encoded), "different data") {
		t.Fatal("an I/O error was reported as different storage", string(encoded))
	}
}

func TestSharedPreflightFallsBackOnlyWhenNoNodeCanWrite(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := readOnlyTestAgents(t, f, "io", &calls)
	shared := t.TempDir()
	sharedTree(t, shared, "v1")
	data := validateRows(t, f, uploadRow(taskBinding{agents[0].node.Address, shared}, taskBinding{agents[1].node.Address, shared}))
	encoded, _ := json.Marshal(data)
	if data["valid"] != false || calls.Load() != 0 || !strings.Contains(string(encoded), "not every failure was a missing write permission") || !strings.Contains(string(encoded), "input/output error") {
		t.Fatal("an I/O error was treated as an unwritable source", string(encoded), calls.Load())
	}
	for _, code := range bindingCodes(data) {
		if code != "SHARED_STORAGE_PROBE_FAILED" {
			t.Fatal("unexpected binding code", data)
		}
	}

	// One node that can write keeps the test file check: the read-only node
	// only reads, and the writer becomes the first node the runtime uses.
	g := newAgentHTTPFixture(t)
	calls.Store(0)
	mixed := sharedTestAgents(t, g, &calls, "read-only", "writable")
	row := uploadRow(taskBinding{mixed[0].node.Address, shared}, taskBinding{mixed[1].node.Address, shared})
	data = validateRows(t, g, row)
	for _, code := range bindingCodes(data) {
		if data["valid"] != true || code != "SHARED_STORAGE_VERIFIED" || calls.Load() != 0 {
			t.Fatal("a writable node did not keep the test file check", data, calls.Load())
		}
	}
	spec, _, err := g.master.resolveTaskSpec(context.Background(), row, "local-transfers")
	if err != nil || spec.Nodes[0].ID != mixed[1].node.ID {
		t.Fatal("the writable node was not placed first", spec.Nodes, err)
	}
}

func TestSharedPreflightAsksToUpgradeOldAgents(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := readOnlyTestAgents(t, f, "legacy", &calls)
	if _, err := f.master.agents.registry.UpdateLabel(agents[0].node.ID, "Legacy worker"); err != nil {
		t.Fatal(err)
	}
	shared := t.TempDir()
	sharedTree(t, shared, "v1")
	row := uploadRow(taskBinding{agents[0].node.Address, shared}, taskBinding{agents[1].node.Address, shared})
	data := validateRows(t, f, row)
	encoded, _ := json.Marshal(data)
	if data["valid"] != false || !strings.Contains(string(encoded), "Upgrade Agent Legacy worker (127.0.0.1) to compare files in a shared directory that no node can write") {
		t.Fatal("an old Agent did not produce an upgrade request", string(encoded))
	}
	// A skipped check needs shared_proof_skip on every Agent: without it the
	// task could be created but never start.
	row.SkipSharedCheck, row.SkipSharedCheckAcknowledged = true, true
	data = validateRows(t, f, row)
	encoded, _ = json.Marshal(data)
	if data["valid"] != false || !strings.Contains(string(encoded), "Upgrade Agent Legacy worker (127.0.0.1) to run a task whose shared directory check was skipped") || !strings.Contains(string(encoded), "AGENT_UPGRADE_REQUIRED") {
		t.Fatal("a skipped check passed the precheck on Agents that cannot run it", string(encoded))
	}
	code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers", f.admin, []taskCreateRequest{row})
	if created := response["data"].(map[string]any); code != 200 || created["acceptedCount"] != float64(0) {
		t.Fatal("a task that old Agents cannot start was created", code, response)
	}
	for _, key := range []string{"Upgrade Agent %s to compare files in a shared directory that no node can write", "Upgrade Agent %s to run a task whose shared directory check was skipped"} {
		if localizeMessage("zh-CN", key) == key {
			t.Fatal("upgrade message is not translated:", key)
		}
	}
}

func TestUnwritableWriteFailureClassification(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{&AgentError{Status: 403, Code: agentservice.UnwritableStorageCode, Message: "anything"}, true},
		{&AgentError{Status: 500, Message: "open /x: read-only file system"}, true},
		{&AgentError{Status: 500, Message: "open /x: Permission denied"}, true},
		{&AgentError{Status: 500, Message: "open /x: Access is denied."}, true},
		{&AgentError{Status: 500, Message: `open D:\x: The media is write protected.`}, true},
		{&AgentError{Status: 500, Message: "write /x: no space left on device"}, true},
		{&AgentError{Status: 500, Message: "write /x: disk quota exceeded"}, true},
		{&AgentError{Status: 500, Message: "open /x: input/output error"}, false},
		{&AgentError{Status: 404, Message: "unknown_operation"}, false},
		{fmt.Errorf("transport: permission denied"), false},
	} {
		if got := unwritableFailure(test.err); got != test.want {
			t.Fatalf("%v: got %t", test.err, got)
		}
	}
}

func sharedSourceParent(t *testing.T, f *agentHTTPFixture, roots ...string) (task.Parent, []sharedTestAgent) {
	t.Helper()
	var calls atomic.Int64
	modes := make([]string, len(roots))
	for i := range modes {
		modes[i] = "read-only"
	}
	return sharedSourceParentWith(t, f, &calls, modes, roots...)
}

func sharedSourceParentWith(t *testing.T, f *agentHTTPFixture, calls *atomic.Int64, modes []string, roots ...string) (task.Parent, []sharedTestAgent) {
	t.Helper()
	agents := sharedTestAgents(t, f, calls, modes...)
	for i := range agents {
		agents[i].node.LocalRoot = roots[i]
	}
	p := takeoverParent(t, f, agents[0].node, agents[1].node)
	p.Spec.Direction = "LOCAL_TO_OSS"
	p.Spec.Source = execution.Endpoint{Kind: "LOCAL", Root: roots[0]}
	p.Spec.SharedLocalSource = true
	return p, agents
}

func storedProof(t *testing.T, f *agentHTTPFixture, id int64) taskStorageProof {
	t.Helper()
	var proof taskStorageProof
	if err := f.master.store.View(func(tx *state.Tx) error {
		_, err := tx.Get("master-task-storage-proofs", fmt.Sprint(id), &proof)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return proof
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
			t.Fatal("an unwritable source did not grant the Agent's shared proof")
		}
	}

	f2 := newAgentHTTPFixture(t)
	other := t.TempDir()
	sharedTree(t, other, "v2")
	p, agents = sharedSourceParent(t, f2, shared, other)
	if err := f2.master.tasks.ensureSharedSource(ctx, p); err == nil || !strings.Contains(err.Error(), "read different data") {
		t.Fatal("different read-only directories passed the runtime check", err)
	}
	if agents[1].service.Shared.Verified(p.ID, p.Generation, other) {
		t.Fatal("a mismatched Agent was granted a shared proof")
	}
}

// The runtime, like the precheck, compares sampled files only when no node at
// all can write the test file: one read-only node does not weaken the check.
func TestSharedRuntimeTriesEveryWriterBeforeComparingFiles(t *testing.T) {
	f := newAgentHTTPFixture(t)
	shared := t.TempDir()
	sharedTree(t, shared, "v1")
	var calls atomic.Int64
	p, agents := sharedSourceParentWith(t, f, &calls, []string{"read-only", "writable"}, shared, shared)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := f.master.tasks.ensureSharedSource(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, p.Spec.Nodes, true); err != nil {
		t.Fatal(err)
	}
	if proof := storedProof(t, f, p.ID); calls.Load() != 0 || proof.ReadOnly || !proof.Ready {
		t.Fatal("a writable node did not keep the test file check", calls.Load(), proof)
	}
	for _, agent := range agents {
		if !agent.service.Shared.Verified(p.ID, p.Generation, shared) {
			t.Fatal("the marker check did not grant the Agent's shared proof")
		}
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{agents[0].node}, false); err != nil {
		t.Fatal("the read-only node could not read the marker another node wrote", err)
	}

	// A download destination must be writable: a refused write fails takeover
	// setup and never falls back to sampled files.
	g := newAgentHTTPFixture(t)
	calls.Store(0)
	q, _ := sharedSourceParentWith(t, g, &calls, []string{"read-only", "read-only"}, shared, shared)
	q.Spec.Direction, q.Spec.Source, q.Spec.SharedLocalSource = "OSS_TO_LOCAL", execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "source"}, false
	q.Spec.Destination = execution.Endpoint{Kind: "LOCAL", Root: shared}
	if err := g.master.tasks.verifyTaskStorage(ctx, q, q.Spec.Nodes, true); err == nil || calls.Load() != 0 {
		t.Fatal("an unwritable download destination passed takeover setup", err, calls.Load())
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
	// directory, but two nodes that see the same edit keep the edited files
	// in the sample with their new content.
	sharedTree(t, shared, "edited, longer")
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{b}, false); err == nil || !strings.Contains(err.Error(), "with one node online") {
		t.Fatal("a single node replaced the sample", err)
	}
	original := storedProof(t, f, p.ID)
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{a, b}, false); err != nil {
		t.Fatal("two agreeing nodes could not follow an edited sample", err)
	}
	refreshed := storedProof(t, f, p.ID)
	for i, fingerprint := range refreshed.Fingerprints {
		content, err := os.ReadFile(filepath.Join(shared, filepath.FromSlash(fingerprint.Path)))
		if err != nil || fingerprint.Path != original.Fingerprints[i].Path || fingerprint.Size != int64(len(content)) || fingerprint.SHA256 == original.Fingerprints[i].SHA256 {
			t.Fatal("the edited files were not kept with their new content", fingerprint, original.Fingerprints[i], err)
		}
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{b}, false); err != nil {
		t.Fatal("the refreshed sample was not kept", err)
	}
	// Every file gone and nothing left to sample looks like an unmounted share.
	for _, part := range []string{"part-0", "part-1", "part-2", "part-3"} {
		if err := os.RemoveAll(filepath.Join(shared, part)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{a, b}, false); err == nil || !strings.Contains(err.Error(), "still mounted") {
		t.Fatal("an emptied source was accepted", err)
	}
}

// Deleting sampled files at the source, which every node sees, must not
// block takeover; the sample follows the source instead.
func TestReadOnlyTakeoverFollowsADeletionEveryNodeSees(t *testing.T) {
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
	removed := storedProof(t, f, p.ID).Fingerprints[0].Path
	if err := os.Remove(filepath.Join(shared, filepath.FromSlash(removed))); err != nil {
		t.Fatal(err)
	}
	// One node alone cannot tell a deletion from another directory.
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{b}, false); err == nil || !strings.Contains(err.Error(), "is missing on node") || !strings.Contains(err.Error(), "with one node online") {
		t.Fatal("a single node accepted a missing sampled file", err)
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{a, b}, false); err != nil {
		t.Fatal("a file deleted at the source blocked takeover", err)
	}
	refreshed := storedProof(t, f, p.ID)
	if len(refreshed.Fingerprints) != transferstore.SharedFingerprintMaxFiles {
		t.Fatal("the sample was not topped up", refreshed.Fingerprints)
	}
	for _, fingerprint := range refreshed.Fingerprints {
		if fingerprint.Path == removed {
			t.Fatal("the deleted file stayed in the sample")
		}
	}
	for i, agent := range agents {
		if !agent.service.Shared.Verified(p.ID, p.Generation, shared) {
			t.Fatal("an agreeing node lost its proof", i)
		}
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{b}, false); err != nil {
		t.Fatal("a single node failed the refreshed sample", err)
	}
}

// Two nodes that see different data at the same sampled path do not see the
// same storage, however the stored sample compares; the evidence must not be
// replaced by a fresh random sample.
func TestReadOnlyTakeoverRefusesNodesThatSeeDifferentData(t *testing.T) {
	f := newAgentHTTPFixture(t)
	shared, copied := t.TempDir(), t.TempDir()
	sharedTree(t, shared, "v1")
	sharedTree(t, copied, "v1") // An identical copy passes setup: a known limit.
	p, agents := sharedSourceParent(t, f, shared, copied)
	a, b := agents[0].node, agents[1].node
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := f.master.tasks.verifyTaskStorage(ctx, p, p.Spec.Nodes, true); err != nil {
		t.Fatal(err)
	}
	original := storedProof(t, f, p.ID)
	changed, removed := original.Fingerprints[0].Path, original.Fingerprints[1].Path
	if err := os.WriteFile(filepath.Join(shared, filepath.FromSlash(changed)), []byte("written on the share only"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{a, b}, false)
		if err == nil || !strings.Contains(err.Error(), "do not see the same storage") || !strings.Contains(err.Error(), changed) {
			t.Fatal("nodes that read different data were accepted", err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, filepath.FromSlash(changed)), []byte(fmt.Sprintf("object %s v1", strings.TrimSuffix(strings.TrimPrefix(filepath.Base(changed), "object-"), ".bin"))), 0600); err != nil {
		t.Fatal(err)
	}
	// A file missing on one node only is a disagreement too, not a deletion.
	if err := os.Remove(filepath.Join(copied, filepath.FromSlash(removed))); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{a, b}, false); err == nil || !strings.Contains(err.Error(), "is missing") || !strings.Contains(err.Error(), "do not see the same storage") {
		t.Fatal("a file missing on one node only was accepted", err)
	}
	if !reflect.DeepEqual(storedProof(t, f, p.ID), original) {
		t.Fatal("a disagreement replaced the stored sample")
	}
}

// Files added to a source that had none: every node must find files and
// agree on a fresh sample.
func TestReadOnlyTakeoverOfAnEmptySource(t *testing.T) {
	f := newAgentHTTPFixture(t)
	shared, other := t.TempDir(), t.TempDir()
	p, agents := sharedSourceParent(t, f, shared, shared)
	a, b := agents[0].node, agents[1].node
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := f.master.tasks.verifyTaskStorage(ctx, p, p.Spec.Nodes, true); err != nil {
		t.Fatal(err)
	}
	if proof := storedProof(t, f, p.ID); !proof.ReadOnly || len(proof.Fingerprints) != 0 {
		t.Fatal("an empty source did not store an empty sample", proof)
	}
	moved := b
	moved.LocalRoot = other
	sharedTree(t, shared, "v1")
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{a, moved}, false); err == nil || !strings.Contains(err.Error(), "do not see the same storage") {
		t.Fatal("a node finding no file matched one that finds files", err)
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, []task.Node{a, b}, false); err != nil {
		t.Fatal("files added to an empty source blocked takeover", err)
	}
	if proof := storedProof(t, f, p.ID); len(proof.Fingerprints) != transferstore.SharedFingerprintMaxFiles {
		t.Fatal("the new files were not sampled", proof)
	}
}

func TestSkipSharedCheckRequiresAcknowledgement(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	// Any probe write would fail: a passing preflight proves it was skipped.
	agents := readOnlyTestAgents(t, f, "io", &calls)
	a, b := agents[0].node, agents[1].node
	first, second := t.TempDir(), t.TempDir()
	row := uploadRow(taskBinding{a.Address, first}, taskBinding{b.Address, second})
	row.SkipSharedCheck = true
	data := validateRows(t, f, row)
	if data["valid"] != false || !strings.Contains(fmt.Sprint(data["validationErrors"]), "skipSharedCheckAcknowledged:true") {
		t.Fatal("skip without acknowledgement was accepted", data)
	}
	code, response := request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers", f.admin, []taskCreateRequest{row})
	created := response["data"].(map[string]any)
	if code != 200 || created["acceptedCount"] != float64(0) || !strings.Contains(fmt.Sprint(created["validationErrors"]), "skipSharedCheckAcknowledged:true") {
		t.Fatal("creation accepted a skip without acknowledgement", code, response)
	}
	if parents, _, err := f.master.allTaskParents(); err != nil || len(parents) != 0 {
		t.Fatal("an unacknowledged skip created a task", parents, err)
	}

	row.SkipSharedCheckAcknowledged = true
	data = validateRows(t, f, row)
	if data["valid"] != true || calls.Load() != 0 {
		t.Fatal("acknowledged skip still verified the directory", data, calls.Load())
	}
	for _, code := range bindingCodes(data) {
		if code != "SHARED_STORAGE_CHECK_SKIPPED" {
			t.Fatal("skipped check is not reported on every binding", data)
		}
	}
	for _, root := range []string{first, second} {
		if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
			t.Fatal("skipped check wrote to the directory", entries, err)
		}
	}
	ctx := context.Background()
	spec, _, err := f.master.resolveTaskSpec(ctx, row, "local-transfers")
	if err != nil || !spec.SharedCheckSkipped {
		t.Fatal("acknowledged skip was not recorded in the task", spec, err)
	}
	// One node has no shared check; the option is ignored, not recorded.
	single := uploadRow(taskBinding{a.Address, first})
	single.SkipSharedCheck, single.SkipSharedCheckAcknowledged = true, true
	spec, _, err = f.master.resolveTaskSpec(ctx, single, "local-transfers")
	if err != nil || spec.SharedCheckSkipped {
		t.Fatal("single-node skip was recorded", spec, err)
	}
	buckets := taskCreateRequest{SourceS3Alias: "qa", SourceBucket: "a", DestS3Alias: "qa", DestBucket: "b", SkipSharedCheck: true, SkipSharedCheckAcknowledged: true}
	if _, _, _, err := normalizeTaskRequest(buckets, "add-buckets"); err == nil {
		t.Fatal("a bucket migration accepted skipSharedCheck")
	}
	if localizeMessage("zh-CN", skipSharedCheckUnacknowledged) == skipSharedCheckUnacknowledged || localizeMessage("ja-JP", skipSharedCheckUnacknowledged) == skipSharedCheckUnacknowledged {
		t.Fatal("acknowledgement error is not translated")
	}

	// The record is fenced from older Masters, which would drop it.
	ensureStorage(t, f)
	code, response = request(t, f.http.Client(), "POST", f.http.URL+"/api/master/local-transfers", f.admin, []taskCreateRequest{row})
	if created := response["data"].(map[string]any); code != 200 || created["acceptedCount"] != float64(1) {
		t.Fatal("an acknowledged skip was not created", code, response)
	}
	var format struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := f.master.store.View(func(tx *state.Tx) error { _, err := tx.Get("metadata", "format", &format); return err }); err != nil || format.SchemaVersion < state.SchemaSharedCheckSkipped {
		t.Fatal("a skipped check did not raise the database format", format, err)
	}
}

func TestSkippedSharedCheckGrantsProofsWithoutChecking(t *testing.T) {
	f := newAgentHTTPFixture(t)
	first, second := t.TempDir(), t.TempDir()
	sharedTree(t, first, "v1")
	sharedTree(t, second, "v2")
	p, agents := sharedSourceParent(t, f, first, second)
	p.Spec.SharedCheckSkipped = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := f.master.tasks.ensureSharedSource(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := f.master.tasks.verifyTaskStorage(ctx, p, p.Spec.Nodes, true); err != nil {
		t.Fatal(err)
	}
	for i, root := range []string{first, second} {
		if !agents[i].service.Shared.Verified(p.ID, p.Generation, root) {
			t.Fatal("a skipped check did not grant the Agent's proof")
		}
	}
	if row := taskDTO(p); row["sharedCheckSkipped"] != true {
		t.Fatal("task details do not show the skipped check", row)
	}
	if row := resolvedCSVRow(t, reportSummaryHeader, func(w *csv.Writer) error { return summaryCSV(w, p, nil) }); row["sharedCheckSkipped"] != "true" {
		t.Fatal("task report does not show the skipped check", row)
	}
	if coverage := newCoverage(p); !coverage.SharedCheckSkipped {
		t.Fatal("report metadata does not show the skipped check")
	}
	if !strings.Contains(sharedCheckSkippedMeaning, "an upload") || !strings.Contains(sharedCheckSkippedMeaning, "a download") {
		t.Fatal("the report meaning does not cover both directions", sharedCheckSkippedMeaning)
	}

	old := newAgentHTTPFixture(t)
	var calls atomic.Int64
	legacy := readOnlyTestAgents(t, old, "legacy", &calls)
	for i := range legacy {
		legacy[i].node.LocalRoot = first
	}
	q := takeoverParent(t, old, legacy[0].node, legacy[1].node)
	q.Spec.Direction, q.Spec.Source, q.Spec.SharedLocalSource, q.Spec.SharedCheckSkipped = "LOCAL_TO_OSS", execution.Endpoint{Kind: "LOCAL", Root: first}, true, true
	if err := old.master.tasks.ensureSharedSource(ctx, q); err == nil || !strings.Contains(err.Error(), "whose shared directory check was skipped") {
		t.Fatal("an old Agent did not ask for an upgrade", err)
	}
}

// Adding nodes to a task created with the check skipped leaves the new nodes
// unchecked too: that needs its own acknowledgement, the Agents must support
// it, and the precheck must say that nothing was checked.
func TestAddingNodesToASkippedTaskNeedsItsOwnAcknowledgement(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "io", "io", "io", "legacy")
	members := []task.Node{agents[0].node, agents[1].node}
	roots := []string{t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()}
	for i := range members {
		members[i].LocalRoot = roots[i]
	}
	p, _, err := f.master.tasks.repo.Create(task.Spec{Direction: "LOCAL_TO_OSS", Source: execution.Endpoint{Kind: "LOCAL", Root: roots[0]}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: members, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", SharedLocalSource: true, SharedCheckSkipped: true}, "skipped-membership")
	if err != nil {
		t.Fatal(err)
	}
	waitOnline(t, f, agents[2].node, agents[3].node)
	status, view := request(t, f.http.Client(), "GET", fmt.Sprintf("%s/api/master/tasks/%d/nodes", f.http.URL, p.ID), f.admin, nil)
	if status != 200 || view["data"].(map[string]any)["sharedCheckSkipped"] != true {
		t.Fatal("the node view does not say that the check is skipped", status, view)
	}
	add := func(agent int, extra map[string]any) (int, map[string]any) {
		body := map[string]any{"add": []string{agents[agent].node.Address}, "bindings": []map[string]string{{"executionNodeIp": agents[agent].node.Address, "localPath": roots[agent]}}}
		for key, value := range extra {
			body[key] = value
		}
		return postNodes(t, f, p.ID, body)
	}
	status, body := add(2, map[string]any{"dryRun": true})
	data, _ := body["data"].(map[string]any)
	if status != 200 || data["valid"] != true || calls.Load() != 0 {
		t.Fatal("the dry run did not pass without a check", status, body, calls.Load())
	}
	checks := data["nodeBindings"].([]any)
	if check := checks[0].(map[string]any); len(checks) != 1 || check["code"] != "SHARED_STORAGE_CHECK_SKIPPED" || !strings.HasPrefix(fmt.Sprint(check["message"]), "Not checked") {
		t.Fatal("the added node is not reported as unchecked", checks)
	}
	if status, body := add(2, nil); status != 409 || body["code"] != "SHARED_CHECK_SKIP_UNACKNOWLEDGED" || !strings.Contains(fmt.Sprint(body["message"]), "skipSharedCheckAcknowledged:true") {
		t.Fatal("a node joined unchecked without an acknowledgement", status, body)
	}
	if status, body := add(3, map[string]any{"skipSharedCheckAcknowledged": true}); status != 409 || !strings.Contains(fmt.Sprint(body["message"]), "to run a task whose shared directory check was skipped") {
		t.Fatal("an Agent that cannot record the skipped check joined", status, body)
	}
	if status, body := add(2, map[string]any{"skipSharedCheckAcknowledged": true}); status != 200 || body["data"].(map[string]any)["accepted"] != true {
		t.Fatal("an acknowledged node addition was refused", status, body)
	}
	for _, key := range []string{skipSharedCheckJoinUnacknowledged, "Not checked: this task was created with the shared directory check skipped, so this node was not verified to see the same storage as the task's other nodes"} {
		if localizeMessage("zh-CN", key) == key || localizeMessage("ja-JP", key) == key {
			t.Fatal("not translated:", key)
		}
	}
}

// Manual recovery of a download without takeover repeats the write probe,
// unless the operator skipped the shared check when creating the task.
func TestChildRecoveryHonoursASkippedSharedCheck(t *testing.T) {
	nodes := []task.Node{{ID: "a"}, {ID: "b"}}
	download := task.Parent{Spec: task.Spec{Direction: "OSS_TO_LOCAL", Source: execution.Endpoint{Kind: "S3"}, Destination: execution.Endpoint{Kind: "LOCAL", Root: "/mnt/share"}, Nodes: nodes}}
	if got := recoveryDestinationCheck(download); got != "probe" {
		t.Fatal("a multi-node download must be probed", got)
	}
	download.Spec.SharedCheckSkipped = true
	if got := recoveryDestinationCheck(download); got != "skipped" {
		t.Fatal("recovery repeated a check the operator skipped", got)
	}
	download.Spec.Nodes = nodes[:1]
	if got := recoveryDestinationCheck(download); got != "" {
		t.Fatal("one node has nothing to compare", got)
	}
	download.Spec.Nodes = nodes
	download.Failover = &task.FailoverState{Enabled: true}
	if got := recoveryDestinationCheck(download); got != "" {
		t.Fatal("takeover already verifies the directory", got)
	}
}

// Adding nodes to a task whose source no node can write compares sampled
// files across the current and added nodes, as the creation precheck does.
func TestAddingNodesToAnUnwritableSourceComparesSampledFiles(t *testing.T) {
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "read-only", "read-only", "read-only")
	shared, other := t.TempDir(), t.TempDir()
	sharedTree(t, shared, "v1")
	sharedTree(t, other, "v2")
	members := []task.Node{agents[0].node, agents[1].node}
	for i := range members {
		members[i].LocalRoot = shared
	}
	p, _, err := f.master.tasks.repo.Create(task.Spec{Direction: "LOCAL_TO_OSS", Source: execution.Endpoint{Kind: "LOCAL", Root: shared}, Destination: execution.Endpoint{Kind: "S3", Ref: "qa", Bucket: "target"}, Nodes: members, ConflictPolicy: "SKIP", DistributionMode: "BALANCED", SharedLocalSource: true}, "unwritable-membership")
	if err != nil {
		t.Fatal(err)
	}
	add := func(root string) (int, map[string]any) {
		return postNodes(t, f, p.ID, map[string]any{"dryRun": true, "add": []string{agents[2].node.Address}, "bindings": []map[string]string{{"executionNodeIp": agents[2].node.Address, "localPath": root}}})
	}
	status, body := add(shared)
	data, _ := body["data"].(map[string]any)
	if status != 200 || data["valid"] != true || calls.Load() == 0 {
		t.Fatal("an added node on the same unwritable source was refused", status, body)
	}
	if check := data["nodeBindings"].([]any)[0].(map[string]any); check["code"] != "SHARED_STORAGE_FINGERPRINT_VERIFIED" || !strings.Contains(fmt.Sprint(check["message"]), "first and last 64 KiB") {
		t.Fatal("the added node does not say what was compared", check)
	}
	status, body = add(other)
	data, _ = body["data"].(map[string]any)
	if encoded, _ := json.Marshal(body); status != 200 || data["valid"] != false || !strings.Contains(string(encoded), "read different data for sampled file") {
		t.Fatal("an added node on another directory passed", status, string(encoded))
	}
}

// The real Agent path, without a simulated refusal: a directory this account
// may read but not write falls back to comparing sampled files.
func TestRealAgentsCompareFilesInADirectoryTheyCannotWrite(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced for this account")
	}
	f := newAgentHTTPFixture(t)
	var calls atomic.Int64
	agents := sharedTestAgents(t, f, &calls, "writable", "writable")
	shared := t.TempDir()
	sharedTree(t, shared, "v1")
	if err := os.Chmod(shared, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shared, 0700) })
	data := validateRows(t, f, uploadRow(taskBinding{agents[0].node.Address, shared}, taskBinding{agents[1].node.Address, shared}))
	if codes := bindingCodes(data); data["valid"] != true || codes[0] != "SHARED_STORAGE_FINGERPRINT_VERIFIED" || calls.Load() == 0 {
		t.Fatal("a directory the Agents cannot write did not fall back to sampled files", data)
	}
}
