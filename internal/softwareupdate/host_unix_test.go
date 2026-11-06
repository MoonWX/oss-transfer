//go:build !windows

package softwareupdate

import (
	"reflect"
	"testing"
)

func TestParseSystemdVersion(t *testing.T) {
	for output, want := range map[string]int{
		"systemd 219\n+PAM +AUDIT +SELINUX +IMA -APPARMOR +SMACK +SYSVINIT\n": 219,
		"systemd 252 (252.22-1~deb12u1)\n+PAM +AUDIT\n":                       252,
		"systemd 255 (255.4-1ubuntu8.4)":                                      255,
		"":                                                                    0,
		"systemd\n":                                                           0,
		"systemd 2x9\n":                                                       0,
		"not systemd 240\n":                                                   0,
	} {
		if got := parseSystemdVersion(output); got != want {
			t.Errorf("parseSystemdVersion(%q) = %d, want %d", output, got, want)
		}
	}
}

func TestSystemdRunArgumentsMatchServiceManager(t *testing.T) {
	tail := []string{"/helper", "__apply-update", "/data/runtime/updates/id/request.json"}
	for _, test := range []struct {
		version int
		want    []string
	}{
		// Unknown versions and CentOS 7 / RHEL 7 use only systemd 219 options;
		// the helper applies its own umask.
		{0, []string{"--quiet", "--unit=oss-transfer-update-id"}},
		{219, []string{"--quiet", "--unit=oss-transfer-update-id"}},
		{237, []string{"--quiet", "--collect", "--unit=oss-transfer-update-id"}},
		{239, []string{"--quiet", "--collect", "--unit=oss-transfer-update-id"}},
		{240, []string{"--quiet", "--collect", "--unit=oss-transfer-update-id", "--property=Type=exec", "--property=UMask=0077"}},
		{255, []string{"--quiet", "--collect", "--unit=oss-transfer-update-id", "--property=Type=exec", "--property=UMask=0077"}},
	} {
		got := systemdRunArguments(test.version, "oss-transfer-update-id", tail[0], tail[2])
		if want := append(test.want, tail...); !reflect.DeepEqual(got, want) {
			t.Errorf("systemd %d: got %q, want %q", test.version, got, want)
		}
	}
}
