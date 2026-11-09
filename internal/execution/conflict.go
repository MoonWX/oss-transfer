package execution

import (
	"strings"

	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// Conflict policies decide what happens when the destination key exists.
const (
	// ConflictSkip keeps any existing destination object.
	ConflictSkip = "SKIP"
	// ConflictOverwrite replaces any existing destination object.
	ConflictOverwrite = "OVERWRITE"
	// ConflictOverwriteChanged replaces an existing destination object only
	// when its listing metadata says it differs from the source (see
	// DestinationOutdated). It never reads object content.
	ConflictOverwriteChanged = "OVERWRITE_CHANGED"
)

// ValidConflictPolicy reports whether policy is one of the conflict policies.
func ValidConflictPolicy(policy string) bool {
	switch policy {
	case ConflictSkip, ConflictOverwrite, ConflictOverwriteChanged:
		return true
	}
	return false
}

// DestinationOutdated is the OVERWRITE_CHANGED decision for a destination
// that exists. It uses only LIST/HEAD metadata:
//
//  1. sizes differ: overwrite;
//  2. both ETags are simple MD5 ETags (32 hex digits, case-insensitive,
//     quotes removed) and differ: overwrite. Multipart ETags ("<hex>-<n>"),
//     local file fingerprints and any other form are never compared;
//  3. the source was modified after the destination: overwrite;
//  4. otherwise keep the destination (skip).
//
// An unknown modification time on either side never counts as later.
func DestinationOutdated(source, destination transferstore.Object) bool {
	if source.Size != destination.Size {
		return true
	}
	if left, ok := simpleMD5ETag(source.ETag); ok {
		if right, ok := simpleMD5ETag(destination.ETag); ok && left != right {
			return true
		}
	}
	return !source.ModifiedAt.IsZero() && !destination.ModifiedAt.IsZero() && source.ModifiedAt.After(destination.ModifiedAt)
}

// simpleMD5ETag returns the lowercase digest of a single-part MD5 ETag.
func simpleMD5ETag(etag string) (string, bool) {
	etag = strings.TrimSpace(etag)
	if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
		etag = etag[1 : len(etag)-1]
	}
	if len(etag) != 32 {
		return "", false
	}
	for i := 0; i < len(etag); i++ {
		c := etag[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return "", false
		}
	}
	return strings.ToLower(etag), true
}
