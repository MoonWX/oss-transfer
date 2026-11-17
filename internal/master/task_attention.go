package master

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
	"github.com/MoonWX/oss-transfer/internal/transferstore"
)

// failureMessages are the causes of the task-level failure summary. OTHER has
// no message: the summary shows a sample error instead.
var failureMessages = map[string]string{
	"SOURCE_CHANGED":          "源对象在扫描后发生变化，重试会读取其当前版本",
	"NO_SPACE":                "目标磁盘空间不足",
	"READ_ONLY":               "目标文件系统为只读",
	"LOCAL_PERMISSION_DENIED": "执行节点没有本地目录的读写权限",
	"INVALID_CREDENTIALS":     "存储访问密钥无效，请检查连接配置",
	"ACCESS_DENIED":           "存储拒绝访问，请检查凭据和存储桶权限",
	"ARCHIVED":                "源对象为归档存储，需先解冻再重试",
	"NO_SUCH_BUCKET":          "存储桶不存在",
	"HOST_NOT_FOUND":          "无法解析存储地址，请检查 Endpoint 或改用路径风格访问",
	"CONNECTION_REFUSED":      "无法连接存储（连接被拒绝），请确认存储服务可访问",
	"CONNECTION_RESET":        "与存储的连接被中断",
	"TIMEOUT":                 "连接存储超时",
	"THROTTLED":               "存储服务持续限流，请调低任务的请求上限后重试",
	"SERVER_ERROR":            "存储服务端出错",
}

// localFailures are file-system causes, in order: the first whose marker
// appears in an error decides.
var localFailures = []struct {
	code    string
	markers []string
}{
	{"NO_SPACE", []string{"no space left on device", "not enough space on the disk"}},
	{"READ_ONLY", []string{"read-only file system"}},
	{"LOCAL_PERMISSION_DENIED", []string{"permission denied", "access is denied"}},
}

// storageCodeFailures name the cause of a storage reply by its error code.
// Throttling codes are transferstore's (IsThrottleCode).
var storageCodeFailures = map[string]string{
	"InvalidAccessKeyId":    "INVALID_CREDENTIALS",
	"SignatureDoesNotMatch": "INVALID_CREDENTIALS",
	"AccessDenied":          "ACCESS_DENIED",
	"AllAccessDisabled":     "ACCESS_DENIED",
	"NoSuchBucket":          "NO_SUCH_BUCKET",
	"InternalError":         "SERVER_ERROR",
	"ServiceUnavailable":    "SERVER_ERROR",
	"RequestTimeout":        "TIMEOUT",
}

const failureSummaryScan = 1000

// failureSummaryVisits bounds the exception records one summary reads,
// counting those it skips (resolved, or pending a retry): after a retry of
// many failures most records are pending, and the detail view is polled.
const failureSummaryVisits = 10 * failureSummaryScan

func failureCause(e task.Exception) (string, string, string) {
	text := e.Error
	if e.RetryStatus == task.ExceptionRetryFailed && e.RetryError != "" {
		text = e.RetryError
	}
	code := "SOURCE_CHANGED"
	if !(e.Reason == execution.ReasonSourceChanged && e.RetryStatus != task.ExceptionRetryFailed || e.RetryReason == execution.ReasonSourceChanged) {
		code = objectFailureCause(text)
	}
	return code, failureMessages[code], text
}

// objectFailureCause classifies the text of an object failure without its
// object key. A storage reply is classified by its error code; its HTTP status
// decides only for a reply without one (a HEAD has no body). A 503 without a
// throttling code can be an outage, so only the codes and 429 are throttling.
// GetObject of an archived object that was not restored fails with 403
// InvalidObjectState, which no permission change can fix.
func objectFailureCause(text string) string {
	cause := execution.ObjectErrorCause(text)
	lower := strings.ToLower(cause)
	for _, local := range localFailures {
		for _, marker := range local.markers {
			if strings.Contains(lower, marker) {
				return local.code
			}
		}
	}
	code, status := transferstore.StorageErrorReply(cause)
	if failure, ok := storageCodeFailures[code]; ok {
		return failure
	}
	switch {
	case code == "InvalidObjectState" && strings.Contains(cause, "S3: GetObject"):
		return "ARCHIVED"
	case transferstore.IsThrottleCode(code) || status == http.StatusTooManyRequests:
		return "THROTTLED"
	case status >= 500:
		return "SERVER_ERROR"
	case status == http.StatusForbidden && code == "":
		return "ACCESS_DENIED"
	case status >= 400:
		// Another refusal: its own text says more than a guess.
		return "OTHER"
	}
	switch transferstore.ClassifyStorageMessage(cause) {
	case transferstore.StorageErrorDNS:
		return "HOST_NOT_FOUND"
	case transferstore.StorageErrorConnectionRefused:
		return "CONNECTION_REFUSED"
	case transferstore.StorageErrorConnectionReset, transferstore.StorageErrorEOF:
		return "CONNECTION_RESET"
	case transferstore.StorageErrorTimeout:
		return "TIMEOUT"
	case transferstore.StorageErrorAccessDenied:
		return "ACCESS_DENIED"
	case transferstore.StorageErrorInvalidAccessKey, transferstore.StorageErrorSignatureMismatch:
		return "INVALID_CREDENTIALS"
	case transferstore.StorageErrorNoSuchBucket:
		return "NO_SUCH_BUCKET"
	}
	return "OTHER"
}

// addTaskAttention adds to a task's detail view the cause an operator needs
// first: why its objects failed, summarised from the exception index, or that
// every node it runs on is offline, so it waits for them.
func (s *Server) addTaskAttention(ctx context.Context, row map[string]any, p task.Parent) {
	if summary := s.failureSummary(ctx, p); summary != nil {
		row["failureSummary"] = summary
		row["attention"] = map[string]any{"code": "OBJECTS_FAILED", "severity": "error", "message": summary["message"]}
	}
	if task.Ended(p.Status) {
		return
	}
	offline := s.offlineTaskNodes(p)
	if len(offline) == 0 {
		return
	}
	row["offlineNodes"] = offline
	if len(offline) < len(p.CurrentNodes()) {
		return
	}
	labels := make([]string, 0, len(offline))
	for _, node := range offline {
		labels = append(labels, node["displayLabel"].(string))
	}
	message := fmt.Sprintf(localizeContext(ctx, "执行节点 %s 离线，任务等待其恢复连接"), strings.Join(labels, localizeContext(ctx, "、")))
	row["attention"] = map[string]any{"code": "NODES_OFFLINE", "severity": "warning", "message": message}
}

// offlineTaskNodes lists the task's current nodes that have not connected
// within the online window, with when each was last seen.
func (s *Server) offlineTaskNodes(p task.Parent) []map[string]any {
	nodes := p.CurrentNodes()
	if len(nodes) == 0 {
		return nil
	}
	displays := s.nodeDisplaySnapshot()
	now := s.options.Now()
	s.agents.mu.Lock()
	defer s.agents.mu.Unlock()
	var offline []map[string]any
	for _, node := range nodes {
		seen, ok := s.agents.lastSeen[node.ID]
		if ok && now.Sub(seen) < 75*time.Second {
			continue
		}
		entry := map[string]any{"executionNodeIp": node.Address, "displayLabel": displays.lookup(node.ID, node.Address).DisplayLabel}
		if ok {
			entry["lastSeen"] = seen.UnixMilli()
		}
		offline = append(offline, entry)
	}
	return offline
}

var errSummaryFull = errors.New("summary scan limit")

type failureGroup struct {
	code, message, sample string
	count                 int
}

// failureTally groups unresolved failures by cause. It stops (errSummaryFull)
// after failureSummaryScan of them, or after failureSummaryVisits records of
// any kind, so a summary is a sample when the index is large.
type failureTally struct {
	groups           map[string]*failureGroup
	visited, scanned int
}

func (t *failureTally) add(e task.Exception) error {
	if t.visited >= failureSummaryVisits {
		return errSummaryFull
	}
	t.visited++
	if e.Resolved() || e.RetryStatus == task.ExceptionPending {
		return nil
	}
	if t.scanned >= failureSummaryScan {
		return errSummaryFull
	}
	t.scanned++
	code, message, text := failureCause(e)
	g := t.groups[code]
	if g == nil {
		g = &failureGroup{code: code, message: message, sample: text}
		t.groups[code] = g
	}
	g.count++
	return nil
}

// failureSummary groups the task's unresolved failures by cause. It reads at
// most failureSummaryScan exceptions, which is enough to name the main causes.
func (s *Server) failureSummary(ctx context.Context, p task.Parent) map[string]any {
	if p.Counters.Failed == 0 {
		return nil
	}
	tally := failureTally{groups: map[string]*failureGroup{}}
	err := s.tasks.repo.VisitExceptions(ctx, task.ExceptionFilter{ParentID: p.ID}, tally.add)
	groups, scanned := tally.groups, tally.scanned
	if err != nil && !errors.Is(err, errSummaryFull) || len(groups) == 0 {
		return nil
	}
	ordered := make([]*failureGroup, 0, len(groups))
	for _, g := range groups {
		ordered = append(ordered, g)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].count != ordered[j].count {
			return ordered[i].count > ordered[j].count
		}
		return ordered[i].code < ordered[j].code
	})
	if len(ordered) > 3 {
		ordered = ordered[:3]
	}
	causes := make([]map[string]any, 0, len(ordered))
	for _, g := range ordered {
		message := localizeContext(ctx, g.message)
		if g.message == "" {
			message = g.sample
			if len(message) > 300 {
				message = message[:300] + "…"
			}
		}
		causes = append(causes, map[string]any{"code": g.code, "count": g.count, "message": message, "sample": g.sample})
	}
	allFailed := task.Ended(p.Status) && p.Counters.Succeeded+p.Counters.Skipped == 0
	main := causes[0]["message"].(string)
	var message string
	if allFailed {
		message = fmt.Sprintf(localizeContext(ctx, "全部 %d 个对象失败：%s"), p.Counters.Failed, main)
	} else {
		message = fmt.Sprintf(localizeContext(ctx, "%d 个对象失败，主要原因：%s"), p.Counters.Failed, main)
	}
	return map[string]any{"failed": p.Counters.Failed, "allFailed": allFailed, "causes": causes, "sampled": scanned, "truncated": errors.Is(err, errSummaryFull), "message": message}
}
