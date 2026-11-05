package softwareupdate

import (
	"strconv"
	"strings"
)

// Release versions follow SemVer 2.0 without build metadata: MAJOR.MINOR.PATCH
// with an optional dot-separated prerelease such as 4.1.29-beta.1. Validation
// is deliberately strict because a version becomes part of download URLs,
// installer arguments and persisted update requests.
const (
	maxVersionLength      = 64
	maxNumericDigits      = 9
	maxIdentifierLength   = 32
	maxPrereleaseSegments = 8
)

type semanticVersion struct {
	core       [3]uint64
	prerelease []string
}

func numericIdentifier(value string) (uint64, bool) {
	if value == "" || len(value) > maxNumericDigits || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(value, 10, 64)
	return n, err == nil
}

func allDigits(value string) bool {
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
	}
	return value != ""
}

func parseVersion(value string) (semanticVersion, bool) {
	var parsed semanticVersion
	if value == "" || len(value) > maxVersionLength {
		return parsed, false
	}
	core, pre, hasPre := strings.Cut(value, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return parsed, false
	}
	for i, part := range parts {
		n, ok := numericIdentifier(part)
		if !ok {
			return parsed, false
		}
		parsed.core[i] = n
	}
	if !hasPre {
		return parsed, true
	}
	identifiers := strings.Split(pre, ".")
	if len(identifiers) > maxPrereleaseSegments {
		return parsed, false
	}
	for _, identifier := range identifiers {
		if identifier == "" || len(identifier) > maxIdentifierLength {
			return parsed, false
		}
		for _, c := range identifier {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-') {
				return parsed, false
			}
		}
		if allDigits(identifier) {
			if _, ok := numericIdentifier(identifier); !ok {
				return parsed, false
			}
		}
	}
	// Unreleased binaries report MAJOR.MINOR.PATCH-dev (see cmd/*/main.go).
	// They are development builds, not releases, and are never compared.
	if identifiers[0] == "dev" {
		return parsed, false
	}
	parsed.prerelease = identifiers
	return parsed, true
}

func compareIdentifiers(a, b string) int {
	aNumeric, bNumeric := allDigits(a), allDigits(b)
	switch {
	case aNumeric && bNumeric:
		x, _ := strconv.ParseUint(a, 10, 64)
		y, _ := strconv.ParseUint(b, 10, 64)
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
		return 0
	case aNumeric:
		return -1 // Numeric identifiers have lower precedence.
	case bNumeric:
		return 1
	}
	return strings.Compare(a, b)
}

func (v semanticVersion) compare(other semanticVersion) int {
	for i := range v.core {
		if v.core[i] != other.core[i] {
			if v.core[i] < other.core[i] {
				return -1
			}
			return 1
		}
	}
	// A release without a prerelease outranks any prerelease of the same core.
	switch {
	case len(v.prerelease) == 0 && len(other.prerelease) == 0:
		return 0
	case len(v.prerelease) == 0:
		return 1
	case len(other.prerelease) == 0:
		return -1
	}
	for i := 0; i < len(v.prerelease) && i < len(other.prerelease); i++ {
		if result := compareIdentifiers(v.prerelease[i], other.prerelease[i]); result != 0 {
			return result
		}
	}
	switch {
	case len(v.prerelease) < len(other.prerelease):
		return -1
	case len(v.prerelease) > len(other.prerelease):
		return 1
	}
	return 0
}

// ValidVersion reports whether value is an accepted release version.
func ValidVersion(value string) bool {
	_, ok := parseVersion(value)
	return ok
}

// IsPrerelease reports whether value is a valid prerelease version.
func IsPrerelease(value string) bool {
	parsed, ok := parseVersion(value)
	return ok && len(parsed.prerelease) > 0
}

// CompareVersions orders two release versions by SemVer precedence. ok is
// false when either value is not an accepted release version.
func CompareVersions(a, b string) (result int, ok bool) {
	x, okA := parseVersion(a)
	y, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	return x.compare(y), true
}

// Newer reports whether target is a valid release with higher precedence than
// current. Unparseable versions, such as development builds, are never newer.
func Newer(target, current string) bool {
	result, ok := CompareVersions(target, current)
	return ok && result > 0
}
