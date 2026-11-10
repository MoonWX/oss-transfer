package main

// Catalog entries of the human output: command summaries, the exceptions
// table, error summaries and hints. They join cliMessages at start-up.
var cliOutputMessages = map[string][2]string{
	// Bucket permission checks.
	"列举对象（ListObjectsV2）":                 {"List objects (ListObjectsV2)", "オブジェクトの一覧（ListObjectsV2）"},
	"读取对象元数据（HeadObject）":                 {"Read object metadata (HeadObject)", "オブジェクトのメタデータを読み取り（HeadObject）"},
	"读取对象内容（GetObject）":                   {"Read object content (GetObject)", "オブジェクトの内容を読み取り（GetObject）"},
	"写入对象（PutObject，条件写入）":                {"Write an object (PutObject, conditional)", "オブジェクトを書き込み（PutObject、条件付き）"},
	"读回写入的对象（HeadObject）":                 {"Read the written object back (HeadObject)", "書き込んだオブジェクトを読み戻し（HeadObject）"},
	"分片上传（创建、上传分片、中止）":                    {"Multipart upload (create, upload a part, abort)", "マルチパートアップロード（作成、パートのアップロード、中止）"},
	"删除测试对象（DeleteObject，迁移不需要）":          {"Delete the test object (DeleteObject, not needed for migrations)", "テストオブジェクトを削除（DeleteObject、移行には不要）"},
	"存储桶为空，没有可读取的对象":                      {"The bucket is empty: no object to read", "バケットが空のため、読み取るオブジェクトがありません"},
	"列举失败，无法选取要读取的对象":                     {"Listing failed, so no object could be chosen to read", "一覧の取得に失敗したため、読み取るオブジェクトを選べません"},
	"写入失败，跳过":                             {"Skipped because the write failed", "書き込みに失敗したためスキップ"},
	"存储桶不存在，不尝试写入":                        {"The bucket does not exist; writing is not tried", "バケットが存在しないため、書き込みは試しません"},
	"未检查写入，加 --write 检查":                  {"Writing not checked; add --write", "書き込みは未確認です。--write を付けて確認してください"},
	"跳过":                                  {"skipped", "スキップ"},
	"存储桶 %s 的权限检查（由执行节点 %s 检查）：\n":        {"Permission check of bucket %s (by execution node %s):\n", "バケット %s の権限の確認（実行ノード %s）:\n"},
	"存储桶 %s 的权限检查：\n":                     {"Permission check of bucket %s:\n", "バケット %s の権限の確認:\n"},
	"结论：存储桶 %s 读取正常但不能写入，只能作为迁移的源。\n":     {"Result: bucket %s can be read but not written; it can only be a migration source.\n", "結果: バケット %s は読み取れますが書き込めません。移行元としてのみ使えます。\n"},
	"结论：存储桶 %s 有 %d 项检查未通过。\n":            {"Result: bucket %s has %d failed checks.\n", "結果: バケット %s の確認のうち %d 件が失敗しました。\n"},
	"结论：存储桶 %s 可以列举对象，但存储桶为空，未能检查读取对象。\n": {"Result: objects of bucket %s can be listed, but the bucket is empty, so reading objects could not be checked.\n", "結果: バケット %s のオブジェクトは一覧できますが、バケットが空のため読み取りを確認できませんでした。\n"},
	"结论：存储桶 %s 读取和写入权限都正常，可以作为迁移的源或目标。\n": {"Result: bucket %s can be read and written; it can be a migration source or target.\n", "結果: バケット %s は読み取りも書き込みもできます。移行元にも移行先にも使えます。\n"},
	"结论：存储桶 %s 读取权限正常，可以作为迁移的源；作为目标时请加 --write 检查写入。\n": {"Result: bucket %s can be read; it can be a migration source. To use it as a target, check writing with --write.\n", "結果: バケット %s は読み取れます。移行元として使えます。移行先にする場合は --write で書き込みを確認してください。\n"},
	"注意：测试对象 %s 未能删除（删除不是迁移所需的权限），请手动删除。\n":             {"Note: the test object %s could not be deleted (deleting is not needed for migrations); delete it manually.\n", "注意: テストオブジェクト %s を削除できませんでした（削除は移行に必要な権限ではありません）。手動で削除してください。\n"},
	"Master 版本较旧，未逐项检查存储桶权限":                            {"This Master is too old to check bucket permissions one by one", "この Master はバージョンが古いため、バケットの権限を 1 つずつ確認できません"},
	"存储桶 %s：%d 项权限检查未通过":                                {"Bucket %s: %d permission checks failed", "バケット %s: 権限の確認のうち %d 件が失敗しました"},
	"--write 需要与 --bucket 一起使用":                         {"--write needs --bucket", "--write は --bucket と一緒に指定してください"},

	// Generic formatting.
	"%s：%s":       {"%s: %s", "%s：%s"},
	"（%s）":        {" (%s)", "（%s）"},
	"%s（HTTP %d）": {"%s (HTTP %d)", "%s（HTTP %d）"},
	"提交 %s · 分支 %s · 构建于 %s\n": {"Commit %s · branch %s · built %s\n", "コミット %s · ブランチ %s · ビルド %s\n"},
	"提交 %s · 构建于 %s\n":         {"Commit %s · built %s\n", "コミット %s · ビルド %s\n"},
	"无":                        {"None", "なし"},
	"查看进度：%s":                  {"Follow progress: %s", "進行状況: %s"},

	// Task creation.
	"未创建任务：%s":                                     {"No task was created: %s", "タスクは作成されませんでした: %s"},
	"%d 个任务未创建：%s":                                 {"%d tasks not created: %s", "%d 件のタスクは作成されませんでした: %s"},
	"第 %s 项：%s":                                    {"Item %s: %s", "%s 件目: %s"},
	"已确认创建的任务：%s":                                  {"Tasks confirmed as created: %s", "作成が確認されたタスク: %s"},
	"已创建 %d 个任务：%s":                                {"Created %d tasks: %s", "%d 件のタスクを作成しました: %s"},
	"查看进度：oss-transfer-cli task show %d --watch 5": {"Follow progress: oss-transfer-cli task show %d --watch 5", "進行状況: oss-transfer-cli task show %d --watch 5"},
	"%w；请求标识 %s（重试时请保留原参数并添加 --request-id %s，以免重复创建；先用 task list 核对是否已创建）": {"%w; request ID %s (to retry without creating duplicates, keep the same arguments and add --request-id %s; check task list first)", "%w。リクエスト ID: %s（重複作成を避けるには同じ引数に --request-id %s を付けて再試行してください。先に task list で確認してください）"},
	"存储连接 %s 不存在": {"Storage connection %s does not exist", "ストレージ接続 %s は存在しません"},
	"存储连接已存在":     {"The storage connection already exists", "ストレージ接続はすでに存在します"},

	// Task control.
	"停止":                 {"stop", "停止"},
	"继续":                 {"resume", "再開"},
	"重试":                 {"retry", "再試行"},
	"启动":                 {"start", "開始"},
	"删除":                 {"delete", "削除"},
	"已请求停止":              {"Stop requested", "停止を要求しました"},
	"已恢复执行":              {"Resumed", "再開しました"},
	"已开始重试":              {"Retry started", "再試行を開始しました"},
	"已启动":                {"Started", "開始しました"},
	"已移入回收站":             {"Moved to trash", "ごみ箱に移動しました"},
	"已从回收站恢复":            {"Restored from trash", "ごみ箱から復元しました"},
	"%s %d 个任务，%d 个无需处理": {"%s %d tasks; %d needed no action", "%s: %d 件、対応不要: %d 件"},
	"未提交：%s":             {"Not submitted: %s", "未送信: %s"},
	"结果未确认，请查看任务状态": {"Outcome unknown; check the task status", "結果は未確認です。タスクの状態を確認してください"},
	"失败：%s":         {"Failed: %s", "失敗: %s"},
	"无需处理":          {"No action needed", "対応不要"},
	"无需处理（当前状态：%s）": {"No action needed (current state: %s)", "対応不要（現在の状態: %s）"},
	"已重试 %d 个失败对象，任务继续运行":                  {"Retried %d failed objects; the task keeps running", "失敗した %d 件のオブジェクトを再試行しました。タスクは実行を続けます"},
	"任务 #%d 的报告已保存到 %s":                    {"Report of task #%d saved to %s", "タスク #%d のレポートを %s に保存しました"},
	"任务报告已写到标准输出，但报告不完整：%s":                {"The task report was written to standard output, but it is incomplete: %s", "タスクレポートを標準出力に書き込みましたが、レポートは不完全です: %s"},
	"--output - 把报告写到标准输出，不能与 --json 同时使用": {"--output - writes the report to standard output and cannot be combined with --json", "--output - はレポートを標準出力に書き出すため、--json と同時に使用できません"},
	"写入标准输出失败: %w":                         {"Cannot write to standard output: %w", "標準出力に書き込めません: %w"},

	// Exceptions.
	"任务\tKey\t大小\t节点\t原因\t重试\t时间": {"Task\tKey\tSize\tNode\tReason\tRetry\tTime", "タスク\tKey\tサイズ\tノード\t理由\t再試行\t時刻"},
	"（%d 次）":           {" (%d times)", "（%d 回）"},
	"显示 %d 条，共 %d 条\n": {"Showing %d of %d records\n", "%d 件を表示（全 %d 件）\n"},
	"共 %d 条\n":         {"%d records\n", "%d 件\n"},
	"没有失败对象":           {"No failed objects", "失敗したオブジェクトはありません"},
	"仅显示部分记录；可调大 --limit，或用 exceptions export 导出全部": {"Only some records are shown; raise --limit or use exceptions export for all of them", "一部の記録のみ表示しています。--limit を増やすか、exceptions export ですべてをエクスポートしてください"},
	"完整错误信息：加 --json，或用 exceptions export 导出 CSV":   {"Full error messages: add --json, or export a CSV with exceptions export", "完全なエラーメッセージ: --json を付けるか、exceptions export で CSV をエクスポートしてください"},
	"当前传输引擎不使用降级记录":                                 {"The current transfer engine does not use fallback records", "現在の転送エンジンはフォールバック記録を使用しません"},
	"当前传输引擎不使用降级记录，无需清理":                            {"The current transfer engine does not use fallback records; nothing to clear", "現在の転送エンジンはフォールバック記録を使用しないため、削除は不要です"},
	"已清理 %d 条已处理的记录":                                {"Cleared %d handled records", "処理済みの記録を %d 件削除しました"},
	"已导出 %d 条记录到 %s":                                {"Exported %d records to %s", "%d 件の記録を %s にエクスポートしました"},
	"仅导出了部分记录；可调大 --limit 后重新导出":                    {"Only some records were exported; raise --limit and export again", "一部の記録のみエクスポートしました。--limit を増やして再度エクスポートしてください"},
	"本地迁移报告已保存到 %s":                                 {"Local transfer report saved to %s", "ローカル転送レポートを %s に保存しました"},

	// Error summaries of failed objects.
	"无法解析存储服务地址，请检查 Endpoint；自建存储可改用 path-style 寻址": {"Cannot resolve the storage address; check the endpoint, or use path-style addressing for self-hosted storage", "ストレージのアドレスを解決できません。エンドポイントを確認するか、自前のストレージではパススタイルを使用してください"},
	"存储服务拒绝连接，请检查 Endpoint 地址和端口":                   {"The storage service refused the connection; check the endpoint address and port", "ストレージサービスが接続を拒否しました。エンドポイントのアドレスとポートを確認してください"},
	"连接被存储服务中断，可重试失败对象":                             {"The storage service dropped the connection; retry the failed objects", "ストレージサービスが接続を切断しました。失敗したオブジェクトを再試行してください"},
	"存储服务的 TLS 证书无效或不受信任":                           {"The storage service's TLS certificate is invalid or not trusted", "ストレージサービスの TLS 証明書が無効か、信頼されていません"},
	"连接存储服务超时，请检查网络后重试":                             {"Connecting to the storage service timed out; check the network and retry", "ストレージサービスへの接続がタイムアウトしました。ネットワークを確認して再試行してください"},
	"Access Key 无效，请检查存储连接的凭据":                      {"The access key is invalid; check the storage connection's credentials", "アクセスキーが無効です。ストレージ接続の認証情報を確認してください"},
	"签名不匹配，请检查 Secret Key 和签名方式":                    {"Signature mismatch; check the secret key and the signature version", "署名が一致しません。シークレットキーと署名方式を確認してください"},
	"存储服务拒绝访问，请检查凭据对该存储桶的权限":                        {"Access denied; check the credentials' permissions on the bucket", "アクセスが拒否されました。認証情報のバケットへの権限を確認してください"},
	"存储桶不存在，请检查存储桶名称":                               {"The bucket does not exist; check its name", "バケットが存在しません。名前を確認してください"},
	"源对象已不存在": {"The source object no longer exists", "転送元オブジェクトはすでに存在しません"},
	"源对象在迁移期间发生变化，可重试失败对象": {"The source object changed during the transfer; retry the failed objects", "転送中に転送元オブジェクトが変更されました。失敗したオブジェクトを再試行してください"},
	"存储服务限流，可稍后重试失败对象":     {"The storage service is throttling; retry the failed objects later", "ストレージサービスがスロットリングしています。後で失敗したオブジェクトを再試行してください"},
	"连接意外中断，可重试失败对象":       {"The connection ended unexpectedly; retry the failed objects", "接続が予期せず終了しました。失敗したオブジェクトを再試行してください"},

	// Hints after an error.
	"提示: 请先运行 oss-transfer-cli change-password 修改初始密码。": {"Hint: run oss-transfer-cli change-password to change the initial password first.", "ヒント: 先に oss-transfer-cli change-password で初期パスワードを変更してください。"},
	"提示: 请运行 oss-transfer-cli login 登录。":                {"Hint: run oss-transfer-cli login to sign in.", "ヒント: oss-transfer-cli login でサインインしてください。"},
	"提示: 无法连接 Master（%s）。请检查地址和端口是否正确、Master 服务是否在运行；可用 --host 指定地址，或运行 oss-transfer-cli config set master URL 保存地址。": {"Hint: could not connect to the Master (%s). Check that the address and port are correct and that the Master service is running; name the address with --host, or save it with oss-transfer-cli config set master URL.", "ヒント: Master（%s）に接続できません。アドレスとポートが正しいこと、Master サービスが動作していることを確認してください。--host でアドレスを指定するか、oss-transfer-cli config set master URL で保存できます。"},
	"提示: 未找到本机安装，也未配置 Master；请运行 oss-transfer-cli config set master URL，或用 --host 指定 Master。":                         {"Hint: no installation was found on this computer and no Master is configured; run oss-transfer-cli config set master URL, or name the Master with --host.", "ヒント: このコンピューターにインストールが見つからず、Master も設定されていません。oss-transfer-cli config set master URL を実行するか、--host で Master を指定してください。"},

	// Accounts.
	"已登录 %s（用户 %s）": {"Signed in to %s as %s", "%s にサインインしました（ユーザー %s）"},
	"已注销 %s":        {"Signed out of %s", "%s からサインアウトしました"},
	"本机服务访问无需注销；账号会话未改变": {"Local service access needs no sign-out; account sessions are unchanged", "ローカルサービスアクセスにサインアウトは不要です。アカウントのセッションは変更されていません"},
	"密码已修改":  {"Password changed", "パスワードを変更しました"},
	"用户名":    {"User name", "ユーザー名"},
	"显示名称":   {"Display name", "表示名"},
	"角色":     {"Role", "ロール"},
	"登录方式":   {"Signed in by", "サインイン方法"},
	"本机服务访问": {"Local service access", "ローカルサービスアクセス"},

	// Storage connections and nodes.
	"存储连接 %s 已保存":             {"Storage connection %s saved", "ストレージ接続 %s を保存しました"},
	"存储连接 %s 已删除":             {"Storage connection %s deleted", "ストレージ接続 %s を削除しました"},
	"，已同步到 %d 个节点":            {", synchronized to %d nodes", "。%d 台のノードに同期しました"},
	"，已同步到 %d 个节点，%d 个节点等待同步": {", synchronized to %d nodes; %d nodes waiting to synchronize", "。%d 台のノードに同期しました。%d 台は同期待ちです"},
	"节点 %s 已更新：%s":            {"Node %s updated: %s", "ノード %s を更新しました: %s"},
	"节点 %s 已移除":               {"Node %s removed", "ノード %s を削除しました"},
	"已提交 %d 个清单文件：%s":         {"Submitted %d inventory files: %s", "%d 件のインベントリファイルを送信しました: %s"},

	// Updates.
	"Master 正在更新到 %s；服务会短暂重启\n":                     {"Master is updating to %s; the service restarts briefly\n", "Master を %s に更新しています。サービスは一時的に再起動します\n"},
	"已安排把 %d 个 Agent 更新到 %s\n":                      {"Scheduled %d agent updates to %s\n", "%d 台の Agent を %s に更新するよう予定しました\n"},
	"节点\t状态\t说明":                                    {"Node\tState\tDetails", "ノード\t状態\t詳細"},
	"查看进度：oss-transfer-cli update status --watch 5": {"Follow progress: oss-transfer-cli update status --watch 5", "進行状況: oss-transfer-cli update status --watch 5"},

	// Local preferences.
	"来源：%s\n":                             {"Source: %s\n", "取得元: %s\n"},
	"自动识别本机安装":                            {"Detected from the local installation", "ローカルインストールから自動検出"},
	"已保存的设置":                              {"Saved setting", "保存済みの設定"},
	"--host 或 OSS_TRANSFER_MASTER_SERVER": {"--host or OSS_TRANSFER_MASTER_SERVER", "--host または OSS_TRANSFER_MASTER_SERVER"},
	"默认地址（未找到本机安装，也未保存 Master）": {"Default address (no local installation found and no Master saved)", "既定のアドレス（ローカルインストールが見つからず、Master も保存されていません）"},

	// Comparisons.
	"比对 %s 已在运行，无需继续": {"Comparison %s is already running; nothing to resume", "比較 %s はすでに実行中です。再開は不要です"},
	"读取速率":            {"Read rate", "読み取り速度"},
	"未发现差异":           {"No differences found", "差異は見つかりませんでした"},
}

func init() {
	for key, value := range cliOutputMessages {
		cliMessages[key] = value
	}
}
