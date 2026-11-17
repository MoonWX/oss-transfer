#!/usr/bin/env bash
# Run OSS Transfer without systemd or Docker.
#
# Put this script next to the programs of a release archive
# (oss-transfer-master, oss-transfer-agent, oss-transfer-cli) and run:
#
#   ./oss-transfer-run.sh master start [--listen 0.0.0.0:1685]   (later starts reuse the last --listen)
#   ./oss-transfer-run.sh agent start --join-url 'http://MASTER:1685/api/agents/bootstrap/TOKEN'   (to join or rejoin)
#   ./oss-transfer-run.sh agent start                                                            (later starts)
#   ./oss-transfer-run.sh master|agent stop|restart|status|logs
#
# A small supervisor restarts the program if it exits, and stops for good when
# an Agent was removed on the Master (exit status 78). Data and logs live in
# <data>/ (default: <this directory>/data/<role>); change it with --data DIR.
# For start at boot, add to root's crontab: @reboot /path/oss-transfer-run.sh master start
# shellcheck disable=SC1111 # Chinese messages quote with “ ”.
set -euo pipefail

usage() { sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
[[ $# -ge 2 ]] || usage
role=$1 action=$2; shift 2
[[ "$role" == master || "$role" == agent ]] || usage
dir=$(cd "$(dirname "$0")" && pwd)
data="$dir/data/$role" listen="" join_url=""
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
# The Master's address of the last successful start. Agents keep the address
# they joined with, so a start without --listen (restart, @reboot) reuses it.
listenfile="$data/run/listen" saved_listen=""
if [[ "$role" == master && -f "$listenfile" ]]; then saved_listen=$(head -n 1 "$listenfile"); fi
explicit_listen=$listen
listen=${listen:-${saved_listen:-0.0.0.0:1685}}

# PID files survive a reboot or a container restart, and their numbers can
# then belong to unrelated processes. A recorded PID is acted on only when
# that process's command line shows it is ours; /proc also works in minimal
# containers without ps.
process_args() {
  if [[ -d /proc/self ]]; then { tr '\0' ' ' < "/proc/$1/cmdline"; } 2>/dev/null
  else ps -ww -p "$1" -o args= 2>/dev/null; fi
}
# Prints the PID of this data directory's supervisor. Its name carries the
# token recorded beside the PID; a PID file written before tokens names the
# role's supervisor.
supervisor_pid() {
  local pid='' token='' args
  { read -r pid token < "$pidfile"; } 2>/dev/null || [[ -n "$pid" ]] || return 1
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
  args=$(process_args "$pid") || return 1
  if [[ -n "$token" ]]; then [[ "$args" == *" oss-transfer-supervisor-$token "* ]] || return 1
  else [[ "$args" == *" oss-transfer-supervisor "*"/oss-transfer-$role "* ]] || return 1; fi
  printf '%s\n' "$pid"
}
# Prints the PID of the program itself, which the supervisor runs as
# "<binary> --data-dir <data> …". ps may show non-ASCII paths escaped, so
# without /proc the program is recognised by its name.
program_pid() {
  local pid='' args
  { read -r pid < "$childfile"; } 2>/dev/null || [[ -n "$pid" ]] || return 1
  [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
  args=$(process_args "$pid") || return 1
  [[ "$args" == *"$binary --data-dir $data "* || ( ! -d /proc/self && "$args" == *"/oss-transfer-$role --data-dir "* ) ]] || return 1
  printf '%s\n' "$pid"
}
running() { supervisor_pid > /dev/null; }
program_up() { program_pid > /dev/null; }

args=(--data-dir "$data")
[[ "$role" == master ]] && args+=(--listen "$listen")

# The Master's explanation of a refused join, and what to do about it.
join_advice() {
  if grep -q 'HTTP 403' <<< "$1"; then
    echo "Master 拒绝了加入链接：链接无效、已过期（有效期 30 分钟）或已被其他节点使用。请在控制台“执行节点 → 添加节点”为这台主机生成新的加入链接，再用 --join-url 运行 start。"
    echo "The Master rejected the join link: it is invalid, has expired (links are valid for 30 minutes) or was already used by another node. Create a new join link for this host in Nodes > Add node, then run start with --join-url."
  elif grep -q 'HTTP 409' <<< "$1"; then
    echo "Master 拒绝了加入：此节点或同一地址的其他节点已注册。仍在册的节点无需重新加入，直接运行 start（不加 --join-url）即可；如需重新加入，请先在控制台移除旧节点，再生成新的加入链接。"
    echo "The Master rejected the join: this node, or another node with the same address, is already registered. A node that is still registered does not need to join again; run start without --join-url. To rejoin, remove the old node in the console first, then create a new join link."
  elif grep -qiE 'dial tcp|no such host|connection refused|network is unreachable|i/o timeout|deadline exceeded|x509|certificate' <<< "$1"; then
    echo "无法连接 Master。请确认加入链接中的地址可以从本机访问，再重新运行同一条命令。"
    echo "The Master could not be reached. Check that the address in the join link is reachable from this host, then run the same command again."
  fi
}

start() {
  if running; then
    if [[ -n "$join_url" ]]; then
      echo "$role 已在运行，本次没有使用 --join-url。如需用新链接重新加入，请运行 restart --join-url …" >&2
      echo "$role is already running; --join-url was not used. To rejoin with a new link, run restart --join-url …" >&2
      exit 1
    fi
    if [[ "$role" == master && -n "$explicit_listen" && "$explicit_listen" != "$saved_listen" ]]; then
      echo "master 已在运行，本次没有使用 --listen。如需改为监听 ${explicit_listen}，请运行 restart --listen $explicit_listen" >&2
      echo "The Master is already running; --listen was not used. To listen on $explicit_listen, run restart --listen $explicit_listen" >&2
      exit 1
    fi
    echo "$role 已在运行（如需重启请用 restart） / $role is already running (use restart to restart it)"; return 0
  fi
  # Whatever these files name is not running as ours any more.
  rm -f "$pidfile" "$childfile"
  if [[ "$role" == agent && -n "$join_url" ]]; then
    # A join link always joins: the program reuses an existing identity,
    # rejoins after the node was removed, and refuses another Master's link.
    # The invitation link must not carry the installer's query (?lang=zh …).
    join_url=${join_url%%\?*}
    echo "正在加入 Master… / joining the Master…"
    if ! output=$("$binary" --join-url "$join_url" --data-dir "$data" --enroll-only 2>&1); then
      printf '%s\n' "$output" >> "$log"
      echo "加入失败 / joining failed: $(printf '%s\n' "$output" | tail -n 1)" >&2
      join_advice "$output" >&2
      echo "日志 / log: $log" >&2
      exit 1
    fi
    printf '%s\n' "$output" >> "$log"
  elif [[ "$role" == agent && ! -f "$data/agent.db" ]]; then
    echo "首次启动需要 --join-url（在控制台“添加节点”或 oss-transfer-cli agent invite 获取） / the first start needs --join-url (from Nodes > Add node, or oss-transfer-cli agent invite)" >&2; exit 1
  fi
  if [[ "$role" == master && -n "$explicit_listen" && -n "$saved_listen" && "$explicit_listen" != "$saved_listen" ]]; then
    echo "注意：监听地址从 $saved_listen 改为 ${listen}。按原地址加入的执行节点需要能通过新地址访问 Master。"
    echo "Note: the listen address changes from $saved_listen to $listen. Agents that joined with the old address must be able to reach the Master at the new one."
  fi
  rm -f "$stopfile"
  # Only this start's output counts: the log keeps earlier runs, whose initial
  # password may belong to a database that no longer exists.
  offset=0; [[ ! -f "$log" ]] || offset=$(wc -c < "$log" | tr -d ' ')
  token=$(od -An -N8 -tx1 /dev/urandom 2>/dev/null | tr -d ' \n') || token=
  token=${token:-$$$RANDOM}
  # The supervisor loop runs detached; it restarts the program 5 s after an
  # unexpected exit and stops when asked to, or when the Agent was revoked (78).
  # OSS_TRANSFER_LAUNCHER tells the program how it is run: neither the
  # installer nor online updates replace programs started by this script.
  # shellcheck disable=SC2016 # Expanded by the supervisor's own shell.
  OSS_TRANSFER_LAUNCHER=run-script nohup bash -c '
    binary=$1 log=$2 stopfile=$3 childfile=$4; shift 4
    child=
    trap "[[ -z \$child ]] || kill -TERM \$child 2>/dev/null; wait \$child 2>/dev/null; exit 0" TERM INT
    while [[ ! -f "$stopfile" ]]; do
      "$binary" "$@" >> "$log" 2>&1 & child=$!
      echo $child > "$childfile"
      wait $child && code=0 || code=$?
      child=
      rm -f "$childfile"
      [[ -f "$stopfile" || $code -eq 78 ]] && break
      echo "$(date -u +%FT%TZ) supervisor: exited with $code, restarting in 5 s" >> "$log"
      sleep 5
    done' "oss-transfer-supervisor-$token" "$binary" "$log" "$stopfile" "$childfile" "${args[@]}" > /dev/null 2>&1 &
  supervisor=$!
  echo "$supervisor $token" > "$pidfile"
  # Started means the program came up. The Master says so in its log: it
  # prints "Master listening on" last, after any initial password, which a
  # first start on a slow machine can take well over a few seconds to reach.
  # An Agent has to stay up for a few seconds.
  state=starting
  if [[ "$role" == master ]]; then
    for _ in $(seq 1 60); do
      segment=$(tail -c +"$((offset + 1))" "$log" 2>/dev/null) || segment=
      if [[ "$segment" == *"Master listening on "* ]]; then state=ready; break; fi
      # This shell started the supervisor, so its PID is still its own.
      if [[ "$segment" == *"supervisor: exited with"* ]] || ! kill -0 "$supervisor" 2>/dev/null; then state=failed; break; fi
      sleep 1
    done
    # Earlier releases print the password just after that line.
    if [[ "$state" == ready && "$segment" != *"Initial password: "* ]]; then
      sleep 1; segment=$(tail -c +"$((offset + 1))" "$log" 2>/dev/null) || segment=
    fi
  else
    sleep 4
    if program_up; then state=ready; else state=failed; fi
  fi
  if [[ "$state" == failed ]] || ! program_up; then
    echo "$role 未能保持运行，已停止。最近的日志： / $role did not stay up and was stopped. The latest log:" >&2; tail -n 20 "$log" >&2
    stop >/dev/null; exit 1
  fi
  echo "$role 已启动 / $role started (pid $(program_pid))"
  echo "日志 / log: $log"
  [[ "$role" == master ]] || return 0
  printf '%s\n' "$listen" > "$listenfile"
  if [[ "$state" != ready ]]; then
    echo "master 尚未开始监听，可能仍在初始化（例如升级数据库）。请稍后用 status 和 logs 查看；本次如生成初始密码，也会写在 $log 中。"
    echo "The Master is not listening yet and may still be initializing (for example upgrading its database). Check it later with status and logs; an initial password, if this start creates one, is written to $log as well."
  else
    # Print the password alone on its line so that copying it, by selection
    # or double-click, cannot pick up neighbouring text.
    password=$(printf '%s\n' "$segment" | sed -n 's/^Initial password: //p' | tail -n 1)
    if [[ -n "$password" ]]; then
      echo "初始账号 / username: admin"
      echo "初始密码 / password:"
      echo "$password"
      echo "首次登录后需要修改密码。 / Change the password at the first sign-in."
    else
      echo "本次启动没有生成新的初始密码。如果尚未修改首次启动时显示的初始密码（$log 中 “Initial password” 一行），它仍然有效。忘记密码时，先运行 $0 master stop，再运行 $binary --data-dir $data --reset-admin-password"
      echo "This start created no new initial password. The initial password shown at the first start (the \"Initial password\" line in $log) stays valid until it is changed. If you forgot the password, run $0 master stop, then $binary --data-dir $data --reset-admin-password"
    fi
  fi
  host=${listen%:*} port=${listen##*:}
  host=${host#[}; host=${host%]}
  case "$host" in
    ''|0.0.0.0|::) host='<本机IP / host IP>';;
    *:*) host="[$host]";;
  esac
  echo "监听地址 / listen address: $listen"
  echo "控制台 / console: http://$host:$port"
}

stop() {
  local pid
  if ! pid=$(supervisor_pid); then
    rm -f "$pidfile" "$childfile"; echo "$role 未运行 / $role is not running"; return 0
  fi
  touch "$stopfile"
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 45); do running || break; sleep 1; done
  if running; then
    echo "45 秒内未停止，强制结束 / not stopped within 45 s; killing it"
    pkill -KILL -P "$pid" 2>/dev/null || true; kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$pidfile" "$childfile"; echo "$role 已停止 / $role stopped"
}

case "$action" in
  start) start;;
  stop) stop;;
  restart) stop; start;;
  status)
    if pid=$(program_pid); then echo "$role 正在运行 (pid $pid) / $role is running (pid $pid)"
    elif running; then echo "$role 正在重启（程序反复退出，见 ${log}） / $role is restarting (the program keeps exiting; see $log)"; exit 3
    else echo "$role 未运行 / $role is not running"; exit 3; fi;;
  logs) tail -n 50 -f "$log";;
  *) usage;;
esac
