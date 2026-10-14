#!/usr/bin/env python3
"""Publish verified Go assets to a version directory, then advance latest metadata."""
import argparse
import base64
import hashlib
import json
import mimetypes
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

ENDPOINT = "https://s3.cn-north-1.jdcloud-oss.com"
BUCKET = "moonwx"
PREFIX = "osstransfer"
LATEST = ("install.sh", "Install.ps1", "docker-compose.yml", "VERSION", "manifest.tsv", "release.json")
# Retire only the old mutable Java bundle URLs. Versioned V4.0.x downloads remain.
RETIRED_LATEST = ("oss-transfer.tar.gz", "oss-transfer.tar.gz.sha256", "install.sh.sha256")


def digest(path, algorithm="sha256"):
    value = hashlib.new(algorithm)
    with path.open("rb") as stream:
        for data in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(data)
    return value


def verify(directory, tag, commit):
    if not re.fullmatch(r"v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?", tag):
        raise ValueError("Invalid release tag")
    if "-" in tag and any(re.fullmatch(r"0[0-9]+", value) for value in tag.split("-", 1)[1].split(".")):
        raise ValueError("Invalid numeric prerelease identifier")
    release = json.loads((directory / "release.json").read_text(encoding="utf-8"))
    if (release.get("format"), release.get("tag"), release.get("version"), release.get("commit")) != ("oss-transfer-release-v1", tag, tag[1:], commit):
        raise ValueError("Release metadata does not match the tagged commit")
    records = {}
    for row in release["assets"]:
        name = row["name"]
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", name) or name in records or name in ("release.json", "SHA256SUMS"):
            raise ValueError("Invalid or duplicate release asset")
        records[name] = row
    checks = {}
    for line in (directory / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
        checksum, name = line.split("  ", 1)
        if name in checks or not re.fullmatch(r"[a-f0-9]{64}", checksum):
            raise ValueError("Invalid or duplicate release checksum")
        checks[name] = checksum
    if set(checks) != set(records) | {"release.json"}:
        raise ValueError("Release checksums are incomplete")
    expected = set(checks) | {"SHA256SUMS"}
    if {path.name for path in directory.iterdir()} != expected:
        raise ValueError("Release directory has missing or unlisted files")
    for name in expected:
        path = directory / name
        if path.is_symlink() or not path.is_file():
            raise ValueError("Release asset is not a regular file")
        if path.stat().st_size > 5 * 1024 ** 3:
            raise ValueError("Release asset exceeds the single upload size limit")
        if name == "SHA256SUMS":
            continue
        checksum = digest(path).hexdigest()
        if checksum != checks[name]:
            raise ValueError("Release checksum mismatch: " + name)
        if name in records and (records[name]["sha256"] != checksum or records[name]["size"] != path.stat().st_size):
            raise ValueError("Release manifest differs from asset: " + name)
    if not set(LATEST).issubset(expected):
        raise ValueError("Release is missing installer metadata")
    return sorted(expected)


def publish(assets, tag, commit, dry_run=False):
    # Validate a private copy so another local build cannot change bytes between
    # checksum validation and uploading them.
    with tempfile.TemporaryDirectory(prefix="oss-go-publish-") as work:
        root = Path(work)
        snapshot = root / "assets"
        snapshot.mkdir()
        for path in assets.iterdir():
            if path.is_symlink() or not path.is_file():
                raise ValueError("Release directory contains a non-regular asset")
            shutil.copyfile(path, snapshot / path.name)
        names = verify(snapshot, tag, commit)
        latest = "-" not in tag
        if dry_run:
            print(f"Verified {len(names)} assets for {tag} ({commit}); latest update: {latest}")
            return
        if not os.environ.get("AWS_ACCESS_KEY_ID") or not os.environ.get("AWS_SECRET_ACCESS_KEY"):
            raise ValueError("AWS credential environment variables are required")
        config = root / "aws-config"
        config.write_text("[default]\nregion = cn-north-1\ns3 =\n    signature_version = s3v4\n    addressing_style = virtual\n    payload_signing_enabled = true\n")
        environment = dict(os.environ, AWS_CONFIG_FILE=str(config), AWS_SHARED_CREDENTIALS_FILE=str(root / "unused-credentials"),
                           AWS_PROFILE="default", AWS_DEFAULT_PROFILE="default", AWS_EC2_METADATA_DISABLED="true", AWS_PAGER="",
                           AWS_CLI_AUTO_PROMPT="off", AWS_REQUEST_CHECKSUM_CALCULATION="when_required", AWS_RESPONSE_CHECKSUM_VALIDATION="when_required")
        environment.pop("AWS_SESSION_TOKEN", None)
        environment.pop("AWS_SECURITY_TOKEN", None)

        def upload(prefix, name):
            path = snapshot / name
            md5 = base64.b64encode(digest(path, "md5").digest()).decode()
            content_type = mimetypes.guess_type(name)[0] or "application/octet-stream"
            subprocess.run(["aws", "--endpoint-url", ENDPOINT, "--region", "cn-north-1", "--cli-connect-timeout", "15", "--cli-read-timeout", "120",
                            "s3api", "put-object", "--bucket", BUCKET, "--key", f"{PREFIX}/{prefix}/{name}", "--body", str(path),
                            "--content-md5", md5, "--content-type", content_type, "--cache-control", "no-cache"],
                           env=environment, check=True, stdout=subprocess.DEVNULL, timeout=600)

        for name in names:
            upload(tag, name)
        print("Published " + tag)
        if latest:
            # Installers use manifest.tsv to choose the immutable version URL.
            # release.json is the final commit point for clients selecting latest.
            for name in LATEST:
                upload("latest", name)
            for name in RETIRED_LATEST:
                subprocess.run(["aws", "--endpoint-url", ENDPOINT, "--region", "cn-north-1", "--cli-connect-timeout", "15", "--cli-read-timeout", "120",
                                "s3api", "delete-object", "--bucket", BUCKET, "--key", f"{PREFIX}/latest/{name}"],
                               env=environment, check=True, stdout=subprocess.DEVNULL, timeout=180)
            print("Updated latest metadata and retired the old latest Java bundle")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--assets", type=Path, required=True)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    repository = Path(__file__).resolve().parents[2]
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=repository, text=True).strip()
    tag_commit = subprocess.check_output(["git", "rev-parse", "--verify", "refs/tags/" + args.tag + "^{commit}"], cwd=repository, text=True).strip()
    if head != tag_commit:
        parser.error("Release tag does not point to this checkout")
    publish(args.assets.resolve(), args.tag, head, args.dry_run)
