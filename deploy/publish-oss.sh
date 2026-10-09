#!/bin/bash
# Usage: publish-oss.sh <vX.Y.Z[-prerelease]> [artifact-directory]
# Requires Git tag refs, AWS CLI, and credentials supplied through environment.
set +x
set -euo pipefail

fail() { echo "OSS publish: $*" >&2; exit 1; }
[[ $# -ge 1 && $# -le 2 ]] || fail "usage: $0 <tag> [artifact-directory]"
TAG="$1"
[[ "$TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$ ]] || fail "invalid release tag"
VERSION="${TAG#v}"
UPDATE_LATEST=1
if [[ "$VERSION" = *-* ]]; then
    UPDATE_LATEST=0
    IFS='.' read -r -a identifiers <<< "${VERSION#*-}"
    for identifier in "${identifiers[@]}"; do
        [[ ! "$identifier" =~ ^0[0-9]+$ ]] || fail "invalid numeric prerelease identifier"
    done
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SOURCE_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
ARTIFACT_DIR="${2:-$SOURCE_DIR/output}"
for tool in aws git tar openssl; do
    command -v "$tool" >/dev/null 2>&1 || fail "missing required tool: $tool"
done
[[ -n "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_SECRET_ACCESS_KEY:-}" ]] || fail "AWS credential environment variables are required"
HEAD_COMMIT="$(git -C "$SOURCE_DIR" rev-parse --verify 'HEAD^{commit}')"
TAG_COMMIT="$(git -C "$SOURCE_DIR" rev-parse --verify "refs/tags/$TAG^{commit}")" || fail "release tag is not available locally"
[[ "$HEAD_COMMIT" = "$TAG_COMMIT" ]] || fail "release tag does not point to the current checkout"

# Publish a private snapshot so concurrent local builds cannot change the files
# after validation. The configuration contains no access keys.
umask 077
STAGE_DIR="$(mktemp -d "${TMPDIR:-/tmp}/oss-publish.XXXXXX")"
trap 'rm -rf -- "$STAGE_DIR"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
ARCHIVE="oss-transfer.tar.gz"
CHECKSUM="$ARCHIVE.sha256"
for name in "$ARCHIVE" "$CHECKSUM"; do
    [[ -f "$ARTIFACT_DIR/$name" ]] || fail "missing artifact: $name"
    cp -- "$ARTIFACT_DIR/$name" "$STAGE_DIR/$name"
done
checksum_line="$(cat "$STAGE_DIR/$CHECKSUM")"
[[ "$checksum_line" =~ ^([0-9a-fA-F]{64})[[:blank:]]+\*?oss-transfer\.tar\.gz$ ]] || fail "invalid checksum file"
EXPECTED_SHA256="${BASH_REMATCH[1]}"
if command -v sha256sum >/dev/null 2>&1; then
    ACTUAL_SHA256="$(sha256sum "$STAGE_DIR/$ARCHIVE")"
else
    command -v shasum >/dev/null 2>&1 || fail "missing SHA-256 tool"
    ACTUAL_SHA256="$(shasum -a 256 "$STAGE_DIR/$ARCHIVE")"
fi
[[ "${ACTUAL_SHA256%% *}" = "$(printf '%s' "$EXPECTED_SHA256" | tr '[:upper:]' '[:lower:]')" ]] || fail "archive SHA-256 does not match"
[[ "$(wc -c < "$STAGE_DIR/$ARCHIVE")" -le 5368709120 ]] || fail "archive exceeds the OSS PutObject 5 GiB limit"
tar -xOzf "$STAGE_DIR/$ARCHIVE" oss-transfer/VERSION > "$STAGE_DIR/VERSION" || fail "cannot read package VERSION"
PACKAGE_VERSION="$(sed -n 's/^Version: //p' "$STAGE_DIR/VERSION")"
PACKAGE_COMMIT="$(sed -n 's/^Commit: //p' "$STAGE_DIR/VERSION")"
[[ "$PACKAGE_VERSION" = "$VERSION" ]] || fail "package version does not match the release tag"
[[ "$PACKAGE_COMMIT" =~ ^[0-9a-f]{7,40}$ ]] || fail "invalid package commit"
PACKAGE_COMMIT_FULL="$(git -C "$SOURCE_DIR" rev-parse --verify "$PACKAGE_COMMIT^{commit}")" || fail "package commit is unavailable or ambiguous"
[[ "$HEAD_COMMIT" = "$PACKAGE_COMMIT_FULL" ]] || fail "package commit does not match the release tag"

# JD OSS documents SigV4 and virtual-hosted requests for PutObject:
# https://docs.jdcloud.com/cn/object-storage-service/api/user-signature-authentication-s3
# https://docs.jdcloud.com/cn/object-storage-service/api/put-object-2
cat > "$STAGE_DIR/aws-config" <<'CONFIG'
[default]
region = cn-north-1
s3 =
    signature_version = s3v4
    addressing_style = virtual
    payload_signing_enabled = true
CONFIG
export AWS_CONFIG_FILE="$STAGE_DIR/aws-config"
export AWS_SHARED_CREDENTIALS_FILE="$STAGE_DIR/unused-credentials"
export AWS_PROFILE=default AWS_DEFAULT_PROFILE=default
export AWS_EC2_METADATA_DISABLED=true AWS_PAGER='' AWS_CLI_AUTO_PROMPT=off
# Avoid optional AWS-specific checksum trailers on an S3-compatible endpoint.
export AWS_REQUEST_CHECKSUM_CALCULATION=when_required
export AWS_RESPONSE_CHECKSUM_VALIDATION=when_required
unset AWS_SESSION_TOKEN AWS_SECURITY_TOKEN

upload() {
    local prefix="$1" name="$2" content_type="$3" md5
    md5="$(openssl dgst -md5 -binary "$STAGE_DIR/$name" | openssl base64 -A)"
    aws --endpoint-url https://s3.cn-north-1.jdcloud-oss.com --region cn-north-1 \
        s3api put-object --bucket moonwx --key "osstransfer/$prefix/$name" \
        --body "$STAGE_DIR/$name" --content-md5 "$md5" \
        --content-type "$content_type" --cache-control no-cache >/dev/null
}
publish_directory() {
    local prefix="$1"
    echo "Publishing s3://moonwx/osstransfer/$prefix/"
    upload "$prefix" "$ARCHIVE" application/gzip
    upload "$prefix" "$CHECKSUM" 'text/plain; charset=utf-8'
    # VERSION is written only after both payloads succeed. Multiple objects
    # are not updated atomically; a failed upload can leave partial payloads.
    upload "$prefix" VERSION 'text/plain; charset=utf-8'
}
publish_directory "$TAG"
if [[ "$UPDATE_LATEST" -eq 1 ]]; then
    publish_directory latest
fi
echo "OSS publish completed for $TAG"
