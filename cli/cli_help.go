package main

import "strings"

const cliHelp = `OSS Transfer CLI
用法：oss-transfer-cli [全局选项] <命令> [参数]

任务
  task list                      查看三种方向的主任务、状态与进度
  task show <ID>                 查看主任务及首批子任务
  task children <ID>             分页查看子任务，编号从 ID-1 开始
  task create <方向> ...         创建 OSS 间迁移、下载或上传任务
  task import <类型> <文件>      批量导入任务
  task stop|resume|start|retry <ID...>
  task delete <ID...>            将任务记录移入回收站
  task trash                     查看及筛选回收站任务
  task purge <ID...> --yes        永久删除回收站内指定记录
  task bandwidth <ID> [Mbps|unlimited]
  task report <ID> [--output 文件.zip]
  task creation <创建记录ID>     查看异步创建进度

节点与存储
  node list|show|update|remove    管理执行节点
  agent invite|list|show|update|remove
  storage list|add|update|remove  管理存储连接
  storage buckets|test <连接名>  列举 Bucket 并验证连接

限速与排障
  limit list|set|history|adjustment
  exceptions list|export|clear <failed|fallback|local>
  object compare ...             比对源、目标对象的元数据

账户
  setup status|complete          查看或完成首次设置
  login --username admin --password-stdin
  logout                         注销当前服务的会话
  whoami                         查看当前登录状态
  change-password --old-password-env OLD --new-password-env NEW

本机设置
  config get language            查看语言设置
  config set language auto|zh|en|ja  保存语言，默认跟随系统

清单工具
  diff-objects <源清单目录> <有序目标清单> <输出目录>
  count-lines <目录>
  import-inventory <源连接/桶> <目标连接/桶> <本机清单目录> [--overwrite]

全局选项
  --host <http://IP:1685>         服务地址，默认 http://localhost:1685
  --lang auto|zh|en|ja            临时指定语言
  --json                         输出结构化 JSON；持续刷新时每次一行
  --verbose                      将请求路径及 HTTP 状态写入标准错误
  --watch <秒|5s>                每隔 1–3600 整数秒刷新，只支持只读查询
  --version                      查看构建版本
  --help                         查看帮助，也可使用 help <命令组>

常用示例
  oss-transfer-cli task list --watch 5
  oss-transfer-cli task show 224 --child 224-1
  oss-transfer-cli task resume 224,225
  oss-transfer-cli task bandwidth 224 200
  oss-transfer-cli task report 224 --output task-224-report.zip
  oss-transfer-cli help task create

说明
  批量操作须明确指定主任务 ID，可用 224,225 或 224-226；不会默认操作全部任务。
  新任务受理后自动执行；已受理、预检通过均不代表迁移完成。
  删除节点、存储连接、永久删除任务及清空异常需要 --yes。
  查询不完整会保留节点错误；退出码 0=成功，1=操作失败，2=命令错误，3=结果不完整，130=用户中断。
  OSS_TRANSFER_MASTER_SERVER 可设置默认服务地址；密钥和密码请用环境变量或文件。
  OSS_TRANSFER_CLI_CONFIG_DIR 可指定配置和会话目录，须为绝对路径。
  默认跟随系统语言；无法识别时使用英文。config set language zh 可保存为中文，auto 恢复跟随系统。
  --lang 和 OSS_TRANSFER_LANG 可临时覆盖已保存设置。
  JSON 字段名、枚举值和用户数据保持原样。
  帮助：help task、help task create、help task import、help node、help storage、
        help agent、help setup、help limit、help exceptions、help object、help login、help config、help tools。
`

const cliTaskHelp = `任务管理
用法：oss-transfer-cli task <子命令> [参数]

查询
  list [--status 状态] [--source 源Bucket] [--dest 目标Bucket]
       [--worker 节点] [--direction oss-to-oss|oss-to-local|local-to-oss]
       [--search 关键词]
  show <ID> [--offset 0] [--limit 25]
  show <ID> --child ID-序号 [--node 节点]
  children <ID> [--offset 0] [--limit 25]
  creation <创建记录ID>
  trash [--task-id ID] [--worker 节点] [--direction 方向] [--search 关键词]

操作
  stop <ID...>                    停止传输并保留进度
  resume <ID...>                  继续已停止或失败的任务
  retry <ID...>                   与 resume 相同，保留已完成进度
  start <ID...>                   启动尚未自动执行的待执行任务
  delete <ID...>                  将任务记录移入回收站
  purge <ID...> --yes             永久删除回收站内指定任务记录
  bandwidth <ID>                 查看任务带宽上限
  bandwidth <ID> <Mbps|unlimited> 设置或移除任务额外带宽上限
  report <ID> [--output 文件.zip] 导出已结束或已停止任务的 ZIP 报告

创建与导入
  create oss-to-oss <源连接/桶> <目标连接/桶> [选项]
  create oss-to-local <源连接/桶> --binding 节点=/目标目录 [选项]
  create local-to-oss <目标连接/桶> --binding 节点=/源路径 [选项]
  import buckets|tasks|local <文件> [选项]
  使用 help task create 或 help task import 查看完整参数。

示例
  oss-transfer-cli task list --direction oss-to-local --status FAILED
  oss-transfer-cli task list --limit 100 --before-id 500
  oss-transfer-cli task show 224 --child 224-1 --node 10.0.0.2
  oss-transfer-cli task children 224 --offset 25 --limit 25
  oss-transfer-cli task stop 224,225
  oss-transfer-cli task resume 224-226
  oss-transfer-cli task bandwidth 224 200
  oss-transfer-cli task bandwidth 224 unlimited
  oss-transfer-cli task report 224 --output ./task-224.zip
  oss-transfer-cli task trash --direction oss-to-local --worker 10.0.0.2

状态：PENDING、RUNNING、LIST_DONE、RECOVERABLE、RESUMING、STOPPING、STOPPED、COMPLETED、PARTIAL_FAILED、FAILED。
操作只接受主任务 ID；子任务编号用于详情查询。节点状态不完整时不执行任务控制操作。
子任务默认读取 25 个编号，--limit 为 1–200；--offset 跳过指定数量的编号，不重新编号。
list/trash 的 --limit 为 1–10000；--before-id 从指定 ID 之前加载更早任务。
--child 按编号直接读取详情，不加载其他子任务；同编号在多个节点存在时须用 --node。
--watch 仅支持 list/show/children/creation/trash 及不带新值的 bandwidth 查询。
删除操作只处理迁移记录，源端和目标端的数据对象不受影响。
`

const cliTaskCreateHelp = `创建迁移任务
用法：
  oss-transfer-cli task create oss-to-oss <源连接/桶> <目标连接/桶> [选项]
  oss-transfer-cli task create oss-to-local <源连接/桶> --binding 节点=/目标目录 [选项]
  oss-transfer-cli task create local-to-oss <目标连接/桶> --binding 节点=/源路径 [选项]

通用选项
  --conflict skip|overwrite       已存在对象的处理方式，默认 skip
  --overwrite                     等同 --conflict overwrite
  --distribution affinity|balanced  任务分发方式，默认 affinity
  --balanced                      等同 --distribution balanced
  --date-after YYYY-MM-DDTHH:mm:ss  对象更新时间起点
  --date-before YYYY-MM-DDTHH:mm:ss 对象更新时间终点，须晚于起点
  --bandwidth-mbps <值>            任务带宽上限，0.1–1000000 Mbps
  --request-id <标识>              复用同一次创建请求；省略时自动生成

OSS 间迁移
  --node <节点[,节点...]>              指定执行节点，可重复；省略时选全部可用节点
  --prefix <前缀>                  迁移指定前缀，和 --object-key 互斥
  --object-key <Key>               仅迁移一个对象
  --mapping preserve              保留原始 Key，默认值
  --mapping add-prefix --target-prefix <前缀>
  --mapping replace-prefix --prefix <源前缀> [--target-prefix <目标前缀>]
  --mapping exact-key --object-key <源Key> --target-key <目标Key>

本地迁移
  --binding <节点=绝对路径>         按节点绑定本地路径，可重复
  --validate-only                  只执行节点和路径预检，不创建任务
  下载：--prefix <前缀> 或 --object-key <Key>
        --target-relative-path <相对路径> 仅用于单对象下载
  上传：--target-prefix <目标前缀> 或 --target-key <目标Key>
        指定目标 Key 仅适用于单节点上的单文件。
  本地上传根据绑定节点数量选择分发方式；多个节点须绑定同一逻辑目录。

示例
  oss-transfer-cli task create oss-to-oss source/photos target/archive --node 10.0.0.2 --prefix 2026/ --balanced
  oss-transfer-cli task create oss-to-oss source/photos target/archive --object-key a.jpg --mapping exact-key --target-key backup/a.jpg
  oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download --prefix 2026/ --validate-only
  oss-transfer-cli task create oss-to-local source/photos --binding 10.0.0.2=/data/download --prefix 2026/
  oss-transfer-cli task create local-to-oss target/archive --binding 10.0.0.2=/data/upload --target-prefix backup/

连接名须先通过 storage 添加；本地路径属于执行节点，支持 /data、C:\data 和 UNC 共享目录。
本地迁移提交前会自动预检；创建受理后自动执行，请继续查询任务状态。
响应丢失时，Go 服务可用原参数加 --request-id <返回标识> 重试；改参数或新建任务应使用新标识。
旧 V4 服务不支持此去重，重试前须核对任务列表。CLI 不自动重试创建请求。
`

const cliTaskImportHelp = `批量导入
用法：oss-transfer-cli task import buckets|tasks|local <文件> [选项]

类型
  buckets   存储桶迁移请求；支持 JSON 数组或无表头 CSV
  tasks     OSS 子任务请求；使用 JSON 数组
  local     本地迁移请求；使用 JSON 数组，提交前自动执行节点预检

选项
  --file <文件>                   也可代替文件位置参数
  --format json|csv               默认按扩展名识别，.csv/.txt 为 CSV
  --balanced                     设置 distributionMode 为 balanced
  --validate-only                仅适用于 local，只预检不创建任务
  --request-id <标识>              复用整批导入的请求标识；省略时自动生成

buckets CSV 列顺序（后 3 列可省略，无表头）
  源连接/桶,目标连接/桶,dateAfter,overwrite,distributionMode
  source/photos,target/archive,2026-09-01T00:00:00,false,balanced

JSON 使用 WebUI 对应 API 的请求字段，例如 buckets：
  [{"sourceS3Alias":"source","sourceBucket":"photos","destS3Alias":"target","destBucket":"archive","eligibleWorkerNodes":["10.0.0.2"],"distributionMode":"balanced"}]

示例
  oss-transfer-cli task import buckets ./buckets.csv
  oss-transfer-cli task import buckets ./requests.json
  oss-transfer-cli task import local ./local-requests.json --validate-only

导入文件必须为非空任务数组；节点、密钥、路径等校验仍由服务端执行。
部分受理或响应丢失时，Go 服务可用原文件及参数加 --request-id <返回标识> 重试整批。
不要删行、改顺序或改参数后复用标识；新建另一批任务应使用新标识。预检不使用请求标识。
旧 V4 服务不支持此去重，重试前须核对任务列表。CLI 不自动重试创建请求。
`

const cliNodeHelp = `执行节点
用法：oss-transfer-cli node <子命令> [参数]

  list                            查看节点配置及连接状态
  show <节点>                     查看节点详情
  add <节点> [--label 名称]        生成指定地址的 Agent 加入指令
  invite [--address 节点] [--label 名称]
  update <节点> [--address 新地址] [--label 名称]
  remove <节点> --yes              移除指定节点注册

示例
  oss-transfer-cli node list --watch 5
  oss-transfer-cli node add 10.0.0.2 --label 数据节点
  oss-transfer-cli node update 10.0.0.2 --label 备用节点
  oss-transfer-cli node remove 10.0.0.2 --yes

新增节点使用 agent invite。节点支持名称、IPv4 和 IPv6；省略时由 Agent 自动识别。
Go Agent 地址注册后不能直接修改，替换节点需重新加入。
服务端会检查节点身份、运行任务及主节点限制；未响应节点会明确列出。
队列与速率原始状态可使用 --json 查看，节点限速请使用 limit list。
`

const cliAgentHelp = `Agent 管理
用法：oss-transfer-cli agent <子命令> [参数]

  invite [--label 名称] [--address 节点]
  list                            查看全部 Agent
  show <节点>                     查看连接、队列、根目录和限速
  update <节点> [--label 名称] [--local-root 目录 ...]
  update <节点> --clear-local-roots
  remove <节点> --yes              撤销该 Agent 的注册

示例
  oss-transfer-cli agent invite --address storage-node --label 迁移节点
  oss-transfer-cli agent show storage-node
  oss-transfer-cli agent update storage-node --local-root /mnt/data --local-root /mnt/archive
  oss-transfer-cli agent update windows-node --local-root 'D:\data'
  oss-transfer-cli agent remove storage-node --yes

邀请有效期为 30 分钟，仅可加入一次。在执行节点运行对应系统的原生部署指令；Docker 为可选方式。
--local-root 使用 Agent 所在系统的绝对路径；配置会替换该节点已有的根目录列表。
`

const cliSetupHelp = `首次设置
用法：oss-transfer-cli setup status|complete

  status                          查看是否已完成首次设置
  complete --username 用户名 --public-url URL
           --setup-token-env NAME|--setup-token-file 文件|--setup-token-stdin
           --password-env NAME|--password-file 文件|--password-stdin

设置令牌来自主节点首次启动日志。密码和令牌各选择一种输入方式，不能同时使用 stdin。
--public-url 填写浏览器和 Agent 可以访问的主节点地址。

示例
  oss-transfer-cli --host http://10.0.0.10:1685 setup status
  oss-transfer-cli --host http://10.0.0.10:1685 setup complete --username admin \
    --public-url http://10.0.0.10:1685 --setup-token-file ./setup-token --password-env ADMIN_PASSWORD
  oss-transfer-cli --host http://10.0.0.10:1685 login --username admin --password-env ADMIN_PASSWORD
`

const cliStorageHelp = `存储连接
用法：oss-transfer-cli storage <子命令> [参数]

  list                            列出连接，密钥始终脱敏
  add <连接名> --endpoint URL --access-key-env NAME --secret-key-env NAME
  update <连接名> [要修改的字段]   未指定字段沿用现有配置
  remove <连接名> --yes            删除连接并同步到执行节点
  buckets <连接名>                列出 Bucket
  test <连接名>                   通过列举 Bucket 验证连接及列举权限

添加/更新选项
  --endpoint <http[s]://地址>
  --access-key-env <环境变量名>    或 --access-key-file <文件>
  --secret-key-env <环境变量名>    或 --secret-key-file <文件>
  --validate true|false           保存时是否校验连接，默认 true
  --v2-signer inherit|true|false   签名方式，新增默认继承
  --path-style inherit|true|false  寻址方式，新增默认继承

示例
  oss-transfer-cli storage add source --endpoint https://oss.example.com --access-key-env OSS_ACCESS_KEY --secret-key-env OSS_SECRET_KEY
  oss-transfer-cli storage update source --endpoint https://new-oss.example.com
  oss-transfer-cli storage test source
  oss-transfer-cli storage remove source --yes

密钥仅从环境变量或文件读取，避免出现在命令历史中。
更新后请关注逐节点同步结果；部分成功时结果不会显示为完整同步。
连接测试只验证列举操作，迁移所需的读取和写入权限由实际操作验证。
`

const cliLimitHelp = `节点限速
用法：oss-transfer-cli limit <子命令> [参数]

  list                            查看目标上限、运行上限和自动调节状态
  set <list|put|head|bandwidth> <数值|unlimited> (--worker 节点|--all)
       [--unit bps|Kbps|Mbps|Gbps]
  history [--limit 50]             查看各节点最近的调节记录，最多 1000 条/节点
  adjustment --worker 节点 --event-id ID [--cursor 游标]

示例
  oss-transfer-cli limit set bandwidth 200 --worker 10.0.0.2
  oss-transfer-cli limit set bandwidth 1 --unit Gbps --all
  oss-transfer-cli limit set put 500 --all
  oss-transfer-cli limit set bandwidth unlimited --worker 10.0.0.2
  oss-transfer-cli limit list --watch 5
  oss-transfer-cli limit history --limit 50
  oss-transfer-cli limit adjustment --worker 10.0.0.2 --event-id EVENT_ID

bandwidth 默认单位 Mbps；其他类型单位为次/秒，不接受 --unit。
数值须大于 0；移除上限须明确使用 unlimited，不将无效数值转成不限速。
必须明确选择一个节点或 --all；任务额外带宽请使用 task bandwidth。
调节详情保留 nextCursor、done、historyComplete 及进程重启信息。
`

const cliExceptionsHelp = `异常记录
用法：oss-transfer-cli exceptions list|export|clear <类型> [选项]

类型
  failed     迁移失败对象
  fallback   旧版 SDK 降级记录；Go 执行器返回不支持标记
  local      本地迁移失败

查询
  list failed|fallback [--task-id ID] [--source-bucket 桶] [--dest-bucket 桶]
                       [--worker 节点] [--limit 1000]
  list local [--task-id ID] [--worker 节点] [--direction OSS_TO_LOCAL|LOCAL_TO_OSS]
             [--endpoint 值] [--storage-id ID] [--offset 0] [--limit 25]

导出
  export failed|fallback --output 文件.csv [与查询相同的筛选选项]
  export local --output 文件.zip
  CSV 导出本次查询返回的记录，保留截断和节点完整性提示；不会覆盖已有文件。
  local ZIP 包含全量本地任务报告，不接受查询筛选；单任务请使用 task report。

清空
  clear failed|fallback --all --yes
  清空仅支持所选类型的全节点记录，不支持按任务或节点筛选；local 不提供清空接口。
  清空展示记录不会改变任务中的对象执行结果。旧版服务返回不完整时退出码为 3。

示例
  oss-transfer-cli exceptions list failed --task-id 224 --limit 1000
  oss-transfer-cli exceptions export failed --task-id 224 --output task-224-failures.csv
  oss-transfer-cli exceptions list local --direction OSS_TO_LOCAL --offset 25 --limit 25
  oss-transfer-cli exceptions export local --output local-report.zip
  oss-transfer-cli exceptions clear failed --all --yes

complete/partial 表示节点返回是否完整；truncated 表示记录被数量上限截断。
旧版服务可能限制每节点记录数，超过时显示 truncatedWorkers；更多记录用 ZIP 报告。
报告仅可导出已结束或已停止任务，不能将下载成功视为迁移验收通过。
`

const cliObjectHelp = `对象比对
用法：oss-transfer-cli object compare
      --source-bucket <源连接/桶> --source-key <源Key>
      --dest-bucket <目标连接/桶> --dest-key <目标Key>

示例
  oss-transfer-cli object compare --source-bucket source/photos --source-key '2026/a.jpg' --dest-bucket target/archive --dest-key 'backup/a.jpg'

Key 按原值传递，含空格或 shell 特殊字符时请加引号。
显示两端存在性、元数据、查询可用性及服务端的比对结论；查询失败不会当成对象不存在。
`

const cliAuthHelp = `登录与账户
  oss-transfer-cli login --username admin --password-stdin
  oss-transfer-cli login --username admin --password-env PASSWORD_ENV
  oss-transfer-cli login --username admin --password-file /path/to/password
  oss-transfer-cli whoami
  oss-transfer-cli logout
  oss-transfer-cli change-password --old-password-env OLD --new-password-env NEW

密码来源必须选择一种，不接受明文命令行密码。
--password-stdin 从标准输入读取；文件读取使用 --password-file。
修改密码的来源也可使用 --old-password-file / --new-password-file。
登录会话按 --host 对应服务保存；切换服务时须使用对应会话。
OSS_TRANSFER_AUTH_TOKEN 可提供用户登录令牌；CLI 不接受 OSS_TRANSFER_INTERNAL_TOKEN。
`

const cliToolsHelp = `对象清单工具
  oss-transfer-cli diff-objects <源CSV目录> <有序目标清单> <输出目录>
      [--mode sort-merge|bloom] [--false-positive 0.01]
  oss-transfer-cli count-lines <目录>
  oss-transfer-cli import-inventory <源连接/桶> <目标连接/桶> <本机清单目录>
      [--node 节点] [--overwrite] [--request-id 标识]

diff-objects 默认排序归并；bloom 可节省内存，但存在误判率。
目标清单必须按 Key 排序；输入与输出目录请使用不同路径。
import-inventory 读取本机目录中的 .incr 文件，每行一个 UTF-8 Key，每个文件创建一个主任务。
空行忽略，重复 Key 去重，其余 Key 保持原样；文件内容不会上传，仅提交 Key 清单。
--node 可重复填写；未指定时由 Master 选择执行节点。此命令需要 Go Master。
连接中断后，保留原文件和参数，用输出的 --request-id 重试，不会重复创建已受理的任务。
`

func helpFor(args []string) string {
	if len(args) > 0 && args[0] == "help" {
		args = args[1:]
	}
	if len(args) == 0 {
		return cliHelp
	}
	switch strings.ToLower(args[0]) {
	case "config":
		return cliConfigHelp
	case "task":
		if len(args) > 1 {
			switch args[1] {
			case "create":
				return cliTaskCreateHelp
			case "import":
				return cliTaskImportHelp
			}
		}
		return cliTaskHelp
	case "node", "list-worker", "worker-status":
		return cliNodeHelp
	case "agent":
		return cliAgentHelp
	case "setup":
		return cliSetupHelp
	case "storage", "list-s3-config", "remove-s3-config", "set-s3-config":
		return cliStorageHelp
	case "limit", "set-rate-limit", "list-rate-limit", "list-rate-limit-history", "rate-limit", "rate-limit-history":
		return cliLimitHelp
	case "exceptions", "list-failed-objs", "list-fallback-objs", "failed", "fallback", "delete-failed-objs", "delete-fallback-objs", "clear-failed", "clear-fallback":
		return cliExceptionsHelp
	case "object":
		return cliObjectHelp
	case "login", "logout", "whoami", "change-password", "auth":
		return cliAuthHelp
	case "tools", "diff-objects", "count-lines", "import-inventory":
		return cliToolsHelp
	case "add-task":
		return cliTaskCreateHelp
	case "import-tasks":
		return cliTaskImportHelp
	case "list-task", "status", "show-task", "stop", "resume", "start", "retry", "delete-task", "clear", "list-deleted", "permanent-delete", "export-report":
		return cliTaskHelp
	default:
		return cliHelp
	}
}

const cliConfigHelp = `本机设置
用法：
  oss-transfer-cli config get language
  oss-transfer-cli config set language auto|zh|en|ja

默认跟随系统。保存一次语言后，后续命令直接使用；auto 恢复跟随系统。
--lang 和 OSS_TRANSFER_LANG 只覆盖当前运行；auto 可临时使用系统语言。
`
