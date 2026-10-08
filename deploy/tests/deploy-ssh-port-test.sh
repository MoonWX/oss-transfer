#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/oss-ssh-test.XXXXXX")"
trap 'rm -rf -- "$fixture"' EXIT
mkdir -p "$fixture/package/deploy" "$fixture/bin"
cp "$here/deploy.sh" "$here/install.sh" "$here/preflight-node.sh" "$fixture/package/deploy/"
for file in oss-transfer.jar VERSION oss-transfer-cli-amd64 oss-transfer-cli-arm64 agent-bundle.tar.gz; do printf 'fixture\n' > "$fixture/package/$file"; done
cat > "$fixture/bin/ssh" <<'SSH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$OPERATIONS"
if [[ "$*" == *'mktemp -d /tmp/oss-transfer-deploy.XXXXXX'* ]]; then printf '/tmp/oss-transfer-deploy.fixture\n'; fi
SSH
cat > "$fixture/bin/scp" <<'SCP'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$OPERATIONS"
SCP
chmod +x "$fixture/bin/"*
export PATH="$fixture/bin:$PATH" OPERATIONS="$fixture/operations"
bash "$fixture/package/deploy/deploy.sh" 192.0.2.10 > "$fixture/result"
grep -q -- '-p 22 root@192.0.2.10' "$OPERATIONS"
grep -q -- '-P 22 ' "$OPERATIONS"
grep -q -- '--role master' "$OPERATIONS"
: > "$OPERATIONS"
bash "$fixture/package/deploy/deploy.sh" --role agent --port 1785 --master-url http://192.0.2.10:1685 \
    --join-token 0123456789abcdef0123456789abcdef --label 'agent two' 192.0.2.11:2222 > "$fixture/result"
grep -q -- '-p 2222 root@192.0.2.11' "$OPERATIONS"
grep -q -- '-P 2222 ' "$OPERATIONS"
grep -q -- '--role agent' "$OPERATIONS"
grep -q -- '--port 1785' "$OPERATIONS"
! grep -q '0123456789abcdef' "$fixture/result"
for target in 192.0.2.10:0 192.0.2.10:65536 192.0.2.10:22:22 256.0.2.10 192.00.2.10; do
    : > "$OPERATIONS"
    if bash "$fixture/package/deploy/deploy.sh" "$target" > "$fixture/result" 2>&1; then echo 'Accepted invalid target' >&2; exit 1; fi
    [[ ! -s "$OPERATIONS" ]]
done
: > "$OPERATIONS"
if bash "$fixture/package/deploy/deploy.sh" 192.0.2.10 192.0.2.11 > "$fixture/result" 2>&1; then echo 'Accepted reused enrollment across nodes' >&2; exit 1; fi
[[ ! -s "$OPERATIONS" ]]
printf 'SSH wrapper checks passed: default/custom port, both roles, invalid targets and single-use enrollment.\n'
