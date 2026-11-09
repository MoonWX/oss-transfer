package master

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/state"
)

func (c Configuration) agentURL() string {
	if c.AgentURL != "" {
		return c.AgentURL
	}
	return c.PublicURL
}

func validMasterOrigin(value string) bool {
	if strings.ContainsAny(value, "\r\n\x00\t ") {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return false
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	return true
}

// Request fallback is for old clients without a configured URL. Browser and CLI
// clients send the intended endpoint explicitly, including reverse-proxy HTTPS.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func networkData(config Configuration) map[string]string {
	return map[string]string{"publicUrl": config.PublicURL, "agentUrl": config.AgentURL, "effectiveAgentUrl": config.agentURL()}
}

type lanInterface struct {
	name     string
	up       bool
	loopback bool
	addrs    []net.Addr
}

// Replaced in tests.
var hostInterfaces = func() ([]lanInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := make([]lanInterface, 0, len(interfaces))
	for _, item := range interfaces {
		addrs, err := item.Addrs()
		if err != nil {
			continue
		}
		result = append(result, lanInterface{name: item.Name, up: item.Flags&net.FlagUp != 0, loopback: item.Flags&net.FlagLoopback != 0, addrs: addrs})
	}
	return result, nil
}

var runningInContainer = func() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	return false
}

// virtualInterfacePrefixes name container and VM bridges whose addresses other
// machines cannot reach.
var virtualInterfacePrefixes = []string{"docker", "br-", "veth", "cni", "flannel", "cali", "virbr", "vmnet", "vboxnet", "utun", "podman", "lxc", "lxd"}

// selectLANAddresses returns private IPv4 addresses that another machine on the
// same network can usually reach. The console offers them in place of a
// loopback Master address.
func selectLANAddresses(interfaces []lanInterface) []string {
	seen := map[string]bool{}
	result := []string{}
	for _, item := range interfaces {
		if !item.up || item.loopback {
			continue
		}
		virtual := false
		for _, prefix := range virtualInterfacePrefixes {
			virtual = virtual || strings.HasPrefix(strings.ToLower(item.name), prefix)
		}
		if virtual {
			continue
		}
		for _, addr := range item.addrs {
			prefix, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := prefix.IP.To4()
			if ip == nil || !ip.IsPrivate() || seen[ip.String()] {
				continue
			}
			seen[ip.String()] = true
			result = append(result, ip.String())
		}
	}
	sort.Strings(result)
	if len(result) > 4 {
		result = result[:4]
	}
	return result
}

// lanAddresses is empty inside a container: its interfaces belong to the
// container network, not to the host that other machines reach.
func lanAddresses() []string {
	if runningInContainer() {
		return []string{}
	}
	interfaces, err := hostInterfaces()
	if err != nil {
		return []string{}
	}
	return selectLANAddresses(interfaces)
}

func (s *Server) networkSettings(w http.ResponseWriter, r *http.Request) {
	config, err := s.configuration()
	if err != nil {
		fail(w, 500, "Cannot read Master configuration")
		return
	}
	data := map[string]any{"lanAddresses": lanAddresses()}
	for key, value := range networkData(config) {
		data[key] = value
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": data})
}

func (s *Server) updateNetworkSettings(w http.ResponseWriter, r *http.Request) {
	var request struct {
		PublicURL *string `json:"publicUrl"`
		AgentURL  *string `json:"agentUrl"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if request.PublicURL != nil && !validMasterOrigin(*request.PublicURL) {
		fail(w, 400, "Public URL must be an HTTP or HTTPS origin")
		return
	}
	if request.AgentURL != nil && *request.AgentURL != "" && !validMasterOrigin(*request.AgentURL) {
		fail(w, 400, "Agent URL must be an HTTP or HTTPS origin")
		return
	}
	var config Configuration
	err := s.store.Update(func(tx *state.Tx) error {
		if _, err := tx.Get("system", "configuration", &config); err != nil {
			return err
		}
		if request.PublicURL != nil {
			config.PublicURL = strings.TrimRight(*request.PublicURL, "/")
		}
		if request.AgentURL != nil {
			config.AgentURL = strings.TrimRight(*request.AgentURL, "/")
		}
		return tx.Put("system", "configuration", config)
	})
	if err != nil {
		fail(w, 500, "Cannot save Master addresses")
		return
	}
	writeJSON(w, 200, map[string]any{"success": true, "data": networkData(config)})
}
