# 在其他系统上安装

[English](install.md)

使用 systemd 219 及以上版本的 Linux（CentOS 7 / RHEL 7 及更新版本）请使用[安装命令](../README.zh-CN.md#1-安装-master)。以下步骤适用于其他系统和自定义路径。无法联网的主机请使用[离线安装](offline-install.zh-CN.md)。

## CentOS 7 / RHEL 7

CentOS 7 和 RHEL 7 是支持的最早的 Linux 版本，安装命令与其他 Linux 相同：安装器和在线更新都能使用系统自带的 systemd 219、Bash 4.2 和 curl 7.29。较旧的 systemd 会让在线更新有两处不同：

- systemd 低于 236 时，在线更新失败后，其辅助单元 `oss-transfer-update-*` 会一直显示为失败状态，执行 `systemctl reset-failed` 即可清除。
- systemd 低于 240 时，无法启动的更新辅助进程不会被立即报告，要等更新超时后才显示失败。

## macOS

为当前用户安装服务：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
bash install.sh --role master --user
```

安装为系统服务时，改用 `sudo bash install.sh --role master`。安装器会打印服务名称和路径。

## Windows

在管理员 PowerShell 中运行：

```powershell
Invoke-WebRequest https://raw.githubusercontent.com/MoonWX/oss-transfer/main/deploy/native/Install.ps1 -OutFile Install.ps1
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master
```

在中国大陆，也可以从 `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/Install.ps1` 获取该脚本。

## FreeBSD

以 root 身份运行：

```sh
pkg install -y bash curl
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh -o install.sh
bash install.sh --role master
```

在所有系统上，都使用安装器打印的 `admin` 密码登录。原生 Master 安装器会询问是否同时安装本机 Agent。其他 Agent 在 **执行节点 → 添加节点** 中添加。

## 下载源

安装器默认从 GitHub 下载；主机公网 IP 位于中国大陆时，改从京东云 OSS 镜像下载。首选来源无法访问时，会自动尝试另一个。使用 `--source github` 或 `--source jdoss`（`Install.ps1` 使用 `-Source`）可指定来源。镜像中的 `https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/` 始终包含最新正式版的全部文件，其中也有 `install.sh` 和 `Install.ps1`；每个版本（包括预发布版本）另有自己的目录，例如 `v4.1.36/`。

以文件形式保存的安装器（而不是通过管道交给 shell 运行）会依次在自身所在文件夹和当前目录中查找 `manifest.tsv` 或本机平台的压缩包。找到其中之一时，直接用该文件夹中的文件安装，不下载任何内容；如果指定了 `--version`，它必须与这些文件的版本一致。加 `--source github` 或 `--source jdoss`（`Install.ps1` 使用 `-Source`）则改为在线下载。详见[离线安装](offline-install.zh-CN.md)。

## 安装指定版本或预发布版本

安装器默认使用最新正式版。需要安装固定版本（包括 `4.1.29-beta.1` 这类预发布版本）时，显式指定版本：

```sh
bash install.sh --role master --version 4.1.29-beta.1
```

```powershell
powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\Install.ps1 -Role master -Version 4.1.29-beta.1
```

安装器校验预发布版本的方式与正式版完全相同：使用该标签的 GitHub Release 摘要或 OSS 版本目录中的清单，而不是 `latest`。对已有安装，带 `--upgrade` 和 `--version` 重新运行安装器即可升级到该版本，包括从预发布版本升级到对应的正式版。在控制台中添加的 Agent 下载与 Master 相同的版本。

Docker Compose 在 `.env` 中设置精确的镜像标签；`latest` 和 `X.Y` 只跟随正式版：

```dotenv
OSS_TRANSFER_IMAGE=moonwx/osstransfer:4.1.29-beta.1
```

有比该预发布版本更新的正式版后，把标签改回正式版（或 `latest`）即可回到正式版。预发布版本属于实验性版本；在线更新和回到正式版的方法见[试用预发布版本](operations.zh-CN.md#试用预发布版本)。

## 路径和端口

新的原生安装把文件放在同一个根目录下。Linux 默认为 `/opt/oss-transfer`，macOS 为 `/Library/Application Support/OSS Transfer`（使用 `--user` 时为用户的 Library），FreeBSD 为 `/usr/local/lib/oss-transfer`，Windows 为 `%ProgramData%\OSS Transfer`。使用其他磁盘时设置 `--root-dir` 或 `-RootDir`。操作系统的服务注册不在该目录中；Unix 还会保留一个全局的 CLI 入口。

Linux 的默认位置：

| | Master | Agent |
| --- | --- | --- |
| 服务 | `oss-transfer-master` | `oss-transfer-agent` |
| 程序 | `/opt/oss-transfer/master/bin` | `/opt/oss-transfer/agent/bin` |
| 配置 | `/opt/oss-transfer/master/config/service.json` | `/opt/oss-transfer/agent/config/service.json` |
| 数据 | `/opt/oss-transfer/master/data` | `/opt/oss-transfer/agent/data` |
| 升级备份 | `/opt/oss-transfer/master/backups` | `/opt/oss-transfer/agent/backups` |

Master 默认端口为 `1685`。`install.sh` 使用 `--port 1785` 修改，`Install.ps1` 使用 `-Listen '0.0.0.0:1785'`。自定义位置时使用 `--install-dir`、`--config-dir` 或 `--data-dir`；这些目录不能相互包含。运行 `bash install.sh --help` 查看选项，Windows 上运行 `Get-Help .\Install.ps1`。升级会沿用已安装的路径。

在 Linux 上查看服务状态和日志：

```sh
sudo systemctl status oss-transfer-master
sudo tail -n 50 /opt/oss-transfer/master/data/logs/service.log
```

把 `master` 换成 `agent` 即可查看 Agent。其他系统会在安装时打印日志路径。

## 重装且不丢失数据

再次运行安装器；如果修改过 `--root-dir`，使用相同的值。安装器会识别保留的服务配置和数据，补回缺失的程序或服务定义，并沿用 Agent 身份。旧目录布局的已有安装保留其保存的路径。

保留每个角色的 `config` 目录和完整的 `data` 目录，包括其中的密钥文件（`master.db.key` 或 `agent.db.key`）。密钥必须与数据库匹配。缺少配置或密钥时，安装器会停止，而不是创建一个空的替代安装。源文件、目标文件和共享挂载与这些应用目录相互独立。

无人值守安装原生 Master 时，使用 `--with-agent` 或 `--without-agent`；Windows 使用 `-WithAgent` 或 `-WithoutAgent`。已有的本机 Agent 只有属于该 Master 时才会被沿用。

## 反向代理

在域名根路径提供服务，例如 `https://transfer.example.com/`。在该域名的 NGINX server 块中添加：

```nginx
location / {
    proxy_pass http://127.0.0.1:1685;
    proxy_set_header Host $http_host;
    proxy_read_timeout 60s;
}
```

不支持 `/transfer/` 这样的子路径。添加 Agent 时，请选择 Agent 能访问的 Master 地址；Agent 可以使用私有 IP，而浏览器通过代理访问。
