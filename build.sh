#!/bin/bash
set -euo pipefail

TEST_MODE=none
usage() { echo 'Usage: build.sh [--test|--test=smoke]' >&2; }
case "${1:-}" in
    '') [ "$#" -eq 0 ] || { usage; exit 2; } ;;
    --test) [ "$#" -eq 1 ] || { usage; exit 2; }; TEST_MODE=full ;;
    --test=smoke) [ "$#" -eq 1 ] || { usage; exit 2; }; TEST_MODE=smoke ;;
    --help|-h) [ "$#" -eq 1 ] || { usage; exit 2; }
        printf 'Usage: build.sh [--test|--test=smoke]\n  --test        Run all server and CLI tests before packaging.\n  --test=smoke  Run core server tests and CLI tests before packaging.\n  BUILD_TEST_COMPONENTS=server,cli|server|cli|none selects test components.\n'
        exit 0 ;;
    *) usage; exit 2 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_SERVER=0
TEST_CLI=0
case "${BUILD_TEST_COMPONENTS-server,cli}" in
    server,cli) TEST_SERVER=1; TEST_CLI=1 ;;
    server) TEST_SERVER=1 ;;
    cli) TEST_CLI=1 ;;
    none) ;;
    *) echo 'BUILD_TEST_COMPONENTS must be server,cli, server, cli, or none' >&2; exit 2 ;;
esac
SMOKE_TESTS=""
if [ "$TEST_MODE" = smoke ] && [ "$TEST_SERVER" -eq 1 ]; then
    smoke_file="$SCRIPT_DIR/.github/ci/server-smoke-tests.txt"
    [ -r "$smoke_file" ] || { echo "Missing server smoke test list: $smoke_file" >&2; exit 2; }
    smoke_classes=""
    while IFS= read -r test_selection || [ -n "$test_selection" ]; do
        case "$test_selection" in ''|'#'*) continue ;; esac
        if [[ ! "$test_selection" =~ ^[[:alpha:]_][[:alnum:]_]*(\.[[:alpha:]_][[:alnum:]_]*)+#[[:alpha:]_][[:alnum:]_]*(\+[[:alpha:]_][[:alnum:]_]*)*$ ]]; then
            echo "Invalid server smoke test selection: $test_selection" >&2; exit 2
        fi
        test_class="${test_selection%%#*}"
        test_source="$SCRIPT_DIR/src/test/java/${test_class//.//}.java"
        [ -f "$test_source" ] || { echo "Missing server smoke test class: $test_class" >&2; exit 2; }
        case ",$smoke_classes," in *",$test_class,"*) echo "Duplicate server smoke test class: $test_class" >&2; exit 2 ;; esac
        smoke_classes="${smoke_classes:+$smoke_classes,}$test_class"
        IFS=+ read -r -a smoke_methods <<< "${test_selection#*#}"
        for test_method in "${smoke_methods[@]}"; do
            grep -Eq "public[[:space:]]+void[[:space:]]+${test_method}[[:space:]]*\(" "$test_source" || \
                { echo "Missing server smoke test method: $test_class#$test_method" >&2; exit 2; }
        done
        SMOKE_TESTS="${SMOKE_TESTS:+$SMOKE_TESTS,}$test_selection"
    done < "$smoke_file"
    [ -n "$SMOKE_TESTS" ] || { echo 'Server smoke test list is empty' >&2; exit 2; }
fi
OUTPUT_DIR="${OSS_TRANSFER_OUTPUT_DIR:-$SCRIPT_DIR/output}"
mkdir -p "$OUTPUT_DIR"
OUTPUT_DIR="$(cd "$OUTPUT_DIR" && pwd)"
PACKAGE_NAME="oss-transfer"
VERSION="$(sed -n 's/^[[:space:]]*<version>\([^<]*\)<\/version>.*/\1/p' "$SCRIPT_DIR/pom.xml" | head -n 1)"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.-]+)?$ ]] || { echo "Invalid project version" >&2; exit 1; }
STAGE_DIR="$(mktemp -d "$OUTPUT_DIR/.build.XXXXXX")"
PACKAGE_DIR="$STAGE_DIR/$PACKAGE_NAME"
mkdir -p "$PACKAGE_DIR"
BACKUP_DIR=""
PUBLISH_LOCK="$OUTPUT_DIR/.publish.lock"
PUBLISH_LOCK_HELD=0
PUBLISHING=0
PUBLISHED=0
PACKAGE_FILES=("$PACKAGE_NAME" "$PACKAGE_NAME.tar.gz" "$PACKAGE_NAME.tar.gz.sha256")
cleanup() {
    local status=$?
    local name rollback_failed=0 cleanup_failed=0
    trap - EXIT
    trap '' INT TERM
    # A failed restore must not prevent recovery of the remaining artifacts.
    set +e
    if [ "$PUBLISHING" -eq 1 ] && [ "$PUBLISHED" -eq 0 ]; then
        for name in "${PACKAGE_FILES[@]}"; do
            # Only remove a newly published entry if its original was backed up,
            # or this build recorded that there was no original entry.
            if [ -e "$BACKUP_DIR/$name" ] || [ -L "$BACKUP_DIR/$name" ] || [ -e "$BACKUP_DIR/.absent-$name" ]; then
                if ! rm -rf -- "${OUTPUT_DIR:?}/$name"; then
                    rollback_failed=1
                    continue
                fi
                if [ -e "$BACKUP_DIR/$name" ] || [ -L "$BACKUP_DIR/$name" ]; then
                    if ! mv -- "$BACKUP_DIR/$name" "$OUTPUT_DIR/$name"; then
                        rollback_failed=1
                    fi
                fi
            fi
        done
    fi
    if ! rm -rf -- "$STAGE_DIR"; then
        echo "无法清理构建临时目录: $STAGE_DIR" >&2
        cleanup_failed=1
    fi
    if [ "$rollback_failed" -eq 1 ]; then
        echo "发布回滚未完成，未恢复的旧文件保存在: $BACKUP_DIR" >&2
        echo "请检查并恢复后移除发布锁: $PUBLISH_LOCK" >&2
        cleanup_failed=1
    elif [ "$PUBLISH_LOCK_HELD" -eq 1 ]; then
        if ! rmdir -- "$PUBLISH_LOCK"; then
            echo "无法清理发布锁: $PUBLISH_LOCK" >&2
            cleanup_failed=1
        fi
    fi
    if [ "$status" -eq 0 ] && [ "$cleanup_failed" -ne 0 ]; then
        status=1
    fi
    exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "$SCRIPT_DIR"
BRANCH=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "unknown")
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
COMMIT_MSG=$(git log -1 --pretty=format:"%s" 2>/dev/null || echo "unknown")
COMMIT_TIME=$(git log -1 --pretty=format:"%cI" 2>/dev/null || echo "unknown")
BUILD_TIME=$(date -u "+%Y-%m-%dT%H:%M:%SZ")

echo "[1/6] Building console..."
(cd "$SCRIPT_DIR/frontend" && npm ci && npm run build)
echo "[2/6] Building server..."
if [ "$TEST_MODE" != none ] && [ "$TEST_SERVER" -eq 1 ]; then
    if [ "$TEST_MODE" = smoke ]; then
        mvn clean verify -q "-Dtest=$SMOKE_TESTS" -Dsurefire.failIfNoSpecifiedTests=true
    else
        mvn clean verify -q
    fi
else
    mvn clean package -q -DskipTests
fi
cp "target/oss-transfer.jar" "$PACKAGE_DIR/"

echo "[3/6] Building CLI..."
if [ "$TEST_MODE" != none ] && [ "$TEST_CLI" -eq 1 ]; then
    (cd "$SCRIPT_DIR/cli-go" && go test ./...)
fi
LDFLAGS="-X main.version=$VERSION -X main.branch=$BRANCH -X main.commit=$COMMIT -X main.buildTime=$BUILD_TIME"
for architecture in amd64 arm64; do
    (cd "$SCRIPT_DIR/cli-go" && CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go build -ldflags="$LDFLAGS" -o "$PACKAGE_DIR/oss-transfer-cli-$architecture" .)
done

echo "[4/6] Preparing deployment files..."
cp README.md "$PACKAGE_DIR/"
cp README.zh-CN.md "$PACKAGE_DIR/"
cp -R deploy "$PACKAGE_DIR/"
cp Dockerfile .dockerignore docker-compose.yml .env.example "$PACKAGE_DIR/"
cp LICENSE "$PACKAGE_DIR/" 2>/dev/null || true
cat > "$PACKAGE_DIR/VERSION" <<VERSION
Branch: $BRANCH
Commit: $COMMIT
Message: $COMMIT_MSG
Time: $COMMIT_TIME
Version: $VERSION
BuildTime: $BUILD_TIME
VERSION
cp "$PACKAGE_DIR/VERSION" "$PACKAGE_DIR/CLI-VERSION"
cp "$PACKAGE_DIR/VERSION" "$PACKAGE_DIR/APPLICATION-VERSION"
chmod +x "$PACKAGE_DIR/oss-transfer-cli-amd64" "$PACKAGE_DIR/oss-transfer-cli-arm64"

# A credential-free bundle is served by masters to invited agents.
BUNDLE_STAGE="$(mktemp -d "$STAGE_DIR/agent-bundle.XXXXXX")"
cp "$PACKAGE_DIR/oss-transfer.jar" "$PACKAGE_DIR/oss-transfer-cli-amd64" "$PACKAGE_DIR/oss-transfer-cli-arm64" "$PACKAGE_DIR/VERSION" "$BUNDLE_STAGE/"
mkdir -p "$BUNDLE_STAGE/deploy"
for file in install.sh install-master.sh install-agent.sh preflight-node.sh wait-node-ready.sh runtime.sh; do
    cp "deploy/$file" "$BUNDLE_STAGE/deploy/"
done
(cd "$BUNDLE_STAGE" && tar -czf "$PACKAGE_DIR/agent-bundle.tar.gz" .)
rm -rf "$BUNDLE_STAGE"
if command -v sha256sum >/dev/null 2>&1; then
    (cd "$PACKAGE_DIR" && sha256sum agent-bundle.tar.gz > agent-bundle.tar.gz.sha256)
else
    (cd "$PACKAGE_DIR" && shasum -a 256 agent-bundle.tar.gz > agent-bundle.tar.gz.sha256)
fi
chmod +x "$PACKAGE_DIR/deploy/"*.sh

echo "[5/6] Packaging release..."
(cd "$STAGE_DIR" && tar -czf "$PACKAGE_NAME.tar.gz" "$PACKAGE_NAME")
if command -v sha256sum >/dev/null 2>&1; then
    (cd "$STAGE_DIR" && sha256sum "$PACKAGE_NAME.tar.gz" > "$PACKAGE_NAME.tar.gz.sha256")
else
    (cd "$STAGE_DIR" && shasum -a 256 "$PACKAGE_NAME.tar.gz" > "$PACKAGE_NAME.tar.gz.sha256")
fi
tar -tzf "$STAGE_DIR/$PACKAGE_NAME.tar.gz" >/dev/null

echo "[6/6] Publishing local artifacts..."
if ! mkdir -- "$PUBLISH_LOCK"; then
    echo "无法获取发布锁: ${PUBLISH_LOCK}；请确认没有其他发布，或先处理上次未完成的回滚。" >&2
    exit 1
fi
PUBLISH_LOCK_HELD=1
mkdir -p "$OUTPUT_DIR/package-backups"
BACKUP_DIR="$(mktemp -d "$OUTPUT_DIR/package-backups/$(date +%Y%m%dT%H%M%S)-$COMMIT.XXXXXX")"
PUBLISHING=1
for name in "${PACKAGE_FILES[@]}"; do
    if [ -e "$OUTPUT_DIR/$name" ] || [ -L "$OUTPUT_DIR/$name" ]; then
        mv -- "$OUTPUT_DIR/$name" "$BACKUP_DIR/$name"
    else
        touch "$BACKUP_DIR/.absent-$name"
    fi
    mv -- "$STAGE_DIR/$name" "$OUTPUT_DIR/$name"
done
PUBLISHED=1
echo "发布包: $OUTPUT_DIR/$PACKAGE_NAME.tar.gz"
echo "校验值: $OUTPUT_DIR/$PACKAGE_NAME.tar.gz.sha256"
echo "上次版本: $BACKUP_DIR"
