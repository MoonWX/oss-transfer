#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/oss-entrypoint-test.XXXXXX")"
trap 'rm -rf -- "$fixture"' EXIT
mkdir -p "$fixture/bin" "$fixture/data"
# Keep the test fully isolated from real container volumes and configurations.
sed "s#/app/data#$fixture/data#g" "$here/docker/entrypoint.sh" > "$fixture/entrypoint.sh"
cat > "$fixture/bin/java" <<'JAVA'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" > "$CHECK_DIR/java-arguments"
printf '%s\n' "$SERVER_ADDRESS" > "$CHECK_DIR/server-address"
[[ ${#OSS_TRANSFER_INTERNAL_TOKEN} -ge 32 && ${#OSS_TRANSFER_BOOTSTRAP_PASSWORD} -ge 12 ]]
JAVA
chmod +x "$fixture/bin/java"
export PATH="$fixture/bin:$PATH" CHECK_DIR="$fixture"
if TRANSFER_ROLE=bad bash "$fixture/entrypoint.sh" > "$fixture/log" 2>&1; then echo 'Accepted invalid role' >&2; exit 1; fi
TRANSFER_ROLE=master OSS_TRANSFER_PORT=18085 bash "$fixture/entrypoint.sh" > "$fixture/log"
[[ "$(cat "$fixture/server-address")" == 0.0.0.0 ]]
grep -q -- '--transfer.role=master' "$fixture/java-arguments"
grep -q -- '--server.port=18085' "$fixture/java-arguments"
[[ "$(stat -f '%Lp' "$fixture/data/secrets/admin_password" 2>/dev/null || stat -c '%a' "$fixture/data/secrets/admin_password")" == 600 ]]
before="$(openssl dgst -sha256 "$fixture/data/secrets/internal_token")"
password_before="$(openssl dgst -sha256 "$fixture/data/secrets/admin_password")"
TRANSFER_ROLE=master bash "$fixture/entrypoint.sh" > "$fixture/log"
[[ "$before" == "$(openssl dgst -sha256 "$fixture/data/secrets/internal_token")" ]]
[[ "$password_before" == "$(openssl dgst -sha256 "$fixture/data/secrets/admin_password")" ]]
if TRANSFER_ROLE=agent TRANSFER_AGENT_MASTER_URL='http://host/path' bash "$fixture/entrypoint.sh" > "$fixture/log" 2>&1; then echo 'Accepted invalid master URL' >&2; exit 1; fi
TRANSFER_ROLE=agent TRANSFER_AGENT_MASTER_URL='https://master.example.com' bash "$fixture/entrypoint.sh" > "$fixture/log"
[[ "$(cat "$fixture/server-address")" == 127.0.0.1 ]]
grep -q -- '--transfer.role=agent' "$fixture/java-arguments"
if OSS_TRANSFER_PORT=65536 bash "$fixture/entrypoint.sh" > "$fixture/log" 2>&1; then echo 'Accepted invalid port' >&2; exit 1; fi
printf 'Docker entrypoint checks passed: role, port, local-only agent, credential generation and restart persistence.\n'
