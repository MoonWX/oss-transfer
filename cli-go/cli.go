package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"
)

type globalOptions struct {
	host                         string
	json, verbose, help, version bool
	watch                        time.Duration
	args                         []string
}

func parseGlobals(args []string) (globalOptions, error) {
	o := globalOptions{host: os.Getenv("OSS_TRANSFER_MASTER_SERVER")}
	if o.host == "" {
		o.host = "http://localhost:1685"
	}
	if len(args) == 1 && args[0] == "-v" {
		o.version = true
		return o, nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			o.args = append(o.args, args[i:]...)
			break
		}
		name, value, equals := strings.Cut(arg, "=")
		switch name {
		case "--host", "--watch":
			if !equals {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
					return o, fmt.Errorf("%s 缺少值", name)
				}
				i++
				value = args[i]
			}
			if name == "--host" {
				o.host = value
			} else {
				duration, err := time.ParseDuration(value)
				if err != nil {
					seconds, e := strconv.Atoi(value)
					if e != nil || seconds < 1 || seconds > 3600 {
						return o, fmt.Errorf("--watch 请输入 1–3600 秒，如 1 或 5s")
					}
					duration = time.Duration(seconds) * time.Second
				}
				if duration < time.Second || duration > time.Hour || duration%time.Second != 0 {
					return o, fmt.Errorf("--watch 必须为 1–3600 整数秒")
				}
				o.watch = duration
			}
		case "--json", "--verbose", "-v", "--help", "-h", "--version":
			if equals {
				return o, fmt.Errorf("%s 不接受值", name)
			}
			switch name {
			case "--json":
				o.json = true
			case "--verbose", "-v":
				o.verbose = true
			case "--help", "-h":
				o.help = true
			case "--version":
				o.version = true
			}
		default:
			o.args = append(o.args, arg)
		}
	}
	parsed, err := url.Parse(o.host)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Trim(parsed.Path, "/") != "" {
		return o, fmt.Errorf("--host 必须是服务 origin，例如 http://10.0.0.10:1685")
	}
	o.host = strings.TrimRight(o.host, "/")
	return o, nil
}
func runCLI(args []string, input io.Reader, output, errors io.Writer) int {
	options, err := parseGlobals(args)
	if err != nil {
		fmt.Fprintln(errors, "错误:", err)
		return 2
	}
	if options.version {
		if options.json {
			json.NewEncoder(output).Encode(map[string]string{"version": version, "branch": branch, "commit": commit, "buildTime": buildTime})
		} else {
			fmt.Fprintf(output, "OSS Transfer CLI V4\nVersion: %s\nBranch: %s\nCommit: %s\nBuildTime: %s\n", version, branch, commit, buildTime)
		}
		return 0
	}
	if len(options.args) == 0 || options.help || options.args[0] == "help" {
		fmt.Fprint(output, helpFor(options.args))
		return 0
	}
	normalized, err := normalizeCLICommand(options.args)
	if err != nil {
		fmt.Fprintln(errors, "错误:", err)
		return 2
	}
	if options.json && (normalized[0] == "diff-objects" || normalized[0] == "count-lines" || normalized[0] == "import-inventory") {
		fmt.Fprintln(errors, "错误: 此旧版离线辅助命令不支持 --json")
		return 2
	}
	if options.watch > 0 && !watchable(normalized) {
		fmt.Fprintln(errors, "错误: --watch 仅用于任务、节点、限速和异常的只读查询")
		return 2
	}
	client := NewApiClient(options.host)
	client.SetVerbose(options.verbose)
	ctx := context.Background()
	stop := func() {}
	if normalized[0] != "diff-objects" && normalized[0] != "count-lines" && normalized[0] != "login" {
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt)
	}
	defer stop()
	client.requestContext = ctx
	if os.Getenv("OSS_TRANSFER_AUTH_TOKEN") == "" && (os.Getenv("OSS_TRANSFER_INTERNAL_TOKEN") == "" || normalized[0] == "whoami" || normalized[0] == "logout" || normalized[0] == "change-password") && normalized[0] != "login" {
		token, err := loadSession(options.host)
		if err != nil {
			if normalized[0] == "logout" && len(normalized) == 1 {
				if removeErr := removeSession(options.host); removeErr == nil {
					result := map[string]interface{}{"success": true, "complete": false, "message": "本机会话已删除；原会话无法读取，服务端注销未确认"}
					if options.json {
						json.NewEncoder(output).Encode(result)
					} else {
						renderCLIResult(output, result)
					}
					fmt.Fprintln(errors, "提示: 本机会话已清理；服务端注销未确认。")
					return 3
				} else {
					err = fmt.Errorf("本机会话读取失败，且清理失败: %w", removeErr)
				}
			}
			if options.json {
				json.NewEncoder(output).Encode(map[string]interface{}{"success": false, "error": err.Error()})
			}
			fmt.Fprintln(errors, "错误:", err)
			return 1
		}
		if token != "" {
			client.SetAuthToken(token)
		}
	}
	for {
		result, callErr := dispatchCLI(client, normalized, input, options.host)
		if options.json {
			if result == nil {
				result = map[string]interface{}{}
			}
			if callErr != nil {
				result["success"] = false
				result["error"] = callErr.Error()
			}
			encoder := json.NewEncoder(output)
			if options.watch == 0 {
				encoder.SetIndent("", "  ")
			}
			if err := encoder.Encode(sanitizeOutput(result)); err != nil {
				fmt.Fprintln(errors, "错误:", err)
				return 1
			}
		} else if result != nil {
			if options.watch > 0 {
				fmt.Fprintln(output, time.Now().Format("2006-01-02 15:04:05"))
			}
			renderCLIResult(output, result)
		}
		if callErr != nil {
			fmt.Fprintln(errors, "错误:", callErr)
			if ctx.Err() != nil {
				return 130
			}
			return 1
		}
		partial := incompleteResult(result)
		if partial {
			fmt.Fprintln(errors, "提示: 返回不完整，当前结果不代表全量。")
			renderIncomplete(errors, result)
		}
		if options.watch == 0 {
			if partial {
				return 3
			}
			return 0
		}
		timer := time.NewTimer(options.watch)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 130
		case <-timer.C:
		}
	}
}
func watchable(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch args[0] {
	case "task":
		switch args[1] {
		case "list", "show", "children", "creation", "trash":
			return true
		case "bandwidth":
			return len(args) == 3
		}
	case "node":
		return args[1] == "list"
	case "limit":
		return args[1] == "list" || args[1] == "history" || args[1] == "adjustment"
	case "exceptions":
		return args[1] == "list"
	}
	return false
}
func dispatchCLI(client *ApiClient, args []string, input io.Reader, host string) (map[string]interface{}, error) {
	switch args[0] {
	case "task":
		return runTaskCommand(client, args[1:])
	case "node", "agent", "storage", "limit", "exceptions", "object":
		return runAdminCommand(client, args[0], args[1:])
	case "login", "logout", "whoami", "change-password":
		return runAuthCommand(client, args[0], args[1:], input, host)
	case "diff-objects":
		return nil, cmdDiffObjects(args[1:])
	case "count-lines":
		return nil, cmdCountLines(args[1:])
	case "import-inventory":
		return cmdImportInventory(client, args[1:])
	default:
		return nil, fmt.Errorf("未知命令 %q；使用 --help 查看用法", args[0])
	}
}

// Keep commonly used names while routing actions through the same parent-task API as WebUI.
func normalizeCLICommand(args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	command, rest := args[0], append([]string{}, args[1:]...)
	replacements := map[string]string{"--dateAfter": "--date-after", "--dateBefore": "--date-before", "--taskIndex": "--task-id"}
	for i, arg := range rest {
		key, value, hasValue := strings.Cut(arg, "=")
		if renamed, ok := replacements[key]; ok {
			rest[i] = renamed
			if hasValue {
				rest[i] += "=" + value
			}
		}
	}
	aliases := map[string][]string{
		"list-task": {"task", "list"}, "status": {"task", "list"}, "show-task": {"task", "show"}, "stop": {"task", "stop"}, "resume": {"task", "resume"}, "start": {"task", "start"}, "retry": {"task", "retry"}, "delete-task": {"task", "delete"}, "clear": {"task", "delete"}, "list-deleted": {"task", "trash"}, "permanent-delete": {"task", "purge"}, "export-report": {"task", "report"},
		"list-worker": {"node", "list"}, "worker-status": {"node", "list"}, "list-rate-limit": {"limit", "list"}, "rate-limit": {"limit", "list"}, "list-rate-limit-history": {"limit", "history"}, "rate-limit-history": {"limit", "history"}, "list-s3-config": {"storage", "list"}, "remove-s3-config": {"storage", "remove"},
		"list-failed-objs": {"exceptions", "list", "failed"}, "failed": {"exceptions", "list", "failed"}, "list-fallback-objs": {"exceptions", "list", "fallback"}, "fallback": {"exceptions", "list", "fallback"}, "delete-failed-objs": {"exceptions", "clear", "failed", "--all"}, "clear-failed": {"exceptions", "clear", "failed", "--all"}, "delete-fallback-objs": {"exceptions", "clear", "fallback", "--all"}, "clear-fallback": {"exceptions", "clear", "fallback", "--all"},
		"add-task": {"task", "create", "oss-to-oss"}, "import-tasks": {"task", "import-legacy"},
	}
	if command == "set-s3-config" {
		return nil, fmt.Errorf("请使用 storage add/update；凭据通过 --access-key-env/--secret-key-env 或文件读取，避免出现在命令历史中")
	}
	if command == "set-rate-limit" {
		if len(rest) < 2 || len(rest) > 3 {
			return nil, fmt.Errorf("用法: set-rate-limit <list|put|head|bandwidth> <值> [IP]")
		}
		kind, value := rest[0], rest[1]
		converted := []string{"limit", "set", kind, value}
		if kind == "bandwidth" {
			unit := "bps"
			if len(value) > 0 {
				switch strings.ToUpper(value[len(value)-1:]) {
				case "K":
					unit = "Kbps"
					value = value[:len(value)-1]
				case "M":
					unit = "Mbps"
					value = value[:len(value)-1]
				case "G":
					unit = "Gbps"
					value = value[:len(value)-1]
				}
			}
			converted[3] = value
			converted = append(converted, "--unit", unit)
		}
		if value == "0" {
			converted[3] = "unlimited"
		}
		if len(rest) == 3 {
			converted = append(converted, "--worker", rest[2])
		} else {
			converted = append(converted, "--all")
		}
		return converted, nil
	}
	if command == "list-rate-limit-history" || command == "rate-limit-history" {
		if len(rest) == 1 && !strings.HasPrefix(rest[0], "-") {
			rest = []string{"--limit", rest[0]}
		}
	}
	if command == "list-deleted" {
		if len(rest) == 1 && (rest[0] == "-a" || rest[0] == "--all") {
			rest = nil
		}
	}
	if prefix, ok := aliases[command]; ok {
		return append(prefix, rest...), nil
	}
	return args, nil
}
func sanitizeOutput(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		copy := map[string]interface{}{}
		for k, item := range v {
			key := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "_", ""), "-", ""))
			if strings.Contains(key, "password") || key == "secretkey" || key == "accesskey" || key == "internaltoken" || key == "token" {
				copy[k] = "***"
			} else {
				copy[k] = sanitizeOutput(item)
			}
		}
		return copy
	case []map[string]interface{}:
		list := []interface{}{}
		for _, item := range v {
			list = append(list, sanitizeOutput(item))
		}
		return list
	case []interface{}:
		list := make([]interface{}, len(v))
		for i, item := range v {
			list[i] = sanitizeOutput(item)
		}
		return list
	default:
		return value
	}
}
func safeCLIText(value interface{}) string {
	s := fmt.Sprint(value)
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s)
}
func renderCLIResult(out io.Writer, result map[string]interface{}) {
	data := responseData(result)
	if renderAdminTable(out, result, data) {
		return
	}
	if value, ok := data["tasks"]; ok {
		renderTaskRows(out, objectList(value), false)
		fmt.Fprintf(out, "显示 %d 个主任务\n", len(objectList(value)))
		if msg := getString(data, "message"); msg != "" {
			fmt.Fprintln(out, safeCLIText(msg))
		}
		return
	}
	if task, ok := result["task"].(map[string]interface{}); ok {
		if child, ok := result["child"].(map[string]interface{}); ok {
			fmt.Fprintf(out, "子任务 #%s\n", getString(child, "childTaskId"))
			renderTaskDetail(out, child)
		} else {
			fmt.Fprintf(out, "任务 #%d\n", getInt(task, "taskIndex"))
			renderTaskDetail(out, task)
			if children, ok := result["children"]; ok {
				fmt.Fprintln(out, "子任务")
				renderTaskRows(out, objectList(children), true)
			}
		}
		return
	}
	if children, ok := result["children"]; ok {
		renderTaskRows(out, objectList(children), true)
		return
	}
	// Keep envelope fields such as submissionStatus and node status. Unwrapping
	// data for generic output would silently discard these operation results.
	clean, _ := sanitizeOutput(result).(map[string]interface{})
	renderFields(out, clean, "")
}

func renderAdminTable(out io.Writer, result, data map[string]interface{}) bool {
	if getString(data, "joinUrl") != "" && getString(data, "dockerCommand") != "" {
		fmt.Fprintln(out, "Agent 加入链接（仅可加入一次）")
		fmt.Fprintln(out, safeCLIText(data["joinUrl"]))
		if expiry := getString(data, "expiresAt"); expiry != "" {
			fmt.Fprintf(out, "有效至 %s\n", safeCLIText(expiry))
		}
		fmt.Fprintln(out, "\nDocker")
		fmt.Fprintln(out, safeCLIText(data["dockerCommand"]))
		fmt.Fprintln(out, "\n传统部署")
		fmt.Fprintln(out, safeCLIText(data["traditionalCommand"]))
		return true
	}
	if raw, ok := data["configs"]; ok {
		configs := objectList(raw)
		w := newCLITable(out)
		fmt.Fprintln(w, "连接名称\tEndpoint\tAccess Key\tSecret Key\tV2 签名\tPath Style")
		for _, config := range configs {
			credential := func(key string) string {
				if value, ok := config[key].(string); ok && value != "" {
					return "已配置"
				}
				return "未配置"
			}
			option := func(key string) string {
				if value, ok := config[key].(bool); ok {
					if value {
						return "开启"
					}
					return "关闭"
				}
				return "继承"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", safeCLIText(config["alias"]), safeCLIText(config["endpoint"]), credential("accessKey"), credential("secretKey"), option("useV2SignerType"), option("pathStyleEnable"))
		}
		w.Flush()
		fmt.Fprintf(out, "共 %d 个存储连接\n", len(configs))
		renderCLIAdminMessage(out, result)
		return true
	}
	if raw, ok := data["nodes"]; ok {
		nodes := objectList(raw)
		// The local precheck also returns nodes; only registry entries have address.
		if len(nodes) > 0 && getString(nodes[0], "address") == "" {
			return false
		}
		workers, _ := result["workers"].(map[string]interface{})
		w := newCLITable(out)
		fmt.Fprintln(w, "节点地址\t名称\t角色\t连接状态\t统计口径\t进行中\t待执行\t已完成")
		hasCurrent, hasLegacy := false, false
		for _, node := range nodes {
			address := getString(node, "address")
			worker, _ := workers[address].(map[string]interface{})
			state := "未读取"
			if worker != nil {
				state = "无法连接"
				if worker["success"] == true {
					state = "在线"
				}
			}
			role := "执行节点"
			if node["agent"] == true {
				role = "Agent"
				state = "离线"
				if node["online"] == true {
					state = "在线"
				}
			}
			if node["master"] == true {
				role = "主节点 / 执行节点"
			}
			scope, running, pending, completed := cliNodeTaskCounts(worker)
			hasCurrent = hasCurrent || scope == "当前子任务"
			hasLegacy = hasLegacy || scope == "旧版记录"
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", safeCLIText(address), safeCLIText(getString(node, "label")), role, state, scope, running, pending, completed)
		}
		w.Flush()
		fmt.Fprintf(out, "共 %d 个执行节点\n", len(nodes))
		if hasCurrent {
			fmt.Fprintln(out, "按节点当前保留的子任务计数；进行中包含扫描后传输。详细口径见 node --help。")
		}
		if hasLegacy {
			fmt.Fprintln(out, "旧版记录沿用旧统计口径，升级节点后可统一。")
		}
		for _, node := range nodes {
			address := getString(node, "address")
			worker, _ := workers[address].(map[string]interface{})
			if worker["success"] != true {
				if message := getString(worker, "message"); message != "" {
					fmt.Fprintf(out, "%s: %s\n", safeCLIText(address), safeCLIText(message))
				}
			}
		}
		renderCLIAdminMessage(out, result)
		return true
	}
	if raw, ok := data["rateLimits"]; ok {
		w := newCLITable(out)
		fmt.Fprintln(w, "节点\t类型\t目标上限\t目标来源\t运行上限\t实测速率\t状态")
		for _, node := range objectList(raw) {
			for _, kind := range []string{"list", "put", "head", "bandwidth"} {
				rate, _ := node[kind].(map[string]interface{})
				unit, suffix := " 次/秒", "Qps"
				if kind == "bandwidth" {
					unit, suffix = " Mbps", "Bps"
				}
				format := func(value interface{}, limit bool) string {
					if value == nil {
						return "—"
					}
					number, ok := cliNumeric(value)
					if !ok {
						return "—"
					}
					if limit && number <= 0 {
						return "不限速"
					}
					if kind == "bandwidth" {
						number /= 1e6
					}
					return strconv.FormatFloat(number, 'f', -1, 64) + unit
				}
				target := rate["manualLimit"+suffix]
				if target == nil {
					target = rate["configuredLimit"+suffix]
				}
				state := getString(rate, "state")
				if translated, ok := map[string]string{"NORMAL": "正常", "REDUCED": "已降速", "RECOVERING": "恢复中", "RECOVERED": "已恢复", "UNLIMITED": "不限速"}[state]; ok {
					state = translated
				}
				if state == "" {
					state = "—"
				}
				source := "配置目标"
				if rate["limitSource"] == "PERSISTED_MANUAL" {
					source = "已保存目标"
				}
				if target == nil {
					source = "—"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", safeCLIText(node["workerIp"]), kind, format(target, true), source, format(rate["limit"+suffix], true), format(rate["current"+suffix], false), safeCLIText(state))
			}
		}
		w.Flush()
		if len(objectList(raw)) == 0 {
			fmt.Fprintln(out, "暂无返回的节点限速记录")
		}
		renderCLIAdminMessage(out, result)
		return true
	}
	return false
}

func cliNodeTaskCounts(worker map[string]interface{}) (scope, running, pending, completed string) {
	if worker["success"] != true {
		return "—", "—", "—", "—"
	}
	stats, _ := worker["executionTaskStats"].(map[string]interface{})
	current := getString(stats, "scope") == "CURRENT_EXECUTION_SUBTASKS"
	scope = "当前子任务"
	if !current {
		scope = "旧版记录"
		stats, _ = worker["taskStats"].(map[string]interface{})
	}
	count := func(keys ...string) string {
		total := float64(0)
		for _, key := range keys {
			value, ok := cliNumeric(stats[key])
			if !ok || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || math.Trunc(value) != value {
				return "—"
			}
			total += value
		}
		return strconv.FormatFloat(total, 'f', 0, 64)
	}
	running = count("runningTasks")
	if current {
		running = count("runningTasks", "listDoneTasks")
	}
	return scope, running, count("pendingTasks"), count("completedTasks")
}

func cliNumeric(value interface{}) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case json.Number:
		value, err := number.Float64()
		return value, err == nil
	}
	return 0, false
}

func renderCLIAdminMessage(out io.Writer, result map[string]interface{}) {
	seen := map[string]bool{}
	for _, source := range []map[string]interface{}{result, responseData(result)} {
		for _, key := range []string{"message", "warning", "submissionNote"} {
			if message := getString(source, key); message != "" && !seen[message] {
				fmt.Fprintln(out, safeCLIText(message))
				seen[message] = true
			}
		}
	}
}
func renderTaskRows(out io.Writer, tasks []map[string]interface{}, children bool) {
	if len(tasks) == 0 {
		fmt.Fprintln(out, "暂无记录")
		return
	}
	w := newCLITable(out)
	fmt.Fprintln(w, "ID\t方向\t状态\t来源\t目标\t已迁移/已扫描\t跳过\t失败\t执行节点")
	for _, task := range tasks {
		id := fmt.Sprintf("#%d", getInt(task, "taskIndex"))
		if children {
			id = "#" + getString(task, "childTaskId")
		}
		status := getString(task, "status")
		if task["creationStatus"] == "QUEUED" || task["creationStatus"] == "RUNNING" {
			status += " (扫描中)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d/%d\t%d\t%d\t%s\n", id, taskDirection(task), safeCLIText(status), safeCLIText(taskEndpoint(task, true)), safeCLIText(taskEndpoint(task, false)), getInt64(task, "putObjectCount"), getInt64(task, "listObjectCount"), getInt64(task, "skippedObjectCount"), getInt64(task, "failedObjectCount"), safeCLIText(taskWorkers(task)))
	}
	w.Flush()
}
func renderTaskDetail(out io.Writer, task map[string]interface{}) {
	policy := getString(task, "conflictPolicy")
	if policy == "" {
		policy = "SKIP"
		if task["overwrite"] == true {
			policy = "OVERWRITE"
		}
	}
	bandwidth := "仅受节点上限约束"
	if value, ok := task["taskBandwidthLimitMbps"]; ok && value != nil {
		bandwidth = fmt.Sprint(value, " Mbps")
	}
	fields := [][2]string{{"方向", taskDirection(task)}, {"状态", getString(task, "status")}, {"来源", taskEndpoint(task, true)}, {"目标", taskEndpoint(task, false)}, {"执行节点", taskWorkers(task)}, {"冲突策略", policy}, {"分发方式", getString(task, "distributionMode")}, {"任务带宽", bandwidth}, {"已扫描", fmt.Sprintf("%d / %s", getInt64(task, "listObjectCount"), formatSize(getInt64(task, "listObjectSize")))}, {"已迁移", fmt.Sprintf("%d / %s", getInt64(task, "putObjectCount"), formatSize(getInt64(task, "putObjectSize")))}, {"已跳过", fmt.Sprint(getInt64(task, "skippedObjectCount"))}, {"失败", fmt.Sprint(getInt64(task, "failedObjectCount"))}, {"当前速度", formatSize(getInt64(task, "currentSpeedBytesPerSecond")) + "/s"}, {"平均速度", formatSize(getInt64(task, "averageSpeedBytesPerSecond")) + "/s"}, {"耗时", getString(task, "duration")}, {"创建时间", getString(task, "createTime")}, {"开始时间", getString(task, "startTime")}, {"结束时间", getString(task, "endTime")}, {"起始 Key", getString(task, "startKey")}, {"结束 Key", getString(task, "endKey")}}
	w := newCLITable(out)
	for _, field := range fields {
		if field[1] != "" {
			fmt.Fprintf(w, "%s\t%s\n", field[0], safeCLIText(field[1]))
		}
	}
	w.Flush()
}

var fieldLabels = map[string]string{"success": "成功", "message": "说明", "error": "错误", "data": "结果", "nodes": "节点", "workers": "执行节点", "taskIds": "任务 ID", "parentTaskIds": "主任务 ID", "creationIds": "创建记录 ID", "creationId": "创建记录 ID", "submissionStatus": "受理状态", "requestedCount": "请求数量", "accepted": "已受理", "ignored": "已忽略", "failed": "失败", "complete": "返回完整", "partial": "部分结果", "unreachableWorkers": "未响应节点", "failedWorkers": "失败节点", "details": "详情", "output": "保存位置", "address": "地址", "label": "名称", "executionNodeIp": "节点 IP", "reachable": "可达", "status": "状态", "limitMbps": "任务带宽 Mbps"}

func renderFields(out io.Writer, data map[string]interface{}, indent string) {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := data[key]
		if value == nil {
			continue
		}
		label := fieldLabels[key]
		if label == "" {
			label = key
		}
		switch v := value.(type) {
		case map[string]interface{}:
			fmt.Fprintf(out, "%s%s:\n", indent, label)
			renderFields(out, v, indent+"  ")
		case []interface{}:
			if len(v) == 0 {
				fmt.Fprintf(out, "%s%s: []\n", indent, label)
				continue
			}
			fmt.Fprintf(out, "%s%s:\n", indent, label)
			for _, item := range v {
				if m, ok := item.(map[string]interface{}); ok {
					fmt.Fprintf(out, "%s  -\n", indent)
					renderFields(out, m, indent+"    ")
				} else {
					fmt.Fprintf(out, "%s  %s\n", indent, safeCLIText(item))
				}
			}
		default:
			fmt.Fprintf(out, "%s%s: %s\n", indent, label, safeCLIText(v))
		}
	}
}

func renderIncomplete(out io.Writer, result map[string]interface{}) {
	seen := map[string]bool{}
	write := func(context, label string, value interface{}) {
		line := label + ": " + safeCLIText(value)
		if context != "" {
			line = context + " · " + line
		}
		if !seen[line] {
			fmt.Fprintln(out, line)
			seen[line] = true
		}
	}
	var visit func(map[string]interface{}, string)
	visit = func(current map[string]interface{}, context string) {
		if id := getInt(current, "taskIndex"); id > 0 {
			context = fmt.Sprintf("任务 #%d", id)
		}
		for _, entry := range [][2]string{{"failedWorkers", "失败节点"}, {"unreachableWorkers", "未响应节点"}, {"unavailableWorkers", "不可用节点"}, {"truncatedWorkers", "记录被截断的节点"}, {"missingTaskIds", "缺失子任务 ID"}, {"errors", "错误详情"}, {"workerErrors", "节点错误"}, {"nodeErrors", "节点错误"}} {
			if value := current[entry[0]]; nonEmptyCLICollection(value) {
				write(context, entry[1], value)
			}
		}
		if current["snapshotComplete"] == false {
			write(context, "任务快照", "不完整")
		}
		if current["historyComplete"] == false {
			write(context, "调节历史", "不完整")
		}
		if message := getString(current, "warning"); message != "" {
			write(context, "提示", message)
		}
		for _, key := range []string{"data", "task", "child", "localDetails"} {
			if nested, ok := current[key].(map[string]interface{}); ok {
				next := context
				if key == "localDetails" {
					next = "本地任务详情"
				}
				visit(nested, next)
			}
		}
		for _, key := range []string{"tasks", "children", "shards"} {
			for _, task := range objectList(current[key]) {
				visit(task, context)
			}
		}
		if groups, ok := current["deletedTasks"].(map[string]interface{}); ok {
			keys := make([]string, 0, len(groups))
			for key := range groups {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				for _, task := range objectList(groups[key]) {
					visit(task, context)
				}
			}
		}
	}
	visit(result, "")
}
