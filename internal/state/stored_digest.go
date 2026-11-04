package state

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/maphash"
)

// StoredHash hashes a value's stored bytes without decoding it. Equal hashes
// under one seed mean the stored value is (with overwhelming probability)
// unchanged, so callers can reuse a value they decoded earlier. Put never
// rewrites identical bytes, so an unchanged value keeps its hash.
func (t *Tx) StoredHash(bucket, key string, seed maphash.Seed) (uint64, bool) {
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return 0, false
	}
	value := b.Get([]byte(key))
	if value == nil {
		return 0, false
	}
	return maphash.Bytes(seed, value), true
}

// StoredDigest is a stable SHA-256 of a value's stored bytes. It identifies an
// exact stored version across processes: any change to the value changes the
// digest, while identical content may in rare cases have more than one digest
// (for example before and after compression was enabled).
func (t *Tx) StoredDigest(bucket, key string) (string, bool) {
	b := t.tx.Bucket([]byte(bucket))
	if b == nil {
		return "", false
	}
	value := b.Get([]byte(key))
	if value == nil {
		return "", false
	}
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:]), true
}
