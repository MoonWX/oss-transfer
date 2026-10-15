#!/usr/bin/env bash
set -euo pipefail
root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$root"
run_tests=0
while (($#)); do
  case "$1" in
    --all) BUILD_PLATFORMS="$(python3 deploy/native/platforms.py --list)"; export BUILD_PLATFORMS;;
    --test) run_tests=1;;
    --help|-h)
      cat <<'HELP'
Usage: ./build.sh [--all] [--test]
  --all   Build every target listed in deploy/native/platforms.json.
          Without this flag, build for the current OS and architecture.
  --test  Run backend and CLI tests before packaging.
Archives and checksums are written to output/packages/.
Set BUILD_PLATFORMS to build a specific list, such as 'linux/amd64 linux/arm64'.
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
  (cd cli && go test ./...)
fi
exec bash scripts/build.sh
