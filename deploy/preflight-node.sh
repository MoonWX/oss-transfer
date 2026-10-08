#!/bin/bash
# Run over SSH before any deployment files or services are changed.
set -euo pipefail

java_override="${1:-}"
safe_java_path() { [[ "$1" =~ ^/[a-zA-Z0-9_./+-]+$ ]]; }
fail() { echo "节点预检失败: $*" >&2; exit 1; }

architecture="$(uname -m)"
case "$architecture" in
    x86_64|aarch64|arm64) ;;
    *) fail "不支持的架构 ${architecture}；仅支持 x86_64 / aarch64 / arm64" ;;
esac
for dependency in systemctl curl timeout ss; do
    command -v "$dependency" >/dev/null 2>&1 || fail "缺少 ${dependency}；请先安装后重试"
done

candidates=()
if [ -n "$java_override" ]; then
    safe_java_path "$java_override" || fail "OSS_TRANSFER_JAVA_BIN 必须为绝对路径，且只能包含字母、数字、/、_、.、+、-"
    candidates+=("$java_override")
else
    candidates+=(/usr/bin/java)
    if [ -n "${JAVA_HOME:-}" ]; then candidates+=("$JAVA_HOME/bin/java"); fi
    path_java="$(command -v java || true)"
    if [ -n "$path_java" ]; then candidates+=("$path_java"); fi
fi

for java_bin in "${candidates[@]}"; do
    if ! safe_java_path "$java_bin"; then
        echo "  跳过不支持的 Java 路径；请用 OSS_TRANSFER_JAVA_BIN 指定安全绝对路径" >&2
        continue
    fi
    if [ ! -f "$java_bin" ] || [ ! -x "$java_bin" ]; then
        echo "  Java 不存在或不可执行: $java_bin" >&2
        continue
    fi
    if ! version_output="$(timeout 10 "$java_bin" -version 2>&1)"; then
        echo "  Java 执行失败或超时: ${java_bin}；请检查权限、二进制架构和运行时依赖" >&2
        continue
    fi
    java_version="$(printf '%s\n' "$version_output" | sed -n 's/.*version "\([^"]*\)".*/\1/p' | head -n 1)"
    if [[ "$java_version" =~ ^1\.([0-9]+)([._+-]|$) ]]; then
        major="${BASH_REMATCH[1]}"
    elif [[ "$java_version" =~ ^([0-9]+)([._+-]|$) ]]; then
        major="${BASH_REMATCH[1]}"
    else
        echo "  无法识别 Java 版本: ${java_bin}；需要 Java 8 或更高版本" >&2
        continue
    fi
    if [ "${#major}" -gt 3 ] || (( 10#$major < 8 )); then
        echo "  Java 版本不支持: ${java_bin}；需要 Java 8 或更高版本" >&2
        continue
    fi
    # Only these validated fields go to stdout; callers must not parse diagnostics.
    printf '%s\t%s\t%s\n' "$architecture" "$java_bin" "$((10#$major))"
    exit 0
done
if [ -n "$java_override" ]; then
    fail "指定的 OSS_TRANSFER_JAVA_BIN 不可用，未回退到其他 Java；请修复该路径后重试"
fi
fail "未找到可运行的 Java 8+；请安装匹配节点架构的 Java，或设置 OSS_TRANSFER_JAVA_BIN=/绝对路径/bin/java 后重试"
