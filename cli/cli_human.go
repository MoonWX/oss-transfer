package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Human output helpers. Machine output (--json) keeps the original values;
// these functions only change what a person reads in a terminal.

// cliSummaryKey holds the lines a write command prints instead of its raw
// reply. It never reaches --json output.
const cliSummaryKey = "_cliSummary"

// setCLISummary records the human summary of a command's result.
func setCLISummary(result map[string]interface{}, lines ...string) {
	if result != nil && len(lines) > 0 {
		result[cliSummaryKey] = lines
	}
}

// quietCLIResult marks a result whose error line says everything: human
// output prints nothing else.
func quietCLIResult(result map[string]interface{}) {
	if result != nil {
		result[cliSummaryKey] = []string{}
	}
}

// withoutHumanFields returns result without the human-only entries.
func withoutHumanFields(result map[string]interface{}) map[string]interface{} {
	if _, ok := result[cliSummaryKey]; !ok {
		return result
	}
	copy := make(map[string]interface{}, len(result))
	for key, value := range result {
		if key != cliSummaryKey {
			copy[key] = value
		}
	}
	return copy
}

// renderCLISummary prints a recorded summary and reports whether there was one.
func renderCLISummary(out io.Writer, result map[string]interface{}) bool {
	lines, ok := result[cliSummaryKey].([]string)
	if !ok {
		return false
	}
	for _, line := range lines {
		fmt.Fprintln(out, safeCLIText(line))
	}
	return true
}

// cliNumberText prints a number without scientific notation: integers in
// full, other values with the digits they need.
func cliNumberText(number float64) string {
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	if number == math.Trunc(number) && math.Abs(number) < 1<<53 {
		return strconv.FormatInt(int64(number), 10)
	}
	return strconv.FormatFloat(number, 'f', -1, 64)
}

// cliTimeLayout shows local time with its UTC offset, so that a time read in
// a terminal matches the console's local time unambiguously.
const cliTimeLayout = "2006-01-02 15:04:05 -07:00"

func cliLocalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format(cliTimeLayout)
}

// cliTimeKey reports a field holding a time: Unix milliseconds or RFC 3339.
func cliTimeKey(key string) bool {
	switch key {
	case "timestamp", "lastSeen", "createTime", "startTime", "endTime", "generatedAt":
		return true
	}
	return strings.HasSuffix(key, "At") || strings.HasSuffix(key, "Time")
}

// cliTimeValue formats a time field. ok is false for a value that is not a
// time; an unset (zero) time returns "" with ok true.
func cliTimeValue(value interface{}) (string, bool) {
	switch v := value.(type) {
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			return "", false
		}
		return cliLocalTime(parsed), true
	default:
		number, ok := cliNumeric(value)
		// Milliseconds since 1970: 1e11 ms is 1973, anything smaller is not
		// a timestamp of this software.
		if !ok || math.IsNaN(number) || number < 1e11 || number > 1e15 {
			if ok && number == 0 {
				return "", true
			}
			return "", false
		}
		return cliLocalTime(time.UnixMilli(int64(number))), true
	}
}

// cliScalarText formats one value of the generic field renderer. show is
// false for a value that carries no information (an unset time).
func cliScalarText(out io.Writer, key string, value interface{}) (text string, show bool) {
	if cliTimeKey(key) {
		if formatted, ok := cliTimeValue(value); ok {
			return formatted, formatted != ""
		}
	}
	switch v := value.(type) {
	case bool:
		return comparisonYesNo(out, v), true
	case float64, float32, int, int64, json.Number:
		number, _ := cliNumeric(v)
		return cliNumberText(number), true
	case string:
		if label := cliEnumLabel(out, key, v); label != "" {
			return label, true
		}
		return v, true
	}
	return fmt.Sprint(value), true
}

// cliEnumLabel translates a known enum value of a known field; "" otherwise.
func cliEnumLabel(out io.Writer, key, value string) string {
	switch key {
	case "status":
		if label, ok := cliTaskStatusLabels[value]; ok {
			return label.in(out)
		}
	case "direction":
		return cliDirectionLabel(out, value)
	case "conflictPolicy":
		return cliConflictLabel(out, value)
	case "distributionMode":
		return cliDistributionLabel(out, value, "")
	case "retryStatus":
		if label, ok := cliRetryStatusLabels[value]; ok {
			return label.in(out)
		}
	case "phase":
		return cliLabelFor(out, value, cliUpdatePhaseLabels)
	case "role":
		return cliLabelFor(out, value, map[string]cliLabel{"admin": {"管理员", "Administrator", "管理者"}})
	}
	return ""
}

// cliLabel is an enum label in Chinese, English and Japanese. Enum labels
// use their own tables: the same Chinese word can need a different English
// word elsewhere (a node state "执行中" is "Active", a task state "Running").
type cliLabel [3]string

func (label cliLabel) in(out io.Writer) string {
	switch localeOf(out) {
	case localeEN:
		return label[1]
	case localeJA:
		return label[2]
	}
	return label[0]
}

// Task states as the console shows them.
var cliTaskStatusLabels = map[string]cliLabel{
	"PENDING":        {"排队中", "Queued", "実行待ち"},
	"WAITING":        {"排队中", "Queued", "実行待ち"},
	"RUNNING":        {"执行中", "Running", "実行中"},
	"LIST_DONE":      {"执行中", "Running", "実行中"},
	"STOPPING":       {"停止中", "Stopping", "停止処理中"},
	"RESUMING":       {"恢复中", "Resuming", "再開中"},
	"RECOVERABLE":    {"等待恢复", "Awaiting resume", "再開待ち"},
	"COMPLETED":      {"已完成", "Completed", "完了"},
	"PARTIAL_FAILED": {"部分失败", "Partially failed", "一部失敗"},
	"FAILED":         {"失败", "Failed", "失敗"},
	"STOPPED":        {"已停止", "Stopped", "停止済み"},
	"DELETED":        {"已删除", "Deleted", "削除済み"},
}

// cliTaskStatusLabel shows a task state; unknown states stay as they are.
func cliTaskStatusLabel(out io.Writer, status string) string {
	if label, ok := cliTaskStatusLabels[status]; ok {
		return label.in(out)
	}
	return status
}

func cliDirectionLabel(out io.Writer, direction string) string {
	return cliLabelFor(out, direction, map[string]cliLabel{
		"OSS_TO_OSS":   {"OSS → OSS", "OSS → OSS", "OSS → OSS"},
		"OSS_TO_LOCAL": {"OSS → 本地", "OSS → Local", "OSS → ローカル"},
		"LOCAL_TO_OSS": {"本地 → OSS", "Local → OSS", "ローカル → OSS"},
	})
}

func cliConflictLabel(out io.Writer, policy string) string {
	return cliLabelFor(out, policy, map[string]cliLabel{
		"SKIP":              {"跳过同名对象或文件", "Skip existing objects or files", "既存のオブジェクトまたはファイルをスキップ"},
		"OVERWRITE":         {"覆盖同名对象或文件", "Overwrite existing objects or files", "既存のオブジェクトまたはファイルを上書き"},
		"OVERWRITE_CHANGED": {"覆盖有变化的对象", "Overwrite changed objects", "変更されたオブジェクトを上書き"},
	})
}

func cliDistributionLabel(out io.Writer, mode, direction string) string {
	mode = strings.ToLower(mode)
	if mode == "affinity" && direction == "LOCAL_TO_OSS" {
		mode = "single"
	}
	return cliLabelFor(out, mode, map[string]cliLabel{
		"balanced": {"均衡分发", "Balanced", "均等配分"},
		"affinity": {"亲和分发", "Affinity", "アフィニティ配分"},
		"single":   {"单节点执行", "Single node", "単一ノード実行"},
	})
}

func cliKeyMappingLabel(out io.Writer, mode string) string {
	if label := cliLabelFor(out, mode, map[string]cliLabel{
		"PRESERVE":       {"保留原 Key", "Keep original key", "元のキーを維持"},
		"ADD_PREFIX":     {"添加目标前缀", "Add destination prefix", "転送先プレフィックスを追加"},
		"REPLACE_PREFIX": {"替换源前缀", "Replace source prefix", "転送元プレフィックスを置換"},
		"EXACT_KEY":      {"指定目标 Key", "Set destination key", "転送先キーを指定"},
	}); label != "" {
		return label
	}
	return mode
}

// cliLabelFor returns the label of value, or "" for an unknown value.
func cliLabelFor(out io.Writer, value string, labels map[string]cliLabel) string {
	if label, ok := labels[value]; ok {
		return label.in(out)
	}
	return ""
}

// Retry results of a failed object, as the console shows them.
var cliRetryStatusLabels = map[string]cliLabel{
	"NOT_RETRIED": {"未重试", "Not retried", "未再試行"},
	"PENDING":     {"重试中", "Retrying", "再試行中"},
	"INTERRUPTED": {"重试未完成", "Retry incomplete", "再試行未完了"},
	"RECOVERED":   {"已恢复", "Recovered", "復旧済み"},
	"FAILED":      {"再次失败", "Failed again", "再失敗"},
	"SKIPPED":     {"已跳过", "Skipped", "スキップ済み"},
}

// Software update phases and reasons.
var cliUpdatePhaseLabels = map[string]cliLabel{
	"IDLE":        {"无更新", "Idle", "更新なし"},
	"QUEUED":      {"排队中", "Queued", "待機中"},
	"RUNNING":     {"进行中", "In progress", "進行中"},
	"UPDATING":    {"更新中", "Updating", "更新中"},
	"DOWNLOADING": {"下载中", "Downloading", "ダウンロード中"},
	"APPLYING":    {"安装中", "Installing", "インストール中"},
	"COMPLETED":   {"已完成", "Completed", "完了"},
	"FAILED":      {"失败", "Failed", "失敗"},
	"SKIPPED":     {"已跳过", "Skipped", "スキップ済み"},
	"UNKNOWN":     {"未知", "Unknown", "不明"},
}

var cliUpdateReasonLabels = map[string]cliLabel{
	"OFFLINE":            {"节点离线", "Node offline", "ノードがオフライン"},
	"LEGACY":             {"需先用安装脚本升级一次", "Upgrade once with the installer first", "先にインストーラーで一度アップグレードしてください"},
	"UNMANAGED":          {"此安装不支持在线更新", "Online updates are unavailable for this installation", "このインストールではオンライン更新を利用できません"},
	"OTHER_INSTALLATION": {"属于其他安装", "Belongs to another installation", "別のインストールに属しています"},
	"BUSY":               {"节点正忙，请稍后再试", "Node busy; try again later", "ノードが処理中です。後で再試行してください"},
	"ERROR":              {"读取失败", "Cannot read", "読み取りに失敗しました"},
	"UP_TO_DATE":         {"已是目标版本", "Already up to date", "すでに対象バージョンです"},
	"PREVIOUS_FAILED":    {"前一个节点更新失败", "A previous node failed to update", "前のノードの更新に失敗しました"},
}

func cliUpdatePhaseLabel(out io.Writer, phase string) string {
	if label, ok := cliUpdatePhaseLabels[phase]; ok {
		return label.in(out)
	}
	return phase
}

// Agreement of a node's version with the Master's, and backup availability.
var cliStateLabels = map[string]cliLabel{
	"MATCH":       {"一致", "Same", "一致"},
	"MISMATCH":    {"不一致", "Different", "不一致"},
	"UNKNOWN":     {"未知", "Unknown", "不明"},
	"AVAILABLE":   {"可用", "Available", "利用可能"},
	"UNAVAILABLE": {"不可用", "Unavailable", "利用不可"},
}

func cliStateLabel(out io.Writer, state string) string {
	if label := cliLabelFor(out, state, cliStateLabels); label != "" {
		return label
	}
	return cliUpdateReasonLabel(out, state)
}

// cliVersionText shows a release version as v<version>.
func cliVersionText(version string) string {
	if version == "" || strings.HasPrefix(version, "v") {
		return version
	}
	if version[0] >= '0' && version[0] <= '9' {
		return "v" + version
	}
	return version
}

func cliUpdateReasonLabel(out io.Writer, reason string) string {
	if label, ok := cliUpdateReasonLabels[reason]; ok {
		return label.in(out)
	}
	return reason
}

// cliTruncate shortens text to at most width terminal cells.
func cliTruncate(text string, width int) string {
	if cliTextWidth(text) <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 && cliTextWidth(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// cliErrorSummaries recognize common storage and network failures in raw
// error text. Values are catalog keys: a short sentence saying what to do.
var cliErrorSummaries = []struct {
	pattern *regexp.Regexp
	key     string
}{
	{regexp.MustCompile(`(?i)no such host|server misbehaving|temporary failure in name resolution|name resolution`), "无法解析存储服务地址，请检查 Endpoint；自建存储可改用 path-style 寻址"},
	{regexp.MustCompile(`(?i)connection refused|actively refused`), "存储服务拒绝连接，请检查 Endpoint 地址和端口"},
	{regexp.MustCompile(`(?i)connection reset|broken pipe`), "连接被存储服务中断，可重试失败对象"},
	{regexp.MustCompile(`(?i)x509|certificate|tls: `), "存储服务的 TLS 证书无效或不受信任"},
	{regexp.MustCompile(`(?i)timeout|timed out|deadline exceeded`), "连接存储服务超时，请检查网络后重试"},
	{regexp.MustCompile(`InvalidAccessKeyId`), "Access Key 无效，请检查存储连接的凭据"},
	{regexp.MustCompile(`SignatureDoesNotMatch`), "签名不匹配，请检查 Secret Key 和签名方式"},
	{regexp.MustCompile(`AccessDenied|(?i)\b403\b|forbidden`), "存储服务拒绝访问，请检查凭据对该存储桶的权限"},
	{regexp.MustCompile(`NoSuchBucket`), "存储桶不存在，请检查存储桶名称"},
	{regexp.MustCompile(`NoSuchKey|(?i)\b404\b|not found`), "源对象已不存在"},
	{regexp.MustCompile(`PreconditionFailed|(?i)\b412\b|etag mismatch`), "源对象在迁移期间发生变化，可重试失败对象"},
	{regexp.MustCompile(`SlowDown|(?i)\b503\b|too many requests|\b429\b|throttl`), "存储服务限流，可稍后重试失败对象"},
	{regexp.MustCompile(`(?i)\bEOF\b`), "连接意外中断，可重试失败对象"},
}

// cliErrorSummary returns a short sentence for a raw storage error: a known
// cause, otherwise the first line of the text.
func cliErrorSummary(out io.Writer, message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return "—"
	}
	for _, summary := range cliErrorSummaries {
		if summary.pattern.MatchString(message) {
			return cliText(out, summary.key)
		}
	}
	line, _, _ := strings.Cut(message, "\n")
	return cliTruncate(line, 60)
}

// Server messages the Master sends without translation. Each pattern's
// submatches fill the catalog key's verbs.
var cliServerMessages = []struct {
	pattern *regexp.Regexp
	key     string
}{
	{regexp.MustCompile(`^storage connection (.+) does not exist$`), "存储连接 %s 不存在"},
	{regexp.MustCompile(`^storage connection already exists$`), "存储连接已存在"},
}

// translateServerMessage translates a known untranslated server message.
func translateServerMessage(locale cliLocale, message string) string {
	for _, known := range cliServerMessages {
		if match := known.pattern.FindStringSubmatch(message); match != nil {
			args := make([]interface{}, len(match)-1)
			for i, value := range match[1:] {
				args[i] = value
			}
			return locale.format(known.key, args...)
		}
	}
	return message
}

// mustChangePasswordMessages are the Master's reply to a session that still
// uses the initial password, in each language it answers in.
var mustChangePasswordMessages = map[string]bool{
	"Change the initial password before continuing": true,
	"请先修改初始密码":                                      true,
	"初期パスワードを変更してください":                              true,
}

// cliErrorHints returns follow-up advice for a failed command. fallbackHost
// marks a Master address that was neither given nor discovered.
func cliErrorHints(err error, fallbackHost bool) []string {
	var problem *apiHTTPError
	if errors.As(err, &problem) {
		switch {
		case problem.Status == http.StatusForbidden && mustChangePasswordMessages[problem.Message]:
			return []string{"提示: 请先运行 oss-transfer-cli change-password 修改初始密码。"}
		case problem.Status == http.StatusUnauthorized:
			return []string{"提示: 请运行 oss-transfer-cli login 登录。"}
		}
		return nil
	}
	var dial *net.OpError
	var dns *net.DNSError
	if !(errors.As(err, &dial) && dial.Op == "dial") && !errors.As(err, &dns) {
		return nil
	}
	if fallbackHost {
		return []string{"提示: 未找到本机安装，也未配置 Master；请运行 oss-transfer-cli config set master URL，或用 --host 指定 Master。"}
	}
	// The connection itself failed (refused, timed out or the name did not
	// resolve): no request reached the Master.
	return []string{cliMasterUnreachableHint}
}

// cliMasterUnreachableHint is followed by the Master address the CLI used.
const cliMasterUnreachableHint = "提示: 无法连接 Master（%s）。请检查地址和端口是否正确、Master 服务是否在运行；可用 --host 指定地址，或运行 oss-transfer-cli config set master URL 保存地址。"

// cliAuthFailure reports a rejected session or permission: a failure, never
// an incomplete result.
func cliAuthFailure(err error) bool {
	var problem *apiHTTPError
	return errors.As(err, &problem) && (problem.Status == http.StatusUnauthorized || problem.Status == http.StatusForbidden)
}

// commandLineError reports an error found before any request was sent: the
// command or its arguments are wrong (exit status 2). Offline tools keep
// their own statuses.
func commandLineError(client *ApiClient, args []string, err error) bool {
	if isCLIUsageError(err) {
		return true
	}
	if len(args) == 0 || args[0] == "diff-objects" || args[0] == "count-lines" || client.requestsSent() > 0 {
		return false
	}
	var own *cliMessageError
	return errors.As(err, &own)
}

// humanErrorResult drops the parts of a failed reply that only repeat the
// printed error. It returns nil when nothing else remains.
func humanErrorResult(result map[string]interface{}, err error, locale cliLocale) map[string]interface{} {
	if result == nil || err == nil {
		return result
	}
	printed := localizedCLIError(locale, err)
	var problem *apiHTTPError
	errors.As(err, &problem)
	copy := map[string]interface{}{}
	for key, value := range result {
		switch key {
		case "success", "code", "detail", "reasonCode", "pathStyleHint":
			// The raw detail is shown with --verbose (renderErrorDetail).
			continue
		case "message", "error":
			if text, ok := value.(string); ok && (text == "" || strings.Contains(printed, text) || strings.Contains(printed, translateServerMessage(locale, text)) || problem != nil && text == problem.Message) {
				continue
			}
		}
		copy[key] = value
	}
	if len(copy) == 0 {
		return nil
	}
	return copy
}
