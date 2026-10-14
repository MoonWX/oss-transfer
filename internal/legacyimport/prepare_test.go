package legacyimport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agent"
	"github.com/MoonWX/oss-transfer/internal/auth"
	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/storageconfig"
)

type preparationFixture struct {
	options PrepareOptions
	root    string
	secret  string
	master  map[string]any
	node    map[string]any
	worker  map[string]any
}

func prepareFixture(t *testing.T) *preparationFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX offline migration preparation; Windows must reject until ACL support is implemented")
	}
	root := t.TempDir()
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	credentialHash := sha256.Sum256([]byte(secret))
	salt := strings.Repeat("a1", 16)
	passwordHash := sha256.Sum256([]byte(salt + "existing password"))
	connection := map[string]any{"alias": "storage", "endpoint": "https://s3.example.test", "accessKey": "original-access", "secretKey": "original-secret", "pathStyleEnable": true, "useV2SignerType": false}
	f := &preparationFixture{root: root, secret: secret,
		master: map[string]any{
			"user:operator":     map[string]any{"username": "operator", "passwordHash": salt + ":" + hex.EncodeToString(passwordHash[:]), "role": "admin", "displayName": "Existing operator"},
			"user:reader":       map[string]any{"username": "reader", "passwordHash": salt + ":" + hex.EncodeToString(passwordHash[:]), "role": "user", "displayName": "Reader"},
			"node_config":       map[string]any{"localIp": "master", "workerIps": []string{"master", "node", "worker"}, "advertisedLocalRoots": []string{"/master-root"}},
			"agent:member:node": map[string]any{"instanceId": "existing-instance", "enrollmentId": "existing-enrollment", "address": "node", "label": "Existing node", "credentialHash": base64.RawURLEncoding.EncodeToString(credentialHash[:]), "enabled": true, "joinedAt": int64(1_700_000_000_000)},
			"s3_config_storage": connection,
			"task:10":           map[string]any{"taskIndex": 10, "status": "RUNNING", "marker": "not-a-completion", "startKey": "a", "endKey": "z"},
			"completed:10:a":    map[string]any{"taskId": 10, "logicalKey": "a", "size": 3},
			"unknown:retain":    map[string]any{"opaque": "must remain"},
		},
		node: map[string]any{
			"agent:identity":              map[string]any{"masterUrl": "http://old-master:1685", "address": "node", "credential": secret, "instanceId": "existing-instance", "joined": true},
			"node_config":                 map[string]any{"localIp": "node", "advertisedLocalRoots": []string{"/node-root"}},
			"s3_config_storage":           connection,
			"rate_limit:manual:bandwidth": 100_000_000,
		},
		worker: map[string]any{"node_config": map[string]any{"localIp": "worker"}, "s3_config_storage": connection},
	}
	f.options = PrepareOptions{Destination: filepath.Join(root, "migrated"), PublicURL: "https://new-master.example.test", DirectNodes: map[string]DirectNodeApproval{"master": {Address: "master-execution", Label: "Master execution"}, "worker": {Address: "worker", Label: "Direct worker"}}}
	f.write(t)
	return f
}

func (f *preparationFixture) write(t *testing.T) {
	t.Helper()
	f.options.SnapshotPaths = nil
	for _, entry := range []struct {
		node, role string
		values     map[string]any
	}{{"master", "master", f.master}, {"node", "agent", f.node}, {"worker", "worker", f.worker}} {
		path := filepath.Join(f.root, entry.node+".jsonl")
		if err := os.WriteFile(path, snapshot(t, entry.node, entry.role, entry.values), 0600); err != nil {
			t.Fatal(err)
		}
		f.options.SnapshotPaths = append(f.options.SnapshotPaths, path)
	}
}

func TestPreparePreservesAccountsAgentCredentialsAndRawHistory(t *testing.T) {
	f := prepareFixture(t)
	original := map[string][]byte{}
	for _, path := range f.options.SnapshotPaths {
		original[path], _ = os.ReadFile(path)
	}
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	if _, err := os.Stat(f.options.Destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Prepare published destination")
	}
	if !p.Manifest.Pending || p.Manifest.Accounts != 2 || p.Manifest.Connections != 1 || len(p.Agents) != 3 {
		t.Fatalf("unexpected preparation: %+v", p.Manifest)
	}
	var user importedUser
	if err := p.Master.View(func(tx *state.Tx) error { _, err := tx.Get("users", "operator", &user); return err }); err != nil {
		t.Fatal(err)
	}
	if user.Role != "admin" || user.DisplayName != "Existing operator" || user.Revision != 1 || !auth.VerifyPassword("existing password", user.PasswordHash) {
		t.Fatal("existing account was not preserved")
	}
	if err := p.Master.View(func(tx *state.Tx) error {
		var config struct {
			Configured bool
			PublicURL  string
		}
		if _, err := tx.Get("system", "configuration", &config); err != nil {
			return err
		}
		if !config.Configured || config.PublicURL != f.options.PublicURL {
			t.Fatal("setup state was not imported")
		}
		return tx.Scan("execution_jobs", "", "", 1, func(string, json.RawMessage) error { t.Fatal("legacy completion became runnable Go state"); return nil })
	}); err != nil {
		t.Fatal(err)
	}
	registered := cluster.New(p.Master, nil)
	session, err := registered.Authenticate(f.secret)
	if err != nil || session.NodeID != "existing-instance" || session.EnrollmentID != "existing-enrollment" {
		t.Fatalf("Go credential hash conversion failed: %+v %v", session, err)
	}
	for node, db := range p.Agents {
		var identity agent.Identity
		if err := db.View(func(tx *state.Tx) error { _, err := tx.Get("agent-identity", "self", &identity); return err }); err != nil {
			t.Fatal(err)
		}
		if identity.MasterURL != f.options.PublicURL || identity.PendingToken != "" {
			t.Fatal("Agent would re-enroll instead of keeping identity")
		}
		session, err := registered.Authenticate(identity.Credential)
		if err != nil || session.NodeID != identity.InstanceID || session.EnrollmentID != identity.EnrollmentID {
			t.Fatalf("node %s: %+v %v", node, session, err)
		}
		config, err := storageconfig.NewAgent(db).Snapshot()
		if err != nil || config.Revision != 1 || len(config.Connections) != 1 || config.Connections[0].Region != "default" || config.Connections[0].SecretKey != "original-secret" {
			t.Fatalf("storage config: %+v %v", config, err)
		}
		if err := db.ClaimRole("agent"); !errors.Is(err, state.ErrMigrationPending) {
			t.Fatalf("pending Agent started: %v", err)
		}
		if _, err := agent.New(db, agent.Options{}); !errors.Is(err, state.ErrMigrationPending) {
			t.Fatalf("Agent startup bypassed fence: %v", err)
		}
	}
	if err := p.Master.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
		t.Fatalf("pending Master started: %v", err)
	}
	var seenUnknown, seenCheckpoint bool
	if err := p.Iterate(context.Background(), "master", func(key, raw []byte) error {
		if string(key) == "unknown:retain" {
			seenUnknown = bytes.Contains(raw, []byte("must remain"))
		}
		if string(key) == "task:10" {
			seenCheckpoint = bytes.Contains(raw, []byte("not-a-completion"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !seenUnknown || !seenCheckpoint {
		t.Fatal("raw checkpoint/history not retained")
	}
	for _, source := range p.Manifest.Sources {
		before := original[filepath.Join(f.root, source.Node+".jsonl")]
		copied, err := os.ReadFile(filepath.Join(p.Directory, filepath.FromSlash(source.Path)))
		if err != nil || !bytes.Equal(before, copied) {
			t.Fatal("raw snapshot changed")
		}
		current, _ := os.ReadFile(filepath.Join(f.root, source.Node+".jsonl"))
		if !bytes.Equal(before, current) {
			t.Fatal("source snapshot mutated")
		}
	}
	if err := filepath.Walk(p.Directory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatalf("nonprivate output %s: %s", path, info.Mode())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := os.ReadFile(filepath.Join(p.Directory, "manifest.json"))
	if bytes.Contains(manifest, []byte(f.secret)) || bytes.Contains(manifest, []byte("original-secret")) || bytes.Contains(manifest, []byte(user.PasswordHash)) {
		t.Fatal("manifest exposed credentials")
	}
	if err := p.Finalize(context.Background()); err == nil {
		t.Fatal("incomplete conversion was publishable")
	}
	directory := p.Directory
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(filepath.Join(directory, "master", "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClaimRole("master"); !errors.Is(err, state.ErrMigrationPending) {
		t.Fatal("restart lost migration fence")
	}
}

func TestPrepareRejectsInvalidClusterWithoutLeavingPartialOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*preparationFixture)
	}{
		{"missing-node", func(f *preparationFixture) { f.options.SnapshotPaths = f.options.SnapshotPaths[:2] }},
		{"missing-direct-approval", func(f *preparationFixture) { delete(f.options.DirectNodes, "worker") }},
		{"credential-mismatch", func(f *preparationFixture) {
			f.node["agent:identity"].(map[string]any)["credential"] = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
			f.write(t)
		}},
		{"connection-drift", func(f *preparationFixture) {
			f.node["s3_config_storage"] = map[string]any{"alias": "storage", "endpoint": "https://elsewhere.test", "accessKey": "different", "secretKey": "secret"}
			f.write(t)
		}},
		{"unknown-role", func(f *preparationFixture) {
			f.master["user:operator"].(map[string]any)["role"] = "superuser"
			f.write(t)
		}},
		{"truncated", func(f *preparationFixture) {
			b, _ := os.ReadFile(f.options.SnapshotPaths[0])
			os.WriteFile(f.options.SnapshotPaths[0], b[:len(b)-20], 0600)
		}},
		{"destination-exists", func(f *preparationFixture) {
			os.Mkdir(f.options.Destination, 0700)
			os.WriteFile(filepath.Join(f.options.Destination, "keep"), []byte("untouched"), 0600)
		}},
		{"empty-destination-exists", func(f *preparationFixture) { os.Mkdir(f.options.Destination, 0700) }},
		{"unsafe-url", func(f *preparationFixture) { f.options.PublicURL = "https://name:secret@example.test" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := prepareFixture(t)
			test.mutate(f)
			p, err := Prepare(context.Background(), f.options)
			if err == nil {
				p.Close()
				t.Fatal("unsafe preparation accepted")
			}
			entries, _ := os.ReadDir(f.root)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".oss-transfer-migration-") {
					t.Fatal("failed preparation left partial output")
				}
			}
			if test.name == "destination-exists" {
				b, _ := os.ReadFile(filepath.Join(f.options.Destination, "keep"))
				if string(b) != "untouched" {
					t.Fatal("existing destination was modified")
				}
			}
		})
	}
}

func TestPrepareRetainsRevokedAgentWithoutReactivating(t *testing.T) {
	f := prepareFixture(t)
	f.master["agent:member:node"].(map[string]any)["enabled"] = false
	f.write(t)
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := cluster.New(p.Master, nil).Authenticate(f.secret); !errors.Is(err, cluster.ErrUnauthorized) {
		t.Fatal("revoked credential was restored")
	}
	found := false
	for _, member := range p.Manifest.Nodes {
		if member.LegacyNode == "node" {
			found = member.Revoked
		}
	}
	if !found {
		t.Fatal("revocation lost in identity mapping")
	}
}

func TestPrepareCancellationAndChangedPrivateCopy(t *testing.T) {
	f := prepareFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Prepare(ctx, f.options); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	p, err := Prepare(context.Background(), f.options)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	path := p.paths["master"]
	if err := os.WriteFile(path, []byte("truncated\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := p.Iterate(context.Background(), "master", func([]byte, []byte) error { calls++; return nil }); err == nil || calls != 0 {
		t.Fatalf("converter saw unvalidated bytes: calls=%d err=%v", calls, err)
	}
}
