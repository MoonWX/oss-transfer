package softwareupdate

import (
	"strings"
	"testing"
)

func TestSemVerValidationIsStrict(t *testing.T) {
	for _, valid := range []string{
		"0.0.0", "4.1.29", "4.1.29-beta.1", "4.1.29-rc.1", "1.0.0-alpha", "1.0.0-alpha.beta", "1.0.0-0.3.7",
		"1.0.0-x-y-z.--", "1.0.0-0a", "999999999.999999999.999999999", "1.0.0-" + strings.Repeat("a", 32),
		"1.0.0-a.b.c.d.e.f.g.h",
	} {
		if !ValidVersion(valid) {
			t.Errorf("rejected %q", valid)
		}
	}
	for _, invalid := range []string{
		"", "latest", "v4.1.29", "4.1", "4.1.29.1", "04.1.29", "4.01.29", "4.1.029", "4.1.29-", "4.1.29-beta..1",
		"4.1.29-beta.01", "4.1.29-01", "4.1.29+build.1", "4.1.29-beta+build", "4.1.29-be_ta", "4.1.29-beta/../x",
		" 4.1.29", "4.1.29 ", "4.1.29-beta.1\n", "1234567890.0.0", "1.0.0-1234567890", "1.0.0-" + strings.Repeat("a", 33),
		"1.0.0-a.b.c.d.e.f.g.h.i", "1.0.0-" + strings.Repeat("a.", 31) + "a", "4.1.0-dev", "4.1.0-dev.1", "4.1.29-ß",
		"-1.0.0", "1.-0.0", "１.0.0",
	} {
		if ValidVersion(invalid) {
			t.Errorf("accepted %q", invalid)
		}
	}
	if IsPrerelease("4.1.29") || !IsPrerelease("4.1.29-beta.1") || IsPrerelease("4.1.29-beta.01") {
		t.Fatal("prerelease classification is wrong")
	}
}

func TestSemVerPrecedence(t *testing.T) {
	// Ordered lists from SemVer 2.0 section 11 and this project's release names.
	for _, chain := range [][]string{
		{"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0"},
		{"4.1.28", "4.1.29-beta.1", "4.1.29-beta.2", "4.1.29-beta.10", "4.1.29-rc.1", "4.1.29", "4.1.30-alpha", "4.2.0", "10.0.0"},
		{"1.0.0-1", "1.0.0-2", "1.0.0-10", "1.0.0-A", "1.0.0-a", "1.0.0-a.1"},
		{"1.9.0", "1.10.0", "1.11.0"},
	} {
		for i := range chain {
			for j := range chain {
				result, ok := CompareVersions(chain[i], chain[j])
				want := 0
				if i < j {
					want = -1
				} else if i > j {
					want = 1
				}
				if !ok || result != want {
					t.Fatalf("compare(%s, %s) = %d,%v; want %d", chain[i], chain[j], result, ok, want)
				}
				if Newer(chain[i], chain[j]) != (i > j) {
					t.Fatalf("Newer(%s, %s) wrong", chain[i], chain[j])
				}
			}
		}
	}
	if _, ok := CompareVersions("4.1.29", "4.1.0-dev"); ok {
		t.Fatal("development builds must not be comparable")
	}
	if Newer("4.1.29", "garbage") || Newer("garbage", "4.1.29") {
		t.Fatal("invalid versions must never be newer")
	}
}

func TestRequestedVersionPolicy(t *testing.T) {
	for _, tc := range []struct {
		version string
		allow   bool
		want    error
	}{
		{"4.1.29", false, nil}, {"4.1.29", true, nil}, {"4.1.29-beta.1", true, nil},
		{"4.1.29-beta.1", false, ErrPrereleaseNotAllowed}, {"4.1.29-beta.01", true, ErrInvalidVersion}, {"latest", true, ErrInvalidVersion},
	} {
		if err := checkRequested(tc.version, tc.allow); err != tc.want {
			t.Fatalf("%s allow=%v: %v, want %v", tc.version, tc.allow, err, tc.want)
		}
	}
}
