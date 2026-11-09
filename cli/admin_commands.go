package main

import (
	"encoding/csv"
	"encoding/json"
	"math"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// runAdminCommand uses the same master APIs as the console.
func runAdminCommand(client *ApiClient, group string, args []string) (map[string]interface{}, error) {
	if len(args) == 0 {
		return nil, cliErrorf("缺少 %s 子命令，请运行 oss-transfer-cli %s --help", group, group)
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
		return nil, cliErrorf("未知命令组: %s", group)
	}
}

func adminOptions(args, values, switches []string, count int) (commandOptions, error) {
	o, err := parseCommandOptions(args, values, switches)
	if err == nil {
		for name, supplied := range o.Values {
			if len(supplied) > 1 {
				return o, cliErrorf("--%s 只能指定一次", name)
			}
		}
	}
	if err == nil && len(o.Positionals) != count {
		err = cliErrorf("需要 %d 个位置参数，实际收到 %d 个", count, len(o.Positionals))
	}
	return o, err
}

func adminConfirmed(o commandOptions) error {
	if !o.Has("yes") {
		return cliErrorf("此操作需要明确确认，请检查目标后添加 --yes")
	}
	return nil
}

func adminNodeAddress(value string) error {
	if !utf8.ValidString(value) || len(value) == 0 || len(value) > 256 {
		return cliErrorf("请指定有效的节点名称、IPv4 或 IPv6 地址")
	}
	if ip := net.ParseIP(value); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return cliErrorf("不能使用未指定或组播地址")
		}
		return nil
	}
	onlyNumbers := true
	for _, ch := range value {
		if !(ch == '.' || ch >= '0' && ch <= '9') {
			onlyNumbers = false
		}
		if !(unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '-' || ch == '_' || ch == '.') {
			return cliErrorf("节点名称不能包含端口、空格或路径分隔符")
		}
	}
	if onlyNumbers || value == "." || value == ".." {
		return cliErrorf("无效节点地址")
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
	if action == "update" {
		for _, arg := range args {
			if arg == "--child-concurrency" || strings.HasPrefix(arg, "--child-concurrency=") || arg == "--object-concurrency" || strings.HasPrefix(arg, "--object-concurrency=") {
				return agentUpdate(client, args)
			}
		}
	}
	switch action {
	case "network":
		return nodeNetwork(client, args)
	case "details":
		return nodeDetails(client, args)
	case "show":
		return agentShow(client, args)
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
			result["warning"] = client.text("节点已列出，但运行状态未完整读取: ") + localizedCLIError(client.locale, statusErr)
			return result, nil
		}
		result["workers"] = status["workers"]
		if displays, ok := status["nodeDisplays"]; ok {
			result["nodeDisplays"] = displays
		}
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
		if action == "add" {
			values = append(values, "master-url")
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
		if err := adminNodeAddress(original); err != nil {
			return nil, err
		}
		if action == "remove" {
			if !o.Has("yes") {
				// Per-object task results and comparison results live on the node.
				return nil, cliErrorf("移除执行节点后，保存在该节点上的任务报告（逐个对象的结果）和对象比对结果将无法再获取：任务报告会不完整，比对记录会消失。需要保留时，请先用 task report 和 object export 导出。确认移除请添加 --yes")
			}
			result, err := client.Delete("/nodes/" + url.PathEscape(original))
			if err == nil {
				setCLISummary(result, strings.TrimSuffix(client.locale.format("已移除执行节点 %s。该主机上的 Agent 服务会自行停止且不再重启；如需删除其程序和数据，请在该主机运行安装脚本并加 --uninstall --role agent\n", original), "\n"))
			}
			return result, err
		}
		address, label := original, o.Value("label")
		if action == "update" {
			if !o.Has("address") && !o.Has("label") {
				return nil, cliErrorf("请通过 --address 或 --label 指定要修改的字段")
			}
			if o.Has("address") {
				address = o.Value("address")
				if err := adminNodeAddress(address); err != nil {
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
					return nil, cliErrorf("节点未注册: %s", original)
				}
			}
		}
		if len([]rune(label)) > 80 || strings.ContainsAny(label, "\r\n") {
			return nil, cliErrorf("节点名称不能超过 80 个字符或包含换行")
		}
		body := map[string]interface{}{"address": address, "label": strings.TrimSpace(label)}
		if action == "add" {
			if body["masterUrl"], err = invitationMasterURL(client, o); err != nil {
				return nil, err
			}
			return client.Post("/agents/invitations", body)
		}
		result, err := client.Put("/nodes/"+url.PathEscape(original), body)
		if err == nil {
			setCLISummary(result, nodeUpdatedSummary(client, original, body))
		}
		return result, err
	default:
		return nil, cliErrorf("未知 node 子命令: %s", action)
	}
}

func runAgentAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	switch action {
	case "invite-status":
		return agentInviteStatus(client, args)
	case "list", "show", "details", "remove":
		return runNodeAdmin(client, action, args)
	case "update":
		return agentUpdate(client, args)
	}
	if action != "invite" {
		return nil, cliErrorf("未知 agent 子命令: %s", action)
	}
	o, err := adminOptions(args, []string{"label", "address", "master-url"}, nil, 0)
	if err != nil {
		return nil, err
	}
	address, label := strings.TrimSpace(o.Value("address")), strings.TrimSpace(o.Value("label"))
	if o.Has("address") {
		if err := adminNodeAddress(address); err != nil {
			return nil, err
		}
	}
	if len([]rune(label)) > 80 || strings.ContainsAny(label, "\r\n") {
		return nil, cliErrorf("节点名称不能超过 80 个字符或包含换行")
	}
	masterURL, err := invitationMasterURL(client, o)
	if err != nil {
		return nil, err
	}
	return client.Post("/agents/invitations", map[string]interface{}{"address": address, "label": label, "masterUrl": masterURL})
}

// invitationMasterURL is the Master address an invitation gives the Agent:
// --master-url when given, otherwise the address the CLI is connected to.
func invitationMasterURL(client *ApiClient, o commandOptions) (string, error) {
	if o.Has("master-url") {
		return masterOrigin(o.Value("master-url"), "master-url")
	}
	return strings.TrimSuffix(client.baseUrl, "/api/master"), nil
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
					for _, field := range []string{"accessKey", "secretKey", "sessionToken"} {
						if value, ok := config[field].(string); ok && value != "" {
							config[field] = "********"
						}
					}
				}
			}
		}
		return result, err
	case "buckets", "test", "remove":
		switches, values := []string{}, []string{"bucket"}
		if action == "remove" {
			switches, values = append(switches, "yes"), nil
		}
		o, err := adminOptions(args, values, switches, 1)
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
			result, err := client.Delete("/s3-config/" + url.PathEscape(alias))
			if err == nil {
				setCLISummary(result, storageSavedSummary(client, client.locale.format("存储连接 %s 已删除", alias), result)...)
			}
			return result, err
		}
		path := "/s3-config/" + url.PathEscape(alias) + "/buckets"
		if o.Has("bucket") {
			bucket := o.Value("bucket")
			if strings.TrimSpace(bucket) == "" || strings.ContainsAny(bucket, "/\\ ") {
				return nil, cliUsageErrorf("--bucket 需为存储桶名称，不能为空或包含斜杠、空格")
			}
			path += "?bucket=" + url.QueryEscape(bucket)
		}
		result, err := client.Get(path)
		if err != nil {
			return result, storagePathStyleAdvice(result, err, alias)
		}
		if action == "test" {
			_, checkSupported := result["pathStyle"]
			switch {
			case getString(result, "checkedBucket") != "":
				result["message"] = client.text("连接测试通过：已列举存储桶，并用当前寻址方式访问了一个存储桶")
			case getString(result, "bucketCheckFailed") != "":
				result["message"] = client.text("已读取存储桶列表；第一个存储桶的存储桶级检查未通过，见警告；可用 --bucket 检查要迁移的存储桶")
			case !checkSupported:
				// Masters before v4.2 only list buckets and ignore ?bucket=.
				result["message"] = client.text("已读取存储桶列表；Master 版本较旧，未检查存储桶级访问")
			case result["bucketCheckSupported"] == false:
				result["message"] = client.text("已读取存储桶列表；执行节点版本较旧，未检查存储桶级访问")
			case len(adminArray(result["buckets"])) == 0 && !o.Has("bucket"):
				result["message"] = client.text("已读取存储桶列表；没有可检查的存储桶，可用 --bucket 指定一个")
			default:
				result["message"] = client.text("已读取存储桶列表；未检查存储桶级访问")
			}
		}
		return result, nil
	case "add", "update":
		o, err := adminOptions(args, []string{"endpoint", "region", "access-key-env", "access-key-file", "secret-key-env", "secret-key-file", "session-token-env", "session-token-file", "validate", "v2-signer", "path-style"}, []string{"clear-session-token"}, 1)
		if err != nil {
			return nil, err
		}
		if o.Has("clear-session-token") && (o.Has("session-token-env") || o.Has("session-token-file")) {
			return nil, cliErrorf("--clear-session-token 不能与 --session-token-env 或 --session-token-file 同时使用")
		}
		alias := o.Positionals[0]
		if err := adminAlias(alias); err != nil {
			return nil, err
		}
		body := map[string]interface{}{"alias": alias, "validate": true, "useV2SignerType": nil, "pathStyleEnable": nil}
		if action == "update" {
			if len(o.Values) == 0 && !o.Has("clear-session-token") {
				return nil, cliErrorf("请指定至少一个要修改的字段")
			}
			current, err := client.Get("/s3-config")
			if err != nil {
				return current, err
			}
			found := false
			for _, raw := range adminArray(current["configs"]) {
				config, _ := raw.(map[string]interface{})
				if config["alias"] == alias {
					for _, key := range []string{"endpoint", "region", "useV2SignerType", "pathStyleEnable"} {
						if value, exists := config[key]; exists {
							body[key] = value
						}
					}
					found = true
					break
				}
			}
			if !found {
				return nil, cliErrorf("存储连接不存在: %s", alias)
			}
		}
		if o.Has("endpoint") {
			body["endpoint"] = strings.TrimSpace(o.Value("endpoint"))
		}
		if o.Has("region") {
			body["region"] = strings.TrimSpace(o.Value("region"))
		}
		for flag, field := range map[string]string{"access-key": "accessKey", "secret-key": "secretKey", "session-token": "sessionToken"} {
			value, supplied, err := adminCredential(o, flag)
			if err != nil {
				return nil, err
			}
			if supplied {
				body[field] = value
			}
		}
		if o.Has("clear-session-token") {
			body["sessionToken"] = ""
		}
		for flag, field := range map[string]string{"validate": "validate", "v2-signer": "useV2SignerType", "path-style": "pathStyleEnable"} {
			if o.Has(flag) {
				switch o.Value(flag) {
				case "true":
					body[field] = true
				case "false":
					body[field] = false
				case "inherit", "auto", "default":
					if flag == "validate" {
						return nil, cliErrorf("--validate 只支持 true 或 false")
					}
					body[field] = nil
				default:
					if flag == "validate" {
						return nil, cliErrorf("--validate 只支持 true 或 false")
					}
					if flag == "path-style" {
						return nil, cliUsageErrorf("--path-style 需使用 auto、true 或 false")
					}
					return nil, cliUsageErrorf("--v2-signer 需使用 default、true 或 false")
				}
			}
		}
		access, _ := body["accessKey"].(string)
		secret, _ := body["secretKey"].(string)
		if (access == "") != (secret == "") {
			return nil, cliErrorf("更换凭据时，请同时提供 Access Key 和 Secret Key")
		}
		required := []string{"endpoint"}
		if action == "add" || access != "" || secret != "" {
			required = append(required, "accessKey", "secretKey")
		}
		for _, field := range required {
			value, _ := body[field].(string)
			if strings.TrimSpace(value) == "" || (field != "endpoint" && value == "********") {
				return nil, cliErrorf("缺少 %s，请提供 Endpoint 或对应密钥的环境变量/文件", field)
			}
		}
		endpoint, err := url.Parse(body["endpoint"].(string))
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil {
			return nil, cliErrorf("Endpoint 必须是 http:// 或 https:// 地址，且不得包含用户名或密码，例如 https://s3.example.com 或 http://10.0.0.5:9000")
		}
		var result map[string]interface{}
		if action == "add" {
			result, err = client.Post("/s3-config", body)
		} else {
			result, err = client.Put("/s3-config/"+url.PathEscape(alias), body)
		}
		if err != nil {
			return result, storagePathStyleAdvice(result, err, alias)
		}
		setCLISummary(result, storageSavedSummary(client, client.locale.format("存储连接 %s 已保存", alias), result)...)
		return result, nil
	default:
		return nil, cliErrorf("未知 storage 子命令: %s", action)
	}
}

// storagePathStyleAdvice adds the command that applies the Master's advice to
// switch a connection to path-style addressing.
func storagePathStyleAdvice(result map[string]interface{}, err error, alias string) error {
	if err == nil || result == nil || result["pathStyleHint"] != true {
		return err
	}
	return cliErrorf("%w\n提示: 运行 oss-transfer-cli storage update %s --path-style true", err, alias)
}

func adminAlias(alias string) error {
	if strings.TrimSpace(alias) == "" || strings.TrimSpace(alias) != alias || strings.ContainsAny(alias, "/\\\r\n") || alias == "." || alias == ".." {
		return cliErrorf("连接名称不能为空、含首尾空白或包含路径分隔符")
	}
	return nil
}

func adminCredential(o commandOptions, flag string) (string, bool, error) {
	env, file := o.Value(flag+"-env"), o.Value(flag+"-file")
	if o.Has(flag+"-env") && o.Has(flag+"-file") {
		return "", false, cliErrorf("--%s-env 和 --%s-file 不能同时使用", flag, flag)
	}
	if !o.Has(flag+"-env") && !o.Has(flag+"-file") {
		return "", false, nil
	}
	var value string
	if env != "" {
		var found bool
		value, found = os.LookupEnv(env)
		if !found {
			return "", false, cliErrorf("密钥环境变量未设置: %s", env)
		}
	} else {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", false, cliErrorf("无法读取 --%s-file 指定的密钥文件", flag)
		}
		value = strings.TrimRight(string(data), "\r\n")
	}
	if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
		return "", false, cliErrorf("--%s 的密钥不能为空或包含换行", flag)
	}
	return value, true, nil
}

func runLimitAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	switch action {
	case "list":
		if _, err := adminOptions(args, nil, nil, 0); err != nil {
			return nil, err
		}
		result, err := client.Get("/rate-limit")
		if err == nil {
			displays := collectCLINodeDisplays(result)
			if workers, ok := result["workers"].(map[string]interface{}); ok {
				rates := []interface{}{}
				addresses := make([]string, 0, len(workers))
				for address := range workers {
					addresses = append(addresses, address)
				}
				sort.Strings(addresses)
				for _, address := range addresses {
					worker, _ := workers[address].(map[string]interface{})
					if worker["success"] != true {
						continue
					}
					rate, _ := worker["rateLimit"].(map[string]interface{})
					if rate == nil {
						continue
					}
					rate = withCLINodeDisplay(rate, address, displays)
					rate["workerIp"] = address
					rates = append(rates, rate)
				}
				result["rateLimits"] = rates
			}
		}
		return result, err
	case "history":
		o, err := adminOptions(args, []string{"limit"}, nil, 0)
		if err != nil {
			return nil, err
		}
		limit, err := adminInteger(o, "limit", 50, 1, 200)
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
			return nil, cliErrorf("调节详情需要 --worker 和 --event-id")
		}
		if err := adminNodeAddress(o.Value("worker")); err != nil {
			return nil, err
		}
		query := url.Values{"workerIp": {o.Value("worker")}, "eventId": {o.Value("event-id")}}
		if o.Has("cursor") {
			query.Set("cursor", o.Value("cursor"))
		}
		result, err := client.Get("/rate-limit/adjustment?" + query.Encode())
		if err == nil && result["historyComplete"] == false {
			result["complete"] = false
			result["warning"] = client.text("本轮部分调节记录未保存，详情不完整")
		}
		return result, err
	case "set":
		o, err := adminOptions(args, []string{"worker", "unit"}, []string{"all"}, 2)
		if err != nil {
			return nil, err
		}
		kind := o.Positionals[0]
		if kind != "list" && kind != "put" && kind != "head" && kind != "bandwidth" {
			return nil, cliErrorf("限速类型必须是 list、put、head 或 bandwidth")
		}
		if o.Has("worker") == o.Has("all") {
			return nil, cliErrorf("请指定 --worker <IPv4> 或 --all，且不能同时使用")
		}
		if o.Has("worker") {
			if err := adminNodeAddress(o.Value("worker")); err != nil {
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
		return nil, cliErrorf("未知 limit 子命令: %s", action)
	}
}

func adminRate(kind, raw, unit string) (float64, error) {
	if kind != "bandwidth" && unit != "" {
		return 0, cliErrorf("--unit 只用于 bandwidth，其他类型单位为次/秒")
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
			return 0, cliErrorf("带宽单位必须为 bps、Kbps、Mbps 或 Gbps；默认 Mbps")
		}
	}
	if raw == "unlimited" {
		return 0, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	value *= multiplier
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0, cliErrorf("限速须为大于 0 的有限数值；取消限速请明确使用 unlimited")
	}
	return value, nil
}

func adminInteger(o commandOptions, name string, fallback, min, max int) (int, error) {
	if !o.Has(name) {
		return fallback, nil
	}
	value, err := strconv.Atoi(o.Value(name))
	if err != nil || value < min || value > max {
		return 0, cliErrorf("--%s 必须是 %d 到 %d 之间的整数", name, min, max)
	}
	return value, nil
}

func runExceptionsAdmin(client *ApiClient, action string, args []string) (map[string]interface{}, error) {
	if action == "clear" {
		o, err := adminOptions(args, []string{"task-id", "worker", "direction", "source-bucket", "dest-bucket", "endpoint"}, []string{"all", "yes"}, 1)
		if err != nil {
			return nil, err
		}
		kind := o.Positionals[0]
		if kind != "failed" && kind != "fallback" && kind != "local" {
			return nil, cliErrorf("异常类型必须是 failed、fallback 或 local")
		}
		if o.Has("all") == (len(o.Values) > 0) {
			return nil, cliErrorf("请指定清理筛选条件或 --all，不能同时使用")
		}
		if err := adminConfirmed(o); err != nil {
			return nil, err
		}
		body := map[string]interface{}{"type": kind}
		if kind == "fallback" {
			delete(body, "type")
		}
		for flag, field := range map[string]string{"worker": "workerNode", "direction": "direction", "source-bucket": "sourceBucket", "dest-bucket": "destBucket", "endpoint": "endpoint"} {
			if o.Has(flag) {
				if strings.TrimSpace(o.Value(flag)) == "" {
					return nil, cliErrorf("--%s 不能为空", flag)
				}
				body[field] = o.Value(flag)
			}
		}
		if o.Has("task-id") {
			id, err := adminInteger(o, "task-id", 0, 1, math.MaxInt32)
			if err != nil {
				return nil, err
			}
			body["taskIndex"] = id
		}
		path := kind
		if kind == "local" {
			path = "failed"
		}
		if kind != "fallback" {
			// Older masters clear unresolved records too. Require the new API
			// before sending a destructive request, even when --all is explicit.
			query := make(map[string]interface{}, len(body)+1)
			for key, value := range body {
				query[key] = value
			}
			query["limit"] = 1
			probe, err := client.Post("/failed", query)
			if err != nil {
				return nil, err
			}
			if _, supported := probe["clearableCount"]; !supported {
				return nil, cliErrorf("Master 不支持安全清理已处理记录，请先升级 Master")
			}
		}
		result, err := client.Post("/"+path+"/clear", body)
		if _, confirmed := result["complete"].(bool); err == nil && kind == "fallback" && !confirmed {
			result["complete"] = false
			result["warning"] = client.text("降级记录清空请求已提交；此 API 不逐节点确认结果，请重新查询检查记录及节点完整性")
			result["message"] = result["warning"]
		}
		if err == nil {
			switch {
			case result["supported"] == false:
				setCLISummary(result, client.text("当前传输引擎不使用降级记录，无需清理"))
			case result["complete"] == false:
				// The generic output keeps the warning and node lists.
			default:
				setCLISummary(result, client.locale.format("已清理 %d 条已处理的记录", getInt(result, "clearedCount")))
			}
		}
		return result, err
	}
	if action != "list" && action != "export" {
		return nil, cliErrorf("未知 exceptions 子命令: %s", action)
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
		return nil, cliErrorf("异常类型必须是 failed、fallback 或 local")
	}
	if action == "export" && o.Value("output") == "" {
		return nil, cliErrorf("导出需要 --output <文件路径>")
	}
	if kind == "local" && action == "export" {
		for key := range o.Values {
			if key != "output" {
				return nil, cliErrorf("本地迁移报告 API 导出所有已结束或停止任务的 ZIP，不支持 --%s 筛选；单任务报告请使用 task report", key)
			}
		}
		if err := client.Download("GET", "/api/master/local-transfers/report", nil, o.Value("output")); err != nil {
			return nil, err
		}
		result := map[string]interface{}{"success": true, "output": o.Value("output"), "message": client.text("本地迁移报告已保存"), "scope": "all-local-transfers"}
		if err := taskReportCompleteness(o.Value("output"), result); err != nil {
			return result, err
		}
		if getString(result, "reportKind") == "" {
			setCLISummary(result, client.locale.format("本地迁移报告已保存到 %s", o.Value("output")))
		}
		return result, nil
	}
	var result map[string]interface{}
	if kind == "local" {
		result, err = adminLocalExceptions(client, o)
	} else {
		result, err = adminOSSExceptions(client, kind, o)
	}
	if err == nil && action == "list" {
		setCLISummary(result, exceptionsListSummary(client, kind, o, result)...)
	}
	if err != nil || action != "export" {
		return result, err
	}
	if err := adminExportExceptions(o.Value("output"), adminArray(result["items"])); err != nil {
		return result, err
	}
	result["output"] = o.Value("output")
	result["exported"] = len(adminArray(result["items"]))
	result["message"] = client.text("已导出本次查询返回的异常记录；完整性及截断状态见 complete、partial 和 truncated")
	delete(result, "items")
	summary := []string{client.locale.format("已导出 %d 条记录到 %s", result["exported"], o.Value("output"))}
	if result["truncated"] == true {
		summary = append(summary, client.text("仅导出了部分记录；可调大 --limit 后重新导出"))
	}
	setCLISummary(result, summary...)
	return result, nil
}

func adminLocalExceptions(client *ApiClient, o commandOptions) (map[string]interface{}, error) {
	for _, flag := range []string{"source-bucket", "dest-bucket"} {
		if o.Has(flag) {
			return nil, cliErrorf("local 不支持 --%s，请使用 --endpoint 筛选", flag)
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
		if err := adminNodeAddress(o.Value("worker")); err != nil {
			return nil, err
		}
	}
	if direction := o.Value("direction"); direction != "" && direction != "OSS_TO_LOCAL" && direction != "LOCAL_TO_OSS" {
		return nil, cliErrorf("local 的 --direction 必须为 OSS_TO_LOCAL 或 LOCAL_TO_OSS")
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
		result["warning"] = client.text("后端每节点最多读取 1000 条异常；更多记录请导出报告")
	}
	return result, nil
}

func adminOSSExceptions(client *ApiClient, kind string, o commandOptions) (map[string]interface{}, error) {
	for _, flag := range []string{"offset", "direction", "endpoint", "storage-id"} {
		if o.Has(flag) {
			return nil, cliErrorf("%s 不支持 --%s", kind, flag)
		}
	}
	limit, err := adminInteger(o, "limit", 1000, 1, 10000)
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
		if err := adminNodeAddress(o.Value("worker")); err != nil {
			return nil, err
		}
		body["workerNode"] = o.Value("worker")
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
		return result, cliErrorf("服务未返回有效的异常记录分组")
	}
	workers := []string{}
	for worker, value := range grouped {
		if _, ok := value.([]interface{}); ok && (!o.Has("worker") || o.Value("worker") == worker) {
			workers = append(workers, worker)
		}
	}
	sort.Strings(workers)
	items, total := []interface{}{}, float64(0)
	displays := collectCLINodeDisplays(result)
	for _, worker := range workers {
		for _, raw := range adminArray(grouped[worker]) {
			if row, ok := raw.(map[string]interface{}); ok {
				row = withCLINodeDisplay(row, worker, displays)
				row["workerNode"] = worker
				items = append(items, row)
			}
		}
		total += adminNumber(grouped[worker+"Count"])
	}
	result["items"], result["total"], result["respondingNodes"] = items, total, workers
	result["truncated"] = total > float64(len(items))
	delete(result, key)
	// The Go API declares the exact queried node set after parent/node filters.
	// Do not compare it with unrelated cluster members as the V4 fallback did.
	if queried, authoritative := result["queriedWorkers"].([]interface{}); authoritative {
		if result["supported"] == false {
			return result, nil
		}
		missing := []string{}
		for _, raw := range queried {
			address, valid := raw.(string)
			if !valid {
				return result, cliErrorf("服务返回无效节点范围")
			}
			rows, hasRows := grouped[address].([]interface{})
			count, hasCount := grouped[address+"Count"].(float64)
			if !hasRows || !hasCount || count < float64(len(rows)) {
				missing = append(missing, address)
			}
		}
		if len(missing) > 0 {
			result["complete"], result["partial"], result["missingWorkers"] = false, true, missing
		}
		return result, nil
	}
	// Older fallback APIs silently omit unreachable workers. Compare membership
	// instead of reporting an empty successful result as a complete cluster read.
	registry, registryErr := client.Get("/nodes")
	if registryErr != nil {
		result["complete"], result["partial"] = false, true
		result["warning"] = client.text("无法读取节点清单，异常查询的完整性未确认: ") + localizedCLIError(client.locale, registryErr)
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
		return result, cliErrorf("节点未注册: %s", o.Value("worker"))
	}
	failed := []string{}
	for worker := range missing {
		failed = append(failed, worker)
	}
	sort.Strings(failed)
	complete := len(expected) > 0 && len(failed) == 0 && result["complete"] != false && result["partial"] != true
	result["complete"], result["partial"], result["failedWorkers"] = complete, !complete, failed
	if !complete {
		result["warning"] = client.text("异常记录读取不完整，请检查 failedWorkers 和节点状态")
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
		return cliErrorf("无法创建导出文件（不会覆盖已有文件）: %w", err)
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
	fields := []string{"workerNode", "taskIndex", "parentTaskId", "direction", "sourceBucket", "destBucket", "key", "destinationKey", "localPath", "stage", "errorCode", "errorMessage", "objectSize", "retryCount", "timestamp", "timestampFormatted", "tmpFilePath", "storageId", "nodeId", "retryStatus", "retryUpdatedAt", "retryError", "retryGeneration", "retryExecutionNode", "retryExecutionNodeIp", "reasonCode", "retryReasonCode"}
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
			value := row[field]
			identity := firstNodeString(row, "workerNode", "executionNodeIp")
			if field == "workerNode" {
				value = cliNodeLabel(row, identity, localeEN)
			} else if field == "nodeId" {
				value = firstNodeString(row, "nodeId", "workerNode", "executionNodeIp")
			} else if field == "retryExecutionNode" {
				if identity := getString(row, "retryExecutionNodeIp"); identity != "" {
					display, _ := row["retryExecutionNodeDisplay"].(map[string]interface{})
					value = cliNodeLabel(display, identity, localeEN)
				}
			}
			if value != nil {
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

// storageSavedSummary reports a saved or deleted connection and its
// synchronization to the execution nodes.
func storageSavedSummary(client *ApiClient, done string, result map[string]interface{}) []string {
	lines := []string{done}
	synced, failed := getInt(result, "successCount"), getInt(result, "failedCount")
	if _, reported := result["successCount"]; reported {
		if failed == 0 {
			lines[0] += client.locale.format("，已同步到 %d 个节点", synced)
		} else {
			lines[0] += client.locale.format("，已同步到 %d 个节点，%d 个节点等待同步", synced, failed)
		}
	}
	if failed > 0 {
		for _, detail := range cliStringList(result["details"]) {
			lines = append(lines, "  "+detail)
		}
	}
	for _, key := range []string{"warning", "connectionWarning"} {
		if warning := getString(result, key); warning != "" {
			lines = append(lines, warning)
		}
	}
	return lines
}
