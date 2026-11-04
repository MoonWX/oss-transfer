package main

import (
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// exceptionTasks lists the parent tasks that have exception records, like the
// console's task picker (GET /exceptions/tasks).
func exceptionTasks(client *ApiClient, args []string) (map[string]interface{}, error) {
	o, err := adminOptions(args, []string{"search", "limit"}, nil, 1)
	if err != nil {
		return nil, err
	}
	kind := o.Positionals[0]
	if kind != "failed" && kind != "fallback" && kind != "local" {
		return nil, usageErrorf("异常类型必须是 failed、fallback 或 local")
	}
	query := url.Values{"type": {kind}}
	if o.Has("search") {
		id, err := taskID(strings.TrimSpace(o.Value("search")))
		if err != nil {
			return nil, err
		}
		query.Set("search", strconv.Itoa(id))
	}
	limit, err := adminInteger(o, "limit", 100, 1, 100)
	if err != nil {
		return nil, err
	}
	query.Set("limit", strconv.Itoa(limit))
	response, err := client.Get("/exceptions/tasks?" + query.Encode())
	if err != nil {
		return response, err
	}
	data := adminData(response)
	items, _ := data["items"].([]interface{})
	if items == nil {
		items = []interface{}{}
	}
	return map[string]interface{}{"success": true, "type": kind, "exceptionTasks": items, "hasMore": data["hasMore"] == true}, nil
}

func renderExceptionTasks(out io.Writer, result map[string]interface{}) bool {
	value, ok := result["exceptionTasks"]
	if !ok {
		return false
	}
	tasks := objectList(value)
	renderTaskRows(out, tasks, false)
	fmt.Fprint(out, cliTextf(out, "%d 个任务有异常记录\n", len(tasks)))
	if result["hasMore"] == true {
		fmt.Fprintln(out, cliText(out, "还有更多任务；可用 --search 任务ID 查找指定任务。"))
	}
	if len(tasks) > 0 {
		fmt.Fprint(out, cliTextf(out, "查看记录: oss-transfer-cli exceptions list %s --task-id %d\n", safeCLIText(result["type"]), getInt(tasks[0], "taskIndex")))
	}
	return true
}
