#!/usr/bin/env python3
"""Withdraw two selected versions after verifying their published replacement."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time
from urllib.error import HTTPError, URLError
from urllib.request import HTTPRedirectHandler, Request, build_opener, urlopen

ENDPOINT = "https://s3.cn-north-1.jdcloud-oss.com"
BUCKET = "moonwx"
OSS = "https://moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer"
GITHUB = "https://api.github.com/repos/MoonWX/oss-transfer"
REGISTRY = "https://registry-1.docker.io/v2/moonwx/osstransfer"
HUB = "https://hub.docker.com"
LATEST = ("release.json", "manifest.tsv", "install.sh", "Install.ps1")
LEGACY_LATEST = ("VERSION", "docker-compose.yml")
SEMVER = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?\Z")
NAME = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}\Z")
SHA = re.compile(r"[a-f0-9]{64}\Z")
INDEX = ("application/vnd.oci.image.index.v1+json", "application/vnd.docker.distribution.manifest.list.v2+json")
MANIFEST = ("application/vnd.oci.image.manifest.v1+json", "application/vnd.docker.distribution.manifest.v2+json")


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def plan(replacement, retired):
    versions = retired.split(",")
    require(len(versions) == len(set(versions)) == 2, "Provide exactly two distinct comma-separated versions")
    for version in [replacement] + versions:
        match = SEMVER.fullmatch(version)
        require(match and len(version) <= 96, "Invalid semantic version")
        require(not match[4] or not any(re.fullmatch(r"0[0-9]+", part) for part in match[4].split(".")), "Invalid prerelease number")
    require("-" not in replacement, "Replacement must be a stable release")
    stable = [version for version in versions if "-" not in version]
    require(len(stable) == 1, "Select one stable version and one of its prereleases")
    stable = stable[0]
    require(all(version == stable or version.startswith(stable + "-") for version in versions), "Retired versions must have the same base version")
    alias = ".".join(stable.split(".")[:2])
    tags = versions + [alias]
    protected = {replacement, ".".join(replacement.split(".")[:2]), "latest"}
    require(not protected.intersection(tags), "Withdrawal overlaps the replacement or its aliases")
    return {"replacement": replacement, "versions": versions,
            "prefixes": ["osstransfer/v" + version + "/" for version in versions], "tags": tags}


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def request(url, method="GET", headers=None, payload=None, missing=False, private=False):
    # Credentials stay in memory. Never log request bodies, headers or error bodies.
    request_headers = {"User-Agent": "oss-transfer-release-withdrawal", "Cache-Control": "no-cache"}
    request_headers.update(headers or {})
    if payload is not None:
        request_headers["Content-Type"] = "application/json"
    opener = build_opener(NoRedirect()).open if private else urlopen
    for attempt in range(3):
        try:
            with opener(Request(url, data=payload, headers=request_headers, method=method), timeout=45) as response:
                body = b"" if method == "HEAD" else response.read(2 * 1024 * 1024 + 1)
                require(len(body) <= 2 * 1024 * 1024, "HTTP response exceeds size limit")
                return response.status, dict(response.headers.items()), body
        except HTTPError as error:
            if missing and error.code == 404:
                return 404, {}, b""
            if method in ("GET", "HEAD") and error.code in (429, 500, 502, 503, 504) and attempt < 2:
                time.sleep(attempt + 1)
                continue
            hint = "; Docker PAT needs Repo Read, Write, Delete permission" if private and error.code == 403 else ""
            raise RuntimeError(f"{method} {url.split('?', 1)[0]} returned HTTP {error.code}{hint}") from None
        except (URLError, TimeoutError):
            if method in ("GET", "HEAD") and attempt < 2:
                time.sleep(attempt + 1)
                continue
            raise RuntimeError(f"{method} {url.split('?', 1)[0]} did not complete; rerun to reconcile") from None


def read_json(url, headers=None, private=False):
    status, response_headers, body = request(url, headers=headers, private=private)
    require(status == 200, "Expected HTTP 200")
    return json.loads(body), response_headers, body


def header(headers, name):
    return next((value for key, value in headers.items() if key.lower() == name.lower()), None)


def verify_github(version):
    release, _, _ = read_json(GITHUB + "/releases/tags/v" + version)
    latest, _, _ = read_json(GITHUB + "/releases/latest")
    require(release.get("tag_name") == "v" + version and release.get("draft") is False and release.get("prerelease") is False,
            "Replacement GitHub release is not public and stable")
    require(latest.get("id") == release.get("id"), "GitHub latest is not the replacement")
    ref, _, _ = read_json(GITHUB + "/git/ref/tags/v" + version)
    obj = ref["object"]
    for _ in range(3):
        require(re.fullmatch(r"[a-f0-9]{40}", obj.get("sha", "")), "Invalid GitHub commit identity")
        if obj["type"] == "commit":
            return obj["sha"]
        require(obj["type"] == "tag", "Unexpected Git tag object")
        ref, _, _ = read_json(GITHUB + "/git/tags/" + obj["sha"])
        obj = ref["object"]
    raise RuntimeError("Cannot resolve replacement GitHub tag")


def verify_oss(version, commit):
    base = OSS + "/v" + version + "/"
    release, _, body = read_json(base + "release.json")
    require((release.get("format"), release.get("version"), release.get("tag"), release.get("commit")) ==
            ("oss-transfer-release-v1", version, "v" + version, commit), "Replacement OSS identity differs from GitHub")
    rows = release["assets"]
    require(isinstance(rows, list) and 6 <= len(rows) <= 100, "Invalid replacement asset count")
    records = {}
    for row in rows:
        name = row["name"]
        require(NAME.fullmatch(name) and name not in records and SHA.fullmatch(row["sha256"]) and
                type(row["size"]) is int and row["size"] > 0, "Invalid replacement asset")
        records[name] = row
    require(set(LATEST) - {"release.json"} <= set(records), "Replacement installer metadata is missing")

    def check_asset(row):
        status, headers, _ = request(base + row["name"], method="HEAD")
        require(status == 200 and int(header(headers, "Content-Length") or -1) == row["size"],
                "Replacement asset missing or wrong size: " + row["name"])

    with ThreadPoolExecutor(max_workers=4) as pool:
        list(pool.map(check_asset, rows))
    # Before archive-based installation, releases also published VERSION and
    # Compose at latest. Keep those releases verifiable without requiring the
    # retired files from the current compact layout.
    legacy = set(LEGACY_LATEST).intersection(records)
    require(not legacy or legacy == set(LEGACY_LATEST), "Replacement legacy metadata is incomplete")
    latest_names = LATEST + (LEGACY_LATEST if legacy else ())
    metadata = {"release.json": body}
    for name in latest_names:
        if name not in metadata:
            _, _, metadata[name] = request(base + name)
            require(len(metadata[name]) == records[name]["size"] and sha(metadata[name]) == records[name]["sha256"],
                    "Replacement metadata checksum mismatch: " + name)
        _, _, latest = request(OSS + "/latest/" + name)
        require(latest == metadata[name], "OSS latest differs from replacement: " + name)
    if legacy:
        require(metadata["VERSION"].decode() == f"Version: {version}\nFullCommit: {commit}\n", "Replacement VERSION differs")
    fields = [line.split("\t") for line in metadata["manifest.tsv"].decode().splitlines()]
    for key, value in (("format", "oss-transfer-go-v1"), ("version", version), ("commit", commit)):
        require([row for row in fields if row[0] == key] == [[key, value]], "Replacement manifest identity differs")
    if not legacy:
        archives = [row for row in fields if row[0] == "archive"]
        require(archives, "Replacement archive manifest is missing")
        names, platforms = set(), set()
        for row in archives:
            require(len(row) == 6, "Invalid replacement archive record")
            _, operating_system, architecture, name, checksum, size = row
            platform = (operating_system, architecture)
            require(name not in names and platform not in platforms and name in records and
                    checksum == records[name]["sha256"] and size == str(records[name]["size"]),
                    "Replacement archive manifest differs")
            names.add(name)
            platforms.add(platform)
        require(set(records) == names | {"install.sh", "native-install.sh", "Install.ps1", "manifest.tsv"},
                "Replacement compact release contains unexpected files")
    return {name: sha(data) for name, data in metadata.items()}


def verify_docker(version, commit):
    login, _, _ = read_json("https://auth.docker.io/token?service=registry.docker.io&scope=repository:moonwx/osstransfer:pull")
    token = login.get("token") or login.get("access_token")
    require(isinstance(token, str) and token, "Docker anonymous pull token unavailable")
    headers = {"Authorization": "Bearer " + token, "Accept": ", ".join(INDEX + MANIFEST)}
    digests = []
    for tag in (version, ".".join(version.split(".")[:2]), "latest"):
        index, response_headers, body = read_json(REGISTRY + "/manifests/" + tag, headers)
        checksum = "sha256:" + sha(body)
        require(header(response_headers, "Docker-Content-Digest") == checksum and index.get("mediaType") in INDEX,
                "Replacement Docker index differs")
        digests.append(checksum)
    require(len(set(digests)) == 1, "Docker latest and replacement aliases differ")
    require(sorted((row["platform"]["os"], row["platform"]["architecture"]) for row in index["manifests"]) ==
            [("linux", "amd64"), ("linux", "arm64")], "Replacement Docker platforms are incomplete")
    for child in index["manifests"]:
        require(re.fullmatch(r"sha256:[a-f0-9]{64}", child["digest"]), "Invalid child image digest")
        manifest, _, body = read_json(REGISTRY + "/manifests/" + child["digest"], headers)
        require("sha256:" + sha(body) == child["digest"] and len(body) == child["size"] and manifest.get("mediaType") in MANIFEST,
                "Replacement child image digest differs")
        config = manifest["config"]
        require(re.fullmatch(r"sha256:[a-f0-9]{64}", config["digest"]), "Invalid image config digest")
        data, _, body = read_json(REGISTRY + "/blobs/" + config["digest"], headers)
        require("sha256:" + sha(body) == config["digest"] and len(body) == config["size"], "Replacement image config checksum differs")
        labels = data.get("config", {}).get("Labels", {})
        require((data.get("os"), data.get("architecture"), labels.get("org.opencontainers.image.version"),
                 labels.get("org.opencontainers.image.revision")) == ("linux", child["platform"]["architecture"], version, commit),
                "Replacement Docker image version/commit/platform differs")
    return digests[0]


def verify_replacement(version):
    commit = verify_github(version)
    metadata = verify_oss(version, commit)
    digest = verify_docker(version, commit)
    return {"version": version, "commit": commit, "metadataSHA256": metadata, "dockerDigest": digest}


class ObjectStore:
    def __init__(self, directory, prefixes):
        self.prefixes = prefixes
        self.directory = directory
        config = directory / "aws-config"
        config.write_text("[default]\nregion = cn-north-1\ns3 =\n    signature_version = s3v4\n    addressing_style = virtual\n    payload_signing_enabled = true\n")
        self.env = dict(os.environ, AWS_CONFIG_FILE=str(config), AWS_SHARED_CREDENTIALS_FILE=str(directory / "unused"),
                        AWS_PROFILE="default", AWS_DEFAULT_PROFILE="default", AWS_EC2_METADATA_DISABLED="true", AWS_PAGER="",
                        AWS_CLI_AUTO_PROMPT="off", AWS_REQUEST_CHECKSUM_CALCULATION="when_required", AWS_RESPONSE_CHECKSUM_VALIDATION="when_required")
        self.env.pop("AWS_SESSION_TOKEN", None)
        self.env.pop("AWS_SECURITY_TOKEN", None)

    def call(self, operation, *args):
        result = subprocess.run(["aws", "--endpoint-url", ENDPOINT, "--region", "cn-north-1", "--cli-connect-timeout", "15",
                                 "--cli-read-timeout", "120", "--no-paginate", "--output", "json", "s3api", operation,
                                 "--bucket", BUCKET, *args], env=self.env, capture_output=True, timeout=180)
        require(result.returncode == 0, f"OSS {operation} failed (exit {result.returncode}); credentials/permissions or connectivity require checking")
        require(len(result.stdout) <= 2 * 1024 * 1024, "OSS response exceeds size limit")
        return json.loads(result.stdout or b"{}")

    def list(self, prefix):
        require(prefix in self.prefixes, "OSS prefix is outside the withdrawal plan")
        keys, token = [], None
        for _ in range(4):
            args = ["--prefix", prefix, "--max-keys", "1000"]
            if token:
                args += ["--continuation-token", token]
            page = self.call("list-objects-v2", *args)
            for row in page.get("Contents", []):
                key = row["Key"]
                require(isinstance(key, str) and key.startswith(prefix) and key not in keys, "OSS returned an out-of-scope or duplicate key")
                keys.append(key)
            require(len(keys) <= 2000, "Withdrawal exceeds 2000 objects per version")
            if not page.get("IsTruncated", False):
                return keys
            previous, token = token, page.get("NextContinuationToken")
            require(isinstance(token, str) and token and token != previous, "OSS pagination did not advance")
        raise RuntimeError("OSS pagination exceeded the withdrawal bound")

    def delete(self, prefix, keys):
        require(prefix in self.prefixes and keys and len(keys) <= 1000 and len(keys) == len(set(keys)) and
                all(isinstance(key, str) and key.startswith(prefix) for key in keys), "OSS delete is outside the withdrawal plan")
        for key in keys:
            self.call("delete-object", "--key", key)


class DockerHub:
    def __init__(self, tags):
        # Current auth contract: https://docs.docker.com/reference/api/hub/latest/operations/AuthCreateAccessToken/
        # Exact tag deletion route: https://github.com/docker/hub-tool/blob/main/pkg/hub/tags.go
        # PAT permissions: https://docs.docker.com/security/access-tokens/reference/
        self.tags = tags
        payload = json.dumps({"identifier": os.environ["DOCKER_USERNAME"], "secret": os.environ["DOCKER_PAT"]}).encode()
        status, _, body = request(HUB + "/v2/auth/token", "POST", payload=payload, private=True)
        require(status == 200, "Docker authentication failed")
        token = json.loads(body).get("access_token")
        require(isinstance(token, str) and token, "Docker bearer token unavailable")
        self.headers = {"Authorization": "Bearer " + token}

    def url(self, tag):
        require(tag in self.tags, "Docker tag is outside the withdrawal plan")
        return HUB + "/v2/repositories/moonwx/osstransfer/tags/" + tag + "/"

    def exists(self, tag):
        status, _, body = request(self.url(tag), headers=self.headers, missing=True, private=True)
        if status == 404:
            return False
        require(status == 200 and json.loads(body).get("name") == tag, "Docker returned a different tag")
        return True

    def delete(self, tag):
        status, _, _ = request(self.url(tag), "DELETE", headers=self.headers, missing=True, private=True)
        require(status in (202, 204, 404), "Docker did not acknowledge tag deletion")
        for _ in range(10):
            if not self.exists(tag):
                return
            time.sleep(2)
        raise RuntimeError("Docker tag remains visible after deletion")


def withdraw(selection, store, hub, execute, evidence, save, verify=verify_replacement):
    evidence["replacement"] = verify(selection["replacement"])
    objects = {prefix: store.list(prefix) for prefix in selection["prefixes"]}
    tags = {tag: hub.exists(tag) for tag in selection["tags"]}
    evidence.update(objects=objects, dockerTags=tags, deletedObjects=[], deletedTags=[])
    save()
    if not execute:
        evidence["status"] = "DRY_RUN_PASSED"
        return
    require(verify(selection["replacement"]) == evidence["replacement"], "Replacement changed during preflight; nothing deleted")
    for prefix, keys in objects.items():
        for offset in range(0, len(keys), 1000):
            batch = keys[offset:offset + 1000]
            store.delete(prefix, batch)
            evidence["deletedObjects"].extend(batch)
            save()
        require(not store.list(prefix), "OSS version prefix is still populated")
    for tag, exists in tags.items():
        if exists:
            hub.delete(tag)
            evidence["deletedTags"].append(tag)
            save()
    require(not any(hub.exists(tag) for tag in selection["tags"]), "A retired Docker tag remains")
    require(verify(selection["replacement"]) == evidence["replacement"], "Replacement changed after withdrawal; inspect publishing activity")
    evidence["status"] = "WITHDRAWN"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--replacement-version", required=True)
    parser.add_argument("--retired-versions", required=True)
    parser.add_argument("--execute", action="store_true", help="Delete after preflight; omitted means a read-only plan")
    parser.add_argument("--evidence", type=Path, required=True)
    args = parser.parse_args()
    evidence = {"status": "RUNNING", "execute": args.execute, "startedUnix": time.time()}
    args.evidence.parent.mkdir(parents=True, exist_ok=True)

    def save():
        args.evidence.write_text(json.dumps(evidence, indent=2) + "\n")

    try:
        selection = plan(args.replacement_version, args.retired_versions)
        evidence["plan"] = selection
        save()
        for name in ("AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "DOCKER_USERNAME", "DOCKER_PAT"):
            require(bool(os.environ.get(name)), "Missing credential environment variable: " + name)
        # Public replacement checks precede even authenticated listing/login.
        verify_replacement(selection["replacement"])
        with tempfile.TemporaryDirectory(prefix="oss-withdrawal-") as work:
            withdraw(selection, ObjectStore(Path(work), selection["prefixes"]), DockerHub(selection["tags"]), args.execute, evidence, save)
        print(evidence["status"] + ": replacement verified; see withdrawal evidence")
    except Exception as error:
        evidence["status"] = "FAILED"
        # Only our own errors are safe to print; unexpected decoder/subprocess
        # exceptions can contain response data or credential-bearing arguments.
        evidence["error"] = str(error) if type(error) is RuntimeError else type(error).__name__ + ": withdrawal failed; inspect network/metadata"
        raise SystemExit(evidence["error"]) from None
    finally:
        evidence["finishedUnix"] = time.time()
        save()


if __name__ == "__main__":
    main()
