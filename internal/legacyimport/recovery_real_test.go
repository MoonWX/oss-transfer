package legacyimport

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// This opt-in test consumes the independently captured, real stopped V4 fixture.
// It copies the S3 database before resuming, so the original evidence stays intact.
// It uses the scanner and executor directly while the service startup guard stays
// enabled; full process/HTTP acceptance follows finalization separately.
func TestRealV4ResumeInterruptedCopy(t *testing.T) {
	snapshot := os.Getenv("OSS_TRANSFER_LEGACY_RESUME_SNAPSHOT")
	if snapshot == "" {
		t.Skip("set OSS_TRANSFER_LEGACY_RESUME_SNAPSHOT to the real V4 fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	startRealLegacyS3(t, ctx, snapshot)
	input, err := os.Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Inspect(ctx, input)
	input.Close()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(ctx, PrepareOptions{SnapshotPaths: []string{snapshot}, Destination: filepath.Join(t.TempDir(), "native"), PublicURL: "http://127.0.0.1:1685", DirectNodes: map[string]DirectNodeApproval{plan.Header.Node: {Address: plan.Header.Node}}})
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
	repo := task.New(p.Master)
	old, err := repo.Get(8)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := repo.BeginLegacyRecovery(8, old.Spec)
	if err != nil {
		t.Fatal(err)
	}
	db := p.Agents[plan.Header.Node]
	factory := &storageconfig.Factory{Source: storageconfig.NewMaster(p.Master)}
	scanner, err := discovery.New(db, factory, discovery.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	jobs, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := execution.NewEngine(jobs, factory, execution.Options{WorkersPerJob: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	if err := engine.StopParent(ctx, 8, 1); err != nil {
		t.Fatal(err)
	}
	if err := engine.ResumeParent(ctx, 8, parent.Generation, true); err != nil {
		t.Fatal(err)
	}
	if err := repo.Resumed(8, parent.Generation); err != nil {
		t.Fatal(err)
	}
	spec := discovery.Spec{ParentID: 8, Generation: parent.Generation, Source: parent.Spec.Source, Prefix: parent.Spec.Prefix, StartKey: parent.Spec.StartKey, EndKey: parent.Spec.EndKey, LegacyManifestID: parent.Legacy.MigrationID}
	if _, err := scanner.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	var scanned discovery.State
	for {
		scanned, err = scanner.Status(ctx, 8)
		if err != nil {
			t.Fatal(err)
		}
		if scanned.Status == discovery.Completed {
			break
		}
		if scanned.Status == discovery.Failed {
			t.Fatal(scanned.Error)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := repo.ScanProgress(8, parent.Generation, scanned.TotalObjects, scanned.TotalBytes, int(scanned.LastBatch), scanned.ScanComplete, scanned.Error); err != nil {
		t.Fatal(err)
	}
	verified, written := 0, 0
	var after int64
	for {
		batch, found, err := scanner.Batch(ctx, 8, parent.Generation, after)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		child, err := repo.Assign(8, parent.Generation, int(batch.Number), batch.Objects, batch.Legacy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Accept(ctx, child.Job); err != nil {
			t.Fatal(err)
		}
		if err := engine.Start(ctx, child.Job.ChildID, parent.Generation); err != nil {
			t.Fatal(err)
		}
		record, err := engine.Wait(ctx, child.Job.ChildID)
		if err != nil {
			t.Fatal(err)
		}
		if record.Status != execution.Completed {
			for _, result := range record.Results {
				if result.Error != "" {
					t.Logf("item result=%s reconciliation=%s error=%s", result.Status, result.Reconciliation, result.Error)
				}
			}
			t.Fatalf("child %s status %s error %s", child.Job.ChildID, record.Status, record.Error)
		}
		for _, result := range record.Results {
			if result.Reconciliation == "SHA256_VERIFIED" {
				verified++
			}
			if result.TargetCommitted {
				written++
			}
		}
		if err := repo.Observe(8, parent.Generation, child.Job.Number, record); err != nil {
			t.Fatal(err)
		}
		// Independently read every resulting object and compare complete SHA256.
		source, target, err := factory.Open(ctx, child.Job)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range child.Job.Items {
			var hashes [2][32]byte
			for index, store := range []transferstore.Store{source, target} {
				key := item.Source.Key
				if index == 1 {
					key = item.DestinationKey
				}
				r, _, err := store.Open(ctx, key, transferstore.OpenOptions{})
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.New()
				n, err := io.Copy(hash, r)
				r.Close()
				if err != nil || n != item.Source.Size {
					t.Fatal("incomplete independent read", err)
				}
				copy(hashes[index][:], hash.Sum(nil))
			}
			if hashes[0] != hashes[1] {
				t.Fatal("resumed destination differs from current source")
			}
		}
		source.Close()
		target.Close()
		after = batch.Number
	}
	done, err := repo.FinishIfReady(8, parent.Generation)
	if err != nil || !done {
		t.Fatal(done, err)
	}
	current, err := repo.Get(8)
	if err != nil || current.Status != "COMPLETED" || current.Counters.Succeeded != 260 || current.ScannedObjects != 260 || verified < 1 || written < 1 {
		t.Fatal("incorrect actual recovery", current.Status, current.Counters, verified, written, err)
	}
	t.Logf("real V4 interrupted task resumed: SHA256 verified %d existing objects, wrote %d, independently checked all 260", verified, written)
}

type legacyPrivateRuntime struct {
	LegacyAdminPassword string
	S3Command           []string
	Storage             struct{ Endpoint, AccessKey, SecretKey string }
}

func startRealLegacyS3(t *testing.T, ctx context.Context, snapshot string) legacyPrivateRuntime {
	t.Helper()
	var private legacyPrivateRuntime

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(snapshot), "private-runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &private); err != nil {
		t.Fatal(err)
	}
	args := append([]string(nil), private.S3Command...)
	if len(args) < 2 {
		t.Fatal("fixture S3 restart command missing")
	}
	for i, arg := range args {
		if strings.HasPrefix(arg, "-dir=") {
			copyDir := filepath.Join(t.TempDir(), "seaweed")
			if err := os.CopyFS(copyDir, os.DirFS(strings.TrimPrefix(arg, "-dir="))); err != nil {
				t.Fatal(err)
			}
			args[i] = "-dir=" + copyDir
		}
	}
	log, err := os.Create(filepath.Join(t.TempDir(), "s3.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Env = append(os.Environ(), "AWS_ACCESS_KEY_ID="+private.Storage.AccessKey, "AWS_SECRET_ACCESS_KEY="+private.Storage.SecretKey)
	command.Stdout = log
	command.Stderr = log
	endpoint, err := url.Parse(private.Storage.Endpoint)
	if err != nil || endpoint.Host == "" {
		t.Fatal("fixture storage endpoint absent")
	}
	if existing, err := net.DialTimeout("tcp", endpoint.Host, 50*time.Millisecond); err == nil {
		existing.Close()
		t.Fatal("fixture S3 port already in use; refusing to use another process")
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	ready := false
	for until := time.Now().Add(20 * time.Second); time.Now().Before(until); {
		conn, err := net.DialTimeout("tcp", endpoint.Host, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("isolated S3 fixture did not start")
	}

	return private
}
