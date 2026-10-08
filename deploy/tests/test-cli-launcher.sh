#!/bin/bash
set -euo pipefail

source_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture_dir="$(mktemp -d)"
trap 'rm -rf "$fixture_dir"' EXIT
mkdir -p "$fixture_dir/mock-bin" "$fixture_dir/layout with spaces/deploy" "$fixture_dir/layout with spaces/cli-go"
launcher="$fixture_dir/layout with spaces/deploy/oss-transfer-cli"
cp "$source_dir/oss-transfer-cli" "$launcher"
chmod +x "$launcher"

cat > "$fixture_dir/mock-bin/uname" <<'MOCK'
#!/bin/bash
case "$1" in
    -s) printf '%s\n' "${CLI_TEST_OS:-Linux}" ;;
    -m) printf '%s\n' "${CLI_TEST_ARCH:-x86_64}" ;;
    *) exit 1 ;;
esac
MOCK
cat > "$fixture_dir/fake-cli" <<'MOCK'
#!/bin/bash
printf '%s\0' "$@" > "$CLI_TEST_ARGS"
printf '%s\n' "$0" > "$CLI_TEST_SELECTED"
exit "${CLI_TEST_EXIT:-0}"
MOCK
# Any accidental fallback to PATH or the previous curl implementation fails.
cat > "$fixture_dir/mock-bin/oss-transfer-cli" <<'MOCK'
#!/bin/bash
printf 'unexpected PATH fallback\n' > "$CLI_TEST_UNSAFE"
exit 99
MOCK
cp "$fixture_dir/mock-bin/oss-transfer-cli" "$fixture_dir/mock-bin/curl"
chmod +x "$fixture_dir/mock-bin/"* "$fixture_dir/fake-cli"
export PATH="$fixture_dir/mock-bin:$PATH"
export CLI_TEST_ARGS="$fixture_dir/args" CLI_TEST_SELECTED="$fixture_dir/selected" CLI_TEST_UNSAFE="$fixture_dir/unsafe"
export CLI_TEST_OS=Linux CLI_TEST_ARCH=x86_64 CLI_TEST_EXIT=0
unset OSS_TRANSFER_OUTPUT_DIR

check_exit() {
    local wanted=$1
    shift
    local status=0
    "$launcher" "$@" > "$fixture_dir/stdout" 2> "$fixture_dir/stderr" || status=$?
    if [[ "$status" -ne "$wanted" ]]; then
        echo "launcher exit mismatch: expected $wanted, got $status" >&2
        cat "$fixture_dir/stderr" >&2
        exit 1
    fi
    [[ ! -e "$CLI_TEST_UNSAFE" ]]
}

# The adjacent matching binary wins, preserves every argument, and propagates status.
amd64="$fixture_dir/layout with spaces/deploy/oss-transfer-cli-amd64"
cp "$fixture_dir/fake-cli" "$amd64"
check_exit 0 --host http://localhost:1685 task list --search 'a b/*' '' --json
printf '%s\0' --host http://localhost:1685 task list --search 'a b/*' '' --json > "$fixture_dir/expected-args"
cmp "$CLI_TEST_ARGS" "$fixture_dir/expected-args"
[[ "$(cat "$CLI_TEST_SELECTED")" == "$amd64" ]]
for code in 1 3 7 42; do
    export CLI_TEST_EXIT=$code
    check_exit "$code" task report 224
done
export CLI_TEST_EXIT=0

# Executable permissions are required; an unusable adjacent file may use a parent binary.
chmod -x "$amd64"
parent="$fixture_dir/layout with spaces/oss-transfer-cli-amd64"
cp "$fixture_dir/fake-cli" "$parent"
check_exit 0 task list
[[ "$(cat "$CLI_TEST_SELECTED")" == "$fixture_dir/layout with spaces/deploy/../oss-transfer-cli-amd64" ]]
rm "$parent" "$amd64"

# New release output wins over an old binary retained in the source checkout.
repo_binary="$fixture_dir/layout with spaces/cli-go/oss-transfer-cli-amd64"
cp "$fixture_dir/fake-cli" "$repo_binary"
release_dir="$fixture_dir/layout with spaces/output/oss-transfer"
mkdir -p "$release_dir"
cp "$fixture_dir/fake-cli" "$release_dir/oss-transfer-cli-amd64"
check_exit 0 task list
[[ "$(cat "$CLI_TEST_SELECTED")" == "$fixture_dir/layout with spaces/deploy/../output/oss-transfer/oss-transfer-cli-amd64" ]]

# An explicit build output directory is also respected, including spaces.
export OSS_TRANSFER_OUTPUT_DIR="$fixture_dir/custom output"
mkdir -p "$OSS_TRANSFER_OUTPUT_DIR/oss-transfer"
cp "$fixture_dir/fake-cli" "$OSS_TRANSFER_OUTPUT_DIR/oss-transfer/oss-transfer-cli-amd64"
check_exit 0 task list
[[ "$(cat "$CLI_TEST_SELECTED")" == "$OSS_TRANSFER_OUTPUT_DIR/oss-transfer/oss-transfer-cli-amd64" ]]
unset OSS_TRANSFER_OUTPUT_DIR

# The legacy source binary is still a fallback when the release is unavailable.
chmod -x "$release_dir/oss-transfer-cli-amd64"
check_exit 0 task list
[[ "$(cat "$CLI_TEST_SELECTED")" == "$fixture_dir/layout with spaces/deploy/../cli-go/oss-transfer-cli-amd64" ]]
rm "$repo_binary" "$release_dir/oss-transfer-cli-amd64"

for architecture in aarch64 arm64; do
    export CLI_TEST_ARCH=$architecture
    arm64="$fixture_dir/layout with spaces/deploy/oss-transfer-cli-arm64"
    cp "$fixture_dir/fake-cli" "$arm64"
    check_exit 0 --help
    [[ "$(cat "$CLI_TEST_SELECTED")" == "$arm64" ]]
    rm "$arm64"
done

export CLI_TEST_ARCH=x86_64
check_exit 127 clear
[[ ! -s "$fixture_dir/stdout" ]]
grep -q 'build.sh' "$fixture_dir/stderr"
# A binary name accidentally symlinked to this launcher must not recurse.
ln -s "$launcher" "$amd64"
check_exit 127 clear
rm "$amd64"
export CLI_TEST_ARCH=riscv64
check_exit 126 task list
grep -q 'riscv64' "$fixture_dir/stderr"
export CLI_TEST_ARCH=x86_64 CLI_TEST_OS=Darwin
check_exit 126 --help
grep -q 'Linux' "$fixture_dir/stderr"

bash -n "$source_dir/oss-transfer-cli"
echo "CLI launcher checks passed (arguments, exit codes, architecture, release preference, missing binary, no PATH fallback)"
