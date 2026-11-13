# Security

[中文](security.zh-CN.md)

This page is for you, the person deploying OSS Transfer into a production network. After reading it you will know who can access the Master, how to configure HTTPS, where storage credentials are stored and where they flow, how passwords and sessions are managed, and which components run as root. The complete list of ports and storage permissions is in [Plan a migration](planning.md).

## Network exposure

The Master serves everything on one port: the console, the API and Agent communication all go through `0.0.0.0:1685` by default, over plain HTTP. Anyone who can reach that port sees the sign-in page.

Only two groups need to reach it: the people who operate the migration (browser or CLI) and the Agents. Agents do not need any inbound ports; they initiate every connection themselves.

Your first step, therefore, is to restrict port 1685 with a host or cloud firewall so that only the addresses or subnets of operators and Agents can reach it.

If you want the Master to listen on a specific address, pass `--listen HOST:PORT` at install time (`-Listen` for `Install.ps1`), or edit `listen` in the Master's `service.json` and restart. The local Agent installed alongside the Master connects over loopback, so keep loopback reachable. When a reverse proxy on the same host serves external traffic, you can listen on `127.0.0.1:1685`.

With Docker Compose, the Master's port is published on every interface of the host. The port number comes from `TRANSFER_PORT` in `.env`.

Over plain HTTP, passwords, session tokens, Agent credentials and storage keys all travel unencrypted. Accept HTTP only inside a trusted private network or VPN; everywhere else, put HTTPS in front of the Master.

## HTTPS with a reverse proxy

The Master does not terminate TLS itself. Put a reverse proxy such as NGINX in front of it, at the root of its own domain. Subpaths such as `/transfer/` are not supported.

```nginx
server {
    listen 443 ssl;
    server_name transfer.example.com;
    ssl_certificate     /etc/nginx/tls/transfer.example.com.crt;
    ssl_certificate_key /etc/nginx/tls/transfer.example.com.key;

    # Agents send results of up to about 8 MiB in one request.
    client_max_body_size 16m;

    location / {
        proxy_pass http://127.0.0.1:1685;
        proxy_set_header Host $http_host;
        # Agents hold each poll open for up to 25 seconds.
        proxy_read_timeout 60s;
    }
}
```

The console uses ordinary HTTP requests and Agents use HTTP long polling. No WebSocket or event-stream settings are needed.

`proxy_read_timeout` must be longer than 25 seconds. When there is nothing to do, the Master holds an Agent's poll open for up to 25 seconds. `client_max_body_size` must allow at least about 8 MiB, because an Agent sends up to 8 MiB of results in one request and task-creation requests can reach 4 MiB; NGINX's default of 1 MiB is not enough. The `Host` header must be passed through unchanged, or `Origin` and `Host` will not match and the Master will return *Cross-origin requests are not allowed*.

To have Agents connect over HTTPS, set **Master address (reachable from the Agent)** to the HTTPS address, for example `https://transfer.example.com`, before you generate join commands. With the CLI, use `agent invite --master-url https://transfer.example.com`.

Agents verify the certificate against the host's trusted certificate authorities. A certificate from a public CA is the simplest choice. If you use your own CA, add it to the trust store on every Agent host. Agents do not follow redirects; give them the HTTPS address directly, not an HTTP address that redirects to it.

Agents keep the address they joined with, so settle on the address before you add Agents. Agents on a private network can keep connecting to `http://<private-ip>:1685` directly while browsers go through the proxy.

When all traffic goes through a proxy on the same host, you can bind the Master to `127.0.0.1:1685` (see [Network exposure](#network-exposure)).

## Storage credentials

Here is how credentials flow through the system:

1. You enter the keys in the console or with `oss-transfer-cli storage add`. The CLI reads keys from environment variables or files (`--access-key-env`, `--secret-key-file`, and so on) and does not accept them on the command line, so they do not appear in the shell history or process list.
2. The Master saves each storage connection in `master.db`, encrypted with AES-256-GCM. The key file is `master.db.key` beside it.
3. The Master syncs every storage connection to every Agent. Each Agent stores it in `agent.db`, encrypted the same way with `agent.db.key`. This means every Agent holds every storage connection in the cluster.
4. The console shows the access key but never the secret key. Leaving the key fields empty when editing keeps the saved values. In storage error details, keys appear as `[redacted]`.

Create keys specifically for the migration, grant only the [minimum permissions](planning.md#storage-permissions), and revoke them when the migration is done. `storage remove` deletes a connection from the Master and every Agent. Only join Agents on hosts you trust as much as the Master.

Backups of a data directory that include the `.key` file contain usable credentials; protect them accordingly. If the database is copied without the key file, the credentials cannot be read. See [database maintenance](database-maintenance.md) for details.

## Accounts and passwords

The Master has one administrator account, `admin`. At first start it generates a random password and prints it once: look in the installer output and the service log (`/opt/oss-transfer/master/data/logs/service.log` on Linux) for a native installation, or run `docker compose logs master` for a Docker deployment.

You must change the password at first sign-in. After the change, the Master logs *Initial password for admin has been changed; the initial password printed earlier is no longer valid.* The old line stays in the log but no longer works.

A new password needs at least 8 characters, must not match the username (ignoring case and surrounding spaces), and must differ from both the current password and the initial one. The Master also rejects a short list of easy-to-guess passwords: common strings such as `password` or `qwerty123` (case-insensitive), a single character repeated (`aaaaaaaa`), and consecutive digits or letters in either direction (`12345678`, `87654321`, `abcdefgh`). Both the console and `oss-transfer-cli change-password` check these rules before submitting. Existing passwords are unaffected; the rules apply only when a password is changed.

Passwords are stored as Argon2id hashes. Failed sign-ins do not lock the account, but the Master checks at most 4 passwords at a time and returns HTTP 429 for the rest. Limiting network access is therefore the main protection.

If you lose the password, reset it on the Master host (see [reset the administrator password](operations.md#reset-the-administrator-password)). A reset ends every active session.

## Sessions and CLI tokens

| Access | How it works | Lifetime |
| --- | --- | --- |
| Console | Sign-in returns a token that the browser keeps for the current tab only | 24 hours from sign-in. **Sign out** ends it; closing the tab makes the browser forget it |
| CLI `login` | The token is saved per Master in the user's configuration directory (`~/.config/oss-transfer-cli` on Linux, `~/Library/Application Support/oss-transfer-cli` on macOS, `%APPDATA%\oss-transfer-cli` on Windows) with permissions `0600` | 24 hours from sign-in; `logout` ends it |
| CLI on a Master or Agent host | The service publishes a local access token that only its owner (root, or the installing user for a macOS user service) can read. `sudo oss-transfer-cli …` therefore works without signing in. | Until the service is uninstalled |

Changing the password ends every other session of that account. Root on any Master or Agent host is effectively a cluster administrator, because the local access token gives control of the entire cluster.

## Joining Agents

The join command contains a single-use invitation that expires after 30 minutes. Treat the join command as a secret until the invitation is used or has expired. You can check whether an invitation has been used with `oss-transfer-cli agent invite-status <invitation-ID>`.

When an Agent joins, it receives its own credential, stored in its data directory, and sends it with every request to the Master.

Removing a node on the **Execution nodes** page, or with `oss-transfer-cli agent remove <node> --yes`, revokes that credential. The Agent's service then stops and does not restart (on Linux, systemd shows it as failed with status 78). Uninstall it on its host with the installer's `--uninstall --role agent`. For a Docker Agent, remove the container and its volume (see [Docker Compose](docker-compose.md#remove-an-installation)).

## What runs as root and why

| Deployment | Runs as | Why |
| --- | --- | --- |
| Native service on Linux and FreeBSD | root | The installer writes the service definition and program directories, and online updates replace them. A root Agent can read and write any local directory a task names. |
| Native service on Windows | LocalSystem | Same reasons |
| macOS system service / user service (`--user`) | root / the current user | A user service needs no administrator rights but can only access that user's files |
| Docker Master (Compose file) | UID 65532, read-only root file system | No root needed inside the container |
| Docker Agent (console command, or the Compose installer) | UID 65532, read-only root file system, all capabilities dropped, `no-new-privileges` | Local directories must be mounted and accessible to UID 65532 |

The native installers need root (`sudo`) because they create system services. The console's Docker command does not need root, but on Linux your user must be allowed to run `docker`, which is itself equivalent to root on that host.

Configuration and data directories are created with mode `700`, and the service runs with umask `077`, so only the service owner can read them.

## Checklist

1. Firewall port 1685 to operators and Agents only.
2. Use HTTPS through a reverse proxy, or keep all traffic on a private network.
3. Change the initial password at once, to one used nowhere else.
4. Use migration-only storage keys with minimum permissions; revoke them afterwards.
5. Remove Agents you no longer use, and uninstall them on their hosts.
6. Protect backups of the data directories and their `.key` files.
