package auth

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// MinPasswordLength is the minimum number of characters (Unicode code points)
// in a password chosen by a user. Generated initial passwords are longer.
const MinPasswordLength = 8

// Password policy violations. The messages are also the Master's translation
// keys, so keep them in sync with internal/master/i18n_messages.go.
var (
	ErrPasswordTooShort        = errors.New("The password must be at least 8 characters")
	ErrPasswordMatchesUsername = errors.New("The password must not be the same as the user name")
	ErrPasswordUnchanged       = errors.New("The new password must be different from the current password")
)

// CheckNewPassword enforces the password rule for every password a user
// chooses: at least MinPasswordLength characters, not the user name (ignoring
// case and surrounding spaces) and not the current (or initial) password.
func CheckNewPassword(username, current, next string) error {
	if utf8.RuneCountInString(next) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if name := strings.TrimSpace(username); name != "" && strings.EqualFold(strings.TrimSpace(next), name) {
		return ErrPasswordMatchesUsername
	}
	if next == current {
		return ErrPasswordUnchanged
	}
	return nil
}
