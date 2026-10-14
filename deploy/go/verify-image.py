#!/usr/bin/env python3
"""Check the archived two-platform image before loading or publishing it."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess


def verify(archive, version, commit):
    checksum = Path(str(archive) + ".sha256").read_text(encoding="utf-8").split()
    digest = hashlib.sha256()
    with archive.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    if checksum != [digest.hexdigest(), archive.name]:
        raise ValueError("Image archive checksum mismatch")
    source = "oci-archive:" + str(archive)
    manifest = json.loads(subprocess.check_output(["skopeo", "--command-timeout", "2m", "inspect", "--raw", source], timeout=150))
    if manifest.get("mediaType") not in ("application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json"):
        raise ValueError("Image archive is not a multi-platform index")
    platforms = sorted((row.get("platform", {}).get("os"), row.get("platform", {}).get("architecture")) for row in manifest.get("manifests", []))
    if platforms != [("linux", "amd64"), ("linux", "arm64")]:
        raise ValueError("Image must contain exactly Linux amd64 and arm64")
    for arch in ("amd64", "arm64"):
        config = json.loads(subprocess.check_output(["skopeo", "--command-timeout", "2m", "--override-os", "linux", "--override-arch", arch, "inspect", "--config", source], timeout=150))
        labels = config.get("config", {}).get("Labels", {})
        if (config.get("os"), config.get("architecture"), labels.get("org.opencontainers.image.version"), labels.get("org.opencontainers.image.revision")) != ("linux", arch, version, commit):
            raise ValueError("Image platform or release identity differs: " + arch)
    print("Verified image: " + version + " (" + commit + ")")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    args = parser.parse_args()
    verify(args.archive.resolve(), args.version, args.commit)
