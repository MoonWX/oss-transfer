# OSS Transfer

[English](README.md)

OSS Transfer 在兼容 Amazon S3（Simple Storage Service）的存储桶之间迁移数据，也支持上传、下载本地文件。适合需要在一台或多台服务器上执行迁移、查看进度和恢复中断任务的运维人员。

## 在 Linux 上启动

在装有 systemd、Bash 和 curl 的 Linux 主机上执行：

```sh
(curl -fsSL --connect-timeout 3 --max-time 15 \
  https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh ||
 curl -fsSL --connect-timeout 3 --max-time 15 \
  https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/install.sh -o install.sh) &&
bash install.sh
```

以下选项回车使用默认值，随后确认安装：

```text
Role: 1 Master / 2 Agent [1]:
Deployment: 1 Native (recommended) / 2 Docker Compose [1]:
Master port [1685]:
```

安装器自动识别系统和 CPU 架构。下载源按公网 IP 自动选择：中国大陆使用京东云 OSS，其他地区使用 GitHub；所选下载源无法连接时，会尝试另一个。

在浏览器打开 Master 主机 IP 的 `1685` 端口；本机安装可访问 `http://localhost:1685`。输入服务日志中的设置令牌，创建管理员账号。Master URL 填写浏览器和执行主机都能访问的地址。Linux 默认日志可这样查看：

```sh
sudo cat /var/lib/oss-transfer-go/master/logs/service.log
```

**Master** 提供控制台并调度任务，**Agent** 在执行主机上传输数据。单机部署也需要添加一个 Agent。

## 添加执行主机

1. 在 **执行节点 → 添加节点** 创建邀请。
2. 将生成的安装命令复制到执行主机运行；单机部署就在 Master 所在主机上运行。
3. 等待节点显示在线。

后续增加主机时重复这几个步骤。Agent 主动连接 Master，因此每台执行主机都需要能访问 Master URL。Master 与 Agent 保持相同版本。

## 创建迁移任务

在 **存储连接** 中添加服务地址和访问凭据。进入 **创建任务**，选择存储桶之间迁移、下载或上传，再选择来源、目标和执行节点。

本地路径指的是 Agent 主机上的路径，Agent 服务需要有源文件的读取权限或目标目录的写入权限。多个 Agent 处理同一本地目录时，各主机必须在相同的绝对路径挂载同一共享文件系统。

`SKIP` 保留目标已有对象，`OVERWRITE` 覆盖已有对象。任务带宽上限由所选节点共同使用，同时受各节点限速约束。任务详情可查看进度和失败记录，支持停止、继续；停止或结束后可导出报告。

需要终端操作时，查看 [CLI 使用文档](docs/cli.md)。控制台和 CLI 支持中文、英文和日文。

## 其他安装方式

预编译包支持 Linux、macOS、Windows 和 FreeBSD。系统及架构要求、Windows 和 macOS 安装、自定义目录及离线安装见[原生部署文档](deploy/go/README.md)。

也可使用 [Docker Compose](docs/docker-compose.md)。容器中的 Agent 只能访问已挂载的宿主机目录；原生 Agent 直接使用主机路径。

## 升级

先停止正在运行的任务，然后下载并运行最新安装脚本：

```sh
(curl -fsSL --connect-timeout 3 --max-time 15 \
  https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh ||
 curl -fsSL --connect-timeout 3 --max-time 15 \
  https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/latest/install.sh -o install.sh) &&
bash install.sh --upgrade
```

安装器会查找已安装的服务，沿用原来的路径和设置。替换前会停止并备份服务，新服务启动检查失败时恢复备份。Windows、macOS 用户服务及备份方法见[部署文档](deploy/go/README.md#upgrade)。V4.0.x 需要先完成[数据库迁移](docs/go-migration.md)。

## 迁移在线业务前

本工具不提供持续复制。迁移期间对源端的修改不保证同步到目标端；需要一致的最终结果时，应安排停写后再做一次迁移。任务恢复会保留已完成对象的结果，但未完成的对象可能需要重新传输。迁移范围是读取到的对象，不包含存储桶的完整历史版本或存储服务配置。

## 编译与贡献

在仓库目录中，安装 [go.mod](https://github.com/MoonWX/oss-transfer/blob/main/go.mod) 指定的 Go 版本、Node.js 24 和 Python 3，然后运行：

```sh
./build.sh --test
```

安装包输出到 `output/go-development/`。测试范围见 [CI 文档](.github/acceptance/README.md)，发布流程见[发版文档](.github/RELEASING.md)。问题可提交到 [GitHub Issues](https://github.com/MoonWX/oss-transfer/issues)，附上版本和去除凭据后的相关日志。

许可证：TODO。
