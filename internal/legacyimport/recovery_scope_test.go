package legacyimport

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/discovery"
	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

type recoveryLocalFactory struct{}

func (recoveryLocalFactory) OpenEndpoint(_ context.Context, endpoint execution.Endpoint) (transferstore.Store, error) {
	return transferstore.NewLocal(endpoint.Root)
}
func (recoveryLocalFactory) Open(ctx context.Context, job execution.Job) (transferstore.Store, transferstore.Store, error) {
	source, err := transferstore.NewLocal(job.Source.Root)
	if err != nil {
		return nil, nil, err
	}
	target, err := transferstore.NewLocal(job.Destination.Root)
	if err != nil {
		source.Close()
		return nil, nil, err
	}
	return source, target, nil
}

// The historical records exercise the real importer, and the resulting batches
// run through real local stores. No mock response stands in for a target write.
func TestRecoveryTimeScopeDoesNotOverwriteExcludedObjects(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := taskFixture(t)
	from := time.Unix(1700000000, 0).UTC()
	until := from.Add(time.Hour)
	request := fixture.master["task_creation:fixture"].(map[string]any)["request"].(map[string]any)
	request["dateAfter"], request["dateBefore"] = from.Format(time.RFC3339), until.Format(time.RFC3339)
	for _, values := range []map[string]any{fixture.master, fixture.node} {
		for key := range values {
			if strings.HasPrefix(key, "file-report:") {
				delete(values, key)
			}
		}
	}
	fixture.master["task:224"].(map[string]any)["status"] = "RUNNING"
	fixture.node["task:225"].(map[string]any)["status"] = "RUNNING"
	sourceRoot, targetRoot := t.TempDir(), t.TempDir()
	cases := []struct {
		key            string
		when           time.Time
		result, reason string
	}{
		{"logs/inside", from, "UNPROCESSED", ""},
		// Its current mtime now passes, but its original explicit exclusion stays.
		{"logs/excluded", from.Add(time.Minute), "SKIPPED", "TIME_FILTER"},
		{"logs/at-end", until, "UNPROCESSED", ""},
		{"logs/after-end", until.Add(time.Second), "UNPROCESSED", ""},
	}
	for _, test := range cases {
		for _, root := range []string{sourceRoot, targetRoot} {
			path := filepath.Join(root, filepath.FromSlash(test.key))
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			data := "keep-target"
			if root == sourceRoot {
				data = "new-source"
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, test.when, test.when); err != nil {
				t.Fatal(err)
			}
		}
		encoded := base64.RawURLEncoding.EncodeToString([]byte(test.key))
		fixture.master["file-report:224:item:"+encoded] = map[string]any{"taskId": 224, "reportTaskId": 224, "logicalKey": test.key, "sourceKey": test.key, "result": test.result, "reasonCode": test.reason, "size": 10}
	}
	// A duplicate older completion and an enumerated local batch must not undo
	// the explicit TIME_FILTER row during union construction.
	fixture.master["completed:224:"+base64.RawURLEncoding.EncodeToString([]byte("logs/excluded"))] = map[string]any{"taskId": 224, "logicalKey": "logs/excluded", "size": 10}
	fixture.master["task:224"].(map[string]any)["localFileEntries"] = []map[string]any{{"relativeKey": "logs/excluded", "size": 10}}
	fixture.write(t)
	prepared, err := Prepare(ctx, fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	if _, err := prepared.ConvertTasks(ctx, TaskImportOptions{LegacyTimezone: "UTC"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := prepared.PrepareRecovery(ctx); err != nil {
			t.Fatal(err)
		}
	}
	parent, err := task.New(prepared.Master).Get(224)
	if err != nil {
		t.Fatal(err)
	}
	var agentKey string
	for _, node := range prepared.Manifest.Nodes {
		if node.InstanceID == parent.Spec.Nodes[0].ID {
			agentKey = node.LegacyNode
		}
	}
	db := prepared.Agents[agentKey]
	factory := recoveryLocalFactory{}
	scanner, err := discovery.New(db, factory, discovery.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	spec := discovery.Spec{ParentID: 224, Generation: 1, Source: execution.Endpoint{Kind: "LOCAL", Root: sourceRoot}, LegacyManifestID: prepared.Manifest.MigrationID, ModifiedFrom: *parent.Spec.ModifiedFrom, ModifiedUntil: *parent.Spec.ModifiedUntil}
	if _, err := scanner.Start(ctx, spec); err != nil {
		t.Fatal(err)
	}
	for {
		status, err := scanner.Status(ctx, 224)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == discovery.Completed {
			if status.TotalObjects != 1 {
				t.Fatalf("excluded objects reentered scan: %+v", status)
			}
			break
		}
		if status.Status == discovery.Failed || ctx.Err() != nil {
			t.Fatal(status, ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	batch, found, err := scanner.Batch(ctx, 224, 1, 0)
	if err != nil || !found || len(batch.Objects) != 1 || batch.Objects[0].Key != "logs/inside" {
		t.Fatal(batch, found, err)
	}
	repo, err := execution.NewRepository(db, execution.RepositoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := execution.NewEngine(repo, factory, execution.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	job := execution.Job{ChildID: "224-1", ParentID: 224, Number: 1, Generation: 1, DiscoveryComplete: true, ConflictPolicy: "OVERWRITE", Source: spec.Source, Destination: execution.Endpoint{Kind: "LOCAL", Root: targetRoot}}
	for _, object := range batch.Objects {
		candidate := batch.Legacy[object.Key]
		job.Items = append(job.Items, execution.Item{ID: object.Key, Source: object, DestinationKey: object.Key, Legacy: &candidate})
	}
	if _, err := engine.Accept(ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := engine.Start(ctx, job.ChildID, 1); err != nil {
		t.Fatal(err)
	}
	for {
		status, err := engine.Get(ctx, job.ChildID)
		if err != nil {
			t.Fatal(err)
		}
		if status.Status == execution.Completed {
			break
		}
		if status.Status == execution.Failed || status.Status == execution.PartialFailed || ctx.Err() != nil {
			t.Fatal(status, ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	for _, test := range cases {
		data, err := os.ReadFile(filepath.Join(targetRoot, filepath.FromSlash(test.key)))
		want := "keep-target"
		if test.key == "logs/inside" {
			want = "new-source"
		}
		if err != nil || string(data) != want {
			t.Fatalf("%s target=%q want=%q error=%v", test.key, data, want, err)
		}
	}
	history, err := task.New(prepared.Master).LegacyHistory(224, "OBJECT", "", 128)
	if err != nil || len(history) != 4 {
		t.Fatal("original exclusion history lost", len(history), err)
	}
}
