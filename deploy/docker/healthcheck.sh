#!/usr/bin/env bash
set -euo pipefail
port="${SERVER_PORT:-${TRANSFER_PORT:-${OSS_TRANSFER_PORT:-1685}}}"
exec curl --fail --silent --show-error --max-time 5 \
    "http://127.0.0.1:${port}/api/auth/current" --output /dev/null
