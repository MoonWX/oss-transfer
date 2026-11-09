package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
)

// exceptionsListSummary renders the failed objects of exceptions list as a
// table: one line per object with a short reason. The complete records stay
// available with --json and exceptions export.
func exceptionsListSummary(client *ApiClient, kind string, o commandOptions, result map[string]interface{}) []string {
	var text strings.Builder
	out := &cliLocaleWriter{Writer: &text, locale: client.locale}
	items := objectList(result["items"])
	if kind == "fallback" && result["supported"] == false {
		if message := getString(result, "message"); message != "" {
			fmt.Fprintln(out, safeCLIText(message))
		} else {
			fmt.Fprintln(out, cliText(out, "当前传输引擎不使用降级记录"))
		}
		return cliTextLines(text.String())
	}
	if len(items) == 0 {
		fmt.Fprintln(out, cliText(out, "没有失败对象"))
	} else {
		renderExceptionRows(out, items)
		total := int(adminNumber(result["total"]))
		if total > len(items) {
			fmt.Fprint(out, cliTextf(out, "显示 %d 条，共 %d 条\n", len(items), total))
		} else {
			fmt.Fprint(out, cliTextf(out, "共 %d 条\n", len(items)))
		}
	}
	if kind == "local" && result["hasMore"] == true {
		offset, _ := adminInteger(o, "offset", 0, 0, 1<<31-1)
		limit, _ := adminInteger(o, "limit", 25, 1, 1000)
		fmt.Fprintf(out, cliText(out, "下一页：%s\n"), fmt.Sprintf("exceptions list local --offset %d --limit %d", offset+len(items), limit))
	} else if kind != "local" && result["truncated"] == true {
		fmt.Fprintln(out, cliText(out, "仅显示部分记录；可调大 --limit，或用 exceptions export 导出全部"))
	}
	if len(items) > 0 {
		fmt.Fprintln(out, cliText(out, "完整错误信息：加 --json，或用 exceptions export 导出 CSV"))
	}
	if warning := getString(result, "warning"); warning != "" {
		fmt.Fprintln(out, safeCLIText(warning))
	}
	return cliTextLines(text.String())
}

// renderExceptionRows prints failed objects: task, key, size, node, a short
// reason, the retry result and when the failure was recorded.
func renderExceptionRows(out io.Writer, items []map[string]interface{}) {
	table := newCLITable(out)
	fmt.Fprintln(table, cliText(out, "任务\tKey\t大小\t节点\t原因\t重试\t时间"))
	for _, row := range items {
		task := "#" + strconv.Itoa(getInt(row, "taskIndex"))
		if child := getString(row, "childTaskId"); child != "" {
			task = "#" + child
		}
		key := firstNodeString(row, "key", "logicalKey", "localPath")
		node := cliNodeLabel(row, firstNodeString(row, "workerNode", "executionNodeIp"), localeOf(out))
		retry := cliRetryStatusLabels["NOT_RETRIED"].in(out)
		if status := getString(row, "retryStatus"); status != "" {
			retry = status
			if label, ok := cliRetryStatusLabels[status]; ok {
				retry = label.in(out)
			}
		}
		if count := getInt(row, "retryCount"); count > 0 {
			retry += cliTextf(out, "（%d 次）", count)
		}
		observed, _ := cliTimeValue(row["observedAt"])
		if observed == "" {
			observed, _ = cliTimeValue(row["timestamp"])
		}
		if observed == "" {
			observed = "—"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", safeCLIText(task), safeCLIText(key), cliTaskSize(row, "objectSize"), safeCLIText(node), safeCLIText(cliErrorSummary(out, getString(row, "errorMessage"))), safeCLIText(retry), observed)
	}
	table.Flush()
}

// cliTextLines splits rendered text into summary lines.
func cliTextLines(text string) []string {
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}
