package agentservice

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

func fingerprintService(t *testing.T) *Service {
	t.Helper()
	db, err := state.Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.ClaimRole("agent"); err != nil {
		t.Fatal(err)
	}
	s, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s
}

func execute(t *testing.T, s *Service, operation string, payload any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	response, err := s.Execute(context.Background(), agenttransport.Command{Request: agenttransport.Request{Operation: operation, Mutating: operation != "shared_fingerprint_sample", Payload: raw}})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(response.Payload, &body)
	return response.Status, body
}

func TestSharedFingerprintOperations(t *testing.T) {
	s := fingerprintService(t)
	a, b := t.TempDir(), t.TempDir()
	for _, root := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(root, "data.txt"), []byte("same"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if capabilities := s.LeaseCapabilities(); capabilities["sharedProofProtocol"] != SharedProofProtocol {
		t.Fatal("the Agent does not advertise its shared proof operations", capabilities)
	}
	status, body := execute(t, s, "shared_fingerprint_sample", SharedFingerprintSampleRequest{Root: a})
	if status != 200 {
		t.Fatal(status, body)
	}
	raw, _ := json.Marshal(body["fingerprints"])
	var sample []transferstore.SharedFingerprint
	if err := json.Unmarshal(raw, &sample); err != nil || len(sample) != 1 {
		t.Fatal(sample, err)
	}
	// A check without a task records nothing; with one it grants the proof.
	if status, body := execute(t, s, "shared_fingerprint_verify", SharedFingerprintVerifyRequest{Root: b, Fingerprints: sample}); status != 200 {
		t.Fatal(status, body)
	}
	if status, body := execute(t, s, "shared_fingerprint_verify", SharedFingerprintVerifyRequest{ParentID: 7, Generation: 1, Root: b, Fingerprints: sample}); status != 200 || !s.Shared.Verified(7, 1, b) {
		t.Fatal("verified fingerprints did not grant the proof", status, body)
	}
	if err := os.WriteFile(filepath.Join(b, "data.txt"), []byte("edited, longer"), 0600); err != nil {
		t.Fatal(err)
	}
	status, body = execute(t, s, "shared_fingerprint_verify", SharedFingerprintVerifyRequest{ParentID: 7, Generation: 1, Root: b, Fingerprints: sample})
	differences, _ := body["differences"].([]any)
	if status != 409 || body["code"] != SharedFingerprintMismatchCode || s.Shared.Verified(7, 1, b) || len(differences) != 1 {
		t.Fatal("a changed file kept the proof or lost its code", status, body)
	}
	if difference := differences[0].(map[string]any); difference["status"] != transferstore.SharedFingerprintChanged || difference["path"] != "data.txt" || difference["size"] != float64(len("edited, longer")) || difference["sha256"] == "" {
		t.Fatal("the difference does not carry what this Agent read", difference)
	}
	if err := os.Remove(filepath.Join(b, "data.txt")); err != nil {
		t.Fatal(err)
	}
	status, body = execute(t, s, "shared_fingerprint_verify", SharedFingerprintVerifyRequest{Root: b, Fingerprints: sample})
	if differences, _ := body["differences"].([]any); status != 409 || len(differences) != 1 || differences[0].(map[string]any)["status"] != transferstore.SharedFingerprintMissing {
		t.Fatal("a missing file was not reported as missing", status, body)
	}
	escape := []transferstore.SharedFingerprint{{Path: "../data.txt", Size: 4, SHA256: sample[0].SHA256}}
	for _, test := range []struct {
		operation string
		payload   any
	}{
		{"shared_fingerprint_sample", SharedFingerprintSampleRequest{Root: "relative"}},
		{"shared_fingerprint_sample", SharedFingerprintSampleRequest{Root: a, Limit: transferstore.SharedFingerprintMaxFiles + 1}},
		{"shared_fingerprint_verify", SharedFingerprintVerifyRequest{Root: b, Fingerprints: escape}},
		{"shared_fingerprint_verify", SharedFingerprintVerifyRequest{Root: "relative", Fingerprints: sample}},
		{"shared_fingerprint_verify", SharedFingerprintVerifyRequest{ParentID: 7, Root: b, Fingerprints: sample}},
		{"shared_proof_skip", SharedProofSkipRequest{ParentID: 7, Root: b}},
		{"shared_fingerprint_verify", map[string]any{"root": b, "fingerprints": sample, "unexpected": true}},
	} {
		if status, body := execute(t, s, test.operation, test.payload); status != 400 {
			t.Fatalf("%s %+v was not rejected: %d %v", test.operation, test.payload, status, body)
		}
	}
	if status, body := execute(t, s, "shared_proof_skip", SharedProofSkipRequest{ParentID: 8, Generation: 2, Root: a}); status != 200 || !s.Shared.Verified(8, 2, a) || body["granted"] != true {
		t.Fatal("skipped check did not grant the proof", status, body)
	}
	// The Master logs only grants to an Agent process that held no proof.
	if status, body := execute(t, s, "shared_proof_skip", SharedProofSkipRequest{ParentID: 8, Generation: 2, Root: a}); status != 200 || body["granted"] != false {
		t.Fatal("a repeated skip was reported as a new grant", status, body)
	}
	s.Shared.Forget(8, 0)
	if status, _ := execute(t, s, "shared_proof_skip", SharedProofSkipRequest{ParentID: 8, Generation: 1, Root: a}); status != 409 {
		t.Fatal("a skip for an older generation passed the fence")
	}
}

func TestSharedWriteResponseMarksUnwritableRoots(t *testing.T) {
	for _, test := range []struct {
		err  error
		code string
	}{
		{&fs.PathError{Op: "open", Path: "/x", Err: syscall.EROFS}, UnwritableStorageCode},
		{&fs.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}, UnwritableStorageCode},
		{&fs.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}, UnwritableStorageCode},
		{&fs.PathError{Op: "open", Path: "/x", Err: syscall.EIO}, "operation_failed"},
	} {
		response, err := sharedWriteResponse(nil, test.err)
		var body map[string]string
		if err != nil || json.Unmarshal(response.Payload, &body) != nil || body["code"] != test.code || !strings.Contains(body["message"], "/x") {
			t.Fatalf("%v: %d %s", test.err, response.Status, response.Payload)
		}
	}
}
