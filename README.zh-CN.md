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

然后打开 `http://<master-ip>:1685`，创建管理员账号。

Master 只负责调度，数据由 Agent 传输。在 **执行节点 → 添加节点** 中生成命令，到对应主机上运行，至少加一个。Master 所在主机也可以运行 Agent。

添加存储连接，创建任务，即可开始迁移。

需要升级时，重新运行安装命令即可。

## 文档

- [部署](deploy/native/README.md)：Windows、离线安装、自定义路径
- [Docker Compose](docs/docker-compose.md)
- [CLI](docs/cli.md)

从源码构建：运行 `./build.sh`。
