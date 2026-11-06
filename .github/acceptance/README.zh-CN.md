# 验收测试

[English](README.md)

CI 只构建一次发布压缩包，然后用其中的 Master、Agent 和 CLI 对 SeaweedFS 4.48 进行测试。一个独立的 S3 客户端读取目标端对象，将其键名、长度和 SHA-256 哈希与源端测试数据比对。发布流程直接发布这次 CI 运行产出的压缩包和镜像。

## 平台

| 平台 | Runner |
| --- | --- |
| Linux amd64 | `ubuntu-latest` |
| Linux arm64 | `ubuntu-26.04-arm` |
| macOS amd64 | `macos-15-intel` |
| macOS arm64 | `macos-latest` |
| Windows amd64 | `windows-latest` |
| Windows arm64 | `windows-11-arm` |

这六个平台各有一个 Go 测试 job 和一个验收 job；在 Windows 和 macOS amd64 上，安装是耗时最长的检查（Windows 上因为 PowerShell，Intel Mac runner 上约 200 秒），因此由第三个 job 运行原生安装器场景。它们合起来在每个平台上运行相同的应用场景和原生安装器场景，两个 Linux job 还会测试各自的 Docker 镜像和 Compose 安装器。验收 job 和安装 job 不需要 Go 工具链：测试程序 job 为每个平台交叉编译分片上传崩溃测试程序和两个安装失败测试程序。Go 测试启用竞态检测器，只有 Windows arm64 除外，因为该平台不支持。这个 runner 使用原生 arm64 应用程序，SeaweedFS 测试服务则以 x64 模拟方式运行。

Linux ARMv7、riscv64 和 loong64 在 QEMU 中执行目标程序，SeaweedFS 测试服务使用 amd64 版本。FreeBSD amd64 和 arm64 各在一台 FreeBSD 14.4 虚拟机中运行，SeaweedFS 测试服务直接运行在 Linux 宿主机上。Master、Agent、CLI 和崩溃恢复测试程序仍在 FreeBSD 内运行，使用其文件系统和服务管理器。这些平台各用一个 job 运行 Go 测试、CLI 检查、安装和传输。QEMU 平台只在完整范围（每晚运行和手动完整运行）中运行，因此 push 和发布永远不必等待模拟器或它的软件包镜像；FreeBSD amd64 在每次 push 时也会运行。在完整范围下，它们运行的 Go/CLI 测试和应用场景与上面六个平台相同，包括共享带宽、停止/重启、分片上传清理、清单恢复和安装回滚。它们的测试程序用 `CGO_ENABLED=0` 交叉编译，不运行竞态检测器。

### 测试范围

| | push 和 pull request（`platform`） | 每晚运行和手动完整运行（`full`） |
| --- | --- | --- |
| Go 测试，Linux amd64 | 两个模块的全部测试，启用竞态检测器 | 相同 |
| Go 测试，其他平台 | 平台相关范围 | 全部测试 |
| 原生验收 | CLI 检查、安装、全部传输场景 | 相同 |
| FreeBSD amd64 | 平台相关的 Go 测试、CLI 检查、安装、三种传输方向和分片上传崩溃恢复 | 全部 Go 测试和传输场景 |
| QEMU 平台（ARMv7、riscv64、loong64） | 不运行 | 全部 Go 测试、CLI 检查、安装和传输场景 |

平台相关范围定义在 [master-shards.json](master-shards.json) 的 `platformScope` 中：除 `internal/task` 外的所有包（它是基于数据库的任务和分发记账，没有平台相关代码），以及 `internal/master` 中仅在 `agent_packages_test.go`、`backups_test.go` 和 `install_commands_test.go` 里声明的测试，这些测试的结果取决于操作系统。文件系统、服务、安装器、路径、数据库和传输相关的包、命令程序以及 CLI 模块在所有平台上运行。如果列出的文件不存在、没有声明测试，或声明了 `go test -list` 没有报告的测试，或者被跳过的包已不存在，运行就会失败。被跳过的测试在每次 push 时仍会在 Linux amd64 上运行，并每晚在所有平台上运行。每晚运行失败不影响发布；GitHub 会通知最后一个修改定时配置的人。

每晚运行在 UTC 18:40 开始。按需运行完整范围：

```sh
gh workflow run ci.yml --ref main -f full_suite=true
```

push 和 pull request 运行七个平台：六个原生平台和 FreeBSD amd64。每晚运行和手动完整运行再加上三个 QEMU 平台。FreeBSD ARM64 是可选的，因为它的软件模拟测试套件大约需要十六分钟。它的压缩包仍会和另外十个发布压缩包一起构建和检查。要运行它的完整运行时测试套件，在 CI 中选择 **Run workflow** 并启用 `freebsd_arm64`，或执行：

```sh
gh workflow run ci.yml --ref main -f freebsd_arm64=true
```

这会构建压缩包，然后只运行完整的 FreeBSD ARM64 测试套件，不会重复其他平台，也不会构建容器镜像。手动运行不会取消 push 触发的运行，也不提供发布产物。

旧版 Java 数据库转换器支持 Linux 和 macOS。其他平台测试在修改迁移状态之前就拒绝发布；FreeBSD 还会检查暂存数据和启动保护标记保持完好。

Linux QEMU job 在 Ubuntu runner 上安装真实的 systemd 服务。只有安装器的 CPU 检测被映射为目标架构；服务管理、可执行文件、PID 检查和回滚都是真实的。这样可以在宿主机的 Linux 内核上执行目标 CPU 代码，但不能证明与某款物理 CPU、发行版或内核兼容。FreeBSD 在虚拟机内使用 rc.d。

## 调度

打包 job 构建全部十一个发布压缩包，最多同时构建四个平台，Python 打包检查和消息测试与构建并行运行；任何一项失败都会在上传之前终止该 job。另一个测试程序 job 与其并行：它交叉编译已启用的附加平台测试程序（每个模块和目标执行一次 `go test -c`，不包含 DWARF 和符号表），并为六个原生平台各只编译 `internal/agentservice` 测试程序和两个安装失败测试程序，所有目标同时编译。push 编译 FreeBSD amd64 测试包和六个测试程序包；完整运行再加上三个 QEMU 测试包；选择 FreeBSD ARM64 时只编译它自己的测试包。

Go 构建缓存和模块缓存按 runner 操作系统、架构、Go 版本和用途（`cross` 用于压缩包，`tests` 用于测试程序，`race` 用于原生 Go 测试 job）分别保存。main 分支和手动运行按提交保存缓存；pull request 只恢复缓存。恢复的条目会相对于上次保存进行裁剪，所以一周没有提交也不会导致 Go 丢弃缓存。

测试产物记录提交、目标架构和 SHA-256 哈希，不会随版本发布。运行任何程序之前，`test-binaries.py verify` 会检查提交、可执行文件格式和架构、每个校验和，以及测试源码和测试程序源码是否与当前检出一致（比较前统一换行符，因为 Windows 检出可能转换换行符）。分片上传崩溃测试使用目标平台的 `agentservice` 测试程序，包括它自行启动的崩溃辅助进程。

六个 Go 测试 job 不需要安装包，所以与打包 job 和测试程序 job 同时开始。[go-unit.py](go-unit.py) 把最慢的 `internal/master` 包拆成并发分片，与其余包同时运行；其余包按耗时从长到短运行，最多同时运行两个（CI job 独占 runner，同时运行三个），CLI 模块在旁边同时运行。分片根据 [master-shards.json](master-shards.json) 均衡分配；列表中没有的测试分到负载最轻的分片，如果分片覆盖的测试与 `go test -list` 报告的选中测试不完全一致，运行就会失败。失败的顶层测试会重跑一次；真正的回归会再次失败。

只有两个 Linux 验收 job 需要等待镜像；macOS 和 Windows 在安装包和测试程序就绪后立即开始。QEMU 和 FreeBSD job 等待安装包和测试程序。选择 FreeBSD ARM64 时，运行打包 job、测试程序 job 和一个 FreeBSD ARM64 job。[run-platform.py](run-platform.py) 同时启动一个 job 的所有组：CLI 和清单检查（含安装器源码检查）、安装（Windows 和 macOS amd64 的安装 job 使用 `--install only`，它们的验收 job 使用 `--install separate`）、传输；在 QEMU 和 FreeBSD 平台上还有 Go 测试。原生安装和 Compose 安装依次运行，因为两者都会发现已安装的服务。

在完整传输范围下，三个集群并行运行，每个都有自己的 SeaweedFS、Master 和两个 Agent：`transfer`（初始化、分片上传崩溃恢复和三种传输方向，然后是带宽和按需分配）、`recovery`（停止/重启和崩溃恢复）和 `nodes`（执行节点变更）。每个集群先在其他组运行期间上传自己的测试数据。在测量带宽或注入停止、崩溃之前，每个集群都会等待同一 runner 上的其他所有组通过、退出并完成清理；之后这些测量场景只彼此共享 runner。任何一组失败都不会进入这一独占阶段，并使 job 失败；每次运行都有独立的门控标识。核心传输范围只运行第一个集群的初始化、分片上传和传输方向检查，不做任何测量，所以不需要等待。

某个组失败时，job 日志末尾会附上失败阶段日志的最后 150 行；完整日志在上传的证据中。

FreeBSD 缓存其 Python 环境和依赖；修改 requirements 文件会使缓存失效。它只接收自己平台的压缩包，只返回日志和证据。宿主机上的 S3 测试服务使用固定版本的 SeaweedFS 压缩包、临时凭证，以及通过 QEMU 私有网络访问的回环监听地址。上传的产物中不包含测试服务凭证。安装测试数据使用快速 gzip/ZIP 压缩；发布的压缩包和压缩包校验不受影响。

`output/platform/evidence.json` 记录每个阶段的耗时和结果，详细输出在同目录的日志中。集群证据位于 `cluster/`、`cluster-recovery/` 和 `cluster-nodes/`，每个都记录哪些组在其独占传输阶段开始前已经完成。Go 测试 job 上传 `unit-evidence-*`，其中包含测试范围、选中和跳过的测试，以及每条命令的日志；安装 job 上传 `install-evidence-*`。

FreeBSD arm64 在 `ubuntu-26.04-arm` 上使用 QEMU 软件模拟，配置两个虚拟 CPU 和 `GOMAXPROCS=2`。能力检查显示这台 ARM 宿主机没有提供 `/dev/kvm`；客户机仍使用 TCG，而不是硬件虚拟化。宿主机和客户机的诊断信息记录 CPU 数量、QEMU 版本和 KVM 虚拟机创建探测结果。下关于速度的结论之前，先比较各阶段耗时。FreeBSD amd64 在 `ubuntu-latest` 上使用默认的虚拟机 CPU 设置。

发布流程等待对应提交上默认的 push CI，它在最慢的 push job 结束时完成。每晚运行和可选的手动运行（包括 QEMU 平台和 FreeBSD ARM64 验收）是独立的，不阻塞发布。默认 CI 成功并不代表 FreeBSD ARM64 运行时验收通过。只修改 VERSION 的提交可以复用代码完全相同的一次成功 push 运行中的默认测试套件结果；手动运行不能作为复用基准。构建和发布始终保留[平台列表](../../deploy/native/platforms.json)中的全部十一项。

## 覆盖范围

| 测试 | 断言 |
| --- | --- |
| 安装包和镜像校验 | 校验和、压缩包成员、可执行文件架构、版本和提交一致。镜像中的程序与原生安装包一致。 |
| 状态和请求处理 | 重复请求只保留一个任务；停止后过期的工作不能继续执行；账本记录与计数器一致。还会检查取消、源端变化、路径逃逸和清理归属。 |
| 初始化和 CLI | 初始化会保护管理权限，凭证校验生效，生成的 Agent 地址与发布版本一致，账号、节点和限制在重启后保留。 |
| 传输 | 三种迁移方向都产生预期的键名和哈希。两个选中的 Agent 都会分到工作。命名规则、SKIP、OVERWRITE 和子任务总数与测试数据一致。 |
| 任务带宽 | 两个并发的 2 Mbps 任务各自在两个 Agent 之间共享自己的限额。测试在传输过程中采样目标端增长，并校验完成的文件和报告。 |
| 按需分配份额 | 一个由两个 Agent 执行的下载任务，第一个分发批次中有一个 24 MiB 对象，第二个批次中有一个 256 KiB 对象。拿到小对象的 Agent 空闲后，节点视图必须显示它为空闲，忙的 Agent 至少持有 限额 × (1 − 1/64) − 1 B/s；节点视图的每次采样中份额之和都不超过限额，目标端增长也不超过限额。只记录吞吐量，不对吞吐量做断言。使用恢复测试的任务限速。`--scenario dynamic_share` 只运行这项检查。 |
| 停止和重启 | 确认停止后不再写入；停止意图在重启后保留。终止 Agent 后，已完成的进度和已有目标对象保留。恢复后完成剩余文件，并删除临时文件。 |
| 分片上传恢复 | 重启会清理中断的上传及其暂存文件，同时保留无关的上传和之前已完成的对象。中断的对象从头重试。 |
| 执行节点调整 | 向限速的 OSS→OSS 任务中途添加 Agent 后，它加入任务，限额由两个 Agent 分担，两者都传输对象，每个对象的 SHA-256 和任务计数都一致。中途排空两个 Agent 中的一个时，它在任务结束前离开；剩余 Agent 获得全部限额，任务以精确的计数完成。立即移出扫描 Agent 时，它的工作以及仍需要的扫描转给另一个 Agent；节点视图随后标记新的扫描节点或显示扫描已完成，不会标记已移出的节点。三种调整的全过程中任务都不报告错误。`--scenario nodes` 只运行这项检查。 |
| 报告 | 导出的行、状态、目标键名和总数与校验后的清单一致。 |
| 清单导入 | 响应丢失或重复提交时只保留一个父任务。重启后未完成的导入仍然保留。同一请求 ID 下输入发生变化时会被拒绝，缺失的键名会被报告。 |
| 原生安装 | 服务 PID 和版本正确。升级保留密码和节点注册。校验和错误时在停止服务之前就拒绝。覆盖回滚和接管已有数据的流程。 |
| Docker 和 Compose | 初始化、节点注册和绑定挂载目录的传输在容器内无需 root 即可工作。重建容器后状态保留。 |

主测试数据包含 260 个对象，超过 128 个对象的分发批次边界。其中包括一个空对象、一个包含 Unicode 和空格的名称，以及两个用于分片上传的 17 MiB 对象。带宽测试数据有 129 个对象：两个 4 MiB 文件（每个分发批次各一个）和 127 个空文件。在 2 Mbps 下，8 MiB 的数据每个任务大约需要 34 秒。恢复测试使用独立的 256 个文件的测试数据，任务限速 16 Mbps，按平台在 [run-platform.py](run-platform.py) 的 `RECOVERY_BANDWIDTH_MBPS` 中设置。如果恢复测试的传输在注入停止或崩溃之前就已完成，检查会失败并指出这个设置，而不是在没有触发恢复的情况下通过。

S3 服务由 SeaweedFS 提供，测试不会连接京东云 OSS。服务商特有的签名、权限和限流需要针对该服务商实际运行测试。单元测试使用 HTTP 模拟器复现协议错误。

## 本地运行

用 `./build.sh --test` 构建当前平台的安装包，然后准备测试环境。将下面的 `linux/amd64` 替换为你的平台：

```sh
python3 -m venv /tmp/oss-transfer-qa-venv
/tmp/oss-transfer-qa-venv/bin/pip install -r .github/acceptance/requirements.txt
/tmp/oss-transfer-qa-venv/bin/python .github/acceptance/prepare.py \
  --platform linux/amd64 --directory /tmp/oss-transfer-qa-bin
/tmp/oss-transfer-qa-venv/bin/python .github/acceptance/cluster.py \
  --master /tmp/oss-transfer-qa-bin/oss-transfer-master \
  --agent /tmp/oss-transfer-qa-bin/oss-transfer-agent \
  --weed /tmp/oss-transfer-qa-bin/weed \
  --output output/cluster-local --scenario all --multipart-recovery
```

请使用新的输出目录。在 Windows 上，使用 `.exe` 文件名和你环境中的 Python 可执行文件。默认情况下，`--multipart-recovery` 会编译一个 Agent 测试程序，需要安装 Go。也可以传入 `--multipart-test-binary`，改用经过 `test-binaries.py` 校验的可执行文件。主要场景在可执行文件 PATH 为空的情况下运行打包后的程序。`--scenario all` 在一个集群中依次运行全部场景；CI 则把 `transfer`、`recovery` 和 `nodes` 作为三个并发集群运行，push 时在 QEMU 和 FreeBSD 平台上运行 `core`。

要像 CI 一样运行 Go 测试，使用 `python3 .github/acceptance/go-unit.py --output output/unit --race`；加上 `--scope platform` 即为平台相关范围。`run-platform.py` 运行一个平台的所有组；`--unit none` 不运行 Go 测试，`--transfer core` 只运行核心集群。

在构建主机上准备附加平台的测试程序：

```sh
python3 .github/acceptance/test-binaries.py build --platform linux/armv7 --output /tmp/oss-transfer-tests
```

将该目录复制到目标环境，然后带上 `--platform`、`--directory` 和 `--repo` 执行 `verify` 或 `run`。校验会在运行任何程序之前检查可执行文件架构、校验和以及代码检出。`run` 还需要一个新的 `--output` 目录，用于保存日志和结果；加上 `--scope platform` 即为平台相关范围。Linux 模拟需要为初始进程和恢复测试创建的子进程都注册 binfmt。`build-native --platform darwin/arm64 --output DIR` 只构建原生验收所需的测试程序；对该目录执行 `verify` 会把它们的路径写入 `QA_MULTIPART_TEST_BINARY`、`QA_FAILURE_MASTER` 和 `QA_ROLLBACK_CLI`，`run-platform.py` 再把它们传给安装和传输检查。

服务安装测试使用 `--master`、`--agent`、`--cli`、`--repo` 和 `--output` 运行 `install.py`。Linux 需要 root 和 systemd；Windows 需要管理员权限。macOS 使用用户级 LaunchAgent，加上 `--system` 则使用系统级 LaunchDaemon。测试使用独立的服务名称和目录，结束后会删除注册的服务。只有一次性的 CI 机器才使用 `--all-installed`，它会测试所有已安装角色的发现和升级。

`docker.py` 检查已准备好的镜像。`setup.py` 针对本地发布目录测试公开安装器。完整命令见 [ci.yml](../workflows/ci.yml)。

`local-cli.py` 检查会分别启动已安装的 Master 和 Agent 进程，验证本机自动访问、远程登录隔离、凭证文件权限、重启后保留，以及删除 Agent 后的权限撤销。它使用独立的数据，不会改动已有集群。

## 结果

各场景在输出目录下写入 `evidence.json` 和脱敏后的日志。带宽测试还会保存采样数据；安装包和镜像检查会记录产物标识。CI 上传的结果保留七天。

`compare.py` 使用相同的测试数据比较 V4.0.x JAR 包与 Go 版本，并提供真实数据迁移测试数据的辅助工具。这些可选比较独立于发布验收。
