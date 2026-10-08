#!/usr/bin/env bash
# Optional SSH convenience wrapper. Recommended deployment is local installation.
set -euo pipefail
umask 077
fail() { printf 'Deploy failed: %s\n' "$*" >&2; exit 1; }
usage() {
    cat <<'HELP'
Usage: deploy.sh [--role master|agent] [options] IP[:SSH_PORT]
  --role master|agent   Default: master
  --web-url ORIGIN      Public master URL
  --master-url ORIGIN   Master URL for an agent
  --join-token TOKEN    Single-use agent invitation
  --address IPV4        Agent identity; defaults to the target IP
  --label NAME          Agent display name
  --port PORT           Service port; SSH defaults to 22
This helper copies the release and runs the same local installer over SSH as root.
Add each new agent with a separate invitation from the master's Execution nodes page.
HELP
}
role=master web='' master='' token='' address='' label='' port=1685 target=''
while [[ $# -gt 0 ]]; do
    case "$1" in
        --help|-h) usage; exit 0;;
        --role|--web-url|--master-url|--join-token|--address|--label|--port)
            [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || fail "$1 requires a value"
            case "$1" in
                --role) role="$2";; --web-url) web="$2";; --master-url) master="$2";;
                --join-token) token="$2";; --address) address="$2";; --label) label="$2";; --port) port="$2";;
            esac
            shift 2;;
        -*) fail "unknown option: $1";;
        *) [[ -z "$target" ]] || fail 'one target is allowed; each agent needs its own invitation'; target="$1"; shift;;
    esac
done
[[ "$role" == master || "$role" == agent ]] || fail 'role must be master or agent'
ip="${target%%:*}"; ssh_port=22
[[ "$target" != *:* ]] || ssh_port="${target#*:}"
[[ "$ip" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || fail 'target must be IPv4[:SSH_PORT]'
IFS=. read -r -a octets <<< "$ip"
for octet in "${octets[@]}"; do [[ "$octet" == 0 || "$octet" != 0* ]] && ((10#$octet <= 255)) || fail 'invalid IPv4 target'; done
for value in "$port" "$ssh_port"; do [[ "$value" =~ ^[0-9]{1,5}$ ]] && ((10#$value >= 1 && 10#$value <= 65535)) || fail 'ports must be between 1 and 65535'; done
valid_origin() { [[ "$1" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?/?$ ]]; }
if [[ "$role" == master ]]; then web="${web:-http://$ip:$port}"; valid_origin "$web" || fail 'invalid WebUI URL';
else valid_origin "$master" || fail '--master-url is required'; [[ "$token" =~ ^[A-Za-z0-9_-]{32,}$ && ${#token} -le 256 ]] || fail '--join-token is required'; fi
here="$(cd "$(dirname "$0")" && pwd)"
package_dir="$here"
[[ -f "$package_dir/oss-transfer.jar" ]] || package_dir="$(cd "$here/.." && pwd)"
for required in oss-transfer.jar VERSION deploy/install.sh deploy/preflight-node.sh; do [[ -f "$package_dir/$required" ]] || fail 'run this helper from an extracted release'; done
stage="$(mktemp -d "${TMPDIR:-/tmp}/oss-transfer-ssh.XXXXXX")"
remote_stage=''
cleanup() {
    rm -rf -- "$stage"
    if [[ -n "$remote_stage" ]]; then ssh -p "$ssh_port" "root@$ip" "rm -rf -- '$remote_stage'" >/dev/null 2>&1 || true; fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# Source archives contain no credentials or live state.
(cd "$package_dir" && tar -czf "$stage/package.tar.gz" oss-transfer.jar VERSION oss-transfer-cli-amd64 oss-transfer-cli-arm64 agent-bundle.tar.gz deploy)
remote_stage="$(ssh -p "$ssh_port" "root@$ip" 'umask 077; mktemp -d /tmp/oss-transfer-deploy.XXXXXX')"
[[ "$remote_stage" =~ ^/tmp/oss-transfer-deploy\.[A-Za-z0-9]+$ ]] || fail 'invalid remote temporary directory'
scp -P "$ssh_port" "$stage/package.tar.gz" "root@$ip:$remote_stage/package.tar.gz"
args=(bash "$remote_stage/deploy/install.sh" --role "$role" --package-dir "$remote_stage" --port "$port")
if [[ "$role" == master ]]; then args+=(--web-url "$web");
else args+=(--master-url "$master" --join-token "$token" --address "${address:-$ip}"); [[ -z "$label" ]] || args+=(--label "$label"); fi
printf -v install_command '%q ' "${args[@]}"
ssh -p "$ssh_port" "root@$ip" "tar -xzf '$remote_stage/package.tar.gz' -C '$remote_stage' && $install_command"
printf 'Deployment completed: %s at %s (SSH %s)\n' "$role" "$ip" "$ssh_port"
