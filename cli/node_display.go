package main

import (
	"fmt"
	"net"
	"strings"
)

// Node addresses remain selectors. Display metadata is only used when printing;
// a label or an observed IP is never substituted into an API request.
type cliNodeDisplays map[string]map[string]interface{}

func firstNodeString(row map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value := getString(row, key); value != "" {
			return value
		}
	}
	return ""
}

func cliNodeLabel(row map[string]interface{}, identity string, locale cliLocale) string {
	if label := firstNodeString(row, "executionNodeLabel", "workerNodeLabel", "displayLabel"); label != "" && row["identityOnly"] != true && row["executionNodeIdentityOnly"] != true && (label != identity || firstNodeString(row, "displayName", "executionNodeName", "displayAddress", "executionNodeAddress") != "") {
		return label
	}
	name := firstNodeString(row, "executionNodeName", "displayName", "label", "hostname")
	ip := firstNodeString(row, "executionNodeAddress", "displayAddress")
	if ip == "" && net.ParseIP(identity) != nil {
		ip = identity
	}
	if name != "" && ip != "" && name != ip {
		return name + " (" + ip + ")"
	}
	if name != "" {
		return name
	}
	if ip != "" {
		return ip
	}
	if identity == "" || identity == "—" {
		return "—"
	}
	return fmt.Sprintf(locale.text("节点标识 %s"), identity)
}

func collectCLINodeDisplays(value interface{}) cliNodeDisplays {
	displays := cliNodeDisplays{}
	var visit func(interface{})
	visit = func(value interface{}) {
		switch row := value.(type) {
		case map[string]interface{}:
			if nested, ok := row["nodeDisplays"].(map[string]interface{}); ok {
				for identity, raw := range nested {
					if display, ok := raw.(map[string]interface{}); ok {
						displays[identity] = display
					}
				}
			}
			if firstNodeString(row, "executionNodeLabel", "workerNodeLabel", "displayLabel", "displayName", "executionNodeName") != "" {
				for _, key := range []string{"address", "id", "executionNodeIp", "workerNode", "workerIp"} {
					if identity := getString(row, key); identity != "" {
						displays[identity] = row
					}
				}
			}
			for key, nested := range row {
				if key != "nodeDisplays" {
					visit(nested)
				}
			}
		case []interface{}:
			for _, item := range row {
				visit(item)
			}
		case []map[string]interface{}:
			for _, item := range row {
				visit(item)
			}
		}
	}
	visit(value)
	return displays
}

func (displays cliNodeDisplays) label(identity string, locale cliLocale) string {
	return cliNodeLabel(displays[identity], identity, locale)
}

func taskWorkersDisplay(task map[string]interface{}, locale cliLocale) string {
	displays := collectCLINodeDisplays(task)
	nodes := strings.Split(taskWorkers(task), ",")
	for i, identity := range nodes {
		nodes[i] = displays.label(identity, locale)
	}
	return strings.Join(nodes, ", ")
}

// Flattening grouped responses keeps the grouping identity and copies its
// metadata onto each row, including rows returned by an older Agent.
func withCLINodeDisplay(row map[string]interface{}, identity string, displays cliNodeDisplays) map[string]interface{} {
	copy := make(map[string]interface{}, len(row)+3)
	for key, value := range row {
		copy[key] = value
	}
	if display := displays[identity]; display != nil {
		for _, keys := range [][2]string{{"executionNodeName", "displayName"}, {"executionNodeAddress", "displayAddress"}, {"executionNodeLabel", "displayLabel"}} {
			if getString(copy, keys[0]) == "" {
				copy[keys[0]] = firstNodeString(display, keys[0], keys[1])
			}
		}
		if _, exists := copy["executionNodeIdentityOnly"]; !exists {
			if only, ok := display["executionNodeIdentityOnly"].(bool); ok {
				copy["executionNodeIdentityOnly"] = only
			} else if only, ok := display["identityOnly"].(bool); ok {
				copy["executionNodeIdentityOnly"] = only
			}
		}
	}
	return copy
}

func cliNodeCollection(value interface{}, displays cliNodeDisplays, locale cliLocale) interface{} {
	switch values := value.(type) {
	case []string:
		labels := make([]string, len(values))
		for i, identity := range values {
			labels[i] = displays.label(identity, locale)
		}
		return labels
	case []interface{}:
		labels := make([]interface{}, len(values))
		for i, value := range values {
			labels[i] = value
			if identity, ok := value.(string); ok {
				labels[i] = displays.label(identity, locale)
			}
		}
		return labels
	case map[string]interface{}:
		labels := make(map[string]interface{}, len(values))
		for identity, value := range values {
			labels[displays.label(identity, locale)+" ["+identity+"]"] = value
		}
		return labels
	case map[string]string:
		labels := make(map[string]string, len(values))
		for identity, value := range values {
			labels[displays.label(identity, locale)+" ["+identity+"]"] = value
		}
		return labels
	}
	return value
}

func redundantCLINodeField(key string, row map[string]interface{}) bool {
	switch key {
	case "nodeDisplays", "identityOnly", "executionNodeIdentityOnly":
		return true
	case "displayName", "displayAddress":
		return getString(row, "displayLabel") != ""
	case "executionNodeName", "executionNodeAddress", "executionNodeLabel", "workerNodeLabel":
		return firstNodeString(row, "executionNodeIp", "workerNode", "workerIp", "recycleNode") != ""
	}
	return false
}

func cliNodeCollectionField(key string) bool {
	switch key {
	case "failedWorkers", "unreachableWorkers", "unavailableWorkers", "truncatedWorkers", "missingWorkers", "respondingNodes", "queriedWorkers", "eligibleWorkerNodes", "participatingWorkerNodes", "workerErrors", "nodeErrors":
		return true
	}
	return false
}
