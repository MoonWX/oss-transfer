// Package cluster owns Agent enrollment. Credentials are scoped to one enrollment.
package cluster

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MoonWX/oss-transfer/internal/agenttransport"
	"github.com/MoonWX/oss-transfer/internal/state"
)

var ErrUnauthorized = errors.New("invalid or revoked Agent credential")
var ErrInvitation = errors.New("invitation is invalid, expired or already used")
var ErrConflict = errors.New("Agent identity or address is already registered")

type Member struct {
	ID             string    `json:"id"`
	EnrollmentID   string    `json:"enrollmentId"`
	Address        string    `json:"address"`
	Label          string    `json:"label"`
	CredentialHash string    `json:"credentialHash,omitempty"`
	Revoked        bool      `json:"revoked"`
	JoinedAt       time.Time `json:"joinedAt"`
}

func (m Member) Session() agenttransport.Session {
	return agenttransport.Session{NodeID: m.ID, EnrollmentID: m.EnrollmentID}
}

type invitation struct {
	Address      string    `json:"address"`
	Label        string    `json:"label"`
	MasterURL    string    `json:"masterUrl,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
	MemberID     string    `json:"memberId,omitempty"`
	EnrollmentID string    `json:"enrollmentId,omitempty"`
}
type Invitation struct {
	Token     string    `json:"token"`
	MasterURL string    `json:"masterUrl,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}
type JoinRequest struct {
	Token      string `json:"token"`
	InstanceID string `json:"instanceId"`
	Credential string `json:"credential"`
	Address    string `json:"address"`
	Label      string `json:"label"`
}
type Registry struct {
	store *state.Store
	now   func() time.Time
}

func New(store *state.Store, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{store, now}
}
func Token() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b), err
}
func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func safeLabel(value string, max int) bool {
	return len(value) <= max && !strings.ContainsAny(value, "\x00\r\n")
}
func secret(value string) bool {
	b, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(b) == 32
}

// Public node keys also appear in CLI arguments, URL paths and comma-separated
// task node selections. Display names do not have these restrictions.
func safeAutomaticAddress(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.HasPrefix(value, "-") {
		return false
	}
	if ip := net.ParseIP(value); ip != nil {
		return !ip.IsUnspecified() && !ip.IsMulticast()
	}
	onlyNumbers := true
	for _, char := range value {
		if char != '.' && (char < '0' || char > '9') {
			onlyNumbers = false
		}
		if !unicode.IsLetter(char) && !unicode.IsDigit(char) && char != '-' && char != '_' && char != '.' {
			return false
		}
	}
	return !onlyNumbers
}

func (r *Registry) Invite(address, label string) (Invitation, error) {
	return r.InviteForMaster(address, label, "")
}

// InviteForMaster binds this invitation to the Agent's enrollment endpoint.
// Later changes to cluster defaults cannot change an already copied command.
func (r *Registry) InviteForMaster(address, label, masterURL string) (Invitation, error) {
	if !safeLabel(address, 256) || !safeLabel(label, 128) {
		return Invitation{}, errors.New("invalid node address or label")
	}
	token, err := Token()
	if err != nil {
		return Invitation{}, err
	}
	expires := r.now().Add(30 * time.Minute)
	err = r.store.Update(func(tx *state.Tx) error {
		return tx.Put("agent-invitations", digest(token), invitation{Address: address, Label: label, MasterURL: masterURL, ExpiresAt: expires})
	})
	return Invitation{Token: token, MasterURL: masterURL, ExpiresAt: expires}, err
}

// CheckInvitation authorizes fetching an installer without consuming the
// invitation. Only a successfully enrolled identity consumes it in Join.
func (r *Registry) CheckInvitation(token string) error {
	_, err := r.Invitation(token)
	return err
}

// Invitation reads an unconsumed invitation without consuming its credential.
func (r *Registry) Invitation(token string) (Invitation, error) {
	if !secret(token) {
		return Invitation{}, ErrInvitation
	}
	var result Invitation
	err := r.store.View(func(tx *state.Tx) error {
		var invite invitation
		found, err := tx.Get("agent-invitations", digest(token), &invite)
		if err != nil {
			return err
		}
		if !found || invite.MemberID != "" || !r.now().Before(invite.ExpiresAt) {
			return ErrInvitation
		}
		result = Invitation{Token: token, MasterURL: invite.MasterURL, ExpiresAt: invite.ExpiresAt}
		return nil
	})
	return result, err
}

// Join commits invitation consumption and membership together. If the response
// is lost, only the same persisted Agent identity can repeat the request.
func (r *Registry) Join(request JoinRequest) (Member, error) {
	if !secret(request.Token) || !secret(request.Credential) || request.InstanceID == "" || !safeLabel(request.InstanceID, 128) || !safeLabel(request.Address, 256) || !safeLabel(request.Label, 128) {
		return Member{}, ErrInvitation
	}
	enrollment, err := Token()
	if err != nil {
		return Member{}, err
	}
	var member Member
	err = r.store.Update(func(tx *state.Tx) error {
		var invite invitation
		found, err := tx.Get("agent-invitations", digest(request.Token), &invite)
		if err != nil {
			return err
		}
		if !found {
			return ErrInvitation
		}
		if invite.MemberID != "" {
			found, err = tx.Get("agent-members", invite.MemberID, &member)
			if err != nil {
				return err
			}
			if !found || member.Revoked || member.ID != request.InstanceID || member.EnrollmentID != invite.EnrollmentID || member.CredentialHash != digest(request.Credential) {
				return ErrInvitation
			}
			return nil
		}
		if !r.now().Before(invite.ExpiresAt) {
			return ErrInvitation
		}
		var existing Member
		found, err = tx.Get("agent-members", request.InstanceID, &existing)
		if err != nil {
			return err
		}
		if found && !existing.Revoked {
			return ErrConflict
		}
		if found && existing.Revoked {
			// Preserve older databases' revocation before a fresh enrollment
			// replaces the current member record for this instance ID.
			if err := tx.Put("agent-retired-enrollments", retiredKey(existing.Session()), existing.Session()); err != nil {
				return err
			}
		}
		var credentialOwner string
		found, err = tx.Get("agent-credentials", digest(request.Credential), &credentialOwner)
		if err != nil {
			return err
		}
		if found {
			return ErrConflict
		}
		address := request.Address
		if invite.Address != "" {
			address = invite.Address
		}
		label := request.Label
		if invite.Label != "" {
			label = invite.Label
		}
		automaticAddress := address == ""
		// Keep the opaque instance ID internal. The stable public node key
		// defaults to its chosen name; presentation can still be renamed later.
		if automaticAddress && safeAutomaticAddress(label) {
			address = label
		}
		if address == "" {
			address = "agent-" + digest(request.InstanceID)[:8]
		}
		var addressOwner string
		found, err = tx.Get("agent-addresses", address, &addressOwner)
		if err != nil {
			return err
		}
		if found {
			if !automaticAddress {
				return ErrConflict
			}
			address += "-" + digest(request.InstanceID)[:8]
			found, err = tx.Get("agent-addresses", address, &addressOwner)
			if err != nil {
				return err
			}
			if found {
				return ErrConflict
			}
		}
		if label == "" {
			label = address
		}
		member = Member{ID: request.InstanceID, EnrollmentID: enrollment, Address: address, Label: label, CredentialHash: digest(request.Credential), JoinedAt: r.now().UTC()}
		invite.MemberID = member.ID
		invite.EnrollmentID = member.EnrollmentID
		for _, entry := range []struct {
			bucket, key string
			value       any
		}{
			{"agent-members", member.ID, member}, {"agent-credentials", member.CredentialHash, member.ID}, {"agent-addresses", address, member.ID}, {"agent-invitations", digest(request.Token), invite},
		} {
			if err := tx.Put(entry.bucket, entry.key, entry.value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Member{}, err
	}
	member.CredentialHash = ""
	return member, nil
}
func (r *Registry) Authenticate(credential string) (agenttransport.Session, error) {
	if !secret(credential) {
		return agenttransport.Session{}, ErrUnauthorized
	}
	var session agenttransport.Session
	err := r.store.View(func(tx *state.Tx) error {
		var id string
		found, err := tx.Get("agent-credentials", digest(credential), &id)
		if err != nil {
			return err
		}
		if !found {
			return ErrUnauthorized
		}
		var member Member
		found, err = tx.Get("agent-members", id, &member)
		if err != nil {
			return err
		}
		if !found || member.Revoked || member.CredentialHash != digest(credential) {
			return ErrUnauthorized
		}
		session = member.Session()
		return nil
	})
	return session, err
}
func (r *Registry) Revoke(id string) (agenttransport.Session, error) {
	var session agenttransport.Session
	err := r.store.Update(func(tx *state.Tx) error {
		var member Member
		found, err := tx.Get("agent-members", id, &member)
		if err != nil {
			return err
		}
		if !found {
			return ErrUnauthorized
		}
		session = member.Session()
		member.Revoked = true
		if err := tx.Put("agent-members", id, member); err != nil {
			return err
		}
		if err := tx.Put("agent-retired-enrollments", retiredKey(session), session); err != nil {
			return err
		}
		// Keep the credential index as a tombstone. Re-enrollment must use a
		// fresh secret; otherwise revoking a leaked credential could reactivate it.
		return tx.Delete("agent-addresses", member.Address)
	})
	return session, err
}

func retiredKey(session agenttransport.Session) string {
	return digest(session.NodeID + "\x00" + session.EnrollmentID)
}

// IsRevoked reports only a durable revocation of this exact enrollment. An
// unknown, offline or replaced node must not be assumed safely retired.
func (r *Registry) IsRevoked(session agenttransport.Session) (bool, error) {
	if session.NodeID == "" || session.EnrollmentID == "" {
		return false, nil
	}
	var retired bool
	err := r.store.View(func(tx *state.Tx) error {
		var recorded agenttransport.Session
		found, err := tx.Get("agent-retired-enrollments", retiredKey(session), &recorded)
		if err != nil {
			return err
		}
		if found {
			retired = recorded == session
			return nil
		}
		// Compatibility with revocations persisted before the enrollment
		// tombstone existed. Join preserves it before replacing this row.
		var member Member
		found, err = tx.Get("agent-members", session.NodeID, &member)
		retired = found && member.Revoked && member.Session() == session
		return err
	})
	return retired, err
}
func (r *Registry) Members() ([]Member, error) {
	members := make([]Member, 0)
	after := ""
	for {
		count := 0
		err := r.store.View(func(tx *state.Tx) error {
			return tx.Scan("agent-members", "", after, 256, func(key string, data json.RawMessage) error {
				count++
				after = key
				var member Member
				if err := json.Unmarshal(data, &member); err != nil {
					return err
				}
				if !member.Revoked {
					member.CredentialHash = ""
					members = append(members, member)
				}
				return nil
			})
		})
		if err != nil {
			return nil, err
		}
		if count < 256 {
			return members, nil
		}
	}
}

// UpdateLabel changes presentation only; enrollment, address and credentials stay fixed.
func (r *Registry) UpdateLabel(id, label string) (Member, error) {
	var member Member
	if id == "" || !safeLabel(label, 128) {
		return member, errors.New("invalid node label")
	}
	err := r.store.Update(func(tx *state.Tx) error {
		found, err := tx.Get("agent-members", id, &member)
		if err != nil {
			return err
		}
		if !found || member.Revoked {
			return ErrUnauthorized
		}
		member.Label = label
		return tx.Put("agent-members", id, member)
	})
	return member, err
}
