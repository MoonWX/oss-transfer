package master

// Agents name the node setting they reject (nodecontrol.ErrInvalidRoots and
// the others); the Master also names a limit an older Agent does not have.
var nodeSettingMessages = map[string]struct{ en, zh, ja string }{
	"invalid node setting: recorded directories must be absolute paths on this node, at most 100":                   {"Invalid node setting: recorded directories must be absolute paths on this node, at most 100", "节点设置无效：登记目录必须是该节点上的绝对路径，且最多 100 个", "ノード設定が無効です：登録ディレクトリはこのノード上の絶対パスで、最大 100 個までです"},
	"invalid node setting: child concurrency must be 1–256, objects per child 1–64, and their product at most 1024": {"Invalid node setting: child concurrency must be 1–256, objects per child 1–64, and their product at most 1024", "节点设置无效：子任务并发须为 1–256，每个子任务的对象并发须为 1–64，两者乘积不能超过 1024", "ノード設定が無効です：サブタスク並列数は 1–256、オブジェクト並列数は 1–64 で、その積は 1024 以下にしてください"},
	"invalid node setting: unknown limit type":                                                                      {"Invalid node setting: this node's version does not have this limit; upgrade it first", "节点设置无效：此节点版本不支持该限速，请先升级", "ノード設定が無効です：このノードのバージョンはこの制限に対応していません。先に更新してください"},
	"This node's version does not support this limit; upgrade it first":                                             {"This node's version does not support this limit; upgrade it first", "此节点版本不支持该限速，请先升级", "このノードのバージョンはこの制限に対応していません。先に更新してください"},
}

func init() {
	for key, value := range nodeSettingMessages {
		if _, exists := apiMessages[key]; !exists {
			apiMessages[key] = value
		}
	}
}
