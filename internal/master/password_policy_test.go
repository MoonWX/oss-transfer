package master

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MoonWX/oss-transfer/internal/state"
)

// Every password a user chooses, on first login and later, follows the same
// rule, and replacing the initial password is reported exactly once.
func TestPasswordPolicyOnFirstLoginAndLaterChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mu sync.Mutex
	var changed []string
	s, initial, err := New(db, Options{InitialPasswordChanged: func(username string) {
		mu.Lock()
		defer mu.Unlock()
		changed = append(changed, username)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(s)
	defer server.Close()
	client := server.Client()
	status, body := request(t, client, "POST", server.URL+"/api/auth/login", "", map[string]string{"username": "admin", "password": initial})
	if status != http.StatusOK {
		t.Fatal(status, body)
	}
	token := body["token"].(string)
	change := func(old, next, language string) (int, map[string]any) {
		t.Helper()
		return requestWithLanguage(t, client, server.URL+"/api/auth/change-password", token, language, map[string]string{"oldPassword": old, "newPassword": next})
	}
	for _, c := range []struct{ next, language, message string }{
		{"abc", "zh-CN", "密码至少需要 8 个字符"},
		{"abc", "en", "The password must be at least 8 characters"},
		{"ADMIN", "zh-CN", "密码至少需要 8 个字符"},
		{initial, "zh-CN", "新密码不能与初始密码相同"},
	} {
		status, body := change(initial, c.next, c.language)
		if status != http.StatusBadRequest || body["message"] != c.message {
			t.Fatalf("new password %q accepted or misreported: %d %v", c.next, status, body)
		}
	}
	mu.Lock()
	if len(changed) != 0 {
		t.Fatal("rejected change reported the initial password as changed", changed)
	}
	mu.Unlock()
	if status, body := change(initial, "first permanent", "zh-CN"); status != http.StatusOK {
		t.Fatal(status, body)
	}
	for _, c := range []struct{ next, message string }{
		{"short", "パスワードは 8 文字以上にしてください"},
		{"first permanent", "新しいパスワードは現在のパスワードと異なるものにしてください"},
	} {
		status, body := change("first permanent", c.next, "ja")
		if status != http.StatusBadRequest || body["message"] != c.message {
			t.Fatalf("later change to %q accepted or misreported: %d %v", c.next, status, body)
		}
	}
	if status, body := change("first permanent", "second permanent", "zh-CN"); status != http.StatusOK {
		t.Fatal(status, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(changed) != 1 || changed[0] != "admin" {
		t.Fatal("initial password change must be reported exactly once", changed)
	}
}

func TestPasswordPolicyRejectsUserNameAndTrivialPasswords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.db")
	db, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, initial, err := New(db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Rename the generated account to one long enough to pass the length rule.
	if err := db.Update(func(tx *state.Tx) error {
		var user User
		if _, err := tx.Get("users", "admin", &user); err != nil {
			return err
		}
		if err := tx.Delete("users", "admin"); err != nil {
			return err
		}
		user.Username = "operator"
		return tx.Put("users", user.Username, user)
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s)
	defer server.Close()
	status, body := request(t, server.Client(), "POST", server.URL+"/api/auth/login", "", map[string]string{"username": "operator", "password": initial})
	if status != http.StatusOK {
		t.Fatal(status, body)
	}
	token := body["token"].(string)
	status, body = requestWithLanguage(t, server.Client(), server.URL+"/api/auth/change-password", token, "zh-CN", map[string]string{"oldPassword": initial, "newPassword": "Operator"})
	if status != http.StatusBadRequest || body["message"] != "密码不能与用户名相同" {
		t.Fatal("password equal to the user name accepted", status, body)
	}
	for _, weak := range []string{"12345678", "password", "aaaaaaaa"} {
		status, body = requestWithLanguage(t, server.Client(), server.URL+"/api/auth/change-password", token, "zh-CN", map[string]string{"oldPassword": initial, "newPassword": weak})
		if status != http.StatusBadRequest || body["message"] != "密码过于常见或容易猜到，请换一个" {
			t.Fatal("trivial password accepted", weak, status, body)
		}
	}
}

func requestWithLanguage(t *testing.T, client *http.Client, url, token, language string, body any) (int, map[string]any) {
	t.Helper()
	status, _, result, err := localizedRequest(client, "POST", url, token, language, body)
	if err != nil {
		t.Fatal(err)
	}
	return status, result
}
