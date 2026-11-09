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

var maintenanceVersion = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
var prereleaseIdentifier = regexp.MustCompile(`^[0-9A-Za-z-]+$`)
var numericIdentifier = regexp.MustCompile(`^[0-9]+$`)

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

// releaseVersion mirrors the service's SemVer rules: MAJOR.MINOR.PATCH with an
// optional prerelease (4.1.29-beta.1), no leading zeros, no build metadata and
// bounded lengths. It reports whether the version is a prerelease.
func releaseVersion(value string) (string, bool, bool) {
	value = strings.TrimPrefix(value, "v")
	core, pre, hasPre := strings.Cut(value, "-")
	if len(value) > 64 || !maintenanceVersion.MatchString(core) {
		return "", false, false
	}
	if !hasPre {
		return value, false, true
	}
	identifiers := strings.Split(pre, ".")
	if len(identifiers) > 8 || identifiers[0] == "dev" {
		return "", false, false
	}
	for _, identifier := range identifiers {
		if len(identifier) > 32 || !prereleaseIdentifier.MatchString(identifier) || numericIdentifier.MatchString(identifier) && (len(identifier) > 9 || len(identifier) > 1 && identifier[0] == '0') {
			return "", false, false
		}
	}
	return value, true, true
}

func updateVersion(value string) (string, bool, error) {
	version, prerelease, ok := releaseVersion(value)
	if !ok {
		return "", false, cliErrorf("--version 必须是发布版本号，如 4.1.22")
	}
	return version, prerelease, nil
}

// compareReleases orders two versions accepted by releaseVersion by SemVer
// precedence. Unparseable versions compare as equal so nothing is replaced.
func compareReleases(left, right string) int {
	a, _, okA := releaseVersion(left)
	b, _, okB := releaseVersion(right)
	if !okA || !okB {
		return 0
	}
	coreA, preA, hasA := strings.Cut(a, "-")
	coreB, preB, hasB := strings.Cut(b, "-")
	partsA, partsB := strings.Split(coreA, "."), strings.Split(coreB, ".")
	for i := range partsA {
		if len(partsA[i]) != len(partsB[i]) {
			return len(partsA[i]) - len(partsB[i])
		}
		if c := strings.Compare(partsA[i], partsB[i]); c != 0 {
			return c
		}
	}
	switch {
	case !hasA && !hasB:
		return 0
	case !hasA:
		return 1
	case !hasB:
		return -1
	}
	idsA, idsB := strings.Split(preA, "."), strings.Split(preB, ".")
	for i := 0; i < len(idsA) && i < len(idsB); i++ {
		numericA, numericB := numericIdentifier.MatchString(idsA[i]), numericIdentifier.MatchString(idsB[i])
		switch {
		case numericA && numericB && len(idsA[i]) != len(idsB[i]):
			return len(idsA[i]) - len(idsB[i])
		case numericA != numericB:
			if numericA {
				return -1
			}
			return 1
		}
		if c := strings.Compare(idsA[i], idsB[i]); c != 0 {
			return c
		}
	}
	return len(idsA) - len(idsB)
}

// softwareCheck asks the Master to check for the latest stable release and
// reports it with the Master's current version.
func softwareCheck(client *ApiClient) (map[string]interface{}, string, string, error) {
	result, err := client.Post("/software-updates/check", map[string]interface{}{})
	if err != nil {
		return result, "", "", err
	}
	latest, _, ok := releaseVersion(getString(adminData(result), "version"))
	if !ok {
		return nil, "", "", cliErrorf("Master 未返回有效的最新版本，请稍后重试 update check")
	}
	snapshot, err := client.Get("/software-updates")
	if err != nil {
		return snapshot, "", "", err
	}
	master, _ := adminData(snapshot)["master"].(map[string]interface{})
	info, _ := master["info"].(map[string]interface{})
	return result, latest, strings.TrimPrefix(getString(info, "version"), "v"), nil
}

func runSoftwareUpdate(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	if action == "status" || action == "check" {
		if _, err := adminOptions(args, nil, nil, 0); err != nil {
			return nil, asUsageError(err)
		}
		if action == "check" {
			// The check result keeps its fields; currentVersion and available
			// state the conclusion against the running Master.
			result, latest, current, err := softwareCheck(client)
			if err != nil {
				return result, err
			}
			data := adminData(result)
			data["currentVersion"] = current
			data["available"] = current != "" && compareReleases(latest, current) > 0
			return result, nil
		}
		return client.Get("/software-updates")
	}
	if action != "master" && action != "agent" && action != "agents" {
		return nil, cliUsageErrorf("未知 update 子命令: %s", action)
	}
	o, err := adminOptions(args, []string{"version", "worker"}, []string{"all", "yes", "allow-prerelease"}, 0)
	if err != nil {
		return nil, asUsageError(err)
	}
	if err := adminConfirmed(o); err != nil {
		return nil, asUsageError(err)
	}
	path := "/software-updates/" + action
	switch action {
	case "master":
		if o.Has("worker") || o.Has("all") {
			return nil, cliUsageErrorf("用法: update master [--version 版本] [--allow-prerelease] --yes")
		}
	case "agent":
		if !o.Has("worker") || o.Has("all") {
			return nil, cliUsageErrorf("用法: update agent --worker 节点 [--version 版本] --yes")
		}
		if err := adminNodeAddress(o.Value("worker")); err != nil {
			return nil, asUsageError(err)
		}
		path = "/software-updates/agents/" + url.PathEscape(o.Value("worker"))
	case "agents":
		if !o.Has("all") || o.Has("worker") {
			return nil, cliUsageErrorf("用法: update agents --all [--version 版本] --yes")
		}
	}
	version := ""
	if o.Has("version") {
		var prerelease bool
		version, prerelease, err = updateVersion(o.Value("version"))
		if err != nil {
			return nil, asUsageError(err)
		}
		// A chosen prerelease is an experimental build that normal updates
		// never offer; installing it needs a second, explicit flag.
		if prerelease && !o.Has("allow-prerelease") {
			return nil, cliUsageErrorf("%s 是预发布（实验）版本，正常更新不会提供；确认安装请同时指定 --allow-prerelease", version)
		}
		if action == "master" && prerelease && o.Has("allow-prerelease") {
			return client.Post(path, map[string]interface{}{"version": version, "allowPrerelease": true})
		}
	}
	if action == "master" {
		// The Master installs the latest stable release it has just checked.
		// Check on the user's behalf and default to that release.
		result, latest, current, err := softwareCheck(client)
		if err != nil {
			return result, err
		}
		if version != "" && version != latest {
			return nil, cliErrorf("在线更新只能安装最新正式版 v%s；如需安装其他版本，请在 Master 主机运行安装脚本并指定 --version", latest)
		}
		if current != "" && compareReleases(latest, current) <= 0 {
			return map[string]interface{}{"success": true, "data": map[string]interface{}{"currentVersion": current, "version": latest, "upToDate": true}}, nil
		}
		return client.Post(path, map[string]interface{}{"version": latest})
	}
	if version == "" {
		// Agents follow the running Master's version, never a newer release or
		// an inferred local CLI version. The server rechecks this when queuing.
		snapshot, err := client.Get("/software-updates")
		if err != nil {
			return snapshot, err
		}
		master, _ := adminData(snapshot)["master"].(map[string]interface{})
		info, _ := master["info"].(map[string]interface{})
		// The Master's own version is the explicit choice, so Agents may
		// follow a prerelease Master without --allow-prerelease.
		version, _, err = updateVersion(getString(info, "version"))
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
	if _, ok := data["currentVersion"]; ok && data["master"] == nil {
		renderSoftwareCheck(out, data)
		return true
	}
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

// renderSoftwareCheck states the result of update check, and of update master
// when the Master is already up to date, as one current → available line.
func renderSoftwareCheck(out io.Writer, data map[string]interface{}) {
	current := strings.TrimPrefix(getString(data, "currentVersion"), "v")
	latest := strings.TrimPrefix(getString(data, "version"), "v")
	switch {
	case data["upToDate"] == true && current == latest:
		fmt.Fprintf(out, cliText(out, "Master 已是最新正式版 v%s，无需更新\n"), safeCLIText(latest))
	case data["upToDate"] == true:
		fmt.Fprintf(out, cliText(out, "Master 当前版本 v%s 不低于最新正式版 v%s，无需更新\n"), safeCLIText(current), safeCLIText(latest))
	case current == "":
		fmt.Fprintf(out, cliText(out, "最新正式版: v%s\n"), safeCLIText(latest))
	case data["available"] == true:
		fmt.Fprintf(out, cliText(out, "当前版本 v%s → 可升级到 v%s\n"), safeCLIText(current), safeCLIText(latest))
		if notes := getString(data, "url"); notes != "" {
			fmt.Fprintf(out, cliText(out, "发布说明: %s\n"), safeCLIText(notes))
		}
		fmt.Fprintln(out, cliText(out, "升级 Master: oss-transfer-cli update master --yes；完成后升级执行节点: oss-transfer-cli update agents --all --yes"))
	default:
		fmt.Fprintf(out, cliText(out, "当前版本 v%s，已是最新正式版（v%s）\n"), safeCLIText(current), safeCLIText(latest))
	}
}

// softwarePhaseText and softwareReasonText translate the update status codes
// for human output; --json keeps the codes.
func softwarePhaseText(out io.Writer, phase string) string {
	key := map[string]string{
		"IDLE": "空闲", "UNKNOWN": "未知", "QUEUED": "已排队", "DOWNLOADING": "正在下载", "APPLYING": "正在安装",
		"COMPLETED": "已完成", "FAILED": "失败", "RUNNING": "进行中", "UPDATING": "正在更新", "SKIPPED": "已跳过",
	}[phase]
	if key == "" {
		return safeCLIText(phase)
	}
	return cliText(out, key)
}

func softwareReasonText(out io.Writer, reason string) string {
	key := map[string]string{
		"OFFLINE":            "节点离线，重新连接后可更新",
		"LEGACY":             "需先用安装脚本升级一次，之后可在线更新",
		"CONTAINER":          "容器部署：请拉取新镜像并重建容器来更新",
		"UNMANAGED":          "此安装方式不支持在线更新，请使用安装脚本升级",
		"PERMISSION":         "服务权限不足，无法在线更新，请使用安装脚本升级",
		"MISSING_DEPENDENCY": "缺少在线更新所需的系统组件，请使用安装脚本升级",
		"UNSUPPORTED":        "此平台不支持在线更新，请使用安装脚本升级",
		"RECONNECTING":       "等待节点重新连接",
		"UP_TO_DATE":         "已是 Master 版本",
	}[reason]
	if key == "" {
		return safeCLIText(reason)
	}
	return cliText(out, key)
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
		// Codes are translated; server messages (details, errors) arrive
		// already localized and are shown as they are.
		reason := softwareReasonText(out, firstNodeString(node, "upgradeReason"))
		if reason == "" {
			reason = softwareReasonText(out, getString(info, "reason"))
		}
		if detail := getString(info, "detail"); detail != "" && info["supported"] != true && node["unavailable"] != true {
			reason = safeCLIText(detail)
		}
		if errorText := getString(update, "error"); errorText != "" {
			reason = safeCLIText(errorText)
		}
		if item := queued[getString(node, "address")]; item != nil && !isMaster {
			phase = getString(item, "phase")
			if message := getString(item, "error"); message != "" {
				reason = safeCLIText(message)
			} else if code := getString(item, "reason"); code != "" {
				reason = softwareReasonText(out, code)
			}
		}
		if node["unavailable"] == true {
			reason = softwareReasonText(out, "OFFLINE")
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
			match = cliText(out, "未知")
			if version != "" && masterVersion != "" && node["unavailable"] != true {
				match = cliText(out, "不一致")
				if version == masterVersion {
					match = cliText(out, "一致")
				}
			}
		}
		shown := ""
		if version != "" {
			shown = "v" + strings.TrimPrefix(version, "v")
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", safeCLIText(name), safeCLIText(shown), softwarePhaseText(out, phase), match, reason)
	}
	printNode("Master", master, true)
	for _, row := range objectList(data["agents"]) {
		printNode(cliNodeLabel(row, getString(row, "address"), localeOf(out)), row, false)
	}
	table.Flush()
	if latest, ok := data["latest"].(map[string]interface{}); ok {
		fmt.Fprintf(out, cliText(out, "最新版本: v%s\n"), safeCLIText(strings.TrimPrefix(getString(latest, "version"), "v")))
	}
	if data["checking"] == true {
		fmt.Fprintln(out, cliText(out, "正在检查新版本"))
	}
	if checkError := getString(data, "checkError"); checkError != "" {
		fmt.Fprintln(out, safeCLIText(checkError))
	}
	if phase := getString(queue, "phase"); phase != "" && phase != "IDLE" {
		fmt.Fprintf(out, cliText(out, "Agent 更新队列: %s\n"), softwarePhaseText(out, phase))
	}
}
