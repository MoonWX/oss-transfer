#!/bin/bash
# Repository-scoped release runner. Run on a dedicated Ubuntu/Debian Linux x64 host.
# Official sources: github.com/actions/runner, docs.docker.com/engine/install,
# docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html.
set +x
set -euo pipefail

runner_repo=https://github.com/MoonWX/oss-transfer
runner_user=oss-release
runner_home=/var/lib/oss-release
runner_dir=/opt/oss-transfer-release-runner
runner_cache=/var/cache/oss-transfer-release-runner
runner_label="${RUNNER_LABEL:-jdcloud-publish}"
runner_name="${RUNNER_NAME:-oss-transfer-publish-$(hostname -s)}"
runner_path=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
registration_token="${RUNNER_TOKEN:-}"
unset RUNNER_TOKEN ACTIONS_RUNNER_INPUT_TOKEN GH_TOKEN GITHUB_TOKEN RUNNER_ALLOW_RUNASROOT GITHUB_ACTIONS_RUNNER_SERVICE_TEMPLATE
work_dir=
fail() { printf 'Release runner setup: %s\n' "$*" >&2; exit 1; }
download() { curl -fsSL --proto '=https' --proto-redir '=https' --tlsv1.2 --connect-timeout 10 --max-time 900 --retry 2 --retry-max-time 900 "$1" -o "$2"; }
apt_install() { DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=l apt-get -o Acquire::AllowInsecureRepositories=false -o APT::Get::AllowUnauthenticated=false install -y --no-upgrade "$@"; }
cleanup() { unset registration_token; if [[ -n "$work_dir" ]]; then rm -rf -- "$work_dir"; fi; }
runner_command() { runuser -u "$runner_user" -- env -i HOME="$runner_home" PATH="$runner_path" "$@"; }

validate_existing() {
    if [[ -f "$runner_dir/.runner" ]]; then
        local configured_repo
        configured_repo="$(python3 - "$runner_dir/.runner" <<'PY'
import json, sys
print(json.load(open(sys.argv[1])).get('gitHubUrl', '').rstrip('/'))
PY
)"
        [[ "$configured_repo" == "$runner_repo" ]] || fail 'This directory belongs to another repository; refusing to replace it.'
        [[ -f "$runner_dir/.oss-transfer-runner-label" ]] || fail 'An existing runner belongs to this repository. Verify its labels and service manually; it was not installed by this script.'
        [[ "$(cat "$runner_dir/.oss-transfer-runner-label")" == "$runner_label" ]] || fail 'Existing runner label differs; use its current label or reconfigure it explicitly. Nothing was replaced.'
        printf 'Reusing the existing repository runner.\n'
        return 0
    fi
    return 1
}

prepare_account() {
    if id "$runner_user" >/dev/null 2>&1; then
        [[ "$(id -u "$runner_user")" != 0 ]] || fail 'The runner service account must not be root.'
        [[ "$(getent passwd "$runner_user" | cut -d: -f6)" == "$runner_home" ]] || fail 'Existing service account has a different home directory.'
    else
        useradd --system --create-home --home-dir "$runner_home" --shell /bin/bash "$runner_user"
    fi
    usermod -aG docker "$runner_user"
}

prepare_docker() {
    if ! command -v docker >/dev/null 2>&1; then
        # Do not uninstall conflicting packages or touch an existing daemon/configuration.
        if dpkg-query -W -f='${Status}\n' docker.io podman-docker containerd runc 2>/dev/null | grep -q 'install ok installed'; then
            fail 'Conflicting Docker packages exist. Install Docker deliberately before rerunning; nothing was removed.'
        fi
        install -d -m 0755 /etc/apt/keyrings
        download "https://download.docker.com/linux/$os_id/gpg" "$work_dir/docker.asc"
        install -m 0644 "$work_dir/docker.asc" /etc/apt/keyrings/oss-release-docker.asc
        local source=/etc/apt/sources.list.d/oss-release-docker.sources
        [[ ! -e "$source" ]] || fail 'A Docker source from an earlier partial setup exists; inspect it before retrying.'
        printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nArchitectures: amd64\nSigned-By: /etc/apt/keyrings/oss-release-docker.asc\n' "$os_id" "$os_codename" > "$source"
        apt-get -o Acquire::AllowInsecureRepositories=false update
        apt_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin
        systemctl enable --now docker
    fi
    timeout 15 docker info >/dev/null || fail 'Docker daemon is unavailable; an existing daemon was not restarted.'
    if ! timeout 15 docker buildx version >/dev/null 2>&1; then
        local buildx_package
        for buildx_package in docker-buildx-plugin docker-buildx; do
            if apt-cache policy "$buildx_package" | grep 'Candidate:' | grep -v '(none)' >/dev/null; then
                apt_install "$buildx_package"
                break
            fi
        done
        timeout 15 docker buildx version >/dev/null || fail 'System buildx is unavailable; install a compatible plugin. No existing Docker installation was replaced.'
    fi
}

prepare_aws() {
    if command -v aws >/dev/null 2>&1 && aws --version 2>&1 | grep -q '^aws-cli/2\.'; then return; fi
    local candidate
    candidate="$(apt-cache policy awscli | awk '/Candidate:/ {print $2; exit}')"
    if [[ "$candidate" =~ ^([0-9]+:)?2\. ]]; then
        apt_install awscli
    else
        download https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip "$work_dir/aws.zip"
        download https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip.sig "$work_dir/aws.zip.sig"
        # Only extract the public key from the official installer; never execute downloaded shell.
        download https://awscli.amazonaws.com/v2/install.sh "$work_dir/aws-source.sh"
        python3 - "$work_dir/aws-source.sh" "$work_dir/aws-key.asc" <<'PY'
import re, sys
text = open(sys.argv[1]).read()
keys = re.findall(r'-----BEGIN PGP PUBLIC KEY BLOCK-----.*?-----END PGP PUBLIC KEY BLOCK-----', text, re.S)
if len(keys) != 1:
    raise SystemExit('Official AWS signing key is unavailable')
open(sys.argv[2], 'w').write(keys[0] + '\n')
PY
        verify_aws "$work_dir/aws-key.asc" "$work_dir/aws.zip.sig" "$work_dir/aws.zip"
        unzip -q "$work_dir/aws.zip" -d "$work_dir/aws-extract"
        "$work_dir/aws-extract/aws/install" --install-dir /usr/local/aws-cli --bin-dir /usr/local/bin --update
    fi
    aws --version 2>&1 | grep -q '^aws-cli/2\.' || fail 'AWS CLI v2 is required.'
}

verify_aws() {
    local key="$1" signature="$2" archive="$3" fingerprint
    install -d -m 0700 "$work_dir/gnupg"
    fingerprint="$(gpg --homedir "$work_dir/gnupg" --batch --with-colons --import-options show-only --import "$key" 2>/dev/null | awk -F: '$1=="fpr" {print $10; exit}')"
    [[ "$fingerprint" == FB5DB77FD5C118B80511ADA8A6310ACC4672475C ]] || fail 'AWS signing key fingerprint does not match the official pinned key.'
    gpg --homedir "$work_dir/gnupg" --batch --import "$key" >/dev/null 2>&1
    gpg --homedir "$work_dir/gnupg" --batch --verify "$signature" "$archive" >/dev/null 2>&1 || fail 'AWS package signature verification failed.'
}

runner_asset() {
    python3 - "$1" <<'PY'
import json, re, sys
r = json.load(open(sys.argv[1]))
tag = r.get('tag_name', '')
if r.get('draft') or r.get('prerelease') or not re.fullmatch(r'v[0-9]+\.[0-9]+\.[0-9]+', tag):
    raise SystemExit('Invalid official Runner release')
name = 'actions-runner-linux-x64-' + tag[1:] + '.tar.gz'
assets = [a for a in r.get('assets', []) if a.get('name') == name]
if len(assets) != 1:
    raise SystemExit('Official Linux x64 Runner asset is missing or ambiguous')
asset = assets[0]
digest = asset.get('digest') or ''
url = 'https://github.com/actions/runner/releases/download/' + tag + '/' + name
if asset.get('browser_download_url') != url or not re.fullmatch(r'sha256:[0-9a-f]{64}', digest):
    raise SystemExit('Official Runner asset URL or SHA256 is unavailable')
print(name + '\t' + url + '\t' + digest[7:])
PY
}

prepare_runner() {
    install -d -m 0700 "$runner_cache"
    download https://api.github.com/repos/actions/runner/releases/latest "$work_dir/runner-release.json"
    local asset url digest archive
    IFS=$'\t' read -r asset url digest < <(runner_asset "$work_dir/runner-release.json")
    [[ -n "$digest" ]] || fail 'Cannot verify the official Runner asset.'
    archive="$runner_cache/$asset"
    if [[ ! -f "$archive" ]] || ! printf '%s  %s\n' "$digest" "$archive" | sha256sum -c >/dev/null 2>&1; then
        download "$url" "$work_dir/runner.tar.gz"
        printf '%s  %s\n' "$digest" "$work_dir/runner.tar.gz" | sha256sum -c >/dev/null 2>&1 || fail 'Runner SHA256 verification failed.'
        install -m 0600 "$work_dir/runner.tar.gz" "$archive"
    fi
    if [[ -d "$runner_dir" ]] && [[ -n "$(find "$runner_dir" -mindepth 1 -maxdepth 1 -print -quit)" ]]; then
        [[ -f "$runner_dir/.oss-transfer-prepared" && "$(stat -c %u "$runner_dir")" == 0 && "$(cat "$runner_dir/.oss-transfer-prepared")" == "$digest" ]] || fail 'The unregistered runner directory is not a matching root-owned prepared installation; inspect it before retrying.'
        return
    fi
    install -d -m 0755 "$runner_dir"
    tar -xzf "$archive" -C "$runner_dir"
    [[ -x "$runner_dir/config.sh" && -x "$runner_dir/bin/Runner.Listener" && -x "$runner_dir/bin/installdependencies.sh" ]] || fail 'The official Runner package is incomplete.'
    # The verified official dependency installer handles distribution-specific .NET packages.
    "$runner_dir/bin/installdependencies.sh"
    printf '%s\n' "$digest" > "$runner_dir/.oss-transfer-prepared"
}

preflight() {
    for dependency in bash git curl tar sha256sum openssl python3 mvn jq aws docker runuser timeout df; do
        command -v "$dependency" >/dev/null || fail "Missing dependency: $dependency"
    done
    aws --version 2>&1 | grep -q '^aws-cli/2\.' || fail 'AWS CLI v2 is required.'
    runner_command timeout 15 docker info >/dev/null || fail 'Docker daemon is unavailable to the service account.'
    runner_command timeout 15 docker buildx version >/dev/null || fail 'System Docker buildx plugin is unavailable to the service account.'
    [[ -r /proc/sys/fs/binfmt_misc/qemu-aarch64 ]] && grep -q '^enabled$' /proc/sys/fs/binfmt_misc/qemu-aarch64 && grep -q '^flags:.*F' /proc/sys/fs/binfmt_misc/qemu-aarch64 || fail 'The qemu-aarch64 binfmt handler must be enabled with flag F.'
    local available docker_root location
    docker_root="$(runner_command timeout 15 docker info --format '{{.DockerRootDir}}')"
    [[ "$docker_root" == /* && -d "$docker_root" ]] || fail 'Docker must use a local data directory.'
    for location in "$runner_dir" "$docker_root"; do
        available="$(df -Pk "$location" | awk 'NR==2 {print $4}')"
        [[ "$available" =~ ^[0-9]+$ ]] && (( available >= 10 * 1024 * 1024 )) || fail "At least 10 GiB free disk is required at $location."
    done
}

configure_runner() {
    if [[ -z "$registration_token" ]]; then
        if [[ -t 0 ]]; then printf 'Repository registration token (hidden input): ' >&2; IFS= read -rs registration_token; printf '\n' >&2;
        else IFS= read -r registration_token || true; fi
    fi
    [[ "$registration_token" =~ ^[A-Za-z0-9_.-]+$ ]] && (( ${#registration_token} >= 10 && ${#registration_token} <= 2048 )) || fail 'Provide a short-lived repository registration token via RUNNER_TOKEN or stdin.'
    [[ "$registration_token" != gh[pousr]_* && "$registration_token" != github_pat_* ]] || fail 'Use a repository registration token, not a GitHub personal access token.'
    chown -R "$runner_user:$runner_user" "$runner_dir"
    # Official CommandSettings.cs consumes ACTIONS_RUNNER_INPUT_TOKEN and masks it.
    # stdin avoids token arguments to env/runuser; env -i excludes PATs and parent secrets.
    printf '%s\n' "$registration_token" | runuser -u "$runner_user" -- env -i HOME="$runner_home" PATH="$runner_path" bash -c '
        set +x; set -e
        IFS= read -r ACTIONS_RUNNER_INPUT_TOKEN
        export ACTIONS_RUNNER_INPUT_TOKEN
        cd "$1"; shift
        exec ./config.sh "$@"
    ' runner-config "$runner_dir" --unattended --url "$runner_repo" --name "$runner_name" --labels "$runner_label" --work _work
    unset registration_token
    printf '%s\n' "$runner_label" > "$runner_dir/.oss-transfer-runner-label"
    chown "$runner_user:$runner_user" "$runner_dir/.oss-transfer-runner-label"
}

start_service() {
    [[ -f "$runner_dir/.service" ]] || (cd "$runner_dir" && env -i PATH="$runner_path" HOME=/root ./svc.sh install "$runner_user")
    local service user command listen_since
    service="$(cat "$runner_dir/.service")"
    [[ "$service" =~ ^actions\.runner\.MoonWX-oss-transfer\.[A-Za-z0-9_.-]+\.service$ ]] || fail 'Unexpected Runner service identity.'
    user="$(systemctl show "$service" -p User --value)"
    command="$(systemctl show "$service" -p ExecStart --value)"
    [[ "$user" == "$runner_user" && "$command" == *"$runner_dir/runsvc.sh"* ]] || fail 'Runner service account or executable does not match this installation.'
    if systemctl is-active --quiet "$service"; then
        local existing_pid
        existing_pid="$(systemctl show "$service" -p MainPID --value)"
        [[ "$existing_pid" =~ ^[1-9][0-9]*$ ]] || fail 'The existing Runner service has no live main process.'
        printf 'Runner service is already active: %s. Confirm Online in GitHub repository settings.\n' "$service"
        return
    fi
    listen_since="$(date -u '+%Y-%m-%d %H:%M:%S UTC')"
    (cd "$runner_dir" && env -i PATH="$runner_path" HOME=/root ./svc.sh start)
    local attempt pid
    for ((attempt=0; attempt<45; attempt++)); do
        systemctl is-active --quiet "$service" || fail 'Runner service stopped unexpectedly.'
        pid="$(systemctl show "$service" -p MainPID --value)"
        if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && journalctl -u "$service" --since "$listen_since" -n 300 --no-pager -o cat | grep 'Listening for Jobs' >/dev/null; then
            printf 'Runner service is active and listening for jobs: %s\n' "$service"
            return
        fi
        sleep 2
    done
    fail 'Runner service did not report Listening for Jobs within 90 seconds. Check its journal and GitHub runner status.'
}

main() {
    local mode="${1:-all}"
    [[ "$#" -le 1 && ( "$mode" == all || "$mode" == --prepare-only || "$mode" == --register-only ) ]] || fail 'Usage: setup-release-runner.sh [--prepare-only|--register-only]'
    [[ "$(id -u)" == 0 && "$(uname -s)" == Linux && "$(uname -m)" == x86_64 ]] || fail 'Run as root on Ubuntu/Debian Linux x86_64.'
    [[ -d /run/systemd/system ]] || fail 'A running systemd host is required.'
    # os-release is distribution-owned data, not application configuration.
    . /etc/os-release
    os_id="$ID"; os_codename="${VERSION_CODENAME:-}"
    [[ ( "$os_id" == ubuntu || "$os_id" == debian ) && "$os_codename" =~ ^[a-z0-9]+$ ]] || fail 'Only Ubuntu/Debian with an official APT release codename is supported.'
    [[ "$runner_label" =~ ^[A-Za-z0-9_-]{1,64}$ && "$runner_name" =~ ^[A-Za-z0-9_-]{1,64}$ ]] || fail 'Runner label/name must contain only letters, digits, underscore or hyphen (at most 64).'
    [[ ! -L "$runner_dir" && ! -L "$runner_cache" ]] || fail 'Runner installation and cache must not be symbolic links.'
    work_dir="$(mktemp -d)"; chmod 0700 "$work_dir"; trap cleanup EXIT
    if validate_existing; then
        unset registration_token
        preflight; start_service; return
    fi
    if [[ "$mode" != --register-only ]]; then
        printf 'Preparing release dependencies and the verified official Runner.\n'
        apt-get -o Acquire::AllowInsecureRepositories=false update
        apt_install ca-certificates curl git jq python3 maven unzip gnupg openssl tar util-linux coreutils qemu-user-static binfmt-support groff less
        prepare_docker; prepare_account; prepare_aws
        update-binfmts --enable qemu-aarch64
        prepare_runner
    else
        [[ -f "$runner_dir/.oss-transfer-prepared" && -x "$runner_dir/config.sh" ]] || fail 'Run --prepare-only first.'
    fi
    preflight
    if [[ "$mode" == --prepare-only ]]; then printf 'Prepared. Generate a repository registration token, then run this script with --register-only.\n'; return; fi
    configure_runner
    start_service
}
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
