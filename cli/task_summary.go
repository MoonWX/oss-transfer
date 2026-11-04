package main

import (
	"fmt"
	"io"
)

// renderTaskListSummary prints the console's overview totals for the listed tasks.
func renderTaskListSummary(out io.Writer, data map[string]interface{}, tasks []map[string]interface{}) {
	if len(tasks) == 0 {
		return
	}
	var running, completed, failed, objects, failedObjects, transferred int64
	for _, task := range tasks {
		switch getString(task, "status") {
		case "RUNNING", "LIST_DONE":
			running++
		case "COMPLETED":
			completed++
		case "FAILED", "PARTIAL_FAILED":
			failed++
		}
		objects += getInt64(task, "putObjectCount")
		failedObjects += getInt64(task, "failedObjectCount")
		// Live bytes include in-flight data; older Masters report durable bytes only.
		if _, ok := task["transferredBytes"]; ok {
			transferred += getInt64(task, "transferredBytes")
		} else {
			transferred += getInt64(task, "putObjectSize")
		}
	}
	fmt.Fprint(out, cliTextf(out, "合计 %d 个任务：执行中 %d，已完成 %d，失败 %d；已迁移 %s（%d 个对象），失败对象 %d\n", len(tasks), running, completed, failed, formatSize(transferred), objects, failedObjects))
	if incompleteResult(data) {
		fmt.Fprintln(out, cliText(out, "部分节点未返回数据，合计不完整。"))
	}
}

// renderTaskFilterScope warns that filters only covered the newest page.
func renderTaskFilterScope(out io.Writer, data map[string]interface{}) {
	if window := getInt(data, "filterWindow"); window > 0 && getInt(data, "nextBeforeId") > 0 {
		fmt.Fprint(out, cliTextf(out, "筛选只在最新的 %d 个任务中进行；添加 --all 查看全部匹配的任务\n", window))
	}
}
