#!/usr/bin/env python3
"""Copy the public release tree to oss-transfer/ without changing old downloads."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
from urllib.parse import quote
from urllib.request import urlopen

ENDPOINT = "https://s3.cn-north-1.jdcloud-oss.com"
PUBLIC = "https://moonwx.s3.cn-north-1.jdcloud-oss.com/"
BUCKET = "moonwx"
SOURCE = "osstransfer/"
TARGET = "oss-transfer/"
METADATA = ("ContentType", "CacheControl", "ContentDisposition", "ContentEncoding", "ContentLanguage", "Expires", "Metadata")
SAMPLES = ("latest/install.sh", "latest/manifest.tsv", "latest/release.json")


def destination(key):
    if not isinstance(key, str) or not key.startswith(SOURCE) or len(key) == len(SOURCE):
        raise ValueError("Object is outside the source release prefix")
    return TARGET + key[len(SOURCE):]


def identity(row):
    size = row.get("Size", row.get("ContentLength"))
    etag = row.get("ETag")
    if not isinstance(size, int) or isinstance(size, bool) or size < 0 or size > 5 * 1024 ** 3:
        raise ValueError("Object size is invalid or exceeds CopyObject's 5 GiB limit")
    if not isinstance(etag, str) or not etag:
        raise ValueError("Object has no server ETag")
    return size, etag


def acl_identity(policy):
    # Display names are optional; IDs and grants determine access.
    return policy["Owner"]["ID"], sorted(json.dumps({
        "Permission": grant["Permission"],
        "Grantee": {key: value for key, value in grant["Grantee"].items() if key != "DisplayName"},
    }, sort_keys=True) for grant in policy["Grants"])


class OSS:
    def __init__(self, environment):
        self.environment = environment

    def call(self, operation, *arguments):
        result = subprocess.run([
            "aws", "--endpoint-url", ENDPOINT, "--region", "cn-north-1", "--cli-connect-timeout", "15",
            "--cli-read-timeout", "120", "--no-paginate", "--output", "json", "s3api", operation,
            "--bucket", BUCKET, *arguments,
        ], env=self.environment, capture_output=True, timeout=180)
        if result.returncode:
            error = re.search(rb"An error occurred \(([A-Za-z0-9_.:-]{1,80})\)", result.stderr)
            reason = error.group(1).decode() if error else f"exit {result.returncode}"
            raise RuntimeError(f"OSS {operation} failed ({reason}); no source objects were deleted")
        if len(result.stdout) > 4 * 1024 ** 2:
            raise ValueError("OSS response exceeds its bound")
        return json.loads(result.stdout or b"{}")

    def listing(self, prefix):
        if prefix not in (SOURCE, TARGET):
            raise ValueError("Unexpected release prefix")
        objects, tokens, token = {}, set(), None
        while True:
            arguments = ["--prefix", prefix, "--max-keys", "1000"]
            if token:
                arguments += ["--continuation-token", token]
            page = self.call("list-objects-v2", *arguments)
            for row in page.get("Contents", []):
                key = row["Key"]
                if not key.startswith(prefix) or key in objects or key == prefix:
                    raise ValueError("Listing contains a duplicate or out-of-prefix object")
                objects[key] = identity(row)
                if len(objects) > 10000:
                    raise ValueError("Release tree exceeds the migration's 10,000-object bound")
            if page.get("IsTruncated") is False:
                return objects
            token = page.get("NextContinuationToken")
            if page.get("IsTruncated") is not True or not isinstance(token, str) or not token or token in tokens:
                raise ValueError("Incomplete OSS listing or repeated continuation token")
            tokens.add(token)

    def head(self, key):
        return self.call("head-object", "--key", key)

    def acl(self, key):
        return self.call("get-object-acl", "--key", key)

    def copy(self, source, target, head):
        if target != destination(source):
            raise ValueError("Copy target differs from the exact prefix mapping")
        arguments = ["--key", target, "--copy-source", quote(BUCKET + "/" + source, safe="/"),
                     "--copy-source-if-match", head["ETag"], "--metadata-directive", "COPY"]
        if head.get("StorageClass"):
            arguments += ["--storage-class", head["StorageClass"]]
        self.call("copy-object", *arguments)

    def set_acl(self, key, policy):
        if not key.startswith(TARGET):
            raise ValueError("ACL update is outside the destination prefix")
        self.call("put-object-acl", "--key", key, "--access-control-policy", json.dumps(policy))

    def public_sha256(self, key):
        digest = hashlib.sha256()
        with urlopen(PUBLIC + quote(key, safe="/"), timeout=120) as response:
            for chunk in iter(lambda: response.read(1024 * 1024), b""):
                digest.update(chunk)
        return digest.hexdigest()


def verify_object(store, source, target, source_head, target_head):
    if identity(source_head)[0] != identity(target_head)[0]:
        raise ValueError("Destination size differs: " + target)
    if any(source_head.get(field) != target_head.get(field) for field in METADATA):
        raise ValueError("Destination metadata differs: " + target)
    for field in ("ChecksumSHA256", "ChecksumSHA1", "ChecksumCRC32", "ChecksumCRC32C"):
        if source_head.get(field) and target_head.get(field) and source_head[field] != target_head[field]:
            raise ValueError("Destination server checksum differs: " + target)
    if source_head["ETag"] != target_head["ETag"]:
        # A multipart source can acquire a different ETag after CopyObject.
        # Compare bytes in that case; an ETag is not assumed to be an MD5 hash.
        if store.public_sha256(source) != store.public_sha256(target):
            raise ValueError("Destination bytes differ: " + target)


def migrate(store, apply=False):
    source = store.listing(SOURCE)
    before = store.listing(TARGET)
    if not source or any(SOURCE + name not in source for name in SAMPLES):
        raise ValueError("Source release tree is empty or missing latest metadata")
    expected = {destination(key) for key in source}
    # A new release published to the new prefix must not be erased on a rerun.
    unrelated = {key: value for key, value in before.items() if key not in expected}

    def transfer(key):
        target = destination(key)
        source_head = store.head(key)
        if identity(source_head) != source[key]:
            raise ValueError("Source changed after listing: " + key)
        policy = store.acl(key)
        copied = target not in before
        if not copied:
            verify_object(store, key, target, source_head, store.head(target))
        if apply:
            if copied:
                store.copy(key, target, source_head)
            # Also repairs ACLs if a prior run stopped between copy and ACL update.
            if acl_identity(store.acl(target)) != acl_identity(policy):
                store.set_acl(target, policy)
            verify_object(store, key, target, source_head, store.head(target))
            if acl_identity(store.acl(target)) != acl_identity(policy):
                raise ValueError("Destination ACL differs: " + target)
        return copied

    immutable = sorted(key for key in source if not key.startswith(SOURCE + "latest/"))
    latest = sorted((key for key in source if key.startswith(SOURCE + "latest/")),
                    key=lambda key: (key.endswith("/release.json"), key))
    with ThreadPoolExecutor(max_workers=4) as workers:
        copied = sum(workers.map(transfer, immutable))
    # Publish latest only after every immutable object is present.
    copied += sum(transfer(key) for key in latest)
    if store.listing(SOURCE) != source:
        raise ValueError("Source release tree changed during migration; rerun after publication finishes")
    samples = {}
    if apply:
        after = store.listing(TARGET)
        if set(after) != expected | set(unrelated) or any(after[key] != value for key, value in unrelated.items()):
            raise ValueError("Destination listing is missing files or changed unexpectedly")
        for key, (size, _) in source.items():
            if after[destination(key)][0] != size:
                raise ValueError("Destination listing size differs: " + destination(key))
        for name in SAMPLES:
            checksum = store.public_sha256(SOURCE + name)
            if store.public_sha256(TARGET + name) != checksum:
                raise ValueError("Public download differs: " + name)
            samples[name] = checksum
    report = {"source": SOURCE, "destination": TARGET, "applied": apply, "objects": len(source),
              "bytes": sum(size for size, _ in source.values()), "copied" if apply else "wouldCopy": copied,
              "alreadyPresent": len(source) - copied, "sourceRetained": True, "publicSampleSHA256": samples}
    print(json.dumps(report, indent=2))
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--apply", action="store_true", help="Copy and verify; the default is a read-only plan")
    parser.add_argument("--evidence", type=Path)
    arguments = parser.parse_args()
    if not os.environ.get("AWS_ACCESS_KEY_ID") or not os.environ.get("AWS_SECRET_ACCESS_KEY"):
        parser.error("AWS credential environment variables are required")
    with tempfile.TemporaryDirectory(prefix="oss-prefix-migration-") as temporary:
        directory = Path(temporary)
        config = directory / "aws-config"
        config.write_text("[default]\nregion = cn-north-1\ns3 =\n    signature_version = s3v4\n    addressing_style = virtual\n    payload_signing_enabled = true\n")
        environment = dict(os.environ, AWS_CONFIG_FILE=str(config), AWS_SHARED_CREDENTIALS_FILE=str(directory / "unused-credentials"),
                           AWS_PROFILE="default", AWS_DEFAULT_PROFILE="default", AWS_EC2_METADATA_DISABLED="true", AWS_PAGER="",
                           AWS_CLI_AUTO_PROMPT="off", AWS_REQUEST_CHECKSUM_CALCULATION="when_required", AWS_RESPONSE_CHECKSUM_VALIDATION="when_required")
        environment.pop("AWS_SESSION_TOKEN", None)
        environment.pop("AWS_SECURITY_TOKEN", None)
        report = migrate(OSS(environment), arguments.apply)
        if arguments.evidence:
            arguments.evidence.parent.mkdir(parents=True, exist_ok=True)
            arguments.evidence.write_text(json.dumps(report, indent=2) + "\n")
