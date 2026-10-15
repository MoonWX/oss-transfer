#!/usr/bin/env python3
"""Assemble release downloads from the tested native archives."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

SPEC = importlib.util.spec_from_file_location("native_package", Path(__file__).with_name("package.py"))
PACKAGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PACKAGE)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def release_sources(destination, commit):
    """Use the scripts from the binary commit, regardless of working-tree edits."""
    repository = Path(__file__).resolve().parents[2]
    destination.mkdir()
    for name in PACKAGE.INSTALLERS.values():
        try:
            result = subprocess.run(
                ["git", "show", commit + ":" + name], cwd=repository,
                check=True, capture_output=True,
            )
        except subprocess.CalledProcessError:
            if not name.startswith("deploy/native/"):
                raise
            # Older archives still point to commits before the directory rename.
            # Keep both lookups pinned to the archive's commit.
            legacy_name = name.replace("deploy/native/", "deploy/go/", 1)
            result = subprocess.run(
                ["git", "show", commit + ":" + legacy_name], cwd=repository,
                check=True, capture_output=True,
            )
        (destination / name).parent.mkdir(parents=True, exist_ok=True)
        (destination / name).write_bytes(result.stdout)


def assemble(packages, output, version, commit):
    if not re.fullmatch(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?", version):
        raise ValueError("Expected a semantic release version without a v prefix")
    if "-" in version and any(re.fullmatch(r"0[0-9]+", item) for item in version.split("-", 1)[1].split(".")):
        raise ValueError("Numeric prerelease identifiers cannot have leading zeroes")
    if not re.fullmatch(r"[a-f0-9]{40}", commit):
        raise ValueError("Expected a full clean commit SHA")
    if output.exists():
        raise ValueError("Release output already exists")
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".release-assets-", dir=output.parent) as work:
        root = Path(work)
        scripts = root / "scripts"
        release_sources(scripts, commit)
        snapshot = root / "packages"
        snapshot.mkdir()
        for os_name, arch in PACKAGE.PLATFORMS:
            name = PACKAGE.TARGETS.archive_name(os_name, arch)
            for relative in (name, name + ".sha256"):
                source = packages / relative
                if not source.is_file() or source.is_symlink():
                    raise ValueError("Missing regular release file: " + relative)
                shutil.copyfile(source, snapshot / relative)
            digest, *names = (snapshot / (name + ".sha256")).read_text(encoding="utf-8").split()
            if names != [name] or digest != sha256(snapshot / name):
                raise ValueError("Archive checksum mismatch: " + name)
        stage = root / "assets"
        PACKAGE.package(snapshot, stage, scripts)
        manifest = (stage / "manifest.tsv").read_text(encoding="utf-8").splitlines()
        identity = {row.split("\t")[0]: row.split("\t")[1] for row in manifest if row.startswith(("version\t", "commit\t"))}
        if identity != {"version": version, "commit": commit}:
            raise ValueError("Native packages differ from the release version or commit")
        assets = [{"name": p.name, "size": p.stat().st_size, "sha256": sha256(p)} for p in sorted(stage.iterdir())]
        release = {"format": "oss-transfer-release-v1", "version": version, "tag": "v" + version,
                   "commit": commit, "assets": assets}
        (stage / "release.json").write_text(json.dumps(release, indent=2) + "\n", encoding="utf-8")
        (stage / "SHA256SUMS").write_text("".join(sha256(p) + "  " + p.name + "\n" for p in sorted(stage.iterdir())), encoding="utf-8")
        stage.rename(output)
    print(f"Assembled {version} ({commit}): {output}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--packages", required=True, type=Path)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--commit", required=True)
    args = parser.parse_args()
    assemble(args.packages.resolve(), args.output.resolve(), args.version, args.commit)
