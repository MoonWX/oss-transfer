package master

// Messages of retrying the failed objects of a running task (task_retry_failed.go).
var retryFailedMessages = map[string]struct{ en, zh, ja string }{
	"Only a running task can retry failed objects without stopping; use Retry after the task stops or finishes":                               {"Only a running task can retry failed objects without stopping; use Retry after the task stops or finishes", "只有运行中的任务可以不停止而重试失败项；任务停止或结束后请使用“重试”", "停止せずに失敗項目を再試行できるのは実行中のタスクのみです。停止または終了後は「再試行」を使用してください"},
	"Wait until the task's node change or takeover finishes before retrying its failed objects":                                               {"Wait until the task's node change or takeover finishes before retrying its failed objects", "请等待任务的节点调整或接管完成后再重试失败项", "ノード変更または引き継ぎが完了してから失敗項目を再試行してください"},
	"Retry confirmation timed out; refresh the task status":                                                                                   {"Retry confirmation timed out; refresh the task status", "重试请求确认超时，请刷新任务状态", "再試行の確認がタイムアウトしました。タスクの状態を更新してください"},
	"Cannot retry failed objects: %s":                                                                                                         {"Cannot retry failed objects: %s", "无法重试失败项：%s", "失敗項目を再試行できません：%s"},
	"The task changed; refresh and try again":                                                                                                 {"The task changed; refresh and try again", "任务状态已变化，请刷新后重试", "タスクの状態が変更されました。更新してから再試行してください"},
	"No finished child task has failed objects to retry; failed objects of running child tasks can be retried after those child tasks finish": {"No finished child task has failed objects to retry; failed objects of running child tasks can be retried after those child tasks finish", "没有可重试的已结束子任务；运行中子任务的失败对象需等该子任务结束后再重试", "再試行できる終了済みのサブタスクはありません。実行中のサブタスクの失敗オブジェクトは、そのサブタスクの終了後に再試行できます"},
	"Agent %s is leaving this task; its failed objects are retried on the node that takes over its work":                                      {"Agent %s is leaving this task; its failed objects are retried on the node that takes over its work", "执行节点 %s 正在移出此任务；其失败对象将由接管其工作的节点重试", "Agent %s はこのタスクから削除中です。失敗オブジェクトは作業を引き継ぐノードで再試行されます"},
	"Agent %s is updating; retry its failed objects after the update":                                                                         {"Agent %s is updating; retry its failed objects after the update", "执行节点 %s 正在更新，请在更新完成后重试其失败对象", "Agent %s は更新中です。更新完了後に失敗オブジェクトを再試行してください"},
	"Agent %s has been removed or re-enrolled; the original task cannot resume on that identity":                                              {"Agent %s has been removed or re-enrolled; the original task cannot resume on that identity", "执行节点 %s 已被移除或重新注册，原任务无法在该身份上继续", "Agent %s は削除または再登録されたため、元のタスクをその ID で再開できません"},
	"Agent %s is unavailable: %s":                                        {"Agent %s is unavailable: %s", "执行节点 %s 不可用：%s", "Agent %s は利用できません：%s"},
	"Upgrade Agent %s to retry failed objects while the task is running": {"Upgrade Agent %s to retry failed objects while the task is running", "请先升级执行节点 %s，才能在任务运行时重试失败项", "タスク実行中に失敗項目を再試行するには Agent %s を更新してください"},
	"The task is no longer running; the retry was not started":           {"The task is no longer running; the retry was not started", "任务已不在运行，重试未开始", "タスクは実行中ではなくなったため、再試行は開始されませんでした"},
	"This child task is already retrying or has no failed objects left":  {"This child task is already retrying or has no failed objects left", "该子任务正在重试或已没有失败对象", "このサブタスクは再試行中か、失敗オブジェクトが残っていません"},
	"Agent %s has no free job capacity; retry later":                     {"Agent %s has no free job capacity; retry later", "执行节点 %s 的待执行队列已满，请稍后重试", "Agent %s の待機キューに空きがありません。後で再試行してください"},
	"Agent %s refused the retry: %s":                                     {"Agent %s refused the retry: %s", "执行节点 %s 拒绝了重试：%s", "Agent %s が再試行を拒否しました：%s"},
	"Agent %s did not confirm the retry: %s":                             {"Agent %s did not confirm the retry: %s", "执行节点 %s 未确认重试：%s", "Agent %s が再試行を確認しませんでした：%s"},
	"The child task changed; refresh and try again":                      {"The child task changed; refresh and try again", "子任务状态已变化，请刷新后重试", "サブタスクの状態が変更されました。更新してから再試行してください"},
}

func init() {
	for key, value := range retryFailedMessages {
		if _, exists := apiMessages[key]; !exists {
			apiMessages[key] = value
		}
	}
}
