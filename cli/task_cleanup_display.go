package main

import (
	"fmt"
	"io"
)

func taskCleanup(task map[string]interface{}) map[string]interface{} {
	cleanup, _ := task["cleanup"].(map[string]interface{})
	return cleanup
}

func taskCleanupLabel(out io.Writer, cleanup map[string]interface{}) string {
	switch getString(cleanup, "status") {
	case "DONE":
		return cliText(out, "已清理")
	case "QUEUED":
		return cliText(out, "等待清理")
	case "WAITING":
		return cliText(out, "等待自动继续")
	default:
		if getString(cleanup, "phase") == "MASTER" {
			return cliText(out, "清理主节点记录")
		}
		return cliText(out, "清理执行节点记录")
	}
}

func renderTaskCleanup(out io.Writer, id int, cleanup map[string]interface{}) {
	fmt.Fprintf(out, "#%d: %s · ", id, taskCleanupLabel(out, cleanup))
	fmt.Fprintf(out, cliText(out, "已清理 %s 条记录"), cliTaskCount(cleanup, "recordsDeleted"))
	if getInt(cleanup, "totalNodes") > 0 {
		fmt.Fprintf(out, cliText(out, " · 节点 %d/%d"), getInt(cleanup, "completedNodes"), getInt(cleanup, "totalNodes"))
	}
	fmt.Fprintln(out)
	if message := getString(cleanup, "error"); message != "" {
		fmt.Fprintln(out, "  "+safeCLIText(message))
	}
}

func renderTaskCleanupAdmission(out io.Writer, data map[string]interface{}) bool {
	details := objectList(data["details"])
	found := false
	for _, detail := range details {
		if taskCleanup(detail) != nil {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	pending := false
	for _, detail := range details {
		if cleanup := taskCleanup(detail); cleanup != nil && getString(cleanup, "status") != "DONE" && detail["purged"] != true {
			pending = true
		}
	}
	if pending {
		fmt.Fprintln(out, cliText(out, "清理请求已受理，将在后台继续。使用 task trash 查看进度。"))
	}
	for _, detail := range details {
		if cleanup := taskCleanup(detail); cleanup != nil {
			renderTaskCleanup(out, getInt(detail, "taskId"), cleanup)
			if message := getString(detail, "message"); message != "" {
				fmt.Fprintln(out, "  "+safeCLIText(message))
			}
		} else if detail["success"] == false {
			fmt.Fprintf(out, "#%d: %s\n", getInt(detail, "taskId"), safeCLIText(getString(detail, "message")))
		}
	}
	return true
}
