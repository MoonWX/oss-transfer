package master

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
)

const localCLIPrefix = "otcli1."

type localCLIClaims struct {
	Role         string `json:"role"`
	NodeID       string `json:"nodeId,omitempty"`
	EnrollmentID string `json:"enrollmentId,omitempty"`
}

func (s *Server) initializeLocalCLI() error {
	if !s.options.LocalCLIEnabled {
		return nil
	}
	return s.store.Update(func(tx *state.Tx) error {
		var key []byte
		found, err := tx.Get("system", "local-cli-signing-key", &key)
		if err != nil {
			return err
		}
		if !found {
			key = make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return err
			}
			if err := tx.Put("system", "local-cli-signing-key", key); err != nil {
				return err
			}
		}
		if len(key) != 32 {
			return errors.New("invalid local CLI signing key")
		}
		s.localCLIKey = key
		return nil
	})
}

// LocalCLIToken is published only in an owner-readable file by the service.
// The key stays in the Master database, so Agent enrollment grants remain valid
// across restarts without creating a new database session for every command.
func (s *Server) LocalCLIToken() string {
	return s.signLocalCLI(localCLIClaims{Role: "master"})
}

func (s *Server) signLocalCLI(claims localCLIClaims) string {
	if len(s.localCLIKey) != 32 {
		return ""
	}
	raw, _ := json.Marshal(claims)
	payload := localCLIPrefix + base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.localCLIKey)
	_, _ = mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Server) localCLIUser(token string) (User, bool, error) {
	if len(s.localCLIKey) != 32 || len(token) > 2048 || !strings.HasPrefix(token, localCLIPrefix) {
		return User{}, false, nil
	}
	payload, signature, ok := strings.Cut(strings.TrimPrefix(token, localCLIPrefix), ".")
	if !ok {
		return User{}, false, nil
	}
	supplied, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return User{}, false, nil
	}
	mac := hmac.New(sha256.New, s.localCLIKey)
	_, _ = mac.Write([]byte(localCLIPrefix + payload))
	if !hmac.Equal(supplied, mac.Sum(nil)) {
		return User{}, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return User{}, false, nil
	}
	var claims localCLIClaims
	if json.Unmarshal(raw, &claims) != nil {
		return User{}, false, nil
	}
	user := User{Username: "local:master", DisplayName: "Master", Role: "admin", Authentication: "local", LocalCLI: true}
	if claims.Role == "master" && claims.NodeID == "" && claims.EnrollmentID == "" {
		return user, true, nil
	}
	if claims.Role != "agent" || claims.NodeID == "" || claims.EnrollmentID == "" {
		return User{}, false, nil
	}
	valid := false
	err = s.store.View(func(tx *state.Tx) error {
		var member cluster.Member
		found, err := tx.Get("agent-members", claims.NodeID, &member)
		if err != nil {
			return err
		}
		// IsRevoked deliberately treats unknown enrollments as not retired for
		// task takeover. Administrative access requires positive membership.
		valid = found && !member.Revoked && member.EnrollmentID == claims.EnrollmentID
		if valid {
			user.Username = "local:agent:" + member.ID
			user.DisplayName = member.Label
		}
		return nil
	})
	return user, valid, err
}

// cliAccess issues a separate administrative credential to an enrolled Agent.
// Enabling local CLI access explicitly trusts that Agent's OS administrator;
// the Agent key itself is never accepted by the administrator API.
func (s *Server) cliAccess(w http.ResponseWriter, r *http.Request) {
	if len(s.localCLIKey) != 32 {
		fail(w, http.StatusNotFound, "Unknown API endpoint")
		return
	}
	member, err := s.authenticateAgent(r)
	if err != nil {
		fail(w, http.StatusUnauthorized, "Invalid Agent credential")
		return
	}
	token := s.signLocalCLI(localCLIClaims{Role: "agent", NodeID: member.NodeID, EnrollmentID: member.EnrollmentID})
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "token": token})
}
