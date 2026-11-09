# 安全说明

[English](security.md)

本页说明谁能访问 Master、如何加上 HTTPS、存储凭证会流向哪里、账号、会话和加入指令如何工作，以及哪些程序以管理员权限运行。需要开放的端口和要授予的存储权限见[规划迁移](planning.zh-CN.md)。

## 网络暴露面

- Master 在同一个端口上为管理页面、API 和执行节点（Agent）提供服务：默认 `0.0.0.0:1685`，使用明文 HTTP。能访问这个端口的人都能看到登录页面。
- 只有两类访问方需要连接它：操作迁移的人员（浏览器或 CLI），以及执行节点。执行节点不需要任何入站端口，所有连接都由它主动发起。
- 请用主机或云防火墙把这个端口限制为只允许运维人员和执行节点的地址或网段访问。
- 要只监听某个地址，安装 Master 时加 `--listen HOST:PORT`（`Install.ps1` 用 `-Listen`），或修改 Master `service.json` 中的 `listen` 后重启服务。随 Master 一起安装的本机执行节点通过回环地址连接，所以请保留回环地址可访问：同一台主机上有反向代理为其他人提供服务时，可以使用 `127.0.0.1:1685`。
- 使用 Docker Compose 时，Master 的端口会发布在主机的所有网卡上（`.env` 中的 `TRANSFER_PORT`）。
- 使用明文 HTTP 时，密码、会话令牌、执行节点凭证和存储密钥都以明文在网络上传输。只在可信的内网或 VPN 中使用 HTTP；其他情况请在 Master 前面加 HTTPS。

## 用反向代理提供 HTTPS

Master 本身不处理 TLS。请在它前面放一个反向代理（例如 NGINX），并使用独立域名的根路径（不支持 `/transfer/` 这样的子路径）：

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

这些设置的原因：

- **不需要 WebSocket 或事件流相关设置。** 管理页面使用普通 HTTP 请求，执行节点使用 HTTP 长轮询。
- **`proxy_read_timeout` 必须大于 25 秒。** 没有命令时，Master 会把执行节点的轮询请求最多保持 25 秒。
- **`client_max_body_size` 至少要允许约 8 MiB。** 执行节点一次请求最多发送 8 MiB 的结果，创建任务的请求最多 4 MiB。NGINX 默认的 1 MiB 会拒绝它们。
- **原样传递 `Host` 头。** 请求的 `Origin` 与 `Host` 不一致时，Master 会以 *Cross-origin requests are not allowed* 拒绝。

要让执行节点使用 HTTPS，请在生成加入指令前，把 **Master 地址（Agent 需能访问）** 填为 HTTPS 地址，例如 `https://transfer.example.com`（或使用 `agent invite --master-url https://transfer.example.com`）。注意：

- 执行节点会用主机信任的证书颁发机构校验证书。请使用公共证书颁发机构签发的证书，或把你自己的颁发机构加入每台执行节点主机的信任库。
- 执行节点不跟随重定向。请直接填写 HTTPS 地址，不要填写会重定向到它的 HTTP 地址。
- 执行节点会一直使用加入时的地址。请在添加执行节点之前确定地址。内网中的执行节点也可以继续直接连接 `http://<内网IP>:1685`，浏览器则通过代理访问。

所有访问都经过同一台主机上的代理时，可以让 Master 只监听 `127.0.0.1:1685`（见[网络暴露面](#网络暴露面)）。

## 存储凭证

1. 你在管理页面或用 `oss-transfer-cli storage add` 填写密钥。CLI 从环境变量或文件读取密钥（`--access-key-env`、`--secret-key-file` 等），不接受命令行中的明文，所以密钥不会出现在 shell 历史或进程列表中。
2. Master 把每个存储连接保存在 `master.db` 中，用 AES-256-GCM 加密，密钥文件是旁边的 `master.db.key`。
3. Master 会把**每个**存储连接同步到**每个**执行节点，执行节点用同样的方式加密保存在 `agent.db` 中，密钥文件是 `agent.db.key`。因此每个执行节点都能使用集群中的所有存储连接。
4. 管理页面会显示 Access Key，但从不显示 Secret Key。编辑时留空密钥字段会保留已保存的值。存储错误详情中，该连接的密钥会被替换为 `[redacted]`。

建议：

- 使用专为本次迁移创建、只有[最小权限](planning.zh-CN.md#存储权限)的密钥，迁移结束后吊销。`storage remove` 会从 Master 和所有执行节点上删除该连接。
- 数据目录连同其 `.key` 文件的副本中包含可用的凭证，请同样保护好备份。只复制数据库而没有密钥文件时，凭证无法被读出。见[数据库维护](database-maintenance.zh-CN.md)。
- 只在与 Master 同样可信的主机上加入执行节点。

## 账号与密码

- Master 只有一个管理员账号 `admin`，首次启动时生成随机密码。这个密码只输出一次：在安装器输出和服务日志中（Linux 上为 `/opt/oss-transfer/master/data/logs/service.log`），Docker 部署时在 `docker compose logs master` 中。
- 首次登录时必须设置新密码。之后 Master 会在日志中记录 *Initial password for admin has been changed; the initial password printed earlier is no longer valid.*，原来那一行仍留在日志中，但已经失效。
- 新密码至少 8 个字符，不能与用户名相同（忽略大小写和首尾空格），也不能与当前密码或初始密码相同。新密码也不能容易猜到：Master 会拒绝一小组非常常见的密码，例如 `password`、`qwerty123`（不区分大小写），同一字符重复（`aaaaaaaa`），以及正序或倒序连续的数字或字母（`12345678`、`87654321`、`abcdefgh`）。管理页面和 `oss-transfer-cli change-password` 会在提交前检查。已有密码仍可使用，修改密码时才检查这条规则。
- 密码以 Argon2id 哈希保存。
- 多次登录失败不会锁定账号；Master 同时最多校验 4 个密码，其余请求返回 HTTP 429。因此限制网络访问是主要的防护手段。
- 忘记密码时，在 Master 主机上重置（见[重置管理员密码](operations.zh-CN.md#重置管理员密码)）。重置会使所有会话失效。

## 会话与 CLI 令牌

| 访问方式 | 工作方式 | 有效期 |
| --- | --- | --- |
| 管理页面 | 登录后得到一个令牌，浏览器只在当前标签页中保存 | 登录后 24 小时。**退出登录** 会结束会话；关闭标签页后浏览器不再保留它 |
| CLI `login` | 令牌按 Master 分别保存在用户配置目录中（Linux 为 `~/.config/oss-transfer-cli`，macOS 为 `~/Library/Application Support/oss-transfer-cli`，Windows 为 `%APPDATA%\oss-transfer-cli`），权限为 `0600` | 登录后 24 小时；`logout` 会结束会话 |
| Master 或执行节点主机上的 CLI | 服务会发布一个本机访问令牌，只有服务所属账号（root；macOS 用户服务为安装它的用户）能读取。因此 `sudo oss-transfer-cli …` 无需登录即可使用。 | 直到卸载该服务 |

修改密码会结束该账号的所有其他会话。请把任何 Master 或执行节点主机上的 root 视为集群管理员：本机访问令牌可以控制整个集群。

## 加入执行节点

- 加入指令中包含一个邀请，只能使用一次，30 分钟后过期。在它被使用或过期之前，请把它当作机密。用 `oss-transfer-cli agent invite-status <邀请ID>` 可以查看它是否已被使用。
- 加入时，执行节点会获得自己的凭证，保存在它的数据目录中，并在每次请求 Master 时携带。
- 在 **执行节点** 页面移除节点，或运行 `oss-transfer-cli agent remove <节点> --yes`，会吊销这个凭证。该执行节点的服务随后会停止且不再重启（Linux 上 systemd 显示为失败，状态码 78）。请在它的主机上用安装器的 `--uninstall --role agent` 卸载，Docker 执行节点则删除其容器和数据卷（见 [Docker Compose](docker-compose.zh-CN.md#删除安装)）。

## 哪些程序以 root 运行，为什么

| 部署方式 | 运行身份 | 原因 |
| --- | --- | --- |
| Linux 和 FreeBSD 原生服务 | root | 安装器要写入服务定义和程序目录，在线更新要替换它们。以 root 运行的执行节点可以读写任务中指定的任何本地目录。 |
| Windows 原生服务 | LocalSystem | 原因同上 |
| macOS 系统服务 / 用户服务（`--user`） | root / 当前用户 | 用户服务不需要管理员权限，但只能访问该用户的文件 |
| Docker 部署的 Master（Compose 文件） | UID 65532，只读根文件系统 | 容器内不需要 root |
| Docker 部署的执行节点（管理页面的 Docker 指令，或 Compose 安装器） | UID 65532，只读根文件系统，去掉所有 capability，启用 `no-new-privileges` | 本地目录需要挂载进容器，并允许 UID 65532 访问 |

原生安装器需要 root（`sudo`），因为它们要创建系统服务。管理页面的 Docker 指令不需要 root；在 Linux 上，当前用户需要有运行 `docker` 的权限，而这在该主机上本身就相当于 root。配置和数据目录以 `700` 权限创建，服务以 umask `077` 运行，所以只有服务所属账号能读取它们。

## 检查清单

1. 防火墙只允许运维人员和执行节点访问 1685 端口。
2. 通过反向代理使用 HTTPS，或让所有流量都留在内网中。
3. 立即修改初始密码，并且不要与其他地方的密码相同。
4. 使用只用于本次迁移、权限最小的存储密钥，迁移后吊销。
5. 移除不再使用的执行节点，并在其主机上卸载。
6. 保护好数据目录及其 `.key` 文件的备份。
