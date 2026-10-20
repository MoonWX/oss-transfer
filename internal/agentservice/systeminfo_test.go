package agentservice

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseStorageMountsWithoutTouchingFilesystem(t *testing.T) {
	input := `24 1 8:1 / / rw,relatime - ext4 /dev/vda1 rw
25 24 0:4 / /proc rw - proc proc rw
26 24 0:5 / /sys rw - sysfs sysfs rw
27 24 0:6 / /run rw - tmpfs tmpfs rw
28 24 0:7 / /mnt/cfs rw,relatime - nfs4 10.1.0.232:/cfs rw
29 24 0:8 / /mnt/read\040only ro,relatime - xfs /dev/vdb1 ro
30 24 0:9 / /dev/shm rw - tmpfs tmpfs rw
31 24 0:10 / /scratch rw - tmpfs tmpfs rw
broken row
`
	want := []mountInfo{
		{Path: "/", Source: "/dev/vda1", FileSystem: "ext4"},
		{Path: "/mnt/cfs", Source: "10.1.0.232:/cfs", FileSystem: "nfs4"},
		{Path: "/mnt/read only", Source: "/dev/vdb1", FileSystem: "xfs", ReadOnly: true},
		{Path: "/scratch", Source: "tmpfs", FileSystem: "tmpfs"},
	}
	if got := parseMountInfo(strings.NewReader(input)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}
