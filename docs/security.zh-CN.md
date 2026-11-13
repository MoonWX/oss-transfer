# 安全说明

[English](security.md)

这一页写给负责将 OSS Transfer 部署到生产网络的你。读完你会了解：谁能访问 Master、如何配置 HTTPS、存储凭证存放在哪里并流向哪里、密码和会话如何管理，以及哪些组件以 root 运行。端口和存储权限的完整清单在[规划迁移](planning.zh-CN.md)中。

## 网络暴露面

Master 通过同一个端口提供所有服务：管理页面、API、执行节点（Agent）通信，默认监听 `0.0.0.0:1685`，明文 HTTP。能访问这个端口的人都能看到登录页面。

需要连接这个端口的只有两类：操作迁移的人（浏览器或 CLI）和执行节点。执行节点不需要开放任何入站端口，所有连接都由它主动发起。

因此，首先应当用主机或云防火墙将 1685 端口限制为只有运维人员和执行节点的地址或网段能访问。

如果你想让 Master 只监听某个地址，安装时添加 `--listen HOST:PORT`（`Install.ps1` 用 `-Listen`），或者修改 Master `service.json` 中的 `listen` 再重启。随 Master 一起安装的本机执行节点通过回环地址连接，因此回环地址需要保持可访问。如果同一台主机上有反向代理对外服务，可以监听 `127.0.0.1:1685`。

Docker Compose 部署时，Master 端口发布到主机的所有网卡上，端口号由 `.env` 中的 `TRANSFER_PORT` 控制。

如果使用明文 HTTP，密码、会话令牌、执行节点凭证和存储密钥都以明文在网络上传输。只在可信的内网或 VPN 中才能接受 HTTP；其他场景，在 Master 前面配置 HTTPS。

## 用反向代理提供 HTTPS

Master 本身不处理 TLS。在它前面部署一个反向代理（例如 NGINX），用独立域名的根路径指向 Master。不支持子路径，例如 `/transfer/`。

```nginx
server {
    listen 443 ssl;
    server_name transfer.example.com;
    ssl_certificate     /etc/nginx/tls/transfer.example.com.crt;
    ssl_certificate_key /etc/nginx/tls/transfer.example.com.key;

    # 执行节点一次请求最多发送约 8 MiB 的结果。
    client_max_body_size 16m;

    location / {
        proxy_pass http://127.0.0.1:1685;
        proxy_set_header Host $http_host;
        # 执行节点的每次轮询最长保持 25 秒。
        proxy_read_timeout 60s;
    }
}
```

管理页面使用普通 HTTP 请求，执行节点使用 HTTP 长轮询，不需要 WebSocket 或事件流。

`proxy_read_timeout` 需要大于 25 秒。没有命令时，Master 把执行节点的轮询请求最多保持 25 秒。`client_max_body_size` 至少要允许约 8 MiB，因为执行节点一次请求最多发送 8 MiB 的结果，创建任务的请求最多 4 MiB，NGINX 默认的 1 MiB 不够。`Host` 头需要原样传递，否则 `Origin` 和 `Host` 不一致，Master 返回 *Cross-origin requests are not allowed*。

要让执行节点使用 HTTPS，在生成加入指令前，将 **Master 地址（Agent 需能访问）** 设置为 HTTPS 地址，例如 `https://transfer.example.com`。CLI 使用 `agent invite --master-url https://transfer.example.com`。

执行节点用主机信任的证书颁发机构来校验证书，公共 CA 签发的证书最简便。如果使用自签 CA，每台执行节点主机的信任库中都需要添加该 CA。执行节点不跟随重定向，直接填写 HTTPS 地址即可，不要填写会重定向到它的 HTTP 地址。

执行节点一直使用加入时的地址，因此应当在添加节点之前确定这个地址。内网中的执行节点也可以继续直接连接 `http://<内网IP>:1685`，浏览器通过代理访问即可。

所有访问都经过同一台主机上的代理时，可以让 Master 只监听 `127.0.0.1:1685`（见[网络暴露面](#网络暴露面)）。

## 存储凭证

凭证的流转路径如下：

1. 你在管理页面或用 `oss-transfer-cli storage add` 填写密钥。CLI 从环境变量或文件读取密钥（`--access-key-env`、`--secret-key-file` 等），不接受命令行中的明文，密钥不会出现在 shell 历史或进程列表中。
2. Master 把每个存储连接保存在 `master.db` 中，用 AES-256-GCM 加密，密钥文件是旁边的 `master.db.key`。
3. Master 把每个存储连接同步到每个执行节点。执行节点用同样的方式加密保存在 `agent.db` 中，密钥文件是 `agent.db.key`。也就是说，每个执行节点都拥有集群中的全部存储连接。
4. 管理页面显示 Access Key，但从不显示 Secret Key。编辑时留空密钥字段会保留已保存的值。存储错误详情中，密钥显示为 `[redacted]`。

为本次迁移单独创建密钥，只给[最小权限](planning.zh-CN.md#存储权限)，迁移完成后吊销。`storage remove` 会从 Master 和所有执行节点上删除该连接。只在和 Master 同样可信的主机上加入执行节点。

数据目录连同 `.key` 文件的备份中包含可用的凭证，保护好备份。如果只复制数据库而没有密钥文件，凭证无法读取，详见[数据库维护](database-maintenance.zh-CN.md)。

## 账号与密码

Master 只有一个管理员账号 `admin`。首次启动时，它生成一个随机密码，只输出一次：原生安装查看安装器输出和服务日志（Linux 上是 `/opt/oss-transfer/master/data/logs/service.log`），Docker 部署查看 `docker compose logs master`。

首次登录时必须修改密码。修改完成后，Master 在日志中记录一行 *Initial password for admin has been changed; the initial password printed earlier is no longer valid.*。原来那行仍留在日志中，但已经失效。

新密码至少 8 个字符，不能和用户名相同（忽略大小写和首尾空格），也不能和当前密码或初始密码相同。Master 还拒绝一小组容易猜到的密码：`password`、`qwerty123` 这类常见密码不区分大小写，同一字符重复如 `aaaaaaaa`，以及正序或倒序的连续数字或字母如 `12345678`、`87654321`、`abcdefgh`。管理页面和 `oss-transfer-cli change-password` 都在提交前检查。已有密码不受影响，修改密码时才检查这条规则。

密码以 Argon2id 哈希保存。多次登录失败不会锁定账号，但 Master 同时最多校验 4 个密码，超出的请求返回 HTTP 429。因此，限制网络访问是主要的防护手段。

忘记密码时，在 Master 主机上重置（见[重置管理员密码](operations.zh-CN.md#重置管理员密码)）。重置会让所有会话失效。

## 会话与 CLI 令牌

| 访问方式 | 工作方式 | 有效期 |
| --- | --- | --- |
| 管理页面 | 登录后得到一个令牌，浏览器只在当前标签页中保存 | 登录后 24 小时。**退出登录** 会结束会话；关闭标签页后浏览器不再保留它 |
| CLI `login` | 令牌按 Master 分别保存在用户配置目录中（Linux 为 `~/.config/oss-transfer-cli`，macOS 为 `~/Library/Application Support/oss-transfer-cli`，Windows 为 `%APPDATA%\oss-transfer-cli`），权限为 `0600` | 登录后 24 小时；`logout` 会结束会话 |
| Master 或执行节点主机上的 CLI | 服务会发布一个本机访问令牌，只有服务所属账号（root；macOS 用户服务为安装它的用户）能读取。因此 `sudo oss-transfer-cli …` 无需登录即可使用。 | 直到卸载该服务 |

修改密码会结束该账号的所有其他会话。任何 Master 或执行节点主机上的 root 都相当于集群管理员，因为本机访问令牌可以控制整个集群。

## 加入执行节点

加入指令中包含一个邀请，只能使用一次，30 分钟过期。在邀请被使用或过期之前，应当将加入指令作为机密对待。运行 `oss-transfer-cli agent invite-status <邀请ID>` 可以查看该邀请是否已被使用。

加入时，执行节点获得自己的凭证，保存在自己的数据目录中，之后每次请求 Master 时都携带该凭证。

在 **执行节点** 页面移除节点，或运行 `oss-transfer-cli agent remove <节点> --yes`，会吊销该凭证。该执行节点的服务随后停止且不再重启（Linux 上 systemd 显示为失败，状态码 78）。之后还需要在该执行节点的主机上用安装器的 `--uninstall --role agent` 卸载。Docker 执行节点删除容器和数据卷即可（见 [Docker Compose](docker-compose.zh-CN.md#删除安装)）。

## 哪些程序以 root 运行，为什么

| 部署方式 | 运行身份 | 原因 |
| --- | --- | --- |
| Linux 和 FreeBSD 原生服务 | root | 安装器要写入服务定义和程序目录，在线更新要替换它们。以 root 运行的执行节点可以读写任务中指定的任何本地目录。 |
| Windows 原生服务 | LocalSystem | 原因同上 |
| macOS 系统服务 / 用户服务（`--user`） | root / 当前用户 | 用户服务不需要管理员权限，但只能访问该用户的文件 |
| Docker 部署的 Master（Compose 文件） | UID 65532，只读根文件系统 | 容器内不需要 root |
| Docker 部署的执行节点（管理页面的 Docker 指令，或 Compose 安装器） | UID 65532，只读根文件系统，移除所有 capability，启用 `no-new-privileges` | 本地目录需要挂载进容器，并允许 UID 65532 访问 |

原生安装器需要 root（`sudo`），因为要创建系统服务。管理页面给出的 Docker 指令不需要 root，不过在 Linux 上，当前用户需要有运行 `docker` 的权限，而这在该主机上本身就相当于 root。

配置和数据目录以 `700` 权限创建，服务以 umask `077` 运行，只有服务所属账号能读取它们。

## 检查清单

1. 防火墙只允许运维人员和执行节点访问 1685 端口。
2. 通过反向代理使用 HTTPS，或让所有流量都留在内网中。
3. 立即修改初始密码，并且不要与其他地方的密码相同。
4. 使用只用于本次迁移、权限最小的存储密钥，迁移后吊销。
5. 移除不再使用的执行节点，并在其主机上卸载。
6. 保护好数据目录及其 `.key` 文件的备份。
