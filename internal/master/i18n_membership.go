package master

// Execution-node change messages are kept apart from i18n_messages.go so that
// the feature's catalog stays reviewable in one place.
var membershipMessages = map[string]struct{ en, zh, ja string }{
	"Deleted tasks cannot change execution nodes":                                         {"Deleted tasks cannot change execution nodes", "已删除的任务不能调整执行节点", "削除済みのタスクは実行ノードを変更できません"},
	"Completed tasks cannot change execution nodes":                                       {"Completed tasks cannot change execution nodes", "已完成的任务不能调整执行节点", "完了したタスクは実行ノードを変更できません"},
	"Tasks migrated from the previous version cannot change execution nodes":              {"Tasks migrated from the previous version cannot change execution nodes", "从旧版本迁移的任务不能调整执行节点", "旧バージョンから移行したタスクは実行ノードを変更できません"},
	"Single-object and single-file tasks cannot change execution nodes":                   {"Single-object and single-file tasks cannot change execution nodes", "单对象和单文件任务不能调整执行节点", "単一オブジェクト・単一ファイルのタスクは実行ノードを変更できません"},
	"Wait until the task has finished stopping or resuming":                               {"Wait until the task has finished stopping or resuming", "请等待任务停止或恢复完成后再调整", "タスクの停止または再開が完了するまでお待ちください"},
	"The task's execution nodes changed; refresh and try again":                           {"The task's execution nodes changed; refresh and try again", "任务的执行节点已变化，请刷新后重试", "タスクの実行ノードが変更されました。更新してから再試行してください"},
	"Agent %s is already being removed immediately":                                       {"Agent %s is already being removed immediately", "执行节点 %s 正在立即移出", "Agent %s はすでに即時削除中です"},
	"Agent %s is already joining this task":                                               {"Agent %s is already joining this task", "执行节点 %s 正在加入此任务", "Agent %s はすでにこのタスクに参加中です"},
	"Agent %s is already a member; it rejoins automatically after reconnecting":           {"Agent %s is already a member; it rejoins automatically after reconnecting", "执行节点 %s 已是成员，重新连接后会自动加入", "Agent %s はすでにメンバーです。再接続すると自動的に復帰します"},
	"Agent %s is already an execution node of this task":                                  {"Agent %s is already an execution node of this task", "执行节点 %s 已在此任务中", "Agent %s はすでにこのタスクの実行ノードです"},
	"Agent %s is not an execution node of this task":                                      {"Agent %s is not an execution node of this task", "执行节点 %s 不在此任务中", "Agent %s はこのタスクの実行ノードではありません"},
	"Agent %s is already being removed":                                                   {"Agent %s is already being removed", "执行节点 %s 已在移出中", "Agent %s はすでに削除中です"},
	"Immediate removal needs takeover support: OSS-to-OSS or a verified shared directory": {"Immediate removal needs takeover support: OSS-to-OSS or a verified shared directory", "立即移出需要支持接管：仅适用于 OSS→OSS 或已验证的共享目录", "即時削除には引き継ぎ対応が必要です（OSS→OSS または検証済みの共有ディレクトリ）"},
	"At least one execution node must remain":                                             {"At least one execution node must remain", "至少需要保留一个执行节点", "少なくとも 1 つの実行ノードを残してください"},
	"The task bandwidth limit is too small for the selected Agents":                       {"The task bandwidth limit is too small for the selected Agents", "任务带宽上限不足以分配给所选执行节点", "タスクの帯域上限が選択した Agent 数に対して小さすぎます"},
	"A previous node change is still in progress: %s. Wait for it to finish, cancel a join, re-add a draining node, or remove a draining node immediately": {"A previous node change is still in progress: %s. Wait for it to finish, cancel a join, re-add a draining node, or remove a draining node immediately", "上一次节点调整尚未完成：%s。请等待完成，或取消加入、保留正在移出的节点、将移出改为立即移出", "前回のノード変更がまだ進行中です：%s。完了を待つか、参加の取り消し、削除中ノードの維持、即時削除への切り替えのみ行えます"},
	"removal mode must be drain or now":                                   {"removal mode must be drain or now", "移出方式只能是 drain 或 now", "削除方式は drain または now を指定してください"},
	"Removal mode must be drain or now":                                   {"Removal mode must be drain or now", "移出方式只能是 drain 或 now", "削除方式は drain または now を指定してください"},
	"add or remove between 1 and 64 execution nodes":                      {"add or remove between 1 and 64 execution nodes", "每次可添加或移出 1–64 个执行节点", "一度に追加・削除できる実行ノードは 1–64 個です"},
	"invalid or duplicate execution node":                                 {"invalid or duplicate execution node", "执行节点无效或重复", "実行ノードが無効または重複しています"},
	"the same execution node cannot be added and removed":                 {"the same execution node cannot be added and removed", "同一执行节点不能同时添加和移出", "同じ実行ノードを追加と削除の両方に指定できません"},
	"requestId conflicts with Idempotency-Key":                            {"requestId conflicts with Idempotency-Key", "requestId 与 Idempotency-Key 不一致", "requestId と Idempotency-Key が一致しません"},
	"Request ID is too long":                                              {"Request ID is too long", "请求标识过长", "リクエスト ID が長すぎます"},
	"Select execution nodes to add or remove":                             {"Select execution nodes to add or remove", "请选择要添加或移出的执行节点", "追加または削除する実行ノードを選択してください"},
	"Invalid or duplicate local node binding":                             {"Invalid or duplicate local node binding", "本地节点绑定无效或重复", "ローカルノードのバインドが無効または重複しています"},
	"This request ID was already used for a different node change":        {"This request ID was already used for a different node change", "此请求标识已用于另一次节点调整", "このリクエスト ID は別のノード変更で使用済みです"},
	"A draining node keeps its existing binding":                          {"A draining node keeps its existing binding", "保留正在移出的节点时沿用其原有路径", "削除中のノードを維持する場合は既存のパスを使用します"},
	"Agent %s is not online in this enrollment":                           {"Agent %s is not online in this enrollment", "执行节点 %s 未在线", "Agent %s はオンラインではありません"},
	"Agent %s needs a local path bound to the shared directory":           {"Agent %s needs a local path bound to the shared directory", "执行节点 %s 需要绑定共享目录的本地路径", "Agent %s に共有ディレクトリのローカルパスを指定してください"},
	"Only local transfers accept node bindings":                           {"Only local transfers accept node bindings", "只有本地传输任务需要绑定路径", "パスのバインドはローカル転送でのみ指定できます"},
	"Provide exactly one local path for each added node":                  {"Provide exactly one local path for each added node", "每个新增节点需要且只能绑定一个本地路径", "追加する各ノードにローカルパスを 1 つずつ指定してください"},
	"Agent %s is updating; try again after the update":                    {"Agent %s is updating; try again after the update", "执行节点 %s 正在更新，请更新完成后重试", "Agent %s は更新中です。更新完了後に再試行してください"},
	"Agent %s is too old to join running tasks; upgrade it first":         {"Agent %s is too old to join running tasks; upgrade it first", "执行节点 %s 版本过旧，无法加入运行中的任务，请先升级", "Agent %s は古いため実行中のタスクに参加できません。先に更新してください"},
	"Agent %s is offline; reconnect it before changing this task's nodes": {"Agent %s is offline; reconnect it before changing this task's nodes", "执行节点 %s 离线，请恢复连接后再调整此任务的节点", "Agent %s がオフラインです。再接続してからノードを変更してください"},
	"Agent %s is too old for node changes; upgrade it first":              {"Agent %s is too old for node changes; upgrade it first", "执行节点 %s 版本过旧，不支持调整节点，请先升级", "Agent %s は古いためノード変更に対応していません。先に更新してください"},
	"Reconnect at least one current node to verify the shared directory":  {"Reconnect at least one current node to verify the shared directory", "请至少恢复一个现有节点的连接，以验证共享目录", "共有ディレクトリを検証するため、既存ノードを少なくとも 1 つ再接続してください"},
	"Upgrade this Agent to join running tasks":                            {"Upgrade this Agent to join running tasks", "升级此节点后才能加入运行中的任务", "実行中のタスクに参加するにはこの Agent を更新してください"},
}

func init() {
	for key, value := range membershipMessages {
		if _, exists := apiMessages[key]; !exists {
			apiMessages[key] = value
		}
	}
}
