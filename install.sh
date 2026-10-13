#!/usr/bin/env bash
# Interactive, local deployment. Native upgrades remain in deploy/install.sh.
set +x
set -euo pipefail
umask 077

lang='' yes=0 no_deps=0 role='' method='' source='' version='latest'
package_dir='' package_url='' checksum_url='' install_dir='' config_dir='' data_dir=''
service_name='' cli_path='/usr/local/bin/oss-transfer-cli' port='1685' web_url=''
join_url='' address='' files_dir='' image='' stage='' confirmed='' choice=''
original_args=("$@")
port_given=0 web_given=0 files_given=0 image_given=0
say() { if [[ "$lang" == zh ]]; then printf '%s\n' "$2"; else printf '%s\n' "$1"; fi; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'HELP'
OSS Transfer installer (new deployments, Linux amd64/arm64)
Usage: sudo bash install.sh [options]
  --lang en|zh                  Prompt language
  --role master|agent           Deploy a master or join an existing master
  --method compose|native       Docker Compose (default) or Java/systemd
  --source oss                  Native master download source (default: oss)
  --version latest|VERSION      Release version (default: latest)
  --web-url URL                 Reachable master origin, e.g. http://192.0.2.10:1685
  --join-url URL                Agent invitation link from the master's Nodes page
  --address IPV4                Agent identity; match the invitation if specified
  --port PORT                   Service port (default: 1685)
  --install-dir DIR             Deployment directory (alias: --deployment-dir)
  --data-dir DIR                Persistent data directory
  --files-dir DIR               Optional Docker local-files mount, same path inside
  --image IMAGE                 Docker image (default: moonwx/osstransfer:VERSION)
  --package-dir DIR             Use an extracted local package (native only)
  --package-url URL --checksum-url URL  Explicit native package download
  --config-dir DIR --cli-path PATH --service-name NAME
  --no-deps                     Require dependencies to be installed already
  --yes                        Use defaults and skip prompts; --web-url or --join-url required
  -h, --help                    Show this help

Automatic dependency installation supports Debian and Ubuntu. On other distributions,
install Java 8+/systemd or Docker Compose and common utilities before running this script.
Existing deployments are not overwritten. Use the regular installer or Compose to upgrade.
HELP
}
while [[ $# -gt 0 ]]; do
    case "$1" in
        -h|--help) usage; exit 0;;
        --yes) yes=1; shift;;
        --no-deps) no_deps=1; shift;;
        --lang|--role|--method|--mode|--source|--version|--package-dir|--package-url|--checksum-url|--install-dir|--deployment-dir|--config-dir|--data-dir|--service-name|--cli-path|--port|--web-url|--join-url|--address|--files-dir|--image)
            [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || fail "$1 requires a value"
            case "$1" in
                --lang) lang="$2";; --role) role="$2";; --method|--mode) method="$2";;
                --source) source="$2";; --version) version="${2#v}";; --package-dir) package_dir="$2";;
                --package-url) package_url="$2";; --checksum-url) checksum_url="$2";;
                --install-dir|--deployment-dir) install_dir="$2";; --config-dir) config_dir="$2";;
                --data-dir) data_dir="$2";; --service-name) service_name="$2";; --cli-path) cli_path="$2";;
                --port) port="$2"; port_given=1;; --web-url) web_url="${2%/}"; web_given=1;; --join-url) join_url="$2";;
                --address) address="$2";; --files-dir) files_dir="$2"; files_given=1;; --image) image="$2"; image_given=1;;
            esac
            shift 2;;
        *) fail "unknown option: $1";;
    esac
done
[[ "$(uname -s)" == Linux ]] || fail 'Linux is required'
if [[ "$(id -u)" != 0 ]]; then
    command -v sudo >/dev/null || fail 'run this script as root (sudo is unavailable)'
    exec sudo -- bash "${BASH_SOURCE[0]}" "${original_args[@]}"
fi
prompt() {
    local variable="$1" text="$2" default="$3" value
    printf '%s [%s]: ' "$text" "$default"
    IFS= read -r value || fail 'input ended; use --yes with explicit options for unattended installation'
    printf -v "$variable" '%s' "${value:-$default}"
}
if [[ -z "$lang" ]]; then
    if [[ "$yes" == 1 ]]; then lang=en; else prompt lang 'Language / 语言 (en / zh)' zh; fi
fi
[[ "$lang" == en || "$lang" == zh ]] || fail '--lang must be en or zh'
case "$(uname -m)" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; *) fail 'only amd64 and arm64 are supported';; esac
if [[ "$yes" == 0 ]]; then
    while [[ -z "$role" ]]; do
        prompt choice "$(say 'Role: 1 Master / 2 Agent' '节点类型：1 主节点 / 2 执行节点')" 1
        case "$choice" in 1|master) role=master;; 2|agent) role=agent;; *) say 'Choose 1 or 2.' '请输入 1 或 2。';; esac
    done
    while [[ -z "$method" ]]; do
        prompt choice "$(say 'Deployment: 1 Docker Compose / 2 Native (systemd)' '部署方式：1 Docker Compose / 2 原生部署（systemd）')" 1
        case "$choice" in 1|compose) method=compose;; 2|native) method=native;; *) say 'Choose 1 or 2.' '请输入 1 或 2。';; esac
    done
fi
role="${role:-master}"; method="${method:-compose}"; source="${source:-oss}"
[[ "$method" != docker ]] || method=compose
[[ "$role" == master || "$role" == agent ]] || fail '--role must be master or agent'
[[ "$method" == compose || "$method" == native ]] || fail '--method must be compose or native'
suffix=''; [[ "$role" != agent ]] || suffix='-agent'
install_dir="${install_dir:-/opt/oss-transfer$suffix}"
config_dir="${config_dir:-/etc/oss-transfer$suffix}"
service_name="${service_name:-oss-transfer$suffix}"
if [[ "$method" == compose ]]; then data_dir="${data_dir:-$install_dir/data}"; else data_dir="${data_dir:-/var/lib/oss-transfer$suffix}"; fi
if [[ "$yes" == 0 ]]; then
    [[ "$port_given" == 1 ]] || prompt port "$(say 'Service port' '服务端口')" "$port"
    if [[ "$role" == master ]]; then
        detected="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
        [[ "$web_given" == 1 ]] || prompt web_url "$(say 'Master URL reachable by browsers and agents' '浏览器和执行节点可访问的主节点 URL')" "${web_url:-http://${detected:-localhost}:$port}"
    else
        say 'Paste the invitation link from Nodes > Add node. It will not be printed.' '请粘贴“执行节点 > 添加节点”生成的邀请链接，链接不会回显。'
        if [[ -z "$join_url" ]]; then
            IFS= read -r -s join_url || fail 'invitation link is required'
            printf '\n'
        fi
        detected="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
        [[ -n "$address" ]] || prompt address "$(say 'Agent IPv4 (use the address specified in the invitation)' '执行节点 IPv4（若邀请指定了 IP，请保持一致）')" "$detected"
    fi
    if [[ "$method" == compose ]]; then
        [[ "$files_given" == 1 ]] || prompt files_dir "$(say 'Optional local directory to mount (empty to skip)' '挂载的本地目录（留空跳过）')" "$files_dir"
    fi
fi
version="${version#v}"; web_url="${web_url%/}"
[[ "$source" == oss ]] || fail '--source must be oss; use --package-dir for an offline package'
[[ "$version" == latest || "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'invalid version'
if [[ ! "$port" =~ ^[0-9]{1,5}$ ]] || ! ((10#$port >= 1 && 10#$port <= 65535)); then fail 'invalid port'; fi
port="$((10#$port))"
[[ "$service_name" =~ ^[a-z0-9][a-z0-9_-]*$ && ${#service_name} -le 63 ]] || fail 'invalid service name'
valid_origin() {
    [[ "$1" =~ ^https?://[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:([0-9]{1,5}))?$ ]] || return 1
    local url_port="${BASH_REMATCH[3]:-}"
    [[ -z "$url_port" ]] || ((10#$url_port >= 1 && 10#$url_port <= 65535))
}
valid_path() {
    local path="$1"
    [[ "$path" =~ ^/[A-Za-z0-9_./-]+$ && "$path" != / && "$path" != *//* && ! "$path" =~ (^|/)\.\.?(/|$) ]] || return 1
    while [[ "$path" != / ]]; do [[ ! -L "$path" ]] || return 1; path="$(dirname "$path")"; done
}
valid_address() {
    [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    local octets item
    IFS=. read -r -a octets <<< "$1"
    for item in "${octets[@]}"; do [[ "$item" == 0 || "$item" != 0* ]] && ((10#$item <= 255)) || return 1; done
    ((10#${octets[0]} > 0 && 10#${octets[0]} < 224 && 10#${octets[0]} != 127))
}
for path in "$install_dir" "$config_dir" "$data_dir" "$cli_path"; do valid_path "$path" || fail 'use absolute paths without spaces, symlinks or ..'; done
if [[ -n "$files_dir" ]]; then
    valid_path "$files_dir" && [[ ! -e "$files_dir" || -d "$files_dir" ]] || fail 'local-files mount must be a directory without symlink parents'
    case "$files_dir" in
        /app|/app/*|/opt|/opt/oss-transfer|/opt/oss-transfer/*|/etc|/etc/*|/usr|/usr/*|/bin|/bin/*|/sbin|/sbin/*|/lib|/lib/*|/lib64|/lib64/*|/proc|/proc/*|/sys|/sys/*|/dev|/dev/*|/tmp|/run)
            fail 'local-files mount overlaps container runtime paths; choose a dedicated directory such as /mnt/files';;
    esac
fi
if [[ "$role" == master ]]; then
    valid_origin "$web_url" || fail '--web-url must be a reachable http(s) origin, e.g. http://192.0.2.10:1685'
    [[ -z "$join_url" && -z "$address" ]] || fail 'agent options cannot be used for a master'
else
    [[ "$join_url" =~ ^(https?://[^/]+)/api/agents/bootstrap/([A-Za-z0-9_-]{32,256})$ ]] || fail 'invalid invitation URL; use the link from Nodes > Add node'
    master_url="${BASH_REMATCH[1]}"; join_token="${BASH_REMATCH[2]}"
    valid_origin "$master_url" || fail 'invalid invitation origin'
    valid_address "$address" || fail '--address requires the agent IPv4 (match the invitation if specified)'
fi
[[ "$method" != native || -z "$files_dir" ]] || fail 'native deployment accesses host files directly; --files-dir is only for Compose'
[[ "$method" != compose || ( -z "$package_dir" && -z "$package_url" && -z "$checksum_url" ) ]] || fail 'package options are only for native deployment'
[[ -z "$package_dir" || ( -z "$package_url" && -z "$checksum_url" ) ]] || fail 'choose a package directory or download URL, not both'
[[ ( -z "$package_url" && -z "$checksum_url" ) || ( -n "$package_url" && -n "$checksum_url" ) ]] || fail '--package-url and --checksum-url must be provided together'
image="${image:-moonwx/osstransfer:$version}"
[[ "$image" =~ ^[A-Za-z0-9][A-Za-z0-9._/:-]*(@sha256:[a-fA-F0-9]{64})?$ ]] || fail 'invalid image reference'
validate_package() {
    local required package_version bundle_expected bundle_actual
    local required_files=(oss-transfer.jar "oss-transfer-cli-$arch" VERSION deploy/install.sh deploy/preflight-node.sh deploy/runtime.sh deploy/wait-node-ready.sh)
    [[ "$role" != master ]] || required_files+=(agent-bundle.tar.gz agent-bundle.tar.gz.sha256)
    for required in "${required_files[@]}"; do
        [[ -f "$package_dir/$required" && ! -L "$package_dir/$required" ]] || fail "package missing $required"
    done
    package_version="$(sed -n 's/^Version: *//p' "$package_dir/VERSION" | head -n1)"
    [[ "$package_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][A-Za-z0-9.-]+)?$ ]] || fail 'package VERSION is invalid'
    [[ "$version" == latest || "$version" == "$package_version" ]] || fail 'local/downloaded package version does not match --version'
    if [[ "$role" == master ]]; then
        bundle_expected="$(awk 'NR==1 {print $1}' "$package_dir/agent-bundle.tar.gz.sha256")"
        [[ "$bundle_expected" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'invalid agent bundle checksum'
        if command -v sha256sum >/dev/null; then
            bundle_actual="$(sha256sum "$package_dir/agent-bundle.tar.gz" | awk '{print $1}')"
            [[ "${bundle_actual,,}" == "${bundle_expected,,}" ]] || fail 'agent bundle checksum mismatch'
        fi
    fi
}
if [[ "$method" == native ]]; then
    if [[ -z "$package_dir" && -z "$package_url" && "$role" == master ]]; then
        adjacent="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
        [[ ! -f "$adjacent/oss-transfer.jar" ]] || package_dir="$adjacent"
    fi
    [[ -z "$package_dir" ]] || validate_package
fi
check_empty() {
    local path="$1"
    if [[ -e "$path" ]]; then
        [[ -d "$path" && -z "$(find "$path" -mindepth 1 -maxdepth 1 -print -quit)" ]] || fail "existing files at $path; use the regular installer or Compose to upgrade"
    fi
}
check_empty "$install_dir"
check_empty "$data_dir"
if [[ "$method" == native ]]; then
    check_empty "$config_dir"
    [[ ! -e "/etc/systemd/system/$service_name.service" ]] || fail 'systemd service already exists; use deploy/install.sh to upgrade'
    if command -v systemctl >/dev/null; then
        load_state="$(systemctl show --value --property=LoadState "$service_name.service" 2>/dev/null || true)"
        [[ -z "$load_state" || "$load_state" == not-found ]] || fail 'systemd service already exists; choose another --service-name or use deploy/install.sh'
    fi
    [[ ! -e "$cli_path" ]] || fail "CLI path already exists: $cli_path; choose --cli-path or use deploy/install.sh to upgrade"
    [[ -d /run/systemd/system ]] || fail 'native deployment requires a running systemd host'
fi
java_supported() {
    command -v java >/dev/null || return 1
    local value
    value="$(java -version 2>&1 | sed -n 's/.*version "\([0-9][0-9.]*\).*/\1/p' | head -n1)"
    [[ "$value" != 1.* ]] || value="${value#1.}"
    value="${value%%.*}"
    [[ "$value" =~ ^[0-9]+$ ]] && ((value >= 8))
}
missing=()
for tool in curl tar sha256sum openssl timeout ss; do command -v "$tool" >/dev/null || missing+=("$tool"); done
if [[ "$method" == native ]]; then
    command -v systemctl >/dev/null || fail 'systemctl is required'
    java_supported || missing+=('Java 17')
else
    command -v docker >/dev/null && docker compose version >/dev/null 2>&1 || missing+=('Docker / Compose')
fi
if ((${#missing[@]})); then
    [[ "$no_deps" != 1 ]] || fail "missing dependencies: ${missing[*]}"
    [[ -r /etc/os-release ]] || fail 'cannot identify OS; install dependencies manually'
    # os-release is provided by the local operating system, not a downloaded file.
    # shellcheck source=/dev/null
    . /etc/os-release
    [[ "${ID:-}" == ubuntu || "${ID:-}" == debian ]] || fail "install dependencies manually on this OS: ${missing[*]}"
    [[ "${VERSION_CODENAME:-}" =~ ^[a-z]+$ ]] || fail 'OS codename unavailable; install dependencies manually'
fi
say '' ''
say 'Installation summary' '安装确认'
printf '  %s / %s / %s\n  %s\n  Data: %s\n' "$role" "$method" "$version" "$install_dir" "$data_dir"
if [[ "$role" == master ]]; then printf '  Web: %s\n' "$web_url"; else printf '  Master: %s\n  Agent: %s\n' "$master_url" "$address"; fi
if [[ "$method" == compose ]]; then
    printf '  Image: %s\n' "$image"
    [[ -z "$files_dir" ]] || printf '  Mount: %s -> %s (read/write, container UID 10001)\n' "$files_dir" "$files_dir"
    say '  Docker must run; if needed, its service will be enabled and started.' '  需要运行 Docker；必要时将启用并启动 Docker 服务。'
else
    printf '  Configuration: %s\n  CLI: %s\n' "$config_dir" "$cli_path"
fi
if ((${#missing[@]})); then printf '  Install dependencies: %s\n' "${missing[*]}"; fi
if [[ "$yes" == 0 ]]; then
    prompt confirmed "$(say 'Proceed? y / n' '继续安装？y / n')" n
    [[ "$confirmed" == y || "$confirmed" == Y ]] || { say 'Cancelled. No changes made.' '已取消，未修改系统。'; exit 0; }
fi
[[ "$(id -u)" == 0 ]] || fail 'run with sudo: sudo bash install.sh (no changes made)'
stage="$(mktemp -d /tmp/oss-transfer-setup.XXXXXX)"
trap 'rm -rf -- "$stage"; unset join_token join_url' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
if ((${#missing[@]})); then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update
    packages=(ca-certificates curl tar openssl coreutils iproute2)
    if [[ "$method" == native ]] && ! java_supported; then packages+=(openjdk-17-jre-headless); fi
    apt-get install -y --no-install-recommends "${packages[@]}"
    if [[ "$method" == compose ]] && ! { command -v docker >/dev/null && docker compose version >/dev/null 2>&1; }; then
        # Reuse an existing Docker source; duplicate Signed-By entries break apt.
        if ! grep -Rq "download.docker.com/linux/$ID" /etc/apt/sources.list /etc/apt/sources.list.d 2>/dev/null; then
            for file in /etc/apt/keyrings/oss-transfer-docker.asc /etc/apt/sources.list.d/oss-transfer-docker.sources; do
                [[ ! -e "$file" && ! -L "$file" ]] || fail 'Docker repository file already exists; install Docker Compose manually'
            done
            curl --fail --silent --show-error --proto '=https' --connect-timeout 15 --max-time 60 \
                "https://download.docker.com/linux/$ID/gpg" -o "$stage/docker.asc"
            install -d -m 0755 /etc/apt/keyrings
            install -m 0644 "$stage/docker.asc" /etc/apt/keyrings/oss-transfer-docker.asc
            cat > "$stage/docker.sources" <<REPO
Types: deb
URIs: https://download.docker.com/linux/$ID
Suites: $VERSION_CODENAME
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/oss-transfer-docker.asc
REPO
            install -m 0644 "$stage/docker.sources" /etc/apt/sources.list.d/oss-transfer-docker.sources
        fi
        apt-get update
        if command -v docker >/dev/null; then
            apt-get install -y --no-remove --no-install-recommends docker-compose-plugin
        else
            apt-get install -y --no-remove docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
        fi
    fi
fi
if [[ "$method" == native || "$role" == master ]]; then
    [[ -z "$(ss -H -ltn "sport = :$port")" ]] || fail "port $port is already in use"
fi
download() {
    local url="$1" dest="$2" proto='=https'
    [[ "$url" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?/[^[:space:]\"\\]+$ ]] || fail 'invalid download URL'
    [[ "$url" != http://* ]] || proto='=http,https'
    # Invitation downloads must not redirect credentials to a different origin.
    if [[ "$role" == agent ]]; then
        curl --fail --silent --show-error --proto "$proto" --connect-timeout 15 --max-time 600 "$url" -o "$dest" 2> "$stage/download-error" || fail 'download failed; check the master URL, invitation expiry and network'
    else
        curl --fail --silent --show-error --location --proto "$proto" --proto-redir '=https' --connect-timeout 15 --max-time 600 "$url" -o "$dest" || fail 'package download failed'
    fi
}
if [[ "$method" == native ]]; then
    if [[ -z "$package_dir" ]]; then
        if [[ -z "$package_url" ]]; then
            if [[ "$role" == agent ]]; then
                package_url="$master_url/api/agents/package/$join_token"; checksum_url="$package_url/sha256"
            else
                release="$version"; [[ "$release" == latest ]] || release="v$release"
                base="https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/$release"
                package_url="$base/oss-transfer.tar.gz"; checksum_url="$package_url.sha256"
            fi
        fi
        download "$checksum_url" "$stage/checksum"
        expected="$(awk 'NR==1 {print $1}' "$stage/checksum")"
        [[ "$expected" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'invalid package checksum'
        download "$package_url" "$stage/package.tar.gz"
        actual="$(sha256sum "$stage/package.tar.gz" | awk '{print $1}')"
        [[ "${actual,,}" == "${expected,,}" ]] || fail 'package checksum mismatch; installation aborted'
        tar -tzf "$stage/package.tar.gz" > "$stage/entries"
        if grep -Eq '\\|^/|(^|/)\.\.(/|$)' "$stage/entries"; then fail 'unsafe archive path'; fi
        if tar -tvzf "$stage/package.tar.gz" | awk 'substr($0,1,1)!="-" && substr($0,1,1)!="d" {bad=1} END {exit !bad}'; then fail 'archive contains links or special files'; fi
        mkdir "$stage/package"
        tar -xzf "$stage/package.tar.gz" --no-same-owner --no-same-permissions -C "$stage/package"
        package_dir="$stage/package"; [[ ! -d "$package_dir/oss-transfer" ]] || package_dir="$package_dir/oss-transfer"
    fi
    validate_package
    java_supported || fail 'Java 8+ is required'
    args=(--role "$role" --package-dir "$package_dir" --install-dir "$install_dir" --config-dir "$config_dir" --data-dir "$data_dir" --service-name "$service_name" --cli-path "$cli_path" --port "$port")
    if [[ "$role" == master ]]; then args+=(--web-url "$web_url"); else args+=(--master-url "$master_url" --join-token "$join_token" --address "$address"); fi
    bash "$package_dir/deploy/install.sh" "${args[@]}"
else
    if ! docker info >/dev/null 2>&1; then
        [[ "$no_deps" != 1 ]] || fail 'Docker daemon is not available; start it before using --no-deps'
        systemctl enable --now docker
        docker info >/dev/null 2>&1 || fail 'Docker daemon is unavailable'
    fi
    [[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$service_name")" ]] || fail 'Compose project already exists; use Compose to upgrade'
    if [[ "$role" == agent ]]; then download "$join_url" "$stage/invitation"; fi
    if [[ "$image_given" == 0 ]] || ! timeout 30 docker image inspect "$image" >/dev/null 2>&1; then timeout 600 docker pull "$image"; fi
    if [[ -n "$files_dir" ]]; then
        if [[ ! -d "$files_dir" ]]; then mkdir -p "$files_dir"; chown 10001:10001 "$files_dir"; chmod 0750 "$files_dir"; fi
        # shellcheck disable=SC2016
        timeout 30 docker run --rm --network none --read-only --cap-drop ALL --user 10001:10001 \
            --entrypoint /bin/sh --volume "$files_dir:$files_dir" "$image" \
            -c '[ -r "$1" ] && [ -w "$1" ] && [ -x "$1" ]' sh "$files_dir" || \
            fail "mount is not accessible to container UID 10001: $files_dir; grant that UID read/write access and retry"
    fi
    mkdir -p "$install_dir" "$data_dir"
    chmod 0700 "$install_dir" "$data_dir"
    chown 10001:10001 "$data_dir"
    cat > "$install_dir/docker-compose.yml" <<YAML
services:
  $role:
    image: '$image'
    restart: unless-stopped
    init: true
    stop_grace_period: 90s
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    tmpfs:
      - /tmp:rw,exec,nosuid,nodev,size=256m,mode=1777
    environment:
      TRANSFER_ROLE: '$role'
      OSS_TRANSFER_PORT: '$port'
YAML
    if [[ "$role" == master ]]; then
        cat >> "$install_dir/docker-compose.yml" <<YAML
      TRANSFER_MASTER_WEB_URL: '$web_url'
    ports:
      - '$port:$port'
YAML
    else
        cat >> "$install_dir/docker-compose.yml" <<YAML
      TRANSFER_AGENT_MASTER_URL: '$master_url'
      TRANSFER_AGENT_JOIN_TOKEN: '$join_token'
      TRANSFER_AGENT_ADDRESS: '$address'
YAML
    fi
    printf "    volumes:\n      - '%s:/app/data'\n" "$data_dir" >> "$install_dir/docker-compose.yml"
    [[ -z "$files_dir" ]] || printf "      - '%s:%s'\n" "$files_dir" "$files_dir" >> "$install_dir/docker-compose.yml"
    compose=(docker compose --project-name "$service_name" --file "$install_dir/docker-compose.yml")
    timeout 120 "${compose[@]}" up -d
    container="$("${compose[@]}" ps -q "$role")"
    [[ -n "$container" ]] || fail 'container was not created'
    deadline=$((SECONDS + 150)); ready=0
    while ((SECONDS < deadline)); do
        state="$(timeout 10 docker inspect --format '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' "$container")"
        case "$state" in
            'running healthy') ready=1; break;;
            *unhealthy*|exited*|dead*|restarting*) fail "container startup failed; run docker compose -p $service_name -f $install_dir/docker-compose.yml logs";;
        esac
        sleep 2
    done
    [[ "$ready" == 1 ]] || fail "container readiness timeout; inspect $install_dir/docker-compose.yml"
fi
say '' ''
if [[ "$role" == master ]]; then
    say 'Installation complete.' '安装完成。'
    printf 'Web: %s\nUser: admin\n' "$web_url"
    if [[ "$method" == compose ]]; then password_file="$data_dir/secrets/admin_password"; else password_file="$config_dir/admin-password"; fi
    say 'Read the initial password:' '查看初始密码：'
    printf '  sudo cat %s\n' "$password_file"
else
    say 'Service started.' '服务已启动。'
    say 'Agent service is running. Check its connection on the master Nodes page.' '执行节点服务已启动，请在主节点“执行节点”页面确认连接状态。'
fi
if [[ "$method" == compose ]]; then
    printf '  sudo docker compose -p %s -f %s/docker-compose.yml ps\n' "$service_name" "$install_dir"
    printf '  sudo docker compose -p %s -f %s/docker-compose.yml logs -f\n' "$service_name" "$install_dir"
else
    printf '  sudo systemctl status %s\n  sudo journalctl -u %s -f\n' "$service_name" "$service_name"
fi
