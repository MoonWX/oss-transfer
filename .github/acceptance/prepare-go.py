#!/usr/bin/env python3
"""Verify packaged binaries and install the pinned S3 fixture for native acceptance."""
import argparse
import hashlib
import importlib.util
import os
from pathlib import Path
import shutil
import struct
import tarfile
from urllib.request import urlopen
import zipfile

SPEC = importlib.util.spec_from_file_location("release_platforms", Path(__file__).resolve().parents[2] / "deploy/go/platforms.py")
TARGETS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TARGETS)

WEED = {
    "freebsd_amd64.tar.gz": "30cbba8347e333ae42ff494c0121c09e0a4a8c2bd77b4c8593ed76dc7b282bb2",
    "freebsd_arm64.tar.gz": "670e896ec2ad4cca8223b582bed186b3df2cb2cbcdfb008a5950dffe0ca5044b",
    "darwin_amd64.tar.gz": "425ef61d2ee773d5d5175003f38bd6cc891070ae6319370354aaf5378c45d5b3",
    "darwin_arm64.tar.gz": "faa194e72a346755dc4404288b0561eb3c260351702901d8363024d5099bdd7d",
    "linux_amd64.tar.gz": "4a7d108384d044d95212d1342cdda9533fa55842c1c9b41f606ca3c8a9561124",
    "linux_arm64.tar.gz": "557e92c38c5c7d180748eb8e2b2b45e426f8e5282a4caca8e098a90d09f03a07",
    "windows_amd64.zip": "fe90c04c0620ad1a1c756f86cd5e1443773f56a446688077a4bb5ec04c3cc874",
}


def digest(path):
    with path.open("rb") as stream:
        result = hashlib.sha256()
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            result.update(block)
        return result.hexdigest()


def copy_member(archive, name, destination):
    if zipfile.is_zipfile(archive):
        with zipfile.ZipFile(archive) as package:
            with package.open(name) as source, destination.open("wb") as target:
                shutil.copyfileobj(source, target)
    else:
        with tarfile.open(archive, "r:gz") as package:
            member = package.getmember(name)
            if not member.isfile():
                raise ValueError("Package member is not a regular file: " + name)
            with package.extractfile(member) as source, destination.open("wb") as target:
                shutil.copyfileobj(source, target)


def verify_binary(path, operating_system, architecture):
    # Windows ARM and Apple Silicon can execute x64 programs through emulation.
    # A successful startup alone cannot prove the package contains native code.
    with path.open("rb") as stream:
        header = stream.read(64)
        if operating_system == "windows":
            if header[:2] != b"MZ":
                raise ValueError("Expected a Windows PE executable: " + str(path))
            stream.seek(struct.unpack_from("<I", header, 60)[0])
            pe = stream.read(6)
            expected = 0xAA64 if architecture == "arm64" else 0x8664
            valid = pe[:4] == b"PE\0\0" and struct.unpack_from("<H", pe, 4)[0] == expected
        elif operating_system in ("linux", "freebsd"):
            expected = {"amd64": 62, "arm64": 183, "armv7": 40, "riscv64": 243, "loong64": 258}[architecture]
            elf_class = 1 if architecture == "armv7" else 2
            valid = (header[:6] == b"\x7fELF" + bytes((elf_class, 1))
                     and struct.unpack_from("<H", header, 18)[0] == expected
                     and header[7] in ((9,) if operating_system == "freebsd" else (0, 3)))
        else:
            expected = 0x0100000C if architecture == "arm64" else 0x01000007
            valid = header[:4] == b"\xcf\xfa\xed\xfe" and struct.unpack_from("<I", header, 4)[0] == expected
    if not valid:
        raise ValueError("Executable architecture differs from package platform: " + str(path))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--platform", required=True, choices=[o + "/" + a for o, a in TARGETS.PLATFORMS])
    parser.add_argument("--fixture-platform", help="S3 fixture platform; application binaries always use --platform")
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--packages", type=Path, default=Path("output/go-development"))
    args = parser.parse_args()
    operating_system, architecture = args.platform.split("/")
    extension = ".exe" if operating_system == "windows" else ""
    name = "oss-transfer-" + args.platform.replace("/", "-")
    archive = args.packages / (name + (".zip" if extension else ".tar.gz"))
    expected = Path(str(archive) + ".sha256").read_text(encoding="utf-8").split()[0]
    if digest(archive) != expected:
        raise ValueError("Release archive checksum mismatch")
    directory = args.directory.resolve()
    directory.mkdir(parents=True, exist_ok=False)
    names = ["oss-transfer-" + program + extension for program in ("master", "agent", "cli")]
    for member in names + ["VERSION", "SHA256SUMS"]:
        copy_member(archive, name + "/" + member, directory / member)
    checked = set()
    for row in (directory / "SHA256SUMS").read_text(encoding="utf-8").splitlines():
        checksum, member = row.split(None, 1)
        if member in checked or member not in names + ["VERSION"] or digest(directory / member) != checksum:
            raise ValueError("Binary or VERSION checksum mismatch")
        checked.add(member)
    if checked != set(names + ["VERSION"]):
        raise ValueError("Package checksum manifest is incomplete")
    version = dict(row.split(": ", 1) for row in (directory / "VERSION").read_text(encoding="utf-8").splitlines())
    if version["Platform"] != args.platform:
        raise ValueError("Packaged platform mismatch")
    if os.environ.get("GITHUB_SHA") and version["FullCommit"] != os.environ["GITHUB_SHA"]:
        raise ValueError("Package was not built from this commit")
    for member in names:
        verify_binary(directory / member, operating_system, architecture)
        (directory / member).chmod(0o755)
    # The fixture has no Windows ARM64 release. Windows ARM runs its x64 S3
    # fixture under OS emulation; both tested transfer programs remain native ARM64.
    fixture_os, fixture_arch = (args.fixture_platform or args.platform).split("/")
    weed_asset = "windows_amd64.zip" if fixture_os == "windows" else f"{fixture_os}_{fixture_arch}.tar.gz"
    weed_archive = directory / weed_asset
    with urlopen("https://github.com/seaweedfs/seaweedfs/releases/download/4.48/" + weed_asset, timeout=120) as source, weed_archive.open("wb") as target:
        shutil.copyfileobj(source, target)
    if digest(weed_archive) != WEED[weed_asset]:
        raise ValueError("SeaweedFS fixture checksum mismatch")
    weed = directory / ("weed" + extension)
    copy_member(weed_archive, weed.name, weed)
    weed.chmod(0o755)
    values = {"GO_QA_" + program.upper(): str(directory / member) for program, member in zip(("master", "agent", "cli"), names)}
    values["GO_QA_WEED"] = str(weed)
    if os.environ.get("GITHUB_ENV"):
        with open(os.environ["GITHUB_ENV"], "a", encoding="utf-8") as stream:
            for key, value in values.items():
                stream.write(key + "=" + value + "\n")
    print("Verified native package for " + args.platform)


if __name__ == "__main__":
    main()
