#!/usr/bin/env bash
# Run OSS Transfer without systemd or Docker.
#
# Put this script next to the programs of a release archive
# (oss-transfer-master, oss-transfer-agent, oss-transfer-cli) and run:
#
#   ./oss-transfer-run.sh master start [--listen 0.0.0.0:1685]
#   ./oss-transfer-run.sh agent start --join-url 'http://MASTER:1685/api/agents/bootstrap/TOKEN'   (first start)
#   ./oss-transfer-run.sh agent start                                                            (later starts)
#   ./oss-transfer-run.sh master|agent stop|restart|status|logs
#
# A small supervisor restarts the program if it exits, and stops for good when
# an Agent was removed on the Master (exit status 78). Data and logs live in
# <data>/ (default: <this directory>/data/<role>); change it with --data DIR.
# For start at boot, add to root's crontab: @reboot /path/oss-transfer-run.sh master start
set -euo pipefail

usage() { sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
[[ $# -ge 2 ]] || usage
role=$1 action=$2; shift 2
[[ "$role" == master || "$role" == agent ]] || usage
dir=$(cd "$(dirname "$0")" && pwd)
data="$dir/data/$role" listen="0.0.0.0:1685" join_url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --data) data=$2; shift 2;;
    --listen) listen=$2; shift 2;;
    --join-url) join_url=$2; shift 2;;
    *) echo "未知选项 / unknown option: $1" >&2; usage;;
  esac
done
binary="$dir/oss-transfer-$role"
[[ -x "$binary" ]] || { echo "找不到 $binary / program not found" >&2; exit 1; }
mkdir -p "$data/logs" "$data/run"
log="$data/logs/service.log" pidfile="$data/run/supervisor.pid" childfile="$data/run/$role.pid" stopfile="$data/run/stop"

running() { [[ -f "$pidfile" ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null; }
# The program itself, not only its supervisor, is up.
program_up() { [[ -f "$childfile" ]] && kill -0 "$(cat "$childfile")" 2>/dev/null; }

args=(--data-dir "$data")
[[ "$role" == master ]] && args+=(--listen "$listen")

start() {
  if running; then echo "$role 已在运行（如需重启请用 restart）"; return 0; fi
  if [[ "$role" == agent && ! -f "$data/agent.db" ]]; then
    [[ -n "$join_url" ]] || { echo "首次启动需要 --join-url（在控制台“添加节点”或 oss-transfer-cli agent invite 获取）" >&2; exit 1; }
    # The invitation link must not carry the installer's query (?lang=zh …).
    join_url=${join_url%%\?*}
    echo "正在加入 Master…"
    "$binary" --join-url "$join_url" --data-dir "$data" --enroll-only >> "$log" 2>&1 || {
      echo "加入失败，详见 $log" >&2; tail -n 5 "$log" >&2; exit 1; }
  fi
  rm -f "$stopfile"
  # The supervisor loop runs detached; it restarts the program 5 s after an
  # unexpected exit and stops when asked to, or when the Agent was revoked (78).
  nohup bash -c '
    binary=$1 log=$2 stopfile=$3 childfile=$4; shift 4
    trap "kill -TERM \$child 2>/dev/null; wait \$child; exit 0" TERM INT
    while [[ ! -f "$stopfile" ]]; do
      "$binary" "$@" >> "$log" 2>&1 & child=$!
      echo $child > "$childfile"
      wait $child && code=0 || code=$?
      rm -f "$childfile"
      [[ -f "$stopfile" || $code -eq 78 ]] && break
      echo "$(date -u +%FT%TZ) supervisor: exited with $code, restarting in 5 s" >> "$log"
      sleep 5
    done' oss-transfer-supervisor "$binary" "$log" "$stopfile" "$childfile" "${args[@]}" > /dev/null 2>&1 &
  echo $! > "$pidfile"
  # Started means the program stays up for a few seconds.
  sleep 4
  if program_up; then
    echo "$role 已启动 (pid $(cat "$childfile"))，日志：$log"
  else
    echo "$role 未能保持运行，已停止。最近的日志：" >&2; tail -n 20 "$log" >&2
    stop >/dev/null; exit 1
  fi
  if [[ "$role" == master ]]; then
    password=$(grep -m1 -i "initial password" "$log" || true)
    [[ -n "$password" ]] && echo "$password（首次登录后请修改）"
    echo "控制台：http://<本机IP>:${listen##*:}"
  fi
}

stop() {
  running || { echo "$role 未运行"; rm -f "$pidfile"; return 0; }
  touch "$stopfile"
  pid=$(cat "$pidfile")
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 45); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill -0 "$pid" 2>/dev/null && { echo "45 秒内未停止，强制结束"; pkill -KILL -P "$pid" 2>/dev/null || true; kill -KILL "$pid" 2>/dev/null || true; }
  rm -f "$pidfile" "$childfile"; echo "$role 已停止"
}

case "$action" in
  start) start;;
  stop) stop;;
  restart) stop; start;;
  status)
    if program_up; then echo "$role 正在运行 (pid $(cat "$childfile"))"
    elif running; then echo "$role 正在重启（程序反复退出，见 $log）"; exit 3
    else echo "$role 未运行"; exit 3; fi;;
  logs) tail -n 50 -f "$log";;
  *) usage;;
esac
