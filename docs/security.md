# Security

[中文](security.zh-CN.md)

This page explains who can reach the Master, how to add HTTPS, where storage credentials go, how accounts, sessions and join commands work, and what runs with administrator rights. For the ports and the storage permissions to grant, see [planning](planning.md).

## Network exposure

- The Master serves the console, the API and the Agents on one port: `0.0.0.0:1685` by default, over plain HTTP. Anyone who can reach that port sees the sign-in page.
- Only two groups need to reach it: the people who operate the migration (browser or CLI) and the Agents. Agents need no inbound ports at all: they open every connection themselves.
- Restrict the port with a host or cloud firewall to the operators' addresses and the Agents' addresses or subnets.
- To bind one address, install the Master with `--listen HOST:PORT` (`-Listen` for `Install.ps1`), or change `listen` in the Master's `service.json` and restart the service. The local Agent installed together with the Master connects over the loopback address, so keep loopback reachable: `127.0.0.1:1685` works when a reverse proxy on the same host serves everyone else.
- With Docker Compose, the Master's port is published on every interface of the host (`TRANSFER_PORT` in `.env`).
- Over plain HTTP, passwords, session tokens, Agent credentials and storage keys cross the network unencrypted. Use HTTP only inside a trusted private network or VPN; elsewhere, put HTTPS in front of the Master.

## HTTPS with a reverse proxy

The Master does not terminate TLS itself. Put a reverse proxy such as NGINX in front of it, at the root of its own domain (subpaths such as `/transfer/` are not supported):

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

Why these settings:

- **No WebSocket or event-stream settings are needed.** The console uses ordinary HTTP requests, and Agents use HTTP long polling.
- **`proxy_read_timeout` must be longer than 25 seconds,** because the Master holds an Agent's poll open for up to 25 seconds when there is nothing to do.
- **`client_max_body_size` must allow about 8 MiB.** An Agent sends results of up to 8 MiB in one request, and task requests can be up to 4 MiB. NGINX's default of 1 MiB rejects them.
- **Pass the `Host` header unchanged.** The Master rejects a request whose `Origin` does not match its `Host` with *Cross-origin requests are not allowed*.

To let Agents use HTTPS, enter the HTTPS address, for example `https://transfer.example.com`, as **Master address (reachable from the Agent)** before you generate the join commands (or `agent invite --master-url https://transfer.example.com`). Then:

- The Agent checks the certificate against the host's trusted certificate authorities. Use a certificate from a public authority, or add your own authority to the trust store of each Agent host.
- The Agent does not follow redirects. Give it the HTTPS address itself, not an HTTP address that redirects to it.
- An Agent keeps the address it joined with. Choose the address before you add Agents. Agents on a private network may also keep connecting to `http://<private-ip>:1685` directly while browsers use the proxy.

When everyone goes through the proxy on the same host, bind the Master to `127.0.0.1:1685` (see [network exposure](#network-exposure)).

## Storage credentials

1. You enter the keys in the console or with `oss-transfer-cli storage add`. The CLI reads them from environment variables or files (`--access-key-env`, `--secret-key-file`, …), never from command-line values, so they do not appear in the shell history or process list.
2. The Master saves each connection in `master.db`, encrypted with AES-256-GCM. The key is in `master.db.key` beside it.
3. The Master sends **every** storage connection to **every** Agent, which saves it in `agent.db`, encrypted the same way with `agent.db.key`. Each Agent can therefore use every connection of the cluster.
4. The console shows the access key but never the secret key. Leaving the key fields empty when editing keeps the saved values. Storage error details have the connection's keys replaced with `[redacted]`.

Recommendations:

- Use keys made for the migration, with the [minimum permissions](planning.md#storage-permissions), and revoke them when the migration is done. `storage remove` removes a connection from the Master and every Agent.
- Copies of a data directory with its `.key` file contain usable credentials; protect backups accordingly. A database copied without its key does not reveal them. See [database maintenance](database-maintenance.md).
- Only join Agents on hosts you trust as much as the Master.

## Accounts and passwords

- The Master has one administrator account, `admin`, created at first start with a random password. The password is printed once: in the installer's output and the service log (`/opt/oss-transfer/master/data/logs/service.log` on Linux), or in `docker compose logs master` for Docker.
- You must choose a new password at the first sign-in. After that, the Master logs *Initial password for admin has been changed; the initial password printed earlier is no longer valid.* The old line stays in the log but no longer works.
- A new password needs at least 8 characters, must not equal the user name (ignoring case and surrounding spaces) and must differ from the current or initial password. It also must not be easy to guess: the Master rejects a short list of very common passwords such as `password` or `qwerty123` (ignoring case), one character repeated (`aaaaaaaa`) and a run of consecutive digits or letters in either direction (`12345678`, `87654321`, `abcdefgh`). The console and `oss-transfer-cli change-password` check this before sending. Existing passwords keep working; the rule applies when a password is changed.
- Passwords are stored as Argon2id hashes.
- There is no lockout after failed sign-ins; the Master checks at most 4 passwords at a time and answers others with HTTP 429. Limiting network access is therefore the main protection.
- If the password is lost, reset it on the Master host ([reset the administrator password](operations.md#reset-the-administrator-password)). Resetting signs out every session.

## Sessions and CLI tokens

| Access | How it works | Lifetime |
| --- | --- | --- |
| Console | Sign-in returns a token that the browser keeps for that tab only | 24 hours from sign-in. **Sign out** ends it; closing the tab makes the browser forget it |
| CLI `login` | The token is saved per Master in the user's configuration directory (`~/.config/oss-transfer-cli` on Linux, `~/Library/Application Support/oss-transfer-cli` on macOS, `%APPDATA%\oss-transfer-cli` on Windows) with permissions `0600` | 24 hours from sign-in; `logout` ends it |
| CLI on a Master or Agent host | The service publishes a local access token that only its owner (root, or the installing user for a macOS user service) can read. `sudo oss-transfer-cli …` then works without a login. | Until the service is uninstalled |

Changing the password ends every other session of the account. Treat root on any Master or Agent host as a cluster administrator: the local access token gives control of the cluster.

## Joining Agents

- A join command contains a single-use invitation that expires after 30 minutes. Treat it as a secret until it is used or has expired. Check whether it was used with `oss-transfer-cli agent invite-status <invitation-ID>`.
- When it joins, the Agent receives its own credential, saved in its data directory, and sends it with each request to the Master.
- Removing a node on the **Execution nodes** page, or with `oss-transfer-cli agent remove <node> --yes`, revokes that credential. The Agent's service then stops and does not restart (on Linux, systemd shows it failed with status 78). Uninstall it on its host with the installer's `--uninstall --role agent`, or remove a Docker Agent's container and volume ([Docker Compose](docker-compose.md#remove-an-installation)).

## What runs as root and why

| Deployment | Runs as | Why |
| --- | --- | --- |
| Native service on Linux and FreeBSD | root | The installer writes the service definition and the program directories, and online updates replace them. A root Agent can read and write any local directory a task names. |
| Native service on Windows | LocalSystem | The same reasons |
| macOS system service / user service (`--user`) | root / the current user | A user service needs no administrator rights but can only access that user's files |
| Docker Master (Compose file) | UID 65532, read-only root file system | No root needed inside the container |
| Docker Agent (console command, or the Compose installer) | UID 65532, read-only root file system, all capabilities dropped, `no-new-privileges` | Local directories must be mounted and accessible to UID 65532 |

The native installers need root (`sudo`) because they create system services. The console's Docker command needs no root; on Linux, your user must be allowed to run `docker`, which is itself equivalent to root on that host. Configuration and data directories are created with mode `700` and the service runs with umask `077`, so only the service owner can read them.

## Checklist

1. Firewall port 1685 to operators and Agents only.
2. Use HTTPS through a reverse proxy, or keep all traffic on a private network.
3. Change the initial password at once, to one used nowhere else.
4. Use migration-only storage keys with minimum permissions; revoke them afterwards.
5. Remove Agents you no longer use, and uninstall them on their hosts.
6. Protect backups of the data directories and their `.key` files.
