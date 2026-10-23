package master

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/auth"
	"github.com/MoonWX/oss-transfer/internal/state"
)

func TestExistingAdministratorUpgradePreservesAccountAndConfiguration(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	hash, err := auth.HashPassword("existing password")
	if err != nil {
		t.Fatal(err)
	}
	config := Configuration{Configured: true, PublicURL: "https://console.example", AgentURL: "http://10.0.0.1:1685", CreatedAt: time.Now().UTC()}
	// This is the pre-upgrade record shape, with no mustChangePassword field.
	err = db.Update(func(tx *state.Tx) error {
		if err := tx.Put("metadata", "format", map[string]any{"role": "master", "schemaVersion": 1}); err != nil {
			return err
		}
		if err := tx.Put("users", "operator", map[string]any{
			"username": "operator", "displayName": "Existing operator", "passwordHash": hash, "role": "admin", "revision": 7,
		}); err != nil {
			return err
		}
		return tx.Put("system", "configuration", config)
	})
	if err != nil {
		t.Fatal(err)
	}
	assertMasterSchema(t, db, 1)
	s, password, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if password != "" {
		t.Fatal("upgrade replaced the administrator")
	}
	assertMasterSchema(t, db, state.SchemaCompressedValues)
	httpServer := httptest.NewServer(s)
	defer httpServer.Close()
	status, result := request(t, httpServer.Client(), "POST", httpServer.URL+"/api/auth/login", "", map[string]string{"username": "operator", "password": "existing password"})
	if status != 200 || result["user"].(map[string]any)["mustChangePassword"] != false {
		t.Fatal("existing account incorrectly forced through first login", status, result)
	}
	token := result["token"].(string)
	status, _ = request(t, httpServer.Client(), "GET", httpServer.URL+"/api/master/nodes", token, nil)
	if status != 200 {
		t.Fatal("existing account lost access", status)
	}
	err = db.View(func(tx *state.Tx) error {
		var user User
		if _, err := tx.Get("users", "operator", &user); err != nil {
			return err
		}
		if user.PasswordHash != hash || user.Revision != 7 || user.DisplayName != "Existing operator" {
			t.Fatal("account changed during upgrade")
		}
		var current Configuration
		if _, err := tx.Get("system", "configuration", &current); err != nil {
			return err
		}
		if current != config {
			t.Fatal("network settings changed during upgrade", current)
		}
		var unexpected User
		if found, err := tx.Get("users", "admin", &unexpected); found || err != nil {
			t.Fatal("upgrade created an extra administrator", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertMasterSchema(t, db, state.SchemaCompressedValues)
}

func assertMasterSchema(t *testing.T, db *state.Store, want int) {
	t.Helper()
	err := db.View(func(tx *state.Tx) error {
		var format struct {
			Role          string `json:"role"`
			SchemaVersion int    `json:"schemaVersion"`
		}
		found, err := tx.Get("metadata", "format", &format)
		if err != nil {
			return err
		}
		if !found || format.Role != "master" || format.SchemaVersion != want {
			t.Fatalf("master database schema = %+v, want %d", format, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewAdministratorRequiresPasswordAwareReaderAfterPasswordChange(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server, initial, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	assertMasterSchema(t, db, state.SchemaCompressedValues)
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	status, login := request(t, httpServer.Client(), "POST", httpServer.URL+"/api/auth/login", "", map[string]string{
		"username": "admin", "password": initial,
	})
	if status != http.StatusOK || login["user"].(map[string]any)["mustChangePassword"] != true {
		t.Fatal("initial login failed or bypassed password change", status, login)
	}
	status, _ = request(t, httpServer.Client(), "POST", httpServer.URL+"/api/auth/change-password", login["token"].(string), map[string]string{
		"oldPassword": initial, "newPassword": "permanent password",
	})
	if status != http.StatusOK {
		t.Fatal("password change failed", status)
	}
	assertMasterSchema(t, db, state.SchemaCompressedValues)
	status, login = request(t, httpServer.Client(), "POST", httpServer.URL+"/api/auth/login", "", map[string]string{
		"username": "admin", "password": "permanent password",
	})
	if status != http.StatusOK || login["user"].(map[string]any)["mustChangePassword"] != false {
		t.Fatal("permanent password did not clear the restriction", status, login)
	}
}

func TestPasswordResetUpgradesLegacySchemaWithoutChangingUnselectedAccounts(t *testing.T) {
	for _, version := range []int{1, state.SchemaResolvedSources} {
		t.Run(fmt.Sprintf("schema%d", version), func(t *testing.T) {
			db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			hash, err := auth.HashPassword("existing password")
			if err != nil {
				t.Fatal(err)
			}
			original := User{
				Username: "operator", DisplayName: "Existing operator", Role: "admin",
				PasswordHash: hash, Revision: 7,
			}
			config := Configuration{Configured: true}
			err = db.Update(func(tx *state.Tx) error {
				// Build the old format without ClaimRole, which upgrades it on open.
				if err := tx.Put("metadata", "format", map[string]any{"role": "master", "schemaVersion": version}); err != nil {
					return err
				}
				if err := tx.Put("users", "operator", original); err != nil {
					return err
				}
				return tx.Put("system", "configuration", config)
			})
			if err != nil {
				t.Fatal(err)
			}
			assertMasterSchema(t, db, version)
			if _, _, err := ResetAdministratorPassword(db, "missing"); err == nil {
				t.Fatal("reset accepted a missing administrator")
			}
			assertMasterSchema(t, db, state.SchemaCompressedValues)
			if err := db.View(func(tx *state.Tx) error {
				var user User
				if found, err := tx.Get("users", "operator", &user); err != nil || !found || user != original {
					t.Fatalf("failed reset changed the existing account: found=%v user=%+v err=%v", found, user, err)
				}
				var current Configuration
				if found, err := tx.Get("system", "configuration", &current); err != nil || !found || current != config {
					t.Fatalf("failed reset changed configuration: found=%v config=%+v err=%v", found, current, err)
				}
				var missing User
				if found, err := tx.Get("users", "missing", &missing); err != nil || found {
					t.Fatalf("failed reset created the missing account: found=%v err=%v", found, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			name, password, err := ResetAdministratorPassword(db, "operator")
			if err != nil || name != "operator" || password == "" {
				t.Fatal("reset failed", err)
			}
			assertMasterSchema(t, db, state.SchemaCompressedValues)
			err = db.View(func(tx *state.Tx) error {
				var user User
				_, err := tx.Get("users", name, &user)
				if err == nil && (!user.MustChangePassword || user.Revision != 8 || user.DisplayName != "Existing operator" || !auth.VerifyPassword(password, user.PasswordHash)) {
					t.Fatal("reset did not atomically save the restricted account")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPasswordRecoveryPreservesDataAndRevokesSessions(t *testing.T) {
	f := newAgentHTTPFixture(t)
	// Persist application data before stopping the process and resetting offline.
	want := map[string]any{"name": "preserved task", "count": float64(42)}
	if err := f.db.Update(func(tx *state.Tx) error { return tx.Put("recovery-fixture", "task", want) }); err != nil {
		t.Fatal(err)
	}
	oldSession := f.admin
	f.close()
	db, err := state.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	username, initial, err := ResetAdministratorPassword(db, "")
	if err != nil || username != "admin" || initial == "" {
		t.Fatal("reset failed", err)
	}
	err = db.View(func(tx *state.Tx) error {
		var got map[string]any
		if _, err := tx.Get("recovery-fixture", "task", &got); err != nil {
			return err
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(want)
		if string(a) != string(b) {
			t.Fatal("reset changed application data")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if printed := f.open(t); printed != "" {
		t.Fatal("restart printed a new password after recovery")
	}
	status, _ := request(t, f.http.Client(), "GET", f.http.URL+"/api/auth/current", oldSession, nil)
	if status != 401 {
		t.Fatal("reset did not revoke old sessions", status)
	}
	status, body := request(t, f.http.Client(), "POST", f.http.URL+"/api/auth/login", "", map[string]string{"username": username, "password": initial})
	if status != 200 || body["user"].(map[string]any)["mustChangePassword"] != true {
		t.Fatal("reset password not usable or not gated", status, body)
	}
	status, _ = request(t, f.http.Client(), "GET", f.http.URL+"/api/master/nodes", body["token"].(string), nil)
	if status != 403 {
		t.Fatal("recovered session bypassed password change", status)
	}
}

func TestReverseProxyLoginUsesAccessHostWithoutStoredPublicURL(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, initial, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	upstream := httptest.NewServer(s)
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	// This proxy preserves the browser's Host, as production proxies should.
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()
	do := func(path, origin, token string, body any, want int) map[string]any {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", proxy.URL+path, strings.NewReader(string(data)))
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := proxy.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatal(path, response.StatusCode, result)
		}
		return result
	}
	login := do("/api/auth/login", proxy.URL, "", map[string]string{"username": "admin", "password": initial}, 200)
	token := login["token"].(string)
	do("/api/auth/change-password", proxy.URL, token, map[string]string{"oldPassword": initial, "newPassword": "behind proxy"}, 200)
	invite := do("/api/master/agents/invitations", proxy.URL, token, map[string]string{"label": "Private node", "masterUrl": "http://10.0.0.43:1685"}, 200)
	invitation := invite["data"].(map[string]any)
	if invitation["joinUrl"] != "http://10.0.0.43:1685/api/agents/bootstrap/"+invitation["inviteToken"].(string) {
		t.Fatal("browser origin replaced the chosen private endpoint")
	}
	do("/api/auth/login", "https://foreign.example", "", map[string]string{"username": "admin", "password": "behind proxy"}, 403)
}
