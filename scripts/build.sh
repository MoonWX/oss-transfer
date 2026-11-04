#!/usr/bin/env bash
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
python3 scripts/sync-installer-source.py --check
version="$(cat VERSION)"
revision="$(git rev-parse HEAD)"
short_revision="$(git rev-parse --short HEAD)"
branch="$(git branch --show-current)"
build_time="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
artifact_base="${AGENT_ARTIFACT_BASE:-https://moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer}"
artifact_version="${AGENT_ARTIFACT_VERSION:-v$version}"
agent_image="${AGENT_DOCKER_IMAGE:-moonwx/osstransfer:$version}"
if [[ -n "$artifact_base" || -n "$artifact_version" ]]; then
  [[ "$artifact_base" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$ && "$artifact_version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { printf 'Invalid fixed Agent download URL/version\n' >&2; exit 1; }
fi
[[ -z "$agent_image" || "$agent_image" =~ ^[a-zA-Z0-9][a-zA-Z0-9._:/@-]+$ ]] || { printf 'Invalid Agent image reference\n' >&2; exit 1; }
if ! git diff --quiet HEAD || test -n "$(git ls-files --others --exclude-standard)"; then
  revision="$revision-dirty"
  short_revision="$short_revision-dirty"
fi
host_os="${GOOS:-$(go env GOOS)}"
host_arch="${GOARCH:-$(go env GOARCH)}"
if [[ "$host_arch" == arm ]]; then
  host_arm="${GOARM:-$(go env GOARM)}"
  host_arch="armv${host_arm%%,*}"
fi
platforms="${BUILD_PLATFORMS:-$host_os/$host_arch}"
for platform in $platforms; do
  python3 deploy/native/platforms.py --go-env "$platform" >/dev/null
done
mkdir -p output/packages
lock="$root/output/.build-lock"
if ! mkdir "$lock" 2>/dev/null; then
  printf 'Another Go build holds %s; check its PID before removing a stale lock.\n' "$lock" >&2
  exit 1
fi
printf '%s\n' "$$" > "$lock/pid"
stage=""
cleanup() {
  # Stop platform builds before deleting the stage they write into.
  local pids
  pids="$(jobs -p)"
  # shellcheck disable=SC2086 # One PID per word.
  if [[ -n "$pids" ]]; then kill $pids 2>/dev/null || true; wait 2>/dev/null || true; fi
  if [[ -n "$stage" ]]; then rm -rf "$stage"; fi
  rm -f "$lock/pid"
  rmdir "$lock"
}
trap cleanup EXIT
stage="$(mktemp -d "$root/output/.build.XXXXXX")"
(
  cd frontend
  VITE_BUILD_COMMIT="$short_revision" VITE_BUILD_TIME="$build_time" npm run build
)
# This directory contains generated embed inputs only. Replacing it removes old
# hashed JavaScript assets that otherwise keep growing the single-file binary.
rm -rf internal/webui/assets
mkdir -p internal/webui/assets
cp -R frontend/dist/. internal/webui/assets/
build_platform() {
  local platform="$1" target_os target_arch go_arch go_arm name package extension program link_flags archive
  target_os="${platform%/*}"
  target_arch="${platform#*/}"
  IFS=$'\t' read -r target_os go_arch go_arm < <(python3 deploy/native/platforms.py --go-env "$platform")
  name="oss-transfer-$target_os-$target_arch"
  package="$stage/$name"
  mkdir -p "$package"
  extension=""
  if [[ "$target_os" == windows ]]; then extension=.exe; fi
  for program in master agent; do
    link_flags="-s -w -X main.version=$version -X main.commit=$short_revision -X main.builtAt=$build_time"
    if [[ "$program" == master ]]; then
      link_flags+=" -X main.agentArtifactBase=$artifact_base -X main.agentArtifactVersion=$artifact_version -X main.agentDockerImage=$agent_image"
    fi
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$go_arch" GOARM="$go_arm" GOAMD64=v1 GOARM64=v8.0 GORISCV64=rva20u64 go build -tags embedded -trimpath \
      -ldflags "$link_flags" \
      -o "$package/oss-transfer-$program$extension" "./cmd/oss-transfer-$program"
  done
  (
    cd cli
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$go_arch" GOARM="$go_arm" GOAMD64=v1 GOARM64=v8.0 GORISCV64=rva20u64 go build -trimpath \
      -ldflags "-s -w -X main.version=$version -X main.branch=$branch -X main.commit=$short_revision -X main.buildTime=$build_time" \
      -o "$package/oss-transfer-cli$extension" .
  )
  printf 'Version: %s\nCommit: %s\nFullCommit: %s\nBuildTime: %s\nPlatform: %s\n' \
    "$version" "$short_revision" "$revision" "$build_time" "$platform" > "$package/VERSION"
  cp README.md README.zh-CN.md "$package/"
  python3 - "$package" "${revision%-dirty}" <<'PY'
from pathlib import Path
import re
import sys
for name in ('README.md', 'README.zh-CN.md'):
    path = Path(sys.argv[1]) / name
    raw = 'https://raw.githubusercontent.com/MoonWX/oss-transfer/' + sys.argv[2] + '/'
    # Screenshots are not packaged; point images at the matching commit.
    text = re.sub(r'(\]\(|src=")(docs/images/[^)"]+)', lambda match: match[1] + raw + match[2],
                  path.read_text(encoding='utf-8'))
    text = re.sub(r'\]\(((?:docs|deploy|\.github)/[^)]+|docker-compose\.yml|\.env\.example)\)',
                  lambda match: '](https://github.com/MoonWX/oss-transfer/blob/' + sys.argv[2] + '/' + match[1] + ')',
                  text)
    path.write_text(text, encoding='utf-8')
PY
  (cd "$package" && shasum -a 256 oss-transfer-* VERSION > SHA256SUMS)
  if [[ "$target_os" == windows ]]; then
    (cd "$stage" && zip -X -q -r "$name.zip" "$name")
    archive="$name.zip"
  else
    tar -czf "$stage/$name.tar.gz" -C "$stage" "$name"
    archive="$name.tar.gz"
  fi
  (cd "$stage" && shasum -a 256 "$archive" > "$archive.sha256")
  mv "$stage/$archive" "$stage/$archive.sha256" "$root/output/packages/"
  printf '%s\n' "$root/output/packages/$archive" > "$stage/${platform//\//-}.archive"
}
# shellcheck disable=SC2206 # Platform names are validated words.
platform_list=($platforms)
if [[ ${#platform_list[@]} -eq 1 ]]; then
  build_platform "${platform_list[0]}"
else
  # Each platform is mostly a serial link; overlap them, logging per platform
  # so failures stay readable. FIFO waits keep this working on bash 3.2.
  parallel="${BUILD_PARALLEL:-4}"
  [[ "$parallel" =~ ^[1-9][0-9]*$ ]] || { printf 'BUILD_PARALLEL must be a positive integer\n' >&2; exit 1; }
  running=()
  failed=()
  finish() {
    local pid="${running[0]%%:*}" platform="${running[0]#*:}"
    running=("${running[@]:1}")
    if wait "$pid"; then
      printf 'Built %s\n' "$platform"
    else
      failed+=("$platform")
    fi
  }
  for platform in "${platform_list[@]}"; do
    if [[ ${#running[@]} -ge $parallel ]]; then finish; fi
    build_platform "$platform" > "$stage/${platform//\//-}.log" 2>&1 &
    running+=("$!:$platform")
  done
  while [[ ${#running[@]} -gt 0 ]]; do finish; done
  if [[ ${#failed[@]} -gt 0 ]]; then
    for platform in "${failed[@]}"; do
      printf '\n=== Build log for %s ===\n' "$platform" >&2
      cat "$stage/${platform//\//-}.log" >&2
    done
    printf 'Failed to build: %s\n' "${failed[*]}" >&2
    exit 1
  fi
fi
for platform in "${platform_list[@]}"; do
  cat "$stage/${platform//\//-}.archive"
done
