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
artifact_base="${GO_AGENT_ARTIFACT_BASE:-https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer}"
artifact_version="${GO_AGENT_ARTIFACT_VERSION:-v$version}"
agent_image="${GO_AGENT_DOCKER_IMAGE:-moonwx/osstransfer:$version}"
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
platforms="${GO_PLATFORMS:-$host_os/$host_arch}"
for platform in $platforms; do
  python3 deploy/go/platforms.py --go-env "$platform" >/dev/null
done
mkdir -p output/go-development
lock="$root/output/.go-build-lock"
if ! mkdir "$lock" 2>/dev/null; then
  printf 'Another Go build holds %s; check its PID before removing a stale lock.\n' "$lock" >&2
  exit 1
fi
printf '%s\n' "$$" > "$lock/pid"
stage=""
cleanup() { if [[ -n "$stage" ]]; then rm -rf "$stage"; fi; rm -f "$lock/pid"; rmdir "$lock"; }
trap cleanup EXIT
stage="$(mktemp -d "$root/output/.go-build.XXXXXX")"
(
  cd frontend
  VITE_BUILD_COMMIT="$short_revision" VITE_BUILD_TIME="$build_time" npm run build
)
# This directory contains generated embed inputs only. Replacing it removes old
# hashed JavaScript assets that otherwise keep growing the single-file binary.
rm -rf internal/webui/assets
mkdir -p internal/webui/assets
cp -R frontend/dist/. internal/webui/assets/
for platform in $platforms; do
  target_os="${platform%/*}"
  target_arch="${platform#*/}"
  IFS=$'\t' read -r target_os go_arch go_arm < <(python3 deploy/go/platforms.py --go-env "$platform")
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
    cd cli-go
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
    text = re.sub(r'\]\(((?:docs|deploy|\.github)/[^)]+|docker-compose\.yml|\.env\.example)\)',
                  lambda match: '](https://github.com/MoonWX/oss-transfer/blob/' + sys.argv[2] + '/' + match[1] + ')',
                  path.read_text(encoding='utf-8'))
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
  mv "$stage/$archive" "$stage/$archive.sha256" "$root/output/go-development/"
  printf '%s\n' "$root/output/go-development/$archive"
done
