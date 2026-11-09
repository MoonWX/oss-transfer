package auth

import (
	"errors"
	"testing"
)

func TestCheckNewPassword(t *testing.T) {
	for _, c := range []struct {
		username, current, next string
		want                    error
	}{
		{"admin", "initial-secret", "abc", ErrPasswordTooShort},
		{"admin", "initial-secret", "1234567", ErrPasswordTooShort},
		// Characters, not bytes: seven CJK characters are 21 bytes but too short.
		{"admin", "initial-secret", "密码密码密码密", ErrPasswordTooShort},
		{"administrator", "initial-secret", "Administrator", ErrPasswordMatchesUsername},
		{"operator", "initial-secret", " operator ", ErrPasswordMatchesUsername},
		{"admin", "same-password", "same-password", ErrPasswordUnchanged},
		{"admin", "initial-secret", "12345678", nil},
		{"admin", "initial-secret", "密码密码密码密码", nil},
		{"", "initial-secret", "long enough", nil},
	} {
		if got := CheckNewPassword(c.username, c.current, c.next); !errors.Is(got, c.want) {
			t.Errorf("CheckNewPassword(%q, %q, %q) = %v, want %v", c.username, c.current, c.next, got, c.want)
		}
	}
}
