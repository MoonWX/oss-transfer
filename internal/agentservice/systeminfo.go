package agentservice

import (
	"bufio"
	"io"
	"net"
	"os"
	"runtime"
	"sort"
	"strings"
)

type mountInfo struct {
	Path       string `json:"path"`
	Source     string `json:"source"`
	FileSystem string `json:"fileSystem"`
	ReadOnly   bool   `json:"readOnly"`
}

// Addresses belong to this host. The Master separately records the peer
// address it sees; NAT addresses must not be guessed from local interfaces.
func hostDetails() map[string]any {
	hostname, _ := os.Hostname()
	addresses := make([]string, 0)
	seen := map[string]bool{}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		list, _ := iface.Addrs()
		for _, address := range list {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil || !ip.IsGlobalUnicast() || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			addresses = append(addresses, ip.String())
		}
	}
	sort.Strings(addresses)
	mounts := make([]mountInfo, 0)
	if runtime.GOOS == "linux" {
		if file, err := os.Open("/proc/self/mountinfo"); err == nil {
			mounts = parseMountInfo(io.LimitReader(file, 4*1024*1024))
			_ = file.Close()
		}
	}
	return map[string]any{"hostname": hostname, "os": runtime.GOOS, "arch": runtime.GOARCH, "localIps": addresses, "mounts": mounts}
}

func parseMountInfo(reader io.Reader) []mountInfo {
	mounts := make([]mountInfo, 0)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		halves := strings.SplitN(scanner.Text(), " - ", 2)
		if len(halves) != 2 {
			continue
		}
		left, right := strings.Fields(halves[0]), strings.Fields(halves[1])
		if len(left) < 6 || len(right) < 3 {
			continue
		}
		switch right[0] {
		case "proc", "sysfs", "cgroup", "cgroup2", "devtmpfs", "devpts", "securityfs",
			"pstore", "debugfs", "tracefs", "configfs", "mqueue", "hugetlbfs", "fusectl",
			"binfmt_misc", "autofs", "nsfs":
			continue
		}
		path := unescapeMount(left[4])
		// Keep storage mounts, including tmpfs explicitly mounted outside the
		// system trees. Reading metadata never probes a possibly stalled NFS.
		if path == "/proc" || strings.HasPrefix(path, "/proc/") || path == "/sys" || strings.HasPrefix(path, "/sys/") || path == "/dev" || strings.HasPrefix(path, "/dev/") || path == "/run" || strings.HasPrefix(path, "/run/") {
			continue
		}
		mounts = append(mounts, mountInfo{Path: path, Source: unescapeMount(right[1]), FileSystem: right[0], ReadOnly: strings.Contains(","+left[5]+",", ",ro,")})
	}
	sort.SliceStable(mounts, func(i, j int) bool { return mounts[i].Path < mounts[j].Path })
	return mounts
}

func unescapeMount(value string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(value)
}
