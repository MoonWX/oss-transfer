export const adminMessages: Record<string, { en: string; ja: string }> = {
  已清理: { en: "Cleaned up", ja: "クリーンアップ済み" },
  "已受理清理；已移除节点的本地历史保留：{value1}。": {
    en: "Cleanup accepted. Local history remains on removed nodes: {value1}.",
    ja: "クリーンアップを受理しました。削除済みノードのローカル履歴は残ります：{value1}。",
  },
  请求失败: { en: "Request failed", ja: "リクエストに失敗しました" },
  "任务 {id} 清理进度": {
    en: "Task {id} cleanup progress",
    ja: "タスク {id} のクリーンアップ進捗",
  },
  等待清理: { en: "Queued for cleanup", ja: "クリーンアップ待ち" },
  等待自动继续: { en: "Waiting to continue", ja: "自動再開を待機中" },
  清理主节点记录: {
    en: "Cleaning Master records",
    ja: "Master の記録を削除中",
  },
  清理执行节点记录: {
    en: "Cleaning Agent records",
    ja: "Agent の記録を削除中",
  },
  "已清理 {count} 条记录": {
    en: "{count} records removed",
    ja: "{count} 件の記録を削除済み",
  },
  "节点 {done}/{total}": {
    en: "Nodes {done}/{total}",
    ja: "ノード {done}/{total}",
  },
  "已完成 {count} 个任务的清理。": {
    en: "Cleanup finished for {count} tasks.",
    ja: "{count} 件のタスクのクリーンアップが完了しました。",
  },
  "已开始后台清理，可离开此页面。": {
    en: "Cleanup is running in the background. You can leave this page.",
    ja: "バックグラウンドでクリーンアップを開始しました。このページを閉じても処理は続行されます。",
  },
  "暂时无法获取清理进度，恢复连接后自动刷新。": {
    en: "Cleanup progress is unavailable. It will refresh when the connection returns.",
    ja: "クリーンアップの進捗を取得できません。接続が復旧すると自動更新します。",
  },
  批量删除进度: { en: "Bulk deletion progress", ja: "一括削除の進捗" },
  查看失败详情: { en: "Show failures", ja: "失敗の詳細を表示" },
  收起失败详情: { en: "Hide failures", ja: "失敗の詳細を閉じる" },
  "节点标识：{value1}": { en: "Node ID: {value1}", ja: "ノード ID：{value1}" },
  保存: { en: "Save", ja: "保存" },
  权限: { en: "Access", ja: "アクセス" },
  只读: { en: "Read only", ja: "読み取り専用" },
  浏览器访问地址: { en: "Web address", ja: "ブラウザー用アドレス" },
  "Agent 连接地址": { en: "Agent connection address", ja: "Agent 接続先" },
  "填写新节点可访问的主节点地址，同一 VPC 可使用内网地址。": {
    en: "Use a Master address reachable from the new node. A private address works within the same VPC.",
    ja: "新しいノードから到達できる Master のアドレスを指定します。同じ VPC 内ではプライベートアドレスを使用できます。",
  },
  "留空使用浏览器地址。同一 VPC 可填写主节点内网地址。": {
    en: "Defaults to the web address. Use the Master's private address within the same VPC.",
    ja: "空欄の場合はブラウザー用アドレスを使用します。同じ VPC 内では Master のプライベートアドレスを指定できます。",
  },
  连接地址: { en: "Connection addresses", ja: "接続先の設定" },
  "连接地址已保存。": {
    en: "Connection addresses saved.",
    ja: "接続先を保存しました。",
  },
  "用于新节点的加入指令。已加入节点继续使用原连接地址。": {
    en: "Applies to new node invitations. Existing Agents keep their current connection address.",
    ja: "新しいノードの参加コマンドに適用されます。参加済みの Agent は現在の接続先を維持します。",
  },
  "请输入节点名称。": {
    en: "Enter a node name.",
    ja: "ノード名を入力してください。",
  },
  节点详情: { en: "Node details", ja: "ノードの詳細" },
  "节点未返回完整信息。": {
    en: "The node did not return complete details.",
    ja: "ノードからすべての情報を取得できませんでした。",
  },
  主机名: { en: "Hostname", ja: "ホスト名" },
  "系统 / 架构": { en: "OS / architecture", ja: "OS / アーキテクチャ" },
  "网卡 IP": { en: "Interface IPs", ja: "インターフェース IP" },
  "连接来源 IP": { en: "Connection source IP", ja: "接続元 IP" },
  最后连接: { en: "Last connected", ja: "最終接続" },
  "内部 ID": { en: "Internal ID", ja: "内部 ID" },
  "连接来源为 Master 观测到的地址，可能是内网、NAT 或代理地址。": {
    en: "The source address is observed by the Master and may be a private, NAT or proxy address.",
    ja: "接続元は Master が確認したアドレスです。プライベート、NAT、プロキシのアドレスの場合があります。",
  },
  挂载目录: { en: "Mounts", ja: "マウント" },
  目录: { en: "Directory", ja: "ディレクトリ" },
  文件系统: { en: "Filesystem", ja: "ファイルシステム" },
  读写: { en: "Read/write", ja: "読み書き" },
  本地文件系统: {
    en: "Local filesystem",
    ja: "ローカルファイルシステム",
  },
  "{value1}（{value2} 字节）": {
    en: "{value1} ({value2} bytes)",
    ja: "{value1}（{value2} バイト）",
  },
  "请填写源、目标对象的 Bucket 和 Key。": {
    en: "Enter the source and destination buckets and keys.",
    ja: "転送元と転送先のバケットと Key を入力してください。",
  },
  "部分记录未删除，请刷新后重试。": {
    en: "Some records were not deleted. Refresh and try again.",
    ja: "一部のレコードを削除できませんでした。更新して再試行してください。",
  },
  "（未完成节点：{value1}）": {
    en: " (Incomplete nodes: {value1})",
    ja: "（未完了のノード：{value1}）",
  },
  "{value1}{value2}": {
    en: "{value1}{value2}",
    ja: "{value1}{value2}",
  },
  "，未响应节点：{value1}": {
    en: "; unresponsive nodes: {value1}",
    ja: "。応答しないノード：{value1}",
  },
  "，请刷新后重试": {
    en: "; refresh and try again",
    ja: "。更新して再試行してください",
  },
  "部分记录未删除{value1}。": {
    en: "Some records were not deleted{value1}.",
    ja: "一部のレコードを削除できませんでした{value1}。",
  },
  "任务记录已删除；已移除节点的本地历史保留：{value1}。": {
    en: "Task records deleted. Local history remains on removed nodes: {value1}.",
    ja: "タスクレコードを削除しました。削除済みノードのローカル履歴は残ります：{value1}。",
  },
  "永久删除请求已提交，正在刷新回收站。": {
    en: "Deletion requested. Refreshing the recycle bin.",
    ja: "完全削除を要求しました。ごみ箱を更新しています。",
  },
  运维工具: {
    en: "Tools",
    ja: "管理ツール",
  },
  "比对对象元数据，管理回收站任务记录。": {
    en: "Compare object metadata and manage deleted task records.",
    ja: "オブジェクトのメタデータ比較と削除済みタスクの管理。",
  },
  对象比对: {
    en: "Compare objects",
    ja: "オブジェクト比較",
  },
  任务回收站: {
    en: "Recycle bin",
    ja: "タスクのごみ箱",
  },
  源对象: {
    en: "Source object",
    ja: "転送元オブジェクト",
  },
  "源 Bucket": {
    en: "Source bucket",
    ja: "転送元バケット",
  },
  "连接名称/bucket-name": {
    en: "connection-name/bucket-name",
    ja: "接続名/bucket-name",
  },
  "使用已配置的连接名称，例如 storage/my-bucket。": {
    en: "Use a configured connection name, such as storage/my-bucket.",
    ja: "設定済みの接続名を使用します。例：storage/my-bucket。",
  },
  "源对象 Key": {
    en: "Source object key",
    ja: "転送元オブジェクトキー",
  },
  目标对象: {
    en: "Destination object",
    ja: "転送先オブジェクト",
  },
  "目标 Bucket": {
    en: "Destination bucket",
    ja: "転送先バケット",
  },
  "填写连接名称和存储桶，例如 storage/my-bucket。": {
    en: "Enter a connection name and bucket, such as storage/my-bucket.",
    ja: "接続名とバケットを入力します。例：storage/my-bucket。",
  },
  "目标对象 Key": {
    en: "Destination object key",
    ja: "転送先オブジェクトキー",
  },
  "目标使用相同 Key": {
    en: "Use the same destination key",
    ja: "転送先に同じ Key を使用",
  },
  "正在读取元数据…": {
    en: "Reading metadata…",
    ja: "メタデータを読み込み中…",
  },
  开始比对: {
    en: "Compare",
    ja: "比較",
  },
  对象元数据比对: {
    en: "Compare object metadata",
    ja: "オブジェクトのメタデータ比較",
  },
  "比较对象大小、ETag、存储类型和自定义元数据，不修改对象。": {
    en: "Compare size, ETag, storage class and custom metadata without changing objects.",
    ja: "オブジェクトを変更せず、サイズ、ETag、ストレージクラス、カスタムメタデータを比較します。",
  },
  "查询失败，存在状态未知": {
    en: "Query failed; existence unknown",
    ja: "取得失敗・存在不明",
  },
  读取成功: {
    en: "Retrieved",
    ja: "取得済み",
  },
  对象不存在: {
    en: "Object not found",
    ja: "オブジェクトなし",
  },
  存在状态未知: {
    en: "Existence unknown",
    ja: "存在不明",
  },
  "两个对象的大小一致。": {
    en: "The object sizes match.",
    ja: "オブジェクトのサイズは一致しています。",
  },
  "两个对象的大小不一致，请检查迁移结果。": {
    en: "The object sizes differ. Check the transfer result.",
    ja: "オブジェクトのサイズが異なります。転送結果を確認してください。",
  },
  "元数据不完整，无法完成比对。": {
    en: "Metadata is incomplete; comparison is unavailable.",
    ja: "メタデータが不完全なため比較できません。",
  },
  "对象大小和 ETag 的比较不等同于完整内容校验。": {
    en: "Size and ETag comparisons do not verify the full content.",
    ja: "サイズと ETag の比較では、内容全体の一致は検証できません。",
  },
  元数据差异: {
    en: "Metadata differences",
    ja: "メタデータの差分",
  },
  "{value1} 个字段不同": {
    en: "{value1} fields differ",
    ja: "{value1} フィールドに差分",
  },
  "元数据不完整，无法统计差异": {
    en: "Incomplete metadata; differences unavailable",
    ja: "メタデータが不完全なため差分を集計できません",
  },
  显示全部字段: {
    en: "Show all fields",
    ja: "全フィールドを表示",
  },
  仅看差异: {
    en: "Differences only",
    ja: "差分のみ",
  },
  字段: {
    en: "Field",
    ja: "フィールド",
  },
  存在差异: {
    en: "Different",
    ja: "差分あり",
  },
  自定义元数据: {
    en: "Custom metadata",
    ja: "カスタムメタデータ",
  },
  没有元数据差异: {
    en: "No metadata differences",
    ja: "メタデータの差分なし",
  },
  "本次返回的元数据字段一致。": {
    en: "The returned metadata fields match.",
    ja: "取得したメタデータのフィールドは一致しています。",
  },
  "回收站保留已标记删除的任务。永久删除仅清理任务记录，无法恢复；不删除源端或目标端的数据对象。":
    {
      en: "The recycle bin holds deleted tasks. Permanent deletion removes task records only and cannot be undone. Source and destination objects are kept.",
      ja: "ごみ箱には削除済みタスクが保存されます。完全削除で消去されるのはタスクレコードのみで、復元はできません。転送元と転送先のオブジェクトは残ります。",
    },
  "节点清单读取失败，无法确认回收站是否覆盖全部节点。": {
    en: "The node list could not be read. Recycle bin coverage is unknown.",
    ja: "ノード一覧を取得できないため、全ノードのごみ箱データが揃っているか確認できません。",
  },
  "以下节点未返回回收站数据，当前列表可能不完整：": {
    en: "The recycle bin may be incomplete. No data from: ",
    ja: "次のノードからデータを取得できないため、ごみ箱の一覧は不完全な可能性があります：",
  },
  "部分回收站记录未返回，当前列表可能不完整。": {
    en: "Some recycle bin records are missing. The list may be incomplete.",
    ja: "一部のごみ箱レコードを取得できません。一覧は不完全な可能性があります。",
  },
  加载中: {
    en: "Loading",
    ja: "読み込み中",
  },
  加载更早记录: {
    en: "Load earlier records",
    ja: "以前のレコードを読み込む",
  },
  "已加载 {count} 条记录。": {
    en: "Loaded {count} records.",
    ja: "{count} 件を読み込みました。",
  },
  已删除任务: {
    en: "Deleted tasks",
    ja: "削除済みタスク",
  },
  " · 已选择 {value1} 个任务 ID": {
    en: " · {value1} task IDs selected",
    ja: " · タスク ID を {value1} 件選択中",
  },
  "{value1} 条节点任务记录{value2}": {
    en: "{value1} node task records{value2}",
    ja: "ノード上のタスクレコード {value1} 件{value2}",
  },
  正在读取回收站: {
    en: "Loading recycle bin",
    ja: "ごみ箱を読み込み中",
  },
  刷新: {
    en: "Refresh",
    ja: "更新",
  },
  永久删除: {
    en: "Delete permanently",
    ja: "完全に削除",
  },
  刷新回收站: {
    en: "Refresh recycle bin",
    ja: "ごみ箱を更新",
  },
  选择全部已删除任务: {
    en: "Select all deleted tasks",
    ja: "削除済みタスクをすべて選択",
  },
  任务: {
    en: "Task",
    ja: "タスク",
  },
  迁移路径: {
    en: "Transfer path",
    ja: "転送経路",
  },
  节点: {
    en: "Node",
    ja: "ノード",
  },
  最终状态: {
    en: "Final status",
    ja: "最終状態",
  },
  已传输: {
    en: "Transferred",
    ja: "転送済み",
  },
  "选择任务 {value1}": {
    en: "Select task {value1}",
    ja: "タスク {value1} を選択",
  },
  "{value1} 个对象": {
    en: "{value1} objects",
    ja: "{value1} オブジェクト",
  },
  每页: {
    en: "Rows per page",
    ja: "1 ページの件数",
  },
  下一页: {
    en: "Next page",
    ja: "次のページ",
  },
  上一页: {
    en: "Previous page",
    ja: "前のページ",
  },
  第一页: {
    en: "First page",
    ja: "最初のページ",
  },
  最后一页: {
    en: "Last page",
    ja: "最後のページ",
  },
  暂无已删除任务: {
    en: "No deleted tasks",
    ja: "削除済みタスクなし",
  },
  "从任务列表删除的记录会移入回收站。": {
    en: "Tasks deleted from the task list appear here.",
    ja: "タスク一覧から削除したレコードがここに表示されます。",
  },
  "永久删除 {count} 个任务？": {
    en: "Permanently delete {count} tasks?",
    ja: "{count} 件のタスクを完全に削除しますか？",
  },
  "此操作无法撤销，将清理所有执行节点上同 ID 的已删除任务记录。": {
    en: "This removes deleted task records with these IDs from all workers and cannot be undone.",
    ja: "全実行ノードから、同じ ID の削除済みタスクレコードを消去します。この操作は取り消せません。",
  },
  "任务 ID：{ids}": {
    en: "Task IDs: {ids}",
    ja: "タスク ID：{ids}",
  },
  "源端和目标端的数据对象不受影响。": {
    en: "Source and destination objects are not affected.",
    ja: "転送元と転送先のオブジェクトには影響しません。",
  },
  取消: {
    en: "Cancel",
    ja: "キャンセル",
  },
  确认永久删除: {
    en: "Delete permanently",
    ja: "完全に削除",
  },
  留空保留现有凭据: {
    en: "Leave blank to keep current credentials",
    ja: "空欄の場合は現在の認証情報を保持します",
  },
  "更换凭据时，请同时填写 Access Key 和 Secret Key。": {
    en: "Enter both Access Key and Secret Key to replace credentials.",
    ja: "認証情報を変更するには Access Key と Secret Key の両方を入力してください。",
  },
  "请填写连接名称、Endpoint、Access Key 和 Secret Key。": {
    en: "Enter the connection name, endpoint, access key and secret key.",
    ja: "接続名、Endpoint、Access Key、Secret Key を入力してください。",
  },
  "连接名称已存在，请更换名称或编辑现有连接。": {
    en: "This connection name already exists. Choose another name or edit the existing connection.",
    ja: "この接続名は既に使用されています。別の名前を指定するか、既存の接続を編集してください。",
  },
  存储连接已更新: {
    en: "Storage connection updated",
    ja: "ストレージ接続を更新しました",
  },
  存储连接已添加: {
    en: "Storage connection added",
    ja: "ストレージ接続を追加しました",
  },
  存储连接已删除: {
    en: "Storage connection deleted",
    ja: "ストレージ接続を削除しました",
  },
  存储连接: {
    en: "Storage connections",
    ja: "ストレージ接続",
  },
  "管理 S3 兼容存储连接。": {
    en: "Manage S3-compatible storage connections.",
    ja: "S3 互換ストレージへの接続を管理します。",
  },
  添加连接: {
    en: "Add connection",
    ja: "接続を追加",
  },
  部分节点未完成连接同步: {
    en: "Some nodes did not sync the connection",
    ja: "一部のノードで接続の同期が完了していません",
  },
  重试: {
    en: "Retry",
    ja: "再試行",
  },
  已配置连接: {
    en: "Configured connections",
    ja: "設定済みの接続",
  },
  搜索连接: {
    en: "Search connections",
    ja: "接続を検索",
  },
  刷新连接: {
    en: "Refresh connections",
    ja: "接続を更新",
  },
  正在加载存储连接: {
    en: "Loading storage connections",
    ja: "ストレージ接続を読み込み中",
  },
  暂无匹配连接: {
    en: "No matching connections",
    ja: "一致する接続なし",
  },
  暂无存储连接: {
    en: "No storage connections",
    ja: "ストレージ接続なし",
  },
  "请更换连接名称或 Endpoint 关键词。": {
    en: "Try another connection name or endpoint keyword.",
    ja: "別の接続名または Endpoint のキーワードを試してください。",
  },
  "添加存储连接，供迁移任务使用。": {
    en: "Add a storage connection to use in transfer tasks.",
    ja: "転送タスクで使用するストレージ接続を追加してください。",
  },
  "S3 兼容对象存储": {
    en: "S3-compatible object storage",
    ja: "S3 互換オブジェクトストレージ",
  },
  默认签名: {
    en: "Default signing",
    ja: "デフォルト署名",
  },
  默认寻址: {
    en: "Default addressing",
    ja: "デフォルトのアドレス形式",
  },
  访问凭据已隐藏: {
    en: "Credentials hidden",
    ja: "認証情報は非表示",
  },
  测试连接: {
    en: "Test connection",
    ja: "接続テスト",
  },
  编辑连接: {
    en: "Edit connection",
    ja: "接続を編集",
  },
  "编辑连接 {value1}": {
    en: "Edit connection {value1}",
    ja: "接続 {value1} を編集",
  },
  删除连接: {
    en: "Delete connection",
    ja: "接続を削除",
  },
  "删除连接 {value1}": {
    en: "Delete connection {value1}",
    ja: "接続 {value1} を削除",
  },
  编辑存储连接: {
    en: "Edit storage connection",
    ja: "ストレージ接続を編集",
  },
  添加存储连接: {
    en: "Add storage connection",
    ja: "ストレージ接続を追加",
  },
  连接名称: {
    en: "Connection name",
    ja: "接続名",
  },
  "例如：production-oss": {
    en: "Example: production-oss",
    ja: "例：production-oss",
  },
  "连接名称用于关联任务，创建后不可修改。": {
    en: "Tasks reference this name. It cannot be changed after creation.",
    ja: "タスクが参照する名前です。作成後は変更できません。",
  },
  "创建迁移任务时通过此名称选择连接。": {
    en: "Use this name to select the connection when creating tasks.",
    ja: "タスク作成時にこの名前で接続を選択します。",
  },
  隐藏访问凭据: {
    en: "Hide credentials",
    ja: "認証情報を隠す",
  },
  显示访问凭据: {
    en: "Show credentials",
    ja: "認証情報を表示",
  },
  签名版本: {
    en: "Signature version",
    ja: "署名バージョン",
  },
  使用服务端默认值: {
    en: "Use server default",
    ja: "サーバーのデフォルトを使用",
  },
  寻址方式: {
    en: "Addressing style",
    ja: "アドレス形式",
  },
  保存时验证访问权限: {
    en: "Validate access when saving",
    ja: "保存時にアクセス権を確認",
  },
  "凭据通过当前控制台连接提交至服务端，并同步到执行节点。": {
    en: "Credentials are sent through this console connection to the server and synced to workers.",
    ja: "認証情報はこのコンソール接続経由でサーバーに送信され、実行ノードに同期されます。",
  },
  正在保存: {
    en: "Saving",
    ja: "保存中",
  },
  保存连接: {
    en: "Save connection",
    ja: "接続を保存",
  },
  删除存储连接: {
    en: "Delete storage connection",
    ja: "ストレージ接続を削除",
  },
  "删除连接 {alias} 后，后续迁移将无法使用此连接。此操作会同步到执行节点。": {
    en: "After deleting {alias}, future transfers cannot use this connection. The change is synced to workers.",
    ja: "{alias} を削除すると、以後の転送でこの接続を使用できなくなります。変更は実行ノードに同期されます。",
  },
  "连接测试 · {alias}": {
    en: "Connection test · {alias}",
    ja: "接続テスト · {alias}",
  },
  "正在获取可访问的存储桶…": {
    en: "Listing accessible buckets…",
    ja: "アクセス可能なバケットを取得中…",
  },
  "已获取 {count} 个存储桶。": {
    en: "Found {count} buckets.",
    ja: "{count} 個のバケットを取得しました。",
  },
  "此测试仅验证列举存储桶的权限。存储桶读写权限在迁移时验证。": {
    en: "This tests bucket listing only. Read and write permissions are checked during transfers.",
    ja: "このテストはバケット一覧の取得権限のみを確認します。読み書き権限は転送時に確認します。",
  },
  关闭: {
    en: "Close",
    ja: "閉じる",
  },
  "无法连接主节点，请重试。": {
    en: "Cannot connect to the master. Try again.",
    ja: "マスターに接続できません。再試行してください。",
  },
  "主节点返回了无效响应，请重试。": {
    en: "The master returned an invalid response. Try again.",
    ja: "マスターから無効な応答が返されました。再試行してください。",
  },
  "初始化请求失败，请重试。": {
    en: "Setup request failed. Try again.",
    ja: "初期設定の要求に失敗しました。再試行してください。",
  },
  "初始化状态不完整，请重试。": {
    en: "Setup status is incomplete. Try again.",
    ja: "初期設定の状態が不完全です。再試行してください。",
  },
  "读取初始化状态超时，请重试。": {
    en: "Reading setup status timed out. Try again.",
    ja: "初期設定の状態取得がタイムアウトしました。再試行してください。",
  },
  "无法读取初始化状态。": {
    en: "Cannot read setup status.",
    ja: "初期設定の状態を取得できません。",
  },
  "请输入用户名。": {
    en: "Enter a username.",
    ja: "ユーザー名を入力してください。",
  },
  "请输入密码。": {
    en: "Enter a password.",
    ja: "パスワードを入力してください。",
  },
  "请再次输入密码。": {
    en: "Enter the password again.",
    ja: "パスワードを再入力してください。",
  },
  "两次输入的密码不一致。": {
    en: "Passwords do not match.",
    ja: "パスワードが一致しません。",
  },
  "请输入完整的 HTTP 或 HTTPS 地址，不含路径。": {
    en: "Enter a full HTTP or HTTPS address without a path.",
    ja: "パスを含まない完全な HTTP または HTTPS アドレスを入力してください。",
  },
  "请输入初始化密钥。": {
    en: "Enter the setup key.",
    ja: "初期設定キーを入力してください。",
  },
  "请求超时，请刷新页面确认初始化状态。": {
    en: "The request timed out. Refresh to check setup status.",
    ja: "要求がタイムアウトしました。ページを更新して初期設定の状態を確認してください。",
  },
  "初始化失败，请重试。": {
    en: "Setup failed. Try again.",
    ja: "初期設定に失敗しました。再試行してください。",
  },
  "初始化 OSS Transfer": {
    en: "Set up OSS Transfer",
    ja: "OSS Transfer の初期設定",
  },
  "正在读取配置…": {
    en: "Loading configuration…",
    ja: "設定を読み込み中…",
  },
  管理员: {
    en: "Administrator",
    ja: "管理者",
  },
  主节点: {
    en: "Master",
    ja: "マスター",
  },
  确认: {
    en: "Confirm",
    ja: "確認",
  },
  用户名: {
    en: "Username",
    ja: "ユーザー名",
  },
  密码: {
    en: "Password",
    ja: "パスワード",
  },
  隐藏密码: {
    en: "Hide password",
    ja: "パスワードを隠す",
  },
  显示密码: {
    en: "Show password",
    ja: "パスワードを表示",
  },
  确认密码: {
    en: "Confirm password",
    ja: "パスワードの確認",
  },
  主节点地址: {
    en: "Master URL",
    ja: "マスターの URL",
  },
  "浏览器和 Agent 使用此地址连接。": {
    en: "Browsers and agents use this address to connect.",
    ja: "ブラウザーと Agent はこのアドレスに接続します。",
  },
  初始化密钥: {
    en: "Setup key",
    ja: "初期設定キー",
  },
  "从主节点启动输出中复制。": {
    en: "Copy it from the master's startup output.",
    ja: "マスターの起動時出力からコピーしてください。",
  },
  "主节点不执行传输。登录后前往“执行节点”添加 Agent。": {
    en: "The master does not transfer files. After signing in, add an agent under Workers.",
    ja: "マスターは転送を実行しません。ログイン後、「実行ノード」で Agent を追加してください。",
  },
  上一步: {
    en: "Back",
    ja: "戻る",
  },
  "正在初始化…": {
    en: "Setting up…",
    ja: "初期設定中…",
  },
  完成并登录: {
    en: "Finish and sign in",
    ja: "完了してログイン",
  },
  下一步: {
    en: "Next",
    ja: "次へ",
  },
  "节点未返回状态，请检查节点服务和网络。": {
    en: "No status from this node. Check its service and network.",
    ja: "ノードの状態を取得できません。サービスとネットワークを確認してください。",
  },
  "请输入节点标识。": {
    en: "Enter a node ID.",
    ja: "ノード ID を入力してください。",
  },
  "节点状态已失效，请刷新后重新编辑根目录。": {
    en: "Node status is stale. Refresh before editing root directories.",
    ja: "ノードの状態が古いため、更新してからルートディレクトリを編集してください。",
  },
  "本地根目录不能重复。": {
    en: "Local root directories must be unique.",
    ja: "ローカルルートディレクトリを重複させることはできません。",
  },
  "节点设置已保存。": {
    en: "Node settings saved.",
    ja: "ノード設定を保存しました。",
  },
  "节点已添加。": {
    en: "Node added.",
    ja: "ノードを追加しました。",
  },
  "节点已移除。": {
    en: "Node removed.",
    ja: "ノードを削除しました。",
  },
  "请输入大于或等于 0 的数值。": {
    en: "Enter a number greater than or equal to 0.",
    ja: "0 以上の数値を入力してください。",
  },
  "限速设置已提交，正在刷新节点状态。": {
    en: "Limits submitted. Refreshing node status.",
    ja: "制限値を送信しました。ノードの状態を更新しています。",
  },
  执行节点: {
    en: "Workers",
    ja: "実行ノード",
  },
  "查看节点状态和速率。": {
    en: "View node status and transfer rates.",
    ja: "ノードの状態と転送速度を表示します。",
  },
  添加节点: {
    en: "Add node",
    ja: "ノードを追加",
  },
  调整限速: {
    en: "Adjust limits",
    ja: "制限を変更",
  },
  "节点配置读取失败：{error}": {
    en: "Failed to read node configuration: {error}",
    ja: "ノード設定の取得に失敗しました：{error}",
  },
  节点列表: {
    en: "Nodes",
    ja: "ノード一覧",
  },
  "{value1} / {value2} 个节点正常响应": {
    en: "{value1} / {value2} nodes responding",
    ja: "{value1} / {value2} ノードが応答",
  },
  正在读取节点状态: {
    en: "Reading node status",
    ja: "ノードの状態を取得中",
  },
  节点连接正常: {
    en: "Nodes connected",
    ja: "ノード接続正常",
  },
  部分节点不可达: {
    en: "Some nodes unreachable",
    ja: "一部のノードに接続できません",
  },
  尚未配置节点: {
    en: "No nodes configured",
    ja: "ノード未設定",
  },
  节点视图: {
    en: "Node view",
    ja: "ノード表示",
  },
  实时状态: {
    en: "Live status",
    ja: "現在の状態",
  },
  限速历史: {
    en: "Limit history",
    ja: "制限の変更履歴",
  },
  暂无执行节点: {
    en: "No workers",
    ja: "実行ノードなし",
  },
  "添加 Agent 后可查看节点状态。": {
    en: "Add an agent to view its status.",
    ja: "Agent を追加すると状態が表示されます。",
  },
  扫描并发: {
    en: "Scan concurrency",
    ja: "スキャン並列数",
  },
  子任务并发: { en: "Subtask concurrency", ja: "サブタスク並列数" },
  每个子任务的对象并发: {
    en: "Objects per subtask",
    ja: "サブタスクごとのオブジェクト並列数",
  },
  "保存后立即生效；调低并发不会中断正在传输的对象。": {
    en: "Changes apply immediately. Lowering concurrency lets active transfers finish.",
    ja: "保存するとすぐに適用されます。並列数を減らしても転送中のオブジェクトは完了まで続行します。",
  },
  "子任务并发为 1–64，对象并发为 1–32，两者乘积不能超过 256。": {
    en: "Use 1–64 subtasks and 1–32 objects per subtask, with a combined limit of 256 objects.",
    ja: "サブタスクは 1–64、各サブタスクのオブジェクトは 1–32、両者の積は 256 以下にしてください。",
  },
  "升级节点后可调整并发。": {
    en: "Update this node to adjust concurrency.",
    ja: "ノードを更新すると並列数を変更できます。",
  },
  "节点离线时无法调整并发。": {
    en: "Concurrency cannot be changed while the node is offline.",
    ja: "オフラインのノードでは並列数を変更できません。",
  },
  未完成子任务: {
    en: "Unfinished subtasks",
    ja: "未完了のサブタスク",
  },
  "执行节点 {value1}": {
    en: "Worker {value1}",
    ja: "実行ノード {value1}",
  },
  "主节点 · 执行节点": {
    en: "Master · Worker",
    ja: "マスター · 実行ノード",
  },
  "最后连接：": {
    en: "Last connected: ",
    ja: "最終接続：",
  },
  在线: {
    en: "Online",
    ja: "オンライン",
  },
  离线: {
    en: "Offline",
    ja: "オフライン",
  },
  无法连接: {
    en: "Unreachable",
    ja: "接続不可",
  },
  编辑节点: {
    en: "Edit node",
    ja: "ノードを編集",
  },
  移除节点: {
    en: "Remove node",
    ja: "ノードを削除",
  },
  "节点未返回有效状态，请检查节点服务和网络。": {
    en: "No valid node status. Check its service and network.",
    ja: "有効なノード状態を取得できません。サービスとネットワークを確認してください。",
  },
  执行负载: {
    en: "Workload",
    ja: "実行負荷",
  },
  队列占用: {
    en: "Queue usage",
    ja: "キュー使用量",
  },
  "{value1}队列占用": {
    en: "{value1} queue usage",
    ja: "{value1}のキュー使用量",
  },
  本地根目录: {
    en: "Local root directories",
    ja: "ローカルルートディレクトリ",
  },
  未设置: {
    en: "Not set",
    ja: "未設定",
  },
  "限速历史不完整。": {
    en: "Limit history is incomplete.",
    ja: "制限の変更履歴が不完全です。",
  },
  "未获取到节点 {value1} 的记录。": {
    en: "No records from nodes {value1}.",
    ja: "ノード {value1} の履歴を取得できません。",
  },
  "部分节点未返回记录。": {
    en: "Some nodes returned no records.",
    ja: "一部のノードから履歴を取得できません。",
  },
  "显示各节点最近 50 条记录。自动降速与恢复改变运行上限，不覆盖已保存目标。旧记录未保存的调整前值和原因显示为未知。":
    {
      en: "Shows the latest 50 records per node. Automatic reductions and recovery change the active limit, not the saved target. Missing values and reasons in older records are shown as unknown.",
      ja: "各ノードの最新 50 件を表示します。自動制限と回復は実行中の上限を変更し、保存済みの目標値は変更しません。旧レコードにない変更前の値と理由は不明と表示します。",
    },
  时间: {
    en: "Time",
    ja: "時刻",
  },
  类型: {
    en: "Type",
    ja: "種類",
  },
  运行上限变化: {
    en: "Active limit change",
    ja: "実行上限の変更",
  },
  阶段: {
    en: "Stage",
    ja: "段階",
  },
  依据: {
    en: "Reason",
    ja: "理由",
  },
  详情: {
    en: "Details",
    ja: "詳細",
  },
  "当时目标：": {
    en: "Target at the time: ",
    ja: "変更時の目標値：",
  },
  查看详情: {
    en: "View details",
    ja: "詳細を表示",
  },
  未记录详情: {
    en: "No details recorded",
    ja: "詳細の記録なし",
  },
  已响应节点暂无调整记录: {
    en: "No adjustments on responding nodes",
    ja: "応答したノードに変更履歴なし",
  },
  暂无调整记录: {
    en: "No adjustments",
    ja: "変更履歴なし",
  },
  "其余节点的历史尚未获取，无法确认是否发生调节。": {
    en: "History from other nodes is unavailable; their adjustments are unknown.",
    ja: "その他のノードの履歴を取得できないため、変更の有無は不明です。",
  },
  "限速调整记录生成后自动显示。": {
    en: "Limit adjustments appear here when recorded.",
    ja: "制限が変更されると、ここに履歴が表示されます。",
  },
  节点标识: {
    en: "Node ID",
    ja: "ノード ID",
  },
  节点地址: {
    en: "Node address",
    ja: "ノードアドレス",
  },
  使用集群配置: {
    en: "Use cluster configuration",
    ja: "クラスター設定を使用",
  },
  "服务端口：{value1}": {
    en: "Service port: {value1}",
    ja: "サービスポート：{value1}",
  },
  节点名称: {
    en: "Node name",
    ja: "ノード名",
  },
  "节点未返回根目录配置，暂时不能编辑。": {
    en: "Root directory settings are unavailable and cannot be edited yet.",
    ja: "ルートディレクトリ設定を取得できないため、現在は編集できません。",
  },
  "每行一个绝对路径。用于路径选择提示，不限制节点的文件访问权限。": {
    en: "One absolute path per line. These are path suggestions and do not restrict file access.",
    ja: "絶対パスを 1 行に 1 つ入力します。パス選択の候補として使用され、ファイルアクセスは制限しません。",
  },
  "正在保存…": {
    en: "Saving…",
    ja: "保存中…",
  },
  保存节点: {
    en: "Save node",
    ja: "ノードを保存",
  },
  "移除节点 {node}": {
    en: "Remove node {node}",
    ja: "ノード {node} を削除",
  },
  "移除此执行节点，不删除节点上的文件、对象或数据库。": {
    en: "Remove this worker from the cluster. Its files, objects and database are kept.",
    ja: "この実行ノードをクラスターから削除します。ノード上のファイル、オブジェクト、データベースは残ります。",
  },
  "节点不可达、仍有任务记录或集群正在执行任务时，无法移除。": {
    en: "Removal is blocked if the node is unreachable, holds task records, or the cluster has running tasks.",
    ja: "ノードに接続できない場合、タスクレコードが残る場合、またはクラスターに実行中のタスクがある場合は削除できません。",
  },
  "正在检查…": {
    en: "Checking…",
    ja: "確認中…",
  },
  确认移除: {
    en: "Remove",
    ja: "削除",
  },
  调整传输限速: {
    en: "Adjust transfer limits",
    ja: "転送制限を変更",
  },
  应用范围: {
    en: "Apply to",
    ja: "適用先",
  },
  全部执行节点: {
    en: "All workers",
    ja: "すべての実行ノード",
  },
  限制类型: {
    en: "Limit type",
    ja: "制限の種類",
  },
  "带宽上限（Mbps）": {
    en: "Bandwidth limit (Mbps)",
    ja: "帯域制限（Mbps）",
  },
  "请求上限（次/秒）": {
    en: "Request limit (requests/s)",
    ja: "リクエスト上限（回/秒）",
  },
  "修改目标上限，0 表示不限速。保存后立即应用，节点重启后仍生效；临时降速不覆盖该目标。":
    {
      en: "Set the target limit; 0 means unlimited. It applies immediately and survives node restarts. Temporary reductions keep this target.",
      ja: "目標上限を設定します。0 は無制限です。保存後すぐに適用され、再起動後も保持されます。一時的な制限では目標値を変更しません。",
    },
  "设置逐一应用于全部执行节点，各节点使用相同上限。": {
    en: "The same limit is applied to each worker.",
    ja: "各実行ノードに同じ上限を適用します。",
  },
  应用设置: {
    en: "Apply settings",
    ja: "設定を適用",
  },
  "请手动复制。": {
    en: "Copy the text manually.",
    ja: "テキストを手動でコピーしてください。",
  },
  "加入指令不完整，请重新生成。": {
    en: "Join commands are incomplete. Generate them again.",
    ja: "参加コマンドが不完全です。再生成してください。",
  },
  "已复制。": {
    en: "Copied.",
    ja: "コピーしました。",
  },
  "复制失败，请选择文本后手动复制。": {
    en: "Copy failed. Select the text and copy it manually.",
    ja: "コピーに失敗しました。テキストを選択して手動でコピーしてください。",
  },
  传统部署: {
    en: "Native installation",
    ja: "ネイティブインストール",
  },
  直接运行: {
    en: "Run directly",
    ja: "直接実行",
  },
  "节点标识（可选）": {
    en: "Node ID (optional)",
    ja: "ノード ID（任意）",
  },
  "留空时由 Agent 自动识别。": {
    en: "Leave blank for the agent to detect it.",
    ja: "空欄の場合は Agent が自動で識別します。",
  },
  "在新节点执行以下任一指令，Agent 将自动加入集群。": {
    en: "Run one of these commands on the new node to join the cluster.",
    ja: "新しいノードで以下のいずれかのコマンドを実行すると、クラスターに参加します。",
  },
  加入链接: {
    en: "Join link",
    ja: "参加リンク",
  },
  复制链接: {
    en: "Copy link",
    ja: "リンクをコピー",
  },
  "Agent 部署方式": {
    en: "Agent installation method",
    ja: "Agent のインストール方法",
  },
  "访问本地文件前，请将所需目录挂载到容器。": {
    en: "Mount the required directories into the container before accessing local files.",
    ja: "ローカルファイルにアクセスするには、必要なディレクトリをコンテナーにマウントしてください。",
  },
  "{method} 指令": {
    en: "{method} command",
    ja: "{method} コマンド",
  },
  加入: {
    en: "Join",
    ja: "参加",
  },
  复制指令: {
    en: "Copy command",
    ja: "コマンドをコピー",
  },
  "加入凭证仅可使用一次，有效至 {expiry}。": {
    en: "This join credential is single-use and expires at {expiry}.",
    ja: "参加用の認証情報は 1 回限り有効です。有効期限：{expiry}。",
  },
  "加入凭证仅可使用一次，30 分钟内有效。": {
    en: "This join credential is single-use and expires in 30 minutes.",
    ja: "参加用の認証情報は 1 回限り、30 分間有効です。",
  },
  "正在生成…": {
    en: "Generating…",
    ja: "生成中…",
  },
  生成加入指令: {
    en: "Generate join commands",
    ja: "参加コマンドを生成",
  },
  不限速: {
    en: "Unlimited",
    ja: "無制限",
  },
  "{value1} 次/秒": {
    en: "{value1} requests/s",
    ja: "{value1} 回/秒",
  },
  手动设置: {
    en: "Manual change",
    ja: "手動設定",
  },
  自动降速: {
    en: "Automatically reduced",
    ja: "自動制限中",
  },
  逐步恢复: {
    en: "Recovering gradually",
    ja: "段階的に回復中",
  },
  已恢复目标: {
    en: "Target restored",
    ja: "目標値まで回復",
  },
  自动调整: {
    en: "Automatic adjustment",
    ja: "自動調整",
  },
  "用户修改上限，保存后立即生效。": {
    en: "Limit changed by a user and applied on save.",
    ja: "ユーザーが上限を変更し、保存時に適用しました。",
  },
  "OSS 返回 HTTP 503；计数 {value1}，阈值 {value2}。": {
    en: "OSS returned HTTP 503; count {value1}, threshold {value2}.",
    ja: "OSS が HTTP 503 を返しました。件数 {value1}、しきい値 {value2}。",
  },
  "OSS 返回 HTTP 503，触发节点降速。": {
    en: "OSS returned HTTP 503, triggering a node limit reduction.",
    ja: "OSS の HTTP 503 応答により、ノードの上限を引き下げました。",
  },
  "定时恢复，提高运行上限；仍低于目标。": {
    en: "Scheduled recovery increased the active limit, which remains below target.",
    ja: "定期的な回復で実行上限を引き上げました。目標値にはまだ達していません。",
  },
  "定时恢复，运行上限已达到目标。": {
    en: "Scheduled recovery restored the active limit to its target.",
    ja: "定期的な回復により、実行上限が目標値に達しました。",
  },
  "自动调整；未记录更具体的原因。": {
    en: "Automatic adjustment; no further reason recorded.",
    ja: "自動調整。詳細な理由の記録はありません。",
  },
  "历史记录未保存调整原因。": {
    en: "No adjustment reason was saved in this record.",
    ja: "この履歴には調整理由が保存されていません。",
  },
  请求与带宽: {
    en: "Requests and bandwidth",
    ja: "リクエストと帯域",
  },
  "自动刷新已暂停。以下状态为最近一次读取结果。": {
    en: "Auto-refresh is paused. Showing the last retrieved status.",
    ja: "自動更新は停止中です。最後に取得した状態を表示しています。",
  },
  "节点状态未更新。以下为上次读取值，当前限速与恢复进度待确认。": {
    en: "Node status is stale. Current limits and recovery progress are unconfirmed.",
    ja: "ノードの状態が更新されていません。現在の上限と回復状況は未確認です。",
  },
  "节点可能遭遇限速，已自动调整运行上限。": {
    en: "The node may be throttled. Its active limits were adjusted automatically.",
    ja: "ノードが制限を受けた可能性があるため、実行上限を自動調整しました。",
  },
  状态待确认: {
    en: "Status unconfirmed",
    ja: "状態未確認",
  },
  恢复中: {
    en: "Recovering",
    ja: "回復中",
  },
  按目标运行: {
    en: "At target limit",
    ja: "目標上限で実行中",
  },
  "调整 {value1} 的 {value2} 限速": {
    en: "Adjust {value2} limit on {value1}",
    ja: "{value1} の {value2} 制限を変更",
  },
  调整: {
    en: "Adjust",
    ja: "変更",
  },
  运行上限: {
    en: "Active limit",
    ja: "実行上限",
  },
  配置目标: {
    en: "Configured target",
    ja: "設定ファイルの目標値",
  },
  已保存目标: {
    en: "Saved target",
    ja: "保存済みの目標値",
  },
  目标上限: {
    en: "Target limit",
    ja: "目標上限",
  },
  实测速率: {
    en: "Measured rate",
    ja: "実測速度",
  },
  "运行上限 / 目标上限": {
    en: "Active limit / target limit",
    ja: "実行上限 / 目標上限",
  },
  "{value1}运行上限占目标比例": {
    en: "{value1} active limit as a share of target",
    ja: "{value1} の目標値に対する実行上限の割合",
  },
  "{value1}%，运行上限占目标上限比例": {
    en: "{value1}% of target limit",
    ja: "目標上限の {value1}%",
  },
  "LIST 请求": {
    en: "LIST requests",
    ja: "LIST リクエスト",
  },
  "PUT 请求": {
    en: "PUT requests",
    ja: "PUT リクエスト",
  },
  "HEAD 请求": {
    en: "HEAD requests",
    ja: "HEAD リクエスト",
  },
  传输带宽: {
    en: "Transfer bandwidth",
    ja: "転送帯域",
  },
  等待分配: {
    en: "Awaiting assignment",
    ja: "割り当て待ち",
  },
  当前速度: {
    en: "Current speed",
    ja: "現在の速度",
  },
  平均速度: {
    en: "Average speed",
    ja: "平均速度",
  },
  创建时间: {
    en: "Created",
    ja: "作成日時",
  },
  开始时间: {
    en: "Started",
    ja: "開始日時",
  },
  结束时间: {
    en: "Finished",
    ja: "終了日時",
  },
  耗时: {
    en: "Elapsed",
    ja: "経過時間",
  },
  "起始 Key": {
    en: "Start key",
    ja: "開始キー",
  },
  "结束 Key": {
    en: "End key",
    ja: "終了キー",
  },
  源前缀: {
    en: "Source prefix",
    ja: "転送元プレフィックス",
  },
  目标相对路径: {
    en: "Destination relative path",
    ja: "転送先の相対パス",
  },
  目标命名: {
    en: "Destination naming",
    ja: "転送先の命名",
  },
  目标前缀: {
    en: "Destination prefix",
    ja: "転送先プレフィックス",
  },
  修改时间晚于: {
    en: "Modified after",
    ja: "更新日時の下限",
  },
  修改时间早于: {
    en: "Modified before",
    ja: "更新日時の上限",
  },
  冲突策略: {
    en: "Conflict policy",
    ja: "競合ポリシー",
  },
  返回主任务: {
    en: "Back to parent task",
    ja: "親タスクに戻る",
  },
  关闭详情: {
    en: "Close details",
    ja: "詳細を閉じる",
  },
  "子任务 #{id}": {
    en: "Subtask #{id}",
    ja: "サブタスク #{id}",
  },
  "子任务信息不完整，请刷新或检查执行节点。": {
    en: "Subtask information is incomplete. Refresh or check the worker.",
    ja: "サブタスクの情報が不完全です。更新するか、実行ノードを確認してください。",
  },
  "当前节点未返回此子任务，请刷新后重试。": {
    en: "The node did not return this subtask. Refresh and try again.",
    ja: "ノードからこのサブタスクを取得できません。更新して再試行してください。",
  },
  "未找到此子任务。": {
    en: "Subtask not found.",
    ja: "サブタスクが見つかりません。",
  },
  来源: {
    en: "Source",
    ja: "転送元",
  },
  目标: {
    en: "Destination",
    ja: "転送先",
  },
  传输统计: {
    en: "Transfer statistics",
    ja: "転送統計",
  },
  已扫描: {
    en: "Scanned",
    ja: "スキャン済み",
  },
  已迁移: {
    en: "Transferred",
    ja: "転送済み",
  },
  已跳过: {
    en: "Skipped",
    ja: "スキップ済み",
  },
  失败对象: {
    en: "Failed objects",
    ja: "失敗したオブジェクト",
  },
  执行信息: {
    en: "Execution details",
    ja: "実行情報",
  },
  "限速设置响应无效，请重试。": {
    en: "Invalid limit settings response. Try again.",
    ja: "制限設定の応答が無効です。再試行してください。",
  },
  "读取失败，请重试。": {
    en: "Read failed. Try again.",
    ja: "取得に失敗しました。再試行してください。",
  },
  "已清除任务带宽上限。": {
    en: "Task bandwidth limit cleared.",
    ja: "タスクの帯域制限を解除しました。",
  },
  "任务带宽上限已更新。": {
    en: "Task bandwidth limit updated.",
    ja: "タスクの帯域制限を更新しました。",
  },
  "正在核对应用结果…": {
    en: "Checking whether the limit was applied…",
    ja: "制限の適用結果を確認中…",
  },
  "已核对：任务带宽上限已清除。": {
    en: "Confirmed: task bandwidth limit cleared.",
    ja: "確認済み：タスクの帯域制限は解除されています。",
  },
  "已核对：任务带宽上限已生效。": {
    en: "Confirmed: task bandwidth limit applied.",
    ja: "確認済み：タスクの帯域制限は適用されています。",
  },
  "请核对当前设置后重试。": {
    en: "Check the current settings and try again.",
    ja: "現在の設定を確認して再試行してください。",
  },
  "应用结果未确认：{value1}": {
    en: "Application unconfirmed: {value1}",
    ja: "適用結果を確認できません：{value1}",
  },
  部分节点未生效: {
    en: "Not applied on all nodes",
    ja: "一部のノードで未適用",
  },
  "正在同步限速设置…": {
    en: "Synchronizing rate limits…",
    ja: "制限設定を同期中…",
  },
  未生效: {
    en: "Not applied",
    ja: "未適用",
  },
  "读取限速设置…": {
    en: "Reading limit settings…",
    ja: "制限設定を取得中…",
  },
  "带宽上限 {limit}": {
    en: "Bandwidth limit {limit}",
    ja: "帯域制限 {limit}",
  },
  已完成任务不可调整限速: {
    en: "Limits cannot be changed for completed tasks",
    ja: "完了したタスクの制限は変更できません",
  },
  当前任务不可调整限速: {
    en: "Limits cannot be changed for this task",
    ja: "このタスクの制限は変更できません",
  },
  限速: {
    en: "Set limit",
    ja: "制限を設定",
  },
  重试读取任务限速: {
    en: "Retry reading task limits",
    ja: "タスクの制限を再取得",
  },
  "无法读取任务限速：{error}": {
    en: "Cannot read task limits: {error}",
    ja: "タスクの制限を取得できません：{error}",
  },
  "任务 #{id} 限速": {
    en: "Task #{id} limits",
    ja: "タスク #{id} の制限",
  },
  "{count} 个子任务共享此上限。": {
    en: "{count} subtasks share this limit.",
    ja: "{count} 件のサブタスクでこの上限を共有します。",
  },
  正在应用: {
    en: "Applying",
    ja: "適用中",
  },
  重试应用: {
    en: "Retry applying",
    ja: "再適用",
  },
  应用: {
    en: "Apply",
    ja: "適用",
  },
  任务带宽上限: {
    en: "Task bandwidth limit",
    ja: "タスクの帯域上限",
  },
  "留空时仅使用节点限速。": {
    en: "Leave blank to use node limits only.",
    ja: "空欄の場合はノードの制限のみを使用します。",
  },
  对象大小: {
    en: "Object size",
    ja: "オブジェクトサイズ",
  },
  最后修改时间: {
    en: "Last modified",
    ja: "最終更新日時",
  },
  存储类型: {
    en: "Storage class",
    ja: "ストレージクラス",
  },
  生命周期过期时间: {
    en: "Lifecycle expiration",
    ja: "ライフサイクル有効期限",
  },
  "HTTP 过期时间": {
    en: "HTTP expiration",
    ja: "HTTP 有効期限",
  },
  对象扫描: {
    en: "Object scanning",
    ja: "オブジェクトスキャン",
  },
  对象传输: {
    en: "Object transfer",
    ja: "オブジェクト転送",
  },
  进度上报: {
    en: "Progress updates",
    ja: "進捗報告",
  },
  失败记录: {
    en: "Failure records",
    ja: "失敗レコード",
  },
  降级记录: {
    en: "Fallback records",
    ja: "フォールバックレコード",
  },
  部分节点删除失败: {
    en: "Deletion failed on some nodes",
    ja: "一部のノードで削除に失敗しました",
  },
  部分节点更新失败: {
    en: "Update failed on some nodes",
    ja: "一部のノードで更新に失敗しました",
  },
  部分节点添加失败: {
    en: "Addition failed on some nodes",
    ja: "一部のノードで追加に失敗しました",
  },
  连接删除完成: { en: "Connection deleted", ja: "接続を削除しました" },
  连接更新完成: { en: "Connection updated", ja: "接続を更新しました" },
  连接添加完成: { en: "Connection added", ja: "接続を追加しました" },
  "最后连接：{time}": { en: "Last connected: {time}", ja: "最終接続：{time}" },
  "当时目标：{limit}": {
    en: "Target at the time: {limit}",
    ja: "変更時の目標値：{limit}",
  },
  "以下节点未返回回收站数据，当前列表可能不完整：{nodes}": {
    en: "The recycle bin may be incomplete. No data from: {nodes}",
    ja: "次のノードからデータを取得できないため、ごみ箱の一覧は不完全な可能性があります：{nodes}",
  },
  "限速历史不完整。未获取到节点 {nodes} 的记录。": {
    en: "Limit history is incomplete. No records from nodes {nodes}.",
    ja: "制限の変更履歴が不完全です。ノード {nodes} の履歴を取得できません。",
  },
  "限速历史不完整。部分节点未返回记录。": {
    en: "Limit history is incomplete. Some nodes returned no records.",
    ja: "制限の変更履歴が不完全です。一部のノードから履歴を取得できません。",
  },
};
