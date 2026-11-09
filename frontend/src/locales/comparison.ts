export const comparisonMessages: Record<string, { en: string; ja: string }> = {
  元数据: { en: "Metadata", ja: "メタデータ" },
  结果: { en: "Result", ja: "結果" },
  "加载中…": { en: "Loading…", ja: "読み込み中…" },
  删除比对结果: { en: "Delete comparison results", ja: "比較結果を削除" },
  "比对存储桶或对象，管理回收站和软件更新。": {
    en: "Compare buckets or objects, manage deleted tasks and software updates.",
    ja: "バケットやオブジェクトの比較、削除済みタスク、ソフトウェア更新を管理します。",
  },
  比对范围: {
    en: "Comparison scope",
    ja: "比較範囲",
  },
  "存储桶 / 前缀": {
    en: "Bucket / prefix",
    ja: "バケット / プレフィックス",
  },
  单个对象: {
    en: "Single object",
    ja: "単一オブジェクト",
  },
  比对方式: {
    en: "Comparison method",
    ja: "比較方式",
  },
  "元数据（不读取内容）": {
    en: "Metadata (does not read content)",
    ja: "メタデータ（内容を読み取らない）",
  },
  "比较大小、Content-Type、Content-Encoding、Content-Disposition、Cache-Control 和自定义元数据，不读取对象内容。元数据一致不代表内容一致：两端 ETag 都是普通 MD5 形式且不同时，结果标为“ETag 不同”。需要确认内容时使用内容 SHA-256 模式。":
    {
      en: "Compares size, Content-Type, Content-Encoding, Content-Disposition, Cache-Control and custom metadata without reading object content. Matching metadata does not mean matching content: when both ETags are plain MD5 values and differ, the result is “ETag differs”. Use content SHA-256 mode to confirm the content.",
      ja: "サイズ、Content-Type、Content-Encoding、Content-Disposition、Cache-Control、カスタムメタデータを比較し、オブジェクトの内容は読み取りません。メタデータが一致しても内容が一致するとは限りません。両側の ETag がどちらも通常の MD5 形式で異なる場合、結果は「ETag 不一致」になります。内容を確認するには内容 SHA-256 方式を使用してください。",
    },
  已取消: { en: "Canceled", ja: "キャンセル済み" },
  节点不可用: { en: "Node unavailable", ja: "ノード利用不可" },
  "ETag 不同": { en: "ETag differs", ja: "ETag 不一致" },
  大小: { en: "Size", ja: "サイズ" },
  "不同：{reasons}": { en: "Differs: {reasons}", ja: "相違：{reasons}" },
  全部差异: { en: "All differences", ja: "すべての差分" },
  按结果筛选: { en: "Filter by result", ja: "結果で絞り込み" },
  "{days} 天 {hours} 小时": {
    en: "{days} d {hours} h",
    ja: "{days} 日 {hours} 時間",
  },
  "{hours} 小时 {minutes} 分": {
    en: "{hours} h {minutes} min",
    ja: "{hours} 時間 {minutes} 分",
  },
  "{minutes} 分 {seconds} 秒": {
    en: "{minutes} min {seconds} s",
    ja: "{minutes} 分 {seconds} 秒",
  },
  "{seconds} 秒": { en: "{seconds} s", ja: "{seconds} 秒" },
  "已检查 {count}，{rate} 个/秒，已用 {elapsed}": {
    en: "Checked {count}, {rate} objects/s, elapsed {elapsed}",
    ja: "確認済み {count}、{rate} 件/秒、経過 {elapsed}",
  },
  "已检查 {count}，{rate} 个/秒，已用 {elapsed}，已读取 {bytes}": {
    en: "Checked {count}, {rate} objects/s, elapsed {elapsed}, read {bytes}",
    ja: "確認済み {count}、{rate} 件/秒、経過 {elapsed}、読み取り {bytes}",
  },
  "该节点版本最多保存前 {count} 条差异，其余差异只计数。升级该节点后重新比对可保存全部差异。":
    {
      en: "This node's version stores only the first {count} differences; later differences are only counted. Upgrade the node and compare again to store every difference.",
      ja: "このノードのバージョンは先頭 {count} 件の差分のみ保存し、以降の差分は件数のみ集計します。すべての差分を保存するには、ノードを更新してから再度比較してください。",
    },
  "差异明细超过节点的比对结果存储预算，之后的差异只计数未保存。最后保存的 Key：{key}":
    {
      en: "The difference details exceeded the node's storage budget for comparison results; later differences are counted but not stored. Last stored key: {key}",
      ja: "差分の明細がノードの比較結果用ストレージ予算を超えたため、以降の差分は件数のみ集計し、保存していません。最後に保存した Key：{key}",
    },
  "节点磁盘剩余空间不足，之后的差异只计数未保存。最后保存的 Key：{key}": {
    en: "The node is low on free disk space; later differences are counted but not stored. Last stored key: {key}",
    ja: "ノードのディスク空き容量が不足しているため、以降の差分は件数のみ集計し、保存していません。最後に保存した Key：{key}",
  },
  "部分差异明细未保存，之后的差异只计数。最后保存的 Key：{key}": {
    en: "Some difference details were not stored; later differences are only counted. Last stored key: {key}",
    ja: "一部の差分の明細を保存していません。以降の差分は件数のみ集計します。最後に保存した Key：{key}",
  },
  比对历史: { en: "Comparison history", ja: "比較履歴" },
  比对历史分页: {
    en: "Comparison history pages",
    ja: "比較履歴のページ切り替え",
  },
  "{count} 个节点暂时无法连接，这些节点上的比对未列出最新状态。": {
    en: "Cannot reach {count} nodes right now; comparisons there do not show their latest status.",
    ja: "{count} 台のノードに一時的に接続できません。これらのノードの比較は最新の状態を表示していません。",
  },
  "部分节点暂时无法连接，比对列表可能不完整。": {
    en: "Some nodes cannot be reached right now; the comparison list may be incomplete.",
    ja: "一部のノードに一時的に接続できません。比較の一覧が不完全な可能性があります。",
  },
  暂无比对记录: { en: "No comparisons yet", ja: "比較の記録はありません" },
  "源 → 目标": { en: "Source → target", ja: "転送元 → 転送先" },
  差异: { en: "Differences", ja: "差分" },
  "比对结果保存在执行节点上，默认保留 30 天；每个节点的比对结果默认最多占用 2 GiB，超出时先删除最早结束的比对。":
    {
      en: "Results are kept on the node that ran the comparison, for 30 days by default. By default each node keeps at most 2 GiB of comparison results and deletes the comparisons that ended earliest first when it exceeds that.",
      ja: "比較結果は実行ノードに保存され、既定で 30 日間保持されます。各ノードの比較結果は既定で最大 2 GiB までで、超えた場合は終了が最も早い比較から削除します。",
    },
  比对结果: { en: "Comparison results", ja: "比較結果" },
  "执行节点暂时无法连接，节点恢复后将自动加载比对结果。": {
    en: "The node that ran this comparison cannot be reached right now. The results load automatically once the node is back.",
    ja: "実行ノードに一時的に接続できません。ノードが復旧すると比較結果を自動的に読み込みます。",
  },
  "比对 ID": { en: "Comparison ID", ja: "比較 ID" },
  "比对结果不存在或已被删除。": {
    en: "The comparison results do not exist or have been deleted.",
    ja: "比較結果が存在しないか、削除されています。",
  },
  继续比对: { en: "Resume comparison", ja: "比較を再開" },
  "从已比对到的位置继续，已统计的结果保留。": {
    en: "Continues after the last compared object; results counted so far are kept.",
    ja: "比較済みの位置から再開し、集計済みの結果は保持します。",
  },
  "导出 ZIP": { en: "Export ZIP", ja: "ZIP をエクスポート" },
  比对结果导出: {
    en: "Comparison results export",
    ja: "比較結果のエクスポート",
  },
  "导出已请求，请查看浏览器下载列表。": {
    en: "Export requested. Check your browser downloads.",
    ja: "エクスポートを要求しました。ブラウザーのダウンロード一覧を確認してください。",
  },
  "比对结果导出失败：{message}": {
    en: "Comparison export failed: {message}",
    ja: "比較結果のエクスポートに失敗しました：{message}",
  },
  "导出票据无效，请重试。": {
    en: "Invalid export ticket. Please retry.",
    ja: "エクスポートのチケットが無効です。再試行してください。",
  },
  "导出失败，请重试。": {
    en: "Export failed. Please retry.",
    ja: "エクスポートに失敗しました。再試行してください。",
  },
  "服务未返回导出文件，请重试。": {
    en: "The server did not return an export file. Please retry.",
    ja: "サーバーからエクスポートファイルが返されませんでした。再試行してください。",
  },
  "无法读取导出结果，请检查浏览器下载列表或重试。": {
    en: "Unable to read the export result. Check your browser downloads or retry.",
    ja: "エクスポート結果を確認できません。ブラウザーのダウンロード一覧を確認するか、再試行してください。",
  },
  "删除比对结果？": {
    en: "Delete comparison results?",
    ja: "比較結果を削除しますか？",
  },
  "将从执行节点删除这次比对的统计和差异明细，无法恢复。源端和目标端的对象不受影响。":
    {
      en: "Deletes this comparison's counts and difference details from its node. This cannot be undone. Objects in the source and target are not affected.",
      ja: "この比較の集計と差分の明細を実行ノードから削除します。元に戻すことはできません。転送元と転送先のオブジェクトには影響しません。",
    },
  "比对结果已删除。": {
    en: "Comparison results deleted.",
    ja: "比較結果を削除しました。",
  },
  差异明细: { en: "Difference details", ja: "差分の明細" },
  差异明细分页: { en: "Difference pages", ja: "差分の明細のページ切り替え" },
  "正在查找符合条件的差异…": {
    en: "Searching for matching differences…",
    ja: "条件に一致する差分を検索しています…",
  },
  "已查找到第 {number} 条，暂未找到符合条件的差异。": {
    en: "Searched through difference {number} without finding a match yet.",
    ja: "{number} 件目まで検索しましたが、条件に一致する差分はまだ見つかっていません。",
  },
  继续查找: { en: "Keep searching", ja: "検索を続ける" },
  "没有符合条件的差异。": {
    en: "No matching differences.",
    ja: "条件に一致する差分はありません。",
  },
  "暂无已保存的差异明细。": {
    en: "No difference details are stored yet.",
    ja: "保存された差分の明細はまだありません。",
  },
  "暂未发现差异。": {
    en: "No differences found so far.",
    ja: "現在までに差分は見つかっていません。",
  },
  "未发现差异。": {
    en: "No differences found.",
    ja: "差分は見つかりませんでした。",
  },
  "内容 SHA-256（流式）": {
    en: "Content SHA-256 (streaming)",
    ja: "内容 SHA-256（ストリーム）",
  },
  源前缀: {
    en: "Source prefix",
    ja: "転送元プレフィックス",
  },
  "Bucket 填写连接名称/bucket。前缀留空比对整个存储桶，两端按去掉前缀后的 Key 对应。":
    {
      en: "Enter connection-name/bucket. Empty prefixes compare the entire bucket; keys are matched after removing each prefix.",
      ja: "Bucket は接続名/bucket で指定します。プレフィックスが空の場合はバケット全体を比較し、それぞれのプレフィックスを除いた Key で対応付けます。",
    },
  "完整读取两端对象流并计算 SHA-256，不保存对象文件到本地磁盘。会产生读取流量；大对象需要更多时间。":
    {
      en: "Streams both complete objects into SHA-256 without saving object files to disk. This uses read traffic; larger objects take longer.",
      ja: "両側の内容を最後までストリームで読み、SHA-256 を計算します。ローカルファイルは作成しません。読み取り通信が発生し、大きなオブジェクトは時間がかかります。",
    },
  取消比对: {
    en: "Cancel comparison",
    ja: "比較をキャンセル",
  },
  正在比对: {
    en: "Comparing",
    ja: "比較中",
  },
  正在取消: {
    en: "Canceling",
    ja: "キャンセル中",
  },
  比对已中断: {
    en: "Comparison interrupted",
    ja: "比較が中断されました",
  },
  比对失败: {
    en: "Comparison failed",
    ja: "比較に失敗しました",
  },
  比对完成: {
    en: "Comparison completed",
    ja: "比較が完了しました",
  },
  一致: {
    en: "Match",
    ja: "一致",
  },
  不一致: {
    en: "Different",
    ja: "不一致",
  },
  目标缺失: {
    en: "Missing from target",
    ja: "転送先に存在しない",
  },
  目标多余: {
    en: "Only on target",
    ja: "転送先のみに存在",
  },
  比对期间已变化: {
    en: "Changed during comparison",
    ja: "比較中に変更",
  },
  读取失败: {
    en: "Read failed",
    ja: "読み取り失敗",
  },
  两端均不存在: {
    en: "Missing on both sides",
    ja: "両側に存在しない",
  },
  比对进度: {
    en: "Comparison progress",
    ja: "比較の進行状況",
  },
  已检查: {
    en: "Checked",
    ja: "確認済み",
  },
  源大小: {
    en: "Source size",
    ja: "転送元サイズ",
  },
  目标大小: {
    en: "Target size",
    ja: "転送先サイズ",
  },
  "下方仅列出差异和未能确认的对象。结果反映比对时的状态；持续写入的存储桶不提供同一时刻的快照。":
    {
      en: "Only differences and unconfirmed objects are listed below. Results reflect the time of comparison; a bucket being written to does not provide a single point-in-time snapshot.",
      ja: "差分と確認できなかったオブジェクトのみ表示します。結果は比較時の状態です。書き込み中のバケットでは、同一時点のスナップショットにはなりません。",
    },
  "Cannot read object metadata": {
    en: "Cannot read object metadata",
    ja: "オブジェクトのメタデータを読み取れません",
  },
  "Cannot read target metadata": {
    en: "Cannot read target metadata",
    ja: "転送先のメタデータを読み取れません",
  },
  "Cannot read source metadata": {
    en: "Cannot read source metadata",
    ja: "転送元のメタデータを読み取れません",
  },
  "Cannot read complete object content": {
    en: "Cannot read complete object content",
    ja: "オブジェクトの内容を最後まで読み取れません",
  },
  "Comparison could not finish; check storage access and retry.": {
    en: "Comparison could not finish; check storage access and retry.",
    ja: "比較を完了できませんでした。ストレージへの接続を確認し、再実行してください。",
  },
  "Cannot save comparison results": {
    en: "Cannot save comparison results",
    ja: "比較結果を保存できません",
  },
  "Comparison results are unavailable on this node": {
    en: "Comparison results are unavailable on this node",
    ja: "このノードでは比較結果を利用できません",
  },
};
