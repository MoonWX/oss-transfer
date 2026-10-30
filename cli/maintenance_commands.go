package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var maintenanceVersion = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func runMaintenanceCommand(client *ApiClient, group string, args []string) (map[string]interface{}, error) {
	if len(args) == 0 {
		return nil, cliErrorf("缺少 %s 子命令，请运行 oss-transfer-cli %s --help", group, group)
	}
	switch group {
	case "update":
		return runSoftwareUpdate(client, args[0], args[1:])
	case "backup":
		return runBackupMaintenance(client, args[0], args[1:])
	default:
		return nil, cliErrorf("未知命令组: %s", group)
	}
}

func updateVersion(value string) (string, error) {
	if len(value) > 64 || !maintenanceVersion.MatchString(value) {
		return "", cliErrorf("--version 必须是发布版本号，如 4.1.22")
	}
	return strings.TrimPrefix(value, "v"), nil
}

func runSoftwareUpdate(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	if action == "status" || action == "check" {
		if _, err := adminOptions(args, nil, nil, 0); err != nil {
			return nil, err
		}
		if action == "check" {
			return client.Post("/software-updates/check", map[string]interface{}{})
		}
		return client.Get("/software-updates")
	}
	if action != "master" && action != "agent" && action != "agents" {
		return nil, cliErrorf("未知 update 子命令: %s", action)
	}
	o, err := adminOptions(args, []string{"version", "worker"}, []string{"all", "yes"}, 0)
	if err != nil {
		return nil, err
	}
	if err := adminConfirmed(o); err != nil {
		return nil, err
	}
	path := "/software-updates/" + action
	switch action {
	case "master":
		if !o.Has("version") || o.Has("worker") || o.Has("all") {
			return nil, cliErrorf("用法: update master --version 版本 --yes")
		}
	case "agent":
		if !o.Has("worker") || o.Has("all") {
			return nil, cliErrorf("用法: update agent --worker 节点 [--version 版本] --yes")
		}
		if err := adminNodeAddress(o.Value("worker")); err != nil {
			return nil, err
		}
		path = "/software-updates/agents/" + url.PathEscape(o.Value("worker"))
	case "agents":
		if !o.Has("all") || o.Has("worker") {
			return nil, cliErrorf("用法: update agents --all [--version 版本] --yes")
		}
	}
	version := ""
	if o.Has("version") {
		version, err = updateVersion(o.Value("version"))
		if err != nil {
			return nil, err
		}
	} else {
		// Agents follow the running Master's version, never a newer release or
		// an inferred local CLI version. The server rechecks this when queuing.
		snapshot, err := client.Get("/software-updates")
		if err != nil {
			return snapshot, err
		}
		master, _ := adminData(snapshot)["master"].(map[string]interface{})
		info, _ := master["info"].(map[string]interface{})
		version, err = updateVersion(getString(info, "version"))
		if err != nil {
			return nil, cliErrorf("无法确定 Master 的发布版本，请先查看 update status")
		}
	}
	return client.Post(path, map[string]interface{}{"version": version})
}

func runBackupMaintenance(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	path := "/software-updates/backups"
	switch action {
	case "list":
		if _, err := adminOptions(args, nil, nil, 0); err != nil {
			return nil, err
		}
		return backupRequestClient(client).Get(path)
	case "prune":
		o, err := adminOptions(args, []string{"worker"}, []string{"yes"}, 1)
		if err != nil {
			return nil, err
		}
		if err := adminConfirmed(o); err != nil {
			return nil, err
		}
		switch o.Positionals[0] {
		case "master":
			if o.Has("worker") {
				return nil, cliErrorf("用法: backup prune master --yes")
			}
			path += "/master/prune"
		case "agent":
			if !o.Has("worker") {
				return nil, cliErrorf("用法: backup prune agent --worker 节点 --yes")
			}
			if err := adminNodeAddress(o.Value("worker")); err != nil {
				return nil, err
			}
			path += "/agents/" + url.PathEscape(o.Value("worker")) + "/prune"
		default:
			return nil, cliErrorf("备份清理必须明确选择 master 或 agent")
		}
		return backupRequestClient(client).Post(path, map[string]interface{}{})
	default:
		return nil, cliErrorf("未知 backup 子命令: %s", action)
	}
}

// Backup enumeration can outlast the normal API deadline. Copy the clients so
// the longer deadline applies only to this request, preserving cancellation.
func backupRequestClient(client *ApiClient) *ApiClient {
	copy := *client
	httpClient := http.Client{}
	if client.client != nil {
		httpClient = *client.client
	}
	httpClient.Timeout = 120 * time.Second
	copy.client = &httpClient
	return &copy
}

func renderMaintenanceResult(out io.Writer, result map[string]interface{}) bool {
	data := responseData(result)
	master, ok := data["master"].(map[string]interface{})
	if !ok {
		return false
	}
	if _, ok := data["agents"]; !ok {
		return false
	}
	if _, ok := master["info"]; ok {
		renderSoftwareStatus(out, data, master)
		renderCLIAdminMessage(out, result)
		return true
	}
	if _, ok := master["count"]; ok {
		table := newCLITable(out)
		fmt.Fprintln(table, cliText(out, "节点\t备份数量\t占用空间\t可清理数量\t可清理空间\t状态"))
		printSummary := func(name string, row map[string]interface{}) {
			state := getString(row, "reason")
			if row["supported"] == true && state == "" {
				state = "AVAILABLE"
			} else if state == "" {
				state = "UNAVAILABLE"
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", safeCLIText(name), safeCLIText(row["count"]), formatSize(getInt64(row, "bytes")), safeCLIText(row["reclaimableCount"]), formatSize(getInt64(row, "reclaimableBytes")), safeCLIText(state))
		}
		printSummary("Master", master)
		for _, row := range objectList(data["agents"]) {
			printSummary(cliNodeLabel(row, getString(row, "address"), localeOf(out)), row)
		}
		table.Flush()
		renderCLIAdminMessage(out, result)
		return true
	}
	return false
}

func renderSoftwareStatus(out io.Writer, data, master map[string]interface{}) {
	masterInfo, _ := master["info"].(map[string]interface{})
	masterVersion := getString(masterInfo, "version")
	queue, _ := data["queue"].(map[string]interface{})
	queued := map[string]map[string]interface{}{}
	for _, item := range objectList(queue["items"]) {
		queued[getString(item, "address")] = item
	}
	table := newCLITable(out)
	fmt.Fprintln(table, cliText(out, "节点\t当前版本\t更新状态\t与 Master 版本\t说明"))
	printNode := func(name string, node map[string]interface{}, isMaster bool) {
		info, _ := node["info"].(map[string]interface{})
		update, _ := node["update"].(map[string]interface{})
		version := getString(info, "version")
		phase := getString(update, "phase")
		reason := firstNodeString(node, "upgradeReason")
		if reason == "" {
			reason = getString(info, "reason")
		}
		if errorText := getString(update, "error"); errorText != "" {
			reason = errorText
		}
		if item := queued[getString(node, "address")]; item != nil && !isMaster {
			phase = getString(item, "phase")
			if message := firstNodeString(item, "error", "reason"); message != "" {
				reason = message
			}
		}
		if node["unavailable"] == true {
			reason = "OFFLINE"
			if phase == "" {
				phase = "UNKNOWN"
			}
		}
		if phase == "" {
			phase = "IDLE"
			if info == nil {
				phase = "UNKNOWN"
			}
		}
		match := "—"
		if !isMaster {
			match = "UNKNOWN"
			if version != "" && masterVersion != "" && node["unavailable"] != true {
				match = "MISMATCH"
				if version == masterVersion {
					match = "MATCH"
				}
			}
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", safeCLIText(name), safeCLIText(version), safeCLIText(phase), match, safeCLIText(reason))
	}
	printNode("Master", master, true)
	for _, row := range objectList(data["agents"]) {
		printNode(cliNodeLabel(row, getString(row, "address"), localeOf(out)), row, false)
	}
	table.Flush()
	if latest, ok := data["latest"].(map[string]interface{}); ok {
		fmt.Fprintf(out, cliText(out, "最新版本: %s\n"), safeCLIText(latest["version"]))
	}
	if data["checking"] == true {
		fmt.Fprintln(out, cliText(out, "正在检查新版本"))
	}
	if checkError := getString(data, "checkError"); checkError != "" {
		fmt.Fprintln(out, safeCLIText(checkError))
	}
	if phase := getString(queue, "phase"); phase != "" && phase != "IDLE" {
		fmt.Fprintf(out, cliText(out, "Agent 更新队列: %s\n"), safeCLIText(phase))
	}
}
