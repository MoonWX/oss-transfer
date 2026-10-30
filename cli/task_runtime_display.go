package main

import (
	"fmt"
	"io"
	"math"
)

func renderTaskRuntimeResult(out io.Writer, result map[string]interface{}) bool {
	data := responseData(result)
	if result["cleanupStatus"] == true {
		for _, row := range objectList(data["tasks"]) {
			id := getInt(row, "taskId")
			if cleanup := taskCleanup(row); cleanup != nil {
				renderTaskCleanup(out, id, cleanup)
			} else if row["purged"] == true {
				fmt.Fprintf(out, "#%d: %s\n", id, cliText(out, "已永久删除"))
			} else if row["missing"] == true {
				fmt.Fprintf(out, "#%d: %s\n", id, cliText(out, "未找到待清理记录"))
			} else {
				fmt.Fprintf(out, "#%d: %s\n", id, cliText(out, "等待清理"))
			}
		}
		return true
	}
	if _, ok := data["observedAt"]; !ok || getInt(data, "parentTaskId") == 0 {
		return false
	}
	if data["supported"] == false {
		fmt.Fprintln(out, cliText(out, "当前节点不支持文件进度，请升级 Agent。"))
		return true
	}
	if data["partial"] == true {
		fmt.Fprintln(out, cliText(out, "部分文件进度暂不可用，请稍后刷新。"))
	}
	rows := objectList(data["items"])
	if len(rows) == 0 {
		fmt.Fprintln(out, cliText(out, "当前没有可显示的文件传输进度。"))
		return true
	}
	table := newCLITable(out)
	fmt.Fprintln(table, cliText(out, "子任务\t执行节点\t文件\t进度\t已传输 / 大小\t阶段"))
	for _, row := range rows {
		percent := "—"
		size, sizeOK := cliNumeric(row["size"])
		transferred, transferredOK := cliNumeric(row["transferredBytes"])
		if sizeOK && transferredOK && size > 0 && transferred >= 0 && !math.IsInf(size, 0) && !math.IsInf(transferred, 0) && !math.IsNaN(size) && !math.IsNaN(transferred) {
			percent = fmt.Sprintf("%.1f%%", math.Min(100, 100*transferred/size))
		}
		fmt.Fprintf(table, "#%s\t%s\t%s\t%s\t%s / %s\t%s\n", safeCLIText(getString(row, "childTaskId")), safeCLIText(getString(row, "executionNodeLabel")), safeCLIText(getString(row, "sourceKey")), percent, cliTaskSize(row, "transferredBytes"), cliTaskSize(row, "size"), safeCLIText(getString(row, "stage")))
	}
	table.Flush()
	if data["hasMore"] == true {
		fmt.Fprintln(out, cliText(out, "更多文件请使用 --child 查看指定子任务。"))
	}
	return true
}
