#!/usr/bin/env python3
"""Publish verified Go assets to a version directory, then advance latest metadata."""
import argparse
import base64
from concurrent.futures import FIRST_EXCEPTION, ThreadPoolExecutor, wait
import hashlib
import importlib.util
import json
import mimetypes
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import threading
import time

ENDPOINT = "https://s3.cn-north-1.jdcloud-oss.com"
BUCKET = "moonwx"
PREFIX = "oss-transfer"
LATEST = ("install.sh", "Install.ps1", "manifest.tsv", "release.json")
# Only these obsolete mutable URLs are removed, and only when still listed. Old
# version directories still serve the installers and binaries used by Masters
# pinned to those releases.
RETIRED_LATEST = ("VERSION", "docker-compose.yml", "oss-transfer.tar.gz", "oss-transfer.tar.gz.sha256", "install.sh.sha256")
# Parallel streams hide the per-connection latency to cn-north-1 from US runners.
UPLOAD_WORKERS = 8
SPEC = importlib.util.spec_from_file_location("oss_release_platforms", Path(__file__).with_name("platforms.py"))
PLATFORMS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PLATFORMS)


def release_names():
    return {PLATFORMS.archive_name(*target) for target in PLATFORMS.PLATFORMS} | {
        "install.sh", "native-install.sh", "Install.ps1", "manifest.tsv", "release.json", "SHA256SUMS",
    }


def plain_md5(etag):
    # Single-part PUT ETags are the quoted MD5; multipart ETags end in -<parts>.
    value = etag.strip('"').lower() if isinstance(etag, str) else ""
    return value if re.fullmatch(r"[0-9a-f]{32}", value) else None


def verify_listing(page, prefix, names, local=None):
    """Check the exact key set; with local {name: (md5 hex, size)}, also check content."""
    expected = {f"{PREFIX}/{prefix}/{name}" for name in names}
    if not isinstance(page, dict) or page.get("IsTruncated") is not False or page.get("NextContinuationToken"):
        raise ValueError("OSS release listing is incomplete or paginated")
    rows = page.get("Contents", [])
    if not isinstance(rows, list) or len(rows) > 100:
        raise ValueError("OSS release listing exceeds its bound")
    keys = []
    for row in rows:
        key = row.get("Key") if isinstance(row, dict) else None
        if not isinstance(key, str) or not key.startswith(f"{PREFIX}/{prefix}/") or key in keys:
            raise ValueError("OSS release listing contains an invalid or duplicate key")
        keys.append(key)
    if set(keys) != expected:
        raise ValueError("OSS release directory contains missing or unexpected files")
    if local is not None:
        for row in rows:
            name = row["Key"].rsplit("/", 1)[1]
            md5, size = local[name]
            if type(row.get("Size")) is not int or row["Size"] != size:
                raise ValueError("OSS object size differs from the release asset: " + name)
            etag = plain_md5(row.get("ETag"))
            # Fall back to key and size when the ETag is not a plain MD5.
            if etag is not None and etag != md5:
                raise ValueError("OSS object ETag differs from the release asset MD5: " + name)
    return len(keys)


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
    if expected != release_names():
        raise ValueError("Release must contain only supported platform archives and installer metadata")
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
    with tempfile.TemporaryDirectory(prefix="oss-publish-") as work:
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
            print(f"Verified {len(names)} assets for {tag} ({commit}); latest update: {latest}", flush=True)
            return
        if not os.environ.get("AWS_ACCESS_KEY_ID") or not os.environ.get("AWS_SECRET_ACCESS_KEY"):
            raise ValueError("AWS credential environment variables are required")
        config = root / "aws-config"
        config.write_text("[default]\nregion = cn-north-1\ns3 =\n    signature_version = s3v4\n    addressing_style = virtual\n    payload_signing_enabled = true\n")
        environment = dict(os.environ, AWS_CONFIG_FILE=str(config), AWS_SHARED_CREDENTIALS_FILE=str(root / "unused-credentials"),
                           AWS_PROFILE="default", AWS_DEFAULT_PROFILE="default", AWS_EC2_METADATA_DISABLED="true", AWS_PAGER="",
                           AWS_CLI_AUTO_PROMPT="off", AWS_REQUEST_CHECKSUM_CALCULATION="when_required", AWS_RESPONSE_CHECKSUM_VALIDATION="when_required",
                           AWS_RETRY_MODE="standard", AWS_MAX_ATTEMPTS="5")
        environment.pop("AWS_SESSION_TOKEN", None)
        environment.pop("AWS_SECURITY_TOKEN", None)

        md5s = {name: digest(snapshot / name, "md5") for name in names}
        local = {name: (md5s[name].hexdigest(), (snapshot / name).stat().st_size) for name in names}
        output = threading.Lock()

        def log(message):
            with output:
                print(message, flush=True)

        def aws(*arguments, timeout=180, **options):
            return subprocess.run(["aws", "--endpoint-url", ENDPOINT, "--region", "cn-north-1", "--cli-connect-timeout", "15",
                                   "--cli-read-timeout", "120", *arguments], env=environment, timeout=timeout, **options)

        def upload(prefix, name):
            path = snapshot / name
            content_type = mimetypes.guess_type(name)[0] or "application/octet-stream"
            started = time.monotonic()
            try:
                aws("s3api", "put-object", "--bucket", BUCKET, "--key", f"{PREFIX}/{prefix}/{name}", "--body", str(path),
                    "--content-md5", base64.b64encode(md5s[name].digest()).decode(), "--content-type", content_type,
                    "--cache-control", "no-cache", check=True, stdout=subprocess.DEVNULL, timeout=600)
            except Exception:
                log(f"Upload failed: {prefix}/{name}")
                raise
            log(f"Uploaded {prefix}/{name} ({local[name][1]} bytes) in {time.monotonic() - started:.1f}s")

        def upload_all(prefix, names):
            # Nothing references a version directory until latest/ advances, so
            # its objects may land in any order. Stop queueing after a failure.
            with ThreadPoolExecutor(max_workers=UPLOAD_WORKERS) as pool:
                futures = [pool.submit(upload, prefix, name) for name in names]
                for future in wait(futures, return_when=FIRST_EXCEPTION).not_done:
                    future.cancel()
            for future in futures:
                if not future.cancelled():
                    future.result()

        def listing(prefix):
            result = aws("--no-paginate", "--output", "json", "s3api", "list-objects-v2",
                         "--bucket", BUCKET, "--prefix", f"{PREFIX}/{prefix}/", "--max-keys", "100", capture_output=True)
            if result.returncode != 0:
                raise ValueError("OSS release directory verification failed; check list permission and connectivity")
            if len(result.stdout) > 128 * 1024:
                raise ValueError("OSS release listing exceeds its response bound")
            try:
                return json.loads(result.stdout)
            except (ValueError, UnicodeError):
                raise ValueError("OSS release listing is not valid JSON") from None

        def verify_remote(prefix, expected, page=None):
            page = listing(prefix) if page is None else page
            count = verify_listing(page, prefix, expected, local)
            checked = sum(plain_md5(row.get("ETag")) is not None for row in page["Contents"])
            log(f"Verified {prefix} directory: {count} files, {checked} by MD5 ETag")

        started = time.monotonic()
        upload_all(tag, names)
        log(f"Published {tag} in {time.monotonic() - started:.1f}s")
        # Prove the version directory before latest/ can point clients at it.
        verify_remote(tag, names)
        if latest:
            # Installers use manifest.tsv to choose the immutable version URL.
            # release.json is the final commit point for clients selecting latest.
            for name in LATEST:
                upload("latest", name)
            page = listing("latest")
            rows = page.get("Contents", []) if isinstance(page, dict) else []
            present = {row.get("Key") for row in rows if isinstance(row, dict)}
            retired = [name for name in RETIRED_LATEST if f"{PREFIX}/latest/{name}" in present]
            for name in retired:
                aws("s3api", "delete-object", "--bucket", BUCKET, "--key", f"{PREFIX}/latest/{name}",
                    check=True, stdout=subprocess.DEVNULL)
                log(f"Removed obsolete latest/{name}")
            log("Updated latest metadata")
            verify_remote("latest", LATEST, None if retired else page)


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
