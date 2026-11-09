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

# shellcheck shell=bash
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

transfer_source_text() { if [[ "${OSS_TRANSFER_INSTALL_LANG:-en}" == zh ]]; then printf '%s\n' "$2"; else printf '%s\n' "$1"; fi; }

# Verify a platform archive through its internal SHA256SUMS and VERSION, then
# write manifest.tsv beside it in WORK. An empty expected version accepts the
# version recorded in the archive (an offline copy has no external metadata).
transfer_archive_manifest() (
  set -o pipefail
  local work=$1 platform=$2 arch=$3 digest=$4 size=$5 expected_version=$6
  local root=oss-transfer-$platform-$arch name actual version commit role payload expected length
  name=$root.tar.gz
  transfer_source_member "$work/$name" "$root/VERSION" "$work/VERSION" && transfer_source_member "$work/$name" "$root/SHA256SUMS" "$work/SHA256SUMS" || { transfer_source_error 'Invalid archive metadata members'; return 1; }
  [[ $(wc -c < "$work/VERSION") -lt 4096 && $(wc -c < "$work/SHA256SUMS") -lt 4096 ]] || { transfer_source_error 'Archive metadata too large'; return 1; }
  actual=$(awk -v platform="$platform/$arch" -v version="$expected_version" '
    { separator=index($0,": "); if (!separator) exit 1; key=substr($0,1,separator-1); value=substr($0,separator+2); if (key in fields) exit 1; fields[key]=value }
    END {
      if (version!="" && fields["Version"]!=version) exit 1
      if (fields["Version"] !~ /^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/ || fields["Platform"]!=platform || length(fields["FullCommit"])!=40 || fields["FullCommit"] ~ /[^a-f0-9]/) exit 1
      print fields["Version"] "\t" fields["FullCommit"]
    }
  ' "$work/VERSION") || { transfer_source_error 'Archive identity does not match the release'; return 1; }
  IFS=$'\t' read -r version commit <<< "$actual" || return 1
  # Exactly three executables and VERSION are checksummed by scripts/build.sh.
  awk '
    NF!=2 { bad=1; exit 1 }
    { hash=$1; name=$2; sub(/^\*/,"",name); if (length(hash)!=64 || hash ~ /[^a-f0-9]/ || name !~ /^(oss-transfer-(master|agent|cli)|VERSION)$/ || name in seen) { bad=1; exit 1 }; seen[name]=1; print name "\t" hash; count++ }
    END { if (bad || count!=4) exit 1 }
  ' "$work/SHA256SUMS" > "$work/checksums.tsv" || { transfer_source_error 'Invalid internal checksum manifest'; return 1; }
  expected=$(awk -F '\t' '$1=="VERSION" {print $2}' "$work/checksums.tsv")
  [[ $(transfer_source_sha256 "$work/VERSION") == "$expected" ]] || { transfer_source_error 'Archive VERSION checksum mismatch'; return 1; }
  printf 'format\toss-transfer-go-v1\nversion\t%s\ncommit\t%s\n' "$version" "$commit" > "$work/manifest.tsv" || return 1
  for role in master agent cli; do
    payload=oss-transfer-$role
    transfer_source_member "$work/$name" "$root/$payload" "$work/$payload" || { transfer_source_error "Invalid archive executable: $role"; return 1; }
    expected=$(awk -F '\t' -v name="$payload" '$1==name {print $2}' "$work/checksums.tsv")
    [[ $(transfer_source_sha256 "$work/$payload") == "$expected" ]] || { transfer_source_error "Archive executable checksum mismatch: $role"; return 1; }
    length=$(wc -c < "$work/$payload" | tr -d '[:space:]') || return 1
    [[ "$length" != 0 ]] || { transfer_source_error "Empty archive executable: $role"; return 1; }
    printf 'file\t%s\t%s\t%s\t%s-%s-%s\t%s\t%s\n' "$platform" "$arch" "$role" "$payload" "$platform" "$arch" "$expected" "$length" >> "$work/manifest.tsv" || return 1
  done
  printf 'archive\t%s\t%s\t%s\t%s\t%s\n' "$platform" "$arch" "$name" "$digest" "$size" >> "$work/manifest.tsv"
)

transfer_asset_row() {
  local path=$1 name=$2 hash size
  [[ -s "$path" ]] || { transfer_source_error "Empty installer: $name"; return 1; }
  hash=$(transfer_source_sha256 "$path") || return 1
  size=$(wc -c < "$path" | tr -d '[:space:]') || return 1
  printf 'asset\t%s\t%s\t%s\n' "$name" "$hash" "$size"
}

# Produce a local package directory for the existing native installer. The
# archive digest comes from GitHub; the installer is pinned to the FullCommit
# recorded inside that verified archive, never to a moving branch or latest.
transfer_github_prepare() (
  set -o pipefail
  local requested=${1#v} platform=$2 arch=$3 destination=$4
  local name root api tag digest size url actual commit installer installer_path
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
  transfer_archive_manifest "$transfer_download_work" "$platform" "$arch" "$digest" "$size" "${tag#v}" || return 1
  commit=$(awk -F '\t' '$1=="commit" {print $2}' "$transfer_download_work/manifest.tsv")
  # Keep all installers at the verified archive commit. A Master retains these
  # files to install Agents without requiring their hosts to reach the internet.
  for installer in native-install.sh install.sh Install.ps1; do
    case "$installer" in
      native-install.sh) installer_path=deploy/native/install.sh;;
      Install.ps1) installer_path=deploy/native/Install.ps1;;
      *) installer_path=install.sh;;
    esac
    if ! transfer_source_fetch "https://raw.githubusercontent.com/MoonWX/oss-transfer/$commit/$installer_path" "$transfer_download_work/$installer" 1048576; then
      [[ "$installer_path" == deploy/native/* ]] && transfer_source_fetch "https://raw.githubusercontent.com/MoonWX/oss-transfer/$commit/deploy/go/${installer_path#deploy/native/}" "$transfer_download_work/$installer" 1048576 || { transfer_source_error "Cannot fetch the versioned native installer: $installer"; return 1; }
    fi
    transfer_asset_row "$transfer_download_work/$installer" "$installer" >> "$transfer_download_work/manifest.tsv" || return 1
  done
  for name in "$name" manifest.tsv native-install.sh install.sh Install.ps1; do
    [[ ! -e "$destination/$name" && ! -L "$destination/$name" ]] || { transfer_source_error 'Package directory already contains release files'; return 1; }
  done
  mv "$transfer_download_work/$root.tar.gz" "$transfer_download_work/manifest.tsv" "$transfer_download_work/native-install.sh" "$transfer_download_work/install.sh" "$transfer_download_work/Install.ps1" "$destination/"

)

# Offline variant: build manifest.tsv for this host's platform archive copied
# from a GitHub Release, without any network access. The archive's internal
# SHA256SUMS and VERSION are still verified. Installers beside the archive, or
# the NAME PATH fallbacks after DESTINATION, are recorded as release assets.
transfer_local_prepare() (
  set -o pipefail
  local source=$1 platform=$2 arch=$3 destination=$4 name installer candidate index
  local -a fallbacks=("${@:5}")
  name=oss-transfer-$platform-$arch.tar.gz
  [[ -d "$destination" && ! -L "$destination" ]] || { transfer_source_error 'Package directory must be a regular directory'; return 1; }
  if [[ ! -f "$source/$name" || -L "$source/$name" ]]; then
    transfer_source_error "$(transfer_source_text \
      "$source has neither manifest.tsv nor $name. Copy the complete release (manifest.tsv, $name, install.sh, native-install.sh) from the GitHub Release or https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/ into one directory." \
      "$source 中既没有 manifest.tsv，也没有本机所需的 ${name}。请从 GitHub Release 或 https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/ 下载完整发布文件（manifest.tsv、${name}、install.sh、native-install.sh）放入同一目录。")"
    return 1
  fi
  transfer_download_work=$(mktemp -d "$destination/.local-package.XXXXXX") || return 1
  trap 'rm -rf -- "$transfer_download_work"' EXIT
  cp "$source/$name" "$transfer_download_work/$name" || return 1
  transfer_archive_manifest "$transfer_download_work" "$platform" "$arch" \
    "$(transfer_source_sha256 "$transfer_download_work/$name")" "$(wc -c < "$transfer_download_work/$name" | tr -d '[:space:]')" '' || return 1
  for installer in native-install.sh install.sh Install.ps1; do
    candidate=
    if [[ -f "$source/$installer" && ! -L "$source/$installer" ]]; then candidate=$source/$installer
    else
      for ((index = 0; index + 1 < ${#fallbacks[@]}; index += 2)); do
        if [[ "${fallbacks[index]}" == "$installer" && -f "${fallbacks[index+1]}" ]]; then candidate=${fallbacks[index+1]}; break; fi
      done
    fi
    [[ -n "$candidate" ]] || continue
    cp "$candidate" "$transfer_download_work/$installer" || return 1
    transfer_asset_row "$transfer_download_work/$installer" "$installer" >> "$transfer_download_work/manifest.tsv" || return 1
  done
  for installer in "$name" manifest.tsv native-install.sh install.sh Install.ps1; do
    [[ ! -e "$destination/$installer" && ! -L "$destination/$installer" ]] || { transfer_source_error 'Package directory already contains release files'; return 1; }
    [[ ! -e "$transfer_download_work/$installer" ]] || mv "$transfer_download_work/$installer" "$destination/" || return 1
  done
)

# Offline bundles: use release files beside the installer, then in the current
# directory. A piped installer has no file of its own and keeps downloading.
transfer_local_package_dir() {
  local script=$1 platform=$2 arch=$3 directory
  [[ -n "$script" && -f "$script" ]] || return 1
  for directory in "$(cd "$(dirname "$script")" && pwd -P)" "$(pwd -P)"; do
    if [[ -f "$directory/manifest.tsv" && ! -L "$directory/manifest.tsv" ]] ||
       [[ -f "$directory/oss-transfer-$platform-$arch.tar.gz" && ! -L "$directory/oss-transfer-$platform-$arch.tar.gz" ]]; then
      printf '%s\n' "$directory"
      return 0
    fi
  done
  return 1
}

# END embedded download source
# BEGIN embedded native installer
# Generated from deploy/native/install.sh; used only when an offline package lacks native-install.sh.
transfer_embedded_native_installer() {
  cat <<'OSS_TRANSFER_EMBEDDED_NATIVE_INSTALLER'
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

# shellcheck shell=bash
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

transfer_source_text() { if [[ "${OSS_TRANSFER_INSTALL_LANG:-en}" == zh ]]; then printf '%s\n' "$2"; else printf '%s\n' "$1"; fi; }

# Verify a platform archive through its internal SHA256SUMS and VERSION, then
# write manifest.tsv beside it in WORK. An empty expected version accepts the
# version recorded in the archive (an offline copy has no external metadata).
transfer_archive_manifest() (
  set -o pipefail
  local work=$1 platform=$2 arch=$3 digest=$4 size=$5 expected_version=$6
  local root=oss-transfer-$platform-$arch name actual version commit role payload expected length
  name=$root.tar.gz
  transfer_source_member "$work/$name" "$root/VERSION" "$work/VERSION" && transfer_source_member "$work/$name" "$root/SHA256SUMS" "$work/SHA256SUMS" || { transfer_source_error 'Invalid archive metadata members'; return 1; }
  [[ $(wc -c < "$work/VERSION") -lt 4096 && $(wc -c < "$work/SHA256SUMS") -lt 4096 ]] || { transfer_source_error 'Archive metadata too large'; return 1; }
  actual=$(awk -v platform="$platform/$arch" -v version="$expected_version" '
    { separator=index($0,": "); if (!separator) exit 1; key=substr($0,1,separator-1); value=substr($0,separator+2); if (key in fields) exit 1; fields[key]=value }
    END {
      if (version!="" && fields["Version"]!=version) exit 1
      if (fields["Version"] !~ /^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$/ || fields["Platform"]!=platform || length(fields["FullCommit"])!=40 || fields["FullCommit"] ~ /[^a-f0-9]/) exit 1
      print fields["Version"] "\t" fields["FullCommit"]
    }
  ' "$work/VERSION") || { transfer_source_error 'Archive identity does not match the release'; return 1; }
  IFS=$'\t' read -r version commit <<< "$actual" || return 1
  # Exactly three executables and VERSION are checksummed by scripts/build.sh.
  awk '
    NF!=2 { bad=1; exit 1 }
    { hash=$1; name=$2; sub(/^\*/,"",name); if (length(hash)!=64 || hash ~ /[^a-f0-9]/ || name !~ /^(oss-transfer-(master|agent|cli)|VERSION)$/ || name in seen) { bad=1; exit 1 }; seen[name]=1; print name "\t" hash; count++ }
    END { if (bad || count!=4) exit 1 }
  ' "$work/SHA256SUMS" > "$work/checksums.tsv" || { transfer_source_error 'Invalid internal checksum manifest'; return 1; }
  expected=$(awk -F '\t' '$1=="VERSION" {print $2}' "$work/checksums.tsv")
  [[ $(transfer_source_sha256 "$work/VERSION") == "$expected" ]] || { transfer_source_error 'Archive VERSION checksum mismatch'; return 1; }
  printf 'format\toss-transfer-go-v1\nversion\t%s\ncommit\t%s\n' "$version" "$commit" > "$work/manifest.tsv" || return 1
  for role in master agent cli; do
    payload=oss-transfer-$role
    transfer_source_member "$work/$name" "$root/$payload" "$work/$payload" || { transfer_source_error "Invalid archive executable: $role"; return 1; }
    expected=$(awk -F '\t' -v name="$payload" '$1==name {print $2}' "$work/checksums.tsv")
    [[ $(transfer_source_sha256 "$work/$payload") == "$expected" ]] || { transfer_source_error "Archive executable checksum mismatch: $role"; return 1; }
    length=$(wc -c < "$work/$payload" | tr -d '[:space:]') || return 1
    [[ "$length" != 0 ]] || { transfer_source_error "Empty archive executable: $role"; return 1; }
    printf 'file\t%s\t%s\t%s\t%s-%s-%s\t%s\t%s\n' "$platform" "$arch" "$role" "$payload" "$platform" "$arch" "$expected" "$length" >> "$work/manifest.tsv" || return 1
  done
  printf 'archive\t%s\t%s\t%s\t%s\t%s\n' "$platform" "$arch" "$name" "$digest" "$size" >> "$work/manifest.tsv"
)

transfer_asset_row() {
  local path=$1 name=$2 hash size
  [[ -s "$path" ]] || { transfer_source_error "Empty installer: $name"; return 1; }
  hash=$(transfer_source_sha256 "$path") || return 1
  size=$(wc -c < "$path" | tr -d '[:space:]') || return 1
  printf 'asset\t%s\t%s\t%s\n' "$name" "$hash" "$size"
}

# Produce a local package directory for the existing native installer. The
# archive digest comes from GitHub; the installer is pinned to the FullCommit
# recorded inside that verified archive, never to a moving branch or latest.
transfer_github_prepare() (
  set -o pipefail
  local requested=${1#v} platform=$2 arch=$3 destination=$4
  local name root api tag digest size url actual commit installer installer_path
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
  transfer_archive_manifest "$transfer_download_work" "$platform" "$arch" "$digest" "$size" "${tag#v}" || return 1
  commit=$(awk -F '\t' '$1=="commit" {print $2}' "$transfer_download_work/manifest.tsv")
  # Keep all installers at the verified archive commit. A Master retains these
  # files to install Agents without requiring their hosts to reach the internet.
  for installer in native-install.sh install.sh Install.ps1; do
    case "$installer" in
      native-install.sh) installer_path=deploy/native/install.sh;;
      Install.ps1) installer_path=deploy/native/Install.ps1;;
      *) installer_path=install.sh;;
    esac
    if ! transfer_source_fetch "https://raw.githubusercontent.com/MoonWX/oss-transfer/$commit/$installer_path" "$transfer_download_work/$installer" 1048576; then
      [[ "$installer_path" == deploy/native/* ]] && transfer_source_fetch "https://raw.githubusercontent.com/MoonWX/oss-transfer/$commit/deploy/go/${installer_path#deploy/native/}" "$transfer_download_work/$installer" 1048576 || { transfer_source_error "Cannot fetch the versioned native installer: $installer"; return 1; }
    fi
    transfer_asset_row "$transfer_download_work/$installer" "$installer" >> "$transfer_download_work/manifest.tsv" || return 1
  done
  for name in "$name" manifest.tsv native-install.sh install.sh Install.ps1; do
    [[ ! -e "$destination/$name" && ! -L "$destination/$name" ]] || { transfer_source_error 'Package directory already contains release files'; return 1; }
  done
  mv "$transfer_download_work/$root.tar.gz" "$transfer_download_work/manifest.tsv" "$transfer_download_work/native-install.sh" "$transfer_download_work/install.sh" "$transfer_download_work/Install.ps1" "$destination/"

)

# Offline variant: build manifest.tsv for this host's platform archive copied
# from a GitHub Release, without any network access. The archive's internal
# SHA256SUMS and VERSION are still verified. Installers beside the archive, or
# the NAME PATH fallbacks after DESTINATION, are recorded as release assets.
transfer_local_prepare() (
  set -o pipefail
  local source=$1 platform=$2 arch=$3 destination=$4 name installer candidate index
  local -a fallbacks=("${@:5}")
  name=oss-transfer-$platform-$arch.tar.gz
  [[ -d "$destination" && ! -L "$destination" ]] || { transfer_source_error 'Package directory must be a regular directory'; return 1; }
  if [[ ! -f "$source/$name" || -L "$source/$name" ]]; then
    transfer_source_error "$(transfer_source_text \
      "$source has neither manifest.tsv nor $name. Copy the complete release (manifest.tsv, $name, install.sh, native-install.sh) from the GitHub Release or https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/ into one directory." \
      "$source 中既没有 manifest.tsv，也没有本机所需的 ${name}。请从 GitHub Release 或 https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/latest/ 下载完整发布文件（manifest.tsv、${name}、install.sh、native-install.sh）放入同一目录。")"
    return 1
  fi
  transfer_download_work=$(mktemp -d "$destination/.local-package.XXXXXX") || return 1
  trap 'rm -rf -- "$transfer_download_work"' EXIT
  cp "$source/$name" "$transfer_download_work/$name" || return 1
  transfer_archive_manifest "$transfer_download_work" "$platform" "$arch" \
    "$(transfer_source_sha256 "$transfer_download_work/$name")" "$(wc -c < "$transfer_download_work/$name" | tr -d '[:space:]')" '' || return 1
  for installer in native-install.sh install.sh Install.ps1; do
    candidate=
    if [[ -f "$source/$installer" && ! -L "$source/$installer" ]]; then candidate=$source/$installer
    else
      for ((index = 0; index + 1 < ${#fallbacks[@]}; index += 2)); do
        if [[ "${fallbacks[index]}" == "$installer" && -f "${fallbacks[index+1]}" ]]; then candidate=${fallbacks[index+1]}; break; fi
      done
    fi
    [[ -n "$candidate" ]] || continue
    cp "$candidate" "$transfer_download_work/$installer" || return 1
    transfer_asset_row "$transfer_download_work/$installer" "$installer" >> "$transfer_download_work/manifest.tsv" || return 1
  done
  for installer in "$name" manifest.tsv native-install.sh install.sh Install.ps1; do
    [[ ! -e "$destination/$installer" && ! -L "$destination/$installer" ]] || { transfer_source_error 'Package directory already contains release files'; return 1; }
    [[ ! -e "$transfer_download_work/$installer" ]] || mv "$transfer_download_work/$installer" "$destination/" || return 1
  done
)

# Offline bundles: use release files beside the installer, then in the current
# directory. A piped installer has no file of its own and keeps downloading.
transfer_local_package_dir() {
  local script=$1 platform=$2 arch=$3 directory
  [[ -n "$script" && -f "$script" ]] || return 1
  for directory in "$(cd "$(dirname "$script")" && pwd -P)" "$(pwd -P)"; do
    if [[ -f "$directory/manifest.tsv" && ! -L "$directory/manifest.tsv" ]] ||
       [[ -f "$directory/oss-transfer-$platform-$arch.tar.gz" && ! -L "$directory/oss-transfer-$platform-$arch.tar.gz" ]]; then
      printf '%s\n' "$directory"
      return 0
    fi
  done
  return 1
}

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
had_cli_sidecars=0
had_data_cli_connections=0
cli_changed=0
cli_migrated=0
root_dir=
install_dir=
config_dir=
data_dir=
backup_dir=
service_name=
listen=0.0.0.0:1685
master_url=
token_stdin=0
upgrade=0
uninstall=0
keep_data=0
force=0
yes=0
user_service=0
startup_timeout=120
local_detected=0
local_stage=
lang=${OSS_TRANSFER_INSTALL_LANG:-}
# Service definition locations; isolated tests replace them.
systemd_unit_dir=/etc/systemd/system
rc_dir=/usr/local/etc/rc.d
pid_dir=/var/run
say() { if [[ "${lang:-en}" == zh ]]; then printf '%s\n' "$2"; else printf '%s\n' "$1"; fi; }
usage() {
  cat <<'EOF'
Usage: bash install.sh [--role master|agent] [--upgrade] [options]
  --lang en|zh         Output language (default: system locale)
  --release-url URL    Release directory (default: public latest)
  --source auto|github|jdoss  Download source (default: public IP location)
  --version VERSION   Release version (default: latest)
  --package-dir DIR    Install verified files from a local release directory
  --upgrade            Back up and upgrade an existing installation
  --adopt-config FILE   Install a service for stopped, existing Go data
  --cli-path FILE       CLI location (default: global when available)
  --replace-cli         Back up and replace an existing unmanaged CLI
  --listen HOST:PORT   Master listener (default 0.0.0.0:1685; first install only)
  --master-url URL     Master URL for Agent enrollment or rejoining
  --token-stdin        Read the Agent invitation token from standard input
  --service-name NAME  Isolated service name
  --root-dir DIR       Keep programs, configuration and data under one directory
  --install-dir DIR --config-dir DIR --data-dir DIR --backup-dir DIR
  --user               macOS: install a LaunchAgent for the current logged-in user
  --startup-timeout N  Readiness timeout, 10–600 seconds (default 120)
  --force              Reinstall even when the same version is installed and running
  --yes                Proceed without interactive confirmation

Uninstall: bash install.sh --uninstall [--role master|agent] [--keep-data] [--yes]
  Lists, then removes the services, programs, installation records, CLI and
  (unless --keep-data) configuration, data and upgrade backups. Only
  installations recorded by this installer are touched; --service-name,
  --root-dir, --install-dir and --user select one of them.
EOF
}
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
while (($#)); do
  case "$1" in
    --version) (($# >= 2)) || fail 'Missing value: --version'; requested_version=$2; shift 2;;
    --lang|--role|--source|--release-url|--package-dir|--adopt-config|--cli-path|--root-dir|--install-dir|--config-dir|--data-dir|--backup-dir|--service-name|--listen|--master-url|--startup-timeout)
      (($# >= 2)) || fail "Missing value: $1"
      key=${1#--}; key=${key//-/_}; printf -v "$key" '%s' "$2"; shift 2 ;;
    --token-stdin) token_stdin=1; shift;;
    --upgrade) upgrade=1; shift;;
    --uninstall) uninstall=1; shift;;
    --keep-data) keep_data=1; shift;;
    --force) force=1; shift;;
    --replace-cli) replace_cli=1; shift;;
    --yes) yes=1; shift;;
    --user) user_service=1; shift;;
    --help|-h) usage; exit 0;;
    *) fail "Unknown option: $1";;
  esac
done
if [[ -z "$lang" ]]; then
  case "${LC_ALL:-${LC_MESSAGES:-${LANG:-}}}" in zh*) lang=zh;; *) lang=en;; esac
fi
[[ "$lang" == en || "$lang" == zh ]] || fail '--lang must be en or zh'
export OSS_TRANSFER_INSTALL_LANG=$lang
[[ -z "$role" || "$role" == master || "$role" == agent ]] || fail 'Role must be master or agent'
[[ -z "$adopt_config" || "$upgrade" == 0 ]] || fail 'Choose --adopt-config or --upgrade'
if ((uninstall)); then
  # Uninstalling never downloads or installs anything.
  [[ "$upgrade" == 0 && "$force" == 0 && "$replace_cli" == 0 && "$token_stdin" == 0 && -z "$adopt_config$release_url$package_dir$cli_path$master_url$config_dir$data_dir$backup_dir" && "$requested_version" == latest ]] ||
    fail "$(say '--uninstall accepts only --role, --keep-data, --service-name, --root-dir, --install-dir, --user, --lang and --yes' '--uninstall 只能与 --role、--keep-data、--service-name、--root-dir、--install-dir、--user、--lang 和 --yes 一起使用')"
elif ((keep_data)); then
  fail "$(say '--keep-data is used with --uninstall' '--keep-data 需与 --uninstall 一起使用')"
fi
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
# Configuration values are discovery hints only. The downloaded binary validates
# the complete JSON and confirms all paths before any existing file is changed.
config_field() {
  LC_ALL=C sed -nE 's/.*"'"$2"'"[[:space:]]*:[[:space:]]*"([^"\\]*)".*/\1/p' "$1"
}
# Print one unit property. systemd 219 (CentOS 7 / RHEL 7) has no
# `systemctl show --value`; every version prints NAME=VALUE for -p NAME.
systemd_property() {
  local output
  output=$(systemctl show "$1" -p "$2") || return 1
  printf '%s\n' "$output" | awk -v key="$2=" 'index($0, key) == 1 && !found {found=1; print substr($0, length(key) + 1)}'
}
default_root() {
  if [[ "$platform" == linux ]]; then printf '/opt/oss-transfer\n'
  elif [[ "$platform" == freebsd ]]; then printf '/usr/local/lib/oss-transfer\n'
  elif ((user_service)); then printf '%s/Library/Application Support/OSS Transfer\n' "$HOME"
  else printf '/Library/Application Support/OSS Transfer\n'; fi
}
# Collect installer-managed installations into candidates. Installation
# metadata (install.tsv) is the record of ownership; service definitions only
# lead to installations with custom directories.
add_candidate() {
  local directory=$1 existing item_role item_name
  [[ -f "$directory/install.tsv" && ! -L "$directory/install.tsv" ]] || return 0
  item_role=$(metadata_field "$directory/install.tsv" role)
  item_name=$(metadata_field "$directory/install.tsv" serviceName)
  [[ "$item_role" == master || "$item_role" == agent ]] || return 0
  [[ -z "$want_role" || "$want_role" == "$item_role" ]] || return 0
  [[ -z "$want_service" || "$want_service" == "$item_name" ]] || return 0
  for existing in ${candidates[@]+"${candidates[@]}"}; do [[ "$existing" != "$directory" ]] || return 0; done
  candidates+=("$directory")
}
discover_installations() {
  local want_role=$1 want_service=$2 item_role definition definitions installed_binary directory
  candidates=()
  for item_role in master agent; do
    if [[ "$platform" == linux ]]; then
      add_candidate "$root_dir/$item_role/bin"
      if ((root_explicit == 0)); then
        add_candidate "/opt/oss-transfer/$item_role"
        add_candidate "/opt/oss-transfer-go/$item_role"
      fi
    elif [[ "$platform" == freebsd ]]; then add_candidate "$root_dir/$item_role/bin"; if ((root_explicit == 0)); then add_candidate "/usr/local/lib/oss-transfer/$item_role"; fi
    elif ((user_service)); then add_candidate "$root_dir/$item_role/bin"
    else add_candidate "$root_dir/$item_role/bin"; if ((root_explicit == 0)); then add_candidate "/usr/local/lib/oss-transfer/$item_role"; fi; fi
  done
  if ((root_explicit)); then :
  elif [[ "$platform" == linux ]]; then
    for definition in "$systemd_unit_dir"/*.service; do
      [[ -f "$definition" && ! -L "$definition" ]] || continue
      installed_binary=$(sed -nE 's#^ExecStart="([^"]*/oss-transfer-(master|agent))" .*#\1#p' "$definition")
      [[ -z "$installed_binary" ]] || add_candidate "$(dirname "$installed_binary")"
    done
  elif [[ "$platform" == freebsd ]]; then
    for definition in "$rc_dir"/*; do
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
}
# BEGIN uninstall
# Uninstall plans every change before making one: the plan lists what is
# removed and kept, and nothing outside installer-recorded paths is touched.
uninstall_targets=()
uninstall_remaining=()
uninstall_services=()
uninstall_actions=()
uninstall_removed=()
uninstall_kept=()
uninstall_agent=0
uninstall_master=0
path_depth() { local value=${1//[!\/]/}; printf '%s\n' "${#value}"; }
# Directories that are never removed as a whole, even when recorded.
protected_directory() {
  case "$1" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/mnt|/media|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/usr/local|/usr/local/lib|/usr/local/etc|/var|/var/lib|/var/backups|/var/tmp|/Users|/Library|/Applications|/System|/private|/private/var|/private/tmp) return 0;;
  esac
  [[ "$1" != "${HOME:-/}" ]] || return 0
  (( $(path_depth "$1") < 2 ))
}
listed_in() {
  local wanted=$1 item; shift
  for item in "$@"; do [[ "$item" != "$wanted" ]] || return 0; done
  return 1
}
# Another remaining installation's recorded paths inside DIRECTORY.
contains_other_installation() {
  local directory=$1 other field value
  for other in ${uninstall_remaining[@]+"${uninstall_remaining[@]}"}; do
    [[ "$other" != "$directory" && "$other" != "$directory/"* ]] || return 0
    for field in config dataDir backupDir cliPath; do
      value=$(metadata_field "$other/install.tsv" "$field")
      [[ -z "$value" || ( "$value" != "$directory" && "$value" != "$directory/"* ) ]] || return 0
    done
  done
  return 1
}
# Only data the service wrote: its database, or nothing but runtime and logs.
owned_data_directory() {
  local directory=$1 role_name=$2 entry
  [[ -f "$directory/$role_name.db" && ! -L "$directory/$role_name.db" ]] && return 0
  for entry in "$directory"/* "$directory"/.[!.]* "$directory"/..?*; do
    [[ -e "$entry" || -L "$entry" ]] || continue
    case "${entry##*/}" in runtime|logs) ;; *) return 1;; esac
  done
  return 0
}
plan_remove_file() {
  local path=$1 label=$2
  [[ -e "$path" || -L "$path" ]] || return 0
  [[ -f "$path" && ! -L "$path" ]] || fail "$(say "Not a regular file; nothing was removed: $path" "不是普通文件，未做任何更改：$path")"
  uninstall_actions+=("file	$path")
  [[ -z "$label" ]] || uninstall_removed+=("$label: $path")
}
plan_uninstall() {
  local directory=$1 metadata=$1/install.tsv item_role item_service item_config item_unit item_cli item_backup item_data item_root
  local binary expected config_parent shared other suffix
  item_role=$(metadata_field "$metadata" role)
  item_service=$(metadata_field "$metadata" serviceName)
  item_config=$(metadata_field "$metadata" config)
  item_unit=$(metadata_field "$metadata" unit)
  item_cli=$(metadata_field "$metadata" cliPath)
  item_backup=$(metadata_field "$metadata" backupDir)
  item_data=$(metadata_field "$metadata" dataDir)
  item_root=$(metadata_field "$metadata" rootDir)
  [[ "$item_role" == master || "$item_role" == agent ]] && [[ "$item_service" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$ && "$item_config" == /*/service.json ]] ||
    fail "$(say "Invalid installation record; nothing was removed: $metadata" "安装记录无效，未做任何更改：$metadata")"
  if [[ -z "$item_data" && -f "$item_config" && ! -L "$item_config" ]]; then item_data=$(config_field "$item_config" dataDirectory); fi
  safe_path "$directory"
  config_parent=$(dirname "$item_config"); safe_path "$config_parent"
  [[ -z "$item_data" ]] || safe_path "$item_data"
  [[ -z "$item_backup" ]] || safe_path "$item_backup"
  [[ -z "$item_root" ]] || safe_path "$item_root"
  if [[ -n "$item_cli" ]]; then
    safe_path "$(dirname "$item_cli")"
    [[ "$(basename "$item_cli")" == oss-transfer-cli ]] || fail "$(say "Invalid CLI path in $metadata" "$metadata 中的 CLI 路径无效")"
  fi
  binary=$directory/oss-transfer-$item_role
  case "$platform" in
    linux) expected=$systemd_unit_dir/$item_service.service;;
    freebsd) expected=$rc_dir/$item_service;;
    *) if ((user_service)); then expected=$HOME/Library/LaunchAgents/$item_service.plist; else expected=/Library/LaunchDaemons/$item_service.plist; fi;;
  esac
  [[ -z "$item_unit" || "$item_unit" == "$expected" ]] ||
    fail "$(say "$item_service is recorded with service definition $item_unit, not $expected; nothing was removed (macOS: add --user for a LaunchAgent)" "$item_service 记录的服务定义是 ${item_unit}，不是 ${expected}；未做任何更改（macOS 用户服务请加 --user）")"
  [[ ! -L "$expected" ]] || fail "$(say "Service definition is a symlink; nothing was removed: $expected" "服务定义是符号链接，未做任何更改：$expected")"
  if [[ -f "$expected" ]]; then
    # Never stop a service that runs anything but this installation.
    case "$platform" in
      linux) grep -Fx "ExecStart=\"$binary\" --config \"$item_config\"" "$expected" >/dev/null;;
      freebsd) grep -Fx "# oss-transfer-install: $directory" "$expected" >/dev/null;;
      *) [[ $(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$expected" 2>/dev/null) == "$binary" ]];;
    esac || fail "$(say "Service $item_service does not run $binary; nothing was removed" "服务 $item_service 运行的不是 ${binary}；未做任何更改")"
    uninstall_services+=("$item_service	$expected")
    uninstall_removed+=("$(say Service 服务): $item_service ($expected)")
  fi
  [[ "$item_role" == agent ]] && uninstall_agent=1
  [[ "$item_role" == master ]] && uninstall_master=1
  plan_remove_file "$binary" "$(say Program 程序)"
  plan_remove_file "$metadata" "$(say 'Installation record' 安装记录)"
  if [[ -n "$item_cli" ]]; then
    shared=
    for other in ${uninstall_remaining[@]+"${uninstall_remaining[@]}"}; do
      [[ $(metadata_field "$other/install.tsv" cliPath) != "$item_cli" ]] || shared=$other
    done
    if [[ -n "$shared" ]]; then
      uninstall_kept+=("CLI: $item_cli ($(say "used by $shared" "仍由 $shared 使用"))")
    elif ! listed_in "file	$item_cli" ${uninstall_actions[@]+"${uninstall_actions[@]}"}; then
      plan_remove_file "$item_cli" CLI
      for suffix in cluster.json local-auth.json update.lock; do plan_remove_file "$item_cli.$suffix" "$(say 'CLI state' 'CLI 状态文件')"; done
    fi
  fi
  if ((keep_data)); then
    uninstall_kept+=("$(say Configuration 配置): $item_config")
    [[ -z "$item_data" ]] || uninstall_kept+=("$(say Data 数据): $item_data")
    [[ -z "$item_backup" || ! -d "$item_backup" ]] || uninstall_kept+=("$(say 'Upgrade backups' 升级备份): $item_backup")
  else
    plan_remove_file "$item_config" "$(say Configuration 配置)"
    uninstall_actions+=("rmdir	$config_parent")
    if [[ -n "$item_data" && -d "$item_data" ]]; then
      if [[ -L "$item_data" ]] || protected_directory "$item_data" || contains_other_installation "$item_data" || ! owned_data_directory "$item_data" "$item_role"; then
        uninstall_kept+=("$(say Data 数据): $item_data ($(say 'not removed automatically: it holds other files or is a shared directory; check and delete it yourself' '未自动删除：其中有其他文件或是共享目录，请检查后手动删除'))")
      else
        uninstall_actions+=("tree	$item_data")
        uninstall_removed+=("$(say Data 数据): $item_data")
      fi
    fi
    if [[ -n "$item_backup" && -d "$item_backup" && ! -L "$item_backup" ]]; then
      uninstall_actions+=("backups	$item_backup	$item_service" "rmdir	$item_backup")
      uninstall_removed+=("$(say 'Upgrade backups' 升级备份): $item_backup/$item_service-YYYYMMDDTHHMMSSZ.*")
    fi
  fi
  uninstall_actions+=("rmdir	$directory")
  if [[ -n "$item_root" && "$directory" == "$item_root/$item_role/bin" ]]; then
    uninstall_actions+=("rmdir	$item_root/$item_role" "rmdir	$item_root")
  fi
}
# Per-user CLI logins and preferences, removed with the last installation.
plan_cli_sessions() {
  local home user base
  for user in root "${SUDO_USER:-}"; do
    [[ "$user" =~ ^[A-Za-z_][A-Za-z0-9_.-]{0,31}$ ]] || continue
    if [[ "$user" == root ]]; then home=${HOME:-}
    elif command -v getent >/dev/null 2>&1; then home=$(getent passwd "$user" | cut -d: -f6)
    elif [[ "$platform" == darwin ]]; then home=$(dscl . -read "/Users/$user" NFSHomeDirectory 2>/dev/null | awk '{print $2}')
    else home=; fi
    [[ "$home" == /* && "$home" != / ]] || continue
    if [[ "$platform" == darwin ]]; then base=$home/Library/Application\ Support/oss-transfer-cli; else base=$home/.config/oss-transfer-cli; fi
    [[ -d "$base" && ! -L "$base" ]] || continue
    listed_in "tree	$base" ${uninstall_actions[@]+"${uninstall_actions[@]}"} && continue
    uninstall_actions+=("tree	$base")
    uninstall_removed+=("$(say 'CLI logins and preferences' 'CLI 登录会话和偏好设置'): $base")
  done
}
stop_and_remove_service() {
  local name=$1 definition=$2 domain=system
  if [[ "$platform" == linux ]]; then
    systemctl stop "$name" || return 1
    systemctl disable "$name" >/dev/null 2>&1 || true
    rm -f -- "$definition"
    systemctl daemon-reload || true
    systemctl reset-failed "$name" >/dev/null 2>&1 || true
  elif [[ "$platform" == freebsd ]]; then
    if "$definition" onestatus >/dev/null 2>&1; then "$definition" onestop || return 1; fi
    rm -f -- "$definition"
    for pidfile in "$pid_dir/$name.pid" "$pid_dir/$name.supervisor.pid"; do
      [[ ! -f "$pidfile" || -L "$pidfile" ]] || rm -f -- "$pidfile"
    done
  else
    ((user_service == 0)) || domain=gui/$(id -u)
    launchctl bootout "$domain/$name" 2>/dev/null || ! launchctl print "$domain/$name" >/dev/null 2>&1 || return 1
    rm -f -- "$definition"
  fi
}
run_uninstall() {
  local directory answer item kind first second leftover suffix
  local -a leftovers=()
  if [[ -n "$install_dir" ]]; then
    safe_path "$install_dir"
    [[ -f "$install_dir/install.tsv" && ! -L "$install_dir/install.tsv" ]] || fail "$(say "No installation record in $install_dir" "$install_dir 中没有安装记录")"
    candidates=("$install_dir")
  else
    discover_installations "$role" "$service_name"
  fi
  uninstall_targets=(${candidates[@]+"${candidates[@]}"})
  if ((${#uninstall_targets[@]} == 0)); then
    say 'No installation made by this installer was found; nothing to uninstall. Use --root-dir, --install-dir or (macOS) --user for a custom installation.' '未找到由安装脚本安装的服务，无需卸载。自定义位置的安装请指定 --root-dir、--install-dir，macOS 用户服务请加 --user。'
    return 0
  fi
  # Every other installation counts, wherever it is, when deciding what is shared.
  local selected_root=$root_explicit
  root_explicit=0
  discover_installations '' ''
  root_explicit=$selected_root
  for directory in ${candidates[@]+"${candidates[@]}"}; do
    listed_in "$directory" "${uninstall_targets[@]}" || uninstall_remaining+=("$directory")
  done
  for directory in "${uninstall_targets[@]}"; do plan_uninstall "$directory"; done
  if ((keep_data == 0 && ${#uninstall_remaining[@]} == 0)); then plan_cli_sessions; fi
  say 'Installations to uninstall:' '将卸载以下安装：'
  for directory in "${uninstall_targets[@]}"; do
    printf '  %s (%s): %s\n' "$(metadata_field "$directory/install.tsv" serviceName)" "$(metadata_field "$directory/install.tsv" role)" "$directory"
  done
  say 'Will remove:' '将删除：'
  for item in ${uninstall_removed[@]+"${uninstall_removed[@]}"}; do printf '  %s\n' "$item"; done
  if ((${#uninstall_kept[@]})); then
    say 'Will keep:' '将保留：'
    for item in "${uninstall_kept[@]}"; do printf '  %s\n' "$item"; done
  fi
  if ((yes == 0)); then
    { : </dev/tty; } 2>/dev/null || fail "$(say 'Add --yes to uninstall without a prompt' '无交互终端，请加 --yes 确认卸载')"
    read -r -p "$(say 'Uninstall? [y/N] ' '确认卸载？[y/N] ')" answer </dev/tty
    [[ "$answer" == y || "$answer" == Y ]] || { say 'Cancelled; nothing was changed.' '已取消，未做任何更改。'; return 0; }
  fi
  # Stop every service before removing any file, so a failure leaves a
  # complete installation behind.
  for item in ${uninstall_services[@]+"${uninstall_services[@]}"}; do
    IFS=$'\t' read -r first second <<< "$item"
    printf "$(say 'Stopping %s...' '正在停止 %s…')\n" "$first"
    stop_and_remove_service "$first" "$second" || fail "$(say "Cannot stop $first; its files were kept. Stop it and retry." "无法停止 ${first}，已保留其文件。请停止该服务后重试。")"
  done
  for item in ${uninstall_actions[@]+"${uninstall_actions[@]}"}; do
    IFS=$'\t' read -r kind first second <<< "$item"
    case "$kind" in
      file) rm -f -- "$first";;
      tree) rm -rf -- "$first";;
      backups)
        # Only this service's own backups: <service>-<UTC timestamp>.<random>,
        # exactly as created below. A prefix alone would also match a service
        # whose name extends this one (oss-transfer-agent-b).
        for leftover in "$first/$second-"*; do
          [[ -d "$leftover" && ! -L "$leftover" ]] || continue
          suffix=${leftover##*/}; suffix=${suffix#"$second-"}
          [[ "$suffix" =~ ^[0-9]{8}T[0-9]{6}Z\.[A-Za-z0-9]+$ ]] && rm -rf -- "$leftover"
        done;;
      rmdir)
        if [[ -d "$first" && ! -L "$first" ]]; then
          if [[ -z $(ls -A "$first") ]]; then rmdir -- "$first"
          elif ! listed_in "$first" ${leftovers[@]+"${leftovers[@]}"}; then leftovers+=("$first"); fi
        fi;;
    esac
  done
  for leftover in ${leftovers[@]+"${leftovers[@]}"}; do
    [[ ! -d "$leftover" ]] || printf "$(say 'Kept (contains other files): %s' '已保留（目录中还有其他文件）：%s')\n" "$leftover"
  done
  printf '\n=== %s ===\n' "$(say 'Uninstalled' '卸载完成')"
  ((keep_data == 0)) || say 'Configuration and data were kept; rerun the installer with the same --role (and --root-dir) to reinstall with them.' '已保留配置和数据；使用相同的 --role（及 --root-dir）重新运行安装脚本即可沿用。'
  ((uninstall_agent == 0)) || say 'If the node is still listed on the Master, remove it there: Nodes page, or oss-transfer-cli agent remove NODE --yes.' '如果 Master 上仍显示此节点，请在 Master 上移除：执行节点页面，或运行 oss-transfer-cli agent remove 节点 --yes。'
  ((uninstall_master == 0)) || say 'Agents that joined this Master can no longer reach it; uninstall them on their hosts with --uninstall --role agent.' '加入此 Master 的执行节点将无法再连接它；请在各自主机上运行安装脚本 --uninstall --role agent 卸载。'
}
# END uninstall
root_explicit=0; [[ -z "$root_dir" ]] || root_explicit=1
root_dir=${root_dir:-$(default_root)}
if ((uninstall)); then
  safe_path "$root_dir"
  run_uninstall
  exit 0
fi
if ((upgrade == 0)); then role=${role:-master}; fi
safe_path "$root_dir"
# A normal rerun also recognizes an installation whose binary or service was
# removed. Prefer saved paths; selecting a new root never moves existing data.
if [[ -n "$role" && -z "$adopt_config" ]]; then
  if [[ -z "$install_dir" ]]; then
    for candidate in "$root_dir/$role/bin" "$root_dir/$role"; do
      if [[ -f "$candidate/install.tsv" && ! -L "$candidate/install.tsv" ]]; then
        [[ -z "$service_name" || $(metadata_field "$candidate/install.tsv" serviceName) == "$service_name" ]] || continue
        install_dir=$candidate; break
      fi
    done
    if [[ -z "$install_dir" && "$platform" == linux && "$root_explicit" == 0 && -f "/opt/oss-transfer-go/$role/install.tsv" && ! -L "/opt/oss-transfer-go/$role/install.tsv" ]]; then
      if [[ -z "$service_name" || $(metadata_field "/opt/oss-transfer-go/$role/install.tsv" serviceName) == "$service_name" ]]; then install_dir=/opt/oss-transfer-go/$role; fi
    fi
    if [[ -z "$install_dir" ]]; then
      if [[ "$platform" == linux ]]; then legacy_bin=/opt/oss-transfer/$role; legacy_config=/etc/oss-transfer/$role
      elif [[ "$platform" == freebsd ]]; then legacy_bin=/usr/local/lib/oss-transfer/$role; legacy_config=/usr/local/etc/oss-transfer/$role
      elif ((user_service)); then legacy_bin="$HOME/Library/Application Support/OSS Transfer/$role/bin"; legacy_config="$HOME/Library/Application Support/OSS Transfer/$role/config"
      else legacy_bin=/usr/local/lib/oss-transfer/$role; legacy_config="/Library/Application Support/OSS Transfer/$role/config"; fi
      if ((root_explicit == 0)) && [[ -f "$legacy_bin/install.tsv" && ! -L "$legacy_bin/install.tsv" && ( -z "$service_name" || $(metadata_field "$legacy_bin/install.tsv" serviceName) == "$service_name" ) ]]; then install_dir=$legacy_bin
      elif [[ -f "$root_dir/$role/config/service.json" && ! -L "$root_dir/$role/config/service.json" ]]; then
        install_dir=$root_dir/$role/bin; config_dir=${config_dir:-$root_dir/$role/config}
      elif ((root_explicit == 0)) && [[ -f "$legacy_config/service.json" && ! -L "$legacy_config/service.json" && ( -z "$service_name" || $(config_field "$legacy_config/service.json" serviceName) == "$service_name" ) ]]; then
        install_dir=$legacy_bin; config_dir=${config_dir:-$legacy_config}
      fi
    fi
  fi
  if [[ -n "$install_dir" && -f "$install_dir/install.tsv" && ! -L "$install_dir/install.tsv" ]]; then
    upgrade=1
  elif [[ -n "$config_dir" && -f "$config_dir/service.json" && ! -L "$config_dir/service.json" ]]; then
    install_dir=${install_dir:-$root_dir/$role/bin}
    upgrade=1
  fi
fi
trusted_master_release() {
  [[ "$role" == agent && -n "$master_url" ]] || return 1
  local origin=${master_url%/} suffix
  [[ "$origin" =~ ^https?://(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)(:[0-9]{1,5})?$ ]] || return 1
  [[ "$release_url" == "$origin"/api/agents/bootstrap/*/files ]] || return 1
  suffix=${release_url#"$origin"/api/agents/bootstrap/}; suffix=${suffix%/files}
  [[ "$suffix" =~ ^[A-Za-z0-9_-]+$ ]]
}
if [[ -z "$release_url" && -z "$package_dir" ]]; then
  # A copied offline bundle beside this installer (or in the current
  # directory) is used directly and never triggers a download.
  if [[ "$source" == auto ]] && package_dir=$(transfer_local_package_dir "${BASH_SOURCE[0]:-}" "$platform" "$arch"); then
    local_detected=1
    printf '%s: %s\n' "$(say 'Using local release files (offline)' '使用本地安装包目录（离线）')" "$package_dir"
  else
    package_dir=
    selected_source=$(transfer_select_source "$source" "$requested_version") || fail 'Cannot select a download source; use --source or --release-url'
    printf '%s: %s\n' "$(say 'Download source' '下载源')" "$selected_source"
    if [[ "$selected_source" == github ]]; then
      source_stage=$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-download.XXXXXX")
      trap 'rm -rf -- "$source_stage"' EXIT
      transfer_github_prepare "$requested_version" "$platform" "$arch" "$source_stage" || fail 'GitHub release verification failed'
      # Bash before 4.4 treats an empty "${array[@]}" as unset under set -u.
      bash "${BASH_SOURCE[0]}" ${original_args[@]+"${original_args[@]}"} --package-dir "$source_stage"
      exit 0
    fi
    release_part=$requested_version; [[ "$requested_version" == latest ]] || release_part=v$requested_version
    release_url=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/$release_part
  fi
fi
# An archive copied from a GitHub Release has no manifest.tsv beside it. Build
# one from the archive's internal checksums without contacting any server.
if [[ -n "$package_dir" && ! -e "$package_dir/manifest.tsv" ]]; then
  [[ -d "$package_dir" ]] || fail "$(say "Package directory not found: $package_dir" "安装包目录不存在：$package_dir")"
  local_stage=$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-local.XXXXXX")
  trap 'rm -rf -- "$local_stage"' EXIT
  transfer_local_prepare "$package_dir" "$platform" "$arch" "$local_stage" native-install.sh "${BASH_SOURCE[0]:-}" || fail "$(say 'Offline package verification failed' '离线安装包校验失败')"
  say "No manifest.tsv found; verified $package_dir/oss-transfer-$platform-$arch.tar.gz with its internal checksums." "未找到 manifest.tsv，已按压缩包内置校验和验证 $package_dir/oss-transfer-$platform-$arch.tar.gz。"
  package_dir=$local_stage
fi
if ((upgrade)); then
  if [[ -z "$install_dir" ]]; then
    discover_installations "$role" "$service_name"
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
      curl -fsSL --globoff --proto "=$protocol" --proto-redir "=$protocol" --connect-timeout 10 --max-time 600 --retry 2 "$release_url/manifest.tsv" -o "$pin_work/latest.tsv"
      [[ $(wc -c < "$pin_work/latest.tsv") -lt 65536 && $(metadata_field "$pin_work/latest.tsv" format) == oss-transfer-go-v1 ]] || fail 'Invalid release manifest'
      pinned_version=$(metadata_field "$pin_work/latest.tsv" version)
      [[ "$pinned_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'Invalid latest release version'
      release_url="${release_url%/latest}/v$pinned_version"
      curl -fsSL --globoff --proto "=$protocol" --proto-redir "=$protocol" --connect-timeout 10 --max-time 600 --retry 2 "$release_url/manifest.tsv" -o "$pin_work/versioned.tsv"
      [[ $(sha256 "$pin_work/latest.tsv") == "$(sha256 "$pin_work/versioned.tsv")" ]] || fail 'Latest and versioned release manifests differ; retry after publication finishes'
    fi
    for directory in "${candidates[@]}"; do
      if [[ -n "$release_url" ]]; then
        bash "${BASH_SOURCE[0]}" ${original_args[@]+"${original_args[@]}"} --install-dir "$directory" --release-url "$release_url" --yes
      else
        # Pass the resolved local directory so each child stays offline.
        bash "${BASH_SOURCE[0]}" ${original_args[@]+"${original_args[@]}"} --install-dir "$directory" ${package_dir:+--package-dir "$package_dir"} --yes
      fi
    done
    exit 0
  fi
  installed_metadata=$install_dir/install.tsv
  saved_root_dir=
  if [[ -f "$installed_metadata" && ! -L "$installed_metadata" ]]; then
    saved_role=$(metadata_field "$installed_metadata" role)
    [[ "$saved_role" == master || "$saved_role" == agent ]] || fail 'Invalid installed role'
    [[ -z "$role" || "$role" == "$saved_role" ]] || fail 'Installed role differs from --role'
    role=$saved_role
    service_name=${service_name:-$(metadata_field "$installed_metadata" serviceName)}
    saved_config=$(metadata_field "$installed_metadata" config)
    saved_data=$(metadata_field "$installed_metadata" dataDir)
    saved_root_dir=$(metadata_field "$installed_metadata" rootDir)
  else
    [[ -n "$role" && -n "$config_dir" ]] || fail 'No installation configuration found'
    saved_config=$config_dir/service.json
    saved_data=
  fi
  [[ "$saved_config" == /*/service.json && -f "$saved_config" && ! -L "$saved_config" ]] || fail 'Existing service configuration is missing or symlinked'
  config_dir=${config_dir:-$(dirname "$saved_config")}
  safe_path "$install_dir"
  safe_path "$(dirname "$saved_config")"
  installed_binary=$install_dir/oss-transfer-$role
  [[ ! -L "$installed_binary" && ( ! -e "$installed_binary" || -f "$installed_binary" ) ]] || fail 'Existing program is a link or non-file'
  if [[ -f "$installed_binary" ]]; then
    saved_data=$("$installed_binary" --config "$saved_config" --check-config | awk -F '\t' '$1=="dataDirectory"{print $2}')
  elif [[ -z "$saved_data" ]]; then
    saved_data=$(config_field "$saved_config" dataDirectory)
  fi
  service_name=${service_name:-$(config_field "$saved_config" serviceName)}
  [[ -n "$service_name" && -n "$saved_data" ]] || fail 'Existing configuration must contain serviceName and dataDirectory'
  data_dir=${data_dir:-$saved_data}
  # Only installations explicitly recorded as a unified layout retain their
  # root during automatic discovery. Older backupDir values may point at
  # temporary shared storage and are deliberately not inherited.
  if ((root_explicit == 0)) && [[ -n "$saved_root_dir" && "$install_dir" == "$saved_root_dir/$role/bin" && "$config_dir" == "$saved_root_dir/$role/config" && "$data_dir" == "$saved_root_dir/$role/data" ]]; then
    safe_path "$saved_root_dir"
    root_dir=$saved_root_dir
  fi
fi
role=${role:-master}
if [[ "$platform" == linux && "${root_explicit:-0}" == 0 && -z "$install_dir" && "$upgrade" == 0 && -f "/opt/oss-transfer-go/$role/install.tsv" ]]; then
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
  install_dir=${install_dir:-$root_dir/$role/bin}
  config_dir=${config_dir:-$root_dir/$role/config}
  data_dir=${data_dir:-$root_dir/$role/data}
  if [[ "$install_dir" == "$root_dir/$role/bin" ]]; then backup_dir=${backup_dir:-$root_dir/$role/backups}; else backup_dir=${backup_dir:-/var/backups/oss-transfer/$role}; fi
  unit=/etc/systemd/system/$service_name.service
elif [[ "$platform" == freebsd ]]; then
  [[ -f /etc/rc.subr && -x /usr/sbin/daemon ]] || fail 'FreeBSD rc.d and daemon are required'
  install_dir=${install_dir:-$root_dir/$role/bin}
  config_dir=${config_dir:-$root_dir/$role/config}
  data_dir=${data_dir:-$root_dir/$role/data}
  if [[ "$install_dir" == "$root_dir/$role/bin" ]]; then backup_dir=${backup_dir:-$root_dir/$role/backups}; else backup_dir=${backup_dir:-/var/backups/oss-transfer/$role}; fi
  unit=/usr/local/etc/rc.d/$service_name
  rc_name=$service_name
  supervisor_pidfile=/var/run/$service_name.supervisor.pid
  child_pidfile=/var/run/$service_name.pid
else
  command -v launchctl >/dev/null || fail 'launchd is required'
  if ((user_service)); then
    base="$root_dir/$role"
    install_dir=${install_dir:-$base/bin}
    unit="$HOME/Library/LaunchAgents/$service_name.plist"
    domain=gui/$(id -u)
  else
    base="$root_dir/$role"
    install_dir=${install_dir:-$base/bin}
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
global_cli=/usr/local/bin/oss-transfer-cli
if [[ "$platform" == darwin && "$user_service" == 1 ]]; then global_cli=$HOME/.local/bin/oss-transfer-cli; fi
previous_cli=''
if [[ -f "$metadata" && ! -L "$metadata" ]]; then previous_cli=$(metadata_field "$metadata" cliPath); fi

cli_belongs_to_data() {
  local cli=$1 path
  [[ -f "$config" && ! -L "$config" ]] || return 1
  for path in "$cli.cluster.json" "$cli.local-auth.json" "$data_dir/cli-cluster.json" "$data_dir/cli-local-auth.json"; do
    [[ -f "$path" && ! -L "$path" ]] || return 1
  done
  [[ $(config_field "$data_dir/cli-local-auth.json" role) == "$role" ]] || return 1
  cmp -s "$cli.cluster.json" "$data_dir/cli-cluster.json" && cmp -s "$cli.local-auth.json" "$data_dir/cli-local-auth.json"
}
if [[ -z "$previous_cli" ]] && cli_belongs_to_data "$global_cli"; then previous_cli=$global_cli; fi

default_cli_path() {
  local installed=$1
  if [[ ! -e "$global_cli" && ! -L "$global_cli" && ! -e "$global_cli.cluster.json" && ! -L "$global_cli.cluster.json" && ! -e "$global_cli.local-auth.json" && ! -L "$global_cli.local-auth.json" ]]; then
    if [[ -z "$installed" || "$role" == agent && "$installed" == "$install_dir/oss-transfer-cli" ]]; then
      printf '%s\n' "$global_cli"
      return
    fi
  fi
  printf '%s\n' "${installed:-$install_dir/oss-transfer-cli}"
}
cli_path=${cli_path:-$(default_cli_path "$previous_cli")}
safe_path "$(dirname "$cli_path")"
[[ "$cli_path" != *[$'\n\r\t"\\%$`']* && "$(basename "$cli_path")" == oss-transfer-cli ]] || fail 'CLI path must end in oss-transfer-cli'
for path in "$config_dir" "$data_dir" "$backup_dir"; do
  [[ "$cli_path" != "$path" && "$cli_path" != "$path/"* ]] || fail 'CLI must be outside configuration, data and backup directories'
done
[[ ! -L "$cli_path" && ( ! -e "$cli_path" || -f "$cli_path" ) ]] || fail 'CLI path must be a regular file'
for path in "$cli_path.cluster.json" "$cli_path.local-auth.json"; do
  [[ ! -L "$path" && ( ! -e "$path" || -f "$path" ) ]] || fail "CLI connection must be a regular file: $path"
  if [[ -f "$path" ]]; then had_cli_sidecars=1; fi
done
for path in "$data_dir/cli-cluster.json" "$data_dir/cli-local-auth.json"; do
  [[ ! -L "$path" && ( ! -e "$path" || -f "$path" ) ]] || fail "CLI connection must be a regular file: $path"
  if [[ -f "$path" ]]; then had_data_cli_connections=1; fi
done
existed=0
if [[ -f "$metadata" && ! -L "$metadata" ]]; then
  [[ -z "$adopt_config" ]] || fail 'Existing installation found; rerun without --adopt-config'
  existed=1
  [[ $(awk -F '\t' '$1=="role"{print $2}' "$metadata") == "$role" && $(awk -F '\t' '$1=="serviceName"{print $2}' "$metadata") == "$service_name" && $(awk -F '\t' '$1=="config"{print $2}' "$metadata") == "$config" && $(awk -F '\t' '$1=="unit"{print $2}' "$metadata") == "$unit" ]] || fail 'Existing installation identity does not match'
  [[ ! -L "$binary" && ( ! -e "$binary" || -f "$binary" ) && -f "$config" && ! -L "$config" ]] || fail 'Existing installation configuration is missing or symlinked'
elif [[ -z "$adopt_config" && -f "$config" && ! -L "$config" ]]; then
  existed=1
  [[ ! -L "$binary" && ( ! -e "$binary" || -f "$binary" ) ]] || fail 'Existing program is a link or non-file'
  say 'Existing configuration found; preserving its data.' '已发现原有配置，将保留现有数据。'
  # A leftover service is accepted only if it names these exact program and
  # config paths. An unrelated service must never be stopped by recovery.
  if [[ -f "$unit" ]]; then
    if [[ "$platform" == linux ]]; then
      grep -Fx "ExecStart=\"$binary\" --config \"$config\"" "$unit" >/dev/null || fail 'Existing service does not match this installation'
    elif [[ "$platform" == darwin ]]; then
      [[ $(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:0' "$unit") == "$binary" && $(/usr/libexec/PlistBuddy -c 'Print :ProgramArguments:2' "$unit") == "$config" ]] || fail 'Existing service does not match this installation'
    else
      grep -Fx "# oss-transfer-install: $install_dir" "$unit" >/dev/null || fail 'Existing service does not match this installation'
    fi
  fi
else
  ((upgrade == 0)) || fail 'No managed installation found; V4.0.x requires database migration first'
  for path in "$install_dir" "$config_dir" "$data_dir"; do
    [[ -n "$adopt_config" && "$path" == "$data_dir" ]] && continue
    [[ ! -e "$path" || -z $(ls -A "$path") ]] || fail "Refusing nonempty directory without its service.json: $path"
  done
  [[ ! -e "$unit" && ! -L "$unit" ]] || fail "Service already exists: $service_name"
fi
partial_backup=0
if ((existed)) && [[ ! -f "$binary" || ! -f "$metadata" ]]; then partial_backup=1; fi
if [[ -f "$cli_path" ]]; then had_cli=1; fi
if ((had_cli || had_cli_sidecars)); then
  if ((existed == 0)) || [[ ! -f "$metadata" ]] || [[ $(awk -F '\t' '$1=="cliPath"{print $2}' "$metadata") != "$cli_path" ]]; then
    if ! ((existed)) || ! cli_belongs_to_data "$cli_path"; then
      ((replace_cli)) || fail "CLI already exists: $cli_path; use --replace-cli to back it up and replace it, or choose --cli-path"
    fi
  fi
fi
if ((existed)); then
  if [[ -n "$previous_cli" && "$previous_cli" != "$cli_path" ]]; then
    # Only move this Agent's own CLI into an unused global slot. In particular,
    # an existing Master's CLI is never adopted or replaced by an Agent upgrade.
    [[ "$role" == agent && "$previous_cli" == "$install_dir/oss-transfer-cli" && "$cli_path" == "$global_cli" && ! -e "$cli_path" && ! -L "$cli_path" && ! -e "$cli_path.cluster.json" && ! -L "$cli_path.cluster.json" && ! -e "$cli_path.local-auth.json" && ! -L "$cli_path.local-auth.json" ]] || fail 'CLI relocation requires this Agent own CLI and an unused global path'
    [[ -f "$previous_cli" && ! -L "$previous_cli" ]] || fail 'Installed CLI is missing or symlinked'
    for path in "$previous_cli.cluster.json" "$previous_cli.local-auth.json"; do
      [[ ! -L "$path" && ( ! -e "$path" || -f "$path" ) ]] || fail "CLI connection must be a regular file: $path"
    done
    cli_migrated=1
  fi
fi
[[ ! -L "$unit" ]] || fail 'Service definition is a symlink'
if [[ "$platform" == linux ]]; then
  # A failed query must not be reported as an existing unmanaged service.
  load_state=$(systemd_property "$service_name" LoadState) || fail "Cannot query systemd for $service_name; check that systemd is running"
  if ((existed == 0)) && [[ "$load_state" != not-found ]]; then fail 'An unmanaged service already exists'; fi
  if ((existed)) && [[ -n $(systemd_property "$service_name" DropInPaths) ]]; then fail 'Review and remove service overrides before upgrading'; fi
elif [[ "$platform" == darwin ]]; then
  if ((existed == 0)) && launchctl print "$domain/$service_name" >/dev/null 2>&1; then fail 'An unmanaged launchd service already exists'; fi
fi
work=$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-install.XXXXXX")
backup=
owned_backup=
was_active=0
agent_maintenance_started=0
enrollment_requested=0
enrollment_attempted=0
was_enabled=0
had_unit=0
[[ ! -f "$unit" ]] || had_unit=1
cleanup() {
  local result=$?
  rm -rf -- "$work"
  [[ -z "${local_stage:-}" ]] || rm -rf -- "$local_stage"
  if [[ -n "$backup" && -d "$backup" ]]; then printf 'Backup retained: %s\n' "$backup" >&2; fi
  return "$result"
}
trap cleanup EXIT
fetch() {
  local name=$1 protocol=https
  if [[ "$release_url" == http://* ]]; then protocol=http,https; fi
  if [[ -n "$package_dir" ]]; then
    [[ -f "$package_dir/$name" && ! -L "$package_dir/$name" ]] || fail "Release file missing: $name"
    cp "$package_dir/$name" "$work/$name"
  else
    # An invited Master may be an IPv6 literal; curl 7.29 would glob its brackets.
    curl --fail --silent --show-error --location --globoff --proto "=$protocol" --proto-redir "=$protocol" --connect-timeout 10 --max-time 600 --retry 2 "$release_url/$name" -o "$work/$name"
  fi
}
if [[ -n "$release_url" ]]; then
  release_url=${release_url%/}
  [[ "$release_url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._~/-]+$ || "$release_url" =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]+)?/[A-Za-z0-9._~/-]+$ ]] || trusted_master_release || fail 'Release URL must use HTTPS or the invited Master'
fi
fetch manifest.tsv
[[ $(wc -c < "$work/manifest.tsv") -lt 65536 ]] || fail 'Manifest too large'
[[ $(awk -F '\t' '$1=="format"{print $2}' "$work/manifest.tsv") == oss-transfer-go-v1 ]] || fail 'Unknown release format'
version=$(awk -F '\t' '$1=="version"{print $2}' "$work/manifest.tsv")
[[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$ ]] || fail 'Invalid release version'
if ((local_detected)) && [[ "$requested_version" != latest && "$requested_version" != "$version" ]]; then
  fail "$(say "Local release files are version $version, not $requested_version; remove --version or use --source to download" "本地安装包版本为 ${version}，不是 ${requested_version}；请去掉 --version，或使用 --source 在线下载")"
fi
if [[ "$release_url" == */latest ]]; then
  [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail 'Invalid latest release version'
  latest_manifest_sha=$(sha256 "$work/manifest.tsv")
  release_url="${release_url%/latest}/v$version"
  fetch manifest.tsv
  [[ $(sha256 "$work/manifest.tsv") == "$latest_manifest_sha" ]] || fail 'Latest and versioned release manifests differ; retry after publication finishes'
fi
# Rerunning the same release over a complete, running installation changes
# nothing. Anything missing or stale, an enrollment, adoption or CLI change
# still runs the installation, which repairs it.
installation_current() {
  local installed_cli_version
  ((existed && force == 0 && replace_cli == 0 && cli_migrated == 0 && token_stdin == 0)) || return 1
  [[ -z "$adopt_config" && -z "${TRANSFER_JOIN_TOKEN:-}" ]] || return 1
  [[ -f "$metadata" && ! -L "$metadata" && -f "$binary" && ! -L "$binary" && -f "$unit" && ! -L "$unit" && -f "$cli_path" && ! -L "$cli_path" ]] || return 1
  [[ $(metadata_field "$metadata" version) == "$version" && $(metadata_field "$metadata" cliPath) == "$cli_path" ]] || return 1
  [[ $("$binary" --version 2>/dev/null) == "oss-transfer-$role $version "* ]] || return 1
  installed_cli_version=$("$cli_path" --version --json 2>/dev/null | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') || return 1
  [[ "$installed_cli_version" == "$version" ]] || return 1
  if [[ "$platform" == linux ]]; then systemctl is-active --quiet "$service_name" || return 1
  elif [[ "$platform" == freebsd ]]; then "$unit" onestatus >/dev/null 2>&1 || return 1
  else launchctl print "$domain/$service_name" >/dev/null 2>&1 || return 1; fi
  # A Master also keeps the release's Agent packages.
  if [[ "$role" == master ]] && [[ $(awk -F '\t' '$1=="asset" && ($2=="install.sh" || $2=="native-install.sh" || $2=="Install.ps1") {print $2}' "$work/manifest.tsv" | LC_ALL=C sort -u | wc -l | tr -d ' ') == 3 ]]; then
    [[ -f "$data_dir/packages/manifest.tsv" && ! -L "$data_dir/packages/manifest.tsv" && $(metadata_field "$data_dir/packages/manifest.tsv" version) == "$version" ]] || return 1
  fi
}
if installation_current; then
  if ((yes == 0)) && { : </dev/tty; } 2>/dev/null; then
    read -r -p "$(say "$service_name v$version is already installed and running. Reinstall it anyway? [y/N] " "$service_name v$version 已安装且正在运行。仍要重新安装吗？[y/N] ")" answer </dev/tty
    if [[ "$answer" != y && "$answer" != Y ]]; then
      say "Nothing was changed." "未做任何更改。"
      exit 0
    fi
  else
    say "$service_name is already v$version and running; nothing was changed. Add --force to reinstall it." "$service_name 已是 v$version 且正在运行，未做任何更改。如需重新安装，请加 --force。"
    exit 0
  fi
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
# Keep a complete, verified installer set on the Master. Older release layouts
# without all three installers remain usable through the upstream join route.
master_packages=
if [[ "$role" == master && -n "${archive_name:-}" ]]; then
  package_assets=$(awk -F '\t' '$1=="asset" && ($2=="install.sh" || $2=="native-install.sh" || $2=="Install.ps1") {print $2}' "$work/manifest.tsv" | LC_ALL=C sort -u | wc -l | tr -d ' ')
  if [[ "$package_assets" == 3 ]]; then
    master_packages=$work/master-packages
    mkdir "$master_packages"
    cp "$work/manifest.tsv" "$work/$archive_name" "$master_packages/"
    for asset in install.sh native-install.sh Install.ps1; do
      record=$(awk -F '\t' -v name="$asset" '$1=="asset" && $2==name {if(NF!=4) print "invalid"; else print $3 "\t" $4}' "$work/manifest.tsv")
      IFS=$'\t' read -r asset_checksum asset_length <<< "$record"
      [[ "$asset_checksum" =~ ^[a-f0-9]{64}$ && "$asset_length" =~ ^[0-9]+$ && "$record" != *$'\n'* ]] || fail "Invalid installer manifest entry: $asset"
      fetch "$asset"
      [[ $(wc -c < "$work/$asset" | tr -d ' ') == "$asset_length" && $(sha256 "$work/$asset") == "$asset_checksum" ]] || fail "Installer checksum mismatch: $asset"
      cp "$work/$asset" "$master_packages/$asset"
    done
    # Offline bundles may include additional platforms. Retain only archives
    # named by this exact manifest, and verify each one before publishing it.
    if [[ -n "$package_dir" ]]; then
      while IFS=$'\t' read -r kind asset_os asset_arch asset checksum length extra; do
        [[ "$kind" == archive && "$asset" != "$archive_name" ]] || continue
        [[ "$asset_os" =~ ^[a-z0-9]+$ && "$asset_arch" =~ ^[a-z0-9]+$ && ( "$asset" == "oss-transfer-$asset_os-$asset_arch.tar.gz" || "$asset" == "oss-transfer-$asset_os-$asset_arch.zip" ) && -z "$extra" && "$checksum" =~ ^[a-f0-9]{64}$ && "$length" =~ ^[0-9]+$ ]] || fail 'Invalid offline platform archive entry'
        [[ -e "$package_dir/$asset" || -L "$package_dir/$asset" ]] || continue
        [[ -f "$package_dir/$asset" && ! -L "$package_dir/$asset" ]] || fail "Offline archive must be a regular file: $asset"
        [[ $(wc -c < "$package_dir/$asset" | tr -d ' ') == "$length" && $(sha256 "$package_dir/$asset") == "$checksum" ]] || fail "Offline archive checksum mismatch: $asset"
        cp "$package_dir/$asset" "$master_packages/$asset"
      done < "$work/manifest.tsv"
    fi
  fi
fi
publish_master_packages() {
  [[ -n "$master_packages" ]] || return 0
  local destination=$data_dir/packages staged previous
  [[ ! -L "$destination" && ( ! -e "$destination" || -d "$destination" ) ]] || return 1
  staged=$(mktemp -d "$data_dir/.packages-new.XXXXXX") || return 1
  chmod 700 "$staged"
  if ! cp "$master_packages/"* "$staged/"; then rm -rf -- "$staged"; return 1; fi
  previous=$(mktemp -d "$data_dir/.packages-old.XXXXXX") || { rm -rf -- "$staged"; return 1; }
  rmdir "$previous" || return 1
  if [[ -d "$destination" ]] && ! mv "$destination" "$previous"; then rm -rf -- "$staged"; return 1; fi
  if ! mv "$staged" "$destination"; then
    if [[ -d "$previous" ]]; then mv "$previous" "$destination" || true; fi
    rm -rf -- "$staged"; return 1
  fi
  [[ ! -d "$previous" ]] || rm -rf -- "$previous"
}
[[ $("$work/$filename" --version) == "oss-transfer-$role $version "* ]] || fail 'Binary version or platform check failed'
[[ $("$work/$cli_filename" --version --json | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p') == "$version" ]] || fail 'CLI version or platform check failed'
if [[ "$role" == agent ]]; then
  if ((token_stdin)); then
    IFS= read -r TRANSFER_JOIN_TOKEN || true
    [[ -n "${TRANSFER_JOIN_TOKEN:-}" ]] || fail 'Copy a new Agent join command from Nodes > Add node'
  fi
  if [[ -n "${TRANSFER_JOIN_TOKEN:-}" ]]; then
    [[ -n "$master_url" ]] || fail 'Copy a new Agent join command from Nodes > Add node'
    enrollment_requested=1
    export TRANSFER_JOIN_TOKEN
  fi
fi
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
    [[ -n "$master_url" ]] || fail 'Copy an Agent join command from Nodes > Add node'
    ((enrollment_requested)) || fail 'Copy an Agent join command from Nodes > Add node'
  fi
  if [[ "$role" == master ]]; then listen_json=",\"listen\":\"$listen\""; else listen_json=; fi
  [[ "$listen" != *[$'\n\r\t"\\']* ]] || fail 'Invalid listener'
  printf '{"serviceName":"%s","dataDirectory":"%s","readyFile":"%s/runtime/ready","logFile":"%s/logs/service.log"%s}\n' "$service_name" "$data_dir" "$data_dir" "$data_dir" "$listen_json" > "$work/service.json"
  "$work/$filename" --config "$work/service.json" --check-config > "$work/config.tsv"
fi
ready_file=$(awk -F '\t' '$1=="readyFile"{print $2}' "$work/config.tsv")
log_file=$(awk -F '\t' '$1=="logFile"{print $2}' "$work/config.tsv")
for path in "$ready_file" "$ready_file.request" "$ready_file.committed" "$log_file"; do
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
maintain_backups() {
  local action=$1 result count
  local args=(--backup-maintenance "$action" --backup-dir "$backup_dir" --config "$config")
  if [[ "$action" == complete ]]; then args+=(--backup-entry "$(basename "$backup")"); fi
  if ! result=$(trap - ERR; "$work/$filename" "${args[@]}"); then
    printf 'Warning: could not finish upgrade backup maintenance (%s). Check %s.\n' "$action" "$backup_dir" >&2
    return 1
  fi
  if [[ "$action" == prune ]]; then
    count=$(printf '%s\n' "$result" | sed -n 's/.*"count"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')
    if [[ "$count" =~ ^[0-9]+$ ]] && ((count > 0)); then
      printf "$(say 'Removed %s completed upgrade backup(s).' '已清理 %s 个已完成升级的旧备份。')\n" "$count"
    fi
  fi
}
finish_backup() {
  local path name
  [[ -n "$backup" ]] || return 0
  if ((existed && ${partial_backup:-0} == 0)); then
    if maintain_backups complete; then maintain_backups prune || true; fi
  else
    # CLI replacement and database adoption do not create a complete managed
    # upgrade snapshot. Only remove the directory made by this invocation.
    if [[ "$backup" != "$owned_backup" || "$(dirname "$backup")" != "$backup_dir" || "$backup" != "$backup_dir/$service_name-"* || ! -d "$backup" || -L "$backup" ]]; then
      printf 'Warning: temporary backup path changed; retained %s.\n' "$backup" >&2
      return 0
    fi
    for path in "$backup"/* "$backup"/.[!.]* "$backup"/..?*; do
      [[ -e "$path" || -L "$path" ]] || continue
      name=${path##*/}
      if [[ "$name" != database && "$name" != database.key && "$name" != service.json && "$name" != cli && "$name" != cli.cluster.json && "$name" != cli.local-auth.json && "$name" != data-cli.cluster.json && "$name" != data-cli.local-auth.json && "$name" != binary && "$name" != install.tsv && "$name" != unit || ! -f "$path" || -L "$path" ]]; then
        printf 'Warning: unexpected temporary backup contents; retained %s.\n' "$backup" >&2
        return 0
      fi
    done
    if ! rm -f -- "$backup/database" "$backup/database.key" "$backup/service.json" "$backup/cli" "$backup/cli.cluster.json" "$backup/cli.local-auth.json" "$backup/data-cli.cluster.json" "$backup/data-cli.local-auth.json" "$backup/binary" "$backup/install.tsv" "$backup/unit" || ! rmdir -- "$backup"; then
      printf 'Warning: could not remove temporary backup: %s\n' "$backup" >&2
    fi
  fi
}
check_backup_space() {
  local path size required=65536 available probe
  # Only completed, managed backups are pruned before this check. Unrecognized
  # or failed upgrade backups must not be removed to make room.
  mkdir -p "$backup_dir" || fail "Cannot create backup directory: $backup_dir. The service has not been stopped."
  probe=$(mktemp "$backup_dir/.oss-transfer-write.XXXXXX") || fail "Cannot write to backup directory: $backup_dir. The service has not been stopped."
  if ! printf 'backup preflight\n' > "$probe"; then
    rm -f -- "$probe" || true
    fail "Cannot write to backup directory: $backup_dir. The service has not been stopped."
  fi
  rm -f -- "$probe" || fail "Cannot remove backup directory probe: $backup_dir. The service has not been stopped."
  for path in "$database" "$database.key" "$binary" "$cli_path" "$cli_path.cluster.json" "$cli_path.local-auth.json" "$data_dir/cli-cluster.json" "$data_dir/cli-local-auth.json"; do
    [[ -f "$path" ]] || continue
    size=$(LC_ALL=C ls -ln "$path" | awk '{print $5}')
    [[ "$size" =~ ^[0-9]+$ ]] || fail "Cannot determine backup size: $path"
    required=$((required+(size+1023)/1024))
  done
  available=$(LC_ALL=C df -Pk "$backup_dir" | awk 'END {print $4}')
  [[ "$available" =~ ^[0-9]+$ ]] || fail "Cannot determine available backup space: $backup_dir"
  if ((available < required)); then
    fail "Not enough space for the upgrade backup: $backup_dir needs $(((required+1023)/1024)) MiB; $((available/1024)) MiB available. Free space or choose --backup-dir on another disk. The service has not been stopped."
  fi
}
active() { if [[ "$platform" == linux ]]; then systemctl is-active --quiet "$service_name"; elif [[ "$platform" == freebsd ]]; then "$unit" onestatus >/dev/null 2>&1; else launchctl print "$domain/$service_name" >/dev/null 2>&1; fi; }
stop_service() {
  local old_pid load_state deadline=$((SECONDS+50))
  old_pid=$(service_pid || true)
  if [[ "$platform" == linux ]]; then
    load_state=$(systemd_property "$service_name" LoadState) || return 1
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
  if [[ "$platform" == linux ]]; then systemd_property "$service_name" MainPID; elif [[ "$platform" == freebsd ]]; then if [[ -f "$child_pidfile" && ! -L "$child_pidfile" ]]; then cat "$child_pidfile"; fi; else launchctl print "$domain/$service_name" 2>/dev/null | awk '$1=="pid" && $2=="=" {print $3;exit}'; fi
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
    # CentOS 7.0-7.4 ship iproute 3.10: its ss has no -H, and the oldest
    # builds print users:(("name",PID,FD)) rather than pid=PID.
    ss -ltnpe "sport = :$port" > "$raw" 2> "$errors" || fail "Cannot inspect listening port $port with ss"
    awk '$1=="State" && $2=="Recv-Q" {next} { address=$4; family="unknown"; v6only="unknown"; if (address ~ /^\[/ || /v6only:/) family=6; else if (address ~ /^[0-9]+\./) family=4; if (/v6only:1/) v6only=1; else if (/v6only:0/) v6only=0; owners=0; rest=$0; while (match(rest,/pid=[0-9]+/)) { pid=substr(rest,RSTART+4,RLENGTH-4); print address "\t" pid "\t" family "\t" v6only; rest=substr(rest,RSTART+RLENGTH); owners++ } rest=owners ? "" : $0; while (match(rest,/",[0-9]+,[0-9]+\)/)) { split(substr(rest,RSTART+2,RLENGTH-3),old,","); print address "\t" old[1] "\t" family "\t" v6only; rest=substr(rest,RSTART+RLENGTH); owners++ } if (!owners) print address "\tunknown\t" family "\t" v6only }' "$raw" > "$rows"
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
  if [[ "$platform" == linux ]]; then startup_previous_attempt=$(systemd_property "$service_name" ExecMainStartTimestampMonotonic 2>/dev/null || true); fi
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
        *'enrollment was revoked'*) say 'Startup failed: this Agent was removed from the cluster on the Master. Copy a new join command from Nodes > Add node, or uninstall it with --uninstall --role agent.' '启动失败：此 Agent 已在 Master 上被移除。请从“执行节点 > 添加节点”复制新的加入命令重新加入，或使用 --uninstall --role agent 卸载。' >&2;;
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
  printf "$(say 'Waiting for %s to become ready (up to %ss)...' '正在等待 %s 就绪（最多 %s 秒）…')\n" "$service_name" "$startup_timeout"
  while ((SECONDS < deadline)); do
    pid=$(service_pid || true)
    if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && "$binary" --config "$config" --check-ready --expect-pid "$pid" --expect-launch-id "$launch_id" >/dev/null 2>&1; then return 0; fi
    if startup_failure; then return 1; fi
    if ((SECONDS >= next_notice)); then
      printf "$(say 'Still waiting for %s (%ss elapsed); log: %s' '仍在等待 %s（已等待 %s 秒）；日志: %s')\n" "$service_name" "$((SECONDS-started))" "$log_file"
      next_notice=$((SECONDS+10))
    fi
    sleep 1
  done
  printf "$(say 'Startup timed out after %ss. Check %s and the %s service status.' '启动等待超过 %s 秒。请检查日志 %s 和 %s 服务状态。')\n" "$startup_timeout" "$log_file" "$service_name" >&2
  return 1
}
commit_start() {
  local committed_file="$ready_file.committed" temporary_file=
  # Remote scan copies may be reclaimed only after database rollback is no
  # longer possible. Failure keeps the backup and leaves reclamation paused.
  if [[ "$launch_id" =~ ^[a-f0-9]{64}$ && ! -L "$committed_file" && ( ! -e "$committed_file" || -f "$committed_file" ) ]] &&
     temporary_file=$(mktemp "$committed_file.XXXXXX") &&
     printf '%s\n' "$launch_id" > "$temporary_file" &&
     chmod 600 "$temporary_file" && mv -f "$temporary_file" "$committed_file"; then
    return 0
  fi
  [[ -z "$temporary_file" ]] || rm -f "$temporary_file" || true
  printf 'Warning: could not confirm the installation for scan cleanup; the service remains available.\n' >&2
  return 1
}
print_initial_credentials() {
  [[ "$role" == master && -f "$log_file" ]] || return 0
  local password token
  # Existing logs retain first-install credentials. Only print a password
  # generated during the startup that this installer just verified.
  password=$(tail -n +$((startup_log_lines+1)) "$log_file" |
    sed -n 's/^Initial password: \([A-Za-z0-9_-][A-Za-z0-9_-]*\)$/\1/p' | tail -n 1)
  if [[ -n "$password" ]]; then
    printf '\n%s: admin\n%s: %s\n' "$(say 'Initial administrator' '初始管理员')" "$(say 'Initial password' '初始密码')" "$password"
  else
    # Fixed-version installs and downgrades can still run the earlier setup UI.
    token=$(tail -n +$((startup_log_lines+1)) "$log_file" |
      sed -n 's/^Open the console to complete setup\. Setup token: \([A-Za-z0-9_-][A-Za-z0-9_-]*\)$/\1/p' | tail -n 1)
    [[ -z "$token" ]] || printf '\n%s: %s\n' "$(say 'Setup token' '初始化密钥')" "$token"
  fi
}
console_url() {
  local address host port candidate
  address=$(awk -F '\t' '$1=="listen"{print $2}' "$work/config.tsv")
  host=${address%:*}; port=${address##*:}; host=${host#[}; host=${host%]}
  if [[ -z "$host" || "$host" == 0.0.0.0 || "$host" == :: ]]; then
    # Inspect local interfaces only. Printing an address must not depend on an
    # external IP lookup, and must retain the listener's actual custom port.
    candidate=$(hostname -I 2>/dev/null | tr ' ' '\n' | awk '/^[0-9]+\./ && $0 !~ /^127\./ {print; exit}') || candidate=
    if [[ -z "$candidate" ]] && command -v ifconfig >/dev/null 2>&1; then
      candidate=$(ifconfig 2>/dev/null | awk '$1=="inet" && $2 !~ /^127\./ {print $2; exit}') || candidate=
    fi
    host=${candidate:-localhost}
  fi
  [[ "$host" != *:* ]] || host=[$host]
  printf 'http://%s:%s\n' "$host" "$port"
}
backup_cli_sidecars() {
  local suffix kind path name
  for suffix in cluster.json local-auth.json; do
    for kind in cli data-cli; do
      path=$cli_path.$suffix
      if [[ "$kind" == data-cli ]]; then path=$data_dir/cli-$suffix; fi
      [[ ! -L "$path" && ( ! -e "$path" || -f "$path" ) ]] || return 1
      [[ -f "$path" ]] || continue
      name=$kind.$suffix
      # The backup is private before any credential bytes are copied.
      (umask 077; set -C; cat "$path" > "$backup/$name") || return 1
    done
  done
}
restore_cli_sidecars() {
  local suffix kind path saved temporary
  for suffix in cluster.json local-auth.json; do
    for kind in cli data-cli; do
      path=$cli_path.$suffix
      if [[ "$kind" == data-cli ]]; then path=$data_dir/cli-$suffix; fi
      saved=${backup:+$backup/$kind.$suffix}
      [[ ! -L "$path" && ( ! -e "$path" || -f "$path" ) && ! -L "$saved" && ( ! -e "$saved" || -f "$saved" ) ]] || return 1
      if [[ -f "$saved" ]]; then
        temporary=$(mktemp "$path.restore.XXXXXX") || return 1
        if ! chmod 600 "$temporary" || ! cat "$saved" > "$temporary"; then rm -f "$temporary"; return 1; fi
        if [[ "$suffix" == cluster.json ]] && ! chmod 644 "$temporary"; then rm -f "$temporary"; return 1; fi
        if ! mv -f "$temporary" "$path"; then rm -f "$temporary"; return 1; fi
      else
        rm -f "$path" || return 1
      fi
    done
  done
}
finish_agent_maintenance() {
  ((agent_maintenance_started)) || return 0
  if "$work/$filename" --config "$config" --upgrade-maintenance end --upgrade-maintenance-file "$work/agent-maintenance.json"; then
    agent_maintenance_started=0
  else
    printf 'Warning: could not end Agent upgrade maintenance; it expires automatically within 30 minutes.\n' >&2
  fi
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
    if ! restore_cli_sidecars; then printf 'CLI connection rollback failed; backup: %s\n' "$backup" >&2; exit 1; fi
  fi
  if ((existed)); then
    if ((enrollment_attempted == 0)) && [[ -f "$backup/database" ]] && ! "$work/$filename" --config "$backup/service.json" --restore-state "$backup/database"; then
      printf 'Database rollback incomplete. Current files retained; backup: %s\n' "$backup" >&2
      exit 1
    fi
    if [[ -f "$backup/binary" ]]; then cp -p "$backup/binary" "$binary.restore" && mv -f "$binary.restore" "$binary"; else rm -f "$binary"; fi
    cp -p "$backup/service.json" "$config"
    if [[ -f "$backup/install.tsv" ]]; then cp -p "$backup/install.tsv" "$metadata"; else rm -f "$metadata"; fi
    if [[ -f "$backup/unit" ]]; then cp -p "$backup/unit" "$unit"; else rm -f "$unit"; fi
    if [[ "$platform" == linux ]]; then systemctl daemon-reload; fi
    if ((was_active)) && [[ -f "$backup/binary" ]]; then
      prepare_start
      if start_service && wait_ready; then finish_agent_maintenance;
      else printf 'Rollback files restored, but the original service did not become ready. Check %s\n' "$log_file" >&2; fi
    fi
  else
    rm -f "$unit"
    if [[ "$platform" == linux ]]; then systemctl daemon-reload; fi
    if ((enrollment_attempted == 0)) && [[ -n "$adopt_config" && -f "$backup/database" ]]; then
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
  if ((enrollment_attempted)); then
    say 'Agent data and enrollment state were retained. Check the Master connection and retry enrollment.' '已保留 Agent 数据和注册状态，请检查 Master 连接后重试注册。' >&2
  fi
  [[ $result != 0 ]] || result=1
  exit "$result"
}
check_listener
if ((existed)); then
  maintain_backups prune || true
fi
if ((existed || had_cli || had_cli_sidecars || had_data_cli_connections)) || [[ -n "$adopt_config" ]]; then
  check_backup_space
fi
mkdir -p "$install_dir" "$config_dir" "$data_dir" "$backup_dir" "$(dirname "$unit")"
chmod 700 "$install_dir" "$config_dir" "$data_dir" "$backup_dir"
if ((existed)); then
  backup=$(mktemp -d "$backup_dir/$service_name-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")
  owned_backup=$backup
  printf '%s: %s\n' "$(say 'Backup' '备份')" "$backup"
  if [[ -f "$binary" ]]; then cp -p "$binary" "$backup/binary"; fi
  cp -p "$config" "$backup/service.json"
  if [[ -f "$metadata" ]]; then cp -p "$metadata" "$backup/install.tsv"; fi
  if [[ -f "$unit" ]]; then cp -p "$unit" "$backup/unit"; fi
  if active; then was_active=1; fi
  if [[ "$platform" == linux ]] && systemctl is-enabled --quiet "$service_name"; then was_enabled=1; fi
  if [[ "$role" == agent && "$enrollment_requested" == 0 ]]; then
    say 'Protecting Agent tasks during the upgrade...' '正在保护升级期间的 Agent 任务…'
    "$work/$filename" --config "$config" --upgrade-maintenance begin --upgrade-maintenance-file "$work/agent-maintenance.json"
    agent_maintenance_started=1
  fi
  trap rollback ERR INT TERM
  printf "$(say 'Stopping %s before the database backup...' '正在停止 %s，随后备份数据库…')\n" "$service_name"
  stop_service
  # Recheck the database lock after stopping the service; a manual process may
  # still own it. Take a consistent snapshot only when all writers are stopped.
  "$work/$filename" --config "$backup/service.json" --backup-state "$backup/database"
elif [[ -n "$adopt_config" ]]; then
  backup=$(mktemp -d "$backup_dir/$service_name-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")
  owned_backup=$backup
  printf '%s: %s\n' "$(say 'Backup' '备份')" "$backup"
  cp -p "$work/service.json" "$backup/service.json"
  "$work/$filename" --config "$backup/service.json" --backup-state "$backup/database"
fi
if ((had_cli || had_cli_sidecars || had_data_cli_connections)); then
  if [[ -z "$backup" ]]; then
    backup=$(mktemp -d "$backup_dir/$service_name-$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")
    owned_backup=$backup
    printf '%s: %s\n' "$(say 'Backup' '备份')" "$backup"
  fi
  if ((had_cli)); then cp -p "$cli_path" "$backup/cli"; fi
  backup_cli_sidecars
fi
trap rollback ERR INT TERM
printf "$(say 'Installing %s %s...' '正在安装 %s %s…')\n" "$role" "$version"
cp "$work/$filename" "$binary.new"
chmod 755 "$binary.new"
mv -f "$binary.new" "$binary"
if [[ ! -d "$(dirname "$cli_path")" ]]; then
  mkdir -p "$(dirname "$cli_path")"
  chmod 755 "$(dirname "$cli_path")"
fi
cp "$work/$cli_filename" "$cli_path.new"
chmod 755 "$cli_path.new"
mv -f "$cli_path.new" "$cli_path"
cli_changed=1
if ((existed == 0)); then cp "$work/service.json" "$config"; chmod 600 "$config"; fi
printf 'role\t%s\nserviceName\t%s\nconfig\t%s\nunit\t%s\nversion\t%s\n' "$role" "$service_name" "$config" "$unit" "$version" > "$metadata"
printf 'cliPath\t%s\nbackupDir\t%s\ndataDir\t%s\n' "$cli_path" "$backup_dir" "$data_dir" >> "$metadata"
if [[ "$install_dir" == "$root_dir/$role/bin" && "$config_dir" == "$root_dir/$role/config" && "$data_dir" == "$root_dir/$role/data" ]]; then
  printf 'rootDir\t%s\n' "$root_dir" >> "$metadata"
fi
if ((enrollment_requested)); then
  if ((existed)) || [[ -n "$adopt_config" ]]; then
    say 'Rejoining the Agent to the cluster...' '正在将 Agent 重新加入集群…'
  fi
  # A join may commit on the Master even when its response is lost. Keep the
  # durable pending/new identity on failure so the next attempt is idempotent.
  enrollment_attempted=1
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
# The Agent exits with 78 after the Master removed it; do not restart it.
RestartPreventExitStatus=78
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
printf "$(say 'Starting %s...' '正在启动 %s…')\n" "$service_name"
start_service
wait_ready
if [[ "$platform" == linux ]]; then
  if ((existed == 0 || had_unit == 0 || was_enabled)); then systemctl enable "$service_name" >/dev/null; fi
fi
trap - ERR INT TERM
finish_agent_maintenance
if commit_start; then
  finish_backup
  if ((cli_migrated)); then rm -f "$previous_cli" "$previous_cli.cluster.json" "$previous_cli.local-auth.json"; fi
fi
if ! publish_master_packages; then
  if [[ "$role" == master ]]; then
    printf '%s: %s\n' "$(say 'Web UI' '访问地址')" "$(console_url)"
    print_initial_credentials
  fi
  fail "$(say 'Service installed, but could not retain Agent packages; rerun the installer to repair the package cache' '服务已就绪，但 Agent 安装包保存失败。请重新运行安装脚本修复。')"
fi
printf '\n%s: %s\n%s: %s\n%s: %s\n' "$(say 'Service' '服务')" "$service_name" "$(say 'Configuration' '配置文件')" "$config" "$(say 'Log' '日志')" "$log_file"
# These two labels are also consumed by installation tooling.
printf 'Installation: %s\n' "$install_dir"
printf 'CLI: %s\n' "$cli_path"
printf 'Tasks: %q task list\n' "$cli_path"
printf '\n=== %s %s · %s ===\n' "$role" "$version" "$(say 'Installed successfully' '安装成功')"
if [[ "$role" == master ]]; then
  printf '%s: %s\n' "$(say 'Web UI' '访问地址')" "$(console_url)"
fi
print_initial_credentials
OSS_TRANSFER_EMBEDDED_NATIVE_INSTALLER
}
# END embedded native installer
lang='' role='' method='' version=latest source=auto release_url='' package_dir='' yes=0 upgrade=0 user_service=0 answer=''
install_dir='' data_dir='' cli_path='' service_name='' master_url='' join_url='' files_dir='' image='' port=1685
listen='' adopt_config='' root_dir='' with_agent='' config_dir=''
replace_cli=0 token_stdin=0 no_deps=0 uninstall=0 keep_data=0 force=0
prepared_github=0
local_package_dir=''
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
  --with-agent / --without-agent Install a local Agent with a native Master
  --root-dir DIR                Keep programs, configuration and data under DIR
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
  --force                       Reinstall even when the same version is running
  --yes                         Skip prompts

Uninstall (lists everything first; nothing is downloaded):
  --uninstall [--role master|agent] [--keep-data] [--yes]
                                Native services, programs, CLI and (without
                                --keep-data) configuration, data and backups
  --uninstall --method compose  Docker Compose installation and (without
                                --keep-data) its data directory
EOF
}
while (($#)); do
  case "$1" in
    --help|-h) usage; exit 0;;
    --lang|--role|--method|--version|--source|--release-url|--package-dir|--port|--join-url|--master-url|--files-dir|--image|--install-dir|--data-dir|--cli-path|--service-name|--listen|--adopt-config|--root-dir|--config-dir)
      (($# >= 2)) && [[ -n "$2" && "$2" != --* ]] || fail "Missing value: $1"
      key=${1#--}; key=${key//-/_}; printf -v "$key" '%s' "$2"; shift 2;;
    --backup-dir|--startup-timeout)
      (($# >= 2)) && [[ -n "$2" && "$2" != --* ]] || fail "Missing value: $1"
      native_args+=("$1" "$2"); shift 2;;
    --yes) yes=1; shift;;
    --with-agent) with_agent=1; shift;;
    --without-agent) with_agent=0; shift;;
    --upgrade) upgrade=1; shift;;
    --user) user_service=1; shift;;
    --replace-cli) replace_cli=1; shift;;
    --token-stdin) token_stdin=1; shift;;
    --no-deps) no_deps=1; shift;;
    --uninstall) uninstall=1; shift;;
    --keep-data) keep_data=1; shift;;
    --force) force=1; shift;;
    *) fail "Unknown option: $1";;
  esac
done
[[ -z "$lang" || "$lang" == en || "$lang" == zh ]] || fail '--lang must be en or zh'
# Bootstrap scripts from the console pass its language this way; older
# installers that predate --lang ignore it.
if [[ -z "$lang" ]]; then case "${OSS_TRANSFER_INSTALL_LANG:-}" in en|zh) lang=$OSS_TRANSFER_INSTALL_LANG;; esac; fi
[[ "$source" == auto || "$source" == github || "$source" == jdoss ]] || fail '--source must be auto, github or jdoss'
version=${version#v}
version_requested=$version
[[ "$version" == latest || "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'Invalid version'
os=$(uname -s)
[[ "$os" == Linux || "$os" == Darwin || "$os" == FreeBSD ]] || fail 'Use Install.ps1 on Windows'
[[ "$os" == Darwin || "$user_service" == 0 ]] || fail '--user is macOS only'
# Check before asking for sudo: Docker Desktop cannot use the Compose layout.
[[ "$method" != compose || "$os" == Linux ]] || fail "$(say 'Docker Compose installation needs Linux. With Docker Desktop on macOS or Windows, copy the Docker command from Nodes > Add node > Docker instead; it needs no root.' 'Docker Compose 安装仅支持 Linux。在 macOS 或 Windows 的 Docker Desktop 上，请改用“执行节点 → 添加节点 → Docker”中的 Docker 指令，无需 root。')"
if ((user_service == 0)) && [[ $(id -u) != 0 ]]; then
  # A script read from a pipe cannot be started again; ask for sudo in the pipe.
  [[ -f "${BASH_SOURCE[0]:-}" ]] || fail 'Run as root: curl ... | sudo bash (macOS user service: curl ... | bash -s -- --user)'
  command -v sudo >/dev/null || fail 'Run as root or use --user on macOS'
  # Bash before 4.4 treats an empty "${array[@]}" as unset under set -u. sudo
  # resets the environment, so an inherited language is passed explicitly.
  exec sudo bash "${BASH_SOURCE[0]}" ${original_args[@]+"${original_args[@]}"} ${lang:+--lang "$lang"}
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
# Environment propagation also works with older release installers that do
# not accept --lang. Keep the system locale unchanged.
export OSS_TRANSFER_INSTALL_LANG=$lang
case "$os" in Linux) platform=linux;; Darwin) platform=darwin;; FreeBSD) platform=freebsd;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64;; arm64|aarch64) arch=arm64;; armv7l|armv7) arch=armv7;; riscv64) arch=riscv64;; loongarch64|loong64) arch=loong64;; *) arch=unsupported;; esac
((keep_data == 0 || uninstall)) || fail "$(say '--keep-data is used with --uninstall' '--keep-data 需与 --uninstall 一起使用')"
# Uninstall a Docker Compose installation made by this installer: its project
# containers, Compose file and (without --keep-data) its data directory.
uninstall_compose() {
  local roles=master selected directory file data project removed=0 database users path
  [[ "$os" == Linux ]] || fail 'Compose installations exist only on Linux'
  command -v docker >/dev/null && docker compose version >/dev/null 2>&1 || fail "$(say 'Docker Compose is required to remove a Compose installation' '卸载 Compose 安装需要 Docker Compose')"
  docker info >/dev/null 2>&1 || fail "$(say 'Start the Docker daemon first; nothing was removed' '请先启动 Docker 服务；未做任何更改')"
  if [[ -n "$role" ]]; then roles=$role; else roles='master agent'; fi
  for selected in $roles; do
    directory=${install_dir:-/opt/oss-transfer-compose/$selected}
    file=$directory/docker-compose.yml
    [[ -f "$file" && ! -L "$file" ]] || continue
    # Only a Compose file written by this installer for this role is removed.
    grep -Fqx "    command: ['oss-transfer-$selected', '--data-dir', '/data']" "$file" || fail "$(say "$file was not written by this installer; nothing was removed" "$file 不是安装脚本生成的，未做任何更改")"
    # Act on the project and data directory this installation really uses,
    # not on today's defaults. Files from earlier releases have no project
    # line; Compose labels their containers with the file instead.
    project=$(sed -n 's/^# oss-transfer-project: \([a-z0-9][a-z0-9_-]*\)$/\1/p' "$file" | head -n 1)
    if [[ -z "$project" ]]; then
      project=$(docker ps -a --filter "label=com.docker.compose.project.config_files=$file" --format '{{.Label "com.docker.compose.project"}}' | sort -u)
      [[ "$project" != *$'\n'* ]] || fail "$(say "Several Compose projects use $file: $(printf '%s' "$project" | tr '\n' ' '); nothing was removed" "多个 Compose 项目使用 ${file}：$(printf '%s' "$project" | tr '\n' ' ')；未做任何更改")"
    fi
    project=${project:-${service_name:-oss-transfer-$selected}}
    [[ -z "$service_name" || "$service_name" == "$project" ]] || fail "$(say "$file belongs to Compose project $project, not $service_name; nothing was removed" "$file 属于 Compose 项目 ${project}，不是 ${service_name}；未做任何更改")"
    data=$(sed -n "s|^      - '\(/[^']*\):/data'\$|\1|p" "$file" | head -n 1)
    data=${data:-${data_dir:-/var/lib/oss-transfer-compose/$selected}}
    [[ -z "$data_dir" || "$data_dir" == "$data" ]] || fail "$(say "$file uses data directory $data, not $data_dir; nothing was removed" "$file 使用的数据目录是 ${data}，不是 ${data_dir}；未做任何更改")"
    for path in "$directory" "$data"; do
      [[ "$path" == /*/* && "$path" != *[$'\n\r\t\047"\\$']* && "$path" != *'/../'* && "$path" != */.. ]] || fail 'Use clean absolute paths'
    done
    say "Compose project $project ($selected): $file" "Compose 项目 ${project}（${selected}）：$file"
    if ((keep_data)); then say "  Keep data: $data" "  保留数据：$data"
    else say "  Remove data: $data" "  删除数据：$data"; fi
    if ((yes == 0)); then
      prompt answer "$(say 'Uninstall? y / n' '确认卸载？y / n')" n
      [[ "$answer" == y || "$answer" == Y ]] || { say 'Cancelled; nothing was changed.' '已取消，未做任何更改。'; continue; }
    fi
    docker compose --project-name "$project" --file "$file" down --remove-orphans
    # A container that still mounts the data (another project name, or one
    # started by hand) would keep running on deleted files with no Compose
    # file left to stop it.
    users=$(docker ps -a --filter "volume=$data" --format '{{.Names}}' | tr '\n' ' ')
    [[ -z "${users// /}" ]] || fail "$(say "$data is still used by container(s): $users. The Compose file and data were kept; stop and remove those containers, then run the uninstall again." "$data 仍被容器使用：${users}。已保留 Compose 文件和数据；请停止并删除这些容器后重新卸载。")"
    rm -f -- "$file"
    rmdir -- "$directory" 2>/dev/null || say "Kept (contains other files): $directory" "已保留（目录中还有其他文件）：$directory"
    if ((keep_data == 0)) && [[ -d "$data" && ! -L "$data" ]]; then
      database=$data/$selected.db
      if [[ -f "$database" || -z $(ls -A "$data") ]]; then rm -rf -- "$data"
      else say "Kept (not recognized as OSS Transfer data): $data" "已保留（无法确认是 OSS Transfer 数据）：$data"; fi
    fi
    removed=1
  done
  if ((removed == 0)); then
    say 'No Docker Compose installation made by this installer was found; nothing to uninstall.' '未找到由安装脚本创建的 Docker Compose 安装，无需卸载。'
  else
    say 'Docker Compose installation removed. The Docker image is kept; remove it with docker image rm if it is no longer needed.' 'Docker Compose 安装已卸载。Docker 镜像已保留，如不再需要可用 docker image rm 删除。'
  fi
}
if ((uninstall)); then
  # Nothing is downloaded: the native uninstaller is embedded in this file.
  if [[ "$method" == compose ]]; then uninstall_compose; exit 0; fi
  [[ -z "$method" || "$method" == native ]] || fail 'Invalid deployment method'
  uninstall_args=(--uninstall --lang "$lang")
  for field in role root_dir install_dir service_name; do
    value=${!field}
    [[ -z "$value" ]] || uninstall_args+=("--${field//_/-}" "$value")
  done
  ((keep_data == 0)) || uninstall_args+=(--keep-data)
  ((yes == 0)) || uninstall_args+=(--yes)
  ((user_service == 0)) || uninstall_args+=(--user)
  uninstall_stage=$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-uninstall.XXXXXX")
  trap 'rm -rf -- "$uninstall_stage"' EXIT
  transfer_embedded_native_installer > "$uninstall_stage/native-install.sh"
  bash "$uninstall_stage/native-install.sh" "${uninstall_args[@]}"
  for selected in master agent; do
    if [[ -f "/opt/oss-transfer-compose/$selected/docker-compose.yml" ]]; then
      say "A Docker Compose $selected installation also exists; remove it with --uninstall --method compose." "此主机还有 Docker Compose 方式安装的 ${selected}，可使用 --uninstall --method compose 卸载。"
    fi
  done
  exit 0
fi
# A copied offline bundle beside this installer (or in the current directory)
# is installed directly; nothing is downloaded. --source forces a download.
if [[ -z "$release_url" && -z "$package_dir" && "$source" == auto ]] && local_package_dir=$(transfer_local_package_dir "${BASH_SOURCE[0]:-}" "$platform" "$arch"); then
  printf '%s: %s\n' "$(say 'Using local release files (offline)' '使用本地安装包目录（离线）')" "$local_package_dir"
else
  local_package_dir=''
fi
default_install_dir() {
  if [[ -n "$root_dir" ]]; then printf '%s/%s/bin\n' "$root_dir" "$1"
  elif [[ "$os" == Linux ]]; then printf '/opt/oss-transfer/%s/bin\n' "$1"
  elif [[ "$os" == FreeBSD ]]; then printf '/usr/local/lib/oss-transfer/%s/bin\n' "$1"
  elif ((user_service)); then printf '%s\n' "$HOME/Library/Application Support/OSS Transfer/$1/bin"
  else printf '/Library/Application Support/OSS Transfer/%s/bin\n' "$1"; fi
}
existing_install_dir() {
  local directory legacy
  directory=$(default_install_dir "$1")
  legacy=${directory%/bin}
  if [[ -f "$directory/install.tsv" && ! -L "$directory/install.tsv" ]]; then
    printf '%s\n' "$directory"
  elif [[ -f "$legacy/install.tsv" && ! -L "$legacy/install.tsv" ]]; then
    # Older binaries lived directly in the role directory. This is still
    # inside the selected root and must receive the same identity checks.
    printf '%s\n' "$legacy"
  elif [[ -z "$root_dir" && "$os" == Linux && -f "/opt/oss-transfer/$1/install.tsv" && ! -L "/opt/oss-transfer/$1/install.tsv" ]]; then
    printf '/opt/oss-transfer/%s\n' "$1"
  elif [[ -z "$root_dir" && "$os" != Linux && -f "/usr/local/lib/oss-transfer/$1/install.tsv" && ! -L "/usr/local/lib/oss-transfer/$1/install.tsv" ]]; then
    printf '/usr/local/lib/oss-transfer/%s\n' "$1"
  elif [[ -z "$root_dir" && "$os" == Linux && -f "/opt/oss-transfer-go/$1/install.tsv" && ! -L "/opt/oss-transfer-go/$1/install.tsv" ]]; then
    printf '/opt/oss-transfer-go/%s\n' "$1"
  fi
}
retained_role_config() {
  local selected=$1 directory candidate
  if [[ -n "${config_dir:-}" ]]; then
    [[ ! -f "$config_dir/service.json" || -L "$config_dir/service.json" ]] || printf '%s\n' "$config_dir/service.json"
    return 0
  fi
  directory=${install_dir:-$(existing_install_dir "$selected")}
  if [[ -n "$directory" && -f "$directory/install.tsv" && ! -L "$directory/install.tsv" ]]; then
    candidate=$(awk -F '\t' '$1=="config"{print $2}' "$directory/install.tsv")
    [[ ! -f "$candidate" || -L "$candidate" ]] || printf '%s\n' "$candidate"
    return 0
  fi
  candidate=$(dirname "$(default_install_dir "$selected")")/config/service.json
  if [[ -f "$candidate" && ! -L "$candidate" ]]; then printf '%s\n' "$candidate"; return; fi
  if [[ -z "${root_dir:-}" && -z "${install_dir:-}" ]]; then
    case "$os" in Linux) candidate=/etc/oss-transfer/$selected/service.json;; FreeBSD) candidate=/usr/local/etc/oss-transfer/$selected/service.json;; *) candidate=/Library/Application\ Support/OSS\ Transfer/$selected/config/service.json;; esac
    [[ ! -f "$candidate" || -L "$candidate" ]] || printf '%s\n' "$candidate"
  fi
}
# Rerunning the same one-line command offers to upgrade what is already installed.
if ((interactive && upgrade == 0)) && [[ -z "$role$method$package_dir$release_url$adopt_config" ]]; then
  installed=''
  for candidate in master agent; do
    [[ -z $(existing_install_dir "$candidate") ]] || installed+=" $candidate"
  done
  if [[ -n "$installed" ]]; then
    prompt answer "$(say "Installed:$installed. 1 Upgrade / 2 Install another role" "已安装：${installed}。1 升级 / 2 安装其他角色")" 1
    [[ "$answer" != 1 ]] || upgrade=1
    if [[ "$upgrade" == 1 && "$installed" == *master* && "$installed" != *agent* && -z "$with_agent" ]]; then
      prompt answer "$(say 'Also install a local Agent? y / n' '同时补装本机 Agent？y / n')" y
      case "$answer" in y|Y) with_agent=1; role=master;; n|N) with_agent=0;; *) fail 'Choose y or n';; esac
    fi
  fi
fi
# Generated Agent commands and local package installs already chose native mode.
if [[ -z "$method" && ( -n "$release_url" || -n "$package_dir" || -n "$local_package_dir" || "$upgrade" == 1 ) ]]; then method=native; fi
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
if [[ "$with_agent" == 1 && -z "$role" && "$upgrade" == 1 ]]; then role=master; fi
[[ -z "$role" || "$role" == master || "$role" == agent ]] || fail 'Invalid role'
[[ "$method" == native || "$method" == compose ]] || fail 'Invalid deployment method'
if [[ "$with_agent" == 1 && ( "$role" != master || "$method" != native ) ]]; then
  fail '--with-agent requires a native Master installation'
fi
if [[ "$role" == master && "$method" == native && "$upgrade" == 0 && -z "$with_agent" ]]; then
  with_agent=1
  if ((yes == 0)); then
    prompt answer "$(say 'Install a local Agent to transfer files on this machine? y / n' '同时安装本机 Agent，让这台机器参与传输？y / n')" y
    case "$answer" in y|Y) with_agent=1;; n|N) with_agent=0;; *) fail 'Choose y or n';; esac
  fi
fi
with_agent=${with_agent:-0}
if ((yes == 0)) && [[ "$role" == master && -z "$listen" && "$upgrade" == 0 && -z "$adopt_config" ]]; then
  prompt port "$(say 'Master port' '主节点端口')" "$port"
fi
if [[ ! "$port" =~ ^[0-9]{1,5}$ ]] || ((10#$port < 1 || 10#$port > 65535)); then fail 'Invalid port'; fi
[[ -z "$package_dir" || -z "$release_url" ]] || fail 'Choose --package-dir or --release-url'
valid_master_origin() {
  local host port address
  [[ "$1" =~ ^https?://(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+)(:([0-9]{1,5}))?/?$ ]] || return 1
  host=${BASH_REMATCH[1]}; port=${BASH_REMATCH[3]}
  [[ -z "$port" ]] || ((10#$port >= 1 && 10#$port <= 65535)) || return 1
  if [[ "$host" == \[* ]]; then
    address=${host:1:${#host}-2}
    # Check bracketed IPv6 without requiring Python, DNS, or a live connection.
    # An embedded IPv4 address counts as the last two 16-bit groups.
    LC_ALL=C awk -v address="$address" 'BEGIN {
      if (index(address, ".")) {
        ipv4=address; sub(/^.*:/, "", ipv4)
        if (split(ipv4, octets, "\\.")!=4) exit 1
        for (i=1;i<=4;i++) if (octets[i] !~ /^[0-9]+$/ || length(octets[i])>3 || octets[i]+0>255) exit 1
        sub(/[^:]*$/, "0:0", address)
      }
      compressed=split(address, halves, "::")
      if (compressed>2) exit 1
      groups=0
      for (i=1;i<=compressed;i++) {
        if (halves[i]=="") { if (compressed==1) exit 1; continue }
        count=split(halves[i], pieces, ":")
        for (j=1;j<=count;j++) if (pieces[j] !~ /^[0-9A-Fa-f]+$/ || length(pieces[j])>4) exit 1
        groups+=count
      }
      exit !((compressed==1 && groups==8) || (compressed==2 && groups<8))
    }' || return 1
  fi
}
enrollment_input() {
  local join_master join_token
  if [[ "$role" == agent && "$upgrade" == 0 && -z "$adopt_config" && -z "$master_url" && -z "$join_url" && "$yes" == 0 ]] && [[ -z $(retained_role_config agent) ]]; then
    prompt join_url "$(say 'Agent invitation link from the console' '控制台生成的执行节点邀请链接')" ''
  fi
  if [[ -n "$join_url" ]]; then
    [[ "$role" == agent && "$join_url" =~ ^(.+)/api/agents/bootstrap/([A-Za-z0-9_-]+)$ ]] || fail 'Use the Agent invitation link from the console'
    join_master=${BASH_REMATCH[1]}; join_token=${BASH_REMATCH[2]}
    [[ "$join_master" != */ ]] && valid_master_origin "$join_master" || fail 'Use the Agent invitation link from the console'
    master_url=$join_master; TRANSFER_JOIN_TOKEN=$join_token; export TRANSFER_JOIN_TOKEN
  fi
}
# A joining Agent may fetch packages from the exact Master that issued its
# invitation. Other remote HTTP mirrors are still rejected.
valid_release_url() {
  local origin=$master_url suffix
  if [[ -n "$join_url" && "$join_url" =~ ^(.+)/api/agents/bootstrap/([A-Za-z0-9_-]+)$ ]]; then origin=${BASH_REMATCH[1]}; fi
  if [[ "$role" == agent && -n "$origin" ]] && valid_master_origin "$origin"; then
    origin=${origin%/}
    suffix=${release_url#"$origin"}
    [[ "$release_url" == "$origin"* && "$suffix" =~ ^/api/agents/bootstrap/[A-Za-z0-9_-]+/files$ ]] && return 0
  fi
  [[ "$release_url" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?/[A-Za-z0-9._~/-]+$ || "$release_url" =~ ^http://(127\.0\.0\.1|localhost)(:[0-9]+)?/[A-Za-z0-9._~/-]+$ ]]
}
if [[ -n "${release_url:-}" ]]; then
  release_url=$(transfer_release_url "$release_url")
  valid_release_url || fail 'Release URL must use HTTPS or the inviting Master'
fi
stage=$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-setup.XXXXXX")
cleanup() {
  local status=$?
  if ((status != 0 && with_agent)) && [[ -f "$stage/native-install.log" ]]; then
    # A failed Agent install must not hide credentials for the ready Master.
    print_master_access || true
  fi
  if ((status != 0 && compose_rollback)); then rollback_compose || true; fi
  rm -rf -- "$stage"
  unset TRANSFER_JOIN_TOKEN OSS_TRANSFER_JOIN_TOKEN join_url
}
compose_rollback=0 created_install_dir=0 created_data_dir=0 reuse_data=0 compose=()
# A failed Compose installation removes what this run created (containers,
# network, Compose file and data directory), so the same command can be rerun.
rollback_compose() {
  local path
  compose_rollback=0
  if ((${#compose[@]})) && [[ -f "$install_dir/docker-compose.yml" ]]; then
    "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
  fi
  for path in "$data_dir" "$install_dir"; do
    [[ -n "$path" && "$path" == /* && "$path" != / && -d "$path" ]] || continue
    # Reused data from an earlier installation is never removed.
    [[ "$path" != "$data_dir" ]] || ((reuse_data == 0)) || continue
    if { [[ "$path" == "$data_dir" ]] && ((created_data_dir)); } || { [[ "$path" == "$install_dir" ]] && ((created_install_dir)); }; then
      rm -rf -- "$path"
    else
      # The directory existed and was empty before this run.
      find "$path" -mindepth 1 -delete 2>/dev/null || true
    fi
  done
  say 'Removed the containers, network and files created by this attempt. Fix the problem above and run the same command again.' '已清理本次创建的容器、网络和文件。请解决上述问题后重新运行同一条命令。' >&2
}
trap cleanup EXIT
sha256() { if command -v sha256sum >/dev/null; then sha256sum "$1" | awk '{print $1}'; elif [[ -x /sbin/sha256 ]]; then /sbin/sha256 -q "$1"; else shasum -a 256 "$1" | awk '{print $1}'; fi; }
metadata_field() { awk -F '\t' -v key="$2" '$1==key {print $2}' "$1"; }
native_output() {
  if ((with_agent)); then
    # Keep the Master's access details together at the very end of a combined
    # install. tee has already saved this run's output in the private stage.
    sed -E '/^(Web UI|访问地址|Initial administrator|初始管理员|Initial password|初始密码|Setup token|初始化密钥): /d'
  else
    cat
  fi
}
print_master_access() {
  local address password token
  address=$(sed -nE 's/^(Web UI|访问地址): (https?:\/\/[^[:space:]]+)$/\2/p' "$stage/native-install.log" | tail -n 1)
  password=$(sed -nE 's/^(Initial password|初始密码): ([A-Za-z0-9_-]+)$/\2/p' "$stage/native-install.log" | tail -n 1)
  token=$(sed -nE 's/^(Setup token|初始化密钥): ([A-Za-z0-9_-]+)$/\2/p' "$stage/native-install.log" | tail -n 1)
  [[ -z "$address" ]] || printf '%s: %s\n' "$(say 'Web UI' '访问地址')" "$address"
  if [[ -n "$password" ]]; then
    printf '%s: admin\n%s: %s\n' "$(say 'Initial administrator' '初始管理员')" "$(say 'Initial password' '初始密码')" "$password"
  elif [[ -n "$token" ]]; then
    printf '%s: %s\n' "$(say 'Setup token' '初始化密钥')" "$token"
  fi
}
verify_local_agent_cluster() {
  local origin=$1 config=$2 directory token
  directory=$(sed -nE 's/.*"dataDirectory"[[:space:]]*:[[:space:]]*"([^"\\]*)".*/\1/p' "$config")
  [[ "$directory" == /* && -f "$directory/cli-local-auth.json" && ! -L "$directory/cli-local-auth.json" ]] || fail 'Existing Agent identity could not be verified; its data has been kept'
  token=$(sed -nE 's/.*"token"[[:space:]]*:[[:space:]]*"([A-Za-z0-9_.-]+)".*/\1/p' "$directory/cli-local-auth.json")
  [[ "$token" == otcli1.* && "$token" != *$'\n'* ]] || fail 'Existing Agent credential is invalid; its data has been kept'
  # A retained Agent must belong to this Master. This also accepts an existing
  # identity using a private IP instead of the local Master's loopback origin.
  # The origin is often http://[::1]:PORT; curl 7.29 would glob its brackets.
  printf 'header = "Authorization: Bearer %s"\n' "$token" |
    curl --config - --fail --silent --show-error --globoff --connect-timeout 5 --max-time 15 "$origin/api/auth/current" -o /dev/null ||
    fail 'Existing Agent belongs to another cluster or its identity is unavailable; its data has been kept'
}
install_local_agent() {
  local master_install master_metadata master_cli master_config master_data master_service origin
  local agent_root agent_install agent_config agent_service invitation token name
  local -a agent_args
  master_install=$(sed -n 's/^Installation: //p' "$stage/native-install.log" | tail -n 1)
  [[ -n "$master_install" ]] || master_install=${install_dir:-$(default_install_dir master)}
  master_metadata=$master_install/install.tsv
  [[ -f "$master_metadata" && ! -L "$master_metadata" ]] || fail 'Master installed, but its installation metadata could not be read'
  master_cli=$(metadata_field "$master_metadata" cliPath)
  master_config=$(metadata_field "$master_metadata" config)
  master_data=$(metadata_field "$master_metadata" dataDir)
  master_service=$(metadata_field "$master_metadata" serviceName)
  "$master_install/oss-transfer-master" --config "$master_config" --check-config > "$stage/master-config.tsv"
  # The OS may turn 0.0.0.0 into a dual-stack listener. Use the exact origin
  # published with the local credential, rather than guessing an IPv4 alias.
  [[ -f "$master_data/cli-cluster.json" && ! -L "$master_data/cli-cluster.json" ]] || fail 'Master installed, but its local CLI connection is unavailable'
  origin=$(sed -nE 's/.*"masterUrl"[[:space:]]*:[[:space:]]*"([^"\\]*)".*/\1/p' "$master_data/cli-cluster.json")
  valid_master_origin "$origin" || fail 'Master installed, but its local address is invalid'
  agent_root=$root_dir
  if [[ -z "$agent_root" && "$master_install" == */master/bin ]]; then agent_root=${master_install%/master/bin}; fi
  if [[ -z "$agent_root" && "$master_install" != /opt/oss-transfer/master && "$master_install" != /usr/local/lib/oss-transfer/master && "$master_install" != /opt/oss-transfer-go/master ]]; then
    agent_root=$(dirname "$master_install")/oss-transfer-local-agent
  fi
  agent_args=(--role agent --yes --master-url "$origin")
  ((user_service == 0)) || agent_args+=(--user)
  agent_install=$(root_dir=$agent_root; existing_install_dir agent)
  if [[ -n "$agent_install" ]]; then
    agent_config=$(metadata_field "$agent_install/install.tsv" config)
    agent_args+=(--install-dir "$agent_install")
  else
    # An inferred default root is not a user-selected isolated installation:
    # it may still have an older Agent configuration under /etc.
    if [[ -z "$root_dir" && "$agent_root" == "$(dirname "$(dirname "$(default_install_dir agent)")")" ]]; then agent_root=; fi
    agent_config=$(root_dir=$agent_root; install_dir=; config_dir=; retained_role_config agent)
    if [[ -n "$agent_config" ]]; then
      agent_args+=(--config-dir "$(dirname "$agent_config")")
    else
      if [[ "$os" == FreeBSD ]]; then
        agent_service=${master_service%_master}_agent
      else
        agent_service=${master_service%-master}-agent
      fi
      agent_args+=(--service-name "$agent_service")
    fi
  fi
  [[ -z "$agent_root" ]] || agent_args+=(--root-dir "$agent_root")
  # The Master retains this exact build. Reuse it instead of downloading twice
  # or accidentally pairing a freshly installed Master with a moving latest.
  if [[ -f "$master_data/packages/manifest.tsv" ]]; then agent_args+=(--package-dir "$master_data/packages")
  elif [[ -n "$package_dir" ]]; then agent_args+=(--package-dir "$package_dir")
  else agent_args+=(--release-url "$release_url"); fi
  if [[ ! -f "$agent_config" ]]; then
    name=$(hostname | cut -c 1-80)
    invitation=$("$master_cli" --host "$origin" --json agent invite --label "$name" --master-url "$origin") || fail 'Master installed; local Agent invitation failed'
    token=$(printf '%s\n' "$invitation" | sed -nE 's/.*"inviteToken"[[:space:]]*:[[:space:]]*"([A-Za-z0-9_-]+)".*/\1/p')
    [[ "$token" =~ ^[A-Za-z0-9_-]{20,128}$ ]] || fail 'Master installed; local Agent invitation was invalid'
    export TRANSFER_JOIN_TOKEN=$token
    unset invitation token
  else
    verify_local_agent_cluster "$origin" "$agent_config"
  fi
  say 'Installing local Agent…' '正在安装本机 Agent…'
  if ! bash "$stage/native-install.sh" "${agent_args[@]}"; then
    unset TRANSFER_JOIN_TOKEN
    fail "$(say 'Master is ready. Local Agent installation failed; rerun this installer to finish it.' 'Master 已就绪，但本机 Agent 安装失败。请重新运行安装脚本完成安装。')"
  fi
  unset TRANSFER_JOIN_TOKEN
  printf '\n=== %s ===\n' "$(say 'Master and local Agent installed successfully' 'Master 和本机 Agent 安装成功')"
  say 'Local Agent joined the cluster.' '本机 Agent 已加入集群。'
  print_master_access
}
[[ -z "$local_package_dir" ]] || package_dir=$local_package_dir
if [[ -z "$release_url" && -z "$package_dir" ]]; then
  selected_source=$(transfer_select_source "$source" "$version") || fail 'Cannot select a download source; use --source or --release-url'
  printf '%s: %s\n' "$(say 'Download source' '下载源')" "$selected_source"
  if [[ "$selected_source" == github ]]; then
    [[ "$arch" != unsupported ]] || fail 'Unsupported CPU architecture'
    package_dir=$stage/github
    mkdir "$package_dir"
    transfer_github_prepare "$version" "$platform" "$arch" "$package_dir" || fail 'GitHub release verification failed'
    prepared_github=1
  else
    release_part=$version; [[ "$version" == latest ]] || release_part=v$version
    release_url=https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/$release_part
  fi
fi
# An archive copied from a GitHub Release has no manifest.tsv beside it. Build
# one from the archive's internal checksums without contacting any server.
if [[ -n "$package_dir" && ! -e "$package_dir/manifest.tsv" ]]; then
  [[ -d "$package_dir" ]] || fail "$(say "Package directory not found: $package_dir" "安装包目录不存在：$package_dir")"
  [[ "$arch" != unsupported ]] || fail 'Unsupported CPU architecture'
  mkdir "$stage/local-package"
  transfer_embedded_native_installer > "$stage/embedded-native-install.sh"
  transfer_local_prepare "$package_dir" "$platform" "$arch" "$stage/local-package" \
    native-install.sh "$stage/embedded-native-install.sh" install.sh "${BASH_SOURCE[0]:-}" || fail "$(say 'Offline package verification failed' '离线安装包校验失败')"
  say "No manifest.tsv found; verified $package_dir/oss-transfer-$platform-$arch.tar.gz with its internal checksums." "未找到 manifest.tsv，已按压缩包内置校验和验证 $package_dir/oss-transfer-$platform-$arch.tar.gz。"
  package_dir=$stage/local-package
fi
fetch() {
  local name=$1 protocol=https
  if [[ -n "$package_dir" ]]; then
    [[ -f "$package_dir/$name" && ! -L "$package_dir/$name" ]] || fail "Missing release file: $name"
    cp "$package_dir/$name" "$stage/$name"
  else
    [[ "$release_url" != http://* ]] || protocol=http,https
    # An inviting Master may be an IPv6 literal; curl 7.29 would glob its brackets.
    curl -fsSL --globoff --proto "=$protocol" --proto-redir "=$protocol" --connect-timeout 15 --max-time 600 --retry 2 "$release_url/$name" -o "$stage/$name"
  fi
}
fetch manifest.tsv
[[ $(wc -c < "$stage/manifest.tsv") -lt 65536 && $(awk -F '\t' '$1=="format"{print $2}' "$stage/manifest.tsv") == oss-transfer-go-v1 ]] || fail 'Invalid release manifest'
version=$(awk -F '\t' '$1=="version"{print $2}' "$stage/manifest.tsv")
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || fail 'Invalid release version'
commit=$(awk -F '\t' '$1=="commit"{print $2}' "$stage/manifest.tsv")
if [[ -n "$local_package_dir" && "$version_requested" != latest && "$version_requested" != "$version" ]]; then
  fail "$(say "Local release files are version $version, not $version_requested; remove --version or use --source to download" "本地安装包版本为 ${version}，不是 ${version_requested}；请去掉 --version，或使用 --source 在线下载")"
fi
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
  [[ -z "$root_dir" ]] || native_args+=(--root-dir "$root_dir")
  [[ -z "$config_dir" ]] || native_args+=(--config-dir "$config_dir")
  ((force == 0)) || native_args+=(--force)
  if ((upgrade)); then
    native_args+=(--upgrade)
    if [[ -n "$package_dir" ]]; then native_args+=(--package-dir "$package_dir"); else native_args+=(--release-url "$release_url"); fi
    for field in role install_dir data_dir cli_path service_name; do
      value=${!field}
      [[ -z "$value" ]] || native_args+=("--${field//_/-}" "$value")
    done
    ((user_service == 0)) || native_args+=(--user)
    ((yes == 0)) || native_args+=(--yes)
    bash "$stage/native-install.sh" "${native_args[@]}" | tee "$stage/native-install.log" | native_output
    ((with_agent == 0)) || install_local_agent
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
    if [[ "$role" != agent && -z "$cli_path" ]]; then cli_path=$owned_cli; fi
  fi
  # The native installer chooses an unused global CLI for Agents and preserves
  # a CLI already owned by another installation on the same host.
  if [[ -z "$cli_path" ]]; then
    if [[ "$role" == master ]]; then
      if ((user_service)); then cli_path=$HOME/.local/bin/oss-transfer-cli
      else cli_path=/usr/local/bin/oss-transfer-cli; fi
    fi
  fi
  if [[ -e "$cli_path" && "$cli_path" != "$owned_cli" && "$replace_cli" == 0 && "$yes" == 0 ]] && [[ -z $(retained_role_config "$role") ]]; then
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
  bash "$stage/native-install.sh" "${native_args[@]}" | tee "$stage/native-install.log" | native_output
  ((with_agent == 0)) || install_local_agent
  exit 0
fi

[[ "$os" == Linux && "$user_service" == 0 ]] || fail 'The Compose installer requires Linux and root'
((upgrade == 0)) || fail 'For an existing Compose installation, run docker compose pull and docker compose up -d in its directory'
# --force only skips the native same-version check; Compose never skips.
((force == 0)) || say 'Note: --force applies to native installations only; it is ignored for Docker Compose.' '提示：--force 仅适用于原生安装，Docker Compose 安装会忽略它。'
[[ ( -z "$package_dir" || "$prepared_github" == 1 || -n "$local_package_dir" ) && ${#native_args[@]} == 0 && -z "$listen" && -z "$adopt_config" && -z "$cli_path" && "$replace_cli" == 0 ]] || fail 'Native configuration/package options cannot be used with Compose'
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
compose_command="docker compose -p $service_name -f $install_dir/docker-compose.yml"
if [[ -f "$install_dir/docker-compose.yml" ]] && command -v docker >/dev/null 2>&1 &&
  [[ -n $(docker ps -aq --filter "label=com.docker.compose.project=$service_name" 2>/dev/null || true) ]]; then
  fail "$(say "An installation already exists in $install_dir. To upgrade it, run: $compose_command pull && $compose_command up -d" "$install_dir 中已有安装。如需升级，请运行：$compose_command pull && $compose_command up -d")"
fi
# Data kept by --uninstall --keep-data, by docker compose down or by an
# unfinished join is reused as it is; it is never offered for deletion.
reuse_data=0
if [[ -f "$data_dir/master.db" || -f "$data_dir/agent.db" ]]; then
  if [[ "$role" == master ]]; then other_role=agent; else other_role=master; fi
  [[ -f "$data_dir/$role.db" && ! -e "$data_dir/$other_role.db" ]] ||
    fail "$(say "$data_dir holds $other_role data, not $role data. It was not changed; choose another --data-dir." "$data_dir 中是 $other_role 的数据，不是 $role 的数据。未做任何更改，请改用其他 --data-dir。")"
  data_users=''
  if command -v docker >/dev/null 2>&1; then data_users=$(docker ps -a --filter "volume=$data_dir" --format '{{.Names}}' 2>/dev/null | tr '\n' ' ' || true); fi
  [[ -z "${data_users// /}" ]] ||
    fail "$(say "$data_dir is in use by container(s): $data_users. It was not changed; stop and remove that installation first." "$data_dir 正被容器使用：${data_users}。未做任何更改，请先停止并删除该安装。")"
  if [[ -e "$install_dir" && -n $(ls -A "$install_dir") ]]; then
    { [[ $(ls -A "$install_dir") == docker-compose.yml ]] && grep -Fqx "    command: ['oss-transfer-$role', '--data-dir', '/data']" "$install_dir/docker-compose.yml"; } ||
      fail "$(say "$install_dir contains other files. Nothing was changed; choose an empty --install-dir to reuse the data in $data_dir." "$install_dir 中有其他文件。未做任何更改，请指定一个空的 --install-dir 以沿用 $data_dir 中的数据。")"
  fi
  reuse_data=1
  say "Existing $role data found in $data_dir; it is reused, not removed or reset." "在 $data_dir 中找到已有的 $role 数据，将直接沿用，不会删除或重置。"
fi
for path in "$install_dir" "$data_dir"; do
  ((reuse_data == 0)) || break
  [[ ! -e "$path" || -d "$path" && -z $(ls -A "$path") ]] && continue
  # Earlier releases left these behind when enrollment failed. Neither
  # directory holds a database here, so removing them loses no data.
  fail "$(say "$path contains files from an earlier attempt that did not finish. Remove them, then run the same command again:" "$path 中有上次未完成安装留下的文件。请先删除，再重新运行同一条命令：")
  $compose_command down; rm -rf $install_dir $data_dir"
done
enrollment_input
if [[ "$role" == agent ]]; then
  valid_master_origin "$master_url" || fail 'Copy an Agent join command from Nodes > Add node'
  if ((token_stdin)); then IFS= read -r TRANSFER_JOIN_TOKEN || [[ -n "${TRANSFER_JOIN_TOKEN:-}" ]]; fi
  [[ -n "${TRANSFER_JOIN_TOKEN:-}" ]] || fail 'Copy an Agent join command from Nodes > Add node'
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
[[ -e "$install_dir" ]] || created_install_dir=1
[[ -e "$data_dir" ]] || created_data_dir=1
compose_rollback=1
mkdir -p "$install_dir" "$data_dir"
chmod 700 "$install_dir" "$data_dir"
chown 65532:65532 "$data_dir"
compose_file=$install_dir/docker-compose.yml
cat > "$compose_file" <<EOF
# Written by the OSS Transfer installer. Manage it with: docker compose -p $service_name -f $compose_file COMMAND
# oss-transfer-project: $service_name
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
  if ! "${compose[@]}" run --rm -e OSS_TRANSFER_JOIN_TOKEN agent oss-transfer-agent --data-dir /data --master-url "$master_url" --enroll-only 2> "$stage/enroll.log"; then
    unset OSS_TRANSFER_JOIN_TOKEN TRANSFER_JOIN_TOKEN
    cat "$stage/enroll.log" >&2
    if grep -qi 'permission denied' "$stage/enroll.log"; then
      say "The container user (UID 65532) cannot write $data_dir. Run this installer directly on the Docker host, not inside another container, so the directory it prepares is the one Docker mounts." "容器用户（UID 65532）无法写入 ${data_dir}。请直接在 Docker 主机上运行安装脚本，不要在其他容器内运行，确保脚本准备的目录就是 Docker 挂载的目录。" >&2
    elif grep -q 'HTTP 403' "$stage/enroll.log"; then
      say 'The invitation is invalid, expired or already used. Generate a new one in Nodes > Add node.' '加入凭证无效、已过期或已被使用。请在“执行节点 → 添加节点”中重新生成。' >&2
    elif grep -qiE 'connection refused|no such host|timeout|deadline exceeded|network is unreachable' "$stage/enroll.log"; then
      say "The Agent container cannot reach the Master at $master_url. Inside a container, localhost is the container itself; use an address of the Master that this host can reach." "Agent 容器无法连接 Master（${master_url}）。容器内的 localhost 指容器自身，请使用本机可以访问的 Master 地址。" >&2
    fi
    # A lost response or an unavailable Master may already have committed the
    # join, so the pending identity stays for an idempotent retry. Only a
    # definitive rejection, or a request that never reached the Master, rolls
    # back.
    if [[ -f "$data_dir/agent.db" ]] && ! grep -qiE 'HTTP (403|409)|dial tcp|no such host|connection refused|network is unreachable|x509' "$stage/enroll.log"; then
      compose_rollback=0
      fail "$(say "Agent enrollment did not finish; the Master may already have accepted this Agent. Its identity is kept in $data_dir. Fix the problem above, then run the same command again; it retries with the same identity." "Agent 加入未完成，Master 可能已接受此节点。身份信息保留在 ${data_dir}。请解决上述问题后重新运行同一条命令，将以同一身份重试。")"
    fi
    fail "$(say 'Agent enrollment failed' 'Agent 加入失败')"
  fi
  unset OSS_TRANSFER_JOIN_TOKEN TRANSFER_JOIN_TOKEN
  cat "$stage/enroll.log" >&2
  # The invitation is used now; keep the enrolled identity if starting fails.
  compose_rollback=0
fi
if ! "${compose[@]}" up -d; then
  if [[ "$role" == agent ]]; then
    fail "$(say "The Agent is enrolled, but its container did not start. Its identity is kept in $data_dir; after fixing the problem run: docker compose -p $service_name -f $compose_file up -d" "Agent 已加入，但容器未能启动。身份信息保留在 ${data_dir}；解决问题后运行：docker compose -p $service_name -f $compose_file up -d")"
  fi
  fail "$(say 'The Master container did not start' 'Master 容器未能启动')"
fi
compose_rollback=0
container=$("${compose[@]}" ps -q "$role")
[[ -n "$container" ]] || fail 'Container was not created'
if [[ "$role" == master ]]; then
  deadline=$((SECONDS+120)); ready=0
  while ((SECONDS < deadline)); do
    if [[ $(docker inspect "$container" --format '{{.State.Status}}') == running ]] && curl -fsS --max-time 3 "http://127.0.0.1:$port/healthz" >/dev/null; then ready=1; break; fi
    sleep 1
  done
  ((ready)) || fail "Master did not become ready; inspect logs in $install_dir"
  printf '\n=== %s ===\n' "$(say 'Master installed successfully' 'Master 安装成功')"
  access_host=$(hostname -I 2>/dev/null | tr ' ' '\n' | awk '/^[0-9]+\./ && $0 !~ /^127\./ {print; exit}') || access_host=
  printf '%s: http://%s:%s\n' "$(say 'Web UI' '访问地址')" "${access_host:-localhost}" "$port"
  # This is a newly created container. An existing data directory does not
  # generate a new password, and unrelated log messages are never printed.
  initial_password=$(docker logs "$container" 2>&1 |
    sed -n 's/^Initial password: \([A-Za-z0-9_-][A-Za-z0-9_-]*\)$/\1/p' | tail -n 1)
  if [[ -n "$initial_password" ]]; then
    printf '%s: admin\n%s: %s\n' "$(say 'Initial administrator' '初始管理员')" "$(say 'Initial password' '初始密码')" "$initial_password"
  else
    # Older image versions still use a one-time setup token.
    setup_token=$(docker logs "$container" 2>&1 |
      sed -n 's/^Open the console to complete setup\. Setup token: \([A-Za-z0-9_-][A-Za-z0-9_-]*\)$/\1/p' | tail -n 1)
    [[ -z "$setup_token" ]] || printf '%s: %s\n' "$(say 'Setup token' '初始化密钥')" "$setup_token"
    ((reuse_data == 0)) || [[ -n "$setup_token" ]] || say 'Existing data was reused: sign in with your existing accounts.' '已沿用原有数据：请使用原有账号登录。'
  fi
else
  sleep 2
  [[ $(docker inspect "$container" --format '{{.State.Status}}') == running ]] || fail 'Agent container did not stay running'
  say 'Check the Agent connection on the Master node page.' '请在主节点的执行节点页面确认连接状态。'
fi
printf 'Compose: %s\n' "$compose_file"
printf 'docker compose -p %s -f "%s" logs\n' "$service_name" "$compose_file"
exit 0
}
