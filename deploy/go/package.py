#!/usr/bin/env python3
"""Assemble verified platform archives and the files used by the installer."""
import argparse
import hashlib
import importlib.util
import pathlib
import re
import shutil
import stat
import tarfile
import tempfile
import zipfile

INSTALLERS = {
    'install.sh': 'install.sh',
    'native-install.sh': 'deploy/go/install.sh',
    'Install.ps1': 'deploy/go/Install.ps1',
}

SPEC = importlib.util.spec_from_file_location('native_platforms', pathlib.Path(__file__).with_name('platforms.py'))
TARGETS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TARGETS)
PLATFORMS = TARGETS.PLATFORMS

def members(path):
    result = {}
    seen = set()
    def remember(name):
        if name in seen:
            raise ValueError('package contains a duplicate member: ' + name)
        seen.add(name)
    if path.suffix == '.zip':
        with zipfile.ZipFile(path) as archive:
            for item in archive.infolist():
                remember(item.filename)
                if item.is_dir():
                    continue
                mode = item.external_attr >> 16
                if stat.S_IFMT(mode) not in (0, stat.S_IFREG):
                    raise ValueError('package contains a non-regular file')
                result[item.filename] = archive.read(item)
        return result
    with tarfile.open(path, 'r:gz') as archive:
        for item in archive:
            remember(item.name)
            if item.isdir():
                continue
            if not item.isfile():
                raise ValueError('package contains a non-regular file')
            result[item.name] = archive.extractfile(item).read()
        return result

def package(source, destination, scripts, require_all=True, unpack_binaries=False):
    if destination.exists():
        raise ValueError('release output already exists; use a new directory')
    destination.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='.go-release-', dir=destination.parent) as temporary:
        stage = pathlib.Path(temporary) / 'release'
        stage.mkdir()
        rows, identity, found = [], None, 0
        for os_name, arch in PLATFORMS:
            name = f'oss-transfer-{os_name}-{arch}'
            path = source / TARGETS.archive_name(os_name, arch)
            if not path.exists():
                if require_all:
                    raise ValueError(f'missing platform package: {path.name}')
                continue
            # Read checksums, members and size from the same copy even if a
            # concurrent local build replaces the source archive.
            staged_archive = stage/path.name
            shutil.copyfile(path, staged_archive)
            contents = members(staged_archive)
            prefix = name + '/'
            extension = '.exe' if os_name == 'windows' else ''
            required = {'oss-transfer-' + role + extension for role in ('master', 'agent', 'cli')} | {'VERSION'}
            expected = {}
            for line in contents[prefix+'SHA256SUMS'].decode().splitlines():
                checksum, file = line.split(maxsplit=1)
                file = file.removeprefix('*')
                if file in expected or file not in required:
                    raise ValueError('duplicate or unexpected package checksum entry')
                expected[file] = checksum
            if set(expected) != required:
                raise ValueError('package checksum manifest is incomplete')
            for file, checksum in expected.items():
                if not re.fullmatch(r'[a-f0-9]{64}', checksum) or '/' in file or '\\' in file:
                    raise ValueError('invalid package checksum entry')
                if hashlib.sha256(contents[prefix+file]).hexdigest() != checksum:
                    raise ValueError(f'package checksum mismatch: {path.name}/{file}')
            metadata = {}
            for line in contents[prefix+'VERSION'].decode().splitlines():
                key, value = line.split(': ', 1)
                if key in metadata:
                    raise ValueError('duplicate package VERSION field')
                metadata[key] = value
            current = (metadata['Version'], metadata['FullCommit'])
            if not all(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.+_-]{0,127}', part) for part in current):
                raise ValueError('invalid release identity')
            if identity is not None and identity != current:
                raise ValueError('platform packages have different version or commit')
            identity = current
            if metadata['Platform'] != f'{os_name}/{arch}':
                raise ValueError('package platform mismatch')
            for role in ('master', 'agent', 'cli'):
                extension = '.exe' if os_name == 'windows' else ''
                original = f'oss-transfer-{role}{extension}'
                if original not in expected:
                    raise ValueError('binary missing from internal checksum manifest')
                payload = contents[prefix+original]
                filename = f'oss-transfer-{role}-{os_name}-{arch}{extension}'
                if unpack_binaries:
                    (stage/filename).write_bytes(payload)
                    (stage/filename).chmod(0o755)
                rows.append('\t'.join(('file', os_name, arch, role, filename, hashlib.sha256(payload).hexdigest(), str(len(payload)))))
            archive_digest = hashlib.sha256(staged_archive.read_bytes()).hexdigest()
            rows.append('\t'.join(('archive', os_name, arch, path.name, archive_digest, str(staged_archive.stat().st_size))))
            found += 1
        if not found:
            raise ValueError('no native packages found')
        for name, source_name in INSTALLERS.items():
            payload = (scripts/source_name).read_bytes()
            (stage/name).write_bytes(payload)
            rows.append('\t'.join(('asset',name,hashlib.sha256(payload).hexdigest(),str(len(payload)))))
        (stage/'manifest.tsv').write_text('\n'.join(['format\toss-transfer-go-v1', 'version\t'+identity[0], 'commit\t'+identity[1]]+rows)+'\n', encoding="utf-8")
        stage.rename(destination)
    print(destination)

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--packages', required=True, type=pathlib.Path)
    parser.add_argument('--output', required=True, type=pathlib.Path)
    parser.add_argument('--allow-partial', action='store_true', help='development lifecycle tests only')
    args = parser.parse_args()
    package(args.packages.resolve(), args.output.resolve(), pathlib.Path(__file__).resolve().parents[2], not args.allow_partial)
