#!/usr/bin/env bash
set -euo pipefail
export OSS_TRANSFER_CONFIG_FILE=/app/data/config/application.yml
export OSS_TRANSFER_MASTER_SERVER="${TRANSFER_AGENT_MASTER_URL:-http://127.0.0.1:${OSS_TRANSFER_PORT:-1685}}"
# Preserve the local secret file rather than writing a token to shell startup files.
if [[ "${TRANSFER_ROLE:-master}" == master && -r /app/data/secrets/internal_token ]]; then
    export OSS_TRANSFER_INTERNAL_TOKEN="$(cat /app/data/secrets/internal_token)"
fi
exec /opt/oss-transfer/oss-transfer-cli "$@"
