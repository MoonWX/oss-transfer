#!/usr/bin/env bash
# Public installer: default to native services; Compose is an explicit option.
set +x
set -euo pipefail
umask 077
# The brace group is parsed in full before it runs; a truncated piped download
# therefore fails with a syntax error instead of running a partial script.
{
# BEGIN embedded download source
# Embedded in the standalone Unix installers by scripts/sync-installer-source.py.
# Location describes the public egress IP, including a configured HTTPS proxy.
transfer_release_url() {
  local url=${1%/} legacy=https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer
  case "$url" in
    "$legacy"|"$legacy"/*) url=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer${url#"$legacy"} ;;
  esac
  printf '%s\n' "$url"
}

transfer_country() {
  local response country
  response=$(curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 2 --max-time 3 --max-filesize 4096 \
    -A OSS-Transfer-Installer https://www.cloudflare.com/cdn-cgi/trace 2>/dev/null) || response=''
  country=$(printf '%s\n' "$response" | tr -d '\r' | awk -F= '$1=="loc" {print $2}')
  if [[ "$country" =~ ^[A-Z]{2}$ && "$country" != XX ]]; then printf '%s\n' "$country"; return; fi
  response=$(curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 2 --max-time 3 --max-filesize 4096 \
    -A OSS-Transfer-Installer https://ipinfo.io/country 2>/dev/null) || response=''
  country=$(printf '%s' "$response" | tr -d '\r\n')
  [[ "$country" =~ ^[A-Z]{2}$ && "$country" != XX ]] || return 1
  printf '%s\n' "$country"
}

transfer_source_probe() {
  local source=$1 version=${2#v} url
  if [[ "$source" == github ]]; then
    url=https://api.github.com/repos/MoonWX/oss-transfer/releases/latest
    [[ "$version" == latest ]] || url=https://api.github.com/repos/MoonWX/oss-transfer/releases/tags/v$version
  else
    url=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/manifest.tsv
    [[ "$version" == latest ]] || url=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/v$version/manifest.tsv
  fi
  curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 2 --max-time 5 \
    -A OSS-Transfer-Installer "$url" -o /dev/null 2>/dev/null
}

transfer_select_source() {
  local requested=$1 version=$2 selected other country
  case "$requested" in
    github|jdoss) printf '%s\n' "$requested"; return;;
    auto) ;;
    *) printf 'Invalid download source: %s\n' "$requested" >&2; return 1;;
  esac
  country=$(transfer_country) || country=''
  selected=github; [[ "$country" != CN ]] || selected=jdoss
  if ! transfer_source_probe "$selected" "$version"; then
    other=jdoss; [[ "$selected" != jdoss ]] || other=github
    transfer_source_probe "$other" "$version" || { printf 'Neither download source is reachable for %s.\n' "$version" >&2; return 1; }
    printf '%s is unavailable; using %s.\n' "$selected" "$other" >&2
    selected=$other
  fi
  printf '%s\n' "$selected"
}

# Shared by the standalone Unix installers. Keep this helper dependency-free.
# GitHub supplies an authenticated SHA-256 digest for each uploaded release asset:
# https://docs.github.com/en/rest/releases/assets#get-a-release-asset

transfer_source_sha256() (
  set -o pipefail
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif [[ -x /sbin/sha256 ]]; then /sbin/sha256 -q "$1"
  else shasum -a 256 "$1" | awk '{print $1}'; fi
)

transfer_source_error() { printf 'Error: %s\n' "$*" >&2; return 1; }

# Parse JSON rather than relying on the API's whitespace or property ordering.
# Only validated ASCII release metadata is printed; release notes are ignored.
transfer_github_metadata() {
  LC_ALL=C awk -v wanted="$2" '
  function error() { bad=1; exit 1 }
  function ws() { while (substr(input,pos,1) ~ /^[ \t\r\n]$/) pos++ }
  function hex4(s, n,i,c) {
    if (length(s)!=4 || s ~ /[^0-9a-fA-F]/) error()
    n=0
    for (i=1;i<=4;i++) { c=index("0123456789abcdef",tolower(substr(s,i,1)))-1; n=n*16+c }
    return n
  }
  function string( result,c,escape,code) {
    if (substr(input,pos++,1)!="\"") error()
    result=""
    while (pos<=length(input)) {
      c=substr(input,pos++,1)
      if (c=="\"") return result
      if (c ~ /^[[:cntrl:]]$/) error()
      if (c!="\\") { result=result c; continue }
      escape=substr(input,pos++,1)
      if (escape=="\"" || escape=="\\" || escape=="/") result=result escape
      else if (escape=="b") result=result "\b"
      else if (escape=="f") result=result "\f"
      else if (escape=="n") result=result "\n"
      else if (escape=="r") result=result "\r"
      else if (escape=="t") result=result "\t"
      else if (escape=="u") {
        code=hex4(substr(input,pos,4))
        # Relevant release fields are ASCII. Preserve other escapes so they
        # cannot become an accepted filename, version or metadata key.
        if (code>0 && code<128) result=result sprintf("%c",code)
        else result=result "\\u" substr(input,pos,4)
        pos+=4
      } else error()
    }
    error()
  }
  function key_path(key) { gsub(/~/,"~0",key); gsub(/\//,"~1",key); return key }
  function scalar(path,type,value, parts,count,index_) {
    if (path=="/tag_name") { tag=value; tagtype=type }
    if (path=="/draft") { draft=value; drafttype=type }
    count=split(path,parts,"/")
    if (count==4 && parts[2]=="assets" && parts[3] ~ /^(0|[1-9][0-9]*)$/) {
      index_=parts[3]
      if (parts[4]=="name") { names[index_]=value; nametypes[index_]=type }
      if (parts[4]=="size") { sizes[index_]=value; sizetypes[index_]=type }
      if (parts[4]=="digest") { hashes[index_]=value; hashtypes[index_]=type }
      if (parts[4]=="browser_download_url") { urls[index_]=value; urltypes[index_]=type }
    }
  }
  function value(path,depth, c,id,key,index_,token,remainder) {
    if (depth>64) error()
    ws(); c=substr(input,pos,1)
    if (c=="{") {
      if (depth==0) rootobject=1
      id=++objects; pos++; ws()
      if (substr(input,pos,1)=="}") { pos++; return }
      while (1) {
        ws(); key=string()
        if ((id SUBSEP key) in seen) error()
        seen[id SUBSEP key]=1
        ws(); if (substr(input,pos++,1)!=":") error()
        value(path "/" key_path(key),depth+1)
        ws(); c=substr(input,pos++,1)
        if (c=="}") return
        if (c!=",") error()
      }
    }
    if (c=="[") {
      if (path=="/assets") assetsarray=1
      pos++; ws(); index_=0
      if (substr(input,pos,1)=="]") { pos++; return }
      while (1) {
        value(path "/" index_++,depth+1)
        ws(); c=substr(input,pos++,1)
        if (c=="]") return
        if (c!=",") error()
      }
    }
    if (c=="\"") { token=string(); scalar(path,"string",token); return }
    remainder=substr(input,pos)
    if (match(remainder,/^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/)) {
      token=substr(remainder,1,RLENGTH); pos+=RLENGTH; scalar(path,"number",token); return
    }
    if (substr(remainder,1,4)=="true" || substr(remainder,1,4)=="null") {
      token=substr(remainder,1,4); pos+=4; scalar(path,token,token); return
    }
    if (substr(remainder,1,5)=="false") { pos+=5; scalar(path,"false","false"); return }
    error()
  }
  { input=input $0 "\n"; if (length(input)>2097152) error() }
  END {
    if (bad) exit 1
    pos=1; value("",0); ws()
    if (bad || pos<=length(input) || !rootobject || !assetsarray) exit 1
    if (tagtype!="string" || tag !~ /^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/ || drafttype!="false") exit 1
    matches=0
    for (i in names) if (names[i]==wanted) { selected=i; matches++ }
    if (matches!=1 || nametypes[selected]!="string" || hashtypes[selected]!="string" || sizetypes[selected]!="number" || urltypes[selected]!="string") exit 1
    hash=hashes[selected]
    if (substr(hash,1,7)!="sha256:" || length(hash)!=71 || substr(hash,8) ~ /[^a-f0-9]/) exit 1
    if (sizes[selected] !~ /^[1-9][0-9]*$/ || length(sizes[selected])>12) exit 1
    expected="https://github.com/MoonWX/oss-transfer/releases/download/" tag "/" wanted
    if (urls[selected]!=expected) exit 1
    printf "%s\t%s\t%s\t%s\n",tag,substr(hash,8),sizes[selected],urls[selected]
  }' "$1"
}

transfer_source_fetch() {
  curl -fsSL --proto '=https' --proto-redir '=https' --connect-timeout 10 --max-time 600 --retry 2 --max-filesize "$3" "$1" -o "$2"
}

transfer_source_member() {
  local archive=$1 member=$2 destination=$3 entry
  entry=$(LC_ALL=C tar -tvzf "$archive" -- "$member") || return 1
  [[ "$entry" == -* && "$entry" != *$'\n'* ]] || return 1
  tar -xOzf "$archive" -- "$member" > "$destination"
}

# Produce a local package directory for the existing native installer. The
# archive digest comes from GitHub; the installer is pinned to the FullCommit
# recorded inside that verified archive, never to a moving branch or latest.
transfer_github_prepare() (
  set -o pipefail
  local requested=${1#v} platform=$2 arch=$3 destination=$4
  local name root api tag digest size url actual version commit native_hash native_size role expected length payload
  case "$platform/$arch" in
    linux/amd64|linux/arm64|linux/armv7|linux/riscv64|linux/loong64|darwin/amd64|darwin/arm64|freebsd/amd64|freebsd/arm64) ;;
    *) transfer_source_error 'Unsupported GitHub release platform'; return 1;;
  esac
  [[ "$requested" == latest || "$requested" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { transfer_source_error 'Invalid GitHub release version'; return 1; }
  [[ -d "$destination" && ! -L "$destination" ]] || { transfer_source_error 'Package directory must be a regular directory'; return 1; }
  root=oss-transfer-$platform-$arch
  name=$root.tar.gz
  transfer_download_work=$(mktemp -d "$destination/.github-download.XXXXXX") || return 1
  trap 'rm -rf -- "$transfer_download_work"' EXIT
  api=https://api.github.com/repos/MoonWX/oss-transfer/releases/latest
  [[ "$requested" == latest ]] || api=https://api.github.com/repos/MoonWX/oss-transfer/releases/tags/v$requested
  transfer_source_fetch "$api" "$transfer_download_work/release.json" 2097152 || { transfer_source_error 'Cannot fetch GitHub release metadata'; return 1; }
  actual=$(transfer_github_metadata "$transfer_download_work/release.json" "$name") || { transfer_source_error 'Invalid GitHub release metadata or missing archive checksum'; return 1; }
  IFS=$'\t' read -r tag digest size url <<< "$actual" || return 1
  [[ "$requested" == latest || "$tag" == v$requested ]] || { transfer_source_error 'GitHub release version mismatch'; return 1; }
  transfer_source_fetch "$url" "$transfer_download_work/$name" "$size" || { transfer_source_error 'Cannot download GitHub release archive'; return 1; }
  [[ $(wc -c < "$transfer_download_work/$name" | tr -d '[:space:]') == "$size" && $(transfer_source_sha256 "$transfer_download_work/$name") == "$digest" ]] || { transfer_source_error 'GitHub archive checksum mismatch'; return 1; }
  transfer_source_member "$transfer_download_work/$name" "$root/VERSION" "$transfer_download_work/VERSION" && transfer_source_member "$transfer_download_work/$name" "$root/SHA256SUMS" "$transfer_download_work/SHA256SUMS" || { transfer_source_error 'Invalid archive metadata members'; return 1; }
  [[ $(wc -c < "$transfer_download_work/VERSION") -lt 4096 && $(wc -c < "$transfer_download_work/SHA256SUMS") -lt 4096 ]] || { transfer_source_error 'Archive metadata too large'; return 1; }
  actual=$(awk -v platform="$platform/$arch" -v version="${tag#v}" '
    { separator=index($0,": "); if (!separator) exit 1; key=substr($0,1,separator-1); value=substr($0,separator+2); if (key in fields) exit 1; fields[key]=value }
    END { if (fields["Version"]!=version || fields["Platform"]!=platform || length(fields["FullCommit"])!=40 || fields["FullCommit"] ~ /[^a-f0-9]/) exit 1; print fields["FullCommit"] }
  ' "$transfer_download_work/VERSION") || { transfer_source_error 'Archive identity does not match the release'; return 1; }
  commit=$actual
  # Exactly three executables and VERSION are checksummed by scripts/build.sh.
  awk '
    NF!=2 { bad=1; exit 1 }
    { hash=$1; name=$2; sub(/^\*/,"",name); if (length(hash)!=64 || hash ~ /[^a-f0-9]/ || name !~ /^(oss-transfer-(master|agent|cli)|VERSION)$/ || name in seen) { bad=1; exit 1 }; seen[name]=1; print name "\t" hash; count++ }
    END { if (bad || count!=4) exit 1 }
  ' "$transfer_download_work/SHA256SUMS" > "$transfer_download_work/checksums.tsv" || { transfer_source_error 'Invalid internal checksum manifest'; return 1; }
  expected=$(awk -F '\t' '$1=="VERSION" {print $2}' "$transfer_download_work/checksums.tsv")
  [[ $(transfer_source_sha256 "$transfer_download_work/VERSION") == "$expected" ]] || { transfer_source_error 'Archive VERSION checksum mismatch'; return 1; }
  printf 'format\toss-transfer-go-v1\nversion\t%s\ncommit\t%s\n' "${tag#v}" "$commit" > "$transfer_download_work/manifest.tsv" || return 1
  for role in master agent cli; do
    payload=oss-transfer-$role
    transfer_source_member "$transfer_download_work/$name" "$root/$payload" "$transfer_download_work/$payload" || { transfer_source_error "Invalid archive executable: $role"; return 1; }
    expected=$(awk -F '\t' -v name="$payload" '$1==name {print $2}' "$transfer_download_work/checksums.tsv")
    [[ $(transfer_source_sha256 "$transfer_download_work/$payload") == "$expected" ]] || { transfer_source_error "Archive executable checksum mismatch: $role"; return 1; }
    length=$(wc -c < "$transfer_download_work/$payload" | tr -d '[:space:]') || return 1
    [[ "$length" != 0 ]] || { transfer_source_error "Empty archive executable: $role"; return 1; }
    printf 'file\t%s\t%s\t%s\t%s-%s-%s\t%s\t%s\n' "$platform" "$arch" "$role" "$payload" "$platform" "$arch" "$expected" "$length" >> "$transfer_download_work/manifest.tsv" || return 1
  done
  printf 'archive\t%s\t%s\t%s\t%s\t%s\n' "$platform" "$arch" "$name" "$digest" "$size" >> "$transfer_download_work/manifest.tsv" || return 1
  # Older releases stored the installer under deploy/go. Keep both lookups
  # pinned to the verified archive commit; never substitute the current branch.
  if ! transfer_source_fetch "https://raw.githubusercontent.com/MoonWX/oss-transfer/$commit/deploy/native/install.sh" "$transfer_download_work/native-install.sh" 1048576; then
    transfer_source_fetch "https://raw.githubusercontent.com/MoonWX/oss-transfer/$commit/deploy/go/install.sh" "$transfer_download_work/native-install.sh" 1048576 || { transfer_source_error 'Cannot fetch the versioned native installer'; return 1; }
  fi
  [[ -s "$transfer_download_work/native-install.sh" ]] || { transfer_source_error 'Empty native installer'; return 1; }
  native_hash=$(transfer_source_sha256 "$transfer_download_work/native-install.sh") || return 1
  native_size=$(wc -c < "$transfer_download_work/native-install.sh" | tr -d '[:space:]') || return 1
  printf 'asset\tnative-install.sh\t%s\t%s\n' "$native_hash" "$native_size" >> "$transfer_download_work/manifest.tsv" || return 1
  for name in "$name" manifest.tsv native-install.sh; do
    [[ ! -e "$destination/$name" && ! -L "$destination/$name" ]] || { transfer_source_error 'Package directory already contains release files'; return 1; }
  done
  mv "$transfer_download_work/$root.tar.gz" "$transfer_download_work/manifest.tsv" "$transfer_download_work/native-install.sh" "$destination/"
)

# END embedded download source
lang='' role='' method='' version=latest source=auto release_url='' package_dir='' yes=0 upgrade=0 user_service=0
install_dir='' data_dir='' cli_path='' service_name='' master_url='' join_url='' files_dir='' image='' port=1685
listen='' adopt_config=''
replace_cli=0 token_stdin=0 no_deps=0
prepared_github=0
native_args=()
original_args=("$@")
say() { if [[ "$lang" == zh ]]; then printf '%s\n' "$2"; else printf '%s\n' "$1"; fi; }
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'EOF'
OSS Transfer installer
Usage: bash install.sh [options]
  --lang en|zh                   Prompt language (default: asked, or system locale)
  --role master|agent            Node role
  --method native|compose        Native service (default) or Docker Compose
  --version latest|VERSION       Release version (default: latest)
  --source auto|github|jdoss      Download source (default: public IP location)
  --release-url URL              Fixed release directory or latest
  --package-dir DIR              Local verified native release files
  --port PORT                    Master port (default: 1685)
  --join-url URL                 Agent invitation link
  --master-url URL --token-stdin  Agent enrollment without a link
  --files-dir DIR                Agent Docker bind mount (same path inside)
  --image IMAGE                  Docker image override
  --upgrade                     Discover, back up and upgrade installed native services
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
    --lang|--role|--method|--version|--source|--release-url|--package-dir|--port|--join-url|--master-url|--files-dir|--image|--install-dir|--data-dir|--cli-path|--service-name|--listen|--adopt-config)
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
[[ -z "$lang" || "$lang" == en || "$lang" == zh ]] || fail '--lang must be en or zh'
[[ "$source" == auto || "$source" == github || "$source" == jdoss ]] || fail '--source must be auto, github or jdoss'
version=${version#v}
[[ "$version" == latest || "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'Invalid version'
os=$(uname -s)
[[ "$os" == Linux || "$os" == Darwin || "$os" == FreeBSD ]] || fail 'Use Install.ps1 on Windows'
[[ "$os" == Darwin || "$user_service" == 0 ]] || fail '--user is macOS only'
if ((user_service == 0)) && [[ $(id -u) != 0 ]]; then
  # A script read from a pipe cannot be started again; ask for sudo in the pipe.
  [[ -f "${BASH_SOURCE[0]:-}" ]] || fail 'Run as root: curl ... | sudo bash (macOS user service: curl ... | bash -s -- --user)'
  command -v sudo >/dev/null || fail 'Run as root or use --user on macOS'
  exec sudo bash "${BASH_SOURCE[0]}" "${original_args[@]}"
fi
prompt() {
  # The reply must not be named "answer": callers read that global after return.
  local name=$1 label=$2 fallback=$3 prompt_reply
  printf '%s [%s]: ' "$label" "$fallback" >&2
  IFS= read -r prompt_reply </dev/tty || fail 'Input ended; use --yes with explicit options'
  printf -v "$name" '%s' "${prompt_reply:-$fallback}"
}
interactive=0
if ((yes == 0)) && { : </dev/tty; } 2>/dev/null; then interactive=1; fi
if [[ -z "$lang" ]]; then
  case "${LC_ALL:-${LC_MESSAGES:-${LANG:-}}}" in zh*) lang=zh;; *) lang=en;; esac
  if ((interactive)); then
    default_lang=1; [[ "$lang" != zh ]] || default_lang=2
    prompt answer 'Language / 语言: 1 English / 2 中文' "$default_lang"
    case "$answer" in 2) lang=zh;; *) lang=en;; esac
  fi
fi
default_install_dir() {
  if [[ "$os" == Linux ]]; then printf '/opt/oss-transfer/%s\n' "$1"
  elif [[ "$os" == FreeBSD ]]; then printf '/usr/local/lib/oss-transfer/%s\n' "$1"
  elif ((user_service)); then printf '%s\n' "$HOME/Library/Application Support/OSS Transfer/$1/bin"
  else printf '/usr/local/lib/oss-transfer/%s\n' "$1"; fi
}
existing_install_dir() {
  local directory
  directory=$(default_install_dir "$1")
  if [[ -f "$directory/install.tsv" && ! -L "$directory/install.tsv" ]]; then
    printf '%s\n' "$directory"
  elif [[ "$os" == Linux && -f "/opt/oss-transfer-go/$1/install.tsv" && ! -L "/opt/oss-transfer-go/$1/install.tsv" ]]; then
    printf '/opt/oss-transfer-go/%s\n' "$1"
  fi
}
# Rerunning the same one-line command offers to upgrade what is already installed.
if ((interactive && upgrade == 0)) && [[ -z "$role$method$package_dir$release_url$adopt_config" ]]; then
  installed=''
  for candidate in master agent; do
    [[ -z $(existing_install_dir "$candidate") ]] || installed+=" $candidate"
  done
  if [[ -n "$installed" ]]; then
    prompt answer "$(say "Installed:$installed. 1 Upgrade / 2 Install another role" "已安装：$installed。1 升级 / 2 安装其他角色")" 1
    [[ "$answer" != 1 ]] || upgrade=1
  fi
fi
# Generated Agent commands and local package installs already chose native mode.
if [[ -z "$method" && ( -n "$release_url" || -n "$package_dir" || "$upgrade" == 1 ) ]]; then method=native; fi
if ((yes == 0)); then
  if [[ -z "$role" && "$upgrade" == 0 ]]; then
    prompt role "$(say 'Role: 1 Master / 2 Agent' '节点类型：1 主节点 / 2 执行节点')" 1
    case "$role" in 1) role=master;; 2) role=agent;; esac
  fi
  if [[ -z "$method" ]]; then
    if [[ "$os" != Linux ]]; then method=native; else
      prompt method "$(say 'Deployment: 1 Native (recommended) / 2 Docker Compose' '部署方式：1 原生服务（推荐） / 2 Docker Compose')" 1
      case "$method" in 1) method=native;; 2) method=compose;; esac
    fi
  fi
fi
if ((upgrade == 0)); then role=${role:-master}; fi
method=${method:-native}
[[ -z "$role" || "$role" == master || "$role" == agent ]] || fail 'Invalid role'
[[ "$method" == native || "$method" == compose ]] || fail 'Invalid deployment method'
if ((yes == 0)) && [[ "$role" == master && -z "$listen" && "$upgrade" == 0 && -z "$adopt_config" ]]; then
  prompt port "$(say 'Master port' '主节点端口')" "$port"
fi
if [[ ! "$port" =~ ^[0-9]{1,5}$ ]] || ((10#$port < 1 || 10#$port > 65535)); then fail 'Invalid port'; fi
[[ -z "$package_dir" || -z "$release_url" ]] || fail 'Choose --package-dir or --release-url'
if [[ -n "$release_url" ]]; then
  release_url=$(transfer_release_url "$release_url")
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
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; elif [[ -x /sbin/sha256 ]]; then /sbin/sha256 -q "$1"; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
if [[ -z "$release_url" && -z "$package_dir" ]]; then
  selected_source=$(transfer_select_source "$source" "$version") || fail 'Cannot select a download source; use --source or --release-url'
  printf '%s: %s\n' "$(say 'Download source' '下载源')" "$selected_source"
  if [[ "$selected_source" == github ]]; then
    case "$os" in Linux) platform=linux;; Darwin) platform=darwin;; FreeBSD) platform=freebsd;; esac
    case "$(uname -m)" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; armv7l|armv7) arch=armv7;; riscv64) arch=riscv64;; loongarch64|loong64) arch=loong64;; *) fail 'Unsupported CPU architecture';; esac
    package_dir=$stage/github
    mkdir "$package_dir"
    transfer_github_prepare "$version" "$platform" "$arch" "$package_dir" || fail 'GitHub release verification failed'
    prepared_github=1
  else
    release_part=$version; [[ "$version" == latest ]] || release_part=v$version
    release_url=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/$release_part
  fi
fi
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
[[ $(wc -c < "$stage/manifest.tsv") -lt 65536 && $(awk -F '\t' '$1=="format"{print $2}' "$stage/manifest.tsv") == oss-transfer-go-v1 ]] || fail 'Invalid release manifest'
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
  if ((upgrade)); then
    native_args+=(--upgrade)
    if [[ -n "$package_dir" ]]; then native_args+=(--package-dir "$package_dir"); else native_args+=(--release-url "$release_url"); fi
    for field in role install_dir data_dir cli_path service_name; do
      value=${!field}
      [[ -z "$value" ]] || native_args+=("--${field//_/-}" "$value")
    done
    ((user_service == 0)) || native_args+=(--user)
    ((yes == 0)) || native_args+=(--yes)
    bash "$stage/native-install.sh" "${native_args[@]}"
    exit 0
  fi
  # Preserve an existing installation, but let the downloaded version choose
  # its own defaults on first install (older releases used different paths).
  [[ -n "$install_dir" ]] || install_dir=$(existing_install_dir "$role")
  metadata=${install_dir:+$install_dir/install.tsv}
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
    if [[ "$role" == master ]]; then
      if ((user_service)); then cli_path=$HOME/.local/bin/oss-transfer-cli
      else cli_path=/usr/local/bin/oss-transfer-cli; fi
    fi
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
  native_args+=(--role "$role" --yes)
  [[ -z "$cli_path" ]] || native_args+=(--cli-path "$cli_path")
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
[[ ( -z "$package_dir" || "$prepared_github" == 1 ) && ${#native_args[@]} == 0 && -z "$listen" && -z "$adopt_config" && -z "$cli_path" && "$replace_cli" == 0 ]] || fail 'Native configuration/package options cannot be used with Compose'
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
exit 0
}
