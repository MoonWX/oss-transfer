package master

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/backupretention"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
)

// The updater itself is idle, but listing and deletion use real rollback
// directories. Agent calls travel through enrollment and the reverse channel.
type diskBackupUpdater struct {
	fakeSoftwareUpdater
	backups *backupretention.Manager
}

func (u *diskBackupUpdater) BackupStats(context.Context) (softwareupdate.BackupSummary, error) {
	listing, err := u.backups.List()
	result := softwareupdate.BackupSummary{Supported: true, Count: len(listing.Entries), Bytes: listing.TotalBytes, ReclaimableBytes: listing.ReclaimableBytes}
	for _, entry := range listing.Entries {
		if entry.Eligible {
			result.ReclaimableCount++
		}
	}
	return result, err
}
func (u *diskBackupUpdater) PruneBackups(context.Context) (softwareupdate.BackupCleanup, error) {
	result, err := u.backups.Prune(backupretention.Options{})
	return softwareupdate.BackupCleanup{RemovedCount: len(result.Removed), FreedBytes: result.FreedBytes}, err
}

func backupHTTPFixture(t *testing.T, role string) (*diskBackupUpdater, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "backups")
	id := backupretention.Identity{Role: role, ServiceName: "oss-test-" + role, Config: filepath.Join(base, "config", "service.json"), DataDir: filepath.Join(base, "data")}
	m, err := backupretention.New(root, id)
	if err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]string{"serviceName": id.ServiceName, "dataDirectory": id.DataDir})
	metadata := []byte(fmt.Sprintf("role\t%s\nserviceName\t%s\nconfig\t%s\ndataDir\t%s\nversion\t4.1.18\n", role, id.ServiceName, id.Config, id.DataDir))
	metadataName, binaryName := "install.tsv", "binary"
	if runtime.GOOS == "windows" {
		metadataName, binaryName = "install.json", "binary.exe"
		metadata, _ = json.Marshal(map[string]string{"role": role, "serviceName": id.ServiceName, "config": id.Config, "dataDir": id.DataDir, "version": "4.1.18"})
	}
	for _, name := range []string{"backup-1", "backup-2", "backup-3"} {
		dir := filepath.Join(root, name)
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for file, data := range map[string][]byte{metadataName: metadata, "service.json": config, binaryName: []byte("old binary"), "database": []byte("saved data")} {
			if err = os.WriteFile(filepath.Join(dir, file), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err = m.Complete(name, ""); err != nil {
			t.Fatal(err)
		}
	}
	return &diskBackupUpdater{backups: m}, root
}

func TestBackupHTTPRequiresAdminAndPrunesOnlySelectedNode(t *testing.T) {
	f := newAgentHTTPFixture(t)
	master, masterRoot := backupHTTPFixture(t, "master")
	f.master.options.Updater = master
	a, rootA := backupHTTPFixture(t, "agent")
	b, rootB := backupHTTPFixture(t, "agent")
	nodeA := nodeTestAgentWithUpdater(t, f, "backup-a", a)
	nodeTestAgentWithUpdater(t, f, "backup-b", b)
	legacy := nodeTestAgent(t, f, "backup-legacy")
	base := "/api/master/software-updates/backups"
	for _, path := range []string{base + "/master/prune", base + "/agents/" + nodeA.address + "/prune"} {
		if status, _ := request(t, f.http.Client(), "POST", f.http.URL+path, "", map[string]any{}); status != 401 {
			t.Fatal("unauth cleanup", status)
		}
	}
	if status, _ := request(t, f.http.Client(), "GET", f.http.URL+base, "", nil); status != 401 {
		t.Fatal("unauth listing", status)
	}
	status, body := request(t, f.http.Client(), "GET", f.http.URL+base, f.admin, nil)
	if status != 200 {
		t.Fatal(status, body)
	}
	data := body["data"].(map[string]any)
	if data["master"].(map[string]any)["reclaimableCount"] != float64(3) {
		t.Fatal(data)
	}
	if policy := data["policy"].(map[string]any); policy["keep"] != float64(0) || policy["maxAgeDays"] != float64(0) {
		t.Fatal("obsolete retention policy", policy)
	}
	if len(data["agents"].([]any)) != 3 {
		t.Fatal(data)
	}
	path := base + "/agents/" + nodeA.address + "/prune"
	if status, _ = request(t, f.http.Client(), "POST", f.http.URL+path, f.admin, map[string]any{"path": masterRoot}); status != 400 {
		t.Fatal("arbitrary path accepted", status)
	}
	status, body = request(t, f.http.Client(), "POST", f.http.URL+path, f.admin, map[string]any{})
	if status != 200 || body["data"].(map[string]any)["removedCount"] != float64(3) {
		t.Fatal(status, body)
	}
	if _, err := os.Stat(filepath.Join(rootA, "backup-1")); !os.IsNotExist(err) {
		t.Fatal("selected old backup not removed", err)
	}
	for _, root := range []string{rootB, masterRoot} {
		if _, err := os.Stat(filepath.Join(root, "backup-3")); err != nil {
			t.Fatal("other node backup lost", root, err)
		}
	}
	for _, root := range []string{rootB, masterRoot} {
		if _, err := os.Stat(filepath.Join(root, "backup-1")); err != nil {
			t.Fatal("other node was pruned", root, err)
		}
	}
	if status, _ = request(t, f.http.Client(), "POST", f.http.URL+base+"/agents/"+legacy.address+"/prune", f.admin, map[string]any{}); status != 409 {
		t.Fatal("unsupported agent", status)
	}
	if status, _ = request(t, f.http.Client(), "POST", f.http.URL+base+"/agents/not-registered/prune", f.admin, map[string]any{}); status != 404 {
		t.Fatal("unregistered node", status)
	}
	member, err := f.master.nodeByAddress(nodeA.address)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.master.agentCall(context.Background(), member.Session(), "backup_prune", false, struct{}{}, nil); !agentStatus(err, 400) {
		t.Fatal("read-only command pruned backups", err)
	}
}
