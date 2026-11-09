package main

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Labels for comparison and row states. Values are catalog keys.
var comparisonStatusLabels = map[string]string{
	"RUNNING":                "正在比对",
	"CANCELING":              "正在取消",
	"CANCELED":               "已取消",
	"INTERRUPTED":            "已中断",
	"FAILED":                 "失败",
	"COMPLETED":              "已完成",
	"UNAVAILABLE":            "节点不可用",
	"MATCH":                  "一致",
	"DIFFERENT":              "不一致",
	"ETAG_MISMATCH":          "ETag 不同",
	"MISSING_TARGET":         "目标缺失",
	"EXTRA_TARGET":           "目标多余",
	"CHANGED_DURING_COMPARE": "比对期间已变化",
	"ERROR":                  "读取失败",
	"BOTH_MISSING":           "两端均不存在",
}

// Labels for the properties named by a row's reasons. Values are catalog keys
// or property names that stay unchanged.
var comparisonReasonLabels = map[string]string{
	"size":               "大小",
	"contentType":        "Content-Type",
	"contentEncoding":    "Content-Encoding",
	"contentDisposition": "Content-Disposition",
	"cacheControl":       "Cache-Control",
	"metadata":           "自定义元数据",
	"sha256":             "SHA-256",
	"etag":               "ETag",
}

// renderComparisonResult prints the results of object subcommands. It returns
// false for results it does not render, which then use the generic output.
func renderComparisonResult(out io.Writer, args []string, result map[string]interface{}, callErr error) bool {
	if len(args) < 2 || args[0] != "object" || result == nil {
		return false
	}
	data := responseData(result)
	action, rest := args[1], args[2:]
	if callErr != nil {
		// The Master keeps a start with an unknown outcome under its ID.
		if id := getString(data, "id"); action == "compare" && id != "" {
			fmt.Fprintf(out, cliText(out, "启动结果未确认，比对 ID：%s；稍后用 %s 查看\n"), safeCLIText(id), safeCLIText(comparisonCommand("status", id)))
			return true
		}
		return false
	}
	switch action {
	case "list":
		renderComparisonList(out, data)
	case "status":
		options, err := parseComparisonPageOptions(rest)
		if err != nil {
			return false
		}
		renderComparisonStatus(out, options, data)
	case "export":
		renderComparisonExport(out, data)
	case "compare":
		id := getString(data, "id")
		if id == "" || getString(data, "status") == "" {
			return false // the synchronous single-object metadata check
		}
		fmt.Fprintf(out, cliText(out, "比对已开始，ID：%s\n"), safeCLIText(id))
		fmt.Fprintf(out, cliText(out, "查看进度：%s\n"), safeCLIText(comparisonCommand("status", id, "--watch", "5")))
	case "resume", "cancel", "delete":
		id, err := comparisonIDArgument(rest)
		if err != nil {
			return false
		}
		switch action {
		case "resume":
			fmt.Fprintf(out, cliText(out, "已继续比对 %s\n"), safeCLIText(id))
			fmt.Fprintf(out, cliText(out, "查看进度：%s\n"), safeCLIText(comparisonCommand("status", id, "--watch", "5")))
		case "cancel":
			if status := getString(data, "status"); status != "CANCELING" {
				fmt.Fprintf(out, cliText(out, "比对 %s 当前状态：%s\n"), safeCLIText(id), comparisonStatusLabel(out, status))
			} else {
				fmt.Fprintf(out, cliText(out, "已请求取消比对 %s\n"), safeCLIText(id))
			}
		default:
			fmt.Fprintf(out, cliText(out, "已删除比对 %s 的结果\n"), safeCLIText(id))
		}
	default:
		return false
	}
	return true
}

func comparisonStatusLabel(out io.Writer, status string) string {
	if label, ok := comparisonStatusLabels[status]; ok {
		return cliText(out, label)
	}
	if status == "" {
		return "—"
	}
	return safeCLIText(status)
}

func comparisonListSeparator(out io.Writer) string {
	if localeOf(out) == localeEN {
		return ", "
	}
	return "、"
}

func comparisonStatusList(out io.Writer, statuses []string) string {
	labels := make([]string, len(statuses))
	for i, status := range statuses {
		labels[i] = comparisonStatusLabel(out, status)
	}
	return strings.Join(labels, comparisonListSeparator(out))
}

func comparisonModeLabel(out io.Writer, mode string) string {
	switch mode {
	case "metadata":
		return cliText(out, "元数据")
	case "content":
		return cliText(out, "内容 SHA-256")
	}
	return safeCLIText(mode)
}

func comparisonScopeLabel(out io.Writer, scope string) string {
	switch scope {
	case "prefix":
		return cliText(out, "存储桶/前缀")
	case "object":
		return cliText(out, "单个对象")
	}
	return safeCLIText(scope)
}

// comparisonLocation shows connection/bucket/prefix, or the key of a
// single-object comparison.
func comparisonLocation(data map[string]interface{}, endpointKey, prefixKey string) string {
	endpoint, _ := data[endpointKey].(map[string]interface{})
	location := ""
	for _, part := range []string{getString(endpoint, "ref"), getString(endpoint, "bucket")} {
		if part != "" {
			location += part + "/"
		}
	}
	return location + getString(data, prefixKey)
}

func comparisonTruncationText(out io.Writer, reason string) string {
	switch reason {
	case "LEGACY_LIMIT":
		return cliText(out, "该节点版本最多保存 1 万条差异")
	case "STORAGE_BUDGET":
		return cliText(out, "超过节点的比对结果存储预算")
	case "LOW_DISK_SPACE":
		return cliText(out, "节点磁盘剩余空间不足")
	case "":
		return cliText(out, "差异明细未全部保存")
	}
	return safeCLIText(reason)
}

func comparisonIncompleteReason(out io.Writer, reason string) string {
	kind, value, _ := strings.Cut(reason, ":")
	switch kind {
	case "NOT_COMPLETED":
		return cliTextf(out, "比对未完成（%s）", comparisonStatusLabel(out, value))
	case "ROWS_TRUNCATED":
		return cliTextf(out, "差异明细未全部保存（%s）", comparisonTruncationText(out, value))
	case "ROW_COUNT_MISMATCH":
		return cliText(out, "差异数与统计不一致")
	}
	return safeCLIText(reason)
}

func comparisonYesNo(out io.Writer, value bool) string {
	if value {
		return cliText(out, "是")
	}
	return cliText(out, "否")
}

// comparisonCount formats a non-negative integer count, "—" when unknown.
func comparisonCount(value interface{}) string {
	return cliTaskCount(map[string]interface{}{"value": value}, "value")
}

func comparisonElapsed(data map[string]interface{}) string {
	millis, ok := cliNumeric(data["elapsedMillis"])
	if !ok || math.IsNaN(millis) || millis < 0 || millis > float64(math.MaxInt64/int64(time.Millisecond)) {
		return "—"
	}
	elapsed := time.Duration(millis) * time.Millisecond
	if elapsed < time.Minute {
		return elapsed.Round(100 * time.Millisecond).String()
	}
	return elapsed.Round(time.Second).String()
}

// comparisonRate is compared objects per second of comparison time.
func comparisonRate(out io.Writer, data map[string]interface{}) string {
	compared, comparedOK := cliNumeric(data["compared"])
	millis, millisOK := cliNumeric(data["elapsedMillis"])
	if !comparedOK || !millisOK || compared < 0 || !(millis > 0) || math.IsInf(millis, 0) || math.IsInf(compared, 0) {
		return "—"
	}
	return cliTextf(out, "%s 个对象/秒", strconv.FormatFloat(compared*1000/millis, 'f', 1, 64))
}

func comparisonTime(value interface{}) string {
	text, _ := value.(string)
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return text
	}
	if parsed.IsZero() {
		return ""
	}
	return parsed.Local().Format("2006-01-02 15:04:05")
}

var comparisonPlainID = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// comparisonCommand formats a follow-up command for printed hints. An ID that
// starts with "-" follows "--" so that it is not read as an option.
func comparisonCommand(action, id string, flags ...string) string {
	quoted := id
	if !comparisonPlainID.MatchString(id) {
		quoted = quoteTaskCursor(id)
	}
	parts := []string{"object", action}
	if strings.HasPrefix(id, "-") {
		parts = append(append(parts, flags...), "--", quoted)
	} else {
		parts = append(append(parts, quoted), flags...)
	}
	return strings.Join(parts, " ")
}

func renderComparisonList(out io.Writer, data map[string]interface{}) {
	items := objectList(data["items"])
	if len(items) == 0 {
		fmt.Fprintln(out, cliText(out, "暂无比对记录"))
	} else {
		table := newCLITable(out)
		fmt.Fprintln(table, cliText(out, "ID\t状态\t方式\t范围\t源\t目标\t已检查\t差异\t开始时间\t节点"))
		for _, item := range items {
			compared, differences := "—", "—"
			// Unavailable entries come from the Master's record, without counts.
			if getString(item, "status") != "UNAVAILABLE" {
				compared = cliTaskCount(item, "compared")
				if matched := cliTaskCount(item, "matched"); compared != "—" && matched != "—" && getInt64(item, "compared") >= getInt64(item, "matched") {
					differences = strconv.FormatInt(getInt64(item, "compared")-getInt64(item, "matched"), 10)
				}
			}
			started := comparisonTime(item["startedAt"])
			if started == "" {
				started = "—"
			}
			node := firstNodeString(item, "nodeName", "nodeId")
			if node == "" {
				node = "—"
			}
			fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", safeCLIText(getString(item, "id")), comparisonStatusLabel(out, getString(item, "status")), comparisonModeLabel(out, getString(item, "mode")), comparisonScopeLabel(out, getString(item, "scope")), safeCLIText(comparisonLocation(item, "source", "sourcePrefix")), safeCLIText(comparisonLocation(item, "destination", "destinationPrefix")), compared, differences, safeCLIText(started), safeCLIText(node))
		}
		table.Flush()
		fmt.Fprint(out, cliTextf(out, "共 %d 个比对\n", len(items)))
	}
	for _, node := range objectList(data["unavailableNodes"]) {
		name := firstNodeString(node, "nodeName", "nodeId")
		fmt.Fprint(out, cliTextf(out, "节点 %s 暂时无法连接，%d 个比对的最新状态未知\n", safeCLIText(name), getInt(node, "count")))
	}
}

func renderComparisonStatus(out io.Writer, options comparisonPageOptions, data map[string]interface{}) {
	id := getString(data, "id")
	if id == "" {
		id = options.id
	}
	fields := [][2]string{
		{"ID", id},
		{cliText(out, "状态"), comparisonStatusLabel(out, getString(data, "status"))},
		{cliText(out, "方式"), comparisonModeLabel(out, getString(data, "mode"))},
		{cliText(out, "范围"), comparisonScopeLabel(out, getString(data, "scope"))},
		{cliText(out, "源"), comparisonLocation(data, "source", "sourcePrefix")},
		{cliText(out, "目标"), comparisonLocation(data, "destination", "destinationPrefix")},
		{cliText(out, "已检查"), cliTaskCount(data, "compared")},
		{cliText(out, "一致"), cliTaskCount(data, "matched")},
		{cliText(out, "不一致"), cliTaskCount(data, "different")},
		{cliText(out, "ETag 不同"), cliTaskCount(data, "etagMismatch")},
		{cliText(out, "目标缺失"), cliTaskCount(data, "missing")},
		{cliText(out, "目标多余"), cliTaskCount(data, "extra")},
		{cliText(out, "比对期间已变化"), cliTaskCount(data, "changed")},
		{cliText(out, "读取失败"), cliTaskCount(data, "errors")},
		{cliText(out, "两端均不存在"), cliTaskCount(data, "bothMissing")},
		{cliText(out, "已保存差异"), cliTaskCount(data, "rows")},
		{cliText(out, "已用时间"), comparisonElapsed(data)},
		{cliText(out, "速率"), comparisonRate(out, data)},
	}
	if getString(data, "mode") == "content" {
		fields = append(fields, [2]string{cliText(out, "已读取"), cliTaskSize(data, "bytesRead")})
	}
	if checkpoint := getString(data, "checkpoint"); checkpoint != "" {
		fields = append(fields, [2]string{cliText(out, "检查点"), checkpoint})
	}
	fields = append(fields, [2]string{cliText(out, "可继续"), comparisonYesNo(out, data["resumable"] == true)})
	if data["rowsTruncated"] == true {
		fields = append(fields, [2]string{cliText(out, "截断原因"), comparisonTruncationText(out, comparisonTruncatedReason(data))})
		if key := getString(data, "lastStoredKey"); key != "" {
			fields = append(fields, [2]string{cliText(out, "最后保存的 Key"), key})
		}
	}
	for _, field := range [][2]string{{cliText(out, "开始时间"), comparisonTime(data["startedAt"])}, {cliText(out, "结束时间"), comparisonTime(data["endedAt"])}, {cliText(out, "保留至"), comparisonTime(data["expiresAt"])}, {cliText(out, "错误"), getString(data, "error")}, {cliText(out, "错误详情"), getString(data, "errorDetail")}} {
		if field[1] != "" {
			fields = append(fields, field)
		}
	}
	table := newCLITable(out)
	for _, field := range fields {
		fmt.Fprintf(table, "%s\t%s\n", field[0], safeCLIText(field[1]))
	}
	table.Flush()
	fmt.Fprintln(out)
	renderComparisonRows(out, objectList(data["items"]))
	if next := getInt64(data, "nextAfter"); next > 0 {
		flags := []string{"--after", strconv.FormatInt(next, 10), "--limit", strconv.Itoa(options.limit)}
		if len(options.statuses) > 0 {
			flags = append(flags, "--status", strings.Join(options.statuses, ","))
		}
		fmt.Fprintf(out, cliText(out, "下一页：%s\n"), safeCLIText(comparisonCommand("status", id, flags...)))
	}
	if data["resumable"] == true {
		fmt.Fprintf(out, cliText(out, "可用 %s 从检查点之后继续\n"), safeCLIText(comparisonCommand("resume", id)))
	}
}

func renderComparisonRows(out io.Writer, rows []map[string]interface{}) {
	if len(rows) == 0 {
		fmt.Fprintln(out, cliText(out, "本页没有差异记录"))
		return
	}
	table := newCLITable(out)
	fmt.Fprintln(table, cliText(out, "编号\t结果\t源 Key\t目标 Key\t源大小\t目标大小\t说明"))
	for _, row := range rows {
		status := getString(row, "status")
		sourceSize, destinationSize := cliTaskSize(row, "sourceSize"), cliTaskSize(row, "destinationSize")
		if status == "EXTRA_TARGET" || status == "BOTH_MISSING" {
			sourceSize = "—"
		}
		if status == "MISSING_TARGET" || status == "BOTH_MISSING" {
			destinationSize = "—"
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", cliTaskCount(row, "number"), comparisonStatusLabel(out, status), safeCLIText(getString(row, "sourceKey")), safeCLIText(getString(row, "destinationKey")), sourceSize, destinationSize, safeCLIText(comparisonRowDetail(out, row)))
	}
	table.Flush()
}

// comparisonRowDetail names the differing properties, or the read error.
func comparisonRowDetail(out io.Writer, row map[string]interface{}) string {
	if reasons := cliStringList(row["reasons"]); len(reasons) > 0 {
		labels := make([]string, len(reasons))
		for i, reason := range reasons {
			labels[i] = reason
			if label, ok := comparisonReasonLabels[reason]; ok {
				labels[i] = cliText(out, label)
			}
		}
		return cliTextf(out, "不同：%s", strings.Join(labels, comparisonListSeparator(out)))
	}
	message, detail := getString(row, "error"), getString(row, "errorDetail")
	if message != "" && detail != "" {
		return cliTextf(out, "%s（%s）", message, detail)
	}
	return message + detail
}

func renderComparisonExport(out io.Writer, data map[string]interface{}) {
	summary, _ := data["summary"].(map[string]interface{})
	status, stored := getString(data, "status"), data["storedRows"]
	if summary != nil {
		status, stored = getString(summary, "status"), summary["storedRows"]
	}
	fields := [][2]string{{cliText(out, "保存位置"), getString(data, "output")}, {cliText(out, "格式"), strings.ToUpper(getString(data, "format"))}}
	if statuses := cliStringList(data["statusFilter"]); len(statuses) > 0 {
		fields = append(fields, [2]string{cliText(out, "筛选"), comparisonStatusList(out, statuses)})
	}
	fields = append(fields, [2]string{cliText(out, "状态"), comparisonStatusLabel(out, status)})
	counts, _ := summary["counts"].(map[string]interface{})
	for _, count := range [][2]string{{cliText(out, "已检查"), "compared"}, {cliText(out, "一致"), "matched"}, {cliText(out, "不一致"), "different"}, {cliText(out, "ETag 不同"), "etagMismatch"}, {cliText(out, "目标缺失"), "missing"}, {cliText(out, "目标多余"), "extra"}, {cliText(out, "比对期间已变化"), "changed"}, {cliText(out, "读取失败"), "errors"}, {cliText(out, "两端均不存在"), "bothMissing"}} {
		if value, ok := counts[count[1]]; ok {
			fields = append(fields, [2]string{count[0], comparisonCount(value)})
		}
	}
	fields = append(fields, [2]string{cliText(out, "已保存差异"), comparisonCount(stored)}, [2]string{cliText(out, "已导出差异"), comparisonCount(data["exportedRows"])})
	if summary != nil {
		fields = append(fields, [2]string{cliText(out, "重新迁移的源 Key"), comparisonCount(summary["retryKeys"])})
		if reason := getString(summary, "truncatedReason"); reason != "" {
			fields = append(fields, [2]string{cliText(out, "截断原因"), comparisonTruncationText(out, reason)})
			if key := getString(summary, "lastStoredKey"); key != "" {
				fields = append(fields, [2]string{cliText(out, "最后保存的 Key"), key})
			}
		}
	}
	fields = append(fields, [2]string{cliText(out, "完整"), comparisonYesNo(out, data["complete"] == true)})
	if reasons := cliStringList(data["incompleteReasons"]); len(reasons) > 0 {
		labels := make([]string, len(reasons))
		for i, reason := range reasons {
			labels[i] = comparisonIncompleteReason(out, reason)
		}
		separator := "；"
		if localeOf(out) == localeEN {
			separator = "; "
		}
		fields = append(fields, [2]string{cliText(out, "不完整原因"), strings.Join(labels, separator)})
	}
	table := newCLITable(out)
	for _, field := range fields {
		fmt.Fprintf(table, "%s\t%s\n", field[0], safeCLIText(field[1]))
	}
	table.Flush()
	if summary == nil {
		return
	}
	if omitted, ok := comparisonInteger(summary["retryKeysOmitted"]); ok && omitted > 0 {
		fmt.Fprint(out, cliTextf(out, "%d 个源 Key 无法写入 retry-source-keys.incr（过长、含换行或不是有效 UTF-8），请查看 differences.csv\n", omitted))
	}
	if retry, _ := comparisonInteger(summary["retryKeys"]); retry > 0 && summary["retryPrefixesMatch"] == false {
		fmt.Fprintln(out, cliText(out, "源前缀与目标前缀不同：import-inventory 按原 Key 复制，不能直接使用 retry-source-keys.incr"))
	}
}

// comparisonRowsTruncated reports a page of comparison results whose stored
// rows were truncated: an incomplete answer. A truncated comparison in a list
// or in the reply to an action (start, resume, cancel) is that comparison's
// state, not a partial reply.
func comparisonRowsTruncated(data map[string]interface{}) bool {
	_, page := data["items"]
	return page && data["rowsTruncated"] == true
}

// comparisonIncompleteNotice explains truncated rows in the incomplete-result
// notice.
func comparisonIncompleteNotice(out io.Writer, data map[string]interface{}) [][2]string {
	if !comparisonRowsTruncated(data) {
		return nil
	}
	notice := [][2]string{{cliText(out, "差异明细"), comparisonTruncationText(out, comparisonTruncatedReason(data))}}
	if key := getString(data, "lastStoredKey"); key != "" {
		notice = append(notice, [2]string{cliText(out, "最后保存的 Key"), key})
	}
	return notice
}
