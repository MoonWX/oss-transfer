package main

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

// A source call is reported as unanswered, and a scan without progress as
// idle, only past these times: shorter waits are ordinary storage latency.
const (
	cliScanUnansweredAfter = 5 * time.Second
	cliScanIdleAfter       = 30 * time.Second
)

// cliScanActivity is the Master's scanActivity of a running task with an
// incomplete scan, or nil. Older Masters send none.
func cliScanActivity(value interface{}) map[string]interface{} {
	activity, _ := value.(map[string]interface{})
	return activity
}

func cliScanCount(activity map[string]interface{}, key string) int64 {
	number, ok := cliFinite(activity[key])
	if !ok || number > math.MaxInt64/2 {
		return 0
	}
	return int64(number)
}

func cliScanMillis(activity map[string]interface{}, key string) time.Duration {
	return time.Duration(cliScanCount(activity, key)) * time.Millisecond
}

// cliElapsed writes a duration as the console does: "3 min 5 s".
func cliElapsed(out io.Writer, d time.Duration) string {
	total := int64(max(d, 0) / time.Second)
	days, hours, minutes, seconds := total/86400, total%86400/3600, total%3600/60, total%60
	switch {
	case days > 0:
		return cliTextf(out, "%d 天 %d 小时", days, hours)
	case hours > 0:
		return cliTextf(out, "%d 小时 %d 分", hours, minutes)
	case minutes > 0:
		return cliTextf(out, "%d 分 %d 秒", minutes, seconds)
	}
	return cliTextf(out, "%d 秒", seconds)
}

// cliScanListLabel replaces the task list's "—" progress while a directory is
// read, so that the row moves before the first object is found. It is empty
// when the Master reports no scan activity.
func cliScanListLabel(out io.Writer, task map[string]interface{}) string {
	activity := cliScanActivity(task["scanActivity"])
	switch {
	case activity == nil:
		return ""
	case getString(activity, "state") == "UNSUPPORTED":
		return cliText(out, "扫描中")
	case getString(activity, "state") == "STALE":
		return cliTextf(out, "扫描中 · 已读取 %d · 已检查 %d · %s前", cliScanCount(activity, "namesRead"), cliScanCount(activity, "examined"),
			cliElapsed(out, cliScanMillis(activity, "ageMillis")))
	}
	return cliTextf(out, "扫描中 · 已读取 %d · 已检查 %d", cliScanCount(activity, "namesRead"), cliScanCount(activity, "examined"))
}

func cliScanDirectory(out io.Writer, key string) string {
	if key == "" {
		return cliText(out, "源目录")
	}
	return cliTextf(out, "目录“%s”", key)
}

func cliScanMean(milliseconds float64) string {
	digits := 0
	switch {
	case milliseconds < 1:
		digits = 2
	case milliseconds < 10:
		digits = 1
	}
	return strconv.FormatFloat(milliseconds, 'f', digits, 64)
}

// cliScanActivityLines are the lines of the scan progress block, in the
// console's order and with its wording.
func cliScanActivityLines(out io.Writer, activity map[string]interface{}) []string {
	if activity == nil {
		return nil
	}
	if getString(activity, "state") == "UNSUPPORTED" {
		return []string{cliText(out, "扫描节点版本较旧，不提供实时扫描进度")}
	}
	lines := []string{cliTextf(out, "本轮扫描已读取 %d 个名称，已检查 %d 项（扫描重启后从零计）", cliScanCount(activity, "namesRead"), cliScanCount(activity, "examined"))}
	if directory, ok := activity["directory"].(map[string]interface{}); ok {
		dir := cliScanDirectory(out, getString(directory, "key"))
		names, position := cliScanCount(directory, "names"), cliScanCount(directory, "position")
		elapsed := cliElapsed(out, cliScanMillis(directory, "elapsedMillis"))
		switch reading := directory["reading"] == true; {
		case reading && getString(activity, "mode") == "STREAMING":
			lines = append(lines, cliTextf(out, "正在读取并检查%s：已读取 %d 个名称，已检查 %d 项，用时 %s", dir, names, cliScanCount(directory, "examined"), elapsed))
		case reading:
			lines = append(lines, cliTextf(out, "正在读取%s中的名称：已读取 %d 个，用时 %s。名称不超过 4,096 个的目录读完后才开始检查和迁移其中的文件，超过后改为边读边检查。", dir, names, elapsed))
		case names > 0:
			lines = append(lines, cliTextf(out, "正在检查%s：第 %d 项，共 %d 项", dir, position, names))
		}
	}
	if found := cliScanCount(activity, "alreadyFound"); found > 0 {
		lines = append(lines, cliTextf(out, "其中 %d 项此前已发现，未再次检查", found))
	}
	if vanished := cliScanCount(activity, "vanished"); vanished > 0 {
		lines = append(lines, cliTextf(out, "%d 项在检查前已被删除，已跳过", vanished))
	}
	if mean, ok := cliFinite(activity["lookupMeanMillis"]); ok {
		lines = append(lines, cliTextf(out, "条目查询平均耗时 %s 毫秒", cliScanMean(mean)))
	}
	waiting := getString(activity, "waitingFor")
	unanswered := cliScanMillis(activity, "oldestCallMillis") >= cliScanUnansweredAfter
	if unanswered {
		lines = append(lines, cliTextf(out, "源存储已有 %s 未响应", cliElapsed(out, cliScanMillis(activity, "oldestCallMillis"))))
	}
	switch waiting {
	case "LOCAL_OPERATION_LIMIT":
		lines = append(lines, cliText(out, "正在等待本节点的本地文件操作限制"))
	case "COMMIT":
		lines = append(lines, cliText(out, "正在保存扫描结果"))
	}
	if idle := cliScanMillis(activity, "idleMillis"); !unanswered && waiting == "" && idle >= cliScanIdleAfter {
		lines = append(lines, cliTextf(out, "已有 %s 没有新进展", cliElapsed(out, idle)))
	}
	if getString(activity, "state") == "STALE" {
		if observed := cliTaskTimestamp(activity["observedAt"]); observed != "" {
			lines = append(lines, cliTextf(out, "扫描进度截至 %s", observed))
		}
	}
	return lines
}

// renderScanActivity prints the scan progress block, or nothing when the
// Master reports no scan activity.
func renderScanActivity(out io.Writer, activity map[string]interface{}) {
	lines := cliScanActivityLines(out, activity)
	if len(lines) == 0 {
		return
	}
	fmt.Fprintln(out, cliText(out, "扫描进度"))
	for _, line := range lines {
		fmt.Fprintln(out, "  "+safeCLIText(line))
	}
}
