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
	// A Master that passes each file's basis on says so: a file without one
	// is then an older Agent's. An older Master drops the basis of every
	// file, so which Agent counted a file is unknown.
	reported := data["basisReported"] == true
	target, older := false, false
	for _, row := range rows {
		finalizing := getString(row, "stage") == "FINALIZING"
		percent := "—"
		size, sizeOK := cliNumeric(row["size"])
		transferred, transferredOK := cliNumeric(row["transferredBytes"])
		if sizeOK && transferredOK && size > 0 && transferred >= 0 && !math.IsInf(size, 0) && !math.IsInf(transferred, 0) && !math.IsNaN(size) && !math.IsNaN(transferred) {
			// Floored, so 99.96% never reads as 100% before the file is done.
			value := math.Min(100, math.Floor(1000*transferred/size)/10)
			if !finalizing {
				value = math.Min(value, 99.9)
			}
			percent = fmt.Sprintf("%.1f%%", value)
		}
		switch getString(row, "basis") {
		case "TARGET":
			target = true
		case "":
			older = older || reported
		}
		fmt.Fprintf(table, "#%s\t%s\t%s\t%s\t%s / %s\t%s\n", safeCLIText(getString(row, "childTaskId")), safeCLIText(getString(row, "executionNodeLabel")), safeCLIText(getString(row, "sourceKey")), percent, cliTaskSize(row, "transferredBytes"), cliTaskSize(row, "size"), cliFileStage(out, row, reported))
	}
	table.Flush()
	if target {
		fmt.Fprintln(out, cliText(out, "已传输：目标端已确认的字节，加上各请求本次尝试已交给网络的字节；请求失败时，该次尝试的字节会撤回。"))
	}
	if older {
		fmt.Fprintln(out, cliText(out, "旧版节点的进度按读取源端计算；显示“源端已读完（旧版节点）”时，写入目标端可能仍在进行。"))
	}
	if data["hasMore"] == true {
		fmt.Fprintln(out, cliText(out, "更多文件请使用 --child 查看指定子任务。"))
	}
	return true
}

// cliFileStage names a file's stage and what it waits for. FINALIZING from
// an Agent that counts at the destination means every byte is confirmed;
// from an older Agent (no basis, from a Master that reports one) it only
// means the source was read. Without a reported basis (an older Master), the
// source was read in full whichever Agent counted the file.
func cliFileStage(out io.Writer, row map[string]interface{}, basisReported bool) string {
	stage, basis := getString(row, "stage"), getString(row, "basis")
	label := safeCLIText(stage)
	switch {
	case stage == "TRANSFERRING":
		label = cliText(out, "传输中")
	case stage == "FINALIZING" && basis == "TARGET":
		label = cliText(out, "确认中")
	case stage == "FINALIZING" && (basis == "SOURCE" || !basisReported):
		label = cliText(out, "源端已读完")
	case stage == "FINALIZING":
		label = cliText(out, "源端已读完（旧版节点）")
	}
	phase := ""
	switch getString(row, "phase") {
	case "RETRY_WAIT":
		phase = cliText(out, "等待重试")
	case "WAITING_REQUEST_LIMIT":
		phase = cliText(out, "等待写入请求限速")
	case "WAITING_UPLOAD_BUFFER":
		phase = cliText(out, "等待上传缓冲")
	}
	if phase == "" {
		return label
	}
	return cliTextf(out, "%s（%s）", label, phase)
}
