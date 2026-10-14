package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"

	"golang.org/x/crypto/argon2"
)

const passwordFormat = "argon2id$v=19$m=65536,t=3,p=1$"

func HashPassword(password string) (string, error) {
	if len(password) == 0 || len(password) > 1024 {
		return "", errors.New("password must contain 1 to 1024 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
	return passwordFormat + hex.EncodeToString(salt) + "$" + hex.EncodeToString(key), nil
}

func VerifyPassword(password, encoded string) bool {
	if len(password) > 1024 {
		return false
	}
	if strings.HasPrefix(encoded, passwordFormat) {
		parts := strings.Split(strings.TrimPrefix(encoded, passwordFormat), "$")
		if len(parts) != 2 {
			return false
		}
		salt, err := hex.DecodeString(parts[0])
		if err != nil || len(salt) != 16 {
			return false
		}
		want, err := hex.DecodeString(parts[1])
		if err != nil || len(want) != 32 {
			return false
		}
		got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
		return subtle.ConstantTimeCompare(got, want) == 1
	}
	// V4 hashes can be imported without knowing the plaintext password.
	parts := strings.Split(encoded, ":")
	if len(parts) != 2 || len(parts[0]) != 32 {
		return false
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return false
	}
	want, err := hex.DecodeString(parts[1])
	if err != nil || len(want) != 32 {
		return false
	}
	got := sha256.Sum256([]byte(parts[0] + password))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

func NeedsPasswordUpgrade(encoded string) bool { return !strings.HasPrefix(encoded, passwordFormat) }
