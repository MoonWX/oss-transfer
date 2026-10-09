#!/usr/bin/env bash
# Local Linux/systemd installation. No SSH is used.
set -euo pipefail
umask 077
fail() { printf 'Install failed: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'HELP'
Usage: install.sh --role master|agent [options]
  --package-dir DIR     Extracted release or agent bundle (default: parent of this script)
  --version VERSION     Release to download if no local package exists (default: 4.0.1)
  --package-url URL --checksum-url URL  Download a bundle from the master
  --master-url ORIGIN   Reachable master URL (required for agents)
  --join-token TOKEN    Single-use invitation (required for first agent installation)
  --address IPV4        Stable agent identity (default: detect local IPv4)
  --label NAME          Agent display name
  --web-url ORIGIN       Public WebUI URL (master)
  --port PORT           Local service port (default: 1685)
  --install-dir DIR --config-dir DIR --data-dir DIR --service-name NAME
  --cli-path PATH       CLI wrapper location (default: /usr/local/bin/oss-transfer-cli)
  --java-bin PATH       Java 8+ executable (default: detect)
  --no-start            Install without starting the service
Existing configuration and data are preserved. Run as root on Linux with systemd.
HELP
}
role='' version='4.0.1' package_dir='' package_url='' checksum_url=''
master_url='' join_token='' address='' label='' web_url='' port='1685'
install_dir='/opt/oss-transfer' config_dir='' data_dir='' service_name='' java_override='' cli_path='/usr/local/bin/oss-transfer-cli' no_start=0
while [[ $# -gt 0 ]]; do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --no-start) no_start=1; shift ;;
        --role|--version|--package-dir|--package-url|--checksum-url|--master-url|--join-token|--address|--label|--web-url|--port|--install-dir|--config-dir|--data-dir|--service-name|--java-bin|--cli-path)
            [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || fail "$1 requires a value"
            case "$1" in
                --role) role="$2";; --version) version="${2#v}";; --package-dir) package_dir="$2";;
                --package-url) package_url="$2";; --checksum-url) checksum_url="$2";;
                --master-url) master_url="${2%/}";; --join-token) join_token="$2";;
                --address) address="$2";; --label) label="$2";; --web-url) web_url="${2%/}";; --port) port="$2";;
                --install-dir) install_dir="$2";; --config-dir) config_dir="$2";; --data-dir) data_dir="$2";;
                --service-name) service_name="$2";; --java-bin) java_override="$2";; --cli-path) cli_path="$2";;
            esac
            shift 2 ;;
        *) fail "unknown option: $1" ;;
    esac
done
[[ "$role" == master || "$role" == agent ]] || fail '--role must be master or agent'
[[ "$(uname -s)" == Linux ]] || fail 'traditional deployment requires Linux'
[[ "$(id -u)" == 0 ]] || fail 'run the installer as root'
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]] || fail 'invalid release version'
[[ "$port" =~ ^[0-9]{1,5}$ ]] && ((10#$port >= 1 && 10#$port <= 65535)) || fail 'port must be between 1 and 65535'
port="$((10#$port))"
valid_origin() {
    [[ "$1" =~ ^https?://[A-Za-z0-9.-]+(:([0-9]{1,5}))?$ ]] || return 1
    local value="${BASH_REMATCH[2]:-}"
    [[ -z "$value" ]] || ((10#$value >= 1 && 10#$value <= 65535))
}
valid_agent_address() {
    [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    local parts value
    IFS=. read -r -a parts <<< "$1"
    for value in "${parts[@]}"; do
        [[ "$value" == 0 || "$value" != 0* ]] && ((10#$value <= 255)) || return 1
    done
    ((10#${parts[0]} > 0 && 10#${parts[0]} < 224 && 10#${parts[0]} != 127))
}
if [[ "$role" == agent ]]; then
    config_dir="${config_dir:-/etc/oss-transfer-agent}"
    data_dir="${data_dir:-/var/lib/oss-transfer-agent}"
    service_name="${service_name:-oss-transfer-agent}"
    if [[ -f "$config_dir/application.yml" ]]; then
        [[ -n "$master_url" ]] || master_url="$(sed -n "s/^transfer\.agent\.master-url: *'\([^']*\)' *$/\1/p" "$config_dir/application.yml" | head -n1)"
        [[ -n "$address" ]] || address="$(sed -n "s/^transfer\.agent\.address: *'\([^']*\)' *$/\1/p" "$config_dir/application.yml" | head -n1)"
    fi
    valid_origin "$master_url" || fail '--master-url must be an http(s) origin'
    [[ -z "$address" ]] || valid_agent_address "$address" || fail 'invalid agent IPv4 address'
else
    config_dir="${config_dir:-/etc/oss-transfer}"
    data_dir="${data_dir:-/var/lib/oss-transfer}"
    service_name="${service_name:-oss-transfer}"
    web_url="${web_url:-http://localhost:$port}"
    valid_origin "$web_url" || fail '--web-url must be an http(s) origin'
fi
for path in "$install_dir" "$config_dir" "$data_dir" "$cli_path"; do
    [[ "$path" =~ ^/[A-Za-z0-9_./-]+$ && "$path" != / && ! "$path" =~ (^|/)\.\.?(/|$) ]] || fail 'installation paths must be safe absolute paths without spaces'
    [[ ! -L "$path" ]] || fail 'installation paths must not be symbolic links'
done
[[ "$service_name" =~ ^[A-Za-z0-9_-]+$ ]] || fail 'invalid systemd service name'
[[ "$label" != *$'\n'* && "$label" != *$'\r'* ]] || fail 'node label must be one line'
if [[ -n "$join_token" ]]; then [[ "$join_token" =~ ^[A-Za-z0-9_-]{32,}$ && ${#join_token} -le 256 ]] || fail 'invalid join token'; fi
for dependency in systemctl curl timeout ss tar openssl sha256sum; do
    command -v "$dependency" >/dev/null || fail "missing dependency: $dependency"
done
here="$(cd "$(dirname "$0")" && pwd)"
# Detect the actual executable and verify its version before changing any service.
runtime="$(bash "$here/preflight-node.sh" "$java_override")" || fail 'Java preflight failed'
IFS=$'\t' read -r architecture java_bin java_major <<< "$runtime"
[[ "$java_major" =~ ^[0-9]{1,3}$ ]] && ((10#$java_major >= 8)) || fail 'preflight returned an invalid Java version'
case "$architecture" in x86_64) cli_arch=amd64;; aarch64|arm64) cli_arch=arm64;; *) fail 'unsupported CPU architecture';; esac
[[ "$java_bin" =~ ^/[A-Za-z0-9_./+-]+$ ]] || fail 'preflight returned an invalid Java path'
if [[ -f "$config_dir/application.yml" ]]; then
    printf 'Preserving configuration: %s/application.yml\n' "$config_dir"
    saved_port="$(sed -n 's/^server\.port:[[:space:]]*\([0-9][0-9]*\)[[:space:]]*$/\1/p' "$config_dir/application.yml" | head -n1)"
    if [[ -n "$saved_port" ]]; then
        [[ "$saved_port" =~ ^[0-9]{1,5}$ ]] && ((10#$saved_port >= 1 && 10#$saved_port <= 65535)) || fail 'existing configuration contains an invalid server port'
        port="$((10#$saved_port))"
    fi
elif [[ "$role" == agent && -z "$join_token" ]]; then
    fail '--join-token is required for a new agent'
fi
if [[ "$role" == agent && -z "$address" ]]; then
    route_host="${master_url#*://}"
    route_host="${route_host%%:*}"
    address="$(ip -4 route get "$route_host" 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n1 || true)"
    [[ -n "$address" ]] || address="$(hostname -I 2>/dev/null | awk '{print $1}')"
    valid_agent_address "$address" || fail 'cannot detect IPv4 identity; specify --address'
fi
stage="$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-install.XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if [[ -z "$package_dir" && -z "$package_url" && -f "$here/../oss-transfer.jar" ]]; then package_dir="$(cd "$here/.." && pwd)"; fi
if [[ -z "$package_dir" ]]; then
    package_url="${package_url:-https://github.com/MoonWX/oss-transfer/releases/download/v$version/oss-transfer.tar.gz}"
    checksum_url="${checksum_url:-${package_url}.sha256}"
    for url in "$package_url" "$checksum_url"; do
        [[ "$url" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?/[^[:space:]]+$ && "$url" != *$'\n'* ]] || fail 'invalid download URL'
    done
    curl --fail --silent --show-error --location --connect-timeout 10 --max-time 300 "$package_url" -o "$stage/package.tar.gz"
    curl --fail --silent --show-error --location --connect-timeout 10 --max-time 30 "$checksum_url" -o "$stage/checksum"
    expected="$(awk 'NR==1 {print $1}' "$stage/checksum")"
    [[ "$expected" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'invalid package checksum'
    actual="$(sha256sum "$stage/package.tar.gz" | awk '{print $1}')"
    [[ "${actual,,}" == "${expected,,}" ]] || fail 'package checksum mismatch'
    # Reject traversal, links and special files before extracting an untrusted archive.
    tar -tzf "$stage/package.tar.gz" > "$stage/entries"
    if grep -Eq '^/|(^|/)\.\.(/|$)' "$stage/entries"; then fail 'unsafe archive path'; fi
    if tar -tvzf "$stage/package.tar.gz" | awk 'substr($0,1,1)!="-" && substr($0,1,1)!="d" {bad=1} END {exit !bad}'; then fail 'archive contains links or special files'; fi
    mkdir "$stage/package"
    tar -xzf "$stage/package.tar.gz" --no-same-owner -C "$stage/package"
    if [[ -f "$stage/package/oss-transfer.jar" ]]; then package_dir="$stage/package";
    else package_dir="$stage/package/oss-transfer"; fi
fi
for required in oss-transfer.jar "oss-transfer-cli-$cli_arch" VERSION deploy/runtime.sh deploy/wait-node-ready.sh; do
    [[ -f "$package_dir/$required" && ! -L "$package_dir/$required" ]] || fail "package is missing $required"
done
# Configuration is prepared before a healthy existing service is stopped.
if [[ ! -f "$config_dir/application.yml" ]]; then
    internal="$(openssl rand -hex 32)"
    password="$(openssl rand -hex 24)"
    yaml_quote() { local value="$1"; printf "'%s'" "${value//\'/\'\'}"; }
    cat > "$stage/application.yml" <<YAML
server.port: $port
server.address: $(if [[ "$role" == agent ]]; then printf '127.0.0.1'; else printf '0.0.0.0'; fi)
transfer.port: $port
transfer.role: $role
transfer.master-ip: 127.0.0.1
transfer.local-ip: $(if [[ "$role" == agent ]]; then printf '%s' "$address"; else printf '127.0.0.1'; fi)
transfer.worker-ips: 127.0.0.1
transfer.master-web-url: $(yaml_quote "${web_url:-$master_url}")
transfer.internal-token: $(yaml_quote "$internal")
transfer.agent.master-url: $(yaml_quote "$master_url")
transfer.agent.join-token: $(yaml_quote "$join_token")
transfer.agent.address: $(yaml_quote "$address")
transfer.agent.label: $(yaml_quote "$label")
transfer.agent.package-path: $install_dir/agent-bundle.tar.gz
rocksdb.path: $data_dir/rocksdb
transfer.tmp-dir: $data_dir/tmp-objects
logging.path: $data_dir/logs
logging.file: $data_dir/logs/oss-transfer.log
auth.default.username: admin
auth.default.password: $(yaml_quote "$password")
auth.default.reset-password-on-startup: false
YAML
    printf '%s\n' "$password" > "$stage/admin-password"
    unset internal password
fi
mkdir -p "$install_dir" "$config_dir" "$data_dir/logs" "$data_dir/tmp-objects"
chmod 0700 "$config_dir" "$data_dir"
if [[ -f "$stage/application.yml" ]]; then
    install -m 0600 "$stage/application.yml" "$config_dir/application.yml"
    install -m 0600 "$stage/admin-password" "$config_dir/admin-password"
fi
systemctl stop "$service_name.service" 2>/dev/null || true
install -m 0644 "$package_dir/oss-transfer.jar" "$install_dir/oss-transfer.jar"
install -m 0755 "$package_dir/oss-transfer-cli-$cli_arch" "$install_dir/oss-transfer-cli-bin"
install -m 0755 "$package_dir/deploy/runtime.sh" "$install_dir/runtime.sh"
install -m 0644 "$package_dir/VERSION" "$install_dir/VERSION"
if [[ -f "$package_dir/agent-bundle.tar.gz" ]]; then install -m 0644 "$package_dir/agent-bundle.tar.gz" "$install_dir/agent-bundle.tar.gz"; fi
cat > "/etc/systemd/system/$service_name.service" <<UNIT
[Unit]
Description=OSS Transfer $role
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
WorkingDirectory=$data_dir
ExecStart=$install_dir/runtime.sh $java_bin $install_dir/oss-transfer.jar $config_dir/application.yml
Restart=on-failure
RestartSec=5
KillMode=control-group
TimeoutStopSec=90
UMask=0077
NoNewPrivileges=true
[Install]
WantedBy=multi-user.target
UNIT
# This local administration wrapper reads the current configuration, not a copied token.
mkdir -p "$(dirname "$cli_path")"
cat > "$cli_path" <<WRAPPER
#!/usr/bin/env bash
set -euo pipefail
export OSS_TRANSFER_CONFIG_FILE='$config_dir/application.yml'
export OSS_TRANSFER_MASTER_SERVER='${master_url:-http://127.0.0.1:$port}'
cd '$config_dir'
exec '$install_dir/oss-transfer-cli-bin' "\$@"
WRAPPER
chmod 0755 "$cli_path"
systemctl daemon-reload
systemctl enable "$service_name.service"
if [[ "$no_start" == 0 ]]; then
    systemctl start "$service_name.service"
    bash "$package_dir/deploy/wait-node-ready.sh" 127.0.0.1 "$port" 120 "$service_name" || fail "service is not ready; inspect journalctl -u $service_name"
fi
printf 'Installed %s service: %s\nConfiguration: %s/application.yml\nData: %s\n' "$role" "$service_name" "$config_dir" "$data_dir"
if [[ "$role" == master ]]; then printf 'WebUI: %s\nBootstrap admin password: %s/admin-password\n' "$web_url" "$config_dir"; else printf 'Agent connects to: %s\n' "$master_url"; fi
