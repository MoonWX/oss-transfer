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
	ErrPasswordTooCommon       = errors.New("The password is too common or too easy to guess")
)

// commonPasswords are very common passwords that are long enough to pass the
// length rule. Compared ignoring case. The console (frontend/src/lib/
// passwordRule.ts) and the CLI (cli/cli_auth.go) keep the same list.
var commonPasswords = map[string]bool{
	"password": true, "password1": true, "password12": true, "password123": true, "passw0rd": true, "p@ssw0rd": true, "p@ssword": true,
	"qwertyui": true, "qwertyuiop": true, "qwerty12": true, "qwerty123": true, "qwer1234": true, "1234qwer": true, "asdfghjk": true, "asdf1234": true,
	"1q2w3e4r": true, "1q2w3e4r5t": true, "1qaz2wsx": true, "zaq12wsx": true, "abc12345": true, "abcd1234": true, "a1234567": true, "aa123456": true,
	"12341234": true, "12121212": true, "11223344": true, "123123123": true, "iloveyou": true, "sunshine": true, "princess": true, "football": true,
	"baseball": true, "superman": true, "trustno1": true, "letmein1": true, "welcome1": true, "welcome123": true, "changeme": true, "admin123": true,
	"admin1234": true, "administrator": true, "root1234": true,
}

// CheckNewPassword enforces the password rule for every password a user
// chooses: at least MinPasswordLength characters, not the user name (ignoring
// case and surrounding spaces), not trivial (see PasswordTooCommon) and not the
// current (or initial) password.
func CheckNewPassword(username, current, next string) error {
	if utf8.RuneCountInString(next) < MinPasswordLength {
		return ErrPasswordTooShort
	}
	if name := strings.TrimSpace(username); name != "" && strings.EqualFold(strings.TrimSpace(next), name) {
		return ErrPasswordMatchesUsername
	}
	if PasswordTooCommon(next) {
		return ErrPasswordTooCommon
	}
	if next == current {
		return ErrPasswordUnchanged
	}
	return nil
}

// PasswordTooCommon reports a password that is on the common password list,
// is one character repeated, or is a run of consecutive digits or letters in
// either direction (12345678, 87654321, abcdefgh), ignoring case and
// surrounding spaces.
func PasswordTooCommon(password string) bool {
	normalized := strings.ToLower(strings.TrimSpace(password))
	if commonPasswords[normalized] {
		return true
	}
	runes := []rune(normalized)
	if len(runes) < 2 {
		return false
	}
	digit := func(r rune) bool { return r >= '0' && r <= '9' }
	letter := func(r rune) bool { return r >= 'a' && r <= 'z' }
	repeated, ascending, descending := true, true, true
	sameClass := digit(runes[0]) || letter(runes[0])
	for i := 1; i < len(runes); i++ {
		repeated = repeated && runes[i] == runes[0]
		ascending = ascending && runes[i] == runes[i-1]+1
		descending = descending && runes[i] == runes[i-1]-1
		sameClass = sameClass && digit(runes[i]) == digit(runes[0]) && letter(runes[i]) == letter(runes[0])
	}
	return repeated || sameClass && (ascending || descending)
}
