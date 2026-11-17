package master

// Messages that state what a result actually covers: the completeness of a
// failure list, a sampled failure summary, and storage that must be reached
// through another endpoint.
var conclusionMessages = map[string]struct{ en, zh, ja string }{
	"全部 %d 个对象失败。按编号最小的子任务中的 %d 条失败记录统计，最多的原因（%d 条）：%s":                                                                                 {"All %d objects failed. Of %d failure records from the lowest-numbered subtasks, the most common cause (%d records): %s", "全部 %d 个对象失败。按编号最小的子任务中的 %d 条失败记录统计，最多的原因（%d 条）：%s", "%d 個のオブジェクトがすべて失敗しました。番号の小さいサブタスクの失敗記録 %d 件のうち、最も多い原因（%d 件）: %s"},
	"%d 个对象失败。按编号最小的子任务中的 %d 条失败记录统计，最多的原因（%d 条）：%s":                                                                                    {"%d objects failed. Of %d failure records from the lowest-numbered subtasks, the most common cause (%d records): %s", "%d 个对象失败。按编号最小的子任务中的 %d 条失败记录统计，最多的原因（%d 条）：%s", "%d 個のオブジェクトが失敗しました。番号の小さいサブタスクの失敗記録 %d 件のうち、最も多い原因（%d 件）: %s"},
	"Offline Agents with unfinished subtasks: %s. Failures of those subtasks may not be reported yet, so the result may be incomplete.": {"Offline Agents with unfinished subtasks: %s. Failures of those subtasks may not be reported yet, so the result may be incomplete.", "执行节点 %s 离线，其未完成子任务的失败记录可能尚未上报，结果可能不完整。", "ノード %s はオフラインです。未完了のサブタスクの失敗記録がまだ報告されていない可能性があり、結果は不完全な場合があります。"},
}

func init() {
	for key, value := range conclusionMessages {
		if _, exists := apiMessages[key]; !exists {
			apiMessages[key] = value
		}
	}
}
