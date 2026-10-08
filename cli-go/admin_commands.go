package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

// runAdminCommand uses the same master APIs as the console.
func runAdminCommand(client *ApiClient, group string, args []string) (map[string]interface{}, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("缺少 %s 子命令，请运行 oss-transfer-cli %s --help", group, group)
	}
	switch group {
	case "node":
		return runNodeAdmin(client, args[0], args[1:])
	case "agent":
		return runAgentAdmin(client, args[0], args[1:])
	case "storage":
		return runStorageAdmin(client, args[0], args[1:])
	case "limit":
		return runLimitAdmin(client, args[0], args[1:])
	case "exceptions":
		return runExceptionsAdmin(client, args[0], args[1:])
	case "object":
		return runObjectAdmin(client, args[0], args[1:])
	default:
		return nil, fmt.Errorf("未知命令组: %s", group)
	}
}

func adminOptions(args, values, switches []string, count int) (commandOptions, error) {
	o, err := parseCommandOptions(args, values, switches)
	if err == nil {
		for name, supplied := range o.Values {
			if len(supplied) > 1 {
				return o, fmt.Errorf("--%s 只能指定一次", name)
			}
		}
	}
	if err == nil && len(o.Positionals) != count {
		err = fmt.Errorf("需要 %d 个位置参数，实际收到 %d 个", count, len(o.Positionals))
	}
	return o, err
}

func adminConfirmed(o commandOptions) error {
	if !o.Has("yes") {
		return fmt.Errorf("此操作需要明确确认，请检查目标后添加 --yes")
	}
	return nil
}

func adminIPv4(value string) error {
	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil || ip.String() != value || ip.To4()[0] == 0 || ip.To4()[0] >= 224 {
		return fmt.Errorf("节点须为规范 IPv4 地址，服务端口使用集群配置；SSH 端口仅用于部署")
	}
	return nil
}

func adminData(value map[string]interface{}) map[string]interface{} {
	if data, ok := value["data"].(map[string]interface{}); ok {
		return data
	}
	return value
}

func runNodeAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	switch action {
	case "invite":
		return runAgentAdmin(client, "invite", args)
	case "list":
		if _, err := adminOptions(args, nil, nil, 0); err != nil {
			return nil, err
		}
		result, err := client.Get("/nodes")
		if err != nil {
			return result, err
		}
		status, statusErr := client.Get("/worker-status")
		if statusErr != nil {
			result["complete"] = false
			result["partial"] = true
			result["warning"] = "节点已列出，但运行状态未完整读取: " + statusErr.Error()
			return result, nil
		}
		result["workers"] = status["workers"]
		failed := []string{}
		workers, _ := status["workers"].(map[string]interface{})
		for _, address := range adminNodeAddresses(result) {
			worker, _ := workers[address].(map[string]interface{})
			if worker["success"] != true {
				failed = append(failed, address)
			}
		}
		result["complete"] = len(failed) == 0
		result["partial"] = len(failed) > 0
		result["failedWorkers"] = failed
		return result, nil
	case "add", "update", "remove":
		values, switches := []string{}, []string{}
		if action != "remove" {
			values = append(values, "label")
		}
		if action == "update" {
			values = append(values, "address")
		}
		if action == "remove" {
			switches = append(switches, "yes")
		}
		o, err := adminOptions(args, values, switches, 1)
		if err != nil {
			return nil, err
		}
		original := o.Positionals[0]
		if err := adminIPv4(original); err != nil {
			return nil, err
		}
		if action == "remove" {
			if err := adminConfirmed(o); err != nil {
				return nil, err
			}
			return client.Delete("/nodes/" + url.PathEscape(original))
		}
		address, label := original, o.Value("label")
		if action == "update" {
			if !o.Has("address") && !o.Has("label") {
				return nil, fmt.Errorf("请通过 --address 或 --label 指定要修改的字段")
			}
			if o.Has("address") {
				address = o.Value("address")
				if err := adminIPv4(address); err != nil {
					return nil, err
				}
			}
			if !o.Has("label") {
				registry, err := client.Get("/nodes")
				if err != nil {
					return registry, err
				}
				found := false
				for _, raw := range adminArray(adminData(registry)["nodes"]) {
					node, _ := raw.(map[string]interface{})
					if node["address"] == original {
						label, _ = node["label"].(string)
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("节点未注册: %s", original)
				}
			}
		}
		if len([]rune(label)) > 80 || strings.ContainsAny(label, "\r\n") {
			return nil, fmt.Errorf("节点名称不能超过 80 个字符或包含换行")
		}
		body := map[string]interface{}{"address": address, "label": strings.TrimSpace(label)}
		if action == "add" {
			return client.Post("/agents/invitations", body)
		}
		return client.Put("/nodes/"+url.PathEscape(original), body)
	default:
		return nil, fmt.Errorf("未知 node 子命令: %s", action)
	}
}

func runAgentAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	if action != "invite" {
		return nil, fmt.Errorf("未知 agent 子命令: %s", action)
	}
	o, err := adminOptions(args, []string{"label", "address"}, nil, 0)
	if err != nil {
		return nil, err
	}
	address, label := strings.TrimSpace(o.Value("address")), strings.TrimSpace(o.Value("label"))
	if o.Has("address") {
		if err := adminIPv4(address); err != nil {
			return nil, err
		}
	}
	if len([]rune(label)) > 80 || strings.ContainsAny(label, "\r\n") {
		return nil, fmt.Errorf("节点名称不能超过 80 个字符或包含换行")
	}
	return client.Post("/agents/invitations", map[string]interface{}{"address": address, "label": label})
}

func runStorageAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	switch action {
	case "list":
		if _, err := adminOptions(args, nil, nil, 0); err != nil {
			return nil, err
		}
		result, err := client.Get("/s3-config")
		if err == nil {
			for _, raw := range adminArray(result["configs"]) {
				if config, ok := raw.(map[string]interface{}); ok {
					for _, field := range []string{"accessKey", "secretKey"} {
						if value, ok := config[field].(string); ok && value != "" {
							config[field] = "********"
						}
					}
				}
			}
		}
		return result, err
	case "buckets", "test", "remove":
		switches := []string{}
		if action == "remove" {
			switches = append(switches, "yes")
		}
		o, err := adminOptions(args, nil, switches, 1)
		if err != nil {
			return nil, err
		}
		alias := o.Positionals[0]
		if err := adminAlias(alias); err != nil {
			return nil, err
		}
		if action == "remove" {
			if err := adminConfirmed(o); err != nil {
				return nil, err
			}
			return client.Delete("/s3-config/" + url.PathEscape(alias))
		}
		result, err := client.Get("/s3-config/" + url.PathEscape(alias) + "/buckets")
		if err == nil && action == "test" {
			result["message"] = "Bucket 列表读取成功；此检查验证连接及列举权限"
		}
		return result, err
	case "add", "update":
		o, err := adminOptions(args, []string{"endpoint", "access-key-env", "access-key-file", "secret-key-env", "secret-key-file", "validate", "v2-signer", "path-style"}, nil, 1)
		if err != nil {
			return nil, err
		}
		alias := o.Positionals[0]
		if err := adminAlias(alias); err != nil {
			return nil, err
		}
		body := map[string]interface{}{"alias": alias, "validate": true, "useV2SignerType": nil, "pathStyleEnable": nil}
		if action == "update" {
			if len(o.Values) == 0 {
				return nil, fmt.Errorf("请指定至少一个要修改的字段")
			}
			current, err := client.Get("/s3-config")
			if err != nil {
				return current, err
			}
			found := false
			for _, raw := range adminArray(current["configs"]) {
				config, _ := raw.(map[string]interface{})
				if config["alias"] == alias {
					for _, key := range []string{"endpoint", "accessKey", "secretKey", "useV2SignerType", "pathStyleEnable"} {
						body[key] = config[key]
					}
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("存储连接不存在: %s", alias)
			}
		}
		if o.Has("endpoint") {
			body["endpoint"] = strings.TrimSpace(o.Value("endpoint"))
		}
		for flag, field := range map[string]string{"access-key": "accessKey", "secret-key": "secretKey"} {
			value, supplied, err := adminCredential(o, flag)
			if err != nil {
				return nil, err
			}
			if supplied {
				body[field] = value
			}
		}
		for flag, field := range map[string]string{"validate": "validate", "v2-signer": "useV2SignerType", "path-style": "pathStyleEnable"} {
			if o.Has(flag) {
				switch o.Value(flag) {
				case "true":
					body[field] = true
				case "false":
					body[field] = false
				case "inherit":
					if flag == "validate" {
						return nil, fmt.Errorf("--validate 只支持 true 或 false")
					}
					body[field] = nil
				default:
					return nil, fmt.Errorf("--%s 需使用 true、false%s", flag, map[bool]string{true: "", false: " 或 inherit"}[flag == "validate"])
				}
			}
		}
		for _, field := range []string{"endpoint", "accessKey", "secretKey"} {
			value, _ := body[field].(string)
			if strings.TrimSpace(value) == "" || (field != "endpoint" && value == "********") {
				return nil, fmt.Errorf("缺少 %s，请提供 Endpoint 或对应密钥的环境变量/文件", field)
			}
		}
		endpoint, err := url.Parse(body["endpoint"].(string))
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil {
			return nil, fmt.Errorf("Endpoint 必须是 http:// 或 https:// 地址，且不得包含用户名或密码")
		}
		if action == "add" {
			return client.Post("/s3-config", body)
		}
		return client.Put("/s3-config/"+url.PathEscape(alias), body)
	default:
		return nil, fmt.Errorf("未知 storage 子命令: %s", action)
	}
}

func adminAlias(alias string) error {
	if strings.TrimSpace(alias) == "" || strings.TrimSpace(alias) != alias || strings.ContainsAny(alias, "/\\\r\n") || alias == "." || alias == ".." {
		return fmt.Errorf("连接名称不能为空、含首尾空白或包含路径分隔符")
	}
	return nil
}

func adminCredential(o commandOptions, flag string) (string, bool, error) {
	env, file := o.Value(flag+"-env"), o.Value(flag+"-file")
	if o.Has(flag+"-env") && o.Has(flag+"-file") {
		return "", false, fmt.Errorf("--%s-env 和 --%s-file 不能同时使用", flag, flag)
	}
	if !o.Has(flag+"-env") && !o.Has(flag+"-file") {
		return "", false, nil
	}
	var value string
	if env != "" {
		var found bool
		value, found = os.LookupEnv(env)
		if !found {
			return "", false, fmt.Errorf("密钥环境变量未设置: %s", env)
		}
	} else {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", false, fmt.Errorf("无法读取 --%s-file 指定的密钥文件", flag)
		}
		value = strings.TrimRight(string(data), "\r\n")
	}
	if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
		return "", false, fmt.Errorf("--%s 的密钥不能为空或包含换行", flag)
	}
	return value, true, nil
}

func runLimitAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	switch action {
	case "list":
		if _, err := adminOptions(args, nil, nil, 0); err != nil {
			return nil, err
		}
		return client.Get("/rate-limit")
	case "history":
		o, err := adminOptions(args, []string{"limit"}, nil, 0)
		if err != nil {
			return nil, err
		}
		limit, err := adminInteger(o, "limit", 50, 1, 1000)
		if err != nil {
			return nil, err
		}
		return client.Get("/rate-limit/history?limit=" + strconv.Itoa(limit))
	case "adjustment":
		o, err := adminOptions(args, []string{"worker", "event-id", "cursor"}, nil, 0)
		if err != nil {
			return nil, err
		}
		if o.Value("worker") == "" || o.Value("event-id") == "" {
			return nil, fmt.Errorf("调节详情需要 --worker 和 --event-id")
		}
		if err := adminIPv4(o.Value("worker")); err != nil {
			return nil, err
		}
		query := url.Values{"workerIp": {o.Value("worker")}, "eventId": {o.Value("event-id")}}
		if o.Has("cursor") {
			query.Set("cursor", o.Value("cursor"))
		}
		result, err := client.Get("/rate-limit/adjustment?" + query.Encode())
		if err == nil && result["historyComplete"] == false {
			result["complete"] = false
			result["warning"] = "本轮部分调节记录未保存，详情不完整"
		}
		return result, err
	case "set":
		o, err := adminOptions(args, []string{"worker", "unit"}, []string{"all"}, 2)
		if err != nil {
			return nil, err
		}
		kind := o.Positionals[0]
		if kind != "list" && kind != "put" && kind != "head" && kind != "bandwidth" {
			return nil, fmt.Errorf("限速类型必须是 list、put、head 或 bandwidth")
		}
		if o.Has("worker") == o.Has("all") {
			return nil, fmt.Errorf("请指定 --worker <IPv4> 或 --all，且不能同时使用")
		}
		if o.Has("worker") {
			if err := adminIPv4(o.Value("worker")); err != nil {
				return nil, err
			}
		}
		value, err := adminRate(kind, o.Positionals[1], o.Value("unit"))
		if err != nil {
			return nil, err
		}
		path := "/rate-limit/" + kind
		if o.Has("worker") {
			path += "/" + url.PathEscape(o.Value("worker"))
		}
		return client.Post(path, map[string]interface{}{"qps": value})
	default:
		return nil, fmt.Errorf("未知 limit 子命令: %s", action)
	}
}

func adminRate(kind, raw, unit string) (float64, error) {
	if kind != "bandwidth" && unit != "" {
		return 0, fmt.Errorf("--unit 只用于 bandwidth，其他类型单位为次/秒")
	}
	multiplier := float64(1)
	if kind == "bandwidth" {
		if unit == "" {
			unit = "Mbps"
		}
		units := map[string]float64{"bps": 1, "Kbps": 1e3, "Mbps": 1e6, "Gbps": 1e9}
		var ok bool
		multiplier, ok = units[unit]
		if !ok {
			return 0, fmt.Errorf("带宽单位必须为 bps、Kbps、Mbps 或 Gbps；默认 Mbps")
		}
	}
	if raw == "unlimited" {
		return 0, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	value *= multiplier
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0, fmt.Errorf("限速须为大于 0 的有限数值；取消限速请明确使用 unlimited")
	}
	return value, nil
}

func adminInteger(o commandOptions, name string, fallback, min, max int) (int, error) {
	if !o.Has(name) {
		return fallback, nil
	}
	value, err := strconv.Atoi(o.Value(name))
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("--%s 必须是 %d 到 %d 之间的整数", name, min, max)
	}
	return value, nil
}

func runObjectAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	if action != "compare" {
		return nil, fmt.Errorf("未知 object 子命令: %s", action)
	}
	fields := map[string]string{"source-bucket": "sourceBucket", "source-key": "sourceKey", "dest-bucket": "destBucket", "dest-key": "destKey"}
	o, err := adminOptions(args, []string{"source-bucket", "source-key", "dest-bucket", "dest-key"}, nil, 0)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	for flag, field := range fields {
		value := o.Value(flag)
		if value == "" {
			return nil, fmt.Errorf("对象比对需要 --%s", flag)
		}
		if strings.HasSuffix(flag, "bucket") {
			parts := strings.Split(value, "/")
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
				return nil, fmt.Errorf("--%s 必须使用 连接名称/Bucket 格式", flag)
			}
		}
		query.Set(field, value)
	}
	return client.Get("/object-metadata?" + query.Encode())
}

func runExceptionsAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	if action == "clear" {
		o, err := adminOptions(args, nil, []string{"all", "yes"}, 1)
		if err != nil {
			return nil, err
		}
		kind := o.Positionals[0]
		if kind != "failed" && kind != "fallback" {
			return nil, fmt.Errorf("仅 failed、fallback 支持清空；本地迁移失败需使用任务重试或报告")
		}
		if !o.Has("all") {
			return nil, fmt.Errorf("此 API 只能清空所有节点的该类记录，请明确指定 --all --yes；不支持按筛选条件清空")
		}
		if err := adminConfirmed(o); err != nil {
			return nil, err
		}
		result, err := client.Post("/"+kind+"/clear", nil)
		if err == nil && kind == "fallback" {
			result["complete"] = false
			result["warning"] = "降级记录清空请求已提交；此 API 不逐节点确认结果，请重新查询检查记录及节点完整性"
			result["message"] = result["warning"]
		}
		return result, err
	}
	if action != "list" && action != "export" {
		return nil, fmt.Errorf("未知 exceptions 子命令: %s", action)
	}
	flags := []string{"task-id", "source-bucket", "dest-bucket", "worker", "limit", "offset", "direction", "endpoint", "storage-id"}
	if action == "export" {
		flags = append(flags, "output")
	}
	o, err := adminOptions(args, flags, nil, 1)
	if err != nil {
		return nil, err
	}
	kind := o.Positionals[0]
	if kind != "failed" && kind != "fallback" && kind != "local" {
		return nil, fmt.Errorf("异常类型必须是 failed、fallback 或 local")
	}
	if action == "export" && o.Value("output") == "" {
		return nil, fmt.Errorf("导出需要 --output <文件路径>")
	}
	if kind == "local" && action == "export" {
		for key := range o.Values {
			if key != "output" {
				return nil, fmt.Errorf("本地迁移报告 API 导出所有已结束或停止任务的 ZIP，不支持 --%s 筛选；单任务报告请使用 task report", key)
			}
		}
		if err := client.Download("GET", "/api/master/local-transfers/report", nil, o.Value("output")); err != nil {
			return nil, err
		}
		return map[string]interface{}{"success": true, "output": o.Value("output"), "message": "本地迁移报告已保存", "scope": "all-local-transfers"}, nil
	}
	var result map[string]interface{}
	if kind == "local" {
		result, err = adminLocalExceptions(client, o)
	} else {
		result, err = adminOSSExceptions(client, kind, o)
	}
	if err != nil || action != "export" {
		return result, err
	}
	if err := adminExportExceptions(o.Value("output"), adminArray(result["items"])); err != nil {
		return result, err
	}
	result["output"] = o.Value("output")
	result["exported"] = len(adminArray(result["items"]))
	result["message"] = "已导出本次查询返回的异常记录；完整性及截断状态见 complete、partial 和 truncated"
	delete(result, "items")
	return result, nil
}

func adminLocalExceptions(client *ApiClient, o commandOptions) (map[string]interface{}, error) {
	for _, flag := range []string{"source-bucket", "dest-bucket"} {
		if o.Has(flag) {
			return nil, fmt.Errorf("local 不支持 --%s，请使用 --endpoint 筛选", flag)
		}
	}
	limit, err := adminInteger(o, "limit", 25, 1, 1000)
	if err != nil {
		return nil, err
	}
	offset, err := adminInteger(o, "offset", 0, 0, math.MaxInt32)
	if err != nil {
		return nil, err
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}, "offset": {strconv.Itoa(offset)}}
	for flag, field := range map[string]string{"task-id": "taskId", "worker": "executionNodeIp", "direction": "direction", "endpoint": "endpoint", "storage-id": "storageId"} {
		if o.Has(flag) {
			query.Set(field, o.Value(flag))
		}
	}
	if o.Has("task-id") {
		if _, err := adminInteger(o, "task-id", 0, 0, math.MaxInt32); err != nil {
			return nil, err
		}
	}
	if o.Has("worker") {
		if err := adminIPv4(o.Value("worker")); err != nil {
			return nil, err
		}
	}
	if direction := o.Value("direction"); direction != "" && direction != "OSS_TO_LOCAL" && direction != "LOCAL_TO_OSS" {
		return nil, fmt.Errorf("local 的 --direction 必须为 OSS_TO_LOCAL 或 LOCAL_TO_OSS")
	}
	response, err := client.Get("/local-transfers/failures?" + query.Encode())
	if err != nil {
		return response, err
	}
	result := adminData(response)
	result["success"] = true
	complete := result["partial"] != true && len(adminArray(result["unreachableWorkers"])) == 0
	// The master fetches at most 1000 records from each worker. Expose this
	// server-side truncation even when its aggregate total exceeds the page.
	workers, _ := result["workers"].(map[string]interface{})
	truncatedWorkers := []string{}
	for worker, raw := range workers {
		data, ok := raw.(map[string]interface{})
		if !ok {
			complete = false
			continue
		}
		if adminNumber(data["total"]) > float64(len(adminArray(data["items"]))) {
			truncatedWorkers = append(truncatedWorkers, worker)
		}
	}
	sort.Strings(truncatedWorkers)
	result["complete"] = complete
	if !complete {
		result["partial"] = true
	}
	result["truncatedWorkers"] = truncatedWorkers
	result["truncated"] = len(truncatedWorkers) > 0
	result["hasMore"] = float64(offset+len(adminArray(result["items"]))) < adminNumber(result["total"])
	if len(truncatedWorkers) > 0 {
		result["warning"] = "后端每节点最多读取 1000 条异常；更多记录请导出报告"
	}
	return result, nil
}

func adminOSSExceptions(client *ApiClient, kind string, o commandOptions) (map[string]interface{}, error) {
	for _, flag := range []string{"offset", "direction", "endpoint", "storage-id"} {
		if o.Has(flag) {
			return nil, fmt.Errorf("%s 不支持 --%s", kind, flag)
		}
	}
	limit, err := adminInteger(o, "limit", 1000, 1, math.MaxInt32)
	if err != nil {
		return nil, err
	}
	body := map[string]interface{}{"limit": limit}
	if o.Has("task-id") {
		id, err := adminInteger(o, "task-id", 0, 0, math.MaxInt32)
		if err != nil {
			return nil, err
		}
		body["taskIndex"] = id
	}
	if o.Has("source-bucket") {
		body["sourceBucket"] = o.Value("source-bucket")
	}
	if o.Has("dest-bucket") {
		body["destBucket"] = o.Value("dest-bucket")
	}
	if o.Has("worker") {
		if err := adminIPv4(o.Value("worker")); err != nil {
			return nil, err
		}
	}
	result, err := client.Post("/"+kind, body)
	if err != nil {
		return result, err
	}
	key := "failedKeys"
	if kind == "fallback" {
		key = "fallbackObjects"
	}
	grouped, ok := result[key].(map[string]interface{})
	if !ok {
		return result, fmt.Errorf("服务未返回有效的异常记录分组")
	}
	workers := []string{}
	for worker, value := range grouped {
		if _, ok := value.([]interface{}); ok && (!o.Has("worker") || o.Value("worker") == worker) {
			workers = append(workers, worker)
		}
	}
	sort.Strings(workers)
	items, total := []interface{}{}, float64(0)
	for _, worker := range workers {
		for _, raw := range adminArray(grouped[worker]) {
			if row, ok := raw.(map[string]interface{}); ok {
				row["workerNode"] = worker
				items = append(items, row)
			}
		}
		total += adminNumber(grouped[worker+"Count"])
	}
	result["items"], result["total"], result["respondingNodes"] = items, total, workers
	result["truncated"] = total > float64(len(items))
	delete(result, key)
	// Older fallback APIs silently omit unreachable workers. Compare membership
	// instead of reporting an empty successful result as a complete cluster read.
	registry, registryErr := client.Get("/nodes")
	if registryErr != nil {
		result["complete"], result["partial"] = false, true
		result["warning"] = "无法读取节点清单，异常查询的完整性未确认: " + registryErr.Error()
		return result, nil
	}
	expected := adminNodeAddresses(registry)
	missing := map[string]bool{}
	for _, field := range []string{"failedWorkers", "unreachableWorkers"} {
		for _, raw := range adminArray(result[field]) {
			if worker, ok := raw.(string); ok {
				missing[worker] = true
			}
		}
	}
	selectedFound := !o.Has("worker")
	for _, worker := range expected {
		if o.Has("worker") && o.Value("worker") != worker {
			continue
		}
		selectedFound = true
		rows, hasRows := grouped[worker].([]interface{})
		count, hasCount := grouped[worker+"Count"].(float64)
		if !hasRows || !hasCount || count < float64(len(rows)) {
			missing[worker] = true
		}
	}
	if !selectedFound {
		return result, fmt.Errorf("节点未注册: %s", o.Value("worker"))
	}
	failed := []string{}
	for worker := range missing {
		failed = append(failed, worker)
	}
	sort.Strings(failed)
	complete := len(expected) > 0 && len(failed) == 0 && result["complete"] != false && result["partial"] != true
	result["complete"], result["partial"], result["failedWorkers"] = complete, !complete, failed
	if !complete {
		result["warning"] = "异常记录读取不完整，请检查 failedWorkers 和节点状态"
	}
	return result, nil
}

func adminArray(value interface{}) []interface{} { result, _ := value.([]interface{}); return result }
func adminNumber(value interface{}) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case int:
		return float64(number)
	case int64:
		return float64(number)
	case json.Number:
		value, _ := number.Float64()
		return value
	}
	return 0
}
func adminNodeAddresses(registry map[string]interface{}) []string {
	addresses := []string{}
	for _, raw := range adminArray(adminData(registry)["nodes"]) {
		if node, ok := raw.(map[string]interface{}); ok {
			if address, ok := node["address"].(string); ok {
				addresses = append(addresses, address)
			}
		}
	}
	return addresses
}

func adminExportExceptions(path string, items []interface{}) (err error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("无法创建导出文件（不会覆盖已有文件）: %w", err)
	}
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = file.WriteString("\ufeff"); err != nil {
		return err
	}
	writer := csv.NewWriter(file)
	writer.UseCRLF = true
	fields := []string{"workerNode", "taskIndex", "parentTaskId", "direction", "sourceBucket", "destBucket", "key", "destinationKey", "localPath", "stage", "errorCode", "errorMessage", "objectSize", "retryCount", "timestamp", "timestampFormatted", "tmpFilePath", "storageId"}
	if err = writer.Write(fields); err != nil {
		return err
	}
	for _, raw := range items {
		row, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		cells := make([]string, len(fields))
		for i, field := range fields {
			if value := row[field]; value != nil {
				text, ok := value.(string)
				if !ok {
					encoded, encodeErr := json.Marshal(value)
					if encodeErr != nil {
						return encodeErr
					}
					text = string(encoded)
				}
				if text != "" && strings.ContainsRune("=+-@\t\r", rune(text[0])) {
					text = "'" + text
				}
				cells[i] = text
			}
		}
		if err = writer.Write(cells); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}
