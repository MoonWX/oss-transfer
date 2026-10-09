#!/bin/bash
# Validate the hosted build's archive before a registry credential is used.
set +x
set -euo pipefail
fail() { printf 'Release image: %s\n' "$*" >&2; exit 1; }
[[ $# == 2 || ( $# == 3 && "${3:-}" == --ci-build ) ]] || \
    fail 'usage: verify-release-image.sh <release-tag> <artifact-directory> [--ci-build]'
tag="$1"
artifact_dir="$2"
[[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+\.)*[0-9A-Za-z-]+)?$ ]] || fail 'invalid release tag'
version="${tag#v}"
if [[ "$version" == *-* ]]; then
    IFS='.' read -r -a identifiers <<< "${version#*-}"
    for identifier in "${identifiers[@]}"; do
        [[ ! "$identifier" =~ ^0[0-9]+$ ]] || fail 'invalid numeric prerelease identifier'
    done
fi
source_dir="$(cd "$(dirname "$0")/.." && pwd)"
commit="$(git -C "$source_dir" rev-parse --verify 'HEAD^{commit}')"
project_version="$(sed -n 's/^[[:space:]]*<version>\([^<]*\)<\/version>.*/\1/p' "$source_dir/pom.xml" | head -n 1)"
[[ "$version" == "$project_version" ]] || fail 'image version does not match the project version'
if [[ "${3:-}" != --ci-build ]]; then
    tag_commit="$(git -C "$source_dir" rev-parse --verify "refs/tags/$tag^{commit}")" || fail 'release tag is unavailable'
    [[ "$commit" == "$tag_commit" ]] || fail 'release tag does not point to this checkout'
fi
for tool in skopeo jq; do command -v "$tool" >/dev/null || fail "missing tool: $tool"; done
archive="$artifact_dir/oss-transfer-image.tar"
checksum="$archive.sha256"
[[ -f "$archive" && -f "$checksum" ]] || fail 'image archive or checksum is missing'
checksum_line="$(cat "$checksum")"
[[ "$checksum_line" =~ ^([0-9a-fA-F]{64})[[:blank:]]+\*?oss-transfer-image\.tar$ ]] || fail 'invalid image checksum file'
expected_digest="$(printf '%s' "${BASH_REMATCH[1]}" | tr '[:upper:]' '[:lower:]')"
if command -v sha256sum >/dev/null; then actual_digest="$(sha256sum "$archive")";
else actual_digest="$(shasum -a 256 "$archive")"; fi
[[ "${actual_digest%% *}" == "$expected_digest" ]] || fail 'image SHA-256 does not match'
umask 077
state="$(mktemp -d "${TMPDIR:-/tmp}/oss-image-verify.XXXXXX")"
trap 'rm -rf -- "$state"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
skopeo --tmpdir "$state" --command-timeout 2m inspect --raw "oci-archive:$archive" > "$state/manifest.json"
jq -e '(.mediaType == "application/vnd.docker.distribution.manifest.list.v2+json" or
        .mediaType == "application/vnd.oci.image.index.v1+json") and
    ([.manifests[].platform | .os + "/" + .architecture] | sort) == ["linux/amd64", "linux/arm64"]' \
    "$state/manifest.json" >/dev/null || fail 'image must contain exactly linux/amd64 and linux/arm64'
for arch in amd64 arm64; do
    skopeo --tmpdir "$state" --command-timeout 2m --override-os linux --override-arch "$arch" inspect --config \
        "oci-archive:$archive" > "$state/config.json"
    jq -e --arg arch "$arch" --arg version "$version" --arg commit "$commit" \
        '.os == "linux" and .architecture == $arch and
         .config.Labels["org.opencontainers.image.version"] == $version and
         .config.Labels["org.opencontainers.image.revision"] == $commit' \
        "$state/config.json" >/dev/null || fail "image $arch version or commit does not match the checkout"
done
printf 'Verified release image: %s (%s)\n' "$tag" "$commit"
