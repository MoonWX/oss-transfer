#!/usr/bin/env python3
"""Create a two-platform Docker context from checksummed native archives."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("go_package", Path(__file__).with_name("package.py"))
PACKAGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PACKAGE)


def sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def create(source, output):
    if output.exists() or output.is_symlink():
        raise ValueError("Docker context already exists; choose a new output directory")
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".docker-", dir=output.parent) as temporary:
        root = Path(temporary)
        snapshot = root / "packages"
        snapshot.mkdir()
        archives = []
        # A local build may replace source archives while this command runs.
        # Both provenance and extracted payloads must come from the same copy.
        for os_name, arch in PACKAGE.PLATFORMS:
            name = f"oss-transfer-{os_name}-{arch}" + (".zip" if os_name == "windows" else ".tar.gz")
            for relative in (name, name + ".sha256"):
                original = source / relative
                if original.is_symlink() or not original.is_file():
                    raise ValueError("Missing regular native package file: " + relative)
                shutil.copyfile(original, snapshot / relative)
            archive = snapshot / name
            check = (snapshot / (name + ".sha256")).read_text(encoding="utf-8").split()
            actual = sha256(archive)
            if len(check) != 2 or check[1].lstrip("*") != name or check[0] != actual:
                raise ValueError("archive checksum mismatch: " + name)
            archives.append({"name": name, "sha256": actual})
        release = root / "release"
        PACKAGE.package(snapshot, release, ROOT, require_all=True, unpack_binaries=True)
        lines = [line.split("\t") for line in (release / "manifest.tsv").read_text(encoding="utf-8").splitlines()]
        identity = {row[0]: row[1] for row in lines if row[0] in ("version", "commit")}
        if not re.fullmatch(r"[a-f0-9]{40}", identity["commit"]):
            raise ValueError("Docker context requires a clean full commit SHA")
        dockerfile = subprocess.check_output(["git", "show", identity["commit"] + ":Dockerfile"], cwd=ROOT)
        stage = root / "context"
        (stage / "data").mkdir(parents=True)
        (stage / "data" / ".keep").write_bytes(b"")
        binaries = []
        for row in lines:
            if row[0] != "file" or row[1] != "linux" or row[2] not in ("amd64", "arm64"):
                continue
            _, os_name, arch, role, filename, checksum, size = row
            directory = stage / "bin" / os_name / arch
            directory.mkdir(parents=True, exist_ok=True)
            shutil.copy2(release / filename, directory / ("oss-transfer-" + role))
            binaries.append({"platform": os_name + "/" + arch, "program": role, "sha256": checksum, "bytes": int(size)})
        if len(binaries) != 6:
            raise ValueError("both Linux architectures must contain Master, Agent and CLI")
        (stage / "provenance.json").write_text(json.dumps(dict(identity, archives=archives, binaries=binaries), indent=2) + "\n", encoding="utf-8")
        (stage / "Dockerfile").write_bytes(dockerfile)
        stage.rename(output)
    print("Docker context: " + str(output))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--packages", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    create(args.packages.resolve(), args.output.resolve())
