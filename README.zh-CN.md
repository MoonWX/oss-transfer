# OSS Transfer

[English](README.md)

在 S3 兼容存储和本地磁盘之间搬运数据，可按需扩展到多台服务器并行执行。

## 快速开始

安装 Master：

```sh
curl -fsSL https://raw.githubusercontent.com/MoonWX/oss-transfer/main/install.sh | sudo bash
```

中国大陆：

```sh
curl -fsSL https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/install.sh | sudo bash
```

打开 `http://<master-ip>:1685`，使用 `admin` 和安装完成时终端输出的初始密码登录。首次登录后设置自己的密码。

Master 只负责调度，数据由 Agent 传输。在 **执行节点 → 添加节点** 中复制一行加入指令，到对应主机上运行，至少加一个。Master 所在主机也可以运行 Agent。

添加节点时，Master 地址默认使用当前浏览器地址。Agent 走内网时，将它改为 Master 的内网 URL。

添加存储连接，创建任务，即可开始迁移。

原生部署可在 **运维工具 → 版本更新** 中检测版本，先更新 Master，再更新 Agent。尚无此入口的旧版本需要先通过安装脚本升级一次。也可重新运行安装命令；Docker 部署通过 Compose 更新。

升级成功后会删除本次临时备份。若计划之后降级，请[提前另做备份](deploy/native/README.md#upgrade)。

## 文档

- [部署](deploy/native/README.md)：Windows、离线安装、自定义路径
- [Docker Compose](docs/docker-compose.md)
- [CLI](docs/cli.md)
- [并发、对象比对与升级](docs/operations.md)

从源码构建：运行 `./build.sh`。
