export const tasksMessages: Record<string, { en: string; ja: string }> = {
  "无法完整读取任务列表，请刷新后重试。": {
    en: "Could not load the complete task list. Refresh and try again.",
    ja: "タスク一覧をすべて読み込めませんでした。更新して再試行してください。",
  },
  批量重试失败任务: { en: "Retry failed tasks", ja: "失敗タスクの一括再試行" },
  "所有失败和部分失败的任务，不受异常记录筛选条件影响。": {
    en: "All failed and partially failed tasks, regardless of the exception filters.",
    ja: "例外一覧のフィルターに関係なく、失敗・一部失敗の全タスクが対象です。",
  },
  "重试失败和未完成的对象，保留已完成的传输进度。": {
    en: "Retry failed and unfinished objects, keeping completed progress.",
    ja: "転送済みの進捗を保持し、失敗または未完了のオブジェクトを再試行します。",
  },
  "正在读取全部任务…": {
    en: "Loading all tasks…",
    ja: "すべてのタスクを読み込み中…",
  },
  "已处理 {processed} / {total}": {
    en: "Processed {processed} / {total}",
    ja: "処理済み {processed} / {total}",
  },
  "没有需要重试的失败任务。": {
    en: "No failed tasks need retrying.",
    ja: "再試行が必要な失敗タスクはありません。",
  },
  失败任务: { en: "Failed tasks", ja: "失敗タスク" },
  选择全部失败任务: {
    en: "Select all failed tasks",
    ja: "すべての失敗タスクを選択",
  },
  "选择任务 #{id}": { en: "Select task #{id}", ja: "タスク #{id} を選択" },
  结果待确认: { en: "Result unconfirmed", ja: "結果未確認" },
  已提交: { en: "Submitted", ja: "送信済み" },
  失败任务分页: { en: "Failed task pages", ja: "失敗タスクのページ切り替え" },
  "重试全部（{count}）": {
    en: "Retry all ({count})",
    ja: "すべて再試行（{count}）",
  },
  "重试所选（{count}）": {
    en: "Retry selected ({count})",
    ja: "選択項目を再試行（{count}）",
  },
  "任务 ID 必须是正整数。": {
    en: "Task ID must be a positive integer.",
    ja: "タスク ID は正の整数を指定してください。",
  },
  正在读取任务: { en: "Loading tasks", ja: "タスクを読み込み中" },
  批量重试: { en: "Batch retry", ja: "一括再試行" },
  恢复任务: { en: "Recover tasks", ja: "タスクを再開" },
  恢复: { en: "Recover", ja: "再開" },
  "重试 {count} 个任务的失败项？": {
    en: "Retry failed items in {count} tasks?",
    ja: "{count} 件のタスクの失敗項目を再試行しますか？",
  },
  "重试失败或未完成的对象，已完成的对象不重复传输。": {
    en: "Retry failed or unfinished objects. Completed objects are not transferred again.",
    ja: "失敗または未完了のオブジェクトを再試行します。完了済みのオブジェクトは再転送しません。",
  },
  "失败任务重试失败或未完成的对象；已停止任务继续扫描和传输。已完成的对象不重复传输。":
    {
      en: "Retry failed or unfinished objects in failed tasks, and resume scanning and transfer in stopped tasks. Completed objects are not transferred again.",
      ja: "失敗したタスクは失敗または未完了のオブジェクトを再試行し、停止中のタスクはスキャンと転送を再開します。完了済みのオブジェクトは再転送しません。",
    },
  "任务 #{taskId} 的继续传输请求已受理。": {
    en: "Resume accepted for task #{taskId}.",
    ja: "タスク #{taskId} の転送再開を受け付けました。",
  },
  "继续传输已受理 {value1}，忽略 {value2}，失败 {value3}{value4}": {
    en: "Resume: {value1} accepted, {value2} ignored, {value3} failed{value4}",
    ja: "転送再開：受理 {value1}件、無視 {value2}件、失敗 {value3}件{value4}",
  },
  任务操作: { en: "Task actions", ja: "タスク操作" },
  所属任务: { en: "Task", ja: "対象タスク" },
  "任务 #{id}": { en: "Task #{id}", ja: "タスク #{id}" },
  重试失败项: { en: "Retry failed items", ja: "失敗した項目を再試行" },
  "查看异常及重试结果，导出记录。": {
    en: "Review exceptions and retry results, and export records.",
    ja: "例外と再試行の結果を確認し、記録をエクスポートします。",
  },
  重试节点: { en: "Retry node", ja: "再試行ノード" },
  重试未完成: { en: "Retry incomplete", ja: "再試行未完了" },
  重试结果: { en: "Retry result", ja: "再試行の結果" },
  重试结果时间: { en: "Retry result time", ja: "再試行の結果日時" },
  重试错误: { en: "Retry error", ja: "再試行のエラー" },
  未重试: { en: "Not retried", ja: "未再試行" },
  重试中: { en: "Retrying", ja: "再試行中" },
  已恢复: { en: "Recovered", ja: "復旧済み" },
  再次失败: { en: "Failed again", ja: "再失敗" },
  "重试 {count} 次": { en: "{count} retries", ja: "再試行 {count} 回" },
  任务已完成: { en: "Task completed", ja: "タスク完了" },
  "未响应节点：{value1}": {
    en: "Unresponsive nodes: {value1}",
    ja: "応答していないノード：{value1}",
  },
  "无法确认任务 #{id} 的当前状态，请刷新后重试。": {
    en: "Cannot confirm the current state of task #{id}. Refresh before trying again.",
    ja: "タスク #{id} の現在の状態を確認できません。更新後に再試行してください。",
  },
  "已选 {count} 项": { en: "{count} selected", ja: "{count} 件選択中" },
  "等 {count} 个任务": {
    en: " and others ({count} tasks)",
    ja: " ほか、計 {count} 件",
  },
  批量操作进度: { en: "Batch operation progress", ja: "一括操作の進捗" },
  "已处理 {done} / {total}": {
    en: "Processed {done} / {total}",
    ja: "処理済み {done} / {total}",
  },
  "已受理 {accepted}，忽略 {ignored}，失败 {failed}": {
    en: "Accepted {accepted}, ignored {ignored}, failed {failed}",
    ja: "受付済み {accepted}、対象外 {ignored}、失敗 {failed}",
  },
  "待确认 {uncertain}，未提交 {unsubmitted}": {
    en: "Unconfirmed {uncertain}, not submitted {unsubmitted}",
    ja: "結果未確認 {uncertain}、未送信 {unsubmitted}",
  },
  "部分结果待确认，请刷新任务状态。": {
    en: "Some results are unconfirmed. Refresh task status before trying again.",
    ja: "一部の結果は未確認です。タスクの状態を更新してください。",
  },
  "所选任务正在处理，请勿重复提交。": {
    en: "The selected tasks are being processed. Do not submit them again.",
    ja: "選択したタスクは処理中です。再送信しないでください。",
  },
  分页: { en: "Pagination", ja: "ページ切り替え" },
  任务分页: { en: "Task pagination", ja: "タスクのページ切り替え" },
  "第 {page} 页": { en: "Page {page}", ja: "{page} ページ" },
  跳至页码: { en: "Go to page", ja: "移動先ページ" },
  跳转: { en: "Go", ja: "移動" },
  "检查状态…": { en: "Checking status…", ja: "状態を確認中…" },
  状态暂不可用: { en: "Status unavailable", ja: "状態を取得できません" },
  "处理中…": {
    en: "Processing…",
    ja: "処理中…",
  },
  "任务状态已更新，请查看任务详情。": {
    en: "The task status has changed. Open the task for details.",
    ja: "タスクの状態が変わりました。詳細を確認してください。",
  },
  "任务 #{taskId} 的重试已受理。": {
    en: "Retry accepted for task #{taskId}.",
    ja: "タスク #{taskId} の再試行を受け付けました。",
  },
  "请求超时，请刷新后查看最新状态。": {
    en: "The request timed out. Refresh to check the latest status.",
    ja: "リクエストがタイムアウトしました。更新して最新の状態を確認してください。",
  },
  "传输已中断，等待恢复。": {
    en: "Transfer interrupted; waiting to resume.",
    ja: "転送が中断されました。再開を待っています。",
  },
  立即恢复: {
    en: "Resume now",
    ja: "今すぐ再開",
  },
  "正在检查恢复条件…": {
    en: "Checking recovery conditions…",
    ja: "再開条件を確認中…",
  },
  "节点并发已满，有空位后会自动恢复。": {
    en: "The node is at its concurrency limit. Recovery will start when a slot is available.",
    ja: "ノードの同時実行数が上限に達しています。空きができると自動的に再開します。",
  },
  "子任务已恢复。": {
    en: "Subtask resumed.",
    ja: "子タスクを再開しました。",
  },
  "子任务已在执行。": {
    en: "The subtask is already running.",
    ja: "子タスクはすでに実行中です。",
  },
  "尚未确认恢复结果，请刷新子任务状态。": {
    en: "Recovery has not been confirmed. Refresh the subtask status.",
    ja: "再開結果を確認できませんでした。子タスクの状態を更新してください。",
  },
  均衡分发: {
    en: "Balanced",
    ja: "均等配分",
  },
  单节点执行: {
    en: "Single node",
    ja: "単一ノード実行",
  },
  亲和分发: {
    en: "Affinity",
    ja: "アフィニティ配分",
  },
  "对象存储 → 对象存储": {
    en: "Object storage → Object storage",
    ja: "オブジェクトストレージ → オブジェクトストレージ",
  },
  跨存储桶或云平台迁移: {
    en: "Transfer between buckets or cloud providers",
    ja: "バケット間またはクラウド間の転送",
  },
  "对象存储 → 本地": {
    en: "Object storage → Local",
    ja: "オブジェクトストレージ → ローカル",
  },
  下载至执行节点或共享存储: {
    en: "Download to an agent or shared storage",
    ja: "実行ノードまたは共有ストレージにダウンロード",
  },
  "本地 → 对象存储": {
    en: "Local → Object storage",
    ja: "ローカル → オブジェクトストレージ",
  },
  上传本地文件或目录: {
    en: "Upload local files or directories",
    ja: "ローカルファイルまたはディレクトリをアップロード",
  },
  "{value1}源和目标不能是同一存储桶，请选择其他目标存储桶。": {
    en: "{value1}Source and destination must be different buckets.",
    ja: "{value1}転送元と転送先には別のバケットを指定してください。",
  },
  源: {
    en: "Source",
    ja: "転送元",
  },
  目标: {
    en: "Destination",
    ja: "転送先",
  },
  "{value1}存储连接": {
    en: "{value1} connection",
    ja: "{value1}接続",
  },
  "{value1}存储桶": {
    en: "{value1} bucket",
    ja: "{value1}バケット",
  },
  输入存储桶名称: {
    en: "Enter a bucket name",
    ja: "バケット名を入力",
  },
  "输入存储桶名称，或获取列表后选择。": {
    en: "Enter a bucket name or fetch the list to select one.",
    ja: "バケット名を入力するか、一覧から選択してください。",
  },
  正在获取存储桶列表: {
    en: "Fetching buckets",
    ja: "バケット一覧を取得中",
  },
  获取存储桶列表: {
    en: "Fetch buckets",
    ja: "バケット一覧を取得",
  },
  个可选: {
    en: "available",
    ja: "件選択可能",
  },
  "无法获取存储桶列表：": {
    en: "Unable to fetch buckets: ",
    ja: "バケット一覧を取得できません：",
  },
  "。如有对应存储桶的访问权限，可直接填写名称。": {
    en: ". You can enter the bucket name directly if you have access.",
    ja: "。アクセス権限がある場合は、バケット名を直接入力できます。",
  },
  "结束时间必须晚于开始时间。": {
    en: "End time must be later than start time.",
    ja: "終了時刻は開始時刻より後にしてください。",
  },
  "请选择源存储连接并填写源存储桶。": {
    en: "Select a source connection and enter a source bucket.",
    ja: "転送元の接続を選択し、バケット名を入力してください。",
  },
  "请选择目标存储连接并填写目标存储桶。": {
    en: "Select a destination connection and enter a destination bucket.",
    ja: "転送先の接続を選択し、バケット名を入力してください。",
  },
  "请填写源对象前缀或对象 Key。": {
    en: "Enter a source prefix or object key.",
    ja: "転送元のプレフィックスまたはオブジェクトキーを入力してください。",
  },
  "请至少选择一个执行节点。": {
    en: "Select at least one agent.",
    ja: "実行ノードを1つ以上選択してください。",
  },
  "所选执行节点不可用，请刷新节点列表或重新选择。": {
    en: "A selected agent is unavailable. Refresh the list or select another agent.",
    ja: "選択した実行ノードは利用できません。一覧を更新するか、別のノードを選択してください。",
  },
  "请填写每个执行节点及其本地绝对路径。": {
    en: "Specify each agent and its absolute local path.",
    ja: "各実行ノードとローカルの絶対パスを指定してください。",
  },
  "请输入执行节点上的绝对路径。": {
    en: "Enter an absolute path on the agent.",
    ja: "実行ノード上の絶対パスを入力してください。",
  },
  "同一个执行节点只能绑定一次。": {
    en: "Each agent can only be bound once.",
    ja: "同じ実行ノードは1回のみ指定できます。",
  },
  "请填写目标对象 Key 或目标前缀。": {
    en: "Enter a destination object key or prefix.",
    ja: "転送先のオブジェクトキーまたはプレフィックスを入力してください。",
  },
  "请填写任务数据或读取数据文件。": {
    en: "Enter task data or load a data file.",
    ja: "タスクデータを入力するか、ファイルを読み込んでください。",
  },
  "第 {value1} 行格式错误，请使用“源连接/源桶,目标连接/目标桶”。": {
    en: "Invalid row {value1}. Use source-connection/source-bucket,destination-connection/destination-bucket.",
    ja: "{value1}行目の形式が不正です。「転送元接続/転送元バケット,転送先接続/転送先バケット」の形式で入力してください。",
  },
  "第 {value1} 行的覆盖选项必须是 true 或 false。": {
    en: "The overwrite option in row {value1} must be true or false.",
    ja: "{value1}行目の上書き設定は true または false にしてください。",
  },
  "第 {value1} 行的分发模式必须是 affinity 或 balanced。": {
    en: "The distribution mode in row {value1} must be affinity or balanced.",
    ja: "{value1}行目の配分方式は affinity または balanced にしてください。",
  },
  "第 {value1} 行超过 5 列，其他配置请使用 JSON 格式。": {
    en: "Row {value1} has more than 5 columns. Use JSON for additional options.",
    ja: "{value1}行目は5列を超えています。追加の設定には JSON を使用してください。",
  },
  "JSON 格式无效，请检查括号、双引号和逗号。": {
    en: "Invalid JSON. Check brackets, double quotes, and commas.",
    ja: "JSON が不正です。括弧、二重引用符、カンマを確認してください。",
  },
  "任务数据必须是非空数组。": {
    en: "Task data must be a non-empty array.",
    ja: "タスクデータは空でない配列にしてください。",
  },
  "第 {value1} 项必须是任务对象。": {
    en: "Item {value1} must be a task object.",
    ja: "{value1}番目の項目はタスクオブジェクトにしてください。",
  },
  "第 {value1} 项：{value2}": {
    en: "Item {value1}: {value2}",
    ja: "{value1}番目の項目：{value2}",
  },
  "第 {value1} 项的 direction 必须为 OSS_TO_LOCAL 或 LOCAL_TO_OSS，并提供 sourceEndpoint 和 destinationEndpoint。":
    {
      en: "Item {value1}: direction must be OSS_TO_LOCAL or LOCAL_TO_OSS, with sourceEndpoint and destinationEndpoint.",
      ja: "{value1}番目の項目：direction は OSS_TO_LOCAL または LOCAL_TO_OSS とし、sourceEndpoint と destinationEndpoint を指定してください。",
    },
  "第 {value1} 项必须填写 sourceBucket 和 destBucket。": {
    en: "Item {value1} requires sourceBucket and destBucket.",
    ja: "{value1}番目の項目には sourceBucket と destBucket が必要です。",
  },
  "第 {value1} 项涉及本地路径，请选择“本地迁移请求”。": {
    en: "Item {value1} contains a local path. Select “Local transfer requests”.",
    ja: "{value1}番目の項目にローカルパスが含まれています。「ローカル転送リクエスト」を選択してください。",
  },
  "第 {value1} 项：": {
    en: "Item {value1}: ",
    ja: "{value1}番目の項目：",
  },
  "单文件上传请只保留一个执行节点。": {
    en: "Use a single agent for single-file uploads.",
    ja: "単一ファイルのアップロードには実行ノードを1つだけ指定してください。",
  },
  "源路径是目录。请选择“保留源文件名 / 相对路径”或“添加目标前缀”；指定对象 Key 仅适用于单文件。":
    {
      en: "The source is a directory. Select “Keep filename / relative path” or “Add destination prefix”. An exact key is only supported for a single file.",
      ja: "転送元はディレクトリです。「ファイル名 / 相対パスを維持」または「転送先プレフィックスを追加」を選択してください。キーの直接指定は単一ファイルのみ対応しています。",
    },
  任务配置无效: {
    en: "Invalid task configuration",
    ja: "タスク設定が不正です",
  },
  所有节点预检通过: {
    en: "All agents passed preflight",
    ja: "すべてのノードが事前チェックに合格しました",
  },
  "预检完成，存在未通过的节点。": {
    en: "Preflight finished. Some agents failed the check.",
    ja: "事前チェックが完了しました。一部のノードで失敗しました。",
  },
  "节点预检未通过，请检查节点和源路径。": {
    en: "Preflight failed. Check the agents and source paths.",
    ja: "事前チェックに失敗しました。実行ノードと転送元パスを確認してください。",
  },
  "指定目标对象 Key 时，各节点的源路径必须是可访问的单个文件。目录迁移请选择“添加目标前缀”或“保留源文件名 / 相对路径”。":
    {
      en: "An exact destination key requires an accessible single source file on each agent. For directories, select “Add destination prefix” or “Keep filename / relative path”.",
      ja: "転送先キーを直接指定する場合、各ノードの転送元はアクセス可能な単一ファイルである必要があります。ディレクトリの場合は「転送先プレフィックスを追加」または「ファイル名 / 相対パスを維持」を選択してください。",
    },
  "任务 #{value1} · {value2} · {value3} 个执行节点": {
    en: "Task #{value1} · {value2} · {value3} agents",
    ja: "タスク #{value1} · {value2} · 実行ノード {value3}台",
  },
  "部分任务可能已受理。重试前请核对任务列表，避免重复提交。": {
    en: "Some tasks may have been accepted. Check the task list before retrying to avoid duplicates.",
    ja: "一部のタスクは受理済みの可能性があります。重複を避けるため、再試行前にタスク一覧を確認してください。",
  },
  "提交 {value1} 个任务，{value2}。{value3}任务自动执行，可在“迁移任务”中查看状态。":
    {
      en: "Submitted {value1} tasks; {value2}. {value3}Tasks start automatically. Check their status in Transfers.",
      ja: "{value1}件のタスクを送信しました。{value2}。{value3}タスクは自動実行されます。転送タスク画面で状態を確認できます。",
    },
  "已受理 {value1} 个": {
    en: "{value1} accepted",
    ja: "{value1}件受理",
  },
  服务端未确认受理数量: {
    en: "the server did not confirm the accepted count",
    ja: "受理件数をサーバーが確認できませんでした",
  },
  "任务 ID：{value1}。": {
    en: "Task IDs: {value1}. ",
    ja: "タスク ID：{value1}。",
  },
  提交结果待核对: {
    en: "Check submission results",
    ja: "送信結果を確認してください",
  },
  迁移任务已提交: {
    en: "Transfer tasks submitted",
    ja: "転送タスクを送信しました",
  },
  "部分任务未确认，请核对提交结果": {
    en: "Some tasks are unconfirmed. Check the submission results.",
    ja: "一部のタスクが未確認です。送信結果を確認してください。",
  },
  预检未通过: {
    en: "Preflight failed",
    ja: "事前チェックに失敗しました",
  },
  任务提交失败: {
    en: "Task submission failed",
    ja: "タスクの送信に失敗しました",
  },
  "服务端可能已受理部分任务。重试前请核对任务列表，避免重复提交。": {
    en: "The server may have accepted some tasks. Check the task list before retrying to avoid duplicates.",
    ja: "サーバーが一部のタスクを受理した可能性があります。重複を避けるため、再試行前にタスク一覧を確認してください。",
  },
  重试: {
    en: "Retry",
    ja: "再試行",
  },
  "无法获取节点信息：": {
    en: "Unable to load agents: ",
    ja: "実行ノードを取得できません：",
  },
  执行节点: {
    en: "Agents",
    ja: "実行ノード",
  },
  "移除执行节点 {value1}": {
    en: "Remove agent {value1}",
    ja: "実行ノード {value1} を削除",
  },
  可用: {
    en: "Available",
    ja: "利用可能",
  },
  不可用: {
    en: "Unavailable",
    ja: "利用不可",
  },
  "执行节点 IP": {
    en: "Agent IP",
    ja: "実行ノード IP",
  },
  "选择或输入执行节点 IP": {
    en: "Select or enter an agent IP",
    ja: "実行ノード IP を選択または入力",
  },
  源文件或目录绝对路径: {
    en: "Absolute source file or directory path",
    ja: "転送元ファイルまたはディレクトリの絶対パス",
  },
  目标目录绝对路径: {
    en: "Absolute destination directory path",
    ja: "転送先ディレクトリの絶対パス",
  },
  "节点不可达或不支持本地迁移，请检查节点状态。": {
    en: "The agent is unreachable or does not support local transfers. Check its status.",
    ja: "実行ノードに接続できないか、ローカル転送に対応していません。ノードの状態を確認してください。",
  },
  添加共享存储节点: {
    en: "Add shared-storage agent",
    ja: "共有ストレージの実行ノードを追加",
  },
  "多个节点必须访问同一共享存储中的同一份数据，挂载路径可以不同。预检检查可访问性，不验证是否属于同一共享卷。":
    {
      en: "All agents must access the same data on shared storage; mount paths may differ. Preflight checks access, not whether paths belong to the same volume.",
      ja: "すべてのノードが共有ストレージ上の同じデータにアクセスする必要があります。マウントパスは異なっていても構いません。事前チェックではアクセス可否を確認しますが、同一ボリュームであることは検証しません。",
    },
  "目标相对路径（可选）": {
    en: "Destination relative path (optional)",
    ja: "転送先の相対パス（任意）",
  },
  "指定单个对象在目标目录内的文件名或相对路径。": {
    en: "Filename or relative path for a single object within the destination directory.",
    ja: "単一オブジェクトの転送先ディレクトリ内のファイル名または相対パスを指定します。",
  },
  迁移源: {
    en: "Source",
    ja: "転送元",
  },
  执行节点上的文件或目录: {
    en: "File or directory on an agent",
    ja: "実行ノード上のファイルまたはディレクトリ",
  },
  待迁移的存储桶或对象: {
    en: "Bucket or objects to transfer",
    ja: "転送するバケットまたはオブジェクト",
  },
  迁移范围: {
    en: "Transfer scope",
    ja: "転送範囲",
  },
  整个存储桶: {
    en: "Entire bucket",
    ja: "バケット全体",
  },
  按对象前缀筛选: {
    en: "Filter by object prefix",
    ja: "オブジェクトプレフィックスで絞り込み",
  },
  单个对象: {
    en: "Single object",
    ja: "単一オブジェクト",
  },
  "源对象 Key": {
    en: "Source object key",
    ja: "転送元オブジェクトキー",
  },
  源对象前缀: {
    en: "Source object prefix",
    ja: "転送元オブジェクトプレフィックス",
  },
  "填写完整对象 Key，区分大小写。": {
    en: "Enter the full object key. Keys are case-sensitive.",
    ja: "完全なオブジェクトキーを入力してください。大文字と小文字を区別します。",
  },
  "仅迁移 Key 以此前缀开头的对象，区分大小写。": {
    en: "Transfer only objects whose keys start with this prefix. Case-sensitive.",
    ja: "このプレフィックスで始まるキーのオブジェクトのみ転送します。大文字と小文字を区別します。",
  },
  迁移目标: {
    en: "Destination",
    ja: "転送先",
  },
  执行节点或共享存储上的目标目录: {
    en: "Destination directory on an agent or shared storage",
    ja: "実行ノードまたは共有ストレージ上の転送先ディレクトリ",
  },
  接收数据的存储桶: {
    en: "Bucket receiving the data",
    ja: "データを受け取るバケット",
  },
  目标对象命名: {
    en: "Destination object naming",
    ja: "転送先オブジェクトの命名",
  },
  "保留源文件名 / 相对路径": {
    en: "Keep filename / relative path",
    ja: "ファイル名 / 相対パスを維持",
  },
  添加目标前缀: {
    en: "Add destination prefix",
    ja: "転送先プレフィックスを追加",
  },
  "指定单文件目标 Key": {
    en: "Set destination key for a single file",
    ja: "単一ファイルの転送先キーを指定",
  },
  "目标对象 Key": {
    en: "Destination object key",
    ja: "転送先オブジェクトキー",
  },
  目标对象前缀: {
    en: "Destination object prefix",
    ja: "転送先オブジェクトプレフィックス",
  },
  创建迁移: {
    en: "Create transfer",
    ja: "転送を作成",
  },
  "配置迁移源、目标、范围和执行方式。": {
    en: "Configure the source, destination, scope, and agents.",
    ja: "転送元、転送先、範囲、実行方法を設定します。",
  },
  查看迁移任务: {
    en: "View transfers",
    ja: "転送タスクを表示",
  },
  创建方式: {
    en: "Creation method",
    ja: "作成方法",
  },
  配置迁移: {
    en: "Configure transfer",
    ja: "転送を設定",
  },
  批量导入: {
    en: "Batch import",
    ja: "一括インポート",
  },
  "已恢复当前标签页的草稿。预检结果未保存。": {
    en: "Draft restored for this tab. Preflight results were not saved.",
    ja: "このタブの下書きを復元しました。事前チェックの結果は保存されていません。",
  },
  "无法加载存储连接：": {
    en: "Unable to load storage connections: ",
    ja: "ストレージ接続を読み込めません：",
  },
  添加连接: {
    en: "Add connection",
    ja: "接続を追加",
  },
  "创建任务前需添加存储连接。": {
    en: "Add a storage connection before creating a task.",
    ja: "タスクを作成する前にストレージ接続を追加してください。",
  },
  迁移选项: {
    en: "Transfer options",
    ja: "転送オプション",
  },
  "冲突处理、节点分发、时间与带宽": {
    en: "Conflicts, distribution, time range, and bandwidth",
    ja: "競合処理、ノード配分、時間範囲、帯域幅",
  },
  暂无可用节点: {
    en: "No agents available",
    ja: "利用可能な実行ノードがありません",
  },
  " · 不可用": {
    en: " · Unavailable",
    ja: " · 利用不可",
  },
  节点列表加载失败: {
    en: "Unable to load agents",
    ja: "実行ノードを読み込めません",
  },
  请至少选择一个执行节点: {
    en: "Select at least one agent",
    ja: "実行ノードを1つ以上選択してください",
  },
  "所选节点不可用，请重新选择": {
    en: "A selected agent is unavailable. Select another agent.",
    ja: "選択した実行ノードは利用できません。別のノードを選択してください",
  },
  刷新执行节点: {
    en: "Refresh agents",
    ja: "実行ノードを更新",
  },
  目标已存在时: {
    en: "If the destination exists",
    ja: "転送先が存在する場合",
  },
  "覆盖会替换目标同名对象或文件。": {
    en: "Overwrite replaces objects or files with the same name at the destination.",
    ja: "上書きすると、転送先の同名オブジェクトまたはファイルが置き換えられます。",
  },
  跳过同名对象或文件: {
    en: "Skip existing objects or files",
    ja: "既存のオブジェクトまたはファイルをスキップ",
  },
  覆盖同名对象或文件: {
    en: "Overwrite existing objects or files",
    ja: "既存のオブジェクトまたはファイルを上書き",
  },
  节点分发方式: {
    en: "Distribution mode",
    ja: "ノード配分方式",
  },
  "当前仅绑定一个节点，任务将在该节点执行。": {
    en: "Only one agent is selected; it will execute this task.",
    ja: "実行ノードが1つのみ指定されているため、そのノードで実行します。",
  },
  "亲和分发优先使用固定节点；均衡分发将任务分配给可用节点。": {
    en: "Affinity prefers a fixed agent. Balanced distributes work across available agents.",
    ja: "アフィニティ配分では固定ノードを優先します。均等配分では利用可能なノードにタスクを配分します。",
  },
  "修改时间晚于（可选）": {
    en: "Modified after (optional)",
    ja: "更新日時の下限（任意）",
  },
  "时间按服务端时区解释。": {
    en: "Times use the server time zone.",
    ja: "時刻にはサーバーのタイムゾーンを使用します。",
  },
  "修改时间早于（可选）": {
    en: "Modified before (optional)",
    ja: "更新日時の上限（任意）",
  },
  "选择任务类型，粘贴数据或读取文件。提交后保留输入数据供核对。": {
    en: "Select a task type, then paste data or load a file. Input remains available after submission.",
    ja: "タスクの種類を選び、データを貼り付けるかファイルを読み込みます。送信後も入力内容は保持されます。",
  },
  导入类型: {
    en: "Import type",
    ja: "インポートの種類",
  },
  "OSS 存储桶迁移": {
    en: "OSS bucket transfers",
    ja: "OSS バケット転送",
  },
  "OSS 子任务恢复 / 导入": {
    en: "OSS subtask restore / import",
    ja: "OSS 子タスクの復元 / インポート",
  },
  本地迁移请求: {
    en: "Local transfer requests",
    ja: "ローカル転送リクエスト",
  },
  数据格式: {
    en: "Data format",
    ja: "データ形式",
  },
  "JSON 数组": {
    en: "JSON array",
    ja: "JSON 配列",
  },
  逗号分隔文本: {
    en: "Comma-separated text",
    ja: "カンマ区切りテキスト",
  },
  "用于导入已有的 OSS 子任务或恢复断点。支持 startKey、endKey、marker 和 workerNode；留空 workerNode 时由集群分配。":
    {
      en: "Import existing OSS subtasks or restore checkpoints. Supports startKey, endKey, marker, and workerNode. Leave workerNode empty for cluster assignment.",
      ja: "既存の OSS 子タスクをインポートするか、チェックポイントから復元します。startKey、endKey、marker、workerNode に対応しています。workerNode を省略するとクラスターが割り当てます。",
    },
  "每行一个任务：源连接/源桶,目标连接/目标桶,修改时间晚于,是否覆盖,分发模式。后三项可留空。前缀或修改时间早于筛选请使用 JSON。目标命名规则和任务带宽上限请使用 JSON 配置。":
    {
      en: "One task per row: source-connection/source-bucket,destination-connection/destination-bucket,modified-after,overwrite,distribution-mode. The last three fields are optional. Use JSON for prefixes, modified-before, key mapping, and task bandwidth limits.",
      ja: "1行につき1タスク：転送元接続/転送元バケット,転送先接続/転送先バケット,更新日時の下限,上書き,配分方式。末尾3項目は省略できます。プレフィックス、更新日時の上限、キーのマッピング、タスク帯域幅上限は JSON で設定してください。",
    },
  任务数据: {
    en: "Task data",
    ja: "タスクデータ",
  },
  读取数据文件: {
    en: "Load data file",
    ja: "データファイルを読み込む",
  },
  填入示例: {
    en: "Insert example",
    ja: "例を入力",
  },
  字段说明: {
    en: "Field reference",
    ja: "フィールドの説明",
  },
  "direction：OSS_TO_LOCAL 或 LOCAL_TO_OSS；sourceEndpoint / destinationEndpoint：OSS 端提供 type、s3Alias、bucket 和可选 objectKey / prefix；LOCAL 端提供 type、nodeBindings，多节点绑定必须访问同一份共享数据。LOCAL_TO_OSS 按节点数自动分发，单文件仅支持一个节点。可选 conflictPolicy、dateAfter、dateBefore、targetRelativePath；OSS_TO_LOCAL 可选 distributionMode。taskBandwidthLimitMbps：任务带宽上限，单位 Mbps，0.1–1,000,000；省略或 null 表示仅使用节点限速。":
    {
      en: "direction: OSS_TO_LOCAL or LOCAL_TO_OSS. sourceEndpoint / destinationEndpoint: the OSS endpoint requires type, s3Alias, bucket, and optionally objectKey / prefix; the LOCAL endpoint requires type and nodeBindings. Multiple agents must access the same shared data. LOCAL_TO_OSS distributes work based on agent count; single files support one agent only. Optional: conflictPolicy, dateAfter, dateBefore, targetRelativePath; OSS_TO_LOCAL also supports distributionMode. taskBandwidthLimitMbps: task-wide limit in Mbps, 0.1–1,000,000. Omit or use null to apply only agent limits.",
      ja: "direction：OSS_TO_LOCAL または LOCAL_TO_OSS。sourceEndpoint / destinationEndpoint：OSS 側には type、s3Alias、bucket と任意の objectKey / prefix、LOCAL 側には type と nodeBindings を指定します。複数ノードは同じ共有データにアクセスする必要があります。LOCAL_TO_OSS はノード数に応じて自動配分され、単一ファイルは1ノードのみ対応します。任意項目：conflictPolicy、dateAfter、dateBefore、targetRelativePath。OSS_TO_LOCAL は distributionMode も指定できます。taskBandwidthLimitMbps：タスク全体の帯域幅上限（Mbps、0.1–1,000,000）。省略または null の場合はノードの制限のみ適用します。",
    },
  "sourceS3Alias / destS3Alias：连接名称；sourceBucket / destBucket：存储桶；prefix：源前缀筛选，objectKey：单个源对象，二者互斥。{value1}keyMapping 省略或 null 保留完整源 Key；mode 可选 PRESERVE、ADD_PREFIX（targetPrefix）、REPLACE_PREFIX（sourcePrefix 与源筛选前缀一致，targetPrefix 可为空）、EXACT_KEY（仅单对象，targetKey）。ADD_PREFIX 末尾缺少 / 时补一个；REPLACE_PREFIX 字面替换且不补 /；所有 Key 和前缀均保留空格与斜杠。dateAfter / dateBefore：修改时间范围，格式 YYYY-MM-DDTHH:mm:ss；overwrite：是否覆盖；distributionMode：affinity 或 balanced。taskBandwidthLimitMbps：任务带宽上限，单位 Mbps，0.1–1,000,000；省略或 null 表示仅使用节点限速。":
    {
      en: "sourceS3Alias / destS3Alias: connection names; sourceBucket / destBucket: buckets. prefix filters the source; objectKey selects a single source object. These are mutually exclusive. {value1}Omit keyMapping or set null to keep the full source key. Modes: PRESERVE, ADD_PREFIX (targetPrefix), REPLACE_PREFIX (sourcePrefix must equal the source filter; targetPrefix may be empty), EXACT_KEY (single object only; targetKey). ADD_PREFIX appends / when missing; REPLACE_PREFIX uses literal replacement without adding /. Keys and prefixes retain spaces and slashes. dateAfter / dateBefore: modification time range, YYYY-MM-DDTHH:mm:ss. overwrite: overwrite existing objects. distributionMode: affinity or balanced. taskBandwidthLimitMbps: task-wide limit in Mbps, 0.1–1,000,000; omit or use null to apply only agent limits.",
      ja: "sourceS3Alias / destS3Alias：接続名。sourceBucket / destBucket：バケット。prefix は転送元の絞り込み、objectKey は単一オブジェクトの選択に使用し、同時指定はできません。{value1}keyMapping を省略または null にすると、完全な転送元キーを維持します。mode：PRESERVE、ADD_PREFIX（targetPrefix）、REPLACE_PREFIX（sourcePrefix は転送元の絞り込みと一致、targetPrefix は空でも可）、EXACT_KEY（単一オブジェクトのみ、targetKey）。ADD_PREFIX は末尾に / がなければ追加します。REPLACE_PREFIX は文字列をそのまま置換し、/ は追加しません。キーとプレフィックスの空白とスラッシュは保持します。dateAfter / dateBefore：更新日時の範囲（YYYY-MM-DDTHH:mm:ss）。overwrite：上書き。distributionMode：affinity または balanced。taskBandwidthLimitMbps：タスク全体の上限（Mbps、0.1–1,000,000）。省略または null の場合はノードの制限のみ適用します。",
    },
  "子任务也支持 sourceEndpoint.objectKey；使用 sourceEndpoint.prefix 时须设置 sourceEndpoint.prefixFilterEnabled:true，与顶层字段重复时值必须一致。可选 startKey、endKey、marker、workerNode。":
    {
      en: "Subtasks also support sourceEndpoint.objectKey. sourceEndpoint.prefix requires sourceEndpoint.prefixFilterEnabled:true; duplicate top-level values must match. Optional: startKey, endKey, marker, workerNode. ",
      ja: "子タスクは sourceEndpoint.objectKey も指定できます。sourceEndpoint.prefix を使用する場合は sourceEndpoint.prefixFilterEnabled:true が必要です。トップレベルの値と重複する場合は一致させてください。任意項目：startKey、endKey、marker、workerNode。",
    },
  "存储桶迁移会按源范围生成子任务。": {
    en: "Bucket transfers generate subtasks for the selected source scope. ",
    ja: "バケット転送では転送元の範囲に応じて子タスクが生成されます。",
  },
  节点预检通过: {
    en: "Agent preflight passed",
    ja: "ノードの事前チェックに合格",
  },
  节点预检未通过: {
    en: "Agent preflight failed",
    ja: "ノードの事前チェックに失敗",
  },
  通过: {
    en: "Passed",
    ja: "合格",
  },
  "个节点配置。修改配置后需重新预检。": {
    en: "agent configurations. Run preflight again after changing the configuration.",
    ja: "件のノード設定。設定を変更した場合は事前チェックを再実行してください。",
  },
  预检失败: {
    en: "Preflight failed",
    ja: "事前チェックに失敗",
  },
  "通过{value1}": {
    en: "Passed{value1}",
    ja: "合格{value1}",
  },
  文件: {
    en: "File",
    ja: "ファイル",
  },
  目录: {
    en: "Directory",
    ja: "ディレクトリ",
  },
  未通过: {
    en: "Failed",
    ja: "不合格",
  },
  查看任务: {
    en: "View task",
    ja: "タスクを表示",
  },
  "查看任务 #": {
    en: "View task #",
    ja: "タスクを表示 #",
  },
  批量创建迁移任务: {
    en: "Create transfers in batch",
    ja: "転送タスクを一括作成",
  },
  "提交前请核对存储连接和迁移路径。": {
    en: "Check storage connections and paths before submitting.",
    ja: "送信前にストレージ接続と転送パスを確認してください。",
  },
  " · 上限 {value1}": {
    en: " · Limit {value1}",
    ja: " · 上限 {value1}",
  },
  重置草稿: {
    en: "Reset draft",
    ja: "下書きをリセット",
  },
  "检查节点、路径和存储桶可访问性。目标目录在预检时可能被创建。": {
    en: "Check agent, path, and bucket access. Preflight may create the destination directory.",
    ja: "実行ノード、パス、バケットへのアクセスを確認します。事前チェック時に転送先ディレクトリが作成される場合があります。",
  },
  正在预检: {
    en: "Checking",
    ja: "チェック中",
  },
  运行预检: {
    en: "Run preflight",
    ja: "事前チェックを実行",
  },
  继续配置任务: {
    en: "Continue configuration",
    ja: "設定を続ける",
  },
  正在提交: {
    en: "Submitting",
    ja: "送信中",
  },
  创建迁移任务: {
    en: "Create transfer task",
    ja: "転送タスクを作成",
  },
  导入任务: {
    en: "Import tasks",
    ja: "タスクをインポート",
  },
  "多节点预检会读写临时文件，验证共享目录。": {
    en: "Multi-node preflight writes and reads temporary files to verify the shared directory.",
    ja: "複数ノードの事前チェックでは、一時ファイルを読み書きして共有ディレクトリを確認します。",
  },
  "未提交的配置自动保存在当前标签页，切换页面后可继续填写。": {
    en: "Unsubmitted settings are saved in this tab and remain available when you navigate away.",
    ja: "未送信の設定はこのタブに保存され、別の画面に移動しても入力を続けられます。",
  },
  扫描中: {
    en: "Scanning",
    ja: "スキャン中",
  },
  "OSS → 本地": {
    en: "OSS → Local",
    ja: "OSS → ローカル",
  },
  "本地 → OSS": {
    en: "Local → OSS",
    ja: "ローカル → OSS",
  },
  等待分配: {
    en: "Awaiting assignment",
    ja: "割り当て待ち",
  },
  本地文件系统: {
    en: "Local file system",
    ja: "ローカルファイルシステム",
  },
  迁移任务: {
    en: "Transfers",
    ja: "転送タスク",
  },
  "{value1} 个待执行": {
    en: "{value1} pending",
    ja: "{value1}件待機中",
  },
  执行中任务: {
    en: "Active tasks",
    ja: "実行中のタスク",
  },
  含运行中和扫描完成的任务: {
    en: "Includes running tasks and tasks that finished scanning",
    ja: "実行中およびスキャン完了のタスクを含む",
  },
  已迁移数据: {
    en: "Data transferred",
    ja: "転送済みデータ",
  },
  "{value1} 个对象已传输": {
    en: "{value1} objects transferred",
    ja: "{value1}件のオブジェクトを転送済み",
  },
  失败对象: {
    en: "Failed objects",
    ja: "失敗オブジェクト",
  },
  "{value1} 个失败或部分失败任务": {
    en: "{value1} failed or partially failed tasks",
    ja: "{value1}件の失敗または一部失敗タスク",
  },
  等待服务响应: {
    en: "Waiting for server response",
    ja: "サーバーの応答を待機中",
  },
  "任务 #{value1}：{value2}": {
    en: "Task #{value1}: {value2}",
    ja: "タスク #{value1}：{value2}",
  },
  "报告下载已请求，请查看浏览器下载列表。": {
    en: "Report requested. Check your browser downloads.",
    ja: "レポートを要求しました。ブラウザーのダウンロード一覧を確認してください。",
  },
  "报告下载失败，请重试。": {
    en: "Report download failed. Please retry.",
    ja: "レポートのダウンロードに失敗しました。再試行してください。",
  },
  "迁移任务 #": {
    en: "Transfer #",
    ja: "転送タスク #",
  },
  关闭详情: {
    en: "Close details",
    ja: "詳細を閉じる",
  },
  刷新: {
    en: "Refresh",
    ja: "更新",
  },
  "当前列表中未找到此任务，请刷新任务状态。": {
    en: "This task is not in the current list. Refresh task status.",
    ja: "このタスクは現在の一覧にありません。状態を更新してください。",
  },
  任务详情: {
    en: "Task details",
    ja: "タスク詳細",
  },
  启动: {
    en: "Start",
    ja: "開始",
  },
  启动任务: {
    en: "Start task",
    ja: "タスクを開始",
  },
  停止: {
    en: "Stop",
    ja: "停止",
  },
  停止任务: {
    en: "Stop task",
    ja: "タスクを停止",
  },
  继续传输: {
    en: "Resume",
    ja: "再開",
  },
  删除: {
    en: "Delete",
    ja: "削除",
  },
  删除任务: {
    en: "Delete task",
    ja: "タスクを削除",
  },
  "任务信息不完整，请刷新或检查执行节点。": {
    en: "Task data is incomplete. Refresh or check the agents.",
    ja: "タスク情報が不完全です。更新するか実行ノードを確認してください。",
  },
  来源: {
    en: "Source",
    ja: "転送元",
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
  正在准备下载: {
    en: "Preparing download",
    ja: "ダウンロードを準備中",
  },
  导出任务报告: {
    en: "Export task report",
    ja: "タスクレポートをエクスポート",
  },
  "节点状态恢复后可导出报告。": {
    en: "Reports are available once agent status recovers.",
    ja: "ノードの状態が復旧するとレポートをエクスポートできます。",
  },
  "停止或结束后可导出报告。": {
    en: "Reports are available after the task stops or finishes.",
    ja: "タスクが停止または終了するとレポートをエクスポートできます。",
  },
  查看失败对象: {
    en: "View failed objects",
    ja: "失敗オブジェクトを表示",
  },
  查看异常记录: {
    en: "View exceptions",
    ja: "エラー記録を表示",
  },
  执行信息: {
    en: "Execution",
    ja: "実行情報",
  },
  "共享存储 ID": {
    en: "Shared storage ID",
    ja: "共有ストレージ ID",
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
  目标命名: {
    en: "Destination naming",
    ja: "転送先の命名",
  },
  目标前缀: {
    en: "Destination prefix",
    ja: "転送先プレフィックス",
  },
  "Key 映射": {
    en: "Key mapping",
    ja: "キーマッピング",
  },
  "Key 示例": {
    en: "Key example",
    ja: "キーの例",
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
  时间: {
    en: "Time",
    ja: "時刻",
  },
  子任务: {
    en: "Subtasks",
    ja: "子タスク",
  },
  "正在加载子任务…": {
    en: "Loading subtasks…",
    ja: "子タスクを読み込み中…",
  },
  此范围暂无子任务: {
    en: "No subtasks in this range",
    ja: "この範囲に子タスクはありません",
  },
  尚未生成子任务: {
    en: "No subtasks created yet",
    ja: "子タスクはまだ生成されていません",
  },
  "查看子任务 {value1}-{value2}": {
    en: "View subtask {value1}-{value2}",
    ja: "子タスク {value1}-{value2} を表示",
  },
  "子任务 #": {
    en: "Subtask #",
    ja: "子タスク #",
  },
  "· 失败": {
    en: "· Failed",
    ja: "· 失敗",
  },
  下一页子任务: {
    en: "Next subtasks",
    ja: "次の子タスク",
  },
  上一页子任务: {
    en: "Previous subtasks",
    ja: "前の子タスク",
  },
  加载失败: {
    en: "Unable to load",
    ja: "読み込みに失敗",
  },
  "{value1}已受理 {value2}，忽略 {value3}，失败 {value4}{value5}": {
    en: "{value1}: {value2} accepted, {value3} ignored, {value4} failed{value5}",
    ja: "{value1}：受理 {value2}件、無視 {value3}件、失敗 {value4}件{value5}",
  },
  本地执行项: {
    en: "Local execution items",
    ja: "ローカル実行項目",
  },
  执行项: {
    en: "Execution items",
    ja: "実行項目",
  },
  "；未响应节点：{value1}": {
    en: "; unresponsive agents: {value1}",
    ja: "；未応答ノード：{value1}",
  },
  "部分请求未完成：{value1}": {
    en: "Some requests did not complete: {value1}",
    ja: "一部のリクエストが完了していません：{value1}",
  },
  "部分操作未完成，详见列表上方的结果。": {
    en: "Some actions did not complete. See the results above the list.",
    ja: "一部の操作が完了していません。一覧上部の結果を確認してください。",
  },
  "{value1}请求已提交{value2}。请以刷新后的任务状态为准。": {
    en: "{value1} requested{value2}. Refresh task status to confirm.",
    ja: "{value1}を要求しました{value2}。タスクの状態を更新して確認してください。",
  },
  "{value1} 已迁移": {
    en: "{value1} transferred",
    ja: "{value1}件転送済み",
  },
  统计更新中: {
    en: "Updating statistics",
    ja: "統計を更新中",
  },
  待扫描: {
    en: "Waiting to scan",
    ja: "スキャン待ち",
  },
  扫描未完成: {
    en: "Scan incomplete",
    ja: "スキャン未完了",
  },
  迁移概览: {
    en: "Transfer overview",
    ja: "転送の概要",
  },
  "任务统计与近期传输状态。": {
    en: "Task statistics and recent transfers.",
    ja: "タスク統計と最近の転送状況。",
  },
  "查看传输进度，启动、停止或继续任务。": {
    en: "Track progress, start, stop, or resume tasks.",
    ja: "転送状況の確認と、タスクの開始・停止・再開。",
  },
  刷新任务: {
    en: "Refresh tasks",
    ja: "タスクを更新",
  },
  加载中: {
    en: "Loading",
    ja: "読み込み中",
  },
  加载更早任务: {
    en: "Load older tasks",
    ja: "以前のタスクを読み込む",
  },
  已加载: {
    en: "Loaded",
    ja: "読み込み済み",
  },
  "个任务。统计仅包含最近一页。": {
    en: "tasks. Statistics cover only the most recent page.",
    ja: "件。統計には直近のページのみ含まれます。",
  },
  "部分节点状态不可用，任务统计可能不完整。": {
    en: "Some agent status data is unavailable. Statistics may be incomplete.",
    ja: "一部のノード状態を取得できません。統計が不完全な可能性があります。",
  },
  查看节点: {
    en: "View agents",
    ja: "実行ノードを表示",
  },
  存储连接: {
    en: "Storage connections",
    ja: "ストレージ接続",
  },
  "管理对象存储的访问地址和凭证。": {
    en: "Manage object storage endpoints and credentials.",
    ja: "オブジェクトストレージの接続先と認証情報を管理します。",
  },
  管理存储连接: {
    en: "Manage connections",
    ja: "接続を管理",
  },
  最近任务: {
    en: "Recent tasks",
    ja: "最近のタスク",
  },
  全部任务: {
    en: "All tasks",
    ja: "すべてのタスク",
  },
  查看全部: {
    en: "View all",
    ja: "すべて表示",
  },
  "搜索任务、存储桶或路径": {
    en: "Search tasks, buckets, or paths",
    ja: "タスク、バケット、パスを検索",
  },
  搜索任务: {
    en: "Search tasks",
    ja: "タスクを検索",
  },
  任务状态: {
    en: "Task status",
    ja: "タスクの状態",
  },
  全部状态: {
    en: "All statuses",
    ja: "すべての状態",
  },
  传输方向: {
    en: "Transfer direction",
    ja: "転送方向",
  },
  全部方向: {
    en: "All directions",
    ja: "すべての方向",
  },
  "IP 地址": {
    en: "IP address",
    ja: "IP アドレス",
  },
  已选: {
    en: "Selected",
    ja: "選択済み",
  },
  "选择全部 {value1} 个结果": {
    en: "Select all {value1} results",
    ja: "全 {value1}件を選択",
  },
  "{value1}选中任务": {
    en: "{value1} selected tasks",
    ja: "選択したタスクを{value1}",
  },
  "请选择待启动的 OSS→OSS 任务": {
    en: "Select pending OSS → OSS tasks",
    ja: "待機中の OSS → OSS タスクを選択してください",
  },
  请选择待启动或运行中的任务: {
    en: "Select pending or running tasks",
    ja: "待機中または実行中のタスクを選択してください",
  },
  请选择已停止或失败的任务: {
    en: "Select stopped or failed tasks",
    ja: "停止または失敗したタスクを選択してください",
  },
  请选择未在运行的任务: {
    en: "Select tasks that are not running",
    ja: "実行中でないタスクを選択してください",
  },
  "导出 JSON": {
    en: "Export JSON",
    ja: "JSON をエクスポート",
  },
  取消选择: {
    en: "Clear selection",
    ja: "選択を解除",
  },
  没有匹配的任务: {
    en: "No matching tasks",
    ja: "一致するタスクはありません",
  },
  任务加载失败: {
    en: "Unable to load tasks",
    ja: "タスクを読み込めません",
  },
  暂无迁移任务: {
    en: "No transfer tasks",
    ja: "転送タスクはありません",
  },
  "请调整筛选条件或搜索关键词。": {
    en: "Adjust filters or search terms.",
    ja: "フィルターまたは検索語を変更してください。",
  },
  "请检查连接后重试。": {
    en: "Check the connection and retry.",
    ja: "接続を確認して再試行してください。",
  },
  "配置存储连接后创建迁移任务。": {
    en: "Configure a storage connection, then create a transfer task.",
    ja: "ストレージ接続を設定してから転送タスクを作成してください。",
  },
  迁移任务列表: {
    en: "Transfer task list",
    ja: "転送タスク一覧",
  },
  选择当前页任务: {
    en: "Select tasks on this page",
    ja: "このページのタスクを選択",
  },
  任务: {
    en: "Task",
    ja: "タスク",
  },
  "来源 / 目标": {
    en: "Source / Destination",
    ja: "転送元 / 転送先",
  },
  "已迁移 / 已扫描": {
    en: "Transferred / Scanned",
    ja: "転送済み / スキャン済み",
  },
  状态: {
    en: "Status",
    ja: "状態",
  },
  失败: {
    en: "Failed",
    ja: "失敗",
  },
  "选择任务 {value1}": {
    en: "Select task {value1}",
    ja: "タスク {value1} を選択",
  },
  查看详情: {
    en: "View details",
    ja: "詳細を表示",
  },
  "查看任务 {value1}": {
    en: "View task {value1}",
    ja: "タスク {value1} を表示",
  },
  每页: {
    en: "Rows per page",
    ja: "ページあたりの件数",
  },
  下一页: {
    en: "Next page",
    ja: "次のページ",
  },
  上一页: {
    en: "Previous page",
    ja: "前のページ",
  },
  "最近创建的 5 个任务": {
    en: "5 most recently created tasks",
    ja: "直近に作成された5件のタスク",
  },
  "共 {value1} 个任务": {
    en: "{value1} tasks",
    ja: "全 {value1}件のタスク",
  },
  "进度包含成功与跳过对象；扫描中总量可能变化": {
    en: "Progress includes transferred and skipped objects; totals may change during scanning",
    ja: "進捗には成功とスキップを含みます。スキャン中は合計件数が変わる場合があります",
  },
  "个任务？": {
    en: "tasks?",
    ja: "件のタスク？",
  },
  "任务记录将移至回收站，不会删除源或目标存储中的对象。": {
    en: "Task records will move to the recycle bin. Source and destination objects will not be deleted.",
    ja: "タスクの記録をゴミ箱に移動します。転送元・転送先のオブジェクトは削除されません。",
  },
  "停止扫描和传输，保留已完成的对象与进度。": {
    en: "Stop scanning and transfer. Completed objects and progress are kept.",
    ja: "スキャンと転送を停止します。転送済みオブジェクトと進捗は保持されます。",
  },
  "继续扫描和处理未完成的对象，保留已有传输进度。": {
    en: "Resume scanning and process unfinished objects, keeping existing progress.",
    ja: "既存の進捗を保持してスキャンを再開し、未処理のオブジェクトを転送します。",
  },
  "任务将开始传输，请确认来源和目标配置。": {
    en: "Tasks will start transferring. Confirm the source and destination settings.",
    ja: "転送を開始します。転送元と転送先の設定を確認してください。",
  },
  "任务：": {
    en: "Tasks: ",
    ja: "タスク：",
  },
  取消: {
    en: "Cancel",
    ja: "キャンセル",
  },
  "提交中…": {
    en: "Submitting…",
    ja: "送信中…",
  },
  "确认{value1}": {
    en: "Confirm {value1}",
    ja: "{value1}を確定",
  },
  降级迁移记录: {
    en: "Fallback transfers",
    ja: "フォールバック転送記録",
  },
  本地迁移失败: {
    en: "Local transfer failures",
    ja: "ローカル転送の失敗",
  },
  "任务 ID 必须是非负整数。": {
    en: "Task ID must be a non-negative integer.",
    ja: "タスク ID は0以上の整数にしてください。",
  },
  "部分节点未能清除记录，请刷新后重试。": {
    en: "Some agents could not clear records. Refresh and retry.",
    ja: "一部のノードで記録を削除できませんでした。更新して再試行してください。",
  },
  "清除请求已提交，正在刷新各节点记录。": {
    en: "Clear request submitted. Refreshing agent records.",
    ja: "削除を要求しました。各ノードの記録を更新中です。",
  },
  "（未完成节点：{value1}）": {
    en: " (incomplete agents: {value1})",
    ja: "（未完了ノード：{value1}）",
  },
  "重试已受理 {value1}，忽略 {value2}，失败 {value3}{value4}": {
    en: "Retry: {value1} accepted, {value2} ignored, {value3} failed{value4}",
    ja: "再試行：受理 {value1}件、無視 {value2}件、失敗 {value3}件{value4}",
  },
  "；不可达节点：{value1}": {
    en: "; unreachable agents: {value1}",
    ja: "；接続できないノード：{value1}",
  },
  未知: {
    en: "Unknown",
    ja: "不明",
  },
  异常记录: {
    en: "Exceptions",
    ja: "エラー記録",
  },
  "查看失败对象和降级记录，导出异常明细。": {
    en: "Review failed objects and fallback transfers; export exception records.",
    ja: "失敗オブジェクトとフォールバック転送の確認、エラー記録のエクスポート。",
  },
  异常记录类型: {
    en: "Exception type",
    ja: "エラー記録の種類",
  },
  "任务 ID": {
    en: "Task ID",
    ja: "タスク ID",
  },
  全部节点: {
    en: "All agents",
    ja: "すべての実行ノード",
  },
  迁移方向: {
    en: "Transfer direction",
    ja: "転送方向",
  },
  "端点 / 路径": {
    en: "Endpoint / Path",
    ja: "エンドポイント / パス",
  },
  "源 Bucket": {
    en: "Source bucket",
    ja: "転送元バケット",
  },
  "目标 Bucket": {
    en: "Destination bucket",
    ja: "転送先バケット",
  },
  每节点读取上限: {
    en: "Records per agent",
    ja: "ノードあたりの取得上限",
  },
  条: {
    en: "records",
    ja: "件",
  },
  应用筛选: {
    en: "Apply filters",
    ja: "フィルターを適用",
  },
  重置: {
    en: "Reset",
    ja: "リセット",
  },
  "节点清单读取失败，无法确认结果是否覆盖全部节点。": {
    en: "Unable to load the agent list. Results may not cover all agents.",
    ja: "ノード一覧を読み込めません。すべてのノードの結果が含まれるか確認できません。",
  },
  "部分节点未返回数据，当前结果不完整。": {
    en: "Some agents did not return data. Results are incomplete.",
    ja: "一部のノードからデータが返されませんでした。結果は不完全です。",
  },
  "不可达节点：{value1}": {
    en: "Unreachable agents: {value1}",
    ja: "接続できないノード：{value1}",
  },
  共查询到: {
    en: "Found",
    ja: "検索結果",
  },
  "条记录，当前加载": {
    en: "records; loaded",
    ja: "件、読み込み済み",
  },
  "条。可提高每节点读取上限或缩小筛选范围。": {
    en: "records. Increase the per-agent limit or narrow the filters.",
    ja: "件。ノードあたりの取得上限を増やすか、フィルターで絞り込んでください。",
  },
  "降级记录表示对象曾切换到备用传输方式。请查看对应任务状态确认结果。": {
    en: "Fallback records indicate that an object used an alternate transfer method. Check its task status for the outcome.",
    ja: "フォールバック記録は、オブジェクトの転送方法が代替方式に切り替わったことを示します。結果は対応するタスクの状態で確認してください。",
  },
  "{value1} 条记录{value2}": {
    en: "{value1} records{value2}",
    ja: "{value1}件の記録{value2}",
  },
  " · 已加载 {value1} 条": {
    en: " · {value1} loaded",
    ja: " · {value1}件読み込み済み",
  },
  读取失败: {
    en: "Unable to read",
    ja: "取得に失敗",
  },
  正在读取记录: {
    en: "Loading records",
    ja: "記録を読み込み中",
  },
  导出当前页: {
    en: "Export current page",
    ja: "現在のページをエクスポート",
  },
  导出已加载记录: {
    en: "Export loaded records",
    ja: "読み込み済みの記録をエクスポート",
  },
  完整迁移报告: {
    en: "Full transfer report",
    ja: "完全な転送レポート",
  },
  清除全部记录: {
    en: "Clear all records",
    ja: "すべての記録を削除",
  },
  "对象 / 路径": {
    en: "Object / Path",
    ja: "オブジェクト / パス",
  },
  任务与节点: {
    en: "Task and agent",
    ja: "タスクと実行ノード",
  },
  原因: {
    en: "Reason",
    ja: "原因",
  },
  记录时间: {
    en: "Recorded",
    ja: "記録日時",
  },
  操作: {
    en: "Action",
    ja: "操作",
  },
  "目标：": {
    en: "Destination: ",
    ja: "転送先：",
  },
  调度阶段: {
    en: "Scheduling",
    ja: "スケジューリング段階",
  },
  未提供原因: {
    en: "No reason provided",
    ja: "原因は記録されていません",
  },
  详情: {
    en: "Details",
    ja: "詳細",
  },
  重试任务: {
    en: "Retry task",
    ja: "タスクを再試行",
  },
  当前页无记录: {
    en: "No records on this page",
    ja: "このページに記録はありません",
  },
  暂无匹配记录: {
    en: "No matching records",
    ja: "一致する記録はありません",
  },
  "每个节点最多返回 1,000 条本地失败记录。请按任务或节点缩小筛选范围。": {
    en: "Each agent returns up to 1,000 local failure records. Filter by task or agent to narrow the results.",
    ja: "各ノードから最大1,000件のローカル失敗記録を取得します。タスクまたはノードで絞り込んでください。",
  },
  "请调整筛选条件，或在任务执行后刷新。": {
    en: "Adjust the filters or refresh after running a task.",
    ja: "フィルターを変更するか、タスク実行後に更新してください。",
  },
  第一页: {
    en: "First page",
    ja: "最初のページ",
  },
  最后一页: {
    en: "Last page",
    ja: "最後のページ",
  },
  "完整迁移报告包含所有本地迁移任务与失败记录，ZIP 中包含不可达节点信息；报告不受当前筛选条件影响。":
    {
      en: "The full report includes all local transfer tasks and failures, with unreachable agents listed in the ZIP. Current filters do not apply.",
      ja: "完全なレポートには全ローカル転送タスクと失敗記録が含まれます。接続できないノードの情報も ZIP に含まれます。現在のフィルターは適用されません。",
    },
  清除全部: {
    en: "Clear all",
    ja: "すべて削除",
  },
  降级: {
    en: "fallback",
    ja: "フォールバック",
  },
  "记录？": {
    en: "records?",
    ja: "記録？",
  },
  "清除所有执行节点的记录，不受当前筛选条件限制。": {
    en: "Clear records on all agents, regardless of the current filters.",
    ja: "現在のフィルターに関係なく、すべての実行ノードの記録を削除します。",
  },
  "记录清除后无法恢复。": {
    en: "Cleared records cannot be restored.",
    ja: "削除した記録は復元できません。",
  },
  "清除范围也包括本地迁移产生的失败对象。": {
    en: "This also includes failed objects from local transfers.",
    ja: "ローカル転送で発生した失敗オブジェクトも削除対象に含まれます。",
  },
  "仅删除异常记录，不会重试迁移任务。请先导出需保留的记录。": {
    en: "Only exception records are deleted; tasks are not retried. Export any records you need first.",
    ja: "エラー記録のみ削除します。タスクは再試行しません。必要な記録は先にエクスポートしてください。",
  },
  确认清除全部: {
    en: "Confirm clear all",
    ja: "すべて削除する",
  },
  异常详情: {
    en: "Exception details",
    ja: "例外の詳細",
  },
  源存储连接: {
    en: "Source connection",
    ja: "転送元ストレージ接続",
  },
  目标存储连接: {
    en: "Destination connection",
    ja: "転送先ストレージ接続",
  },
  "逻辑 Key": {
    en: "Logical key",
    ja: "論理キー",
  },
  原因代码: {
    en: "Reason code",
    ja: "理由コード",
  },
  观测时间: {
    en: "Observed",
    ja: "観測日時",
  },
  目标已写入: {
    en: "Destination written",
    ja: "転送先への書き込み済み",
  },
  是: {
    en: "Yes",
    ja: "はい",
  },
  否: {
    en: "No",
    ja: "いいえ",
  },
  "子任务 ID": {
    en: "Subtask ID",
    ja: "子タスク ID",
  },
  本地路径: {
    en: "Local path",
    ja: "ローカルパス",
  },
  错误信息: {
    en: "Error message",
    ja: "エラーメッセージ",
  },
  错误代码: {
    en: "Error code",
    ja: "エラーコード",
  },
  "对象大小（字节）": {
    en: "Object size (bytes)",
    ja: "オブジェクトサイズ（バイト）",
  },
  失败阶段: {
    en: "Failure stage",
    ja: "失敗段階",
  },
  重试次数: {
    en: "Retry count",
    ja: "再試行回数",
  },
  时间戳: {
    en: "Timestamp",
    ja: "タイムスタンプ",
  },
  临时文件路径: {
    en: "Temporary file path",
    ja: "一時ファイルのパス",
  },
  "存储 ID": {
    en: "Storage ID",
    ja: "ストレージ ID",
  },
  关闭: {
    en: "Close",
    ja: "閉じる",
  },
  未记录: {
    en: "Not recorded",
    ja: "記録なし",
  },
  "HTTP 方法": {
    en: "HTTP method",
    ja: "HTTP メソッド",
  },
  "对象 Key": {
    en: "Object key",
    ja: "オブジェクトキー",
  },
  分片编号: {
    en: "Part number",
    ja: "パート番号",
  },
  尝试次数: {
    en: "Attempts",
    ja: "試行回数",
  },
  "HTTP 状态码": {
    en: "HTTP status code",
    ja: "HTTP ステータスコード",
  },
  错误码: {
    en: "Error code",
    ja: "エラーコード",
  },
  "请求 ID": {
    en: "Request ID",
    ja: "リクエスト ID",
  },
  "扩展请求 ID": {
    en: "Extended request ID",
    ja: "拡張リクエスト ID",
  },
  处理阶段: {
    en: "Handling stage",
    ja: "処理段階",
  },
  "后续记录未更新：{value1}": {
    en: "Unable to update subsequent records: {value1}",
    ja: "後続の記録を更新できません：{value1}",
  },
  返回执行节点: {
    en: "Back to agents",
    ja: "実行ノードに戻る",
  },
  调节详情: {
    en: "Adjustment details",
    ja: "調整の詳細",
  },
  刷新调节详情: {
    en: "Refresh adjustment details",
    ja: "調整の詳細を更新",
  },
  "缺少节点或事件 ID。": {
    en: "Agent or event ID is missing.",
    ja: "ノードまたはイベント ID が指定されていません。",
  },
  "本轮部分记录未保存，以下详情不完整。": {
    en: "Some records from this adjustment were not saved. Details are incomplete.",
    ja: "この調整の一部の記録が保存されていません。以下の詳細は不完全です。",
  },
  "节点已重启，以下为历史记录，无法据此判断当前恢复状态。": {
    en: "The agent has restarted. These historical records do not reflect its current recovery status.",
    ja: "ノードは再起動されています。以下は過去の記録であり、現在の回復状態を判断することはできません。",
  },
  "自动刷新已暂停，以下为最近一次读取结果。": {
    en: "Auto-refresh is paused. Showing the last retrieved data.",
    ja: "自動更新は一時停止中です。最後に取得した結果を表示しています。",
  },
  "该节点曾收到 HTTP 503，已自动调整请求上限，最近读取时尚未恢复到目标。": {
    en: "This agent received HTTP 503 and automatically reduced its request limits. The last reading was still below the target.",
    ja: "このノードで HTTP 503 を受信し、リクエスト上限を自動的に引き下げました。直近の取得時点では目標値まで回復していません。",
  },
  "本轮调节已结束。": {
    en: "This adjustment has ended.",
    ja: "この調整は終了しました。",
  },
  手动调整: {
    en: "Manual adjustment",
    ja: "手動調整",
  },
  触发本次降速的请求: {
    en: "Request that triggered the reduction",
    ja: "上限引き下げの契機となったリクエスト",
  },
  调整时间: {
    en: "Adjusted at",
    ja: "調整日時",
  },
  触发时间: {
    en: "Triggered at",
    ja: "発生日時",
  },
  "事件 ID": {
    en: "Event ID",
    ja: "イベント ID",
  },
  "仅记录到迁移处理阶段，未确定具体 HTTP 请求。": {
    en: "Only the transfer handling stage was recorded; the specific HTTP request is unknown.",
    ja: "転送の処理段階のみ記録されており、具体的な HTTP リクエストは特定されていません。",
  },
  "未记录触发请求详情。": {
    en: "Trigger request details were not recorded.",
    ja: "契機となったリクエストの詳細は記録されていません。",
  },
  触发条件: {
    en: "Trigger conditions",
    ja: "発動条件",
  },
  "节点 503 计数 / 阈值": {
    en: "Agent 503 count / threshold",
    ja: "ノードの503回数 / しきい値",
  },
  降速幅度: {
    en: "Reduction",
    ja: "引き下げ率",
  },
  降速下限: {
    en: "Minimum limit",
    ja: "下限値",
  },
  "{value1} 次/秒（目标更低时保持目标）": {
    en: "{value1} requests/s (lower targets are preserved)",
    ja: "{value1}リクエスト/秒（目標値がさらに低い場合はその値を維持）",
  },
  恢复间隔: {
    en: "Recovery interval",
    ja: "回復間隔",
  },
  "{value1} 秒": {
    en: "{value1} seconds",
    ja: "{value1}秒",
  },
  每次恢复幅度: {
    en: "Increase per step",
    ja: "1回あたりの引き上げ率",
  },
  调整记录: {
    en: "Adjustment history",
    ja: "調整履歴",
  },
  定时恢复: {
    en: "Scheduled recovery",
    ja: "定期的な回復",
  },
  "正在加载…": {
    en: "Loading…",
    ja: "読み込み中…",
  },
  加载更多: {
    en: "Load more",
    ja: "さらに読み込む",
  },
  "保留原 Key": {
    en: "Keep original key",
    ja: "元のキーを維持",
  },
  替换源前缀: {
    en: "Replace source prefix",
    ja: "転送元プレフィックスを置換",
  },
  "指定目标 Key": {
    en: "Set destination key",
    ja: "転送先キーを指定",
  },
  "目标命名方式与迁移范围不匹配，请重新选择。": {
    en: "The naming rule does not match the transfer scope. Select another rule.",
    ja: "命名方式が転送範囲と一致しません。再選択してください。",
  },
  "请填写目标对象 Key。": {
    en: "Enter a destination object key.",
    ja: "転送先オブジェクトキーを入力してください。",
  },
  "请填写目标对象前缀。": {
    en: "Enter a destination object prefix.",
    ja: "転送先オブジェクトプレフィックスを入力してください。",
  },
  "替换源前缀需要填写源对象前缀。": {
    en: "Replacing the source prefix requires a source object prefix.",
    ja: "転送元プレフィックスの置換には転送元オブジェクトプレフィックスが必要です。",
  },
  "{value1} 必须是字符串。": {
    en: "{value1} must be a string.",
    ja: "{value1} は文字列にしてください。",
  },
  "prefix 与 sourceEndpoint.prefix 必须完全一致。": {
    en: "prefix must exactly match sourceEndpoint.prefix.",
    ja: "prefix は sourceEndpoint.prefix と完全に一致させてください。",
  },
  "objectKey 与 sourceEndpoint.objectKey 必须完全一致。": {
    en: "objectKey must exactly match sourceEndpoint.objectKey.",
    ja: "objectKey は sourceEndpoint.objectKey と完全に一致させてください。",
  },
  "子任务的 sourceEndpoint.prefix 需同时设置 prefixFilterEnabled:true，或改用顶层 prefix。":
    {
      en: "A subtask using sourceEndpoint.prefix must also set prefixFilterEnabled:true, or use the top-level prefix.",
      ja: "子タスクで sourceEndpoint.prefix を使用する場合は prefixFilterEnabled:true も設定するか、トップレベルの prefix を使用してください。",
    },
  "objectKey 与 prefix 不能同时填写。": {
    en: "objectKey and prefix are mutually exclusive.",
    ja: "objectKey と prefix は同時に指定できません。",
  },
  "keyMapping 必须是对象。": {
    en: "keyMapping must be an object.",
    ja: "keyMapping はオブジェクトにしてください。",
  },
  "keyMapping.mode 与迁移范围不匹配或不受支持。": {
    en: "keyMapping.mode is unsupported or does not match the transfer scope.",
    ja: "keyMapping.mode は未対応か、転送範囲と一致しません。",
  },
  "ADD_PREFIX 需要非空 targetPrefix。": {
    en: "ADD_PREFIX requires a non-empty targetPrefix.",
    ja: "ADD_PREFIX には空でない targetPrefix が必要です。",
  },
  "EXACT_KEY 需要单个源 objectKey 和非空 targetKey。": {
    en: "EXACT_KEY requires a single source objectKey and a non-empty targetKey.",
    ja: "EXACT_KEY には単一の転送元 objectKey と空でない targetKey が必要です。",
  },
  "REPLACE_PREFIX 的 sourcePrefix 必须与源 prefix 完全一致。": {
    en: "REPLACE_PREFIX sourcePrefix must exactly match the source prefix.",
    ja: "REPLACE_PREFIX の sourcePrefix は転送元の prefix と完全に一致させてください。",
  },
  "{value1} 不使用 {value2}，请移除此字段。": {
    en: "{value1} does not use {value2}. Remove this field.",
    ja: "{value1} では {value2} を使用しません。このフィールドを削除してください。",
  },
  不限速: {
    en: "Unlimited",
    ja: "制限なし",
  },
  带宽: {
    en: "Bandwidth",
    ja: "帯域幅",
  },
  "{value1} 自动调节后，上限已手动更新。": {
    en: "{value1}: limits were updated manually after automatic adjustment.",
    ja: "{value1}：自動調整後に上限が手動で更新されました。",
  },
  "{value1} 已恢复到目标上限。": {
    en: "{value1}: restored to the target limit.",
    ja: "{value1}：目標の上限まで回復しました。",
  },
  "{value1} 正在恢复请求上限。": {
    en: "{value1}: restoring request limits.",
    ja: "{value1}：リクエスト上限を回復中です。",
  },
  "{value1} 可能遭遇存储服务限速，已自动降低请求上限。": {
    en: "{value1}: possible storage throttling; request limits were reduced automatically.",
    ja: "{value1}：ストレージ側で制限された可能性があるため、リクエスト上限を自動的に引き下げました。",
  },
  "带宽上限需为 0.1–1,000,000 Mbps，或留空。": {
    en: "Bandwidth must be 0.1–1,000,000 Mbps, or left empty.",
    ja: "帯域幅上限は 0.1–1,000,000 Mbps にするか、空欄にしてください。",
  },
  "报告下载票据无效，请重试。": {
    en: "Invalid report download ticket. Please retry.",
    ja: "レポートのダウンロードチケットが無効です。再試行してください。",
  },
  任务报告下载: {
    en: "Task report download",
    ja: "タスクレポートのダウンロード",
  },
  "服务未返回报告文件，请重试。": {
    en: "The server did not return a report file. Please retry.",
    ja: "サーバーからレポートファイルが返されませんでした。再試行してください。",
  },
  "无法读取报告下载结果，请检查浏览器下载列表或重试。": {
    en: "Unable to read the download result. Check your browser downloads or retry.",
    ja: "ダウンロード結果を確認できません。ブラウザーのダウンロード一覧を確認するか、再試行してください。",
  },
  每节点: {
    en: "per agent",
    ja: "ノードごと",
  },
  任务总计: {
    en: "task total",
    ja: "タスク全体",
  },
  "请求上限（{value1}）": {
    en: "Request limit ({value1})",
    ja: "リクエスト上限（{value1}）",
  },
  "{value1} 次/秒": {
    en: "{value1} requests/s",
    ja: "{value1}リクエスト/秒",
  },
  "带宽上限（{value1}）": {
    en: "Bandwidth limit ({value1})",
    ja: "帯域幅上限（{value1}）",
  },
  "{operation} {count} 个任务？": {
    en: "{operation} {count} tasks?",
    ja: "{count}件のタスクを{operation}しますか？",
  },
  "已加载 {count} 个任务。统计仅包含最近一页。": {
    en: "Loaded {count} tasks. Statistics cover only the most recent page.",
    ja: "{count}件のタスクを読み込みました。統計には直近のページのみ含まれます。",
  },
  "{count} 个可选": {
    en: "{count} available",
    ja: "{count}件選択可能",
  },
  "无法获取存储桶列表：{error}。如有对应存储桶的访问权限，可直接填写名称。": {
    en: "Unable to fetch buckets: {error}. You can enter a bucket name directly if you have access.",
    ja: "バケット一覧を取得できません：{error}。アクセス権限がある場合は、バケット名を直接入力できます。",
  },
  "通过 {passed} / {total} 个节点配置。修改配置后需重新预检。": {
    en: "{passed} / {total} agents passed. Run preflight again after changing the configuration.",
    ja: "{total}台中{passed}台のノードが合格しました。設定を変更した場合は事前チェックを再実行してください。",
  },
  "{count} 条": {
    en: "{count} records",
    ja: "{count}件",
  },
  "共查询到 {total} 条记录，当前加载 {loaded} 条。可提高每节点读取上限或缩小筛选范围。":
    {
      en: "Found {total} records; loaded {loaded}. Increase the per-agent limit or narrow the filters.",
      ja: "全{total}件中{loaded}件を読み込みました。ノードあたりの取得上限を増やすか、フィルターで絞り込んでください。",
    },
  "清除全部降级记录？": {
    en: "Clear all fallback records?",
    ja: "フォールバック記録をすべて削除しますか？",
  },
  "清除全部失败记录？": {
    en: "Clear all failure records?",
    ja: "失敗記録をすべて削除しますか？",
  },

  "已迁移 {transferred} · 失败 {failed}": {
    en: "Transferred {transferred} · Failed {failed}",
    ja: "転送済み {transferred} · 失敗 {failed}",
  },
  "执行节点 {index}": {
    en: "Execution node {index}",
    ja: "実行ノード {index}",
  },
  "部分节点未返回数据，当前结果不完整。不可达节点：{nodes}": {
    en: "Some agents did not return data. Results are incomplete. Unreachable agents: {nodes}",
    ja: "一部のノードからデータが返されませんでした。結果は不完全です。接続できないノード：{nodes}",
  },
  "记录清除后无法恢复。清除范围也包括本地迁移产生的失败对象。仅删除异常记录，不会重试迁移任务。请先导出需保留的记录。":
    {
      en: "Cleared records cannot be restored. This includes failed objects from local transfers. Only exception records are deleted; tasks are not retried. Export any records you need first.",
      ja: "削除した記録は復元できません。ローカル転送の失敗オブジェクトも対象に含まれます。エラー記録のみ削除し、タスクは再試行しません。必要な記録は先にエクスポートしてください。",
    },
  "记录清除后无法恢复。仅删除异常记录，不会重试迁移任务。请先导出需保留的记录。":
    {
      en: "Cleared records cannot be restored. Only exception records are deleted; tasks are not retried. Export any records you need first.",
      ja: "削除した記録は復元できません。エラー記録のみ削除し、タスクは再試行しません。必要な記録は先にエクスポートしてください。",
    },
  "QPS 必须为 0 或 0.000001–1,000,000,000 之间的数值。": {
    en: "QPS must be 0 or between 0.000001 and 1,000,000,000.",
    ja: "QPS は 0、または 0.000001〜1,000,000,000 の範囲で指定してください。",
  },
  "requestLimits 必须是对象。": {
    en: "requestLimits must be an object.",
    ja: "requestLimits はオブジェクトで指定してください。",
  },
  "requestLimits.scope 必须为 CLUSTER 或 PER_NODE。": {
    en: "requestLimits.scope must be CLUSTER or PER_NODE.",
    ja: "requestLimits.scope は CLUSTER または PER_NODE を指定してください。",
  },
  选择执行节点: { en: "Select an execution node", ja: "実行ノードを選択" },
  "冲突处理、节点分发、时间与限速": {
    en: "Conflicts, node distribution, dates and limits",
    ja: "競合処理、ノード割り当て、日時と速度制限",
  },
  任务限速: { en: "Task limits", ja: "タスクの速度制限" },
  所有执行节点合计: {
    en: "Shared across execution nodes",
    ja: "全実行ノードの合計",
  },
  "多个节点必须访问同一共享存储中的同一份数据，挂载路径可以不同。": {
    en: "All nodes must access the same data on shared storage. Mount paths may differ.",
    ja: "すべてのノードから同じ共有ストレージ上の同じデータにアクセスできる必要があります。マウントパスは異なっていても構いません。",
  },
  "任务限速已更新。": {
    en: "Task limits updated.",
    ja: "タスクの速度制限を更新しました。",
  },
  "已核对：任务限速已生效。": {
    en: "Verified: task limits are in effect.",
    ja: "確認済み：タスクの速度制限が適用されています。",
  },
  子任务筛选: { en: "Filter subtasks", ja: "サブタスクの絞り込み" },
  进行中: { en: "Active", ja: "進行中" },
  从头查看子任务: {
    en: "Show subtasks from the start",
    ja: "最初のサブタスクから表示",
  },
  暂无进行中的子任务: {
    en: "No active subtasks",
    ja: "進行中のサブタスクはありません",
  },
  向下滚动加载更多: {
    en: "Scroll down to load more",
    ja: "下にスクロールして続きを表示",
  },
  "已加载全部 {count} 个子任务": {
    en: "All {count} subtasks loaded",
    ja: "全 {count} 件のサブタスクを表示中",
  },
  全部: { en: "All", ja: "すべて" },
};
