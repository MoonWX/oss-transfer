package master

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func request(t *testing.T, client *http.Client, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		input = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, url, input)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, result
}

func TestInitialPasswordAuthenticationRestartAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s, initial, err := New(db, Options{DataDirectory: filepath.Dir(path), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) < 20 {
		t.Fatal("initial password missing or too short")
	}
	server := httptest.NewServer(s)
	client := server.Client()
	login := func(password string, mustChange bool) string {
		t.Helper()
		status, result := request(t, client, "POST", server.URL+"/api/auth/login", "", map[string]any{"username": "admin", "password": password})
		if status != 200 {
			t.Fatal(status, result)
		}
		user := result["user"].(map[string]any)
		if _, ok := user["passwordHash"]; ok {
			t.Fatal("hash exposed")
		}
		if user["mustChangePassword"] != mustChange {
			t.Fatal("wrong password-change requirement", user)
		}
		return result["token"].(string)
	}
	status, body := request(t, client, "GET", server.URL+"/api/setup/status", "", nil)
	encoded, _ := json.Marshal(body)
	if status != 200 || body["configured"] != true || body["tokenRequired"] != false || strings.Contains(string(encoded), initial) || body["dataDirectory"] != nil {
		t.Fatal("setup state leaked a credential or local path", status, body)
	}
	status, _ = request(t, client, "POST", server.URL+"/api/setup/complete", "", map[string]any{"username": "intruder", "password": "takeover", "setupToken": initial})
	if status != 409 {
		t.Fatal("legacy setup allowed takeover", status)
	}
	token := login(initial, true)
	for _, endpoint := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/master/nodes", nil},
		{"GET", "/api/master/status", nil},
		{"GET", "/api/master/network", nil},
		{"POST", "/api/master/agents/invitations", map[string]string{"label": "not allowed"}},
		{"PUT", "/api/master/network", map[string]string{"publicUrl": "https://blocked.example"}},
	} {
		status, _ = request(t, client, endpoint.method, server.URL+endpoint.path, token, endpoint.body)
		if status != 403 {
			t.Fatalf("initial session bypassed restriction at %s: %d", endpoint.path, status)
		}
	}
	status, _ = request(t, client, "POST", server.URL+"/api/auth/change-password", token, map[string]string{"oldPassword": "wrong", "newPassword": "new password"})
	if status != 403 {
		t.Fatal("wrong current password accepted", status)
	}
	status, _ = request(t, client, "POST", server.URL+"/api/auth/change-password", token, map[string]string{"oldPassword": initial, "newPassword": initial})
	if status != 400 {
		t.Fatal("unchanged initial password accepted", status)
	}
	// Restart BEFORE the first change must preserve both the secret and its gate.
	server.Close()
	_ = s.Close()
	_ = db.Close()
	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, reprinted, err := New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if reprinted != "" {
		t.Fatal("restart rotated or reprinted initial password")
	}
	server = httptest.NewServer(s)
	client = server.Client()
	other := login(initial, true)
	status, body = request(t, client, "GET", server.URL+"/api/auth/current", token, nil)
	if status != 200 || body["user"].(map[string]any)["mustChangePassword"] != true {
		t.Fatal("first-login gate lost after restart", status, body)
	}
	status, body = request(t, client, "POST", server.URL+"/api/auth/change-password", token, map[string]string{"oldPassword": initial, "newPassword": "new password"})
	if status != 200 || body["user"].(map[string]any)["mustChangePassword"] != false {
		t.Fatal("password change failed", status, body)
	}
	status, _ = request(t, client, "GET", server.URL+"/api/auth/current", other, nil)
	if status != 401 {
		t.Fatal("other session survived password change", status)
	}
	status, _ = request(t, client, "GET", server.URL+"/api/master/nodes", token, nil)
	if status != 200 {
		t.Fatal("same session not unlocked", status)
	}
	server.Close()
	_ = s.Close()
	_ = db.Close()
	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, reprinted, err = New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if reprinted != "" {
		t.Fatal("credentials reset after completed first login")
	}
	server = httptest.NewServer(s)
	defer server.Close()
	client = server.Client()
	status, _ = request(t, client, "GET", server.URL+"/api/master/nodes", token, nil)
	if status != 200 {
		t.Fatal("committed session missing", status)
	}
	status, _ = request(t, client, "POST", server.URL+"/api/auth/login", "", map[string]string{"username": "admin", "password": initial})
	if status != 401 {
		t.Fatal("initial password remains valid", status)
	}
	next := login("new password", false)
	status, _ = request(t, client, "POST", server.URL+"/api/auth/logout", next, nil)
	if status != 200 {
		t.Fatal(status)
	}
	status, _ = request(t, client, "GET", server.URL+"/api/auth/current", next, nil)
	if status != 401 {
		t.Fatal("logout failed", status)
	}
	now = now.Add(25 * time.Hour)
	status, _ = request(t, client, "GET", server.URL+"/api/auth/current", token, nil)
	if status != 401 {
		t.Fatal("expired session accepted", status)
	}
	if err := db.View(func(tx *state.Tx) error {
		var user User
		_, err := tx.Get("users", "admin", &user)
		if strings.Contains(user.PasswordHash, initial) || user.MustChangePassword {
			t.Fatal("plaintext credential or stale gate persisted")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRejectForeignOriginAndMultipleJSONDocuments(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, _, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, test := range []struct {
		body, origin string
		status       int
	}{{"{} {}", "", 400}, {"{}", "http://evil.example", 403}} {
		r := httptest.NewRequest("POST", "http://localhost/api/auth/login", strings.NewReader(test.body))
		r.Header.Set("Origin", test.origin)
		r.Header.Set("X-Forwarded-Host", "evil.example")
		r.Header.Set("Forwarded", "host=evil.example;proto=http")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != test.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestExistingAccountWithoutConfigurationCannotBeClaimed(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(func(tx *state.Tx) error {
		return tx.Put("users", "existing-admin", User{Username: "existing-admin", Role: "admin", PasswordHash: "do not replace"})
	}); err != nil {
		t.Fatal(err)
	}
	if server, token, err := New(db, Options{}); err == nil || server != nil || token != "" {
		t.Fatal("partial existing installation opened setup", err)
	}
}
