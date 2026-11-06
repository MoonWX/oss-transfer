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
