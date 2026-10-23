package master

import (
	"encoding/json"
	"net"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/cluster"
	"github.com/MoonWX/oss-transfer/internal/state"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// Display metadata is separate from the enrollment address used in API routes.
type NodeDisplay struct {
	DisplayName    string `json:"displayName"`
	DisplayAddress string `json:"displayAddress"`
	DisplayLabel   string `json:"displayLabel"`
	IdentityOnly   bool   `json:"identityOnly"`
}
type nodeRuntimeInfo struct {
	hostname string
	ips      []string
}
type nodeDisplaySet struct{ byID, byAddress map[string]NodeDisplay }

func makeNodeDisplay(address, label, remote string, info nodeRuntimeInfo) NodeDisplay {
	name := strings.TrimSpace(label)
	if name == "" {
		name = strings.TrimSpace(info.hostname)
	}
	ip, loopback := "", ""
	for _, candidate := range append([]string{remote, address}, info.ips...) {
		if parsed := net.ParseIP(candidate); parsed != nil && !parsed.IsUnspecified() && !parsed.IsMulticast() {
			if parsed.IsLoopback() {
				if loopback == "" {
					loopback = parsed.String()
				}
				continue
			}
			ip = parsed.String()
			break
		}
	}
	if ip == "" {
		ip = loopback
	}
	text := name
	if ip != "" {
		if text == "" || text == ip {
			text = ip
		} else {
			text += " (" + ip + ")"
		}
	}
	if text == "" && address != "" {
		text = "Node ID: " + address
	}
	return NodeDisplay{name, ip, text, name == "" && ip == ""}
}

// One registry snapshot includes revoked members, so historical tasks keep their
// names. No Agent requests or per-task database reads are made for presentation.
func (s *Server) nodeDisplaySnapshot() nodeDisplaySet {
	set := nodeDisplaySet{map[string]NodeDisplay{}, map[string]NodeDisplay{}}
	if s.agents == nil {
		return set
	}
	s.agents.mu.Lock()
	remote := make(map[string]string, len(s.agents.remoteAddress))
	for id, address := range s.agents.remoteAddress {
		remote[id] = address
	}
	info := make(map[string]nodeRuntimeInfo, len(s.agents.nodeInfo))
	for id, value := range s.agents.nodeInfo {
		info[id] = value
	}
	s.agents.mu.Unlock()
	_ = s.store.View(func(tx *state.Tx) error {
		after := ""
		for {
			count := 0
			if err := tx.Scan("agent-members", "", after, 256, func(key string, raw json.RawMessage) error {
				var member cluster.Member
				if err := json.Unmarshal(raw, &member); err != nil {
					return err
				}
				display := makeNodeDisplay(member.Address, member.Label, remote[member.ID], info[member.ID])
				set.byID[member.ID] = display
				if _, exists := set.byAddress[member.Address]; !exists || !member.Revoked {
					set.byAddress[member.Address] = display
				}
				after, count = key, count+1
				return nil
			}); err != nil {
				return err
			}
			if count < 256 {
				return nil
			}
		}
	})
	return set
}
func (set nodeDisplaySet) lookup(id, address string) NodeDisplay {
	if value, ok := set.byID[id]; ok {
		return value
	}
	if value, ok := set.byAddress[address]; ok {
		return value
	}
	return makeNodeDisplay(address, "", "", nodeRuntimeInfo{})
}
func (s *Server) rememberNodeInfo(id string, data map[string]any) {
	if data == nil {
		return
	}
	hostname, _ := data["hostname"].(string)
	var ips []string
	if values, ok := data["localIps"].([]string); ok {
		ips = append(ips, values...)
	}
	if values, ok := data["localIps"].([]any); ok {
		for _, value := range values {
			if ip, ok := value.(string); ok {
				ips = append(ips, ip)
			}
		}
	}
	if hostname == "" && len(ips) == 0 {
		return
	}
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	if s.agents.nodeInfo == nil {
		s.agents.nodeInfo = map[string]nodeRuntimeInfo{}
	}
	s.agents.nodeInfo[id] = nodeRuntimeInfo{hostname, ips}
}
func addNodeDisplay(row map[string]any, display NodeDisplay) {
	row["displayName"], row["displayAddress"], row["displayLabel"], row["identityOnly"] = display.DisplayName, display.DisplayAddress, display.DisplayLabel, display.IdentityOnly
}
func addExecutionNodeDisplay(row map[string]any, display NodeDisplay) {
	row["executionNodeIdentityOnly"] = display.IdentityOnly
	row["executionNodeName"], row["executionNodeAddress"], row["executionNodeLabel"], row["workerNodeLabel"] = display.DisplayName, display.DisplayAddress, display.DisplayLabel, display.DisplayLabel
}
func (set nodeDisplaySet) task(row map[string]any, nodes []task.Node) {
	values := make([]map[string]any, 0, len(nodes))
	for _, node := range nodes {
		value := map[string]any{"id": node.ID, "address": node.Address}
		display := set.lookup(node.ID, node.Address)
		addNodeDisplay(value, display)
		values = append(values, value)
	}
	row["executionNodes"] = values
	if len(nodes) == 1 {
		addExecutionNodeDisplay(row, set.lookup(nodes[0].ID, nodes[0].Address))
	}
}
