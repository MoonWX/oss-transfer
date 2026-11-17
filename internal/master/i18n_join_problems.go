package master

// Why a joining node has not joined yet (task_join_problems.go). Messages that
// node changes already use (version information, a too old Agent, an Agent
// that cannot record a skipped shared check) are translated where they are
// defined.
var joinProblemTranslations = map[string]struct{ en, zh, ja string }{
	"Agent %s is offline; it joins after it reconnects": {"Agent %s is offline; it joins after it reconnects", "执行节点 %s 离线，恢复连接后继续加入", "Agent %s はオフラインです。再接続後に参加します"},
	"Agent %s was enrolled again after it was added to this task and cannot join with its earlier enrollment. Cancel the join, then add the node again": {"Agent %s was enrolled again after it was added to this task and cannot join with its earlier enrollment. Cancel the join, then add the node again", "执行节点 %s 添加到此任务后重新注册过，无法以原来的注册身份加入。请取消此次加入，再重新添加该节点", "Agent %s はこのタスクに追加された後に再登録されたため、以前の登録では参加できません。参加を取り消してから、ノードを追加し直してください"},
	"Agent %s is in an upgrade maintenance window; it joins after the window ends":                                                                      {"Agent %s is in an upgrade maintenance window; it joins after the window ends", "执行节点 %s 正处于升级维护期，维护结束后继续加入", "Agent %s はアップグレードのメンテナンス中です。メンテナンスの終了後に参加します"},
	"Cannot read whether Agent %s is in an upgrade maintenance window: %s":                                                                              {"Cannot read whether Agent %s is in an upgrade maintenance window: %s", "无法读取执行节点 %s 是否处于升级维护期：%s", "Agent %s がアップグレードのメンテナンス中かどうかを読み取れません：%s"},
	"Agent %s was the scanning node of this task; it joins after another node has taken over the scan":                                                  {"Agent %s was the scanning node of this task; it joins after another node has taken over the scan", "执行节点 %s 原为此任务的扫描节点，需等其他节点接管扫描后才能加入", "Agent %s はこのタスクのスキャンノードでした。別のノードがスキャンを引き継いだ後に参加します"},
	"Agent %s has not passed the shared directory check: %s":                                                                                            {"Agent %s has not passed the shared directory check: %s", "执行节点 %s 未通过共享目录校验：%s", "Agent %s は共有ディレクトリの検証に合格していません：%s"},
	"The execution lease of Agent %s for this task could not be set up: %s":                                                                             {"The execution lease of Agent %s for this task could not be set up: %s", "无法为执行节点 %s 建立此任务的执行租约：%s", "Agent %s にこのタスクの実行リースを設定できません：%s"},
	"Cannot record the join of Agent %s: %s":                                                                                                            {"Cannot record the join of Agent %s: %s", "无法记录执行节点 %s 的加入：%s", "Agent %s の参加を記録できません：%s"},
	"Agent %s cannot join: this task has used up the revision numbers that a join needs. Cancel the join":                                               {"Agent %s cannot join: this task has used up the revision numbers that a join needs. Cancel the join", "执行节点 %s 无法加入：此任务加入节点所需的修订号已用尽。请取消此次加入", "Agent %s は参加できません。このタスクでは参加に必要なリビジョン番号を使い切りました。参加を取り消してください"},
	"Agent %s joins after the work of a lost or removed node has moved to other nodes":                                                                  {"Agent %s joins after the work of a lost or removed node has moved to other nodes", "执行节点 %s 将在失联或移出节点的工作转移到其他节点后加入", "Agent %s は、切断または削除されたノードの処理が他のノードに移った後に参加します"},
	"Agent %s joins after the scan has moved to another node":                                                                                           {"Agent %s joins after the scan has moved to another node", "执行节点 %s 将在扫描移交到其他节点后加入", "Agent %s は、スキャンが別のノードに移った後に参加します"},
	"Agent %s joins after takeover is set up for this task. Setting it up waits for these current nodes to be available: %s":                            {"Agent %s joins after takeover is set up for this task. Setting it up waits for these current nodes to be available: %s", "执行节点 %s 需等此任务启用接管后才能加入。启用接管需等待以下现有节点可用：%s", "Agent %s は、このタスクで引き継ぎが有効になった後に参加します。有効にするには、次の既存ノードが利用可能になる必要があります：%s"},
	"Agent %s joins after takeover is set up for this task. Setting it up needs every current node to report that it supports takeover":                 {"Agent %s joins after takeover is set up for this task. Setting it up needs every current node to report that it supports takeover", "执行节点 %s 需等此任务启用接管后才能加入。启用接管需要每个现有节点报告其支持接管", "Agent %s は、このタスクで引き継ぎが有効になった後に参加します。有効にするには、すべての既存ノードが引き継ぎに対応していると報告する必要があります"},
	"Agent %s joins after the task's current problem is resolved: %s":                                                                                   {"Agent %s joins after the task's current problem is resolved: %s", "执行节点 %s 需等此任务的当前问题解决后才能加入：%s", "Agent %s は、このタスクの現在の問題が解決した後に参加します：%s"},
	"The task is not running; Agent %s joins after the task runs again":                                                                                 {"The task is not running; Agent %s joins after the task runs again", "任务当前未运行，执行节点 %s 将在任务重新运行后加入", "タスクは実行されていません。Agent %s はタスクが再び実行された後に参加します"},
}

func init() {
	for key, value := range joinProblemTranslations {
		if _, exists := apiMessages[key]; !exists {
			apiMessages[key] = value
		}
	}
}
