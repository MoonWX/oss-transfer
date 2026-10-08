#!/bin/bash
# Show only service execution metadata, never application configuration or logs.
set -euo pipefail

node_ip="$1"
service_port="$2"
wait_seconds="$3"
service_name="${4:-oss-transfer}"
[[ "$service_name" =~ ^[A-Za-z0-9_-]+$ ]] || { echo "Invalid service name" >&2; exit 1; }
deadline=$((SECONDS + wait_seconds))
state=""
report_failure() {
    echo "节点 $node_ip 启动失败: $*" >&2
    printf '%s\n' "$state" >&2
    if [ "${exec_status:-}" = 203 ]; then
        echo "203/EXEC: systemd 无法执行 Java；请检查 ExecStart 路径、执行权限、架构及系统安全策略。" >&2
    fi
    echo "请在该节点检查 systemctl status $service_name --no-pager 和 journalctl -u $service_name -n 50 --no-pager。" >&2
    exit 1
}

while :; do
    remaining=$((deadline - SECONDS))
    if [ "$remaining" -le 0 ]; then report_failure "等待 HTTP 就绪超过 ${wait_seconds} 秒"; fi
    command_timeout=5
    if [ "$remaining" -lt "$command_timeout" ]; then command_timeout="$remaining"; fi
    if ! state="$(timeout "$command_timeout" systemctl show "$service_name" --no-pager \
        -p ActiveState -p SubState -p Result -p MainPID -p ExecMainCode -p ExecMainStatus -p NRestarts)"; then
        state=""
        report_failure "无法读取 systemd 服务状态或读取超时"
    fi
    active="" substate="" exec_status="" restarts=0 main_pid=""
    while IFS='=' read -r key value; do
        case "$key" in
            ActiveState) active="$value" ;;
            SubState) substate="$value" ;;
            ExecMainStatus) exec_status="$value" ;;
            NRestarts) restarts="$value" ;;
            MainPID) main_pid="$value" ;;
        esac
    done <<< "$state"
    if [ "$active" = failed ] || [ "$active" = inactive ] || [ "$substate" = auto-restart ] || \
       [ "$exec_status" = 203 ] || [[ "$restarts" =~ ^[1-9][0-9]*$ ]]; then
        report_failure "服务进程已退出或正在重启"
    fi
    remaining=$((deadline - SECONDS))
    if [ "$remaining" -le 0 ]; then report_failure "等待 HTTP 就绪超过 ${wait_seconds} 秒"; fi
    request_timeout=5
    if [ "$remaining" -lt "$request_timeout" ]; then request_timeout="$remaining"; fi
    # Type=simple is active before Java binds its socket. A different service
    # already answering on this port must not make this deployment look ready.
    if ! listeners="$(timeout "$request_timeout" ss -H -ltnp "sport = :$service_port" 2>/dev/null)"; then
        report_failure "无法检查监听端口的进程归属；请检查 ss 和执行权限"
    fi
    owns_listener=false
    if [[ "$main_pid" =~ ^[1-9][0-9]*$ ]] && [ -n "$listeners" ]; then
        owns_listener=true
        while IFS= read -r listener; do
            if [[ "$listener" != *"pid=$main_pid,"* ]]; then
                report_failure "端口 $service_port 已被其他进程占用或无法确认归属当前 Java 进程"
            fi
        done <<< "$listeners"
    fi
    if [ "$active" = active ] && [ "$owns_listener" = true ]; then
        remaining=$((deadline - SECONDS))
        if [ "$remaining" -le 0 ]; then report_failure "等待 HTTP 就绪超过 ${wait_seconds} 秒"; fi
        if [ "$remaining" -lt "$request_timeout" ]; then request_timeout="$remaining"; fi
        if http_status="$(curl --fail --silent --show-error --noproxy '*' \
            --connect-timeout "$request_timeout" --max-time "$request_timeout" \
            "http://$node_ip:$service_port/api/auth/current" --output /dev/null --write-out '%{http_code}' 2>/dev/null)" && \
            [ "$http_status" = 200 ]; then
            echo "  $node_ip:$service_port 已就绪"
            exit 0
        fi
    fi
    sleep 1
done
