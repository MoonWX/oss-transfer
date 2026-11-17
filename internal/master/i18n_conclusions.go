package master

// Messages that state what a result actually covers: the completeness of a
// failure list, a sampled failure summary, and storage that must be reached
// through another endpoint.
var conclusionMessages = map[string]struct{ en, zh, ja string }{
	"存储桶需要通过其所在地域的 Endpoint 访问，请使用该 Endpoint 创建存储连接":                                                                                                                                             {"The bucket must be accessed through the endpoint of its region; create a storage connection with that endpoint", "存储桶需要通过其所在地域的 Endpoint 访问，请使用该 Endpoint 创建存储连接", "バケットにはそのリージョンのエンドポイントでアクセスする必要があります。そのエンドポイントでストレージ接続を作成してください"},
	"Bucket %s must be accessed through endpoint %s, usually the endpoint of the region the bucket is in. To migrate this bucket, create a storage connection with that endpoint.":               {"Bucket %s must be accessed through endpoint %s, usually the endpoint of the region the bucket is in. To migrate this bucket, create a storage connection with that endpoint.", "存储桶 %s 需要通过 Endpoint %s 访问，这通常是该存储桶所在地域的 Endpoint。如需迁移此存储桶，请使用该 Endpoint 创建存储连接。", "バケット %s にはエンドポイント %s でアクセスする必要があります。通常はバケットのあるリージョンのエンドポイントです。このバケットを転送するには、このエンドポイントでストレージ接続を作成してください。"},
	"Bucket %s must be accessed through another endpoint, usually the endpoint of the region the bucket is in. To migrate this bucket, create a storage connection with that region's endpoint.": {"Bucket %s must be accessed through another endpoint, usually the endpoint of the region the bucket is in. To migrate this bucket, create a storage connection with that region's endpoint.", "存储桶 %s 需要通过另一个 Endpoint 访问，通常是该存储桶所在地域的 Endpoint。如需迁移此存储桶，请使用该地域的 Endpoint 创建存储连接。", "バケット %s には別のエンドポイント（通常はバケットのあるリージョンのエンドポイント）でアクセスする必要があります。このバケットを転送するには、そのリージョンのエンドポイントでストレージ接続を作成してください。"},
	"The bucket must be accessed through another endpoint, usually the endpoint of the region the bucket is in. Create a storage connection with that region's endpoint.":                        {"The bucket must be accessed through another endpoint, usually the endpoint of the region the bucket is in. Create a storage connection with that region's endpoint.", "存储桶需要通过另一个 Endpoint 访问，通常是该存储桶所在地域的 Endpoint。请使用该地域的 Endpoint 创建存储连接。", "バケットには別のエンドポイント（通常はバケットのあるリージョンのエンドポイント）でアクセスする必要があります。そのリージョンのエンドポイントでストレージ接続を作成してください。"},
	"全部 %d 个对象失败。按编号最小的子任务中的 %d 条失败记录统计，最多的原因（%d 条）：%s":                                                                                                                                          {"All %d objects failed. Of %d failure records from the lowest-numbered subtasks, the most common cause (%d records): %s", "全部 %d 个对象失败。按编号最小的子任务中的 %d 条失败记录统计，最多的原因（%d 条）：%s", "%d 個のオブジェクトがすべて失敗しました。番号の小さいサブタスクの失敗記録 %d 件のうち、最も多い原因（%d 件）: %s"},
	"%d 个对象失败。按编号最小的子任务中的 %d 条失败记录统计，最多的原因（%d 条）：%s":                                                                                                                                             {"%d objects failed. Of %d failure records from the lowest-numbered subtasks, the most common cause (%d records): %s", "%d 个对象失败。按编号最小的子任务中的 %d 条失败记录统计，最多的原因（%d 条）：%s", "%d 個のオブジェクトが失敗しました。番号の小さいサブタスクの失敗記録 %d 件のうち、最も多い原因（%d 件）: %s"},
	"Offline Agents with unfinished subtasks: %s. Failures of those subtasks may not be reported yet, so the result may be incomplete.":                                                          {"Offline Agents with unfinished subtasks: %s. Failures of those subtasks may not be reported yet, so the result may be incomplete.", "执行节点 %s 离线，其未完成子任务的失败记录可能尚未上报，结果可能不完整。", "ノード %s はオフラインです。未完了のサブタスクの失敗記録がまだ報告されていない可能性があり、結果は不完全な場合があります。"},
}

func init() {
	for key, value := range conclusionMessages {
		if _, exists := apiMessages[key]; !exists {
			apiMessages[key] = value
		}
	}
}
