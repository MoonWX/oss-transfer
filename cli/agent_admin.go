package main

import (
	"net/url"
	"strings"
	"unicode/utf8"
)

func agentShow(client *ApiClient, args []string) (map[string]interface{}, error) {
	o, err := adminOptions(args, nil, nil, 1)
	if err != nil {
		return nil, err
	}
	address := o.Positionals[0]
	if err := adminNodeAddress(address); err != nil {
		return nil, err
	}
	result, err := runNodeAdmin(client, "list", nil)
	if err != nil {
		return result, err
	}
	var selected map[string]interface{}
	for _, raw := range adminArray(adminData(result)["nodes"]) {
		node, _ := raw.(map[string]interface{})
		if node["address"] == address {
			selected = node
			break
		}
	}
	if selected == nil {
		return nil, cliErrorf("节点未注册: %s", address)
	}
	workers, _ := result["workers"].(map[string]interface{})
	worker, _ := workers[address].(map[string]interface{})
	complete := worker["success"] == true
	result = map[string]interface{}{"success": true, "node": selected, "status": worker, "complete": complete, "partial": !complete}
	if !complete {
		result["failedWorkers"] = []string{address}
	}
	return result, nil
}

func agentUpdate(client *ApiClient, args []string) (map[string]interface{}, error) {
	o, err := parseCommandOptions(args, []string{"label", "local-root"}, []string{"clear-local-roots"})
	if err != nil {
		return nil, err
	}
	if len(o.Positionals) != 1 || len(o.Values["label"]) > 1 || !o.Has("label") && !o.Has("local-root") && !o.Has("clear-local-roots") {
		return nil, cliErrorf("用法: agent update <节点> [--label 名称] [--local-root 目录 ... | --clear-local-roots]")
	}
	if err := adminNodeAddress(o.Positionals[0]); err != nil {
		return nil, err
	}
	if o.Has("clear-local-roots") && o.Has("local-root") {
		return nil, cliErrorf("--local-root 和 --clear-local-roots 不能同时使用")
	}
	body := map[string]interface{}{}
	if o.Has("label") {
		label := strings.TrimSpace(o.Value("label"))
		if !utf8.ValidString(label) || len(label) > 128 || strings.ContainsAny(label, "\x00\r\n") {
			return nil, cliErrorf("节点名称不能超过 128 字节或包含换行")
		}
		body["label"] = label
	}
	if o.Has("local-root") || o.Has("clear-local-roots") {
		roots := []string{}
		seen := map[string]bool{}
		for _, root := range o.Values["local-root"] {
			if !agentAbsolutePath(root) || seen[root] {
				return nil, cliErrorf("本地根目录必须是非重复的节点绝对路径: %s", root)
			}
			seen[root] = true
			roots = append(roots, root)
		}
		body["advertisedLocalRoots"] = roots
	}
	return client.Put("/nodes/"+url.PathEscape(o.Positionals[0]), body)
}

// Paths belong to the Agent OS, which may differ from the CLI's OS.
func agentAbsolutePath(value string) bool {
	if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return false
	}
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		return true
	}
	if len(value) >= 3 && (value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z') && value[1] == ':' && (value[2] == '\\' || value[2] == '/') {
		return true
	}
	if strings.HasPrefix(value, `\\`) || strings.HasPrefix(value, "//") {
		parts := strings.Split(strings.ReplaceAll(value[2:], `\`, "/"), "/")
		return len(parts) >= 2 && parts[0] != "" && parts[0] != "." && parts[0] != "?" && parts[1] != ""
	}
	return false
}
