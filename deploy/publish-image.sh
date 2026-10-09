#!/bin/bash
# Copy one hosted build to a registry; never build on the publishing runner.
# Credentials must be supplied as REGISTRY_USERNAME and REGISTRY_PASSWORD.
set +x
set -euo pipefail
fail() { printf 'Image publish: %s\n' "$*" >&2; exit 1; }
[[ $# == 3 ]] || fail 'usage: publish-image.sh <release-tag> <docker-hub|jcr> <artifact-directory>'
tag="$1"
case "$2" in
    docker-hub) registry=docker.io; image=docker.io/moonwx/osstransfer ;;
    jcr) registry=moonwx-cn-north-1.jcr.service.jdcloud.com; image="$registry/osstransfer" ;;
    *) fail 'unsupported registry destination' ;;
esac
artifact_dir="$3"
script_dir="$(cd "$(dirname "$0")" && pwd)"
umask 077
state="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/oss-image-publish.XXXXXX")"
trap 'rm -rf -- "$state"; unset REGISTRY_USERNAME REGISTRY_PASSWORD' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
for name in oss-transfer-image.tar oss-transfer-image.tar.sha256; do
    [[ -f "$artifact_dir/$name" ]] || fail "missing artifact: $name"
    cp -- "$artifact_dir/$name" "$state/$name"
done
# Work exclusively from a verified private snapshot, including on persistent runners.
bash "$script_dir/verify-release-image.sh" "$tag" "$state"
[[ -n "${REGISTRY_USERNAME:-}" && -n "${REGISTRY_PASSWORD:-}" ]] || fail 'registry credentials are required'
auth_file="$state/auth.json"
printf '%s' "$REGISTRY_PASSWORD" | skopeo --tmpdir "$state" --command-timeout 2m login --authfile "$auth_file" \
    --username "$REGISTRY_USERNAME" --password-stdin "$registry" >/dev/null
unset REGISTRY_USERNAME REGISTRY_PASSWORD
version="${tag#v}"
source="oci-archive:$state/oss-transfer-image.tar"
skopeo --tmpdir "$state" --command-timeout 15m copy --retry-times 2 --all --format v2s2 \
    --authfile "$auth_file" --digestfile "$state/digest" "$source" "docker://$image:$version"
skopeo --tmpdir "$state" --command-timeout 2m inspect --authfile "$auth_file" --raw \
    "docker://$image:$version" > "$state/manifest.json"
jq -e '.mediaType == "application/vnd.docker.distribution.manifest.list.v2+json" and
    ([.manifests[].platform | .os + "/" + .architecture] | sort) == ["linux/amd64", "linux/arm64"]' \
    "$state/manifest.json" >/dev/null || fail 'registry image must contain both Docker schema 2 architectures'
actual_digest="$(skopeo --tmpdir "$state" manifest-digest "$state/manifest.json")"
[[ "$actual_digest" == "$(cat "$state/digest")" ]] || fail 'registry manifest differs from the uploaded image'

# Retrieve actual blobs for both platforms before updating any stable aliases.
for arch in amd64 arm64; do
    skopeo --tmpdir "$state" --command-timeout 5m --override-os linux --override-arch "$arch" copy --retry-times 2 \
        --authfile "$auth_file" "docker://$image:$version" "dir:$state/pull-$arch"
    skopeo --tmpdir "$state" --command-timeout 2m --override-os linux --override-arch "$arch" inspect --config \
        "$source" | jq -S . > "$state/source-$arch.json"
    skopeo --tmpdir "$state" --command-timeout 2m --override-os linux --override-arch "$arch" inspect --config \
        "dir:$state/pull-$arch" | jq -S . > "$state/pulled-$arch.json"
    cmp "$state/source-$arch.json" "$state/pulled-$arch.json" || fail "registry $arch configuration differs from the built artifact"
    rm -rf -- "$state/pull-$arch"
done
if [[ "$version" != *-* ]]; then
    minor="${version%.*}"
    for alias in "$minor" latest; do
        skopeo --tmpdir "$state" --command-timeout 3m copy --retry-times 2 --all --preserve-digests \
            --authfile "$auth_file" "docker://$image@$actual_digest" "docker://$image:$alias"
        skopeo --tmpdir "$state" --command-timeout 2m inspect --authfile "$auth_file" --raw \
            "docker://$image:$alias" > "$state/alias.json"
        [[ "$(skopeo --tmpdir "$state" manifest-digest "$state/alias.json")" == "$actual_digest" ]] || fail "registry alias $alias differs from the versioned image"
    done
fi
printf 'Image publish completed: %s:%s\n' "$image" "$version"
