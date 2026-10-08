package main

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func runTaskCommand(client *ApiClient, args []string) (map[string]interface{}, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("请指定 task 子命令，使用 task --help 查看")
	}
	action, args := args[0], args[1:]
	switch action {
	case "create":
		return runTaskCreate(client, args)
	case "import":
		return runTaskImport(client, args)
	case "import-legacy":
		return runLegacyTaskImport(client, args)
	case "list":
		return taskList(client, args)
	case "show", "children":
		return taskShow(client, args, action == "children")
	case "stop", "resume", "start", "retry", "delete":
		return taskControl(client, action, args)
	case "trash":
		opts, err := parseCommandOptions(args, nil, nil)
		if err != nil {
			return nil, err
		}
		if len(opts.Positionals) > 0 {
			return nil, fmt.Errorf("task trash 不接受额外参数")
		}
		return client.Get("/deleted-tasks")
	case "purge":
		opts, err := parseCommandOptions(args, nil, []string{"yes"})
		if err != nil {
			return nil, err
		}
		ids, err := taskIDs(opts.Positionals)
		if err != nil {
			return nil, err
		}
		if !opts.Has("yes") {
			return nil, fmt.Errorf("永久删除指定任务记录不可恢复；确认后添加 --yes")
		}
		return client.Post("/permanent-delete", map[string]interface{}{"taskIds": ids})
	case "bandwidth":
		return taskBandwidth(client, args)
	case "report":
		return taskReport(client, args)
	case "creation":
		opts, err := parseCommandOptions(args, nil, nil)
		if err != nil {
			return nil, err
		}
		if len(opts.Positionals) != 1 || strings.TrimSpace(opts.Positionals[0]) == "" {
			return nil, fmt.Errorf("用法: task creation <creationId>")
		}
		return client.Get("/task-creations/" + url.PathEscape(opts.Positionals[0]))
	default:
		return nil, fmt.Errorf("未知 task 子命令 %q", action)
	}
}
func taskList(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, []string{"status", "source", "dest", "worker", "direction", "search"}, []string{"all", "a"})
	if err != nil {
		return nil, err
	}
	if len(opts.Positionals) > 0 {
		return nil, fmt.Errorf("task list 不接受位置参数")
	}
	query := url.Values{}
	for flag, param := range map[string]string{"source": "sourceBucket", "dest": "destBucket", "worker": "workerNode"} {
		if opts.Has(flag) {
			query.Set(param, opts.Value(flag))
		}
	}
	if opts.Has("status") {
		status := strings.ToUpper(opts.Value("status"))
		switch status {
		case "PENDING", "RUNNING", "LIST_DONE", "STOPPING", "STOPPED", "COMPLETED", "PARTIAL_FAILED", "FAILED":
		default:
			return nil, fmt.Errorf("无效任务状态 %q", status)
		}
		query.Set("status", status)
	}
	direction := strings.ToUpper(strings.ReplaceAll(opts.Value("direction"), "-", "_"))
	if direction != "" && direction != "OSS_TO_OSS" && direction != "OSS_TO_LOCAL" && direction != "LOCAL_TO_OSS" {
		return nil, fmt.Errorf("无效迁移方向 %q", opts.Value("direction"))
	}
	result, err := client.Get("/status?" + query.Encode())
	if err != nil {
		return result, err
	}
	filtered := []map[string]interface{}{}
	for _, task := range objectList(responseData(result)["tasks"]) {
		if direction != "" && taskDirection(task) != direction {
			continue
		}
		if q := strings.ToLower(opts.Value("search")); q != "" && !strings.Contains(strings.ToLower(fmt.Sprint(task["taskIndex"], " ", taskEndpoint(task, true), " ", taskEndpoint(task, false), " ", taskWorkers(task))), q) {
			continue
		}
		filtered = append(filtered, task)
	}
	sort.SliceStable(filtered, func(i, j int) bool { return getInt(filtered[i], "taskIndex") > getInt(filtered[j], "taskIndex") })
	responseData(result)["tasks"] = filtered
	responseData(result)["filteredCount"] = len(filtered)
	return result, nil
}
func taskDirection(task map[string]interface{}) string {
	if direction := getString(task, "direction"); direction != "" {
		return direction
	}
	return "OSS_TO_OSS"
}
func localTask(task map[string]interface{}) bool { return taskDirection(task) != "OSS_TO_OSS" }

// Child rows carry the actual execution node as well as their parent's member
// list. Select the actual node before consulting the aggregate membership.
func taskWorkerNode(task map[string]interface{}) string {
	for _, key := range []string{"executionNodeIp", "workerNode"} {
		if value := getString(task, key); value != "" {
			return value
		}
	}
	return ""
}
func taskIsChild(task map[string]interface{}) bool {
	return getString(task, "shardId") != "" || (getInt(task, "parentTaskId") > 0 && getInt(task, "parentTaskId") != getInt(task, "taskIndex"))
}
func taskBindings(task, endpoint map[string]interface{}) []map[string]interface{} {
	if bindings := objectList(endpoint["nodeBindings"]); len(bindings) > 0 {
		return bindings
	}
	return objectList(task["nodeBindings"])
}
func taskWorkers(task map[string]interface{}) string {
	if taskIsChild(task) {
		if node := taskWorkerNode(task); node != "" {
			return node
		}
	}
	for _, key := range []string{"participatingWorkerNodes", "eligibleWorkerNodes"} {
		nodes := []string{}
		seen := map[string]bool{}
		add := func(node string) {
			if node != "" && !seen[node] {
				seen[node] = true
				nodes = append(nodes, node)
			}
		}
		switch values := task[key].(type) {
		case []interface{}:
			for _, value := range values {
				if node, ok := value.(string); ok {
					add(node)
				}
			}
		case []string:
			for _, node := range values {
				add(node)
			}
		}
		if len(nodes) > 0 {
			return strings.Join(nodes, ",")
		}
	}
	nodes := []string{}
	seen := map[string]bool{}
	for _, key := range []string{"sourceEndpoint", "destinationEndpoint"} {
		endpoint, _ := task[key].(map[string]interface{})
		for _, binding := range taskBindings(task, endpoint) {
			if node := getString(binding, "executionNodeIp"); node != "" && !seen[node] {
				seen[node] = true
				nodes = append(nodes, node)
			}
		}
	}
	if len(nodes) > 0 {
		return strings.Join(nodes, ",")
	}
	if node := taskWorkerNode(task); node != "" {
		return node
	}
	return "—"
}
func taskEndpoint(task map[string]interface{}, source bool) string {
	endpointKey, bucketKey, aliasKey := "destinationEndpoint", "destBucket", "destS3Alias"
	if source {
		endpointKey, bucketKey, aliasKey = "sourceEndpoint", "sourceBucket", "sourceS3Alias"
	}
	endpoint, _ := task[endpointKey].(map[string]interface{})
	isLocal := endpoint["type"] == "LOCAL" || (source && taskDirection(task) == "LOCAL_TO_OSS") || (!source && taskDirection(task) == "OSS_TO_LOCAL")
	if isLocal {
		bindings := taskBindings(task, endpoint)
		if taskIsChild(task) {
			if path := getString(task, "localPath"); path != "" {
				return path
			}
			node := taskWorkerNode(task)
			for _, binding := range bindings {
				if node != "" && getString(binding, "executionNodeIp") == node {
					if path := getString(binding, "localPath"); path != "" {
						return path
					}
				}
			}
		}
		paths := []string{}
		seen := map[string]bool{}
		for _, binding := range bindings {
			path := getString(binding, "localPath")
			if path == "" {
				continue
			}
			if node := getString(binding, "executionNodeIp"); node != "" {
				path = node + ":" + path
			}
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
		if len(paths) > 0 {
			return strings.Join(paths, ", ")
		}
		for _, key := range []string{"localPath", "path"} {
			if path := getString(endpoint, key); path != "" {
				return path
			}
		}
		if path := getString(task, "localPath"); path != "" {
			return path
		}
		return "本地文件系统"
	}
	bucket := getString(task, bucketKey)
	alias := getString(task, aliasKey)
	if bucket == "" {
		bucket = getString(endpoint, "bucket")
	}
	if alias == "" {
		alias = getString(endpoint, "s3Alias")
	}
	if bucket != "" {
		if alias != "" && !strings.Contains(bucket, "/") {
			return alias + "/" + bucket
		}
		return bucket
	}
	return "—"
}
func taskSnapshot(client *ApiClient) (map[string]interface{}, map[int]map[string]interface{}, error) {
	result, err := client.Get("/status")
	if err != nil {
		return result, nil, err
	}
	tasks := map[int]map[string]interface{}{}
	for _, task := range objectList(responseData(result)["tasks"]) {
		tasks[getInt(task, "taskIndex")] = task
	}
	return result, tasks, nil
}
func taskShow(client *ApiClient, args []string, childrenOnly bool) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, []string{"child", "node"}, nil)
	if err != nil {
		return nil, err
	}
	if len(opts.Positionals) != 1 {
		return nil, fmt.Errorf("用法: task show <ID> [--child ID-序号] [--node IP]")
	}
	id, err := taskID(opts.Positionals[0])
	if err != nil {
		return nil, err
	}
	snapshot, tasks, err := taskSnapshot(client)
	if err != nil {
		return snapshot, err
	}
	task := tasks[id]
	if task == nil {
		return snapshot, fmt.Errorf("未找到主任务 #%d；节点未响应时列表可能不完整", id)
	}
	children := objectList(task["shards"])
	if localTask(task) {
		detail, err := client.Get(fmt.Sprintf("/local-transfers/status?taskId=%d", id))
		if err != nil {
			return detail, err
		}
		data := responseData(detail)
		for _, extended := range objectList(data["tasks"]) {
			if getInt(extended, "taskIndex") == id {
				merged := map[string]interface{}{}
				for k, v := range extended {
					merged[k] = v
				}
				for k, v := range task {
					merged[k] = v
				}
				task = merged
				break
			}
		}
		children = objectList(data["shards"])
		if incompleteResult(detail) {
			snapshot["partial"] = true
			snapshot["localDetails"] = detail
		}
	}
	children = numberCLIChildren(id, children, task["childTaskIds"])
	result := map[string]interface{}{"task": task, "children": children, "partial": incompleteResult(snapshot) || incompleteResult(task), "unreachableWorkers": snapshot["unreachableWorkers"]}
	for _, key := range []string{"snapshotComplete", "missingTaskIds"} {
		if value, ok := task[key]; ok {
			result[key] = value
		}
	}
	for _, key := range []string{"unavailableWorkers", "failedWorkers", "nodeErrors"} {
		if value, ok := responseData(snapshot)[key]; ok {
			result[key] = value
		}
	}
	if detail, ok := snapshot["localDetails"].(map[string]interface{}); ok {
		result["localDetails"] = map[string]interface{}{}
		for _, key := range []string{"unreachableWorkers", "unavailableWorkers", "failedWorkers", "errors"} {
			if value, ok := responseData(detail)[key]; ok {
				result["localDetails"].(map[string]interface{})[key] = value
			}
		}
	}
	if opts.Has("child") {
		matches := []map[string]interface{}{}
		for _, child := range children {
			if getString(child, "childTaskId") == opts.Value("child") && (!opts.Has("node") || taskWorkers(child) == opts.Value("node")) {
				matches = append(matches, child)
			}
		}
		if len(matches) != 1 {
			return result, fmt.Errorf("子任务 %q 匹配到 %d 条记录；检查编号，必要时指定 --node", opts.Value("child"), len(matches))
		}
		result["child"] = matches[0]
		delete(result, "children")
	} else if opts.Has("node") {
		return nil, fmt.Errorf("--node 仅用于选择 --child 对应节点")
	}
	if childrenOnly {
		delete(result, "task")
		result["parentTaskId"] = id
	}
	return result, nil
}

var legacyChildIndex = regexp.MustCompile(`(?:-oss-|-local-|batch-|^oss-.+-)([0-9]+)$`)
var naturalParts = regexp.MustCompile(`[0-9]+|[^0-9]+`)

func naturalLess(a, b string) bool {
	aa, bb := naturalParts.FindAllString(a, -1), naturalParts.FindAllString(b, -1)
	for i := 0; i < len(aa) && i < len(bb); i++ {
		if aa[i] == bb[i] {
			continue
		}
		an, ae := strconv.Atoi(aa[i])
		bn, be := strconv.Atoi(bb[i])
		if ae == nil && be == nil && an != bn {
			return an < bn
		}
		return aa[i] < bb[i]
	}
	return len(aa) < len(bb)
}
func numberCLIChildren(parent int, children []map[string]interface{}, childIDs interface{}) []map[string]interface{} {
	indices := map[int]int{}
	if ids, ok := childIDs.([]interface{}); ok {
		for i, id := range ids {
			indices[getInt(map[string]interface{}{"id": id}, "id")] = i
		}
	}
	indexOf := func(task map[string]interface{}) int {
		if v, ok := task["shardIndex"]; ok && v != nil {
			n := getInt(task, "shardIndex")
			if n >= 0 {
				return n
			}
		}
		if m := legacyChildIndex.FindStringSubmatch(getString(task, "shardId")); m != nil {
			i, _ := strconv.Atoi(m[1])
			return i
		}
		if i, ok := indices[getInt(task, "taskIndex")]; ok {
			return i
		}
		return math.MaxInt32
	}
	result := []map[string]interface{}{}
	for _, child := range children {
		if getInt(child, "taskIndex") == parent && getString(child, "shardId") == "discovery" {
			continue
		}
		copy := map[string]interface{}{}
		for k, v := range child {
			copy[k] = v
		}
		result = append(result, copy)
	}
	sort.SliceStable(result, func(i, j int) bool {
		a, b := indexOf(result[i]), indexOf(result[j])
		if a != b {
			return a < b
		}
		key := func(t map[string]interface{}) string {
			if v := getString(t, "shardId"); v != "" {
				return v
			}
			return fmt.Sprint(t["taskIndex"])
		}
		return naturalLess(key(result[i]), key(result[j]))
	})
	occupied := map[int]bool{}
	for _, task := range result {
		if i := indexOf(task); i != math.MaxInt32 {
			occupied[i+1] = true
		}
	}
	next := 1
	for _, task := range result {
		n := indexOf(task) + 1
		if n == math.MaxInt32+1 {
			for occupied[next] {
				next++
			}
			n = next
			occupied[n] = true
			next++
		}
		task["childTaskId"] = fmt.Sprintf("%d-%d", parent, n)
	}
	return result
}

var legacyStartFailure = regexp.MustCompile(`失败[:：]\s*[1-9][0-9]*`)

func taskControl(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, nil, nil)
	if err != nil {
		return nil, err
	}
	ids, err := taskIDs(opts.Positionals)
	if err != nil {
		return nil, err
	}
	snapshot, tasks, err := taskSnapshot(client)
	if err != nil {
		return snapshot, err
	}
	if incompleteResult(snapshot) {
		return snapshot, fmt.Errorf("节点数据不完整，无法确认任务状态；本次未执行%s", action)
	}
	groups := map[string][]int{}
	paths := []string{}
	for _, id := range ids {
		task := tasks[id]
		if task == nil {
			return nil, fmt.Errorf("未找到主任务 #%d；本次未执行操作", id)
		}
		state := getString(task, "status")
		exploring := task["ossTaskGroup"] == true && (task["creationStatus"] == "QUEUED" || task["creationStatus"] == "RUNNING")
		path := "/" + action
		allowed := false
		switch action {
		case "stop":
			allowed = exploring || state == "RUNNING" || state == "LIST_DONE" || state == "PENDING"
		case "resume", "retry":
			allowed = !exploring && (state == "STOPPED" || state == "FAILED" || state == "PARTIAL_FAILED")
			path = "/resume"
		case "start":
			allowed = !localTask(task) && task["autoStart"] != true && state == "PENDING" && !exploring
		case "delete":
			allowed = state != "RUNNING" && state != "LIST_DONE" && state != "STOPPING" && !exploring
			path = "/clear"
		}
		if !allowed {
			return nil, fmt.Errorf("任务 #%d 当前状态 %s 不允许 %s；本次未执行操作", id, state, action)
		}
		if localTask(task) && (path == "/stop" || path == "/resume") {
			path = "/local-transfers" + path
		}
		if _, exists := groups[path]; !exists {
			paths = append(paths, path)
		}
		groups[path] = append(groups[path], id)
	}
	details := []interface{}{}
	issues := []string{}
	for _, path := range paths {
		result, callErr := client.Post(path, map[string]interface{}{"taskIds": groups[path]})
		entry := map[string]interface{}{"taskIds": groups[path], "operation": action, "result": result}
		if callErr != nil {
			entry["error"] = callErr.Error()
			issues = append(issues, callErr.Error())
		} else if incompleteResult(result) || getInt(responseData(result), "failed") > 0 || (action == "start" && legacyStartFailure.MatchString(getString(result, "message"))) {
			issues = append(issues, "部分任务未受理，请检查节点结果")
		}
		details = append(details, entry)
	}
	result := map[string]interface{}{"success": len(issues) == 0, "operation": action, "taskIds": ids, "details": details, "message": "操作请求已处理；以任务最新状态为准"}
	if len(issues) > 0 {
		return result, fmt.Errorf("%s", strings.Join(issues, "；"))
	}
	return result, nil
}
func taskBandwidth(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(opts.Positionals) < 1 || len(opts.Positionals) > 2 {
		return nil, fmt.Errorf("用法: task bandwidth <ID> [Mbps|unlimited]")
	}
	id, err := taskID(opts.Positionals[0])
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/tasks/%d/bandwidth-limit", id)
	if len(opts.Positionals) == 1 {
		return client.Get(path)
	}
	var value interface{}
	if opts.Positionals[1] != "unlimited" {
		n, err := strconv.ParseFloat(opts.Positionals[1], 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0.1 || n > 1000000 {
			return nil, fmt.Errorf("任务带宽为 0.1–1000000 Mbps；取消额外限制使用 unlimited")
		}
		value = n
	}
	return client.Put(path, map[string]interface{}{"limitMbps": value})
}
func taskReport(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := parseCommandOptions(args, []string{"output"}, nil)
	if err != nil {
		return nil, err
	}
	if len(opts.Positionals) != 1 {
		return nil, fmt.Errorf("用法: task report <ID> [--output 文件.zip]")
	}
	id, err := taskID(opts.Positionals[0])
	if err != nil {
		return nil, err
	}
	output := opts.Value("output")
	if output == "" {
		output = fmt.Sprintf("task-%d-report.zip", id)
	}
	if err := client.Download("GET", fmt.Sprintf("/task-reports/%d", id), nil, output); err != nil {
		return nil, err
	}
	return map[string]interface{}{"success": true, "taskId": id, "output": output, "message": "任务报告已保存"}, nil
}
