package master

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
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

func TestSetupAuthenticationRestartAndRevocation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s, bootstrap, err := New(db, Options{DataDirectory: filepath.Dir(path), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s)
	client := server.Client()
	for _, endpoint := range []string{"/api/auth/login", "/api/master/add-buckets", "/api/agents/join"} {
		status, _ := request(t, client, "POST", server.URL+endpoint, "", map[string]any{})
		if status != 503 {
			t.Fatalf("unconfigured %s returned %d", endpoint, status)
		}
	}
	status, body := request(t, client, "GET", server.URL+"/api/setup/status", "", nil)
	if status != 200 || body["configured"] != false {
		t.Fatal(status, body)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), bootstrap) {
		t.Fatal("setup token leaked")
	}
	setup := map[string]any{"username": "admin", "password": "first password", "publicUrl": server.URL, "setupToken": "wrong"}
	status, _ = request(t, client, "POST", server.URL+"/api/setup/complete", "", setup)
	if status != 403 {
		t.Fatal(status)
	}
	setup["setupToken"] = bootstrap
	status, _ = request(t, client, "POST", server.URL+"/api/setup/complete", "", setup)
	if status != 200 {
		t.Fatal(status)
	}
	setup["password"] = "hijacked"
	status, _ = request(t, client, "POST", server.URL+"/api/setup/complete", "", setup)
	if status != 409 {
		t.Fatal(status)
	}
	login := func(password string) string {
		status, result := request(t, client, "POST", server.URL+"/api/auth/login", "", map[string]any{"username": "admin", "password": password})
		if status != 200 {
			t.Fatal(status, result)
		}
		user := result["user"].(map[string]any)
		if _, ok := user["passwordHash"]; ok {
			t.Fatal("hash exposed")
		}
		return result["token"].(string)
	}
	token := login("first password")
	otherToken := login("first password")
	status, _ = request(t, client, "POST", server.URL+"/api/auth/change-password", token, map[string]any{"oldPassword": "first password", "newPassword": "new password"})
	if status != 200 {
		t.Fatal(status)
	}
	status, _ = request(t, client, "GET", server.URL+"/api/auth/current", otherToken, nil)
	if status != 401 {
		t.Fatal("other session survived password change", status)
	}
	server.Close()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, bootstrap, err = New(db, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if bootstrap != "" {
		t.Fatal("setup reopened after restart")
	}
	server = httptest.NewServer(s)
	defer server.Close()
	client = server.Client()
	status, _ = request(t, client, "GET", server.URL+"/api/auth/current", token, nil)
	if status != 200 {
		t.Fatal("committed session missing", status)
	}
	status, _ = request(t, client, "POST", server.URL+"/api/auth/login", "", map[string]any{"username": "admin", "password": "first password"})
	if status != 401 {
		t.Fatal("old password still valid", status)
	}
	newToken := login("new password")
	status, _ = request(t, client, "POST", server.URL+"/api/auth/logout", newToken, nil)
	if status != 200 {
		t.Fatal(status)
	}
	status, _ = request(t, client, "GET", server.URL+"/api/auth/current", newToken, nil)
	if status != 401 {
		t.Fatal("logout failed", status)
	}
	now = now.Add(25 * time.Hour)
	status, _ = request(t, client, "GET", server.URL+"/api/auth/current", token, nil)
	if status != 401 {
		t.Fatal("expired session accepted", status)
	}
}

func TestConcurrentSetupCommitsExactlyOneAccount(t *testing.T) {
	db, err := state.Open(filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, token, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s)
	defer server.Close()
	var wg sync.WaitGroup
	statuses := make(chan int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, _ := request(t, server.Client(), "POST", server.URL+"/api/setup/complete", "", map[string]any{"username": fmt.Sprintf("admin%d", i), "password": "password", "publicUrl": server.URL, "setupToken": token})
			statuses <- status
		}(i)
	}
	wg.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		if status == 200 {
			success++
		} else if status != 409 {
			t.Fatal("unexpected initialization response", status)
		}
	}
	if success != 1 {
		t.Fatal("setup committed", success, "times")
	}
	count := 0
	if err := db.View(func(tx *state.Tx) error {
		return tx.Scan("users", "", "", 10, func(string, json.RawMessage) error { count++; return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("partial accounts committed", count)
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
	for _, test := range []struct {
		body, origin string
		status       int
	}{{"{} {}", "", 400}, {"{}", "http://evil.example", 403}} {
		r := httptest.NewRequest("POST", "http://localhost/api/setup/complete", strings.NewReader(test.body))
		r.Header.Set("Origin", test.origin)
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
