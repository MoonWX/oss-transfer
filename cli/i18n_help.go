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
	cliTaskNodesHelp: {`Change task execution nodes
Usage:
  oss-transfer-cli task nodes <ID>
  oss-transfer-cli task nodes <ID> [--add NODE ...] [--remove NODE ...] [--binding NODE=/absolute/path ...] [--now] [--yes]

Without --add or --remove, shows the current execution nodes, their state, the scanner,
pending subtasks, each node's bandwidth and QPS share, removed nodes and Agents that can be added.

Options
  --add NODE           Add an execution node; repeat or separate with commas
  --remove NODE        Remove an execution node; repeat or separate with commas
  --binding NODE=PATH  Local transfers need one shared directory for each added node
  --now                Remove now: running work is revoked and continues on the remaining nodes;
                       by default a node leaves after its current work finishes
  --yes                Submit; without it the change and preflight results are only shown
  --request-id ID      Retry safely with the printed request ID when a result is unknown
  --expected-revision N  Reuse the printed membership revision when retrying

Notes
  The default removal assigns no new work, hands queued work to other nodes and leaves after running work finishes.
  Local transfers check the shared directory first; nothing is submitted when the preflight fails.
  While nodes are joining or draining, you can only cancel a join, remove a draining node now, or add it back.
  If someone else changed the membership first, the Master rejects the change; review it again and resubmit.
  Stopped or failed tasks can be changed and apply on resume; completed and single-object/single-file tasks cannot.

Examples
  oss-transfer-cli task nodes 224
  oss-transfer-cli task nodes 224 --add 10.0.0.3 --yes
  oss-transfer-cli task nodes 224 --remove 10.0.0.2 --now --yes
  oss-transfer-cli task nodes 225 --add 10.0.0.3 --binding 10.0.0.3=/mnt/share --yes
`, `タスクの実行ノードの変更
使い方:
  oss-transfer-cli task nodes <ID>
  oss-transfer-cli task nodes <ID> [--add ノード ...] [--remove ノード ...] [--binding ノード=/絶対パス ...] [--now] [--yes]

--add と --remove を指定しない場合は、現在の実行ノード、状態、スキャンノード、未完了の子タスク、
ノードごとの帯域と QPS の割り当て、削除済みノード、追加可能な Agent を表示します。

オプション
  --add ノード          実行ノードを追加。複数回指定またはカンマ区切り
  --remove ノード       実行ノードを削除。複数回指定またはカンマ区切り
  --binding ノード=パス  ローカル転送では追加する各ノードに共有ディレクトリを 1 つ指定
  --now                 即時削除: 実行中の処理を取り消し、残りのノードで継続。
                        既定では実行中の処理の完了後に削除
  --yes                 送信を確定。指定しない場合は変更内容と事前チェック結果のみ表示
  --request-id ID       結果が未確認のとき、表示されたリクエスト ID で安全に再試行
  --expected-revision N  再試行時に表示されたメンバー版を指定

説明
  既定の削除では新しい処理を割り当てず、待機中の処理を他のノードへ渡し、実行中の処理の完了後に離脱します。
  ローカル転送では先に共有ディレクトリを確認し、不合格の場合は送信しません。
  ノードの追加・削除が進行中は、追加の取り消し、削除中ノードの即時削除、または再追加による削除の取り消しのみ可能です。
  他の操作が先にメンバーを変更した場合、Master は変更を拒否します。再確認してから送信してください。
  停止・失敗したタスクも変更でき、再開時に反映されます。完了済みや単一オブジェクト/単一ファイルのタスクは変更できません。

例
  oss-transfer-cli task nodes 224
  oss-transfer-cli task nodes 224 --add 10.0.0.3 --yes
  oss-transfer-cli task nodes 224 --remove 10.0.0.2 --now --yes
  oss-transfer-cli task nodes 225 --add 10.0.0.3 --binding 10.0.0.3=/mnt/share --yes
`},
	cliUpdateHelp: {`Service updates
  oss-transfer-cli update status
  oss-transfer-cli update check
  oss-transfer-cli update master --yes
  oss-transfer-cli update master --version 4.1.21-beta.1 --allow-prerelease --yes
  oss-transfer-cli update agent --worker NODE --yes
  oss-transfer-cli update agents --all --yes

update check shows "current version → available version". update master checks for
a new version itself and, without --version, installs the latest stable release; it
reports and exits when the Master is already up to date.
Agents default to the Master's current version; --version selects another release.
Update checks offer stable releases only. A prerelease (beta/rc) is installed only
with an exact --version plus --allow-prerelease; Agents may follow a prerelease Master.
Follow queued updates with update status --watch 5. Acceptance is not completion.
`, `サービス更新
  oss-transfer-cli update status
  oss-transfer-cli update check
  oss-transfer-cli update master --yes
  oss-transfer-cli update master --version 4.1.21-beta.1 --allow-prerelease --yes
  oss-transfer-cli update agent --worker ノード --yes
  oss-transfer-cli update agents --all --yes

update check は「現在のバージョン → 更新可能なバージョン」を表示します。update master は
新しいバージョンを自動で確認し、--version を省略すると最新の安定版に更新します。
Master がすでに最新の場合は、その旨を表示して終了します。
Agent は既定で Master の現在のバージョンに更新します。--version で変更できます。
更新の確認では安定版のみを提示します。プレリリース（beta/rc）は --version で正確に
指定し --allow-prerelease を付けた場合のみインストールします。Master がプレリリースの
場合、Agent は同じバージョンに追従できます。
update status --watch 5 で結果を確認します。受付は更新完了を意味しません。
`},
	cliBackupHelp: {`Upgrade backups
  oss-transfer-cli backup list
  oss-transfer-cli backup prune master --yes
  oss-transfer-cli backup prune agent --worker NODE --yes

Removes only backups marked removable; recovery backups for failed upgrades remain.
`, `アップグレードのバックアップ
  oss-transfer-cli backup list
  oss-transfer-cli backup prune master --yes
  oss-transfer-cli backup prune agent --worker ノード --yes

削除可能なバックアップのみ削除し、失敗した更新の復元用バックアップは残します。
`},

	cliConfigHelp: {`Local preferences
Usage:
  oss-transfer-cli config get master
  oss-transfer-cli config set master https://transfer.example.com
  oss-transfer-cli config set master auto
  oss-transfer-cli config get language
  oss-transfer-cli config set language auto|zh|en|ja

Master is detected from the local installation. Save another URL once, or use auto to restore detection.
The default language follows the system locale. Save a language once for later commands; auto restores system selection.
--lang and OSS_TRANSFER_LANG override it for the current invocation; auto temporarily uses the system language.
`, `ローカル設定
使い方:
  oss-transfer-cli config get master
  oss-transfer-cli config set master https://transfer.example.com
  oss-transfer-cli config set master auto
  oss-transfer-cli config get language
  oss-transfer-cli config set language auto|zh|en|ja

Master はローカルインストールから検出します。別の URL を保存でき、auto で自動検出に戻せます。
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
  task restore <ID...>            Restore selected tasks from the trash
  task purge <ID...> --yes        Permanently remove selected trash records
  task bandwidth <ID> [Mbps|unlimited]
  task limits <ID> [--list-qps N --head-qps N --put-qps N]
  task nodes <ID> [--add NODE] [--remove NODE [--now]] [--yes]  Show or change execution nodes
  task report <ID> [--output file.zip]

Nodes and storage
  node list|show|details|network|add|invite|update|remove
  agent invite|invite-status|list|show|details|update|remove
  storage list|add|update|remove
  storage buckets|test <alias>

Limits and diagnostics
  limit list|set|history|adjustment
  exceptions list|export|clear <failed|fallback|local>
  update status|check
  update master|agent|agents ...
  backup list|prune ...
  object compare ...              Compare buckets, prefixes or objects
  object list|status|export|resume|cancel|delete  List, inspect, export or resume comparisons

Account
  login --username admin --password-stdin
  logout                          End the session for this service
  whoami                          Show login status
  change-password --old-password-env OLD --new-password-env NEW

Local preferences
  config get master               Show the current Master and address source
  config set master URL|auto      Save another Master or use automatic discovery
  config get language             Show the saved language
  config set language auto|zh|en|ja  Save a language; default: system locale

Inventory tools
  diff-objects <source-dir> <sorted-target-manifest> <output-dir>
  count-lines <directory>
  import-inventory <source-alias/bucket> <target-alias/bucket> <manifest-dir> [--overwrite]

Global options
  --host <URL>                    Override the automatically detected cluster Master
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
On an installed Master or Agent, the service owner or system administrator can use the local cluster without login. Other clusters require login.
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
  task restore <ID...>            指定したタスクをごみ箱から復元
  task purge <ID...> --yes        指定したごみ箱の記録を完全削除
  task bandwidth <ID> [Mbps|unlimited]
  task limits <ID> [--list-qps N --head-qps N --put-qps N]
  task nodes <ID> [--add ノード] [--remove ノード [--now]] [--yes]  実行ノードを表示・変更
  task report <ID> [--output ファイル.zip]

ノードとストレージ
  node list|show|details|network|add|invite|update|remove
  agent invite|invite-status|list|show|details|update|remove
  storage list|add|update|remove
  storage buckets|test <接続名>

制限と診断
  limit list|set|history|adjustment
  exceptions list|export|clear <failed|fallback|local>
  update status|check
  update master|agent|agents ...
  backup list|prune ...
  object compare ...              バケット、プレフィックス、オブジェクトの比較
  object list|status|export|resume|cancel|delete  比較の一覧、確認、エクスポート、再開

アカウント
  login --username admin --password-stdin
  logout                          現在のサービスからログアウト
  whoami                          ログイン状態を表示
  change-password --old-password-env OLD --new-password-env NEW

ローカル設定
  config get master               Master のアドレスと設定元を表示
  config set master URL|auto      接続先を保存、または自動検出に戻す
  config get language             保存した言語を表示
  config set language auto|zh|en|ja  言語を保存（既定はシステム言語）

マニフェスト
  diff-objects <転送元ディレクトリ> <ソート済み転送先マニフェスト> <出力ディレクトリ>
  count-lines <ディレクトリ>
  import-inventory <転送元接続名/バケット> <転送先接続名/バケット> <マニフェストディレクトリ> [--overwrite]

グローバルオプション
  --host <URL>                    自動検出したクラスタの Master を一時的に変更
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

Master または Agent の実行ユーザーやシステム管理者は、本機のクラスタをログインなしで利用できます。別クラスタには login が必要です。
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
       [--limit 100] [--before-id ID] [--all]
  show <ID> [--offset 0] [--limit 25]
  show <ID> --child ID-N [--node NODE]
  children <ID> [--active] [--order priority|id] [--cursor CURSOR] [--offset 0] [--limit 25]
  progress <ID> [--child ID-N] [--generation N]
  cleanup-status <ID...>
  retry --all --yes
  recover-child <ID-N>            Try to resume a subtask within the node's concurrency limit
  trash [--task-id ID] [--worker NODE] [--direction DIRECTION] [--search TEXT]
        [--limit 100] [--before-id ID] [--all]

  stop <ID...>                     Stop and retain progress
  resume|retry <ID...>             Resume stopped/failed tasks, retaining completed work
  retry <ID...>                    On a running task, retry only failed objects of finished
                                   subtasks, without interrupting the transfer
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
  nodes <ID>                       Show execution nodes, addable Agents and per-node shares
  nodes <ID> --add NODE --remove NODE [--now] [--yes]
      Change execution nodes; see help task nodes for details

  create oss-to-oss <source-alias/bucket> <target-alias/bucket> [options]
  create oss-to-local <source-alias/bucket> --binding node=/target/path [options]
  create local-to-oss <target-alias/bucket> --binding node=/source/path [options]
  import buckets|tasks|local <file> [options]
  See help task create and help task import.

States: RUNNING, STOPPING, RESUMING, COMPLETED, PARTIAL_FAILED, FAILED, STOPPED.
Actions read selected parent IDs and submit in bounded batches; retry on a running task retries only failed objects of finished subtasks.
Child pages default to 25 results in status priority order. --active selects ongoing work; --cursor continues.
Use --order id or --offset for numbering order; --limit is 1–200.
list/trash accept --limit 1–10000; --before-id loads older records. list --all follows every page.
--child loads only the selected child. Use --node when the ID exists on multiple nodes.
--watch supports list/show/children/progress/cleanup-status/trash and bandwidth/limits without a new value.
report saves the ZIP and shows the object count of each result; an incomplete report lists the reasons and exits 3.
Deleting records does not delete source or target objects.
`, `タスク管理
使い方: oss-transfer-cli task <サブコマンド> [引数]

  list [--status 状態] [--source バケット] [--dest バケット] [--worker ノード]
       [--direction oss-to-oss|oss-to-local|local-to-oss] [--search 文字列]
       [--limit 100] [--before-id ID] [--all]
  show <ID> [--offset 0] [--limit 25]
  show <ID> --child ID-N [--node ノード]
  children <ID> [--active] [--order priority|id] [--cursor CURSOR] [--offset 0] [--limit 25]
  progress <ID> [--child ID-N] [--generation N]
  cleanup-status <ID...>
  retry --all --yes
  recover-child <ID-N>            ノードの同時実行数制限内で子タスクの再開を試行
  trash [--task-id ID] [--worker ノード] [--direction 方向] [--search 文字列]
        [--limit 100] [--before-id ID] [--all]

  stop <ID...>                     進捗を保持して停止
  resume|retry <ID...>             完了済みの処理を保持して停止・失敗タスクを再開
  retry <ID...>                    実行中のタスクでは、終了した子タスクの失敗オブジェクトのみを
                                   転送を中断せずに再試行
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
  nodes <ID>                       実行ノード、追加可能な Agent、ノードごとの割り当てを表示
  nodes <ID> --add ノード --remove ノード [--now] [--yes]
      実行ノードを変更。詳細は help task nodes を参照

  create oss-to-oss <転送元接続名/バケット> <転送先接続名/バケット> [オプション]
  create oss-to-local <転送元接続名/バケット> --binding ノード=/転送先パス [オプション]
  create local-to-oss <転送先接続名/バケット> --binding ノード=/転送元パス [オプション]
  import buckets|tasks|local <ファイル> [オプション]
  詳細は help task create と help task import を参照してください。

状態: RUNNING、STOPPING、RESUMING、COMPLETED、PARTIAL_FAILED、FAILED、STOPPED。
指定した親タスクを個別に確認し、分割して送信します。実行中のタスクの retry は、終了した子タスクの失敗オブジェクトのみを再試行します。
子タスクは状態の優先順で 25 件ずつ取得します。--active は進行中のみ、--cursor は続きの取得です。
--order id または --offset で番号順に表示します。--limit は 1～200 です。
list/trash の --limit は 1～10000、--before-id は古い記録、list --all は全ページを取得します。
--child は指定した子タスクのみ取得します。同じ ID が複数ノードにある場合は --node を指定してください。
--watch は list/show/children/trash と、新しい値を指定しない bandwidth/limits に対応します。
report は ZIP を保存し、結果ごとのオブジェクト数を表示します。レポートが不完全な場合は理由を示し、終了コード 3 で終了します。
記録を削除しても転送元や転送先のオブジェクトは削除されません。
`},
	cliTaskCreateHelp: {`Create a transfer task
  oss-transfer-cli task create oss-to-oss <source-alias/bucket> <target-alias/bucket> [options]
  oss-transfer-cli task create oss-to-local <source-alias/bucket> --binding node=/target/path [options]
  oss-transfer-cli task create local-to-oss <target-alias/bucket> --binding node=/source/path [options]

  --conflict skip|overwrite|changed  If the destination exists; default skip
      skip: skip existing objects or files without comparing content
      overwrite: overwrite existing objects or files
      changed: overwrite changed objects: when the size or MD5 ETag differs or the
               source was modified later; skip the rest. Content is never read and
               multipart ETags are not compared
  --overwrite                       Same as --conflict overwrite
  --distribution affinity|balanced  Default balanced
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
            --ranged-download auto|off Default auto: inside one Agent, objects of 128 MiB or
                more are read as concurrent byte ranges and written to the file in order.
                off uses one sequential stream per object; choose it only when the target
                storage or network handles parallel range reads badly (memory-constrained
                Agents, storage that throttles concurrent GETs) or when support asks.
                Fixed for the task's lifetime, including resume, retry and node changes.
  Upload: --target-prefix <prefix> or --target-key <key>
          An exact target key requires one file on one node.
  Upload distribution follows the binding count; multiple nodes must bind the same logical directory.

Register connection aliases with storage first. Paths belong to execution nodes;
/data, C:\data and UNC paths are supported. Local transfers run prechecks before submission.
Accepted tasks start automatically; query task status for completion.
After a lost response, retry with identical arguments and the returned --request-id; no duplicate is created.
Use a new ID after changing arguments or for another task. Services before 4.1 do not deduplicate;
check the task list before retrying there. Creation is never retried automatically.
`, `転送タスクの作成
  oss-transfer-cli task create oss-to-oss <転送元接続名/バケット> <転送先接続名/バケット> [オプション]
  oss-transfer-cli task create oss-to-local <転送元接続名/バケット> --binding ノード=/転送先パス [オプション]
  oss-transfer-cli task create local-to-oss <転送先接続名/バケット> --binding ノード=/転送元パス [オプション]

  --conflict skip|overwrite|changed  転送先が存在する場合の処理。既定: skip
      skip: 既存のオブジェクトまたはファイルをスキップ（内容は比較しません）
      overwrite: 既存のオブジェクトまたはファイルを上書き
      changed: 変更されたオブジェクトを上書き。サイズまたは MD5 ETag が異なる場合、
               または転送元の更新日時が新しい場合に上書きし、それ以外はスキップ。
               内容は読み取らず、マルチパートの ETag は比較しません
  --overwrite                       --conflict overwrite と同じ
  --distribution affinity|balanced  既定: balanced
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
                --ranged-download auto|off 既定: auto。1 つの Agent 内で 128 MiB 以上の
                    オブジェクトをバイト範囲ごとに並列取得し、ファイルへ順番に書き込みます。
                    off はオブジェクトごとに 1 本の順次ストリームで取得します。転送先の
                    ストレージやネットワークが並列の範囲取得に向かない場合（Agent のメモリ
                    不足、同時 GET を制限するストレージ）やサポートの指示時のみ使用します。
                    再開、再試行、ノード変更後もタスクの間は変わりません。
  アップロード: --target-prefix <プレフィックス> または --target-key <Key>
                単一 Key の指定は 1 ノード上の 1 ファイルに限ります。
  分配方式はノード数に従います。複数ノードには同じ論理ディレクトリを指定してください。

接続エイリアスは先に storage で登録してください。パスは実行ノード上のパスです。
/data、C:\data、UNC パスに対応します。ローカル転送は送信前に事前チェックを行います。
受理されたタスクは自動開始します。完了はタスク状態で確認してください。
応答を受信できなかった場合、同じ引数と返された --request-id で再試行でき、重複して作成されません。
引数を変えた場合や別タスクには新しい ID を使ってください。4.1 より前のサービスは重複排除に対応しないため、
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
local download rows (OSS_TO_LOCAL) may set "rangedDownload":"auto" or "off"; omitted means auto.
Same meaning as task create --ranged-download; other directions reject off.

  oss-transfer-cli task import buckets ./buckets.csv
  oss-transfer-cli task import local ./local-requests.json --validate-only

Requests must form a non-empty array. The server still validates nodes, credentials and paths.
On partial acceptance or lost responses, retry with the original file, arguments and --request-id.
Do not remove/reorder rows or change arguments while reusing the ID. Prechecks do not use request IDs.
Services before 4.1 do not deduplicate; check the task list before retrying there. Creation is never retried automatically.
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
local のダウンロード行（OSS_TO_LOCAL）では "rangedDownload":"auto" または "off" を指定できます。
省略時は auto です。意味は task create --ranged-download と同じで、他の方向では off を指定できません。

  oss-transfer-cli task import buckets ./buckets.csv
  oss-transfer-cli task import local ./local-requests.json --validate-only

リクエストは空でない配列にしてください。サーバーでもノード、認証情報、パスを検証します。
一部受理や応答消失時は、元のファイル、引数、--request-id で再試行できます。
同じ ID で行の削除・並べ替え・引数変更はしないでください。事前チェックには ID を使いません。
4.1 より前のサービスは重複排除に対応しません。再試行前に一覧を確認してください。作成は自動再試行されません。
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
  update <node> --child-concurrency 32 --object-concurrency 4
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
  update <ノード> --child-concurrency 32 --object-concurrency 4
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
  invite-status <invitation-ID>   Check whether the Agent has joined
  list                            List registered Agents
  show <node>                     Connection, queues, roots and limits
  details <node>                  Host, interface IPs and mounted directories
  update <node> [--label name] [--local-root path ...]
  update <node> --clear-local-roots
  update <node> --child-concurrency 32 --object-concurrency 4
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
  invite-status <招待ID>          ノードの参加状況を確認
  list                            登録済み Agent を表示
  show <ノード>                   接続、キュー、ルート、制限を表示
  details <ノード>                ホスト、インターフェース IP、マウントを表示
  update <ノード> [--label 名前] [--local-root パス ...]
  update <ノード> --clear-local-roots
  update <ノード> --child-concurrency 32 --object-concurrency 4
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
  buckets <alias> [--bucket NAME]  List buckets and check the connection
  test <alias> [--bucket NAME]     Check the connection: list buckets, then list objects in one bucket with the effective addressing

  --endpoint <http[s]://host>      For example https://s3.example.com or http://10.0.0.5:9000
  --region <region>               S3 signing region
  --access-key-env <NAME>          Or --access-key-file <file>
  --secret-key-env <NAME>          Or --secret-key-file <file>
  --session-token-env <NAME>       Or --session-token-file <file>
  --clear-session-token           Remove the saved temporary credential token
  --validate true|false           Check the connection before saving; default true
  --v2-signer default|true|false   Signature version: default is V4, true is V2
  --path-style auto|true|false     Addressing; default auto

Addressing
  auto   Path-style when the endpoint has a port, or its host is an IP address,
         localhost, a name without a dot, or ends with .internal, .local or .lan;
         otherwise virtual-hosted. Self-hosted S3 (MinIO, SeaweedFS, Ceph) is
         usually path-style automatically; AWS, JD Cloud and other cloud
         domains stay virtual-hosted.
  true   Always path-style (http://endpoint/bucket/key)
  false  Always virtual-hosted (http://bucket.endpoint/key)

  oss-transfer-cli storage add source --endpoint https://oss.example.com --access-key-env OSS_ACCESS_KEY --secret-key-env OSS_SECRET_KEY
  oss-transfer-cli storage add minio --endpoint http://10.0.0.5:9000 --access-key-env MINIO_ACCESS_KEY --secret-key-env MINIO_SECRET_KEY
  oss-transfer-cli storage update source --path-style true
  oss-transfer-cli storage test source
  oss-transfer-cli storage test scoped --bucket only-bucket
  oss-transfer-cli storage remove source --yes

Credentials come from environment variables or files, not command-line values.
Check each node's sync result; partial success is reported as incomplete.
A connection check lists buckets (ListBuckets), then lists at most one object in one bucket (ListObjectsV2).
Keys limited to one bucket cannot list buckets: saving only warns, and test --bucket checks that bucket.
Actual operations validate read and write permissions. Failures show the cause and advice first; add --verbose for the original error.
`, `ストレージ接続
使い方: oss-transfer-cli storage <サブコマンド> [引数]

  list                            認証情報を伏せて接続一覧を表示
  add <接続名> --endpoint URL --access-key-env NAME --secret-key-env NAME
  update <接続名> [変更項目]       未指定項目は現在の値を保持
  remove <接続名> --yes            削除して実行ノードに同期
  buckets <接続名> [--bucket 名前] バケット一覧を表示して接続を確認
  test <接続名> [--bucket 名前]    接続を確認: バケットを一覧し、実際のアドレス指定で 1 つのバケットのオブジェクトを一覧

  --endpoint <http[s]://ホスト>    例: https://s3.example.com、http://10.0.0.5:9000
  --region <リージョン>            S3 署名リージョン
  --access-key-env <NAME>          または --access-key-file <ファイル>
  --secret-key-env <NAME>          または --secret-key-file <ファイル>
  --session-token-env <NAME>       または --session-token-file <ファイル>
  --clear-session-token           保存済みの一時認証トークンを削除
  --validate true|false           保存前に接続を確認。既定: true
  --v2-signer default|true|false   署名バージョン: default は V4、true は V2
  --path-style auto|true|false     アドレス指定。既定: auto

アドレス指定
  auto   エンドポイントにポートがある場合、またはホストが IP アドレス、localhost、
         ドットを含まない名前、.internal・.local・.lan で終わる名前の場合はパス形式、
         それ以外は仮想ホスト形式。セルフホストの S3（MinIO、SeaweedFS、Ceph）は
         通常自動でパス形式になり、AWS や JD Cloud などのクラウドのドメインは仮想ホスト形式のままです。
  true   常にパス形式（http://endpoint/bucket/key）
  false  常に仮想ホスト形式（http://bucket.endpoint/key）

  oss-transfer-cli storage add source --endpoint https://oss.example.com --access-key-env OSS_ACCESS_KEY --secret-key-env OSS_SECRET_KEY
  oss-transfer-cli storage add minio --endpoint http://10.0.0.5:9000 --access-key-env MINIO_ACCESS_KEY --secret-key-env MINIO_SECRET_KEY
  oss-transfer-cli storage update source --path-style true
  oss-transfer-cli storage test source
  oss-transfer-cli storage test scoped --bucket only-bucket
  oss-transfer-cli storage remove source --yes

認証情報は環境変数またはファイルから読み取ります。コマンドラインには直接指定しません。
ノードごとの同期結果を確認してください。一部成功は不完全と表示します。
接続確認ではバケットを一覧し（ListBuckets）、1 つのバケットのオブジェクトを最大 1 件一覧します（ListObjectsV2）。
1 つのバケットに限定したキーはバケットを一覧できません。保存時は警告のみで、test --bucket でそのバケットを確認できます。
読み取り・書き込み権限は実際の操作で検証します。失敗時は原因と対処を先に表示し、元のエラーは --verbose で確認できます。
`},
	cliLimitHelp: {`Node rate limits
Usage: oss-transfer-cli limit <subcommand> [arguments]

  list                            Target/running limits and automatic adjustment status
  set <list|put|head|bandwidth> <value|unlimited> (--worker NODE|--all)
       [--unit bps|Kbps|Mbps|Gbps]
  history [--limit 50]             Recent changes across all nodes, newest first; --limit is 1–200 in total
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
  history [--limit 50]             全ノードの最近の調整（新しい順）。--limit は合計 1～200 件
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
Usage: oss-transfer-cli object <subcommand> [arguments]

Subcommands
  compare --source-bucket ALIAS/BUCKET --dest-bucket ALIAS/BUCKET [--scope prefix]
          [--source-prefix PREFIX] [--dest-prefix PREFIX] [--mode metadata|content]
                                  Compare two prefixes; without prefixes, whole buckets
  compare --source-bucket ALIAS/BUCKET --source-key KEY --dest-bucket ALIAS/BUCKET --dest-key KEY
          [--scope object] [--mode metadata|content]
                                  Compare one object
  list                            List comparisons on every node with their status
  status <ID> [--after NUMBER] [--limit 1–1000] [--status RESULT[,RESULT...]]
                                  Show counts and a page of differences; 50 per page by default
  export <ID> --output FILE [--format zip|jsonl] [--status RESULT[,RESULT...]]
                                  Export every stored difference; ZIP by default
  resume <ID>                     Continue an interrupted, failed or canceled comparison after its checkpoint
  cancel <ID>                     Cancel a running comparison
  delete <ID>                     Delete the results of a finished comparison

Results
  DIFFERENT, ETAG_MISMATCH (ETag differs), MISSING_TARGET (missing from target), EXTRA_TARGET (only on target),
  CHANGED_DURING_COMPARE, ERROR (read failed), BOTH_MISSING (missing on both sides).

Examples
  oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --source-prefix '2026/' --dest-prefix 'backup/'
  oss-transfer-cli object compare --source-bucket source/photos --source-key '2026/a.jpg' --dest-bucket target/archive --dest-key 'backup/a.jpg' --mode content
  oss-transfer-cli object list
  oss-transfer-cli object status <comparison-id> --watch 5
  oss-transfer-cli object status <comparison-id> --status MISSING_TARGET,DIFFERENT --limit 200
  oss-transfer-cli object export <comparison-id> --output comparison.zip
  oss-transfer-cli object resume <comparison-id>

Notes
  --scope is optional: object when --source-key/--dest-key are given, otherwise prefix. --mode defaults to metadata.
  compare starts the comparison in the background and prints its ID. For a single object it waits up to 15 seconds
  and, when the comparison has finished, prints the result as status does.
  Empty prefixes compare whole buckets. Keys are matched after removing each prefix.
  metadata checks size, Content-Type, Content-Encoding, Content-Disposition, Cache-Control and custom metadata without reading content.
  Matching metadata does not prove matching content: when both ETags are plain MD5 values and differ, the result is ETAG_MISMATCH. Use content to confirm content.
  content streams both objects into SHA-256 without writing local files. Read traffic is charged by your provider.
  Modification time and storage class do not determine equality. Objects changed during comparison are reported separately.
  A comparison runs concurrently in the background on one node, which keeps every difference for 30 days by default; list finds comparison IDs again.
  The ZIP holds differences.csv, retry-source-keys.incr and summary.json. When both prefixes are equal,
  import-inventory can use retry-source-keys.incr to transfer missing, different or ETag-mismatched objects again.
  After an Agent restart or update, a comparison continues automatically when storage connections did not change;
  otherwise resume continues after the checkpoint and keeps the counts.
  When differences exceed the node's storage budget or free disk space runs low, later differences are counted but not stored.
  Exit code 3: status shows truncated differences, export is incomplete, or list cannot reach a node.
  delete removes finished comparisons only. If an ID starts with -, put -- before it, as in object status -- -abc.
  Keys and prefixes are passed unchanged. Quote shell metacharacters and spaces.
`, `オブジェクトの比較
使い方: oss-transfer-cli object <サブコマンド> [引数]

サブコマンド
  compare --source-bucket 接続名/バケット --dest-bucket 接続名/バケット [--scope prefix]
          [--source-prefix プレフィックス] [--dest-prefix プレフィックス] [--mode metadata|content]
                                  2 つのプレフィックスを比較。プレフィックスがなければバケット全体
  compare --source-bucket 接続名/バケット --source-key Key --dest-bucket 接続名/バケット --dest-key Key
          [--scope object] [--mode metadata|content]
                                  1 つのオブジェクトを比較
  list                            各ノードの比較と状態を一覧表示
  status <ID> [--after 番号] [--limit 1–1000] [--status 結果[,結果...]]
                                  集計と差分の 1 ページを表示。既定は 1 ページ 50 件
  export <ID> --output ファイル [--format zip|jsonl] [--status 結果[,結果...]]
                                  保存済みの差分をすべてエクスポート。既定は ZIP
  resume <ID>                     中断、失敗、キャンセルした比較をチェックポイントの後から再開
  cancel <ID>                     実行中の比較をキャンセル
  delete <ID>                     終了した比較の結果を削除

結果
  DIFFERENT 不一致、ETAG_MISMATCH ETag が異なる、MISSING_TARGET 転送先に存在しない、EXTRA_TARGET 転送先のみに存在、
  CHANGED_DURING_COMPARE 比較中に変更、ERROR 読み取り失敗、BOTH_MISSING 両側に存在しない。

例
  oss-transfer-cli object compare --source-bucket source/photos --dest-bucket target/archive --source-prefix '2026/' --dest-prefix 'backup/'
  oss-transfer-cli object compare --source-bucket source/photos --source-key '2026/a.jpg' --dest-bucket target/archive --dest-key 'backup/a.jpg' --mode content
  oss-transfer-cli object list
  oss-transfer-cli object status <比較 ID> --watch 5
  oss-transfer-cli object status <比較 ID> --status MISSING_TARGET,DIFFERENT --limit 200
  oss-transfer-cli object export <比較 ID> --output comparison.zip
  oss-transfer-cli object resume <比較 ID>

説明
  --scope は省略できます。--source-key/--dest-key を指定すると object、それ以外は prefix です。--mode の既定は metadata です。
  compare は比較をバックグラウンドで開始し、比較 ID を表示します。単一オブジェクトの場合は最大 15 秒待ち、
  終了していれば status と同じ形式で結果を表示します。
  プレフィックスが空の場合はバケット全体を比較します。それぞれのプレフィックスを除いた Key で対応付けます。
  metadata はサイズ、Content-Type、Content-Encoding、Content-Disposition、Cache-Control、カスタムメタデータを確認し、内容を読み取りません。
  メタデータが一致しても内容の一致は保証されません。両側の ETag が通常の MD5 形式で異なる場合、結果は ETAG_MISMATCH です。内容を確認するには content を使用してください。
  content は両側の内容をストリームで読み、SHA-256 を計算します。ローカルファイルは作成しません。読み取り通信が発生します。
  更新日時とストレージクラスは一致判定に使用しません。比較中に変更されたオブジェクトは別途表示します。
  比較は 1 つの実行ノードのバックグラウンドで並列に実行され、差分はすべてそのノードに既定で 30 日間保存されます。list で比較 ID を再確認できます。
  ZIP には differences.csv、retry-source-keys.incr、summary.json が含まれます。両側のプレフィックスが同じ場合、
  retry-source-keys.incr を import-inventory に使用し、転送先に存在しない、不一致、ETag が異なるオブジェクトを再転送できます。
  Agent の再起動や更新の後、ストレージ接続が変わっていなければ比較は自動的に再開します。
  それ以外の場合は resume でチェックポイントの後から再開でき、集計済みの結果は保持されます。
  差分がノードの保存容量の上限を超えるか、ディスクの空き容量が不足すると、以降の差分は件数のみ集計し保存しません。
  終了コード 3: status の差分の明細が切り詰められた、export が不完全、または list で接続できないノードがある。
  delete は終了した比較の結果のみ削除します。ID が - で始まる場合は、object status -- -abc のように ID の前に -- を付けてください。
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

The service owner or system administrator can use the installed cluster without login. Other users or clusters use account login.
logout ends only an account session, not local service access. Log in before changing an account password.
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

サービス実行ユーザーやシステム管理者は、本機のクラスタをログインなしで利用できます。他のユーザーやクラスタにはアカウントでログインします。
logout はアカウントのセッションのみを終了します。パスワード変更には login が必要です。
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
--node can be repeated; otherwise Master selects nodes. Requires Master 4.1 or later.
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
--node は複数回指定できます。省略時は Master が選択します。Master 4.1 以降が必要です。
中断後は元のファイルと引数を保持し、返された --request-id で再試行してください。
受理済みのタスクを重複して作成しません。
`},
}
