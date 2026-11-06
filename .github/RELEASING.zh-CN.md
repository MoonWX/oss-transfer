# 发布流程

[English](RELEASING.md)

推送 `v*` 标签即可发布新版本。发布工作流会等待该标签所在提交的 CI 通过，取用 CI 构建的安装包和镜像，上传到京东云 OSS 和 Docker Hub，最后公开 GitHub Release。

## 发布一个版本

1. 把 `VERSION` 改为发布版本号，例如 `4.1.3` 或 `4.1.3-rc.1`。
2. 提交这次修改。
3. 给该提交打标签，并把提交和标签一起推送。

```sh
version=$(cat VERSION)
git tag -a "v$version" -m "OSS Transfer v$version"
git push --atomic origin main "v$version"
```

`--atomic` 保证两个引用要么都更新，要么都不更新。不需要先等 CI：发布工作流最多等待 65 分钟，每 5 秒检查一次该提交在 `main` 上的 CI，通过后立即开始发布。如果这次 CI 失败，发布工作流也会失败，不会发布任何内容。默认的 push CI 需要 7 分钟多一点，发布本身再用约 1 分钟，因此推送后大约 8.5 分钟版本即可公开。

这次 CI 结束之前，不要再向 `main` 推送：新的推送会取消正在运行的这次 CI，除非被取消的运行符合下文所述的条件，否则发布工作流会失败。遇到这种情况，请等新推送的 CI 结束后，再重新运行被取消的 CI（重新运行属于同一并发组，会取消仍在运行的 CI），然后重试发布。

标签必须与 `VERSION` 一致，并指向要发布的提交。已发布的版本需要修改时，请使用新的版本号。

上传 OSS 和 Docker Hub 期间，GitHub Release 以草稿形式存在，两者都成功后才公开。失败的发布可能留下草稿，同一标签的下一次运行会复用它。

### 发布前修复

如果 CI 失败需要修复，提交修复后把标签移到新提交。只有在 GitHub Release 不存在或仍是草稿时才能这样做；新的运行会覆盖之前为该版本上传的 OSS 文件和 Docker Hub 标签。先取消该标签仍在运行的发布，避免它发布旧提交：

```sh
gh run list --workflow release.yml --limit 5
gh run cancel <run-id>
```

然后移动标签，并和修复一起推送：

```sh
version=$(cat VERSION)
git tag -fa "v$version" -m "OSS Transfer v$version"
git push --atomic origin main "+refs/tags/v$version"
```

`+` 只强制更新标签。推送标签会启动新的发布，它会等待修复提交的 CI。

如果 CI 失败与代码无关，请重新运行失败的 CI 作业，再按下文重试发布。

只修改 `VERSION` 时，CI 会复用 `main` 上代码相同的祖先提交已通过的测试。它仍会重新构建全部安装包和镜像，检查打包后的程序版本，并校验校验和与镜像身份。修改代码、依赖、安装脚本或工作流时运行默认测试集；无法确认可用的通过基线时，也运行默认测试集。

默认 CI 测试七个平台：六个原生平台和 FreeBSD amd64。全部 Go 测试在 Linux amd64 上运行；其他平台运行与平台相关的部分，FreeBSD 只运行核心传输，而不是全部传输场景。三个 QEMU 平台（Linux ARMv7、riscv64 和 loong64）只在每晚运行和手动完整运行中运行，因此发布永远不会等待它们；这些运行会在全部十个受测平台上执行所有测试，不影响发布（见[测试范围](acceptance/README.zh-CN.md#测试范围)）。FreeBSD arm64 的安装包会构建并检查，但运行时测试是可选的。需要单独运行完整的 FreeBSD arm64 测试时：

```sh
gh workflow run ci.yml --ref main -f freebsd_arm64=true
```

它只运行一个 FreeBSD arm64 验收作业，不会重复其他平台，也不影响发布；发布使用默认的 `main` CI 运行结果。

发布工作流按提交和 CI 运行 ID 选取构建产物，只接受 `main` 上由 push 触发的运行，排除 Pull Request、每晚运行和手动触发的 CI。被取消的仅版本号变更的运行，只有在两个构建作业都通过、没有测试失败、且代码相同的祖先提交 CI 通过时，才能提供产物。产物七天后过期；过期后请重新运行对应的 CI。

使用当前发布工作流重试已有标签的发布：

```sh
gh workflow run release.yml --ref main -f tag=v4.1.7
```

标签和它的产物保持不变。

## 发布 beta 版本

带有 SemVer 预发布后缀的版本都是预发布版本，例如 `4.1.29-beta.1` 或 `4.1.29-rc.1`。发布步骤与正式版完全相同：设置 `VERSION`、提交、打 `v4.1.29-beta.1` 标签并推送。请使用以点分隔的标识，保证排序可预期（`beta.2` 早于 `beta.10`，`beta` 早于 `rc`，所有预发布版本都早于 `4.1.29`）。不要使用构建元数据（`+...`）、带前导零的数字标识（`beta.01`）或 `-dev` 后缀（保留给未发布的构建），这些版本无法在线安装。

beta 版本会影响：

- GitHub 把它创建为**预发布版本**，不会标记为最新版本。
- OSS 只上传版本目录 `oss-transfer/v4.1.29-beta.1/`，`oss-transfer/latest/` 仍指向最近的正式版。
- Docker Hub 只推送精确标签 `4.1.29-beta.1`；`X.Y`、`latest` 等别名仍指向正式版。

beta 版本不会影响：默认的安装命令、页面上的**检测新版本**、CLI 的 `update check`，以及使用 `latest` 的 Compose 文件，它们都继续解析为最新正式版。任何安装都不会被自动提示安装预发布版本。

试用 beta 版本：

- 新安装：运行安装脚本时加 `--version 4.1.29-beta.1`（PowerShell 使用 `-Version 4.1.29-beta.1`）；Compose 设置 `OSS_TRANSFER_IMAGE=moonwx/osstransfer:4.1.29-beta.1`。见[安装指定版本](../docs/install.zh-CN.md#安装指定版本或预发布版本)。
- 已有的托管安装：在页面打开 `/tools?tab=updates&channel=prerelease`，或运行 `oss-transfer-cli update master --version 4.1.29-beta.1 --allow-prerelease --yes`。之后 Agent 可以跟随 Master 的版本更新。见[预发布版本更新](../docs/operations.zh-CN.md#试用预发布版本)。

正式版 `4.1.29` 发布后，运行任意 `4.1.29-*` 预发布版本的安装会在正常检测更新时看到它，因为正式版的优先级高于其预发布版本。

## 发布的文件

GitHub Release 附带 [platforms.json](../deploy/native/platforms.json) 列出的十一个平台安装包，以及 `manifest.tsv`、`SHA256SUMS` 和三个安装脚本（`install.sh`、`native-install.sh` 和 `Install.ps1`），因此从这里下载的文件就是一套完整的离线安装包。每个安装包包括 Master、Agent、CLI、文档和校验和。

OSS 的每个版本目录保存同样的文件，外加 `release.json`。安装器下载一个平台安装包，解压出服务和 CLI，例如 Linux amd64 使用 `oss-transfer-linux-amd64.tar.gz`。Docker 镜像使用经过测试的 Linux 程序。

安装器在中国大陆公网 IP 下选择 OSS，其他地区选择 GitHub。GitHub 下载使用 Release API 提供的安装包摘要，以及该安装包记录的提交中的安装脚本；OSS 使用版本目录中的清单。显式指定下载源或本地安装包时，不再检测位置。从文件运行（而不是通过管道运行）的安装器，如果在脚本所在目录或当前目录中找到发布文件，会直接使用这些文件，不会联网，除非用 `--source github|jdoss` 或 `--release-url`（PowerShell 为 `-Source`、`-ReleaseUrl`）要求下载；见[离线安装](../docs/offline-install.zh-CN.md)。

| 版本 | OSS 位置 | GitHub | Docker Hub 标签 |
| --- | --- | --- | --- |
| `vX.Y.Z` | `oss-transfer/vX.Y.Z/` | Release | `X.Y.Z`、`X.Y`、`latest` |
| `vX.Y.Z-rc.N`、`vX.Y.Z-beta.N` | `oss-transfer/vX.Y.Z-rc.N/` | 预发布版本 | `X.Y.Z-rc.N` |

正式版还会把版本目录中的全部文件上传到 `oss-transfer/latest/`，所以这个固定地址下始终有最新正式版的全部文件。在线安装器读取 `latest/manifest.tsv` 只是为了锁定该版本，通过 OSS 检测更新的 Master 读取 `latest/release.json`；两者随后都从对应的版本目录下载。预发布版本不修改 `latest`。Compose 对正式版使用 `latest`，对预发布版本使用精确的镜像标签。

旧的 `osstransfer/` 目录继续保留，供 v4.1.8 之前发布的客户端使用。新版本使用 `oss-transfer/`。

## 凭证

发布前设置以下仓库密钥：

| 目标 | 地址 | 密钥 |
| --- | --- | --- |
| 京东云 OSS | `moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/` | `JDOSS_AK`、`JDOSS_SK` |
| Docker Hub | `moonwx/osstransfer` | `DOCKER_USERNAME`、`DOCKER_PAT` |

发布使用 GitHub 托管的 `ubuntu-latest` 运行器。作业顺序和超时见 [release.yml](workflows/release.yml)，OSS 上传顺序见 [publish-oss.py](../deploy/native/publish-oss.py)。OSS 以八路并发上传版本目录，全部完成后先核对该目录的文件列表、大小和 MD5 ETag，然后才处理 `latest/`。正式版接着把安装包、安装脚本和 `SHA256SUMS` 上传到 `latest/`，之后才依次写入 `manifest.tsv` 和 `release.json`，确保客户端读到的元数据不会指向缺失的文件。随后从 `latest/` 删除已停用的文件和不再发布的平台安装包，并以同样方式核对该目录。即使标签不同，同一时间也只运行一个 OSS 发布。
