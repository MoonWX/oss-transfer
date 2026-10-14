package auth

import (
	"testing"
)

func TestPasswordCompatibility(t *testing.T) {
	password := "exact spaces 密码 "
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(password, hash) || VerifyPassword("wrong", hash) || VerifyPassword("exact spaces 密码", hash) {
		t.Fatal("password verification changed contents")
	}
	if NeedsPasswordUpgrade(hash) {
		t.Fatal("new hash requires upgrade")
	}
	// Fixed V4-format vector computed independently with Python hashlib.
	legacy := "00112233445566778899aabbccddeeff:52a8d83bd18961c89a33fdb90dc0c73930cfd926cbc20e48b57e4ceaa7d545f9"
	if !VerifyPassword(password, legacy) || !NeedsPasswordUpgrade(legacy) {
		t.Fatal("V4 password cannot be upgraded")
	}
	for _, bad := range []string{"", "argon2id$v=19$m=999999999,t=3,p=1$00$00", hash + "$extra", "bad:hash"} {
		if VerifyPassword(password, bad) {
			t.Fatalf("accepted malformed hash %q", bad)
		}
	}
}
