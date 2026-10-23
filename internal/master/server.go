package master

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/MoonWX/oss-transfer/internal/auth"
	"github.com/MoonWX/oss-transfer/internal/softwareupdate"
	"github.com/MoonWX/oss-transfer/internal/state"
)

type Configuration struct {
	Configured bool      `json:"configured"`
	PublicURL  string    `json:"publicUrl"`
	AgentURL   string    `json:"agentUrl,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}
type User struct {
	Username           string `json:"username"`
	PasswordHash       string `json:"passwordHash,omitempty"`
	DisplayName        string `json:"displayName"`
	Role               string `json:"role"`
	Revision           uint64 `json:"revision"`
	MustChangePassword bool   `json:"mustChangePassword"`
}
type session struct {
	Username     string    `json:"username"`
	UserRevision uint64    `json:"userRevision"`
	ExpiresAt    time.Time `json:"expiresAt"`
}
type Options struct {
	Updater              softwareupdate.Controller
	DataDirectory        string
	AgentArtifactBase    string
	AgentArtifactVersion string
	AgentDockerImage     string
	Assets               http.Handler
	Now                  func() time.Time
}
type Server struct {
	updates       softwareUpdateState
	topologyMu    sync.Mutex
	agents        *agentControl
	tasks         *taskRuntime
	store         *state.Store
	options       Options
	mux           *http.ServeMux
	passwordSlots chan struct{}
}

// New creates the first administrator in an empty database and returns its
// initial password once. Existing accounts and credentials are left unchanged.
func New(store *state.Store, options Options) (*Server, string, error) {
	if err := validateAgentArtifacts(options); err != nil {
		return nil, "", err
	}
	if err := store.ClaimRole("master"); err != nil {
		return nil, "", err
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	s := &Server{store: store, options: options, mux: http.NewServeMux(), passwordSlots: make(chan struct{}, 4)}
	configured, err := s.configured()
	if err != nil {
		return nil, "", err
	}
	initialPassword := ""
	if !configured {
		if err := store.View(requireEmptyAccounts); err != nil {
			return nil, "", err
		}
	}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"success": true, "role": "master"})
	})
	s.mux.HandleFunc("GET /api/setup/status", s.setupStatus)
	s.mux.HandleFunc("POST /api/setup/complete", s.completeSetup)
	s.mux.HandleFunc("POST /api/auth/login", s.login)
	s.mux.HandleFunc("POST /api/auth/logout", s.logout)
	s.mux.HandleFunc("GET /api/auth/current", s.current)
	s.mux.HandleFunc("POST /api/auth/change-password", s.changePassword)
	if err := installAgentRoutes(s); err != nil {
		return nil, "", err
	}
	installStorageRoutes(s)
	s.tasks = newTaskRuntime(s)
	if err := installTaskRoutes(s); err != nil {
		_ = s.agents.broker.Close()
		return nil, "", err
	}
	installReportRoutes(s)
	installToolRoutes(s)
	installNodeRoutes(s)
	installSoftwareUpdateRoutes(s)
	if err := installExceptionRoutes(s); err != nil {
		_ = s.agents.broker.Close()
		return nil, "", err
	}
	s.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "Unknown API endpoint") })
	if options.Assets != nil {
		s.mux.Handle("/", options.Assets)
	}
	if !configured {
		initialPassword, err = s.initializeAdministrator()
		if err != nil {
			_ = s.Close()
			return nil, "", err
		}
	}
	return s, initialPassword, nil
}

func requireEmptyAccounts(tx *state.Tx) error {
	return tx.Scan("users", "", "", 1, func(string, json.RawMessage) error {
		return errors.New("accounts exist without completed setup; restore configuration or import the existing installation")
	})
}

func (s *Server) configured() (bool, error) {
	var config Configuration
	err := s.store.View(func(tx *state.Tx) error { _, err := tx.Get("system", "configuration", &config); return err })
	return config.Configured, err
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w, r = localizedAPI(w, r)
		w.Header().Set("Cache-Control", "no-store")
		// There is no cookie authentication or permissive CORS. Also reject
		// foreign-origin POSTs before they reach setup or credential handlers.
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
				fail(w, 403, "Cross-origin requests are not allowed")
				return
			}
		}
		configured, err := s.configured()
		if err != nil {
			fail(w, 500, "Cannot read system configuration")
			return
		}
		if !configured && r.URL.Path != "/api/setup/status" && r.URL.Path != "/api/setup/complete" {
			fail(w, 503, "Complete initial setup first")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	configured, err := s.configured()
	if err != nil {
		fail(w, 500, "Cannot read setup state")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "configured": configured, "tokenRequired": false})
}

func (s *Server) completeSetup(w http.ResponseWriter, r *http.Request) {
	// Retain an explicit response for older clients; initialization is no longer
	// an unauthenticated operation exposed on the network.
	fail(w, 409, "Setup is already complete")
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if !s.acquirePasswordSlot(w, r) {
		return
	}
	defer s.releasePasswordSlot()
	var user User
	err := s.store.View(func(tx *state.Tx) error {
		_, err := tx.Get("users", strings.TrimSpace(request.Username), &user)
		return err
	})
	if err != nil {
		fail(w, 500, "Cannot read account")
		return
	}
	if user.Username == "" || !auth.VerifyPassword(request.Password, user.PasswordHash) {
		fail(w, 401, "Invalid username or password")
		return
	}
	newHash := user.PasswordHash
	if auth.NeedsPasswordUpgrade(newHash) {
		newHash, err = auth.HashPassword(request.Password)
		if err != nil {
			fail(w, 500, "Cannot upgrade account")
			return
		}
	}
	token, err := randomToken()
	if err != nil {
		fail(w, 500, "Cannot create session")
		return
	}
	changed := errors.New("account changed")
	err = s.store.Update(func(tx *state.Tx) error {
		var current User
		if _, err := tx.Get("users", user.Username, &current); err != nil {
			return err
		}
		if current.PasswordHash != user.PasswordHash || current.Revision != user.Revision {
			return changed
		}
		current.PasswordHash = newHash
		if err := tx.Put("users", current.Username, current); err != nil {
			return err
		}
		return tx.Put("sessions", tokenKey(token), session{Username: current.Username, UserRevision: current.Revision, ExpiresAt: s.options.Now().Add(24 * time.Hour)})
	})
	if errors.Is(err, changed) {
		fail(w, 409, "Account changed; sign in again")
		return
	}
	if err != nil {
		fail(w, 500, "Cannot save session")
		return
	}
	user.PasswordHash = ""
	writeJSON(w, 200, map[string]any{"success": true, "token": token, "user": user})
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func randomToken() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b), err
}

func (s *Server) authenticated(r *http.Request) (User, bool, error) {
	var user User
	valid := false
	err := s.store.View(func(tx *state.Tx) error {
		var entry session
		found, err := tx.Get("sessions", tokenKey(bearer(r)), &entry)
		if err != nil {
			return err
		}
		if !found || !s.options.Now().Before(entry.ExpiresAt) {
			return nil
		}
		found, err = tx.Get("users", entry.Username, &user)
		if err != nil {
			return err
		}
		valid = found && user.Revision == entry.UserRevision
		return nil
	})
	return user, valid, err
}

func (s *Server) current(w http.ResponseWriter, r *http.Request) {
	user, valid, err := s.authenticated(r)
	if err != nil {
		fail(w, 500, "Cannot read session")
		return
	}
	if !valid {
		fail(w, 401, "Sign in required")
		return
	}
	user.PasswordHash = ""
	writeJSON(w, 200, map[string]any{"success": true, "user": user})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Update(func(tx *state.Tx) error { return tx.Delete("sessions", tokenKey(bearer(r))) }); err != nil {
		fail(w, 500, "Cannot end session")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	user, valid, err := s.authenticated(r)
	if err != nil {
		fail(w, 500, "Cannot read session")
		return
	}
	if !valid {
		fail(w, 401, "Sign in required")
		return
	}
	var request struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if !s.acquirePasswordSlot(w, r) {
		return
	}
	defer s.releasePasswordSlot()
	if !auth.VerifyPassword(request.OldPassword, user.PasswordHash) {
		fail(w, 403, "Incorrect current password")
		return
	}
	if user.MustChangePassword && request.NewPassword == request.OldPassword {
		fail(w, 400, "Choose a password different from the initial password")
		return
	}
	hash, err := auth.HashPassword(request.NewPassword)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	changed := errors.New("account changed")
	err = s.store.Update(func(tx *state.Tx) error {
		var current User
		if _, err := tx.Get("users", user.Username, &current); err != nil {
			return err
		}
		if current.Revision != user.Revision || current.PasswordHash != user.PasswordHash {
			return changed
		}
		current.PasswordHash = hash
		current.Revision++
		current.MustChangePassword = false
		if err := tx.Put("users", current.Username, current); err != nil {
			return err
		}
		// Keep only this session valid; every other session fails its revision check.
		return tx.Put("sessions", tokenKey(bearer(r)), session{Username: current.Username, UserRevision: current.Revision, ExpiresAt: s.options.Now().Add(24 * time.Hour)})
	})
	if errors.Is(err, changed) {
		fail(w, 409, "Account changed; sign in again")
		return
	}
	if err != nil {
		fail(w, 500, "Cannot save password")
		return
	}
	user.PasswordHash = ""
	user.Revision++
	user.MustChangePassword = false
	writeJSON(w, 200, map[string]any{"success": true, "user": user})
}
func (s *Server) acquirePasswordSlot(w http.ResponseWriter, r *http.Request) bool {
	select {
	case s.passwordSlots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		fail(w, 429, "Too many authentication requests")
		return false
	}
}
func (s *Server) releasePasswordSlot() { <-s.passwordSlots }

func readJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		fail(w, 400, "Invalid JSON request")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		fail(w, 400, "Expected one JSON object")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"success": false, "message": localize(w, message)})
}
