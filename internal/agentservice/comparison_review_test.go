package agentservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/comparison"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

// A manual resume at a newer storage revision becomes the revision later
// automatic resumes check, so the next restart resumes by itself.
func TestComparisonAutoResumeAfterManualResumeAtANewRevision(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	directory := filepath.Join(t.TempDir(), "comparisons")
	s3 := newComparisonS3(t, comparisonFixtureObjects)

	s := openComparisonService(t, db, directory, nil)
	if _, err := s.Configs.ApplySnapshot(comparisonSnapshotAt(1, s3.server.URL)); err != nil {
		t.Fatal(err)
	}
	s3.block()
	if status, body := comparisonCall(t, s, "comparison_start", comparisonRequestFor("cmp", 1), true); status != 200 {
		t.Fatal(status, body)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Connections change while the Agent is stopped.
	if _, err := storageconfig.NewAgent(db).ApplySnapshot(comparisonSnapshotAt(2, s3.server.URL)); err != nil {
		t.Fatal(err)
	}
	s = openComparisonService(t, db, directory, nil)
	status, body := comparisonCall(t, s, "comparison_get", map[string]any{"id": "cmp", "limit": 1}, false)
	if status != 200 || body["status"] != comparison.StateInterrupted || body["errorDetail"] != errComparisonStorageChanged.Error() {
		t.Fatalf("after the connection change: %d %+v", status, body)
	}
	// The user resumes at revision 2; the Agent restarts while it runs.
	status, body = comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{ID: "cmp", StorageRevision: 2}, true)
	if status != 200 || body["status"] != comparison.StateRunning {
		t.Fatal(status, body)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	s3.release()
	s = openComparisonService(t, db, directory, nil)
	done := waitComparison(t, s, "cmp", hasStatus(comparison.StateCompleted, comparison.StateFailed, comparison.StateInterrupted))
	if done["status"] != comparison.StateCompleted || done["resumes"] != float64(2) || done["errorDetail"] != nil {
		t.Fatalf("not resumed automatically after the manual resume: %+v", done)
	}
}

// A result file removed by hand while the Agent runs makes the comparison
// unavailable; the replies never reveal the Agent's local paths.
func TestComparisonMissingResultFileIsReportedAsUnavailable(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s3 := newComparisonS3(t, comparisonFixtureObjects)
	directory := filepath.Join(t.TempDir(), "comparisons")
	s := openComparisonService(t, db, directory, nil)
	if _, err := s.Configs.ApplySnapshot(comparisonSnapshotAt(1, s3.server.URL)); err != nil {
		t.Fatal(err)
	}
	if status, body := comparisonCall(t, s, "comparison_start", comparisonRequestFor("cmp-1", 1), true); status != 200 {
		t.Fatal(status, body)
	}
	waitComparison(t, s, "cmp-1", hasStatus(comparison.StateCompleted))
	sum := sha256.Sum256([]byte("cmp-1"))
	removeOnceReleased(t, filepath.Join(directory, hex.EncodeToString(sum[:16]), "rows.jsonl"))
	for _, op := range []string{"comparison_rows", "comparison_get"} {
		status, body := comparisonCall(t, s, op, map[string]any{"id": "cmp-1", "limit": 10}, false)
		encoded, _ := json.Marshal(body)
		if status != 200 || body["error"] != "Comparison results are unavailable on this node" || body["resumable"] != false || strings.Contains(string(encoded), directory) {
			t.Fatalf("%s: %d %s", op, status, encoded)
		}
		if items, _ := body["items"].([]any); len(items) != 0 {
			t.Fatalf("%s returned rows: %s", op, encoded)
		}
	}
	status, body := comparisonCall(t, s, "comparison_list", `{}`, false)
	if items, _ := body["items"].([]any); status != 200 || len(items) != 1 {
		t.Fatalf("list: %d %+v", status, body)
	}
	if status, body := comparisonCall(t, s, "comparison_resume", comparison.ResumeRequest{ID: "cmp-1", StorageRevision: 1}, true); status != 409 {
		t.Fatalf("resume of unavailable results: %d %+v", status, body)
	}
}

// removeOnceReleased removes a result file. The run commits its final state
// just before it closes the file, and Windows refuses to remove an open file,
// so the removal is retried until the writer has let go.
func removeOnceReleased(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := os.Remove(path)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
