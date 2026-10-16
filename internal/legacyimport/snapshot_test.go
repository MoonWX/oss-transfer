package legacyimport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/auth"
)

func snapshot(t *testing.T, node, role string, entries map[string]any) []byte {
	t.Helper()
	var b bytes.Buffer
	encode := func(value any) {
		t.Helper()
		if err := json.NewEncoder(&b).Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	encode(Header{Type: "header", SchemaVersion: 1, Node: node, Role: role, LegacyJarSHA256: strings.Repeat("a", 64), RocksDBJNISHA256: strings.Repeat("b", 64), ExportedAt: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)})
	var keys []string
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var keyBytes, valueBytes int64
	for _, k := range keys {
		v, err := json.Marshal(entries[k])
		if err != nil {
			t.Fatal(err)
		}
		keyBytes += int64(len(k))
		valueBytes += int64(len(v))
		encode(map[string]string{"type": "entry", "key": base64.RawURLEncoding.EncodeToString([]byte(k)), "value": base64.RawURLEncoding.EncodeToString(v)})
	}
	h := sha256.Sum256(b.Bytes())
	encode(Footer{Type: "footer", Entries: int64(len(keys)), KeyBytes: keyBytes, ValueBytes: valueBytes, SHA256: hex.EncodeToString(h[:])})
	return b.Bytes()
}

func inspect(t *testing.T, input []byte) Plan {
	t.Helper()
	p, err := Inspect(context.Background(), bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func hasIssue(issues []Issue, code string) bool {
	for _, i := range issues {
		if i.Code == code {
			return true
		}
	}
	return false
}

func TestPlanPreservesRecoveryBoundariesAndHidesSecrets(t *testing.T) {
	password := " spaces preserved "
	salt := strings.Repeat("ab", 16)
	hash := sha256.Sum256([]byte(salt + password))
	encodedHash := salt + ":" + hex.EncodeToString(hash[:])
	if !auth.VerifyPassword(password, encodedHash) {
		t.Fatal("actual Go login verifier cannot read fixture hash")
	}
	data := snapshot(t, "master", "master", map[string]any{
		"user:operator":                map[string]any{"username": "operator", "passwordHash": encodedHash, "role": "user"},
		"s3_config_store":              map[string]any{"alias": "store", "endpoint": "https://storage.invalid", "accessKey": "private-ak", "secretKey": "private-sk", "useV2SignerType": false, "pathStyleEnable": true},
		"node_config":                  map[string]any{"localIp": "master", "workerIps": []string{"master", "agent"}},
		"task:100":                     map[string]any{"taskIndex": 100, "status": "STOPPED", "marker": "middle", "startKey": "a", "endKey": "z", "inventoryManifestId": "missing"},
		"task:deleted:-123":            map[string]any{"taskIndex": -123, "status": "PARTIAL_FAILED", "parentTaskId": 100},
		"local:explore:100":            map[string]any{"marker": "middle", "pendingTasks": []string{"pending"}},
		"completed:100:a":              map[string]any{"logicalKey": "k", "size": 1},
		"file-report:100:coverage":     map[string]any{"ledgerCoverage": "LEGACY_UNKNOWN"},
		"local-temp:id":                map[string]any{"temp": "/data/.transfer.part", "stage": "OPEN"},
		"task_bandwidth:desired:scope": map[string]any{"limitMbps": 100, "members": []string{"master", "agent"}},
		"task:purged:99":               true,
		"future:retained":              map[string]any{"@type": "not.loaded", "value": "not discarded"},
	})
	before := append([]byte(nil), data...)
	p := inspect(t, data)
	if !bytes.Equal(data, before) {
		t.Fatal("inspection mutated input")
	}
	if p.ReadyToImport || len(p.Tasks) != 2 || p.Tasks[0].Status != "STOPPED" || !p.Tasks[1].Deleted {
		t.Fatalf("lost execution/retention boundary: %+v", p.Tasks)
	}
	if p.Users[0].Role != "user" || p.Users[0].HashKind != "V4_SHA256" {
		t.Fatal("account mapping changed role")
	}
	if p.Connections[0].Region != "default" || p.Connections[0].Signature != "v4" {
		t.Fatal("legacy signing region changed")
	}
	for _, code := range []string{"INSPECTION_ONLY", "SPLIT_MASTER_EXECUTION", "INVENTORY_NOT_READY", "UNKNOWN_KEYS_RETAINED", "LEGACY_COMPLETION_NEEDS_RECONCILIATION", "OLD_TEMPORARY_FILES", "SHARED_BANDWIDTH_SCOPE"} {
		if !hasIssue(p.Issues, code) {
			t.Fatalf("missing constraint %s", code)
		}
	}
	result, _ := json.Marshal(p)
	for _, secret := range []string{encodedHash, "private-ak", "private-sk"} {
		if bytes.Contains(result, []byte(secret)) {
			t.Fatal("plan exposed secret")
		}
	}
	if p.Categories["purge-fences"] != 1 || p.Categories["discovery"] != 1 {
		t.Fatal("checkpoint categories missing")
	}
}

func TestCorruptSnapshotsNeverReturnPartialPlan(t *testing.T) {
	valid := snapshot(t, "m", "master", map[string]any{"node_config": map[string]any{"localIp": "m"}, "future:a": "a", "future:b": "b"})
	lines := bytes.Split(bytes.TrimSuffix(valid, []byte{'\n'}), []byte{'\n'})
	copyLines := func() [][]byte {
		r := make([][]byte, len(lines))
		for i, l := range lines {
			r[i] = append([]byte(nil), l...)
		}
		return r
	}
	join := func(l [][]byte) []byte { return append(bytes.Join(l, []byte{'\n'}), '\n') }
	truncated := copyLines()
	truncated = truncated[:len(truncated)-1]
	removed := copyLines()
	removed = append(removed[:1], removed[2:]...)
	duplicate := copyLines()
	duplicate = append(duplicate[:2], append([][]byte{duplicate[1]}, duplicate[2:]...)...)
	swapped := copyLines()
	swapped[1], swapped[2] = swapped[2], swapped[1]
	wrongType := bytes.Replace(valid, []byte(`"schemaVersion":1`), []byte(`"schemaVersion":2`), 1)
	for name, input := range map[string][]byte{
		"truncated": join(truncated), "removed_entry": join(removed), "duplicate_entry": join(duplicate), "reordered_entry": join(swapped),
		"footer_no_newline": bytes.TrimSuffix(valid, []byte{'\n'}), "trailing": append(append([]byte(nil), valid...), []byte("{}\n")...),
		"wrong_schema": wrongType, "duplicate_field": bytes.Replace(valid, []byte(`"type":"header"`), []byte(`"type":"header","type":"header"`), 1),
		"changed_value": bytes.Replace(valid, []byte(`"value":"ImEi"`), []byte(`"value":"ImMi"`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			p, err := Inspect(context.Background(), bytes.NewReader(input))
			if err == nil {
				t.Fatal("corrupt snapshot accepted")
			}
			if p.Header.Node != "" || len(p.Tasks) > 0 {
				t.Fatal("returned a partial plan")
			}
		})
	}
}

func TestClusterCoverageAndConflictingConnections(t *testing.T) {
	connection := func(secret string) any {
		return map[string]any{"alias": "s", "endpoint": "http://127.0.0.1:9000", "accessKey": "a", "secretKey": secret}
	}
	m := inspect(t, snapshot(t, "m", "master", map[string]any{"node_config": map[string]any{"localIp": "m", "workerIps": []string{"m", "a", "missing"}}, "s3_config_s": connection("one")}))
	a := inspect(t, snapshot(t, "a", "worker", map[string]any{"s3_config_s": connection("two")}))
	combined, err := Combine([]Plan{m, a})
	if err != nil {
		t.Fatal(err)
	}
	if combined.ReadyToImport || !hasIssue(combined.Issues, "NODE_SNAPSHOT_MISSING") || !hasIssue(combined.Issues, "CONNECTION_DRIFT") {
		t.Fatalf("unsafe cluster plan: %+v", combined)
	}
	if _, err := Combine([]Plan{m, m}); err == nil {
		t.Fatal("duplicate node allowed")
	}
	if _, err := Combine([]Plan{a}); err == nil {
		t.Fatal("missing Master allowed")
	}
	other := m
	other.Header.Node = "different"
	if _, err := Combine([]Plan{m, other}); err == nil {
		t.Fatal("two Masters allowed")
	}
}

func TestInventoryReadyAndCancellation(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			p := inspect(t, snapshot(t, "m", "master", map[string]any{"task:1": map[string]any{"taskIndex": 1, "status": "STOPPED", "inventoryManifestId": "id"}, "inventory:id:ready": ready}))
			if hasIssue(p.Issues, "INVENTORY_NOT_READY") == ready {
				t.Fatal("inventory readiness lost")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Inspect(ctx, bytes.NewReader(snapshot(t, "m", "master", nil))); err != context.Canceled {
		t.Fatal(err)
	}
}
