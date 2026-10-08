#!/usr/bin/env bash
set -euo pipefail
umask 077
fail() { printf 'Startup failed: %s\n' "$*" >&2; exit 1; }
role="${TRANSFER_ROLE:-master}"
[[ "$role" == master || "$role" == agent ]] || fail 'TRANSFER_ROLE must be master or agent'
port="${SERVER_PORT:-${TRANSFER_PORT:-${OSS_TRANSFER_PORT:-1685}}}"
[[ "$port" =~ ^[0-9]{1,5}$ ]] && ((10#$port >= 1 && 10#$port <= 65535)) || fail 'port must be between 1 and 65535'
export OSS_TRANSFER_PORT="$((10#$port))"
[[ -w /app/data ]] || fail '/app/data is not writable; set its owner to container UID 10001'
mkdir -p /app/data/config /app/data/secrets /app/data/tmp-objects /app/data/logs
chmod 0700 /app/data/config /app/data/secrets
[[ -f /app/data/config/application.yml ]] || printf '# Persistent application overrides.\n' > /app/data/config/application.yml
create_secret() {
    local target="$1" input="${2:-}"
    if [[ ! -e "$target" ]]; then
        local temporary
        temporary="$(mktemp "${target}.XXXXXX")"
        if [[ -n "$input" ]]; then
            [[ -f "$input" && -r "$input" ]] || fail 'supplied secret file is not readable'
            cat "$input" > "$temporary"
        else
            openssl rand -hex 32 > "$temporary"
        fi
        chmod 0600 "$temporary"
        mv -n "$temporary" "$target"
        rm -f "$temporary"
    fi
    [[ -f "$target" && -r "$target" ]] || fail 'persisted secret is not readable'
    chmod 0600 "$target"
}
create_secret /app/data/secrets/internal_token "${OSS_TRANSFER_INTERNAL_TOKEN_FILE:-}"
create_secret /app/data/secrets/admin_password "${OSS_TRANSFER_BOOTSTRAP_PASSWORD_FILE:-}"
OSS_TRANSFER_INTERNAL_TOKEN="$(cat /app/data/secrets/internal_token)"
OSS_TRANSFER_BOOTSTRAP_PASSWORD="$(cat /app/data/secrets/admin_password)"
[[ ${#OSS_TRANSFER_INTERNAL_TOKEN} -ge 32 && "$OSS_TRANSFER_INTERNAL_TOKEN" != *$'\n'* && "$OSS_TRANSFER_INTERNAL_TOKEN" != *$'\r'* ]] || fail 'invalid internal token file'
[[ ${#OSS_TRANSFER_BOOTSTRAP_PASSWORD} -ge 12 && "$OSS_TRANSFER_BOOTSTRAP_PASSWORD" != *$'\n'* && "$OSS_TRANSFER_BOOTSTRAP_PASSWORD" != *$'\r'* ]] || fail 'invalid bootstrap password file'
export OSS_TRANSFER_INTERNAL_TOKEN OSS_TRANSFER_BOOTSTRAP_PASSWORD
# Each agent uses its own local token. The master shared token never goes to agents.
if [[ "$role" == agent ]]; then
    [[ "${TRANSFER_AGENT_MASTER_URL:-}" =~ ^https?://[A-Za-z0-9.-]+(:[0-9]{1,5})?/?$ ]] || fail 'TRANSFER_AGENT_MASTER_URL must be an http(s) origin'
    export SERVER_ADDRESS=127.0.0.1
else
    export SERVER_ADDRESS="${SERVER_ADDRESS:-0.0.0.0}"
    printf 'Bootstrap admin password: /app/data/secrets/admin_password\n'
fi
if [[ $# -gt 0 ]]; then exec "$@"; fi
read -r -a java_options <<< "${JAVA_OPTS:--Xms128m -Xmx1024m}"
exec java "${java_options[@]}" -jar /opt/oss-transfer/oss-transfer.jar \
    --spring.config.additional-location=file:/opt/oss-transfer/docker-defaults.yml,file:/app/data/config/application.yml \
    "--server.port=$OSS_TRANSFER_PORT" "--transfer.port=$OSS_TRANSFER_PORT" "--server.address=$SERVER_ADDRESS" \
    "--transfer.role=$role"
