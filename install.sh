#!/usr/bin/env bash
# Public installer: default to native services; Compose is an explicit option.
set +x
set -euo pipefail
umask 077
lang=en role='' method='' version=latest release_url='' package_dir='' yes=0 upgrade=0 user_service=0
install_dir='' data_dir='' cli_path='' service_name='' master_url='' join_url='' files_dir='' image='' port=1685
listen='' adopt_config=''
replace_cli=0 token_stdin=0 no_deps=0
native_args=()
original_args=("$@")
say() { if [[ "$lang" == zh ]]; then printf '%s\n' "$2"; else printf '%s\n' "$1"; fi; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'EOF'
OSS Transfer installer
Usage: bash install.sh [options]
  --lang en|zh                   Prompt language
  --role master|agent            Node role
  --method native|compose        Native service (default) or Docker Compose
  --version latest|VERSION       Release version (default: latest)
  --release-url URL              Fixed release directory or latest
  --package-dir DIR              Local verified native release files
  --port PORT                    Master port (default: 1685)
  --join-url URL                 Agent invitation link
  --master-url URL --token-stdin  Agent enrollment without a link
  --files-dir DIR                Agent Docker bind mount (same path inside)
  --image IMAGE                  Docker image override
  --upgrade                     Upgrade an existing native Go service
  --cli-path PATH --replace-cli  CLI location and explicit backup/replacement
  --install-dir DIR --config-dir DIR --data-dir DIR --backup-dir DIR
  --service-name NAME --adopt-config FILE --listen HOST:PORT
  --user                        macOS user service
  --no-deps                     Do not install missing Docker dependencies
  --yes                         Skip prompts
EOF
}
while (($#)); do
  case "$1" in
    --help|-h) usage; exit 0;;
    --lang|--role|--method|--version|--release-url|--package-dir|--port|--join-url|--master-url|--files-dir|--image|--install-dir|--data-dir|--cli-path|--service-name|--listen|--adopt-config)
      (($# >= 2)) && [[ -n "$2" && "$2" != --* ]] || fail "Missing value: $1"
      key=${1#--}; key=${key//-/_}; printf -v "$key" '%s' "$2"; shift 2;;
    --config-dir|--backup-dir|--startup-timeout)
      (($# >= 2)) && [[ -n "$2" && "$2" != --* ]] || fail "Missing value: $1"
      native_args+=("$1" "$2"); shift 2;;
    --yes) yes=1; shift;;
    --upgrade) upgrade=1; shift;;
    --user) user_service=1; shift;;
    --replace-cli) replace_cli=1; shift;;
    --token-stdin) token_stdin=1; shift;;
    --no-deps) no_deps=1; shift;;
    *) fail "Unknown option: $1";;
  esac
done
[[ "$lang" == en || "$lang" == zh ]] || fail '--lang must be en or zh'
os=$(uname -s)
[[ "$os" == Linux || "$os" == Darwin ]] || fail 'Use Install.ps1 on Windows'
[[ "$os" == Darwin || "$user_service" == 0 ]] || fail '--user is macOS only'
if ((user_service == 0)) && [[ $(id -u) != 0 ]]; then
  command -v sudo >/dev/null || fail 'Run as root or use --user on macOS'
  exec sudo bash "${BASH_SOURCE[0]}" "${original_args[@]}"
fi
prompt() {
  local name=$1 label=$2 fallback=$3 answer
  printf '%s [%s]: ' "$label" "$fallback" >&2
  IFS= read -r answer </dev/tty || fail 'Input ended; use --yes with explicit options'
  printf -v "$name" '%s' "${answer:-$fallback}"
}
# Generated Agent commands and local package installs already chose native mode.
if [[ -z "$method" && ( -n "$release_url" || -n "$package_dir" || "$upgrade" == 1 ) ]]; then method=native; fi
if ((yes == 0)); then
  if [[ -z "$role" ]]; then
    prompt role "$(say 'Role: 1 Master / 2 Agent' '节点类型：1 主节点 / 2 执行节点')" 1
    case "$role" in 1) role=master;; 2) role=agent;; esac
  fi
  if [[ -z "$method" ]]; then
    if [[ "$os" == Darwin ]]; then method=native; else
      prompt method "$(say 'Deployment: 1 Native (recommended) / 2 Docker Compose' '部署方式：1 原生服务（推荐） / 2 Docker Compose')" 1
      case "$method" in 1) method=native;; 2) method=compose;; esac
    fi
  fi
fi
role=${role:-master}; method=${method:-native}
[[ "$role" == master || "$role" == agent ]] || fail 'Invalid role'
[[ "$method" == native || "$method" == compose ]] || fail 'Invalid deployment method'
if ((yes == 0)) && [[ "$role" == master && -z "$listen" && "$upgrade" == 0 && -z "$adopt_config" ]]; then
  prompt port "$(say 'Master port' '主节点端口')" "$port"
fi
if [[ ! "$port" =~ ^[0-9]{1,5}$ ]] || ((10#$port < 1 || 10#$port > 65535)); then fail 'Invalid port'; fi
[[ -z "$package_dir" || -z "$release_url" ]] || fail 'Choose --package-dir or --release-url'
if [[ -z "$release_url" && -z "$package_dir" ]]; then
  version=${version#v}
  [[ "$version" == latest || "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'Invalid version'
  release_part=$version; [[ "$version" == latest ]] || release_part=v$version
  release_url=https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/$release_part
fi
if [[ -n "$release_url" ]]; then
  release_url=${release_url%/}
  [[ "$release_url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._~/-]+$ || "$release_url" =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]+)?/[A-Za-z0-9._~/-]+$ ]] || fail 'Release URL must use HTTPS'
fi
enrollment_input() {
  if [[ "$role" == agent && "$upgrade" == 0 && -z "$adopt_config" && -z "$master_url" && -z "$join_url" && "$yes" == 0 ]]; then
    prompt join_url "$(say 'Agent invitation link from the console' '控制台生成的执行节点邀请链接')" ''
  fi
  if [[ -n "$join_url" ]]; then
    [[ "$role" == agent && "$join_url" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]+)?/api/agents/bootstrap/[A-Za-z0-9_-]+$ ]] || fail 'Use the Agent invitation link from the console'
    master_url=${join_url%/api/agents/bootstrap/*}; TRANSFER_JOIN_TOKEN=${join_url##*/}; export TRANSFER_JOIN_TOKEN
  fi
}
stage=$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-setup.XXXXXX")
trap 'rm -rf -- "$stage"; unset TRANSFER_JOIN_TOKEN OSS_TRANSFER_JOIN_TOKEN join_url' EXIT
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
fetch() {
  local name=$1 protocol=https
  if [[ -n "$package_dir" ]]; then
    [[ -f "$package_dir/$name" && ! -L "$package_dir/$name" ]] || fail "Missing release file: $name"
    cp "$package_dir/$name" "$stage/$name"
  else
    [[ "$release_url" != http://* ]] || protocol=http,https
    curl -fsSL --proto "=$protocol" --proto-redir "=$protocol" --connect-timeout 15 --max-time 600 --retry 2 "$release_url/$name" -o "$stage/$name"
  fi
}
fetch manifest.tsv
[[ $(wc -c < "$stage/manifest.tsv") -lt 65536 && $(awk -F '\t' '$1=="format"{print $2}' "$stage/manifest.tsv") == oss-transfer-go-v1 ]] || fail 'This installer requires a Go release manifest'
version=$(awk -F '\t' '$1=="version"{print $2}' "$stage/manifest.tsv")
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'Invalid release version'
commit=$(awk -F '\t' '$1=="commit"{print $2}' "$stage/manifest.tsv")
if [[ "$release_url" == */latest ]]; then
  manifest_hash=$(sha256 "$stage/manifest.tsv")
  release_url=${release_url%/latest}/v$version
  fetch manifest.tsv
  [[ $(sha256 "$stage/manifest.tsv") == "$manifest_hash" ]] || fail 'Latest changed during download; retry'
fi
if [[ "$method" == native ]]; then
  asset=$(awk -F '\t' '$1=="asset" && $2=="native-install.sh"{print $3 "\t" $4}' "$stage/manifest.tsv")
  IFS=$'\t' read -r checksum size <<< "$asset"
  [[ "$checksum" =~ ^[a-f0-9]{64}$ && "$size" =~ ^[0-9]+$ && "$asset" != *$'\n'* ]] || fail 'Native installer missing from manifest'
  fetch native-install.sh
  [[ $(sha256 "$stage/native-install.sh") == "$checksum" && $(wc -c < "$stage/native-install.sh" | tr -d ' ') == "$size" ]] || fail 'Native installer checksum mismatch'
  # Match the native installer's default directory to recognize a managed
  # installation. The installer validates all metadata and paths again.
  if [[ -z "$install_dir" ]]; then
    if [[ "$os" == Linux ]]; then install_dir=/opt/oss-transfer-go/$role
    elif ((user_service)); then install_dir="$HOME/Library/Application Support/OSS Transfer/$role/bin"
    else install_dir=/usr/local/lib/oss-transfer/$role; fi
  fi
  metadata=$install_dir/install.tsv
  owned_cli=''
  if [[ -f "$metadata" && ! -L "$metadata" ]]; then
    if ((upgrade == 0 && yes == 0)); then
      prompt answer "$(say 'Existing installation: back up and upgrade? y / n' '已有安装，是否备份后升级？y / n')" y
      [[ "$answer" == y || "$answer" == Y ]] || exit 0
      upgrade=1
    fi
    owned_cli=$(awk -F '\t' '$1=="cliPath"{print $2}' "$metadata")
    [[ -n "$cli_path" ]] || cli_path=$owned_cli
  fi
  # Master installs the user-facing CLI globally. An Agent's CLI stays beside
  # its binary, so adding it on the same host does not replace the Master's CLI.
  if [[ -z "$cli_path" ]]; then
    if [[ "$role" == agent ]]; then cli_path=$install_dir/oss-transfer-cli
    elif ((user_service)); then cli_path=$HOME/.local/bin/oss-transfer-cli
    else cli_path=/usr/local/bin/oss-transfer-cli; fi
  fi
  if [[ -e "$cli_path" && "$cli_path" != "$owned_cli" && "$replace_cli" == 0 && "$yes" == 0 ]]; then
    prompt answer "$(say 'Existing CLI: back up and replace? y / n' '已有 CLI，是否备份后替换？y / n')" n
    [[ "$answer" == y || "$answer" == Y ]] || exit 0
    replace_cli=1
  fi
  if ((yes == 0)); then
    printf '%s %s (%s)\n' "$role" "$version" "$method"
    prompt answer "$(say 'Install? y / n' '安装？y / n')" y
    [[ "$answer" == y || "$answer" == Y ]] || exit 0
  fi
  native_args+=(--role "$role" --cli-path "$cli_path" --yes)
  if [[ -n "$package_dir" ]]; then native_args+=(--package-dir "$package_dir"); else native_args+=(--release-url "$release_url"); fi
  for field in install_dir data_dir service_name; do
    value=${!field}
    [[ -z "$value" ]] || native_args+=("--${field//_/-}" "$value")
  done
  ((upgrade == 0)) || native_args+=(--upgrade)
  ((user_service == 0)) || native_args+=(--user)
  ((replace_cli == 0)) || native_args+=(--replace-cli)
  [[ -z "$adopt_config" ]] || native_args+=(--adopt-config "$adopt_config")
  if [[ "$role" == master ]]; then native_args+=(--listen "${listen:-0.0.0.0:$port}"); fi
  enrollment_input
  [[ -z "$master_url" ]] || native_args+=(--master-url "$master_url")
  ((token_stdin == 0)) || native_args+=(--token-stdin)
  bash "$stage/native-install.sh" "${native_args[@]}"
  exit 0
fi

[[ "$os" == Linux && "$user_service" == 0 ]] || fail 'The Compose installer requires Linux and root'
((upgrade == 0)) || fail 'For an existing Compose installation, run docker compose pull and docker compose up -d in its directory'
[[ -z "$package_dir" && ${#native_args[@]} == 0 && -z "$listen" && -z "$adopt_config" && -z "$cli_path" && "$replace_cli" == 0 ]] || fail 'Native configuration/package options cannot be used with Compose'
service_name=${service_name:-oss-transfer-$role}
[[ "$service_name" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || fail 'Invalid Compose project name'
install_dir=${install_dir:-/opt/oss-transfer-compose/$role}
data_dir=${data_dir:-/var/lib/oss-transfer-compose/$role}
for path in "$install_dir" "$data_dir" "$files_dir"; do
  [[ -z "$path" ]] && continue
  [[ "$path" == /* && "$path" != / && "$path" != *[$'\n\r\t\047"\\$']* && "$path" != *'/../'* && "$path" != */.. && "$path" != *'//'* && "$path" != *'/./'* && "$path" != */. && "$path" != */ && "$path" != *:* ]] || fail 'Use clean absolute paths'
  ancestor=$path
  while [[ "$ancestor" != / ]]; do [[ ! -L "$ancestor" ]] || fail 'Symlink in Compose path'; ancestor=$(dirname "$ancestor"); done
done
[[ "$install_dir" != "$data_dir" && "$install_dir" != "$data_dir/"* && "$data_dir" != "$install_dir/"* ]] || fail 'Compose and data directories must be separate'
if [[ -n "$files_dir" ]]; then
  [[ "$role" == agent ]] || fail '--files-dir is for Agents only'
  for path in "$install_dir" "$data_dir"; do
    [[ "$files_dir" != "$path" && "$files_dir" != "$path/"* && "$path" != "$files_dir/"* ]] || fail 'Local-files mount must be separate from service data and configuration'
  done
fi
for path in "$install_dir" "$data_dir"; do [[ ! -e "$path" || -d "$path" && -z $(ls -A "$path") ]] || fail "Existing files in $path; keep the current Compose file when upgrading"; done
enrollment_input
if [[ "$role" == agent ]]; then
  [[ "$master_url" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]+)?/?$ ]] || fail 'Provide --master-url or --join-url for the Agent'
  if ((token_stdin)); then IFS= read -r TRANSFER_JOIN_TOKEN || [[ -n "${TRANSFER_JOIN_TOKEN:-}" ]]; fi
  [[ -n "${TRANSFER_JOIN_TOKEN:-}" ]] || fail 'Provide the Agent invitation with --join-url or --token-stdin'
  [[ -z "$files_dir" || -d "$files_dir" ]] || fail 'Create the local-files directory and grant container UID 65532 access first'
fi
image=${image:-moonwx/osstransfer:$version}
[[ "$image" =~ ^[A-Za-z0-9][A-Za-z0-9._/:@-]+$ ]] || fail 'Invalid Docker image'
if ! { command -v docker >/dev/null && docker compose version >/dev/null 2>&1; }; then
  ((no_deps == 0)) || fail 'Install Docker Engine and the Compose plugin first'
  [[ -r /etc/os-release ]] || fail 'Install Docker Engine and the Compose plugin first'
  # shellcheck source=/dev/null
  . /etc/os-release
  [[ "${ID:-}" == debian || "${ID:-}" == ubuntu ]] || fail 'Automatic Docker installation supports Debian and Ubuntu'
  if ((yes == 0)); then prompt answer "$(say 'Install Docker Engine and Compose? y / n' '安装 Docker Engine 和 Compose？y / n')" y; [[ "$answer" == y || "$answer" == Y ]] || exit 0; fi
  apt-get update
  apt-get install -y --no-install-recommends ca-certificates curl
  if ! grep -Rq "download.docker.com/linux/$ID" /etc/apt/sources.list /etc/apt/sources.list.d 2>/dev/null; then
    [[ ! -e /etc/apt/sources.list.d/oss-transfer-docker.sources && ! -e /etc/apt/keyrings/oss-transfer-docker.asc ]] || fail 'Review the existing Docker package source'
    curl -fsSL --proto '=https' --connect-timeout 15 --max-time 60 "https://download.docker.com/linux/$ID/gpg" -o "$stage/docker.asc"
    install -d -m 755 /etc/apt/keyrings
    install -m 644 "$stage/docker.asc" /etc/apt/keyrings/oss-transfer-docker.asc
    printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/oss-transfer-docker.asc\n' "$ID" "${VERSION_CODENAME:?Missing distribution codename}" "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/oss-transfer-docker.sources
  fi
  apt-get update
  apt-get install -y --no-remove docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
docker info >/dev/null 2>&1 || fail 'Start the Docker daemon first'
[[ -z $(docker ps -aq --filter "label=com.docker.compose.project=$service_name") ]] || fail 'Compose project already exists'
if ((yes == 0)); then printf '%s %s (%s)\n' "$role" "$version" "$image"; prompt answer "$(say 'Install? y / n' '安装？y / n')" y; [[ "$answer" == y || "$answer" == Y ]] || exit 0; fi
# Reuse the exact release already present locally; otherwise fetch it before
# creating any application files or containers.
if [[ $(docker image inspect "$image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' 2>/dev/null || true) != "$commit" ]]; then
  docker pull "$image"
fi
[[ $(docker image inspect "$image" --format '{{index .Config.Labels "org.opencontainers.image.revision"}}') == "$commit" ]] || fail 'Image commit differs from the selected release'
mkdir -p "$install_dir" "$data_dir"
chmod 700 "$install_dir" "$data_dir"
chown 65532:65532 "$data_dir"
compose_file=$install_dir/docker-compose.yml
cat > "$compose_file" <<EOF
services:
  $role:
    image: '$image'
    command: ['oss-transfer-$role', '--data-dir', '/data']
    restart: unless-stopped
    read_only: true
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    tmpfs: ['/tmp:mode=1777']
    stop_grace_period: 45s
    volumes:
      - '$data_dir:/data'
EOF
[[ -z "$files_dir" || "$role" != agent ]] || printf "      - '%s:%s'\n" "$files_dir" "$files_dir" >> "$compose_file"
if [[ "$role" == master ]]; then printf "    ports: ['%s:1685']\n" "$port" >> "$compose_file"; fi
compose=(docker compose --project-name "$service_name" --file "$compose_file")
"${compose[@]}" config --quiet
if [[ "$role" == agent ]]; then
  OSS_TRANSFER_JOIN_TOKEN=$TRANSFER_JOIN_TOKEN; export OSS_TRANSFER_JOIN_TOKEN
  "${compose[@]}" run --rm -e OSS_TRANSFER_JOIN_TOKEN agent oss-transfer-agent --data-dir /data --master-url "$master_url" --enroll-only
  unset OSS_TRANSFER_JOIN_TOKEN TRANSFER_JOIN_TOKEN
fi
"${compose[@]}" up -d
container=$("${compose[@]}" ps -q "$role")
[[ -n "$container" ]] || fail 'Container was not created'
if [[ "$role" == master ]]; then
  deadline=$((SECONDS+120)); ready=0
  while ((SECONDS < deadline)); do
    if [[ $(docker inspect "$container" --format '{{.State.Status}}') == running ]] && curl -fsS --max-time 3 "http://127.0.0.1:$port/healthz" >/dev/null; then ready=1; break; fi
    sleep 1
  done
  ((ready)) || fail "Master did not become ready; inspect logs in $install_dir"
  say 'Open the Master URL and complete setup. Read its setup token from the container log.' '打开主节点地址完成初次设置，设置令牌位于容器日志中。'
else
  sleep 2
  [[ $(docker inspect "$container" --format '{{.State.Status}}') == running ]] || fail 'Agent container did not stay running'
  say 'Check the Agent connection on the Master node page.' '请在主节点的执行节点页面确认连接状态。'
fi
printf 'Compose: %s\n' "$compose_file"
printf 'docker compose -p %s -f "%s" logs\n' "$service_name" "$compose_file"
