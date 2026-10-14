package legacyimport

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type migrationSnapshotSource struct{ value storageconfig.Snapshot }

func (s migrationSnapshotSource) Snapshot() (storageconfig.Snapshot, error) { return s.value, nil }

func TestRealV4FinalizedNativeProcesses(t *testing.T) {
	fixture, masterBinary, agentBinary := os.Getenv("OSS_TRANSFER_LEGACY_RESUME_SNAPSHOT"), os.Getenv("OSS_TRANSFER_NATIVE_MASTER"), os.Getenv("OSS_TRANSFER_NATIVE_AGENT")
	if fixture == "" || masterBinary == "" || agentBinary == "" {
		t.Skip("real V4 fixture and freshly built native binaries required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	private := startRealLegacyS3(t, ctx, fixture)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	origin := "http://" + address
	input, err := os.Open(fixture)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Inspect(ctx, input)
	input.Close()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(ctx, PrepareOptions{SnapshotPaths: []string{fixture}, Destination: filepath.Join(t.TempDir(), "native"), PublicURL: origin, DirectNodes: map[string]DirectNodeApproval{plan.Header.Node: {Address: plan.Header.Node}}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.ConvertTasks(ctx, TaskImportOptions{LegacyTimezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.PrepareRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	// Explicit settings are part of this test's installation plan. Conversion
	// never infers these from the importing machine's environment.
	settings := map[string]string{"rate-limit.src.qps.list": "100", "rate-limit.dest.qps.put": "1400", "rate-limit.dest.qps.head": "2800", "rate-limit.bandwidth": "2000000000", "rate-limit.auto-adjust.consecutive-503-threshold": "20", "rate-limit.auto-adjust.reduce-factor": "0.9", "rate-limit.auto-adjust.increase-factor": "1.2", "rate-limit.auto-adjust.min-put-qps": "200", "rate-limit.auto-adjust.interval-ms": "10000"}
	if err := p.WriteRuntimeConfig(ctx, address, map[string]map[string]string{plan.Header.Node: settings}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := storageconfig.NewMaster(p.Master).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	specs := map[int64]task.Spec{}
	repo := task.New(p.Master)
	for _, id := range []int64{7, 8} {
		parent, err := repo.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		specs[id] = parent.Spec
	}
	if err := p.AcceptRuntimeDefaults(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Finalize(ctx); err != nil {
		t.Fatal(err)
	}
	startProcess := func(binary, config, name string) *exec.Cmd {
		t.Helper()
		log, err := os.Create(filepath.Join(t.TempDir(), name+".log"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { log.Close() })
		command := exec.CommandContext(ctx, binary, "--config", config)
		command.Stdout = log
		command.Stderr = log
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
		return command
	}
	startProcess(masterBinary, filepath.Join(p.Directory, "master", "service.json"), "master")
	client := &http.Client{Timeout: 15 * time.Second}
	token := ""
	rawCall := func(method, path string, body any) (int, []byte, error) {
		var in io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				return 0, nil, err
			}
			in = bytes.NewReader(encoded)
		}
		request, err := http.NewRequestWithContext(ctx, method, origin+path, in)
		if err != nil {
			return 0, nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := client.Do(request)
		if err != nil {
			return 0, nil, err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		return response.StatusCode, data, err
	}
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		status, raw, err := rawCall(method, path, body)
		if err != nil || status != 200 {
			t.Fatalf("native migration API %s: status=%d error=%v", path, status, err)
		}
		var response map[string]any
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		if response["success"] == false {
			t.Fatalf("native migration API %s rejected: %v", path, response["message"])
		}
		return response
	}
	ready := false
	for until := time.Now().Add(20 * time.Second); time.Now().Before(until); {
		if status, _, err := rawCall("GET", "/healthz", nil); err == nil && status == 200 {
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("finalized native Master did not become ready")
	}
	setup := call("GET", "/api/setup/status", nil)
	if setup["configured"] != true {
		t.Fatal("migrated account returned to first setup")
	}
	username := ""
	for _, user := range plan.Users {
		if user.Role == "admin" {
			username = user.Username
			break
		}
	}
	auth := call("POST", "/api/auth/login", map[string]string{"username": username, "password": private.LegacyAdminPassword})
	token, _ = auth["token"].(string)
	if token == "" {
		t.Fatal("original V4 account could not log in")
	}
	agentConfig := filepath.Join(p.Directory, filepath.Dir(filepath.FromSlash(p.Manifest.Nodes[0].Database)), "service.json")
	startProcess(agentBinary, agentConfig, "agent")
	online := false
	for until := time.Now().Add(20 * time.Second); time.Now().Before(until); {
		response := call("GET", "/api/master/nodes", nil)
		data := response["data"].(map[string]any)
		for _, raw := range data["nodes"].([]any) {
			node := raw.(map[string]any)
			if node["online"] == true && node["id"] == p.Manifest.Nodes[0].InstanceID {
				online = true
			}
		}
		if online {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !online {
		t.Fatal("migrated Agent identity did not reconnect")
	}
	workers := call("GET", "/api/master/worker-status", nil)["workers"].(map[string]any)
	node := workers[plan.Header.Node].(map[string]any)
	bandwidth := node["rateLimit"].(map[string]any)["bandwidth"].(map[string]any)
	if bandwidth["manualLimitBps"] != float64(1000000) {
		t.Fatalf("persisted legacy node limit lost: %v", bandwidth)
	}
	// The old test intentionally persisted 1 Mbps. Verify it first, then use
	// the ordinary administrator API to finish the isolated fixture promptly.
	call("POST", "/api/master/rate-limit/bandwidth/"+plan.Header.Node, map[string]float64{"qps": 2000000000})
	for _, id := range []int64{7, 8} {
		detail := call("GET", fmt.Sprintf("/api/master/tasks/%d", id), nil)["data"].(map[string]any)
		if detail["status"] != "STOPPED" {
			t.Fatal("migration automatically started old task", id, detail["status"])
		}
		response := call("POST", "/api/master/resume", map[string]any{"taskIds": []int64{id}})
		if response["failed"] != float64(0) {
			t.Fatalf("resume failed: %v", response["details"])
		}
		lastProgress := time.Time{}
		for {
			detail = call("GET", fmt.Sprintf("/api/master/tasks/%d", id), nil)["data"].(map[string]any)
			if time.Since(lastProgress) > 10*time.Second {
				t.Logf("task %d status=%v scanned=%v migrated=%v runtime=%v", id, detail["status"], detail["listObjectCount"], detail["putObjectCount"], detail["errorMessage"])
				lastProgress = time.Now()
			}
			if detail["status"] == "COMPLETED" {
				break
			}
			if detail["status"] == "FAILED" || detail["status"] == "PARTIAL_FAILED" {
				t.Fatal("native recovery failed", id, detail["errorMessage"])
			}
			if ctx.Err() != nil {
				t.Fatal("native recovery timed out", id, detail["status"], detail["errorMessage"])
			}
			time.Sleep(50 * time.Millisecond)
		}
		if detail["putObjectCount"] != float64(260) || detail["listObjectCount"] != float64(260) {
			t.Fatal("recovery counters include legacy plus new results", id, detail["putObjectCount"], detail["listObjectCount"])
		}
		status, raw, err := rawCall("GET", fmt.Sprintf("/api/master/task-reports/%d", id), nil)
		if err != nil || status != 200 {
			t.Fatal("native report unavailable", status, err)
		}
		archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatal(err)
		}
		var rows [][]string
		for _, file := range archive.File {
			if file.Name == "file-results.csv" {
				reader, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				csvReader := csv.NewReader(reader)
				rows, err = csvReader.ReadAll()
				reader.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		if len(rows) != 261 {
			t.Fatal("report rows missing", id, len(rows))
		}
		fields := map[string]int{}
		for index, name := range rows[0] {
			fields[strings.TrimPrefix(name, "\ufeff")] = index
		}
		factory := &storageconfig.Factory{Source: migrationSnapshotSource{snapshot}}
		spec := specs[id]
		source, target, err := factory.Open(ctx, execution.Job{Source: spec.Source, Destination: spec.Destination})
		if err != nil {
			t.Fatal(err)
		}
		verified := 0
		for _, row := range rows[1:] {
			if row[fields["result"]] != "SUCCESS" {
				t.Fatal("report contains unsuccessful object", id, row[fields["result"]])
			}
			if strings.Contains(row[fields["reasonCode"]], "SHA256_VERIFIED") {
				verified++
			}
			if strings.Contains(row[fields["childTaskId"]], "oss") || !strings.HasPrefix(row[fields["childTaskId"]], fmt.Sprint(id)+"-") {
				t.Fatal("unstable child ID")
			}
			var hashes [2][32]byte
			for index, store := range []transferstore.Store{source, target} {
				key := row[fields["logicalKey"]]
				if index == 1 {
					key = row[fields["destinationKey"]]
				}
				reader, _, err := store.Open(ctx, key, transferstore.OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.New()
				_, err = io.Copy(hash, reader)
				reader.Close()
				if err != nil {
					t.Fatal(err)
				}
				copy(hashes[index][:], hash.Sum(nil))
			}
			if hashes[0] != hashes[1] {
				t.Fatal("independent finalized process byte verification failed", id)
			}
		}
		source.Close()
		target.Close()
		if verified < 1 {
			t.Fatal("old successful content was not reconciled", id)
		}
		t.Logf("native process task %d: old login and Agent identity retained; 260 successful report rows and independent SHA256 checks; %d reused after verification", id, verified)
	}
	recycled := call("GET", "/api/master/deleted-tasks", nil)
	if recycled["totalCount"] != float64(1) {
		t.Fatal("recycle bin not retained")
	}
}
