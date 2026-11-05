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

每个受测平台对应一个验收 job。这六个平台运行相同的应用场景和原生安装器场景，两个 Linux job 还会测试各自的 Docker 镜像和 Compose 安装器。Go 测试启用竞态检测器，只有 Windows arm64 除外，因为该平台不支持。这个 runner 使用原生 arm64 应用程序，SeaweedFS 测试服务则以 x64 模拟方式运行。

Linux ARMv7、riscv64 和 loong64 在 QEMU 中执行目标程序，SeaweedFS 测试服务使用 amd64 版本。FreeBSD amd64 和 arm64 各在一台 FreeBSD 14.4 虚拟机中运行，SeaweedFS 测试服务直接运行在 Linux 宿主机上。Master、Agent、CLI 和崩溃恢复测试程序仍在 FreeBSD 内运行，使用其文件系统和服务管理器。启用后，这五个平台运行的 Go/CLI 测试和应用场景与上面六个平台相同，包括共享带宽、停止/重启、分片上传清理、清单恢复和安装回滚。它们的测试程序用 `CGO_ENABLED=0` 交叉编译，不运行竞态检测器。

push 和 pull request 运行十个平台。FreeBSD ARM64 是可选的，因为它的软件模拟测试套件大约需要十六分钟。它的压缩包仍会和另外十个发布压缩包一起构建和检查。要运行它的完整运行时测试套件，在 CI 中选择 **Run workflow** 并启用 `freebsd_arm64`，或执行：

```sh
gh workflow run ci.yml --ref main -f freebsd_arm64=true
```

这会构建压缩包，然后只运行完整的 FreeBSD ARM64 测试套件，不会重复其他十个平台，也不会构建容器镜像。手动运行不会取消 push 触发的运行，也不提供发布产物。

旧版 Java 数据库转换器支持 Linux 和 macOS。其他平台测试在修改迁移状态之前就拒绝发布；FreeBSD 还会检查暂存数据和启动保护标记保持完好。

Linux QEMU job 在 Ubuntu runner 上安装真实的 systemd 服务。只有安装器的 CPU 检测被映射为目标架构；服务管理、可执行文件、PID 检查和回滚都是真实的。这样可以在宿主机的 Linux 内核上执行目标 CPU 代码，但不能证明与某款物理 CPU、发行版或内核兼容。FreeBSD 在虚拟机内使用 rc.d。

## 调度

打包 job 构建全部十一个发布压缩包，最多同时构建四个平台，Python 打包检查和消息测试与构建并行运行；任何一项失败都会在上传之前终止该 job。另一个测试程序 job 与其并行，交叉编译已启用的附加平台测试程序：每个模块和目标执行一次 `go test -c`，所有目标同时编译，不包含 DWARF 和符号表。默认 CI 编译四个测试包；选择 FreeBSD ARM64 时只编译它自己的测试包。

Go 构建缓存和模块缓存按 runner 操作系统、架构、Go 版本和用途（`cross` 用于压缩包，`tests` 用于测试程序，`race` 用于原生测试）分别保存。main 分支和手动运行按提交保存缓存；pull request 只恢复缓存。恢复的条目会相对于上次保存进行裁剪，所以一周没有提交也不会导致 Go 丢弃缓存。

测试产物记录提交、目标架构和 SHA-256 哈希，不会随版本发布。分片上传崩溃测试使用目标平台的 `agentservice` 测试程序，包括它自行启动的崩溃辅助进程。

默认 CI 运行十个平台 job，以及共享的打包、测试程序和镜像构建。只有两个 Linux 原生 job 需要等待镜像；macOS 和 Windows 在安装包就绪后立即开始。QEMU 和 FreeBSD job 等待安装包和测试程序。选择 FreeBSD ARM64 时，运行打包 job、测试程序 job 和一个 FreeBSD ARM64 job。每个受测平台在内部启动四组测试：单元测试、CLI 和清单检查、安装、传输。单元测试组把最慢的 `internal/master` 包拆成三个并发分片，与其余包同时运行；其余包按耗时从长到短运行，最多同时运行两个。分片根据 [master-shards.json](master-shards.json) 均衡分配；列表中没有的测试分到负载最轻的分片，如果分片覆盖的测试与 `go test -list` 报告的不完全一致，运行就会失败。安装器源码检查和 CLI 模块测试在控制组中运行。原生安装和 Compose 安装依次运行，因为两者都会发现已安装的服务。

传输组在检查初始化、分片上传恢复和三种迁移方向，以及上传带宽和恢复测试数据时，可以与其他组同时运行。测量带宽之前，传输组会等待其他所有组通过、退出并完成清理，然后单独运行带宽和停止/重启测试。任何一组失败都不会进入这一独占阶段，并使 job 失败；每次运行都有独立的门控标识。

FreeBSD 缓存其 Python 环境和依赖；修改 requirements 文件会使缓存失效。它只接收自己平台的压缩包，只返回日志和证据。宿主机上的 S3 测试服务使用固定版本的 SeaweedFS 压缩包、临时凭证，以及通过 QEMU 私有网络访问的回环监听地址。上传的产物中不包含测试服务凭证。安装测试数据使用快速 gzip/ZIP 压缩；发布的压缩包和压缩包校验不受影响。

`output/platform/evidence.json` 记录每个阶段的耗时和结果，详细输出在同目录的日志中。集群证据还会记录哪些组在独占传输阶段开始前已经完成。

FreeBSD arm64 在 `ubuntu-26.04-arm` 上使用 QEMU 软件模拟，配置两个虚拟 CPU 和 `GOMAXPROCS=2`。能力检查显示这台 ARM 宿主机没有提供 `/dev/kvm`；客户机仍使用 TCG，而不是硬件虚拟化。宿主机和客户机的诊断信息记录 CPU 数量、QEMU 版本和 KVM 虚拟机创建探测结果。下关于速度的结论之前，先比较各阶段耗时。FreeBSD amd64 在 `ubuntu-latest` 上使用默认的虚拟机 CPU 设置。

发布流程等待对应提交上默认的 push CI。可选的手动 FreeBSD ARM64 验收是独立的，不阻塞发布。默认 CI 成功并不代表 FreeBSD ARM64 运行时验收通过。只修改 VERSION 的提交可以复用代码完全相同的一次成功 push 运行中的默认测试套件结果；手动运行不能作为复用基准。构建和发布始终保留[平台列表](../../deploy/native/platforms.json)中的全部十一项。

## 覆盖范围

| 测试 | 断言 |
| --- | --- |
| 安装包和镜像校验 | 校验和、压缩包成员、可执行文件架构、版本和提交一致。镜像中的程序与原生安装包一致。 |
| 状态和请求处理 | 重复请求只保留一个任务；停止后过期的工作不能继续执行；账本记录与计数器一致。还会检查取消、源端变化、路径逃逸和清理归属。 |
| 初始化和 CLI | 初始化会保护管理权限，凭证校验生效，生成的 Agent 地址与发布版本一致，账号、节点和限制在重启后保留。 |
| 传输 | 三种迁移方向都产生预期的键名和哈希。两个选中的 Agent 都会分到工作。命名规则、SKIP、OVERWRITE 和子任务总数与测试数据一致。 |
| 任务带宽 | 两个并发的 2 Mbps 任务各自在两个 Agent 之间共享自己的限额。测试在传输过程中采样目标端增长，并校验完成的文件和报告。 |
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

请使用新的输出目录。在 Windows 上，使用 `.exe` 文件名和你环境中的 Python 可执行文件。默认情况下，`--multipart-recovery` 会编译一个 Agent 测试程序，需要安装 Go。也可以传入 `--multipart-test-binary`，改用经过 `test-binaries.py` 校验的可执行文件。主要场景在可执行文件 PATH 为空的情况下运行打包后的程序。

在构建主机上准备附加平台的测试程序：

```sh
python3 .github/acceptance/test-binaries.py build --platform linux/armv7 --output /tmp/oss-transfer-tests
```

将该目录复制到目标环境，然后带上 `--platform`、`--directory` 和 `--repo` 执行 `verify` 或 `run`。校验会在运行任何程序之前检查可执行文件架构、校验和以及代码检出。`run` 还需要一个新的 `--output` 目录，用于保存日志和结果。Linux 模拟需要为初始进程和恢复测试创建的子进程都注册 binfmt。

服务安装测试使用 `--master`、`--agent`、`--cli`、`--repo` 和 `--output` 运行 `install.py`。Linux 需要 root 和 systemd；Windows 需要管理员权限。macOS 使用用户级 LaunchAgent，加上 `--system` 则使用系统级 LaunchDaemon。测试使用独立的服务名称和目录，结束后会删除注册的服务。只有一次性的 CI 机器才使用 `--all-installed`，它会测试所有已安装角色的发现和升级。

`docker.py` 检查已准备好的镜像。`setup.py` 针对本地发布目录测试公开安装器。完整命令见 [ci.yml](../workflows/ci.yml)。

`local-cli.py` 检查会分别启动已安装的 Master 和 Agent 进程，验证本机自动访问、远程登录隔离、凭证文件权限、重启后保留，以及删除 Agent 后的权限撤销。它使用独立的数据，不会改动已有集群。

## 结果

各场景在输出目录下写入 `evidence.json` 和脱敏后的日志。带宽测试还会保存采样数据；安装包和镜像检查会记录产物标识。CI 上传的结果保留七天。

`compare.py` 使用相同的测试数据比较 V4.0.x JAR 包与 Go 版本，并提供真实数据迁移测试数据的辅助工具。这些可选比较独立于发布验收。
