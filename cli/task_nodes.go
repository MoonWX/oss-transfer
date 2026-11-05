package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// task nodes changes the execution Agents of an existing task. Membership is
// read first so the change carries the revision the operator reviewed; the
// Master rejects it when another change landed in between.
func taskNodes(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, []string{"add", "remove", "binding", "request-id", "expected-revision"}, []string{"now", "yes"})
	if err != nil {
		return nil, asUsageError(err)
	}
	if len(opts.Positionals) != 1 {
		return nil, asUsageError(cliErrorf("用法: task nodes <ID> [--add 节点] [--remove 节点] [--binding 节点=/绝对路径] [--now] [--yes]"))
	}
	id, err := taskID(opts.Positionals[0])
	if err != nil {
		return nil, asUsageError(err)
	}
	add, err := taskNodeSelectors(opts.Values["add"], "add")
	if err != nil {
		return nil, asUsageError(err)
	}
	remove, err := taskNodeSelectors(opts.Values["remove"], "remove")
	if err != nil {
		return nil, asUsageError(err)
	}
	removing := map[string]bool{}
	for _, node := range remove {
		removing[node] = true
	}
	for _, node := range add {
		if removing[node] {
			return nil, asUsageError(cliErrorf("同一节点不能同时加入和移出: %s", node))
		}
	}
	bindings, err := taskNodeBindings(opts.Values["binding"], add)
	if err != nil {
		return nil, asUsageError(err)
	}
	if opts.Has("now") && len(remove) == 0 {
		return nil, asUsageError(cliErrorf("--now 仅与 --remove 一起使用"))
	}
	changing := len(add) > 0 || len(remove) > 0
	if !changing {
		for _, flag := range []string{"yes", "request-id", "expected-revision"} {
			if opts.Has(flag) {
				return nil, asUsageError(cliErrorf("--%s 仅用于调整执行节点，请同时指定 --add 或 --remove", flag))
			}
		}
	}
	requestID, err := creationRequestIDOption(opts)
	if err != nil {
		return nil, asUsageError(err)
	}
	expectedRevision := -1
	if opts.Has("expected-revision") {
		n, err := strconv.Atoi(opts.Value("expected-revision"))
		if err != nil || n < 0 || len(opts.Values["expected-revision"]) > 1 {
			return nil, asUsageError(cliErrorf("--expected-revision 须为非负整数且只能填写一次"))
		}
		expectedRevision = n
	}
	path := fmt.Sprintf("/tasks/%d/nodes", id)
	current, err := client.Get(path)
	if err != nil || !changing {
		return current, err
	}
	view := responseData(current)
	if _, ok := view["nodeRevision"]; !ok {
		return nil, cliErrorf("服务未返回任务 #%d 的执行节点信息；请升级 Master 后重试", id)
	}
	if view["editable"] == false {
		return current, cliErrorf("任务 #%d 当前不能调整执行节点: %s", id, getString(view, "reason"))
	}
	mode := "drain"
	if opts.Has("now") {
		mode = "now"
		if view["nowSupported"] == false {
			return current, cliErrorf("任务 #%d 不支持立即移出: %s", id, getString(view, "nowReason"))
		}
	}
	local := view["localTransfer"] == true
	// Re-adding a draining node (or one being removed immediately but not yet
	// fenced) cancels its removal and keeps its existing path; only nodes that
	// actually join need a binding.
	draining := map[string]bool{}
	for _, node := range objectList(view["nodes"]) {
		if state := getString(node, "state"); state == "DRAINING" || state == "REMOVING" {
			draining[getString(node, "address")], draining[getString(node, "id")] = true, true
		}
	}
	joins := 0
	for _, node := range add {
		if !draining[node] {
			joins++
		}
	}
	// A retry with --request-id may replay a change the Master already
	// accepted, after which the current view no longer shows the cancelled
	// removal. The Master's request fingerprint is the authority then.
	retry := opts.Has("request-id")
	for _, binding := range bindings {
		if !retry && draining[getString(binding.(map[string]interface{}), "executionNodeIp")] {
			return nil, asUsageError(cliErrorf("重新加入排空中的节点会沿用原路径，无需 --binding: %s", getString(binding.(map[string]interface{}), "executionNodeIp")))
		}
	}
	if local && !retry && len(bindings) != joins {
		return nil, asUsageError(cliErrorf("本地迁移任务需为每个加入的节点指定一个 --binding 节点=/绝对路径"))
	}
	if !local && len(bindings) > 0 {
		return nil, asUsageError(cliErrorf("--binding 仅适用于本地迁移任务"))
	}
	if expectedRevision < 0 {
		expectedRevision = getInt(view, "nodeRevision")
	}
	body := map[string]interface{}{"add": add, "remove": remove, "mode": mode, "expectedNodeRevision": expectedRevision}
	if local {
		body["bindings"] = bindings
	}
	plan := map[string]interface{}{"taskId": id, "add": add, "remove": remove, "mode": mode, "expectedNodeRevision": expectedRevision}
	if local {
		plan["bindings"] = bindings
	}
	preview := map[string]interface{}{"taskNodesPlan": plan, "view": view}
	// A retry with --request-id may follow an accepted change: the Master
	// replays the stored result, while a fresh preflight would see the new
	// membership and fail. Only new requests with joining nodes preflight.
	if local && joins > 0 && !opts.Has("request-id") {
		// The dry run persists nothing, so it carries no request identity: the
		// accepted request below must not collide with a stored dry-run body.
		dry := map[string]interface{}{"dryRun": true}
		for key, value := range body {
			dry[key] = value
		}
		checked, err := client.Post(path, dry)
		if checked != nil {
			preview["preflight"] = responseData(checked)
		}
		if err != nil {
			return preview, cliErrorf("节点共享目录预检失败，尚未提交变更: %w", err)
		}
		if responseData(checked)["valid"] != true {
			return preview, cliErrorf("节点共享目录预检未通过，尚未提交变更")
		}
	}
	if !opts.Has("yes") {
		return preview, cliErrorf("确认上述执行节点变更后添加 --yes 提交；本次未提交")
	}
	if requestID == "" {
		if requestID, err = newCreationRequestID(); err != nil {
			return preview, err
		}
	}
	body["requestId"] = requestID
	result, err := client.Post(path, body)
	if result == nil && err != nil {
		// No server response: the change may or may not have been accepted.
		return map[string]interface{}{"requestId": requestID, "taskNodesPlan": plan}, cliErrorf("%w；执行节点变更结果未确认（请求标识 %s）。先用 task nodes %d 核对；重试请保留原参数并添加 --request-id %s --expected-revision %d", err, requestID, id, requestID, expectedRevision)
	}
	if result != nil {
		result["requestId"] = requestID
		result["taskNodesChange"] = map[string]interface{}{"taskId": id, "add": add, "remove": remove, "mode": mode}
	}
	return result, err
}

// Accept repeated flags and comma-separated lists, like --node in task create.
func taskNodeSelectors(values []string, flag string) ([]string, error) {
	nodes := []string{}
	seen := map[string]bool{}
	for _, raw := range values {
		for _, item := range strings.Split(raw, ",") {
			node := strings.TrimSpace(item)
			if adminNodeAddress(node) != nil {
				return nil, cliErrorf("--%s 需为节点名称、节点标识或 IP: %q", flag, item)
			}
			if !seen[node] {
				seen[node] = true
				nodes = append(nodes, node)
			}
		}
	}
	return nodes, nil
}

// Paths may contain commas, so each --binding names exactly one node.
func taskNodeBindings(values, add []string) ([]interface{}, error) {
	adding := map[string]bool{}
	for _, node := range add {
		adding[node] = true
	}
	bindings := []interface{}{}
	seen := map[string]bool{}
	for _, value := range values {
		node, path, ok := strings.Cut(value, "=")
		node = strings.TrimSpace(node)
		if !ok || adminNodeAddress(node) != nil || !agentAbsolutePath(path) {
			return nil, cliErrorf("节点路径绑定格式错误: %q，应为节点=绝对路径", value)
		}
		if !adding[node] {
			return nil, cliErrorf("--binding 只能用于本次 --add 的节点: %s", node)
		}
		if seen[node] {
			return nil, cliErrorf("同一执行节点只能绑定一次: %s", node)
		}
		seen[node] = true
		bindings = append(bindings, map[string]interface{}{"executionNodeIp": node, "localPath": path})
	}
	return bindings, nil
}

func renderTaskNodesResult(out io.Writer, result map[string]interface{}) bool {
	data := responseData(result)
	if plan, ok := result["taskNodesPlan"].(map[string]interface{}); ok {
		renderTaskNodesPlan(out, plan)
		if preflight, ok := result["preflight"].(map[string]interface{}); ok {
			renderTaskNodeBindingChecks(out, objectList(preflight["nodeBindings"]))
			for _, message := range cliStringList(preflight["validationErrors"]) {
				fmt.Fprintln(out, safeCLIText(message))
			}
		}
		return true
	}
	if view, ok := data["view"].(map[string]interface{}); ok {
		if _, accepted := data["accepted"]; accepted {
			if data["replayed"] == true {
				fmt.Fprintln(out, cliText(out, "该请求此前已受理，以下为当时的结果。"))
			} else if data["accepted"] == true {
				fmt.Fprintf(out, cliText(out, "已受理执行节点变更（请求标识 %s）。\n"), safeCLIText(getString(result, "requestId")))
			}
			renderTaskNodeBindingChecks(out, objectList(data["nodeBindings"]))
			if data["accepted"] == true {
				// A successful change: summarize what was requested and the
				// resulting members, without the edit-restriction notes that
				// read like errors while the change itself is still running.
				renderTaskNodesAccepted(out, result, view)
			} else {
				renderTaskNodesView(out, view)
			}
			return true
		}
	}
	if _, ok := data["candidates"]; ok {
		if _, ok := data["nodeRevision"]; ok {
			renderTaskNodesView(out, data)
			return true
		}
	}
	if result["success"] == false && getString(result, "code") != "" {
		if bindings := objectList(result["nodeBindings"]); len(bindings) > 0 {
			renderTaskNodeBindingChecks(out, bindings)
			return true
		}
	}
	return false
}

func cliStringList(value interface{}) []string {
	items := []string{}
	if list, ok := value.([]string); ok {
		return list
	}
	if list, ok := value.([]interface{}); ok {
		for _, item := range list {
			if text, ok := item.(string); ok && text != "" {
				items = append(items, text)
			}
		}
	}
	return items
}

func renderTaskNodesPlan(out io.Writer, plan map[string]interface{}) {
	fmt.Fprintf(out, cliText(out, "任务 #%d 执行节点变更（基于成员版本 %d）\n"), getInt(plan, "taskId"), getInt(plan, "expectedNodeRevision"))
	w := newCLITable(out)
	for _, node := range cliStringList(plan["add"]) {
		fmt.Fprintf(w, "%s\t%s\n", cliText(out, "加入"), safeCLIText(node))
	}
	label := cliText(out, "移出（等待当前工作完成）")
	if getString(plan, "mode") == "now" {
		label = cliText(out, "立即移出（撤回运行中的工作）")
	}
	for _, node := range cliStringList(plan["remove"]) {
		fmt.Fprintf(w, "%s\t%s\n", label, safeCLIText(node))
	}
	for _, binding := range objectList(plan["bindings"]) {
		fmt.Fprintf(w, "%s\t%s = %s\n", cliText(out, "目录绑定"), safeCLIText(getString(binding, "executionNodeIp")), safeCLIText(getString(binding, "localPath")))
	}
	w.Flush()
}

func renderTaskNodeBindingChecks(out io.Writer, checks []map[string]interface{}) {
	if len(checks) == 0 {
		return
	}
	fmt.Fprintln(out, cliText(out, "节点共享目录预检"))
	w := newCLITable(out)
	fmt.Fprintln(w, cliText(out, "节点\t路径\t结果\t路径类型\t说明"))
	for _, check := range checks {
		state := cliText(out, "未通过")
		if check["valid"] == true {
			state = cliText(out, "通过")
		}
		node := cliNodeLabel(check, getString(check, "executionNodeIp"), localeOf(out))
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", safeCLIText(node), safeCLIText(getString(check, "localPath")), state, safeCLIText(dashIfEmpty(getString(check, "pathType"))), safeCLIText(dashIfEmpty(getString(check, "message"))))
	}
	w.Flush()
}

func dashIfEmpty(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

func renderTaskNodesView(out io.Writer, view map[string]interface{}) {
	fmt.Fprintf(out, cliText(out, "任务 #%d 执行节点（成员版本 %d，%s）\n"), getInt(view, "taskId"), getInt(view, "nodeRevision"), safeCLIText(cliTaskStatus(getString(view, "status"))))
	renderTaskNodeRows(out, objectList(view["nodes"]), view["localTransfer"] == true)
	renderTaskNodeNotes(out, view)
	if view["editable"] == false {
		fmt.Fprintf(out, cliText(out, "当前不能调整执行节点: %s\n"), safeCLIText(getString(view, "reason")))
	}
	if view["nowSupported"] == false && getString(view, "nowReason") != "" {
		fmt.Fprintf(out, cliText(out, "不支持立即移出: %s\n"), safeCLIText(getString(view, "nowReason")))
	}
	if removed := objectList(view["removedNodes"]); len(removed) > 0 {
		fmt.Fprintln(out, cliText(out, "已移出的节点"))
		w := newCLITable(out)
		for _, node := range removed {
			fmt.Fprintf(w, "%s\t%s\n", safeCLIText(cliNodeLabel(node, firstNodeString(node, "address", "id"), localeOf(out))), safeCLIText(dashIfEmpty(cliTaskTimestamp(node["removedAt"]))))
		}
		w.Flush()
	}
	candidates := objectList(view["candidates"])
	if len(candidates) == 0 {
		fmt.Fprintln(out, cliText(out, "没有可加入的其他在线 Agent"))
		return
	}
	fmt.Fprintln(out, cliText(out, "可加入的 Agent"))
	w := newCLITable(out)
	fmt.Fprintln(w, cliText(out, "节点\t节点标识\t可加入\t说明"))
	for _, node := range candidates {
		eligible := cliText(out, "否")
		if node["eligible"] == true {
			eligible = cliText(out, "是")
		}
		identity := firstNodeString(node, "address", "id")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", safeCLIText(cliNodeLabel(node, identity, localeOf(out))), safeCLIText(identity), eligible, safeCLIText(dashIfEmpty(getString(node, "reason"))))
	}
	w.Flush()
}

func renderTaskNodesAccepted(out io.Writer, result, view map[string]interface{}) {
	change, _ := result["taskNodesChange"].(map[string]interface{})
	if added := cliStringList(change["add"]); len(added) > 0 {
		fmt.Fprintf(out, cliText(out, "加入: %s\n"), safeCLIText(strings.Join(added, ", ")))
	}
	if removed := cliStringList(change["remove"]); len(removed) > 0 {
		mode := cliText(out, "平滑移出：正在传输的子任务完成后离开")
		if getString(change, "mode") == "now" {
			mode = cliText(out, "立即移出：正在传输的子任务转给其他节点继续")
		}
		fmt.Fprintf(out, cliText(out, "移出: %s（%s）\n"), safeCLIText(strings.Join(removed, ", ")), mode)
	}
	fmt.Fprintf(out, cliText(out, "任务 #%d 执行节点（成员版本 %d，%s）\n"), getInt(view, "taskId"), getInt(view, "nodeRevision"), safeCLIText(cliTaskStatus(getString(view, "status"))))
	renderTaskNodeRows(out, objectList(view["nodes"]), view["localTransfer"] == true)
	if view["pending"] == true {
		fmt.Fprintf(out, cliText(out, "变更会在后台完成；查看进度: oss-transfer-cli task nodes %d --watch 5\n"), getInt(view, "taskId"))
	}
}

func renderTaskNodeNotes(out io.Writer, membership map[string]interface{}) {
	if membership["pending"] == true {
		fmt.Fprintln(out, cliText(out, "有节点正在加入或移出；完成前只能取消加入、立即移出正在移出的节点或撤回移出。"))
	}
	// The node view reports the scan as {"scan": {"phase"}}; task rows as scanPhase.
	phase := getString(membership, "scanPhase")
	if scan, ok := membership["scan"].(map[string]interface{}); ok {
		phase = getString(scan, "phase")
	}
	switch phase {
	case "COMPLETE":
		fmt.Fprintln(out, cliText(out, "扫描已完成，不再需要扫描节点。"))
	case "HANDOFF":
		fmt.Fprintln(out, cliText(out, "扫描正在移交给新的扫描节点，从 Master 保存的断点继续。"))
	case "WAITING":
		fmt.Fprintln(out, cliText(out, "原扫描节点已退出，其他节点将从 Master 保存的断点接管扫描。"))
	}
}

func renderTaskNodeRows(out io.Writer, nodes []map[string]interface{}, local bool) {
	if len(nodes) == 0 {
		fmt.Fprintln(out, cliText(out, "暂无记录"))
		return
	}
	w := newCLITable(out)
	header := cliText(out, "节点\t节点标识\t状态\t扫描\t待完成子任务\t带宽份额\tLIST/HEAD/PUT QPS 份额")
	if local {
		header += "\t" + cliText(out, "本地路径")
	}
	fmt.Fprintln(w, header)
	for _, node := range nodes {
		identity := firstNodeString(node, "address", "id")
		scanner := ""
		if node["scanner"] == true {
			scanner = cliText(out, "是")
		}
		pending := "—"
		if value, ok := cliNumeric(node["pendingChildren"]); ok {
			pending = strconv.FormatFloat(value, 'f', -1, 64)
		}
		bandwidth, qps := "—", "—"
		if share, ok := node["share"].(map[string]interface{}); ok {
			bandwidth = cliText(out, "不限")
			if bps, ok := cliNumeric(share["bandwidthBytesPerSecond"]); ok && bps > 0 {
				bandwidth = strconv.FormatFloat(math.Round(bps*8/1e4)/100, 'f', -1, 64) + " Mbps"
			}
			parts := []string{}
			for _, key := range []string{"listQps", "headQps", "putQps"} {
				part := cliText(out, "不限")
				if value, ok := cliNumeric(share[key]); ok && value > 0 {
					part = strconv.FormatFloat(value, 'f', -1, 64)
				}
				parts = append(parts, part)
			}
			qps = strings.Join(parts, " / ")
		}
		row := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s", safeCLIText(cliNodeLabel(node, identity, localeOf(out))), safeCLIText(identity), taskNodeState(out, node), scanner, pending, bandwidth, qps)
		if local {
			row += "\t" + safeCLIText(dashIfEmpty(getString(node, "localPath")))
		}
		fmt.Fprintln(w, row)
	}
	w.Flush()
}

func taskNodeState(out io.Writer, node map[string]interface{}) string {
	state := getString(node, "state")
	label := map[string]string{
		"ACTIVE":       cliText(out, "执行中"),
		"JOINING":      cliText(out, "加入中"),
		"DRAINING":     cliText(out, "移出中（等待当前工作完成）"),
		"REMOVING":     cliText(out, "立即移出中"),
		"DISCONNECTED": cliText(out, "已断开，工作已由其他节点接管"),
	}[state]
	if label == "" {
		label = safeCLIText(dashIfEmpty(state))
	}
	if node["online"] == false && state != "DISCONNECTED" {
		label += cliText(out, "，离线")
	}
	if getString(node, "progress") == "LEASE_EXPIRY" {
		label += cliText(out, "，执行租约到期后离开")
	}
	return label
}

// task show keeps its previous output when an older Master omits membership.
func renderTaskMembership(out io.Writer, task map[string]interface{}) {
	membership, ok := task["executionMembership"].(map[string]interface{})
	if !ok {
		return
	}
	fmt.Fprintf(out, cliText(out, "执行节点成员（成员版本 %d）\n"), getInt(membership, "nodeRevision"))
	renderTaskNodeRows(out, objectList(membership["nodes"]), false)
	renderTaskNodeNotes(out, membership)
}
