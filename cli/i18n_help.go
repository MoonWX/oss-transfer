package main

// Select translated help through the same router used for Chinese, including aliases.
func helpForLocale(args []string, locale cliLocale) string {
	source := helpFor(args)
	if translated, ok := cliHelpTranslations[source]; ok {
		if locale == localeEN {
			return translated[0]
		}
		if locale == localeJA {
			return translated[1]
		}
	}
	return source
}

var cliHelpTranslations = map[string][2]string{
	cliConfigHelp: {`Local preferences
Usage:
  oss-transfer-cli config get language
  oss-transfer-cli config set language auto|zh|en|ja

The default follows the system locale. Save a language once for later commands; auto restores system selection.
--lang and OSS_TRANSFER_LANG override it for the current invocation; auto temporarily uses the system language.
`, `ローカル設定
使い方:
  oss-transfer-cli config get language
  oss-transfer-cli config set language auto|zh|en|ja

既定はシステム言語です。一度保存すると以降のコマンドで使用し、auto で自動選択に戻します。
--lang と OSS_TRANSFER_LANG は今回の設定を上書きし、auto は一時的にシステム言語を使用します。
`},
	cliHelp: {`OSS Transfer CLI
Usage: oss-transfer-cli [global options] <command> [arguments]

Tasks
  task list                       List parent tasks, status and progress
  task show <ID>                  Show task details and the first child task page
  task children <ID>              Page through child tasks, starting at ID-1
  task recover-child <ID-N>       Try to resume a recoverable subtask now
  task create <direction> ...     Create an OSS migration, download or upload
  task import <type> <file>       Import tasks in bulk
  task stop|resume|start|retry <ID...>
  task delete <ID...>             Move records to trash
  task trash                      List deleted task records
  task restore <ID...>             Restore selected tasks from the trash
  task purge <ID...> --yes         Permanently remove selected trash records
  task bandwidth <ID> [Mbps|unlimited]
  task limits <ID> [--list-qps N --head-qps N --put-qps N]
  task report <ID> [--output file.zip]

Nodes and storage
  node list|show|details|network|add|invite|update|remove
  agent invite|list|show|details|update|remove
  storage list|add|update|remove
  storage buckets|test <alias>

Limits and diagnostics
  limit list|set|history|adjustment
  exceptions list|export|clear <failed|fallback|local>
  object compare ...              Compare buckets, prefixes or objects

Account
  login --username admin --password-stdin
  logout                          End the session for this service
  whoami                          Show login status
  change-password --old-password-env OLD --new-password-env NEW

Local preferences
  config get language             Show the saved language
  config set language auto|zh|en|ja  Save a language; default: system locale

Inventory tools
  diff-objects <source-dir> <sorted-target-manifest> <output-dir>
  count-lines <directory>
  import-inventory <source-alias/bucket> <target-alias/bucket> <manifest-dir> [--overwrite]

Global options
  --host <http://IP:1685>          Default: http://localhost:1685
  --lang auto|zh|en|ja             Override language for this invocation
  --json                          Structured JSON; one line per --watch refresh
  --verbose                       Write request paths and HTTP status to stderr
  --watch <seconds|5s>            Refresh read-only queries every 1–3600 seconds
  --version                       Show build version
  --help                          Show help; also: help <command group>

Examples
  oss-transfer-cli task list --watch 5
  oss-transfer-cli task show 224 --child 224-1
  oss-transfer-cli task resume 224,225
  oss-transfer-cli task bandwidth 224 200
  oss-transfer-cli task report 224 --output task-224-report.zip
  oss-transfer-cli help task create

Select parent IDs explicitly: 224,225 or 224-226. Commands never default to all tasks.
New tasks start automatically. Acceptance or prechecks do not confirm completion.
Removing nodes/connections, purging tasks and clearing exceptions require --yes.
Exit codes: 0 success, 1 operation failed, 2 command error, 3 incomplete, 130 interrupted.
OSS_TRANSFER_MASTER_SERVER sets the address. Use files or environment variables for credentials.
OSS_TRANSFER_CLI_CONFIG_DIR sets the configuration and session directory; use an absolute path.
Language follows the system, with English as fallback. Save a preference with config set language en; auto restores system selection.
--lang and OSS_TRANSFER_LANG override the saved preference. JSON keys, enums and user data remain unchanged.
Help groups: task, task create, task import, node, agent, storage, limit, exceptions, object, login, config, tools.
`, `OSS Transfer CLI
使い方: oss-transfer-cli [グローバルオプション] <コマンド> [引数]

タスク
  task list                       親タスク、状態、進捗を表示
  task show <ID>                  詳細と子タスクの最初のページを表示
  task children <ID>              子タスクをページ取得（ID-1 から）
  task recover-child <ID-N>       復旧待ちの子タスクの再開を試行
  task create <方向> ...          OSS 間転送、ダウンロード、アップロードを作成
  task import <種別> <ファイル>   タスクを一括インポート
  task stop|resume|start|retry <ID...>
  task delete <ID...>             記録をごみ箱に移動
  task trash                      ごみ箱の記録を表示
  task restore <ID...>             指定したタスクをごみ箱から復元
  task purge <ID...> --yes         指定したごみ箱の記録を完全削除
  task bandwidth <ID> [Mbps|unlimited]
  task limits <ID> [--list-qps N --head-qps N --put-qps N]
  task report <ID> [--output ファイル.zip]

ノードとストレージ
  node list|show|details|network|add|invite|update|remove
  agent invite|list|show|details|update|remove
  storage list|add|update|remove
  storage buckets|test <接続名>

制限と診断
  limit list|set|history|adjustment
  exceptions list|export|clear <failed|fallback|local>
  object compare ...              バケット、プレフィックス、オブジェクトの比較

アカウント
  login --username admin --password-stdin
  logout                          現在のサービスからログアウト
  whoami                          ログイン状態を表示
  change-password --old-password-env OLD --new-password-env NEW

ローカル設定
  config get language             保存した言語を表示
  config set language auto|zh|en|ja  言語を保存（既定はシステム言語）

マニフェスト
  diff-objects <転送元ディレクトリ> <ソート済み転送先マニフェスト> <出力ディレクトリ>
  count-lines <ディレクトリ>
  import-inventory <転送元接続名/バケット> <転送先接続名/バケット> <マニフェストディレクトリ> [--overwrite]

グローバルオプション
  --host <http://IP:1685>          既定: http://localhost:1685
  --lang auto|zh|en|ja             今回の表示言語を指定
  --json                          JSON で出力。--watch 時は更新ごとに 1 行
  --verbose                       リクエストパスと HTTP 状態を標準エラーに出力
  --watch <秒|5s>                 読み取り専用の照会を 1～3600 秒ごとに更新
  --version                       ビルド情報を表示
  --help                          ヘルプを表示。help <コマンドグループ> も使用可能

例
  oss-transfer-cli task list --watch 5
  oss-transfer-cli task show 224 --child 224-1
  oss-transfer-cli task resume 224,225
  oss-transfer-cli task bandwidth 224 200
  oss-transfer-cli task report 224 --output task-224-report.zip
  oss-transfer-cli help task create

親 ID は 224,225 や 224-226 のように明示してください。既定で全タスクを選択しません。
新規タスクは自動開始します。受理や事前チェックの成功は転送完了を示しません。
ノード・接続の削除、タスクの完全削除、例外記録のクリアには --yes が必要です。
終了コード: 0 成功、1 操作失敗、2 コマンドエラー、3 不完全な結果、130 中断。
OSS_TRANSFER_MASTER_SERVER で接続先を設定します。認証情報はファイルか環境変数を使用します。
OSS_TRANSFER_CLI_CONFIG_DIR は設定とセッションの保存先です。絶対パスで指定してください。
既定はシステム言語で、判定できない場合は英語です。config set language ja で保存し、auto で自動選択に戻します。
--lang と OSS_TRANSFER_LANG は保存した設定より優先されます。JSON のキー、列挙値、利用者のデータは変わりません。
ヘルプ: task、task create、task import、node、agent、setup、storage、limit、exceptions、object、login、config、tools。
`},
	cliTaskHelp: {`Task management
Usage: oss-transfer-cli task <subcommand> [arguments]

  list [--status STATUS] [--source BUCKET] [--dest BUCKET] [--worker NODE]
       [--direction oss-to-oss|oss-to-local|local-to-oss] [--search TEXT]
       [--limit 100] [--before-id ID]
  show <ID> [--offset 0] [--limit 25]
  show <ID> --child ID-N [--node NODE]
  children <ID> [--offset 0] [--limit 25]
  recover-child <ID-N>            Try to resume a subtask within the node's concurrency limit
  trash [--task-id ID] [--worker NODE] [--direction DIRECTION] [--search TEXT]
        [--limit 100] [--before-id ID]

  stop <ID...>                     Stop and retain progress
  resume|retry <ID...>             Resume stopped/failed tasks, retaining completed work
  start <ID...>                    Start pending tasks not started automatically
  delete <ID...>                   Move records to trash
  restore <ID...>                  Restore selected tasks from the trash
  purge <ID...> --yes              Permanently remove selected trash records
  limits <ID> [options]            Show or update request QPS and bandwidth
      --list-qps N --head-qps N --put-qps N --bandwidth-mbps N
      --list-qps covers both LIST operations and GET object reads.
      Use 0 or unlimited to remove a QPS limit; omitted fields stay unchanged.
  bandwidth <ID>                  Show the task bandwidth limit
  bandwidth <ID> <Mbps|unlimited>  Set/remove the task's additional limit
  report <ID> [--output file.zip]  Export an ended or stopped task's ZIP report

  create oss-to-oss <source-alias/bucket> <target-alias/bucket> [options]
  create oss-to-local <source-alias/bucket> --binding node=/target/path [options]
  create local-to-oss <target-alias/bucket> --binding node=/source/path [options]
  import buckets|tasks|local <file> [options]
  See help task create and help task import.

States: RUNNING, STOPPING, RESUMING, COMPLETED, PARTIAL_FAILED, FAILED, STOPPED.
Actions accept parent IDs: 224,225 or 224-226. Incomplete node status prevents task controls.
Child pages default to 25 IDs; --limit is 1–200, --offset skips IDs without renumbering.
list/trash accept --limit 1–10000; --before-id loads older records.
--child loads only the selected child. Use --node when the ID exists on multiple nodes.
--watch supports list/show/children/trash and bandwidth/limits without a new value.
Deleting records does not delete source or target objects.
`, `タスク管理
使い方: oss-transfer-cli task <サブコマンド> [引数]

  list [--status 状態] [--source バケット] [--dest バケット] [--worker ノード]
       [--direction oss-to-oss|oss-to-local|local-to-oss] [--search 文字列]
       [--limit 100] [--before-id ID]
  show <ID> [--offset 0] [--limit 25]
  show <ID> --child ID-N [--node ノード]
  children <ID> [--offset 0] [--limit 25]
  recover-child <ID-N>            ノードの同時実行数制限内で子タスクの再開を試行
  trash [--task-id ID] [--worker ノード] [--direction 方向] [--search 文字列]
        [--limit 100] [--before-id ID]

  stop <ID...>                     進捗を保持して停止
  resume|retry <ID...>             完了済みの処理を保持して停止・失敗タスクを再開
  start <ID...>                    自動開始していない待機タスクを開始
  delete <ID...>                   記録をごみ箱に移動
  restore <ID...>                  指定したタスクをごみ箱から復元
  purge <ID...> --yes              ごみ箱の指定記録を完全削除
  limits <ID> [オプション]         リクエスト QPS と帯域幅を表示・変更
      --list-qps N --head-qps N --put-qps N --bandwidth-mbps N
      --list-qps は LIST 列挙と GET オブジェクト読み取りの両方に適用。
      QPS は 0 または unlimited で解除。未指定の項目は保持します。
  bandwidth <ID>                  タスクの帯域幅上限を表示
  bandwidth <ID> <Mbps|unlimited>  タスクの追加上限を設定・解除
  report <ID> [--output file.zip]  終了・停止したタスクの ZIP レポートを出力

  create oss-to-oss <転送元接続名/バケット> <転送先接続名/バケット> [オプション]
  create oss-to-local <転送元接続名/バケット> --binding ノード=/転送先パス [オプション]
  create local-to-oss <転送先接続名/バケット> --binding ノード=/転送元パス [オプション]
  import buckets|tasks|local <ファイル> [オプション]
  詳細は help task create と help task import を参照してください。

状態: RUNNING、STOPPING、RESUMING、COMPLETED、PARTIAL_FAILED、FAILED、STOPPED。
操作には親 ID（224,225 または 224-226）を指定します。ノード情報が不完全なら実行しません。
子タスクは既定で 25 番号ずつ取得します。--limit は 1～200、--offset は番号を変えずに取得位置を指定します。
list/trash の --limit は 1～10000、--before-id は指定 ID より古い記録を取得します。
--child は指定した子タスクのみ取得します。同じ ID が複数ノードにある場合は --node を指定してください。
--watch は list/show/children/trash と、新しい値を指定しない bandwidth/limits に対応します。
記録を削除しても転送元や転送先のオブジェクトは削除されません。
`},
	cliTaskCreateHelp: {`Create a transfer task
  oss-transfer-cli task create oss-to-oss <source-alias/bucket> <target-alias/bucket> [options]
  oss-transfer-cli task create oss-to-local <source-alias/bucket> --binding node=/target/path [options]
  oss-transfer-cli task create local-to-oss <target-alias/bucket> --binding node=/source/path [options]

  --conflict skip|overwrite         Existing-object policy; default skip
  --overwrite                       Same as --conflict overwrite
  --distribution affinity|balanced  Default affinity
  --balanced                        Same as --distribution balanced
  --date-after YYYY-MM-DDTHH:mm:ss   Modified after this time
  --date-before YYYY-MM-DDTHH:mm:ss  Modified before this time; later than the start
  --bandwidth-mbps <value>           Additional task limit, 0.1–1000000 Mbps
  --list-qps/--head-qps/--put-qps <value>  Task request limits across all selected nodes
      --list-qps covers both LIST operations and GET object reads.
  --request-id <ID>                  Reuse an attempt; generated when omitted

OSS-to-OSS
  --node <node[,node...]>            Repeatable; default all available nodes
  --prefix <prefix>                 Select a prefix; conflicts with --object-key
  --object-key <key>                Transfer one object
  --mapping preserve               Keep original keys (default)
  --mapping add-prefix --target-prefix <prefix>
  --mapping replace-prefix --prefix <source-prefix> [--target-prefix <target-prefix>]
  --mapping exact-key --object-key <source-key> --target-key <target-key>

Local transfers
  --binding <node=absolute-path>     Bind paths per node; repeatable
  --validate-only                   Precheck nodes and paths without creating tasks
  Download: --prefix <prefix> or --object-key <key>
            --target-relative-path <path> for a single-object download
  Upload: --target-prefix <prefix> or --target-key <key>
          An exact target key requires one file on one node.
  Upload distribution follows the binding count; multiple nodes must bind the same logical directory.

Register connection aliases with storage first. Paths belong to execution nodes;
/data, C:\data and UNC paths are supported. Local transfers run prechecks before submission.
Accepted tasks start automatically; query task status for completion.
After a lost response, retry Go services with identical arguments and the returned --request-id.
Use a new ID after changing arguments or for another task. Legacy V4 does not deduplicate;
check its task list before retrying. Creation is never retried automatically.
`, `転送タスクの作成
  oss-transfer-cli task create oss-to-oss <転送元接続名/バケット> <転送先接続名/バケット> [オプション]
  oss-transfer-cli task create oss-to-local <転送元接続名/バケット> --binding ノード=/転送先パス [オプション]
  oss-transfer-cli task create local-to-oss <転送先接続名/バケット> --binding ノード=/転送元パス [オプション]

  --conflict skip|overwrite         既存オブジェクトの処理。既定: skip
  --overwrite                       --conflict overwrite と同じ
  --distribution affinity|balanced  既定: affinity
  --balanced                        --distribution balanced と同じ
  --date-after YYYY-MM-DDTHH:mm:ss   更新日時の開始条件
  --date-before YYYY-MM-DDTHH:mm:ss  更新日時の終了条件。開始より後を指定
  --bandwidth-mbps <値>              タスクの追加上限。0.1～1000000 Mbps
  --list-qps/--head-qps/--put-qps <値>  選択した全ノード合計のタスクリクエスト上限
      --list-qps は LIST 列挙と GET オブジェクト読み取りの両方に適用。
  --request-id <ID>                  同じ作成試行の ID を使用。省略時は自動生成

OSS 間転送
  --node <ノード[,ノード...]>        複数回指定可能。既定は利用可能な全ノード
  --prefix <プレフィックス>          転送範囲。--object-key とは併用不可
  --object-key <Key>                単一オブジェクトを転送
  --mapping preserve               元の Key を保持（既定）
  --mapping add-prefix --target-prefix <プレフィックス>
  --mapping replace-prefix --prefix <転送元プレフィックス> [--target-prefix <転送先プレフィックス>]
  --mapping exact-key --object-key <転送元 Key> --target-key <転送先 Key>

ローカル転送
  --binding <ノード=絶対パス>        ノードごとのパス。複数回指定可能
  --validate-only                   作成せずにノードとパスを事前チェック
  ダウンロード: --prefix <プレフィックス> または --object-key <Key>
                --target-relative-path <パス> は単一オブジェクトのみ
  アップロード: --target-prefix <プレフィックス> または --target-key <Key>
                単一 Key の指定は 1 ノード上の 1 ファイルに限ります。
  分配方式はノード数に従います。複数ノードには同じ論理ディレクトリを指定してください。

接続エイリアスは先に storage で登録してください。パスは実行ノード上のパスです。
/data、C:\data、UNC パスに対応します。ローカル転送は送信前に事前チェックを行います。
受理されたタスクは自動開始します。完了はタスク状態で確認してください。
応答を受信できなかった場合、Go サービスには同じ引数と返された --request-id で再試行できます。
引数を変えた場合や別タスクには新しい ID を使ってください。旧 V4 は重複排除に対応しないため、
再試行前に一覧を確認してください。作成リクエストは自動再試行されません。
`},
	cliTaskImportHelp: {`Bulk task import
Usage: oss-transfer-cli task import buckets|tasks|local <file> [options]

  buckets  Bucket migration requests: JSON array or headerless CSV
  tasks    OSS child task requests: JSON array
  local    Local transfer requests: JSON array; prechecked before submission

  --file <file>                 Alternative to the positional file argument
  --format json|csv             Inferred by extension; .csv/.txt use CSV
  --balanced                    Set distributionMode to balanced
  --validate-only               local only: precheck without creating tasks
  --request-id <ID>              Reuse this batch attempt; generated when omitted

CSV columns (no header; last three optional):
  source-alias/bucket,target-alias/bucket,dateAfter,overwrite,distributionMode
  source/photos,target/archive,2026-09-01T00:00:00,false,balanced

JSON uses WebUI API request fields. Example for buckets:
  [{"sourceS3Alias":"source","sourceBucket":"photos","destS3Alias":"target","destBucket":"archive","eligibleWorkerNodes":["10.0.0.2"],"distributionMode":"balanced"}]

  oss-transfer-cli task import buckets ./buckets.csv
  oss-transfer-cli task import local ./local-requests.json --validate-only

Requests must form a non-empty array. The server still validates nodes, credentials and paths.
On partial acceptance or lost responses, retry Go with the original file, arguments and --request-id.
Do not remove/reorder rows or change arguments while reusing the ID. Prechecks do not use request IDs.
Legacy V4 does not deduplicate; check its task list before retrying. Creation is never retried automatically.
`, `タスクの一括インポート
使い方: oss-transfer-cli task import buckets|tasks|local <ファイル> [オプション]

  buckets  バケット移行。JSON 配列またはヘッダーなし CSV
  tasks    OSS 子タスク。JSON 配列
  local    ローカル転送。JSON 配列。送信前に事前チェック

  --file <ファイル>              位置引数の代わりにファイルを指定
  --format json|csv             拡張子から判定。.csv/.txt は CSV
  --balanced                    distributionMode を balanced に設定
  --validate-only               local のみ。作成せずに事前チェック
  --request-id <ID>              一括作成の試行 ID を使用。省略時は自動生成

CSV の列（ヘッダーなし、最後の 3 列は省略可能）:
  転送元接続名/バケット,転送先接続名/バケット,dateAfter,overwrite,distributionMode
  source/photos,target/archive,2026-09-01T00:00:00,false,balanced

JSON は WebUI API の項目を使用します。buckets の例:
  [{"sourceS3Alias":"source","sourceBucket":"photos","destS3Alias":"target","destBucket":"archive","eligibleWorkerNodes":["10.0.0.2"],"distributionMode":"balanced"}]

  oss-transfer-cli task import buckets ./buckets.csv
  oss-transfer-cli task import local ./local-requests.json --validate-only

リクエストは空でない配列にしてください。サーバーでもノード、認証情報、パスを検証します。
一部受理や応答消失時は、Go に元のファイル、引数、--request-id で再試行できます。
同じ ID で行の削除・並べ替え・引数変更はしないでください。事前チェックには ID を使いません。
旧 V4 は重複排除に対応しません。再試行前に一覧を確認してください。作成は自動再試行されません。
`},
	cliNodeHelp: {`Execution nodes
Usage: oss-transfer-cli node <subcommand> [arguments]

  list                            Node configuration and connection status
  show <node>                     Node runtime status
  details <node>                  Host, interface IPs and mounted directories
  network [--public-url URL] [--agent-url URL]
  add <node> [--label name]        Generate Agent join commands
  invite [--address node] [--label name] [--master-url URL]
  update <node> [--address new-address] [--label name]
  update <node> --child-concurrency 8 --object-concurrency 4
  remove <node> --yes              Remove the registration

  oss-transfer-cli node list --watch 5
  oss-transfer-cli node add 10.0.0.2 --label data-node
  oss-transfer-cli node remove 10.0.0.2 --yes

Use the Node ID column from node list to select a node in commands. Add nodes with agent invite; --address accepts a name, IPv4 or IPv6 and is generated when omitted.
Agent addresses cannot be edited after registration; join again when replacing a node.
node network retains legacy address settings. New invitations use the current CLI connection address.
The server checks identity, active tasks and Master restrictions. Unresponsive nodes are listed.
Use --json for queue/raw rate data; use limit list for limits.
`, `実行ノード
使い方: oss-transfer-cli node <サブコマンド> [引数]

  list                            ノード設定と接続状態を表示
  show <ノード>                   稼働状態を表示
  details <ノード>                ホスト、インターフェース IP、マウントを表示
  network [--public-url URL] [--agent-url URL]
  add <ノード> [--label 名前]      Agent 参加コマンドを生成
  invite [--address ノード] [--label 名前] [--master-url URL]
  update <ノード> [--address 新アドレス] [--label 名前]
  update <ノード> --child-concurrency 8 --object-concurrency 4
  remove <ノード> --yes            登録を解除

  oss-transfer-cli node list --watch 5
  oss-transfer-cli node add 10.0.0.2 --label data-node
  oss-transfer-cli node remove 10.0.0.2 --yes

コマンドのノード指定には node list の「ノード ID」列を使います。追加は agent invite を使い、--address は名前、IPv4、IPv6 に対応します。省略時は自動生成されます。
登録済み Agent のアドレスは直接変更できません。置き換え時は再参加してください。
node network は旧版のアドレス設定を保持します。新しい招待は CLI の接続先を使用します。
サーバーが ID、実行中タスク、Master の制約を確認します。応答しないノードも表示します。
キューや速度の生データは --json、上限は limit list で確認できます。
`},
	cliAgentHelp: {`Agent management
Usage: oss-transfer-cli agent <subcommand> [arguments]

  invite [--label name] [--address node] [--master-url URL]
  list                            List registered Agents
  show <node>                     Connection, queues, roots and limits
  details <node>                  Host, interface IPs and mounted directories
  update <node> [--label name] [--local-root path ...]
  update <node> --clear-local-roots
  update <node> --child-concurrency 8 --object-concurrency 4
  remove <node> --yes              Revoke registration

  oss-transfer-cli agent invite --address storage-node --label transfer-node
  oss-transfer-cli agent update storage-node --local-root /mnt/data --local-root /mnt/archive
  oss-transfer-cli agent update windows-node --local-root 'D:\data'
  oss-transfer-cli agent remove storage-node --yes

--master-url sets the address Agents use to reach Master; it defaults to the current CLI connection address.
Invitations expire after 30 minutes and can be used once. Run the native installation command for the Agent OS; Docker is optional.
--local-root uses absolute paths on the Agent OS and replaces the existing root list.
`, `Agent 管理
使い方: oss-transfer-cli agent <サブコマンド> [引数]

  invite [--label 名前] [--address ノード] [--master-url URL]
  list                            登録済み Agent を表示
  show <ノード>                   接続、キュー、ルート、制限を表示
  details <ノード>                ホスト、インターフェース IP、マウントを表示
  update <ノード> [--label 名前] [--local-root パス ...]
  update <ノード> --clear-local-roots
  update <ノード> --child-concurrency 8 --object-concurrency 4
  remove <ノード> --yes            登録を解除

  oss-transfer-cli agent invite --address storage-node --label transfer-node
  oss-transfer-cli agent update storage-node --local-root /mnt/data --local-root /mnt/archive
  oss-transfer-cli agent update windows-node --local-root 'D:\data'
  oss-transfer-cli agent remove storage-node --yes

--master-url は Agent が接続する Master の URL です。省略時は CLI の接続先を使用します。
招待の有効期限は 30 分で、1 回のみ使用できます。Agent の OS に対応するネイティブインストールコマンドを実行してください。Docker は任意です。
--local-root は Agent の OS 上の絶対パスです。既存のルート一覧を置き換えます。
`},
	cliSetupHelp: {`First login
  oss-transfer-cli login
  oss-transfer-cli change-password

Use the admin password printed after installation, then change it.
setup status remains available for existing scripts. No setup step is needed.
`, `初回ログイン
  oss-transfer-cli login
  oss-transfer-cli change-password

インストール時に表示された admin の初期パスワードでログインし、変更してください。
`},
	cliStorageHelp: {`Storage connections
Usage: oss-transfer-cli storage <subcommand> [arguments]

  list                            List connections with masked credentials
  add <alias> --endpoint URL --access-key-env NAME --secret-key-env NAME
  update <alias> [fields]          Keep unspecified values
  remove <alias> --yes             Remove and sync to execution nodes
  buckets <alias>                 List buckets
  test <alias>                    Validate connection and bucket-list permission

  --endpoint <http[s]://host>
  --region <region>               S3 signing region
  --access-key-env <NAME>          Or --access-key-file <file>
  --secret-key-env <NAME>          Or --secret-key-file <file>
  --session-token-env <NAME>       Or --session-token-file <file>
  --clear-session-token           Remove the saved temporary credential token
  --validate true|false           Validate before saving; default true
  --v2-signer inherit|true|false   Signature mode; new default inherit
  --path-style inherit|true|false  Addressing mode; new default inherit

  oss-transfer-cli storage add source --endpoint https://oss.example.com --access-key-env OSS_ACCESS_KEY --secret-key-env OSS_SECRET_KEY
  oss-transfer-cli storage update source --endpoint https://new-oss.example.com
  oss-transfer-cli storage test source
  oss-transfer-cli storage remove source --yes

Credentials come from environment variables or files, not command-line values.
Check each node's sync result; partial success is reported as incomplete.
Connection tests validate listing; actual operations validate read and write permissions.
`, `ストレージ接続
使い方: oss-transfer-cli storage <サブコマンド> [引数]

  list                            認証情報を伏せて接続一覧を表示
  add <接続名> --endpoint URL --access-key-env NAME --secret-key-env NAME
  update <接続名> [変更項目]       未指定項目は現在の値を保持
  remove <接続名> --yes            削除して実行ノードに同期
  buckets <接続名>                バケット一覧を表示
  test <接続名>                   接続と一覧取得権限を検証

  --endpoint <http[s]://ホスト>
  --region <リージョン>            S3 署名リージョン
  --access-key-env <NAME>          または --access-key-file <ファイル>
  --secret-key-env <NAME>          または --secret-key-file <ファイル>
  --session-token-env <NAME>       または --session-token-file <ファイル>
  --clear-session-token           保存済みの一時認証トークンを削除
  --validate true|false           保存時に検証。既定: true
  --v2-signer inherit|true|false   署名方式。新規の既定: inherit
  --path-style inherit|true|false  アドレス方式。新規の既定: inherit

  oss-transfer-cli storage add source --endpoint https://oss.example.com --access-key-env OSS_ACCESS_KEY --secret-key-env OSS_SECRET_KEY
  oss-transfer-cli storage update source --endpoint https://new-oss.example.com
  oss-transfer-cli storage test source
  oss-transfer-cli storage remove source --yes

認証情報は環境変数またはファイルから読み取ります。コマンドラインには直接指定しません。
ノードごとの同期結果を確認してください。一部成功は不完全と表示します。
接続テストは一覧取得を検証します。読み取り・書き込み権限は実際の操作で検証します。
`},
	cliLimitHelp: {`Node rate limits
Usage: oss-transfer-cli limit <subcommand> [arguments]

  list                            Target/running limits and automatic adjustment status
  set <list|put|head|bandwidth> <value|unlimited> (--worker NODE|--all)
       [--unit bps|Kbps|Mbps|Gbps]
  history [--limit 50]             Recent changes; maximum 1000 per node
  adjustment --worker NODE --event-id ID [--cursor CURSOR]

  oss-transfer-cli limit set bandwidth 200 --worker 10.0.0.2
  oss-transfer-cli limit set bandwidth 1 --unit Gbps --all
  oss-transfer-cli limit set put 500 --all
  oss-transfer-cli limit set bandwidth unlimited --worker 10.0.0.2
  oss-transfer-cli limit list --watch 5
  oss-transfer-cli limit adjustment --worker 10.0.0.2 --event-id EVENT_ID

bandwidth defaults to Mbps; other limits use requests/second and reject --unit.
Values must be greater than zero; use unlimited to remove a limit.
Select one node or --all. Use task bandwidth for an additional task limit.
Adjustment details retain nextCursor, done, historyComplete and process restart information.
`, `ノードの速度制限
使い方: oss-transfer-cli limit <サブコマンド> [引数]

  list                            目標上限、動作上限、自動調整の状態を表示
  set <list|put|head|bandwidth> <値|unlimited> (--worker ノード|--all)
       [--unit bps|Kbps|Mbps|Gbps]
  history [--limit 50]             最近の調整。ノードごとに最大 1000 件
  adjustment --worker ノード --event-id ID [--cursor カーソル]

  oss-transfer-cli limit set bandwidth 200 --worker 10.0.0.2
  oss-transfer-cli limit set bandwidth 1 --unit Gbps --all
  oss-transfer-cli limit set put 500 --all
  oss-transfer-cli limit set bandwidth unlimited --worker 10.0.0.2
  oss-transfer-cli limit list --watch 5
  oss-transfer-cli limit adjustment --worker 10.0.0.2 --event-id EVENT_ID

bandwidth の既定単位は Mbps、他の制限は回/秒で、--unit は使えません。
値は 0 より大きくしてください。解除には unlimited を使用します。
対象ノードまたは --all を明示します。タスクの追加上限には task bandwidth を使用します。
詳細には nextCursor、done、historyComplete、プロセスの再起動情報が保持されます。
`},
	cliExceptionsHelp: {`Exception records
Usage: oss-transfer-cli exceptions list|export|clear <type> [options]

Types: failed (failed objects), fallback (legacy SDK records; unsupported by Go), local (local failures).
  list failed|fallback [--task-id ID] [--source-bucket BUCKET] [--dest-bucket BUCKET]
                       [--worker NODE] [--limit 1000]
  list local [--task-id ID] [--worker NODE] [--direction OSS_TO_LOCAL|LOCAL_TO_OSS]
             [--endpoint VALUE] [--storage-id ID] [--offset 0] [--limit 25]
  export failed|fallback --output file.csv [same filters as list]
  export local --output file.zip
  clear failed|local --task-id ID --yes
  clear failed|local --all --yes

CSV contains this query's records and completeness/truncation indicators; existing files are not overwritten.
Local ZIP exports all local task reports without filters; use task report for one task.
Only ended or stopped tasks can be exported. Clear removes recovered or skipped exception records;
unresolved failures and tasks remain. Use filters or --all, not both.
Filters: --task-id, --worker, --direction, --source-bucket, --dest-bucket, --endpoint.
complete/partial describe node coverage; truncated describes the size cap. Legacy caps may return
truncatedWorkers; use ZIP reports for more records. A downloaded report does not confirm transfer correctness.

  oss-transfer-cli exceptions export failed --task-id 224 --output task-224-failures.csv
  oss-transfer-cli exceptions list local --direction OSS_TO_LOCAL --offset 25 --limit 25
`, `例外記録
使い方: oss-transfer-cli exceptions list|export|clear <種別> [オプション]

種別: failed（失敗オブジェクト）、fallback（旧 SDK 記録。Go は未対応）、local（ローカル転送の失敗）。
  list failed|fallback [--task-id ID] [--source-bucket バケット] [--dest-bucket バケット]
                       [--worker ノード] [--limit 1000]
  list local [--task-id ID] [--worker ノード] [--direction OSS_TO_LOCAL|LOCAL_TO_OSS]
             [--endpoint 値] [--storage-id ID] [--offset 0] [--limit 25]
  export failed|fallback --output ファイル.csv [list と同じ絞り込み条件]
  export local --output ファイル.zip
  clear failed|local --task-id ID --yes
  clear failed|local --all --yes

CSV は今回の記録と完全性・件数省略の情報を出力します。既存ファイルは上書きしません。
ローカル ZIP は絞り込まず全ローカルタスクを出力します。単一タスクには task report を使用します。
出力できるのは終了・停止したタスクのみです。削除するのは回復済み・スキップ済みの例外記録です。
未解決の失敗とタスクは残ります。絞り込み条件と --all は併用できません。
絞り込み: --task-id、--worker、--direction、--source-bucket、--dest-bucket、--endpoint。
complete/partial は応答の完全性、truncated は件数上限による省略を示します。
truncatedWorkers が返された場合、詳細には ZIP レポートを使用してください。
ダウンロード成功は転送データの検証完了を示しません。

  oss-transfer-cli exceptions export failed --task-id 224 --output task-224-failures.csv
  oss-transfer-cli exceptions list local --direction OSS_TO_LOCAL --offset 25 --limit 25
`},
	cliObjectHelp: {`Compare objects
  oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --scope prefix --source-prefix '2026/' --dest-prefix 'backup/' --mode metadata
  oss-transfer-cli object compare --source-bucket source/photos --source-key '2026/a.jpg' --dest-bucket target/archive --dest-key 'backup/a.jpg' --mode content
  oss-transfer-cli object status <comparison-id> [--after 50 --limit 100]
  oss-transfer-cli object cancel <comparison-id>
  oss-transfer-cli object delete <comparison-id>

Empty prefixes compare whole buckets. Keys are matched after removing each prefix.
metadata checks size, Content-Type, Content-Encoding, Content-Disposition, Cache-Control and custom metadata without reading content.
content streams both complete objects into SHA-256 without writing object files locally. Read traffic is charged by your provider.
ETag, modification time and storage class do not determine equality across stores. Objects changed during comparison are reported separately.
Comparisons run in the background. status returns counts and paged differences; matching objects are counted only.
Each node retains 10 comparisons, with up to 10,000 difference rows each. All objects are counted. delete removes finished comparison records only.
Keys and prefixes are passed unchanged. Quote shell metacharacters and spaces.
`, `オブジェクトの比較
  oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --scope prefix --source-prefix '2026/' --dest-prefix 'backup/' --mode metadata
  oss-transfer-cli object compare --source-bucket source/photos --source-key '2026/a.jpg' --dest-bucket target/archive --dest-key 'backup/a.jpg' --mode content
  oss-transfer-cli object status <比較 ID> [--after 50 --limit 100]
  oss-transfer-cli object cancel <比較 ID>
  oss-transfer-cli object delete <比較 ID>

プレフィックスが空の場合はバケット全体を比較します。それぞれのプレフィックスを除いた Key で対応付けます。
metadata はサイズ、Content-Type、Content-Encoding、Content-Disposition、Cache-Control、カスタムメタデータを確認し、内容を読み取りません。
content は両側の内容を最後までストリームで読み、SHA-256 を計算します。ローカルファイルは作成しません。読み取り通信が発生します。
ETag、更新日時、ストレージクラスは一致判定に使用しません。比較中に変更されたオブジェクトは別途表示します。
比較はバックグラウンドで実行されます。status は集計と差分ページを返し、一致したオブジェクトは件数のみ保存します。
各ノードは直近 10 回の比較結果と、各比較の先頭 10,000 件の差分を保存します。集計は全件を含みます。delete は終了した比較記録のみ削除します。
Key とプレフィックスは変更せず渡します。空白やシェル特殊文字を含む場合は引用符で囲んでください。
`},
	cliAuthHelp: {`Login and account
  oss-transfer-cli login
  oss-transfer-cli change-password
  oss-transfer-cli login --username admin --password-stdin
  oss-transfer-cli login --username admin --password-env PASSWORD_ENV
  oss-transfer-cli login --username admin --password-file /path/to/password
  oss-transfer-cli whoami
  oss-transfer-cli logout
  oss-transfer-cli change-password --old-password-env OLD --new-password-env NEW

The username defaults to admin; use --username for another account. Terminal password input is hidden.
After the first login, run change-password. Scripts can select an environment variable, file, or stdin.
--password-stdin reads standard input; --password-file reads a file.
Changes also accept --old-password-file / --new-password-file.
Sessions are saved per --host. OSS_TRANSFER_AUTH_TOKEN supplies a user login token.
OSS_TRANSFER_INTERNAL_TOKEN is not accepted by the CLI.
`, `ログインとアカウント
  oss-transfer-cli login
  oss-transfer-cli change-password
  oss-transfer-cli login --username admin --password-stdin
  oss-transfer-cli login --username admin --password-env PASSWORD_ENV
  oss-transfer-cli login --username admin --password-file /path/to/password
  oss-transfer-cli whoami
  oss-transfer-cli logout
  oss-transfer-cli change-password --old-password-env OLD --new-password-env NEW

ユーザー名の既定値は admin です。他のアカウントは --username で指定します。入力したパスワードは表示されません。
初回ログイン後は change-password を実行してください。スクリプトでは環境変数、ファイル、標準入力を選択できます。
--password-stdin は標準入力、--password-file はファイルから読み取ります。
変更には --old-password-file / --new-password-file も使用できます。
セッションは --host ごとに保存します。OSS_TRANSFER_AUTH_TOKEN でログイントークンを指定できます。
CLI は OSS_TRANSFER_INTERNAL_TOKEN を受け付けません。
`},
	cliToolsHelp: {`Inventory tools
  oss-transfer-cli diff-objects <source-CSV-dir> <sorted-target-manifest> <output-dir>
      [--mode sort-merge|bloom] [--false-positive 0.01]
  oss-transfer-cli count-lines <directory>
  oss-transfer-cli import-inventory <source-alias/bucket> <target-alias/bucket> <local-manifest-dir>
      [--node NODE] [--overwrite] [--request-id ID]

diff-objects defaults to sort-merge. Bloom reduces memory use but can produce false positives.
Sort the target manifest by key. Use different input and output directories.
import-inventory reads local .incr files, one UTF-8 key per line, and creates one parent task per file.
Blank lines are ignored and duplicates removed; other keys are unchanged. Only the key list is submitted.
--node can be repeated; otherwise Master selects nodes. Go Master is required.
After interruption, retain original files and arguments and retry with the returned --request-id.
Already accepted tasks will not be created again.
`, `マニフェストツール
  oss-transfer-cli diff-objects <転送元 CSV ディレクトリ> <ソート済み転送先マニフェスト> <出力ディレクトリ>
      [--mode sort-merge|bloom] [--false-positive 0.01]
  oss-transfer-cli count-lines <ディレクトリ>
  oss-transfer-cli import-inventory <転送元接続名/バケット> <転送先接続名/バケット> <ローカルマニフェストディレクトリ>
      [--node ノード] [--overwrite] [--request-id ID]

diff-objects の既定はソートマージです。bloom はメモリー使用量を減らせますが偽陽性があります。
転送先マニフェストは Key 順にソートしてください。入力と出力は異なるディレクトリを使用します。
import-inventory は .incr ファイルの各行にある UTF-8 Key を読み取り、ファイルごとに親タスクを作成します。
空行は無視し重複を除去します。他の Key は変更せず、Key 一覧のみ送信します。
--node は複数回指定できます。省略時は Master が選択します。Go Master が必要です。
中断後は元のファイルと引数を保持し、返された --request-id で再試行してください。
受理済みのタスクを重複して作成しません。
`},
}
