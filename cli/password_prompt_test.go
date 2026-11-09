package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestLoginDefaultsAdminAndGuidesInitialChange(t *testing.T) {
	t.Setenv("OSS_TRANSFER_CLI_CONFIG_DIR", t.TempDir())
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body := adminTestBody(t, r)
		if body["username"] != "admin" || body["password"] != "initial password" {
			t.Fatal(body)
		}
		adminTestJSON(t, w, map[string]any{"success": true, "token": "session-only", "user": map[string]any{"username": "admin", "mustChangePassword": true}})
	})
	prompts := 0
	client.passwordPrompt = func(io.Reader, string) (string, error) { prompts++; return "initial password", nil }
	result, err := runAuthCommand(client, "login", nil, strings.NewReader(""), "http://admin-cli-test.invalid")
	if err != nil || prompts != 1 || !strings.Contains(result["message"].(string), "oss-transfer-cli change-password") {
		t.Fatal(result, err, prompts)
	}
	if _, ok := result["token"]; ok {
		t.Fatal("session leaked")
	}
	user := sanitizeOutput(result).(map[string]any)["user"].(map[string]any)
	if user["mustChangePassword"] != true {
		t.Fatal("JSON must preserve the password-change flag")
	}
}

func TestPasswordChangePromptsAndConfirmsBeforeRequest(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		calls := 0
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			body := adminTestBody(t, r)
			if r.URL.Path != "/api/auth/change-password" || body["oldPassword"] != "old password" || body["newPassword"] != "new password" {
				t.Fatal(body)
			}
			adminTestJSON(t, w, map[string]any{"success": true})
		})
		values := []string{"old password", "new password", "different"}
		if confirmed {
			values[2] = "new password"
		}
		client.passwordPrompt = func(io.Reader, string) (string, error) { value := values[0]; values = values[1:]; return value, nil }
		_, err := runAuthCommand(client, "change-password", nil, strings.NewReader(""), "")
		if confirmed && (err != nil || calls != 1) || !confirmed && (err == nil || calls != 0) {
			t.Fatal(confirmed, err, calls)
		}
	}
}

func TestNonInteractiveLoginDoesNotReadOrSendImplicitPassword(t *testing.T) {
	client := adminTestClient(t, func(http.ResponseWriter, *http.Request) { t.Fatal("implicit noninteractive credential sent") })
	if _, err := runAuthCommand(client, "login", nil, strings.NewReader("password\n"), ""); err == nil {
		t.Fatal("noninteractive input must be explicit")
	}
}

func TestPasswordChangeShowsRuleAndRejectsKnownViolationsBeforeRequest(t *testing.T) {
	for _, c := range []struct{ next, message string }{
		{"short", "The password must be at least 8 characters"},
		{"old password", "The new password must be different from the current password"},
		{"12345678", "The password is too common or too easy to guess"},
		{"PASSWORD", "The password is too common or too easy to guess"},
		{"zzzzzzzz", "The password is too common or too easy to guess"},
		{"hgfedcba", "The password is too common or too easy to guess"},
	} {
		client := adminTestClient(t, func(http.ResponseWriter, *http.Request) {
			t.Fatal("password breaking the rule was sent")
		})
		client.locale = "en"
		var labels []string
		values := []string{"old password", c.next, c.next}
		client.passwordPrompt = func(_ io.Reader, label string) (string, error) {
			labels = append(labels, label)
			value := values[0]
			values = values[1:]
			return value, nil
		}
		_, err := runAuthCommand(client, "change-password", nil, strings.NewReader(""), "")
		if err == nil || localizedCLIError("en", err) != c.message {
			t.Fatalf("new password %q: %v", c.next, err)
		}
		if len(labels) != 2 || client.text(labels[1]) != "New password (at least 8 characters, not a common or easily guessed password, different from the user name and the current password): " {
			t.Fatalf("prompt does not state the password rule: %q", labels)
		}
	}
}
