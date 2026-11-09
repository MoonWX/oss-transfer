package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
)

var taskQPSFlags = map[string]string{"list-qps": "listQps", "head-qps": "headQps", "put-qps": "putQps"}

func taskRequestLimitOptions(opts commandOptions) (map[string]interface{}, error) {
	limits := map[string]interface{}{}
	for flag, field := range taskQPSFlags {
		if !opts.Has(flag) {
			continue
		}
		value := opts.Value(flag)
		if value == "unlimited" {
			limits[field] = float64(0)
			continue
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || !validTaskQPS(n) {
			return nil, cliErrorf("--%s 需为 0 或 0.000001–1000000000；取消限制可用 unlimited", flag)
		}
		limits[field] = n
	}
	return limits, nil
}

func validTaskQPS(n float64) bool {
	return !math.IsNaN(n) && !math.IsInf(n, 0) && (n == 0 || n >= 1e-6 && n <= 1e9)
}

func taskLimits(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := adminOptions(args, []string{"list-qps", "head-qps", "put-qps", "bandwidth-mbps"}, nil, 1)
	if err != nil {
		return nil, err
	}
	id, err := taskID(opts.Positionals[0])
	if err != nil {
		return nil, err
	}
	patch, err := taskRequestLimitOptions(opts)
	if err != nil {
		return nil, err
	}
	body := map[string]interface{}{}
	if len(patch) > 0 {
		// Do not GET and resubmit the other limits: concurrent edits to HEAD
		// or bandwidth must survive a change to LIST.
		body["requestLimitsPatch"] = patch
	}
	if opts.Has("bandwidth-mbps") {
		var limit interface{}
		if value := opts.Value("bandwidth-mbps"); value != "unlimited" {
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0.1 || n > 1000000 {
				return nil, cliErrorf("任务带宽为 0.1–1000000 Mbps；取消额外限制使用 unlimited")
			}
			limit = n
		}
		body["limitMbps"] = limit
	}
	endpoint := fmt.Sprintf("/tasks/%d/bandwidth-limit", id)
	if len(body) == 0 {
		return client.Get(endpoint)
	}
	return client.Put(endpoint, body)
}

// cliTaskRequestLimitFields lists a task's request limits and their scope;
// limits without a scope are totals for all of the task's execution nodes.
func cliTaskRequestLimitFields(out io.Writer, limits map[string]interface{}) [][2]string {
	scope := cliText(out, "所有执行节点合计")
	if getString(limits, "scope") == "PER_NODE" {
		scope = cliText(out, "每个执行节点")
	}
	fields := [][2]string{{cliText(out, "请求上限范围"), scope}}
	for _, request := range []struct{ field, label string }{{"listQps", "LIST / GET QPS"}, {"headQps", "HEAD QPS"}, {"putQps", "PUT QPS"}} {
		value := cliText(out, "仅受节点上限约束")
		if qps := getFloat(limits, request.field); qps > 0 {
			value = strconv.FormatFloat(qps, 'f', -1, 64)
		}
		fields = append(fields, [2]string{request.label, value})
	}
	return fields
}

// renderTaskLimitsResult prints the limits that task limits and task bandwidth
// read or change. The response also lists every execution node with its
// address, so it must be recognized before the node registry table.
func renderTaskLimitsResult(out io.Writer, result map[string]interface{}) bool {
	data := responseData(result)
	_, hasBandwidth := data["limitMbps"]
	applied, hasApplied := data["applied"].(bool)
	if !hasBandwidth || !hasApplied {
		return false
	}
	bandwidth := cliText(out, "仅受节点上限约束")
	if mbps, ok := cliNumeric(data["limitMbps"]); ok && mbps > 0 {
		bandwidth = strconv.FormatFloat(mbps, 'f', -1, 64) + " Mbps"
	}
	// Without request limits the rows still show the scope: request limits
	// set later are task totals too.
	limits, _ := data["requestLimits"].(map[string]interface{})
	fields := append([][2]string{{cliText(out, "任务带宽"), bandwidth}}, cliTaskRequestLimitFields(out, limits)...)
	// Imported legacy tasks can carry a bandwidth limit with the same scope.
	if bps, ok := cliNumeric(limits["bandwidthBitsPerSecond"]); ok && bps > 0 {
		fields = append(fields, [2]string{cliText(out, "旧版带宽上限"), strconv.FormatFloat(bps/1e6, 'f', -1, 64) + " Mbps"})
	}
	state := cliText(out, "部分节点未生效")
	if applied {
		state = cliText(out, "所有执行节点已生效")
	} else if data["pending"] == true {
		state = cliText(out, "正在同步到执行节点")
	}
	fields = append(fields, [2]string{cliText(out, "生效状态"), state})
	w := newCLITable(out)
	for _, field := range fields {
		fmt.Fprintf(w, "%s\t%s\n", field[0], safeCLIText(field[1]))
	}
	w.Flush()
	// A node that is still receiving the latest limits has not failed.
	for _, node := range objectList(data["nodes"]) {
		if node["applied"] != true && node["pending"] != true {
			message := getString(node, "message")
			if message == "" {
				message = cliText(out, "未生效")
			}
			fmt.Fprintf(out, "%s: %s\n", safeCLIText(cliNodeLabel(node, getString(node, "address"), localeOf(out))), safeCLIText(message))
		}
	}
	renderCLIAdminMessage(out, result)
	return true
}
