package cluster

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestEnrollmentResponseLossRestartAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := New(db, func() time.Time { return now })
	invite, err := r.Invite("node-a", "Agent A")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	request := JoinRequest{Token: invite.Token, Credential: credential, InstanceID: "instance-a"}
	member, err := r.Join(request)
	if err != nil {
		t.Fatal(err)
	}
	if member.CredentialHash != "" {
		t.Fatal("credential hash exposed")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r = New(db, func() time.Time { return now })
	now = now.Add(time.Hour)
	replay, err := r.Join(request)
	if err != nil || replay.EnrollmentID != member.EnrollmentID {
		t.Fatal("lost enrollment response cannot be retried", err)
	}
	if _, err := r.Authenticate(credential); err != nil {
		t.Fatal(err)
	}
	stolen := request
	stolen.InstanceID = "another-node"
	if _, err := r.Join(stolen); !errors.Is(err, ErrInvitation) {
		t.Fatal("invitation reused by another instance", err)
	}
	if _, err := r.Revoke(member.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Authenticate(credential); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("revoked credential accepted", err)
	}
	if _, err := r.Join(request); !errors.Is(err, ErrInvitation) {
		t.Fatal("revoked enrollment resurrected", err)
	}
	invite, err = r.Invite("node-a", "Replacement")
	if err != nil {
		t.Fatal(err)
	}
	request.Token = invite.Token
	if _, err := r.Join(request); !errors.Is(err, ErrConflict) {
		t.Fatal("revoked credential was reused for a new enrollment", err)
	}
	request.Credential, err = Token()
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := r.Join(request)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.EnrollmentID == member.EnrollmentID {
		t.Fatal("replacement reused the old enrollment")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r = New(db, nil)
	if retired, err := r.IsRevoked(member.Session()); err != nil || !retired {
		t.Fatal("old enrollment revocation lost after replacement and restart", retired, err)
	}
	for _, session := range []agenttransport.Session{replacement.Session(), {NodeID: "missing", EnrollmentID: "missing"}, {NodeID: member.ID, EnrollmentID: "other"}, {}} {
		if retired, err := r.IsRevoked(session); err != nil || retired {
			t.Fatal("live or unknown enrollment treated as revoked", session, retired, err)
		}
	}
	if _, err := r.Authenticate(credential); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old credential revived after re-enrollment and restart", err)
	}
	if got, err := r.Authenticate(request.Credential); err != nil || got != replacement.Session() {
		t.Fatal("replacement credential damaged", got, err)
	}
}

func TestReenrollmentPreservesRevocationFromOlderRegistry(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := New(db, nil)
	old := Member{ID: "old-instance", EnrollmentID: "old-enrollment", Revoked: true}
	if err := db.Update(func(tx *state.Tx) error { return tx.Put("agent-members", old.ID, old) }); err != nil {
		t.Fatal(err)
	}
	if retired, err := r.IsRevoked(old.Session()); err != nil || !retired {
		t.Fatal("pre-tombstone revocation not recognized", retired, err)
	}
	invite, err := r.Invite("node", "")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := Token()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Join(JoinRequest{InstanceID: old.ID, Token: invite.Token, Credential: credential}); err != nil {
		t.Fatal(err)
	}
	if retired, err := r.IsRevoked(old.Session()); err != nil || !retired {
		t.Fatal("pre-tombstone revocation overwritten by new enrollment", retired, err)
	}
}
func TestInvitationConsumptionIsAtomic(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := New(db, nil)
	invite, err := r.Invite("", "")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			credential, err := Token()
			if err == nil {
				_, err = r.Join(JoinRequest{InstanceID: id, Credential: credential, Token: invite.Token})
			}
			results <- err
		}(id)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrInvitation) {
			t.Fatal(err)
		}
	}
	members, err := r.Members()
	if err != nil {
		t.Fatal(err)
	}
	if success != 1 || len(members) != 1 {
		t.Fatal("invitation created multiple members", success, len(members))
	}
}
