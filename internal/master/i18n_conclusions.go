package master

// Messages that state what a result actually covers: the completeness of a
// failure list, a sampled failure summary, and storage that must be reached
// through another endpoint.
var conclusionMessages = map[string]struct{ en, zh, ja string }{
	"Offline Agents with unfinished subtasks: %s. Failures of those subtasks may not be reported yet, so the result may be incomplete.": {"Offline Agents with unfinished subtasks: %s. Failures of those subtasks may not be reported yet, so the result may be incomplete.", "执行节点 %s 离线，其未完成子任务的失败记录可能尚未上报，结果可能不完整。", "ノード %s はオフラインです。未完了のサブタスクの失敗記録がまだ報告されていない可能性があり、結果は不完全な場合があります。"},
}

func init() {
	for key, value := range conclusionMessages {
		if _, exists := apiMessages[key]; !exists {
			apiMessages[key] = value
		}
	}
}
