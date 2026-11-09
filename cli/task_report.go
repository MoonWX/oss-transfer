package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// taskReportResultKinds lists the per-object results of report-metadata.json
// in display order.
var taskReportResultKinds = []string{"SUCCESS", "SKIPPED", "FAILED", "UNPROCESSED", "UNKNOWN"}

// taskReportCompleteness reads report-metadata.json of a saved task report
// (one task or the local transfer report) and adds its completeness to
// result: complete, incompleteReasons (the Master's codes), resultCounts and
// unreachableNodes. A report without metadata (an older Master) is left as it
// is. An incomplete report makes the command exit 3.
func taskReportCompleteness(path string, result map[string]interface{}) error {
	meta, found, err := readTaskReportMetadata(path)
	if err != nil {
		return cliErrorf("报告已保存，但无法读取 report-metadata.json: %w", err)
	}
	if !found {
		return nil
	}
	complete, ok := meta["complete"].(bool)
	if !ok {
		return cliErrorf("报告已保存，但 report-metadata.json 缺少完整性信息")
	}
	tasks := objectList(meta["tasks"])
	if len(tasks) == 0 {
		tasks = []map[string]interface{}{meta}
	}
	counts := map[string]int64{}
	for _, kind := range taskReportResultKinds {
		counts[kind] = 0
	}
	reasons, seen := []string{}, map[string]bool{}
	for _, task := range tasks {
		if values, ok := task["resultCounts"].(map[string]interface{}); ok {
			for kind, value := range values {
				if n, ok := comparisonInteger(value); ok {
					counts[kind] += n
				}
			}
		}
		for _, reason := range cliStringList(task["partialReasons"]) {
			if !seen[reason] {
				seen[reason] = true
				reasons = append(reasons, reason)
			}
		}
	}
	if !complete && len(reasons) == 0 {
		reasons = append(reasons, "UNSPECIFIED")
	}
	nodes := []string{}
	displays, _ := meta["nodeDisplays"].(map[string]interface{})
	for _, address := range cliStringList(meta["unreachableNodes"]) {
		label := address
		if display, ok := displays[address].(map[string]interface{}); ok && getString(display, "displayLabel") != "" {
			label = getString(display, "displayLabel")
		}
		nodes = append(nodes, label)
	}
	result["reportKind"] = "task"
	result["complete"] = complete
	result["incompleteReasons"] = reasons
	result["resultCounts"] = counts
	result["unreachableNodes"] = nodes
	return nil
}

func readTaskReportMetadata(path string) (map[string]interface{}, bool, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, false, err
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if entry.Name != "report-metadata.json" {
			continue
		}
		reader, err := entry.Open()
		if err != nil {
			return nil, true, err
		}
		defer reader.Close()
		decoder := json.NewDecoder(io.LimitReader(reader, 64<<20))
		decoder.UseNumber()
		var meta map[string]interface{}
		if err := decoder.Decode(&meta); err != nil {
			return nil, true, err
		}
		return meta, true, nil
	}
	return nil, false, nil
}

// taskReportReason explains one incompleteness code of a task report.
func taskReportReason(out io.Writer, reason string, nodes []string) string {
	switch reason {
	case "SCAN_INCOMPLETE":
		return cliText(out, "源端对象尚未全部列举")
	case "TASK_STOPPED":
		return cliText(out, "任务已停止，还有对象没有迁移")
	case "UNPROCESSED_OBJECTS":
		return cliText(out, "有对象尚未处理")
	case "NODE_REPORT_INCOMPLETE":
		if len(nodes) > 0 {
			return cliTextf(out, "部分对象的结果无法从执行节点读取（%s）", strings.Join(nodes, ", "))
		}
		return cliText(out, "部分对象的结果无法从执行节点读取")
	case "STATE_CHANGED_DURING_EXPORT":
		return cliText(out, "导出期间任务发生了变化，请重新导出")
	case "COUNTS_DO_NOT_RECONCILE":
		return cliText(out, "报告中的对象数与任务统计不一致")
	case "TASK_ERROR":
		return cliText(out, "任务记录了错误，详见 exception-details.csv")
	case "LEGACY_LEDGER_INCOMPLETE":
		return cliText(out, "旧版本任务的对象记录不完整")
	case "LEGACY_RECOVERY_PENDING":
		return cliText(out, "旧版本任务尚未完成恢复核对")
	case "UNSPECIFIED":
		return cliText(out, "Master 未说明原因")
	}
	return safeCLIText(reason)
}

// renderTaskReportResult shows a saved task report and, when it is not
// complete, why, in words. The codes stay in the report files and in --json.
func renderTaskReportResult(out io.Writer, result map[string]interface{}) bool {
	if getString(result, "reportKind") != "task" {
		return false
	}
	counts, _ := result["resultCounts"].(map[string]int64)
	nodes := cliStringList(result["unreachableNodes"])
	complete := result["complete"] == true
	fields := [][2]string{
		{cliText(out, "保存位置"), getString(result, "output")},
		{cliText(out, "报告完整"), comparisonYesNo(out, complete)},
		{cliText(out, "对象结果"), cliTextf(out, "成功 %d，跳过 %d，失败 %d，未处理 %d，结果未知 %d", counts["SUCCESS"], counts["SKIPPED"], counts["FAILED"], counts["UNPROCESSED"], counts["UNKNOWN"])},
	}
	if reasons := cliStringList(result["incompleteReasons"]); !complete && len(reasons) > 0 {
		labels := make([]string, len(reasons))
		for i, reason := range reasons {
			labels[i] = taskReportReason(out, reason, nodes)
		}
		separator := "；"
		if localeOf(out) == localeEN {
			separator = "; "
		}
		fields = append(fields, [2]string{cliText(out, "不完整原因"), strings.Join(labels, separator)})
	}
	if message := getString(result, "message"); message != "" {
		fmt.Fprintln(out, safeCLIText(message))
	}
	table := newCLITable(out)
	for _, field := range fields {
		fmt.Fprintf(table, "%s\t%s\n", field[0], safeCLIText(field[1]))
	}
	table.Flush()
	if !complete && counts["UNKNOWN"] > 0 {
		fmt.Fprintln(out, cliText(out, "结果未知的对象未能确认迁移结果；exception-details.csv 列出了每个对象和原因。"))
	}
	return true
}
