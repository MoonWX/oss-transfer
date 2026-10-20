#!/usr/bin/env bash
# Install or upgrade the Master or Agent as a native service.
set -euo pipefail
umask 077
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
role=
original_args=("$@")
release_url=
package_dir=
source=auto
requested_version=latest
adopt_config=
cli_path=
replace_cli=0
had_cli=0
cli_changed=0
install_dir=
config_dir=
data_dir=
backup_dir=
service_name=
listen=0.0.0.0:1685
master_url=
token_stdin=0
upgrade=0
yes=0
user_service=0
startup_timeout=120
usage() {
  cat <<'EOF'
Usage: bash install.sh [--role master|agent] [--upgrade] [options]
  --release-url URL    Release directory (default: public latest)
  --source auto|github|jdoss  Download source (default: public IP location)
  --version VERSION   Release version (default: latest)
  --package-dir DIR    Install verified files from a local release directory
  --upgrade            Back up and upgrade an existing installation
  --adopt-config FILE   Install a service for stopped, existing Go data
  --cli-path FILE       CLI location (default: beside the service binary)
  --replace-cli         Back up and replace an existing unmanaged CLI
  --listen HOST:PORT   Master listener (default 0.0.0.0:1685; first install only)
  --master-url URL     Master URL for first Agent enrollment
  --token-stdin        Read the Agent invitation token from standard input
  --service-name NAME  Isolated service name
  --install-dir DIR --config-dir DIR --data-dir DIR --backup-dir DIR
  --user               macOS: install a LaunchAgent for the current logged-in user
  --startup-timeout N  Readiness timeout, 10–600 seconds (default 120)
  --yes                Proceed without interactive confirmation
EOF
}
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
while (($#)); do
  case "$1" in
    --version) (($# >= 2)) || fail 'Missing value: --version'; requested_version=$2; shift 2;;
    --role|--source|--release-url|--package-dir|--adopt-config|--cli-path|--install-dir|--config-dir|--data-dir|--backup-dir|--service-name|--listen|--master-url|--startup-timeout)
      (($# >= 2)) || fail "Missing value: $1"
      key=${1#--}; key=${key//-/_}; printf -v "$key" '%s' "$2"; shift 2 ;;
    --token-stdin) token_stdin=1; shift;;
    --upgrade) upgrade=1; shift;;
    --replace-cli) replace_cli=1; shift;;
    --yes) yes=1; shift;;
    --user) user_service=1; shift;;
    --help|-h) usage; exit 0;;
    *) fail "Unknown option: $1";;
  esac
done
[[ -z "$role" || "$role" == master || "$role" == agent ]] || fail 'Role must be master or agent'
[[ -z "$adopt_config" || "$upgrade" == 0 ]] || fail 'Choose --adopt-config or --upgrade'
if [[ ! "$startup_timeout" =~ ^[0-9]+$ ]] || ((startup_timeout < 10 || startup_timeout > 600)); then fail 'Invalid readiness timeout'; fi
[[ "$source" == auto || "$source" == github || "$source" == jdoss ]] || fail '--source must be auto, github or jdoss'
requested_version=${requested_version#v}
[[ "$requested_version" == latest || "$requested_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'Invalid version'
release_url=$(transfer_release_url "$release_url")
[[ -z "$release_url" || -z "$package_dir" ]] || fail 'Choose --release-url or --package-dir'
os=$(uname -s)
case "$os" in
  Linux) platform=linux; ((user_service == 0)) || fail '--user is macOS only';;
  Darwin) platform=darwin;;
  FreeBSD) platform=freebsd; ((user_service == 0)) || fail '--user is macOS only';;
  *) fail 'Supported platforms: Linux, macOS and FreeBSD';;
esac
case "$(uname -m)" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; armv7l|armv7) arch=armv7;; riscv64) arch=riscv64;; loongarch64|loong64) arch=loong64;; *) fail 'Unsupported CPU architecture';; esac
if ((user_service == 0)) && [[ $(id -u) != 0 ]]; then fail 'Run this installer as root'; fi
safe_path() {
  local value=$1 part=$1 target
  [[ "$value" == /* && "$value" != / && "$value" != *'/../'* && "$value" != *'/./'* && "$value" != */ && "$value" != */. && "$value" != */.. && "$value" != *'//'* ]] || fail "Use a clean absolute path: $value"
  [[ "$value" != *[$'\n\r\t"\\%$`']* ]] || fail 'Unsupported character in installation path'
  while [[ "$part" != / ]]; do
    if [[ -L "$part" ]]; then
      target=$(readlink "$part")
      if [[ "$platform" != darwin || ! "$part" =~ ^/(var|tmp|etc)$ || "$target" != "private$part" && "$target" != "/private$part" ]]; then fail "Symlink in installation path: $part"; fi
    fi
    [[ ! -e "$part" || -d "$part" ]] || fail "Not a directory: $part"
    part=$(dirname "$part")
  done
}
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; elif [[ -x /sbin/sha256 ]]; then /sbin/sha256 -q "$1"; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
# Read existing service definitions without evaluating their contents. This also
# discovers older installations with custom directories or service names.
metadata_field() { awk -F '\t' -v key="$2" '$1==key {print $2}' "$1"; }
if [[ -z "$release_url" && -z "$package_dir" ]]; then
  selected_source=$(transfer_select_source "$source" "$requested_version") || fail 'Cannot select a download source; use --source or --release-url'
  printf 'Download source: %s\n' "$selected_source"
  if [[ "$selected_source" == github ]]; then
    source_stage=$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-download.XXXXXX")
    trap 'rm -rf -- "$source_stage"' EXIT
    transfer_github_prepare "$requested_version" "$platform" "$arch" "$source_stage" || fail 'GitHub release verification failed'
    bash "${BASH_SOURCE[0]}" "${original_args[@]}" --package-dir "$source_stage"
    exit 0
  fi
  release_part=$requested_version; [[ "$requested_version" == latest ]] || release_part=v$requested_version
  release_url=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/$release_part
fi
if ((upgrade)); then
  if [[ -z "$install_dir" ]]; then
    candidates=()
    add_candidate() {
      local directory=$1 existing item_role item_name
      [[ -f "$directory/install.tsv" && ! -L "$directory/install.tsv" ]] || return 0
      item_role=$(metadata_field "$directory/install.tsv" role)
      item_name=$(metadata_field "$directory/install.tsv" serviceName)
      [[ "$item_role" == master || "$item_role" == agent ]] || return 0
      [[ -z "$role" || "$role" == "$item_role" ]] || return 0
      [[ -z "$service_name" || "$service_name" == "$item_name" ]] || return 0
      for existing in "${candidates[@]:-}"; do [[ "$existing" != "$directory" ]] || return 0; done
      candidates+=("$directory")
    }
    for item_role in master agent; do
      if [[ "$platform" == linux ]]; then
        add_candidate "/opt/oss-transfer/$item_role"
        add_candidate "/opt/oss-transfer-go/$item_role"
      elif [[ "$platform" == freebsd ]]; then add_candidate "/usr/local/lib/oss-transfer/$item_role"
      elif ((user_service)); then add_candidate "$HOME/Library/Application Support/OSS Transfer/$item_role/bin"
      else add_candidate "/usr/local/lib/oss-transfer/$item_role"; fi
    done
    if [[ "$platform" == linux ]]; then
      for definition in /etc/systemd/system/*.service; do
        [[ -f "$definition" && ! -L "$definition" ]] || continue
        installed_binary=$(sed -nE 's#^ExecStart="([^"]*/oss-transfer-(master|agent))" .*#\1#p' "$definition")
        [[ -z "$installed_binary" ]] || add_candidate "$(dirname "$installed_binary")"
      done
    elif [[ "$platform" == freebsd ]]; then
      for definition in /usr/local/etc/rc.d/*; do
        [[ -f "$definition" && ! -L "$definition" ]] || continue
        directory=$(sed -n 's/^# oss-transfer-install: //p' "$definition")
        [[ -z "$directory" ]] || add_candidate "$directory"
      done
    else
      if ((user_service)); then definitions="$HOME/Library/LaunchAgents"; else definitions=/Library/LaunchDaemons; fi
      for definition in "$definitions"/*.plist; do
        [[ -f "$definition" && ! -L "$definition" ]] || continue
        installed_binary=$(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$definition" 2>/dev/null || true)
        case "$installed_binary" in */oss-transfer-master|*/oss-transfer-agent) add_candidate "$(dirname "$installed_binary")";; esac
      done
    fi
    ((${#candidates[@]})) || fail 'No managed installation found; use --install-dir for an installation without a service definition'
    printf 'Upgrade installations:\n'
    for directory in "${candidates[@]}"; do printf '  %s: %s\n' "$(metadata_field "$directory/install.tsv" serviceName)" "$directory"; done
    if ((yes == 0)); then
      read -r -p 'Back up and upgrade these installations? [y/N] ' answer </dev/tty
      [[ "$answer" == y || "$answer" == Y ]] || exit 0
    fi
    # Resolve latest once so every installed role receives the same release.
    if [[ "$release_url" == */latest ]]; then
      [[ "$release_url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._~/-]+$ || "$release_url" =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]+)?/[A-Za-z0-9._~/-]+$ ]] || fail 'Release URL must use HTTPS'
      pin_work=$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-version.XXXXXX")
      trap 'rm -rf -- "$pin_work"' EXIT
      protocol=https; [[ "$release_url" != http://* ]] || protocol=http,https
      curl -fsSL --proto "=$protocol" --proto-redir "=$protocol" --connect-timeout 10 --max-time 600 --retry 2 "$release_url/manifest.tsv" -o "$pin_work/latest.tsv"
      [[ $(wc -c < "$pin_work/latest.tsv") -lt 65536 && $(metadata_field "$pin_work/latest.tsv" format) == oss-transfer-go-v1 ]] || fail 'Invalid release manifest'
      pinned_version=$(metadata_field "$pin_work/latest.tsv" version)
      [[ "$pinned_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'Invalid latest release version'
      release_url="${release_url%/latest}/v$pinned_version"
      curl -fsSL --proto "=$protocol" --proto-redir "=$protocol" --connect-timeout 10 --max-time 600 --retry 2 "$release_url/manifest.tsv" -o "$pin_work/versioned.tsv"
      [[ $(sha256 "$pin_work/latest.tsv") == "$(sha256 "$pin_work/versioned.tsv")" ]] || fail 'Latest and versioned release manifests differ; retry after publication finishes'
    fi
    for directory in "${candidates[@]}"; do
      if [[ -n "$release_url" ]]; then
        bash "${BASH_SOURCE[0]}" "${original_args[@]}" --install-dir "$directory" --release-url "$release_url" --yes
      else
        bash "${BASH_SOURCE[0]}" "${original_args[@]}" --install-dir "$directory" --yes
      fi
    done
    exit 0
  fi
  installed_metadata=$install_dir/install.tsv
  [[ -f "$installed_metadata" && ! -L "$installed_metadata" ]] || fail 'No managed installation found in the selected directory'
  saved_role=$(metadata_field "$installed_metadata" role)
  [[ "$saved_role" == master || "$saved_role" == agent ]] || fail 'Invalid installed role'
  [[ -z "$role" || "$role" == "$saved_role" ]] || fail 'Installed role differs from --role'
  role=$saved_role
  service_name=${service_name:-$(metadata_field "$installed_metadata" serviceName)}
  [[ -n "$service_name" ]] || fail 'Missing installed service name'
  saved_config=$(metadata_field "$installed_metadata" config)
  [[ "$saved_config" == /*/service.json ]] || fail 'Invalid installed configuration path'
  config_dir=${config_dir:-$(dirname "$saved_config")}
  cli_path=${cli_path:-$(metadata_field "$installed_metadata" cliPath)}
  backup_dir=${backup_dir:-$(metadata_field "$installed_metadata" backupDir)}
  # Old metadata omitted the data path; use the binary's validated config output.
  safe_path "$install_dir"
  safe_path "$(dirname "$saved_config")"
  installed_binary=$install_dir/oss-transfer-$role
  [[ -f "$installed_binary" && ! -L "$installed_binary" && -f "$saved_config" && ! -L "$saved_config" ]] || fail 'Existing installation files are missing or symlinked'
  saved_data=$("$installed_binary" --config "$saved_config" --check-config | awk -F '\t' '$1=="dataDirectory"{print $2}')
  [[ -n "$saved_data" ]] || fail 'Installed data directory is missing'
  data_dir=${data_dir:-$saved_data}
fi
role=${role:-master}
if [[ "$platform" == linux && -z "$install_dir" && "$upgrade" == 0 && -f "/opt/oss-transfer-go/$role/install.tsv" ]]; then
  fail 'Installation exists; rerun with --upgrade'
fi
if [[ "$platform" == freebsd ]]; then
  service_name=${service_name:-oss_transfer_$role}
  [[ "$service_name" =~ ^[A-Za-z][A-Za-z0-9_]{0,99}$ ]] || fail 'FreeBSD service names must start with a letter and contain only letters, digits and underscores'
else
  service_name=${service_name:-oss-transfer-$role}
fi
[[ "$service_name" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$ ]] || fail 'Invalid service name'
if [[ "$platform" == linux ]]; then
  command -v systemctl >/dev/null || fail 'systemd is required'
  install_dir=${install_dir:-/opt/oss-transfer/$role}
  config_dir=${config_dir:-/etc/oss-transfer/$role}
  data_dir=${data_dir:-/var/lib/oss-transfer/$role}
  backup_dir=${backup_dir:-/var/backups/oss-transfer/$role}
  unit=/etc/systemd/system/$service_name.service
elif [[ "$platform" == freebsd ]]; then
  [[ -f /etc/rc.subr && -x /usr/sbin/daemon ]] || fail 'FreeBSD rc.d and daemon are required'
  install_dir=${install_dir:-/usr/local/lib/oss-transfer/$role}
  config_dir=${config_dir:-/usr/local/etc/oss-transfer/$role}
  data_dir=${data_dir:-/var/db/oss-transfer/$role}
  backup_dir=${backup_dir:-/var/backups/oss-transfer/$role}
  unit=/usr/local/etc/rc.d/$service_name
  rc_name=$service_name
  supervisor_pidfile=/var/run/$service_name.supervisor.pid
  child_pidfile=/var/run/$service_name.pid
else
  command -v launchctl >/dev/null || fail 'launchd is required'
  if ((user_service)); then
    base="$HOME/Library/Application Support/OSS Transfer/$role"
    install_dir=${install_dir:-$base/bin}
    unit="$HOME/Library/LaunchAgents/$service_name.plist"
    domain=gui/$(id -u)
  else
    base="/Library/Application Support/OSS Transfer/$role"
    install_dir=${install_dir:-/usr/local/lib/oss-transfer/$role}
    unit=/Library/LaunchDaemons/$service_name.plist
    domain=system
  fi
  config_dir=${config_dir:-$base/config}
  data_dir=${data_dir:-$base/data}
  backup_dir=${backup_dir:-$base/backups}
fi

for path in "$install_dir" "$config_dir" "$data_dir" "$backup_dir"; do safe_path "$path"; done
for left in "$install_dir" "$config_dir" "$data_dir" "$backup_dir"; do
  for right in "$install_dir" "$config_dir" "$data_dir" "$backup_dir"; do
    [[ "$left" == "$right" ]] && continue
    [[ "$left" != "$right/"* ]] || fail 'Binary, config, data and backup directories must not contain one another'
  done
done
[[ "$install_dir" != "$config_dir" && "$install_dir" != "$data_dir" && "$install_dir" != "$backup_dir" && "$config_dir" != "$data_dir" && "$config_dir" != "$backup_dir" && "$data_dir" != "$backup_dir" ]] || fail 'Installation directories must be separate'
config=$config_dir/service.json
binary=$install_dir/oss-transfer-$role
metadata=$install_dir/install.tsv
cli_path=${cli_path:-$install_dir/oss-transfer-cli}
safe_path "$(dirname "$cli_path")"
[[ "$cli_path" != *[$'\n\r\t"\\%$`']* && "$(basename "$cli_path")" == oss-transfer-cli ]] || fail 'CLI path must end in oss-transfer-cli'
for path in "$config_dir" "$data_dir" "$backup_dir"; do
  [[ "$cli_path" != "$path" && "$cli_path" != "$path/"* ]] || fail 'CLI must be outside configuration, data and backup directories'
done
[[ ! -L "$cli_path" && ( ! -e "$cli_path" || -f "$cli_path" ) ]] || fail 'CLI path must be a regular file'
existed=0
if [[ -f "$metadata" && ! -L "$metadata" ]]; then
  existed=1
  ((upgrade)) || fail 'Installation exists; rerun with --upgrade'
  [[ $(awk -F '\t' '$1=="role"{print $2}' "$metadata") == "$role" && $(awk -F '\t' '$1=="serviceName"{print $2}' "$metadata") == "$service_name" && $(awk -F '\t' '$1=="config"{print $2}' "$metadata") == "$config" && $(awk -F '\t' '$1=="unit"{print $2}' "$metadata") == "$unit" ]] || fail 'Existing installation identity does not match'
  [[ -f "$binary" && ! -L "$binary" && -f "$config" && ! -L "$config" ]] || fail 'Existing installation files are missing or symlinked'
else
  ((upgrade == 0)) || fail 'No managed installation found; V4.0.x requires database migration first'
  for path in "$install_dir" "$config_dir" "$data_dir"; do
    [[ -n "$adopt_config" && "$path" == "$data_dir" ]] && continue
    [[ ! -e "$path" || -z $(ls -A "$path") ]] || fail "Refusing nonempty directory: $path"
  done
  [[ ! -e "$unit" && ! -L "$unit" ]] || fail "Service already exists: $service_name"
fi
if [[ -f "$cli_path" ]]; then
  had_cli=1
  if ((existed == 0)) || [[ $(awk -F '\t' '$1=="cliPath"{print $2}' "$metadata") != "$cli_path" ]]; then
    ((replace_cli)) || fail "CLI already exists: $cli_path; use --replace-cli to back it up and replace it, or choose --cli-path"
  fi
fi
if ((existed)); then
  previous_cli=$(awk -F '\t' '$1=="cliPath"{print $2}' "$metadata")
  [[ -z "$previous_cli" || "$previous_cli" == "$cli_path" ]] || fail 'CLI path differs from the installed path; keep --cli-path unchanged during upgrade'
fi
[[ ! -L "$unit" ]] || fail 'Service definition is a symlink'
if [[ "$platform" == linux ]]; then
  if ((existed == 0)) && [[ $(systemctl show "$service_name" -p LoadState --value) != not-found ]]; then fail 'An unmanaged service already exists'; fi
  if ((existed)) && [[ -n $(systemctl show "$service_name" -p DropInPaths --value) ]]; then fail 'Review and remove service overrides before upgrading'; fi
elif [[ "$platform" == darwin ]]; then
  if ((existed == 0)) && launchctl print "$domain/$service_name" >/dev/null 2>&1; then fail 'An unmanaged launchd service already exists'; fi
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-install.XXXXXX")
backup=
was_active=0
was_enabled=0
had_unit=0
[[ ! -f "$unit" ]] || had_unit=1
cleanup() { rm -rf -- "$work"; }
trap cleanup EXIT
fetch() {
  local name=$1 protocol=https
  if [[ "$release_url" == http://* ]]; then protocol=http,https; fi
  if [[ -n "$package_dir" ]]; then
    [[ -f "$package_dir/$name" && ! -L "$package_dir/$name" ]] || fail "Release file missing: $name"
    cp "$package_dir/$name" "$work/$name"
  else
    curl --fail --silent --show-error --location --proto "=$protocol" --proto-redir "=$protocol" --connect-timeout 10 --max-time 600 --retry 2 "$release_url/$name" -o "$work/$name"
  fi
}
if [[ -n "$release_url" ]]; then
  release_url=${release_url%/}
  [[ "$release_url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._~/-]+$ || "$release_url" =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]+)?/[A-Za-z0-9._~/-]+$ ]] || fail 'Release URL must be an HTTPS version directory (loopback HTTP is allowed for testing)'
fi
fetch manifest.tsv
[[ $(wc -c < "$work/manifest.tsv") -lt 65536 ]] || fail 'Manifest too large'
[[ $(awk -F '\t' '$1=="format"{print $2}' "$work/manifest.tsv") == oss-transfer-go-v1 ]] || fail 'Unknown release format'
version=$(awk -F '\t' '$1=="version"{print $2}' "$work/manifest.tsv")
[[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$ ]] || fail 'Invalid release version'
if [[ "$release_url" == */latest ]]; then
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail 'Invalid latest release version'
  latest_manifest_sha=$(sha256 "$work/manifest.tsv")
  release_url="${release_url%/latest}/v$version"
  fetch manifest.tsv
  [[ $(sha256 "$work/manifest.tsv") == "$latest_manifest_sha" ]] || fail 'Latest and versioned release manifests differ; retry after publication finishes'
fi
filename=oss-transfer-$role-$platform-$arch
record=$(awk -F '\t' -v os="$platform" -v arch="$arch" -v role="$role" '$1=="file" && $2==os && $3==arch && $4==role {print $5 "\t" $6 "\t" $7}' "$work/manifest.tsv")
IFS=$'\t' read -r listed checksum length <<< "$record"
[[ "$listed" == "$filename" && "$checksum" =~ ^[a-f0-9]{64}$ && "$length" =~ ^[0-9]+$ && "$record" != *$'\n'* ]] || fail 'Missing or invalid platform binary'
cli_filename=oss-transfer-cli-$platform-$arch
cli_record=$(awk -F '\t' -v os="$platform" -v arch="$arch" '$1=="file" && $2==os && $3==arch && $4=="cli" {print $5 "\t" $6 "\t" $7}' "$work/manifest.tsv")
IFS=$'\t' read -r cli_listed cli_checksum cli_length <<< "$cli_record"
[[ "$cli_listed" == "$cli_filename" && "$cli_checksum" =~ ^[a-f0-9]{64}$ && "$cli_length" =~ ^[0-9]+$ && "$cli_record" != *$'\n'* ]] || fail 'Missing or invalid CLI manifest entry'
if awk -F '\t' '$1=="archive" {found=1} END {exit !found}' "$work/manifest.tsv"; then
  archive_root=oss-transfer-$platform-$arch
  archive_name=$archive_root.tar.gz
  archive_record=$(awk -F '\t' -v os="$platform" -v arch="$arch" '$1=="archive" && $2==os && $3==arch {if (NF!=6) print "invalid"; else print $4 "\t" $5 "\t" $6}' "$work/manifest.tsv")
  IFS=$'\t' read -r archive_listed archive_checksum archive_length <<< "$archive_record"
  [[ "$archive_listed" == "$archive_name" && "$archive_checksum" =~ ^[a-f0-9]{64}$ && "$archive_length" =~ ^[0-9]+$ && "$archive_record" != *$'\n'* ]] || fail 'Missing or invalid platform archive'
  command -v tar >/dev/null || fail 'tar is required to read release archives'
  fetch "$archive_name"
  [[ $(wc -c < "$work/$archive_name" | tr -d ' ') == "$archive_length" && $(sha256 "$work/$archive_name") == "$archive_checksum" ]] || fail 'Archive checksum mismatch'
  extract_binary() {
    local member="$archive_root/oss-transfer-$1" destination=$2 entry
    entry=$(LC_ALL=C tar -tvzf "$work/$archive_name" -- "$member") || fail "Archive member missing: $member"
    [[ "$entry" == -* && "$entry" != *$'\n'* ]] || fail "Archive member must be one regular file: $member"
    # Write only this literal member to our chosen temporary file. Never let
    # archive paths, links or other entries create files on the target host.
    tar -xOzf "$work/$archive_name" -- "$member" > "$work/$destination" || fail "Cannot read archive member: $member"
  }
  extract_binary "$role" "$filename"
  extract_binary cli "$cli_filename"
else
  # Releases before archive-based installation expose individual binaries.
  fetch "$filename"
  fetch "$cli_filename"
fi
[[ $(wc -c < "$work/$filename" | tr -d ' ') == "$length" && $(sha256 "$work/$filename") == "$checksum" ]] || fail 'Binary checksum mismatch'
[[ $(wc -c < "$work/$cli_filename" | tr -d ' ') == "$cli_length" && $(sha256 "$work/$cli_filename") == "$cli_checksum" ]] || fail 'CLI checksum mismatch'
chmod 700 "$work/$filename"
chmod 700 "$work/$cli_filename"
[[ $("$work/$filename" --version) == "oss-transfer-$role $version "* ]] || fail 'Binary version or platform check failed'
[[ $("$work/$cli_filename" --version --json | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') == "$version" ]] || fail 'CLI version or platform check failed'
if ((existed)) || [[ -n "$adopt_config" ]]; then
  source_config=${adopt_config:-$config}
  [[ -f "$source_config" && ! -L "$source_config" ]] || fail 'Existing configuration must be a regular file'
  "$work/$filename" --config "$source_config" --check-config > "$work/config.tsv"
  [[ $(awk -F '\t' '$1=="serviceName"{print $2}' "$work/config.tsv") == "$service_name" ]] || fail 'Configured service name differs from installed service'
  actual_data=$(awk -F '\t' '$1=="dataDirectory"{print $2}' "$work/config.tsv")
  [[ "$actual_data" == "$data_dir" ]] || fail "Configured data directory differs; rerun with --data-dir '$actual_data'"
  cp -p "$source_config" "$work/service.json"
else
  if [[ "$role" == agent ]]; then
    [[ -n "$master_url" ]] || fail 'First Agent installation requires --master-url'
    if ((token_stdin)); then IFS= read -r TRANSFER_JOIN_TOKEN || [[ -n "${TRANSFER_JOIN_TOKEN:-}" ]]; fi
    [[ -n "${TRANSFER_JOIN_TOKEN:-}" ]] || fail 'Provide the invitation with --token-stdin or TRANSFER_JOIN_TOKEN'
    export TRANSFER_JOIN_TOKEN
  fi
  if [[ "$role" == master ]]; then listen_json=",\"listen\":\"$listen\""; else listen_json=; fi
  [[ "$listen" != *[$'\n\r\t"\\']* ]] || fail 'Invalid listener'
  printf '{"serviceName":"%s","dataDirectory":"%s","readyFile":"%s/runtime/ready","logFile":"%s/logs/service.log"%s}\n' "$service_name" "$data_dir" "$data_dir" "$data_dir" "$listen_json" > "$work/service.json"
  "$work/$filename" --config "$work/service.json" --check-config > "$work/config.tsv"
fi
ready_file=$(awk -F '\t' '$1=="readyFile"{print $2}' "$work/config.tsv")
log_file=$(awk -F '\t' '$1=="logFile"{print $2}' "$work/config.tsv")
for path in "$ready_file" "$ready_file.request" "$log_file"; do
  safe_path "$(dirname "$path")"
  [[ ! -L "$path" && ( ! -e "$path" || -f "$path" ) ]] || fail "Service file is a link or non-file: $path"
done
database=$data_dir/$role.db
[[ ! -L "$database" && ( ! -e "$database" || -f "$database" ) ]] || fail 'Database must be a regular file'
[[ ! -e "$data_dir/CURRENT" && ! -e "$data_dir/rocksdb/CURRENT" ]] || fail 'RocksDB requires an explicit migration; Java data will not be replaced'
if ((existed)) || [[ -n "$adopt_config" ]]; then [[ -s "$database" ]] || fail 'Existing Go database missing; refusing to initialize an empty replacement'; fi
# Service-managed data directories do not support an individually mounted DB.
if [[ "$platform" == linux ]] && command -v mountpoint >/dev/null && mountpoint -q "$database" 2>/dev/null; then fail 'Database file is a mount point'; fi
if ((yes == 0)); then
  printf '%s %s %s (%s/%s)\n' "$([[ $existed == 1 ]] && printf Upgrade || printf Install)" "$role" "$version" "$platform" "$arch"
  read -r -p 'Continue? [y/N] ' answer </dev/tty
  [[ "$answer" == y || "$answer" == Y ]] || exit 0
fi
active() { if [[ "$platform" == linux ]]; then systemctl is-active --quiet "$service_name"; elif [[ "$platform" == freebsd ]]; then "$unit" onestatus >/dev/null 2>&1; else launchctl print "$domain/$service_name" >/dev/null 2>&1; fi; }
stop_service() {
  local old_pid load_state deadline=$((SECONDS+50))
  old_pid=$(service_pid || true)
  if [[ "$platform" == linux ]]; then
    load_state=$(systemctl show "$service_name" -p LoadState --value) || return 1
    # Failed first-time adoption removes the unit but preserves installation
    # metadata for a retry. An absent unit has nothing to stop; state backup
    # below still requires the database's exclusive lock.
    if [[ "$load_state" != not-found ]]; then
      systemctl stop "$service_name" || return 1
    elif [[ "$old_pid" != 0 && -n "$old_pid" ]]; then
      return 1
    fi
  elif [[ "$platform" == freebsd ]]; then
    if [[ -f "$unit" ]] && active; then "$unit" onestop || return 1; fi
  else
    launchctl bootout "$domain/$service_name" 2>/dev/null || ! launchctl print "$domain/$service_name" >/dev/null 2>&1 || return 1
  fi
  if [[ "$old_pid" =~ ^[1-9][0-9]*$ ]]; then
    while kill -0 "$old_pid" 2>/dev/null; do
      ((SECONDS < deadline)) || return 1
      sleep 1
    done
  fi
}
start_service() {
  if [[ "$platform" == linux ]]; then systemctl daemon-reload; systemctl start "$service_name"; elif [[ "$platform" == freebsd ]]; then "$unit" onestart; else launchctl bootstrap "$domain" "$unit"; fi
}
service_pid() {
  if [[ "$platform" == linux ]]; then systemctl show "$service_name" -p MainPID --value; elif [[ "$platform" == freebsd ]]; then if [[ -f "$child_pidfile" && ! -L "$child_pidfile" ]]; then cat "$child_pidfile"; fi; else launchctl print "$domain/$service_name" 2>/dev/null | awk '$1=="pid" && $2=="=" {print $3;exit}'; fi
}
nonce() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }
# Keep this check before any installation files, backup or service are changed.
# Socket owners come from the OS, never from an old readiness marker.
listener_address() {
  # Normalize compressed IPv6 and IPv4-mapped addresses without a DNS/runtime
  # dependency. Ordinary IPv4 uses the same representation as ::ffff:a.b.c.d.
  printf '%s\n' "$1" | awk '
    { address=tolower($0); zone=""; if (index(address,"%")) { split(address,z,"%"); address=z[1]; zone="%" z[2] }
      if (index(address,".")) {
        n=split(address,segments,":"); split(segments[n],v4,".")
        tail=sprintf("%x:%x",v4[1]*256+v4[2],v4[3]*256+v4[4])
        if (n==1) address="::ffff:" tail
        else { sub(/[^:]+$/,tail,address) }
      }
      n=split(address,halves,"::"); left=split(halves[1],a,":"); if (halves[1]=="") left=0
      right=0; if (n==2 && halves[2]!="") right=split(halves[2],b,":")
      result=""
      for (i=1;i<=left;i++) { sub(/^0+/,"",a[i]); result=result (result!=""?":":"") (a[i]==""?"0":a[i]) }
      if (n==2) for (i=0;i<8-left-right;i++) result=result (result!=""?":":"") "0"
      for (i=1;i<=right;i++) { sub(/^0+/,"",b[i]); result=result (result!=""?":":"") (b[i]==""?"0":b[i]) }
      print result zone
    }'
}
check_listener() {
  [[ "$role" == master ]] || return 0
  local address port host rows pid endpoint owner old_pid=0 raw errors host_addresses item family v6only normalized overlap
  address=$(awk -F '\t' '$1=="listen"{print $2}' "$work/config.tsv")
  port=${address##*:}; host=${address%:*}; host=${host#[}; host=${host%]}
  host_addresses=$host
  case "$host" in
    ''|0.0.0.0|::|127.*|*:*|[0-9]*.[0-9]*.[0-9]*.[0-9]*) ;;
    *)
      if command -v getent >/dev/null 2>&1; then
        host_addresses=$(getent ahosts "$host" 2>/dev/null | awk '{print $1}' | sort -u) || true
        if [[ -z "$host_addresses" ]]; then host_addresses=$(getent hosts "$host" 2>/dev/null | awk '{print $1}' | sort -u) || true; fi
      elif [[ "$platform" == darwin ]]; then
        host_addresses=$(dscacheutil -q host -a name "$host" | awk '/ip_address:|ipv6_address:/ {print $2}') || true
      fi
      [[ -n "$host_addresses" && "$host_addresses" != "$host" ]] || fail "Cannot resolve listener $address for the port check; use a numeric listen address"
      ;;
  esac
  raw=$work/listeners.raw; rows=$work/listeners.tsv; errors=$work/listeners.error
  if [[ "$platform" == linux ]] && command -v ss >/dev/null 2>&1; then
    ss -H -ltnpe "sport = :$port" > "$raw" 2> "$errors" || fail "Cannot inspect listening port $port with ss"
    awk '{ address=$4; family="unknown"; v6only="unknown"; if (address ~ /^\[/ || /v6only:/) family=6; else if (address ~ /^[0-9]+\./) family=4; if (/v6only:1/) v6only=1; else if (/v6only:0/) v6only=0; owners=0; while (match($0,/pid=[0-9]+/)) { pid=substr($0,RSTART+4,RLENGTH-4); print address "\t" pid "\t" family "\t" v6only; $0=substr($0,RSTART+RLENGTH); owners++ } if (!owners) print address "\tunknown\t" family "\t" v6only }' "$raw" > "$rows"
  elif [[ "$platform" == freebsd ]]; then
    sockstat -l -P tcp -p "$port" > "$raw" 2> "$errors" || fail "Cannot inspect listening port $port with sockstat"
    awk 'NR>1 {family="unknown"; v6only="unknown"; if ($5=="tcp4") family=4; else if ($5=="tcp6") {family=6; v6only=1} else if ($5=="tcp46") {family=6; v6only=0}; print $6 "\t" $3 "\t" family "\t" v6only}' "$raw" > "$rows"
  elif command -v lsof >/dev/null 2>&1; then
    if ! lsof -nP -a -iTCP:"$port" -sTCP:LISTEN -Fptn > "$raw" 2> "$errors"; then
      [[ ! -s "$errors" && ! -s "$raw" ]] || fail "Cannot inspect listening port $port with lsof"
    fi
    awk '/^p/ {pid=substr($0,2); family="unknown"} /^tIPv4/ {family=4} /^tIPv6/ {family=6} /^n/ {print substr($0,2) "\t" (pid ? pid : "unknown") "\t" family "\tunknown"}' "$raw" > "$rows"
  else
    fail 'Port inspection requires ss (iproute2) or lsof; install either before retrying'
  fi
  if ((existed)); then old_pid=$(service_pid || true); fi
  while IFS=$'\t' read -r endpoint pid family v6only; do
    [[ -n "$endpoint" ]] || continue
    endpoint=${endpoint%:*}; endpoint=${endpoint#[}; endpoint=${endpoint%]}
    # Go's tcp wildcard listeners use both address families when supported.
    # Specific IPv6 and IPv4 sockets can coexist; a v6-only wildcard must not
    # block a specific IPv4 listener. Unknown cross-family overlap is left to
    # the real bind and startup diagnostic instead of refusing a valid setup.
    if [[ -n "$host" && "$host" != 0.0.0.0 && "$host" != :: ]]; then
      overlap=0
      while IFS= read -r item; do
        normalized=$(listener_address "$item")
        if [[ "$normalized" == 0:0:0:0:0:ffff:* ]]; then
          [[ "$family" != 6 || "$v6only" == 0 || "$v6only" == unknown && "$(listener_address "$endpoint")" == 0:0:0:0:0:ffff:* ]] || continue
        elif [[ "$family" == 4 || "$family" == unknown && "$endpoint" == '*' ]]; then continue
        fi
        if [[ "$endpoint" == '*' || "$endpoint" == 0.0.0.0 || "$endpoint" == :: || "$normalized" == "$(listener_address "$endpoint")" ]]; then overlap=1; fi
      done <<< "$host_addresses"
      ((overlap)) || continue
    fi
    [[ "$pid" =~ ^[1-9][0-9]*$ && "$pid" == "$old_pid" ]] && continue
    owner='unknown process'
    if [[ "$pid" =~ ^[1-9][0-9]*$ ]]; then owner=$(ps -p "$pid" -o comm= 2>/dev/null || true); owner=${owner:-process}; fi
    fail "Cannot listen on $address: port $port is already in use by $owner (PID $pid). Stop or reconfigure that service, or select another port. No installation files or services were changed."
  done < "$rows"
}
prepare_start() {
  launch_id=$(nonce)
  startup_log_lines=0
  [[ ! -f "$log_file" ]] || startup_log_lines=$(wc -l < "$log_file" | tr -d ' ')
  startup_previous_attempt=
  if [[ "$platform" == linux ]]; then startup_previous_attempt=$(systemctl show "$service_name" -p ExecMainStartTimestampMonotonic --value 2>/dev/null || true); fi
  mkdir -p "$data_dir/runtime"; chmod 700 "$data_dir/runtime"
  printf '%s\n' "$launch_id" > "$ready_file.request"; chmod 600 "$ready_file.request"
}
startup_failure() {
  local status attempt code value detail
  if [[ -f "$log_file" ]]; then
    detail=$(tail -n +$((startup_log_lines+1)) "$log_file" | sed -n '/^Service failed:/p' | tail -n 1)
    if [[ -n "$detail" ]]; then
      # Avoid printing arbitrary service errors, which can contain endpoint
      # credentials. The private log retains the full diagnostic.
      case "$detail" in
        *'address already in use'*) printf 'Startup failed: the listen address is already in use.\n' >&2;;
        *'permission denied'*) printf 'Startup failed: permission denied.\n' >&2;;
        *) printf 'The service reported a startup failure.\n' >&2;;
      esac
      printf 'Service: %s; log: %s\n' "$service_name" "$log_file" >&2
      return 0
    fi
  fi
  if [[ "$platform" == linux ]]; then
    status=$(systemctl show "$service_name" -p ExecMainStartTimestampMonotonic -p ExecMainCode -p ExecMainStatus 2>/dev/null) || return 1
    attempt=$(printf '%s\n' "$status" | sed -n 's/^ExecMainStartTimestampMonotonic=//p')
    code=$(printf '%s\n' "$status" | sed -n 's/^ExecMainCode=//p')
    value=$(printf '%s\n' "$status" | sed -n 's/^ExecMainStatus=//p')
    if [[ -n "$attempt" && "$attempt" != 0 && "$attempt" != "$startup_previous_attempt" && "$code" =~ ^[123]$ && "$value" =~ ^[0-9]+$ && "$value" != 0 ]]; then
      printf 'Startup failed: %s exited with status %s (code %s). Check %s and journalctl -u %s.\n' "$service_name" "$value" "$code" "$log_file" "$service_name" >&2
      return 0
    fi
  fi
  return 1
}
wait_ready() {
  local started=$SECONDS deadline=$((SECONDS+startup_timeout)) next_notice=$((SECONDS+10)) pid
  printf 'Waiting for %s to become ready (up to %ss)...\n' "$service_name" "$startup_timeout"
  while ((SECONDS < deadline)); do
    pid=$(service_pid || true)
    if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && "$binary" --config "$config" --check-ready --expect-pid "$pid" --expect-launch-id "$launch_id" >/dev/null 2>&1; then return 0; fi
    if startup_failure; then return 1; fi
    if ((SECONDS >= next_notice)); then
      printf 'Still waiting for %s (%ss elapsed); log: %s\n' "$service_name" "$((SECONDS-started))" "$log_file"
      next_notice=$((SECONDS+10))
    fi
    sleep 1
  done
  printf 'Startup timed out after %ss. Check %s and the %s service status.\n' "$startup_timeout" "$log_file" "$service_name" >&2
  return 1
}
print_setup_token() {
  [[ "$role" == master && -f "$log_file" ]] || return 0
  local token
  # Read this startup only: initialized installations retain an old token in
  # their log, but the Master does not generate one after setup is complete.
  token=$(tail -n +$((startup_log_lines+1)) "$log_file" |
    sed -n 's/^Open the console to complete setup\. Setup token: \([A-Za-z0-9_-][A-Za-z0-9_-]*\)$/\1/p' | tail -n 1)
  [[ -z "$token" ]] || printf '\nSetup token: %s\n' "$token"
}
rollback() {
  local result=$?
  trap - ERR INT TERM
  set +e
  if ((existed)); then printf 'Upgrade failed; restoring the previous installation.\n' >&2; fi
  if [[ -f "$unit" ]] && ! stop_service >/dev/null 2>&1; then
    printf 'Rollback stopped: the service could not be stopped. Current files retained; backup: %s\n' "$backup" >&2
    exit 1
  fi
  if ((cli_changed)); then
    if ((had_cli)); then
      if ! cp -p "$backup/cli" "$cli_path.restore" || ! mv -f "$cli_path.restore" "$cli_path"; then
        printf 'CLI rollback failed; backup: %s\n' "$backup" >&2; exit 1
      fi
    else
      rm -f "$cli_path"
    fi
  fi
  if ((existed)); then
    if [[ -f "$backup/database" ]] && ! "$work/$filename" --config "$backup/service.json" --restore-state "$backup/database"; then
      printf 'Database rollback incomplete. Current files retained; backup: %s\n' "$backup" >&2
      exit 1
    fi
    cp -p "$backup/binary" "$binary.restore" && mv -f "$binary.restore" "$binary"
    cp -p "$backup/service.json" "$config"
    cp -p "$backup/install.tsv" "$metadata"
    if [[ -f "$backup/unit" ]]; then cp -p "$backup/unit" "$unit"; else rm -f "$unit"; fi
    if [[ "$platform" == linux ]]; then systemctl daemon-reload; fi
    if ((was_active)); then prepare_start; start_service && wait_ready || printf 'Rollback files restored, but the original service did not become ready. Check %s\n' "$log_file" >&2; fi
  else
    rm -f "$unit"
    if [[ "$platform" == linux ]]; then systemctl daemon-reload; fi
    if [[ -n "$adopt_config" && -f "$backup/database" ]]; then
      if ! "$work/$filename" --config "$work/service.json" --restore-state "$backup/database"; then
        printf 'Database rollback incomplete. Current database retained; backup: %s\n' "$backup" >&2
        exit 1
      fi
      printf 'Restored the adopted database. Backup: %s\n' "$backup" >&2
    fi
    printf 'Installation failed. Files retained for diagnosis; log: %s\n' "$log_file" >&2
    if [[ -s "$database" ]]; then
      printf 'After correcting the startup error, retry with --upgrade.\n' >&2
    else
      printf 'No database was initialized. Inspect the startup error before retrying.\n' >&2
    fi
  fi
  [[ $result != 0 ]] || result=1
  exit "$result"
}
check_listener
if [[ -n "$adopt_config" ]]; then
  "$work/$filename" --config "$work/service.json" --backup-state "$work/database"
fi
mkdir -p "$install_dir" "$config_dir" "$data_dir" "$backup_dir" "$(dirname "$unit")"
chmod 700 "$install_dir" "$config_dir" "$data_dir" "$backup_dir"
if ((existed)); then
  backup=$(mktemp -d "$backup_dir/$service_name-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")
  printf 'Backup: %s\n' "$backup"
  cp -p "$binary" "$backup/binary"
  cp -p "$config" "$backup/service.json"
  cp -p "$metadata" "$backup/install.tsv"
  if [[ -f "$unit" ]]; then cp -p "$unit" "$backup/unit"; fi
  if active; then was_active=1; fi
  if [[ "$platform" == linux ]] && systemctl is-enabled --quiet "$service_name"; then was_enabled=1; fi
  trap rollback ERR INT TERM
  stop_service
  # Recheck the database lock after stopping the service; a manual process may
  # still own it. Take a consistent snapshot only when all writers are stopped.
  "$work/$filename" --config "$backup/service.json" --backup-state "$backup/database"
elif [[ -n "$adopt_config" ]]; then
  backup=$(mktemp -d "$backup_dir/$service_name-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")
  cp -p "$work/database" "$backup/database"
  cp -p "$work/service.json" "$backup/service.json"
  printf 'Backup: %s\n' "$backup"
fi
if ((had_cli)); then
  if [[ -z "$backup" ]]; then
    backup=$(mktemp -d "$backup_dir/$service_name-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")
    printf 'Backup: %s\n' "$backup"
  fi
  cp -p "$cli_path" "$backup/cli"
fi
trap rollback ERR INT TERM
cp "$work/$filename" "$binary.new"
chmod 755 "$binary.new"
mv -f "$binary.new" "$binary"
mkdir -p "$(dirname "$cli_path")"
cp "$work/$cli_filename" "$cli_path.new"
chmod 755 "$cli_path.new"
mv -f "$cli_path.new" "$cli_path"
cli_changed=1
if ((existed == 0)); then cp "$work/service.json" "$config"; chmod 600 "$config"; fi
printf 'role\t%s\nserviceName\t%s\nconfig\t%s\nunit\t%s\nversion\t%s\n' "$role" "$service_name" "$config" "$unit" "$version" > "$metadata"
printf 'cliPath\t%s\nbackupDir\t%s\ndataDir\t%s\n' "$cli_path" "$backup_dir" "$data_dir" >> "$metadata"
if [[ "$role" == agent && "$existed" == 0 && -z "$adopt_config" ]]; then
  OSS_TRANSFER_JOIN_TOKEN="$TRANSFER_JOIN_TOKEN" "$binary" --config "$config" --master-url "$master_url" --enroll-only
fi
unset TRANSFER_JOIN_TOKEN
if [[ "$platform" == linux ]]; then
  cat > "$unit" <<EOF
[Unit]
Description=OSS Transfer Go $role
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
ExecStart="$binary" --config "$config"
WorkingDirectory=$data_dir
Restart=on-failure
RestartSec=5
TimeoutStopSec=45
KillMode=control-group
UMask=0077
[Install]
WantedBy=multi-user.target
EOF
elif [[ "$platform" == freebsd ]]; then
  cat > "$unit" <<EOF
#!/bin/sh
# PROVIDE: $rc_name
# REQUIRE: NETWORKING
# KEYWORD: shutdown
# oss-transfer-install: $install_dir
. /etc/rc.subr
name="$rc_name"
rcvar="\${name}_enable"
command="/usr/sbin/daemon"
pidfile="$supervisor_pidfile"
procname="/usr/sbin/daemon"
command_args="-f -r -R 5 -P $supervisor_pidfile -p $child_pidfile -- \"$binary\" --config \"$config\""
load_rc_config "\$name"
: \${${rc_name}_enable:=YES}
run_rc_command "\$1"
EOF
else
  xml() { printf '%s' "$1" | sed 's/\&/\&amp;/g;s/</\&lt;/g;s/>/\&gt;/g'; }
  cat > "$unit" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>$(xml "$service_name")</string>
<key>ProgramArguments</key><array><string>$(xml "$binary")</string><string>--config</string><string>$(xml "$config")</string></array>
<key>WorkingDirectory</key><string>$(xml "$data_dir")</string>
<key>RunAtLoad</key><true/><key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ThrottleInterval</key><integer>5</integer><key>ExitTimeOut</key><integer>45</integer>
<key>Umask</key><integer>63</integer>
</dict></plist>
EOF
  plutil -lint "$unit" >/dev/null
fi
if [[ "$platform" == freebsd ]]; then chmod 755 "$unit"; else chmod 644 "$unit"; fi
prepare_start
printf 'Starting %s...\n' "$service_name"
start_service
wait_ready
if [[ "$platform" == linux ]]; then
  if ((existed == 0 || had_unit == 0 || was_enabled)); then systemctl enable "$service_name" >/dev/null; fi
fi
trap - ERR INT TERM
printf 'Installed %s %s. Service: %s\nConfiguration: %s\nLog: %s\n' "$role" "$version" "$service_name" "$config" "$log_file"
printf 'CLI: %s\n' "$cli_path"
if [[ "$role" == master ]]; then
  installed_listen=$(awk -F '\t' '$1=="listen"{print $2}' "$work/config.tsv")
  cli_host=${installed_listen%:*}
  case "$cli_host" in ''|0.0.0.0) cli_host=127.0.0.1;; '[::]') cli_host='[::1]';; esac
  printf 'Login: %q --host %q login\n' "$cli_path" "http://$cli_host:${installed_listen##*:}"
fi
print_setup_token
