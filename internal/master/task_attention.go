package master

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MoonWX/oss-transfer/internal/execution"
	"github.com/MoonWX/oss-transfer/internal/task"
)

// failureCauses classify object errors for the task-level summary, in
// order: the first whose marker appears in an error decides its cause.
var failureCauses = []struct {
	code, message string
	markers       []string
}{
	{"SOURCE_CHANGED", "源对象在扫描后发生变化，重试会读取其当前版本", []string{"source changed"}},
	{"NO_SPACE", "目标磁盘空间不足", []string{"no space left on device", "not enough space on the disk"}},
	{"READ_ONLY", "目标文件系统为只读", []string{"read-only file system"}},
	{"LOCAL_PERMISSION_DENIED", "执行节点没有本地目录的读写权限", []string{"permission denied", "access is denied"}},
	{"INVALID_CREDENTIALS", "存储访问密钥无效，请检查连接配置", []string{"InvalidAccessKeyId", "SignatureDoesNotMatch"}},
	{"ACCESS_DENIED", "存储拒绝访问，请检查凭据和存储桶权限", []string{"AccessDenied", "StatusCode: 403"}},
	{"NO_SUCH_BUCKET", "存储桶不存在", []string{"NoSuchBucket"}},
	{"HOST_NOT_FOUND", "无法解析存储地址，请检查 Endpoint 或改用路径风格访问", []string{"no such host"}},
	{"CONNECTION_REFUSED", "无法连接存储（连接被拒绝），请确认存储服务可访问", []string{"connection refused"}},
	{"CONNECTION_RESET", "与存储的连接被中断", []string{"connection reset", "broken pipe", "EOF"}},
	{"TIMEOUT", "连接存储超时", []string{"timeout", "deadline exceeded"}},
	{"SERVER_ERROR", "存储服务端出错", []string{"InternalError", "ServiceUnavailable", "SlowDown", "StatusCode: 5"}},
}

const failureSummaryScan = 1000

func failureCause(e task.Exception) (string, string, string) {
	text := e.Error
	if e.RetryStatus == task.ExceptionRetryFailed && e.RetryError != "" {
		text = e.RetryError
	}
	if e.Reason == execution.ReasonSourceChanged && e.RetryStatus != task.ExceptionRetryFailed || e.RetryReason == execution.ReasonSourceChanged {
		return failureCauses[0].code, failureCauses[0].message, text
	}
	lower := strings.ToLower(text)
	for _, cause := range failureCauses {
		for _, marker := range cause.markers {
			if strings.Contains(lower, strings.ToLower(marker)) {
				return cause.code, cause.message, text
			}
		}
	}
	return "OTHER", "", text
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

// failureSummary groups the task's unresolved failures by cause. It reads at
// most failureSummaryScan exceptions, which is enough to name the main causes.
func (s *Server) failureSummary(ctx context.Context, p task.Parent) map[string]any {
	if p.Counters.Failed == 0 {
		return nil
	}
	type group struct {
		code, message, sample string
		count                 int
	}
	groups := map[string]*group{}
	scanned := 0
	err := s.tasks.repo.VisitExceptions(ctx, task.ExceptionFilter{ParentID: p.ID}, func(e task.Exception) error {
		if e.Resolved() || e.RetryStatus == task.ExceptionPending {
			return nil
		}
		if scanned >= failureSummaryScan {
			return errSummaryFull
		}
		scanned++
		code, message, text := failureCause(e)
		g := groups[code]
		if g == nil {
			g = &group{code: code, message: message, sample: text}
			groups[code] = g
		}
		g.count++
		return nil
	})
	if err != nil && !errors.Is(err, errSummaryFull) || len(groups) == 0 {
		return nil
	}
	ordered := make([]*group, 0, len(groups))
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
