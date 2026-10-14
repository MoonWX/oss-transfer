#!/usr/bin/env bash
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$root"
run_tests=0
while (($#)); do
  case "$1" in
    --all) export GO_PLATFORMS='linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64';;
    --test) run_tests=1;;
    --help|-h)
      cat <<'HELP'
Usage: ./build.sh [--all] [--test]
  --all   Build Linux, macOS and Windows archives for amd64 and arm64.
          Without this flag, build for the current OS and architecture.
  --test  Run backend and CLI tests before packaging.
Archives and checksums are written to output/go-development/.
Set GO_PLATFORMS to build a specific list, such as 'linux/amd64 linux/arm64'.
HELP
      exit 0;;
    *) printf 'Unknown option: %s\n' "$1" >&2; exit 1;;
  esac
  shift
done
for program in go node npm python3; do
  command -v "$program" >/dev/null || { printf 'Missing build tool: %s\n' "$program" >&2; exit 1; }
done
npm ci --prefix frontend
if ((run_tests)); then
  go test ./...
  (cd cli-go && go test ./...)
fi
exec bash scripts/build-go.sh
