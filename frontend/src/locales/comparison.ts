export const comparisonMessages: Record<string, { en: string; ja: string }> = {
  元数据: { en: "Metadata", ja: "メタデータ" },
  结果: { en: "Results", ja: "結果" },
  "加载中…": { en: "Loading…", ja: "読み込み中…" },
  "仅保留前 {count} 条差异，全部对象仍计入统计。请缩小前缀范围查看其余差异。": {
    en: "Only the first {count} differences are retained; all objects are counted. Narrow the prefixes to inspect the remaining differences.",
    ja: "差分は先頭 {count} 件まで保存します。集計は全件を含みます。残りの差分を確認するにはプレフィックスの範囲を絞ってください。",
  },
  删除比对结果: { en: "Delete comparison results", ja: "比較結果を削除" },
  "每个节点保留最近 10 次比对结果。": {
    en: "Each node retains its 10 most recent comparisons.",
    ja: "各ノードは直近 10 回の比較結果を保持します。",
  },
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
  "元数据（快速）": {
    en: "Metadata (quick)",
    ja: "メタデータ（高速）",
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
  "只检查对象大小、Content-Type、Content-Encoding、Content-Disposition、Cache-Control 和自定义元数据，不读取对象内容。ETag、修改时间和存储类型不作为一致依据。":
    {
      en: "Checks size, Content-Type, Content-Encoding, Content-Disposition, Cache-Control and custom metadata without reading content. ETag, modification time and storage class do not determine equality.",
      ja: "サイズ、Content-Type、Content-Encoding、Content-Disposition、Cache-Control、カスタムメタデータを確認し、内容は読み取りません。ETag、更新日時、ストレージクラスは一致判定に使用しません。",
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
  加载更多差异: {
    en: "Load more differences",
    ja: "差分をさらに読み込む",
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
};
