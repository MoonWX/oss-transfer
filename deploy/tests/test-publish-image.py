#!/usr/bin/env python3
"""Offline image publication regressions with real Git, checksums and JSON."""
import hashlib
import json
import os
import re
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parents[1]
STUB = r'''#!/usr/bin/env python3
import hashlib, json, os, pathlib, sys
args = sys.argv[1:]
def option(name, default=None):
    return args[args.index(name) + 1] if name in args else default
command = next(arg for arg in args if arg in ('login', 'copy', 'inspect', 'manifest-digest'))
log = pathlib.Path(os.environ['IMAGE_STUB_LOG'])
calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
record = {'command': command, 'args': args}
assert '--debug' not in args and '--tls-verify=false' not in args
assert '--tmpdir' in args
assert pathlib.Path(option('--tmpdir')).exists()
manifest = {'schemaVersion': 2, 'mediaType': 'application/vnd.docker.distribution.manifest.list.v2+json',
            'manifests': [{'platform': {'os': 'linux', 'architecture': arch}} for arch in ('amd64', 'arm64')]}
if os.environ.get('IMAGE_STUB_BAD_MANIFEST'):
    manifest['manifests'].append({'platform': {'os': 'unknown', 'architecture': 'unknown'}})
manifest_bytes = json.dumps(manifest).encode()
if command == 'login':
    secret = sys.stdin.read()
    assert secret == 'offline-registry-secret'
    assert '--password-stdin' in args and secret not in args
    auth = pathlib.Path(option('--authfile'))
    auth.write_text(json.dumps({'auths': {'offline': {'auth': secret}}}))
    record['authfile'] = str(auth)
elif command == 'manifest-digest':
    print('sha256:' + hashlib.sha256(pathlib.Path(args[-1]).read_bytes()).hexdigest())
elif command == 'inspect':
    if '--raw' in args:
        sys.stdout.buffer.write(manifest_bytes)
    elif '--config' in args:
        arch = option('--override-arch', 'amd64')
        config = {'architecture': arch, 'os': 'linux', 'config': {'Labels': {
            'org.opencontainers.image.version': os.environ['IMAGE_STUB_VERSION'],
            'org.opencontainers.image.revision': os.environ['IMAGE_STUB_COMMIT']}}}
        if os.environ.get('IMAGE_STUB_BAD_CONFIG'):
            config['config']['Labels']['org.opencontainers.image.revision'] = 'wrong'
        if args[-1].startswith('dir:') and os.environ.get('IMAGE_STUB_BAD_PULL'):
            config['config']['Labels']['changed'] = 'unexpected'
        print(json.dumps(config))
elif command == 'copy':
    assert '--authfile' in args
    assert pathlib.Path(option('--authfile')).exists()
    destination = args[-1]
    if destination.startswith('dir:'):
        pathlib.Path(destination[4:]).mkdir()
    if '--digestfile' in args:
        digest = 'sha256:' + hashlib.sha256(manifest_bytes).hexdigest()
        pathlib.Path(option('--digestfile')).write_text(digest + '\n')
    copy_count = sum(c['command'] == 'copy' for c in calls) + 1
    if os.environ.get('IMAGE_STUB_FAIL_COPY') == str(copy_count):
        record['failed'] = True
        with log.open('a') as output: output.write(json.dumps(record) + '\n')
        sys.exit(23)
with log.open('a') as output: output.write(json.dumps(record) + '\n')
'''


class PublishImageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='oss image tests ')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / 'source'
        (self.repo / 'deploy').mkdir(parents=True)
        for name in ('verify-release-image.sh', 'publish-image.sh'):
            shutil.copy2(SCRIPTS / name, self.repo / 'deploy' / name)
        (self.repo / 'VERSION').write_text('4.1.0\n')
        self.git('init', '-q')
        self.git('-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid',
                 '-c', 'commit.gpgsign=false', 'commit', '--allow-empty', '-qm', 'fixture')
        self.commit = self.git('rev-parse', 'HEAD').strip()
        self.git('tag', 'v4.1.0')
        self.artifacts = self.repo / 'output'
        self.artifacts.mkdir()
        self.archive = self.artifacts / 'oss-transfer-image.tar'
        self.archive.write_bytes(b'offline image archive')
        self.checksum()
        self.bin = self.root / 'bin'
        self.bin.mkdir()
        skopeo = self.bin / 'skopeo'
        skopeo.write_text(STUB)
        skopeo.chmod(0o755)
        self.log = self.root / 'skopeo-calls'
        self.env = dict(os.environ, PATH=str(self.bin) + ':' + os.environ['PATH'],
                        IMAGE_STUB_LOG=str(self.log), IMAGE_STUB_VERSION='4.1.0', IMAGE_STUB_COMMIT=self.commit,
                        REGISTRY_USERNAME='offline-user', REGISTRY_PASSWORD='offline-registry-secret',
                        RUNNER_TEMP=str(self.root), TMPDIR=str(self.root),
                        GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM='1')

    def git(self, *args):
        return subprocess.check_output(['git', '-C', str(self.repo), *args], text=True,
            stderr=subprocess.DEVNULL, env=dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_NOSYSTEM='1'))

    def checksum(self):
        digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        self.archive.with_suffix('.tar.sha256').write_text(digest + '  oss-transfer-image.tar\n')

    def publish(self, tag='v4.1.0', *args, **env):
        return subprocess.run(['bash', str(self.repo / 'deploy/publish-image.sh'), tag,
                               str(self.artifacts), *args], text=True, capture_output=True,
                              env={**self.env, **env}, timeout=20)

    def calls(self):
        return [json.loads(line) for line in self.log.read_text().splitlines()] if self.log.exists() else []

    def verify(self, *args, tag='v4.1.0', **env):
        return subprocess.run(['bash', str(self.repo / 'deploy/verify-release-image.sh'), tag,
                               str(self.artifacts), *args], text=True, capture_output=True,
                              env={**self.env, **env}, timeout=20)

    def assert_cleaned(self):
        self.assertFalse(list(self.root.glob('oss-image-*')))
        for call in self.calls():
            if 'authfile' in call:
                self.assertFalse(Path(call['authfile']).exists())

    def test_docker_hub_publishes_all_architectures_then_verifies_real_pulls_then_aliases(self):
        image = 'docker.io/moonwx/osstransfer'
        result = self.publish()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        logins = [c['args'] for c in self.calls() if c['command'] == 'login']
        self.assertEqual(len(logins), 1)
        self.assertEqual(logins[0][-1], 'docker.io')
        copies = [c['args'] for c in self.calls() if c['command'] == 'copy']
        self.assertEqual(len(copies), 5)
        self.assertIn('--all', copies[0])
        self.assertEqual(copies[0][copies[0].index('--format') + 1], 'v2s2')
        self.assertNotIn('--preserve-digests', copies[0])
        self.assertEqual(copies[0][-1], 'docker://' + image + ':4.1.0')
        self.assertEqual([c[c.index('--override-arch') + 1] for c in copies[1:3]], ['amd64', 'arm64'])
        for args, alias in zip(copies[3:], ('4.1', 'latest')):
            self.assertIn('--all', args)
            self.assertIn('--preserve-digests', args)
            self.assertTrue(args[-2].startswith('docker://' + image + '@sha256:'))
            self.assertEqual(args[-1], 'docker://' + image + ':' + alias)
        self.assertNotIn('offline-registry-secret', result.stdout + result.stderr)
        self.assert_cleaned()

    def test_prerelease_does_not_update_stable_aliases(self):
        self.git('tag', 'v4.1.0-rc.1')
        (self.repo / 'VERSION').write_text('4.1.0-rc.1\n')
        result = self.publish('v4.1.0-rc.1', IMAGE_STUB_VERSION='4.1.0-rc.1')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(sum(c['command'] == 'copy' for c in self.calls()), 3)
        self.assert_cleaned()

    def test_ci_archive_is_verified_before_a_release_tag_exists(self):
        self.git('tag', '-d', 'v4.1.0')
        result = self.verify('--ci-build')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        inspected_arches = [c['args'][c['args'].index('--override-arch') + 1]
                            for c in self.calls() if '--config' in c['args']]
        self.assertEqual(inspected_arches, ['amd64', 'arm64'])
        self.assertFalse(any(c['command'] in ('login', 'copy') for c in self.calls()))
        self.assert_cleaned()
        self.log.unlink()
        result = self.publish()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('release tag is unavailable', result.stderr)
        self.assertFalse(self.calls())

    def test_ci_mode_still_checks_project_version_architectures_commit_and_checksum(self):
        for env in ({'IMAGE_STUB_BAD_MANIFEST': '1'}, {'IMAGE_STUB_BAD_CONFIG': '1'},
                    {'IMAGE_STUB_VERSION': '4.0.2'}):
            with self.subTest(env=env):
                self.log.unlink(missing_ok=True)
                result = self.verify('--ci-build', **env)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(c['command'] in ('login', 'copy') for c in self.calls()))
                self.assert_cleaned()
        self.log.unlink(missing_ok=True)
        result = self.verify('--ci-build', tag='v4.0.4', IMAGE_STUB_VERSION='4.0.4')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('project version', result.stderr)
        self.assertFalse(self.calls())
        self.archive.write_bytes(b'corrupted archive')
        result = self.verify('--ci-build')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('SHA-256', result.stderr)
        self.assertFalse(self.calls())

    def test_release_tag_must_still_point_to_checked_out_commit(self):
        self.git('-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid',
                 '-c', 'commit.gpgsign=false', 'commit', '--allow-empty', '-qm', 'next commit')
        result = self.publish(IMAGE_STUB_COMMIT=self.git('rev-parse', 'HEAD').strip())
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('does not point to this checkout', result.stderr)
        self.assertFalse(self.calls())
        self.assert_cleaned()

    def test_bad_input_stops_before_login_and_cleans_snapshot(self):
        for env in ({'IMAGE_STUB_BAD_MANIFEST': '1'}, {'IMAGE_STUB_BAD_CONFIG': '1'}):
            with self.subTest(env=env):
                self.log.unlink(missing_ok=True)
                result = self.publish(**env)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(c['command'] == 'login' for c in self.calls()))
                self.assert_cleaned()
        self.archive.write_bytes(b'corrupted archive')
        self.log.unlink(missing_ok=True)
        result = self.publish()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.calls())
        self.assert_cleaned()

    def test_wrong_tag_or_extra_argument_never_uses_credentials(self):
        for args in [('v4.0.4',), ('v4.1.0-01',), ('v04.1.0',),
                     ('v4.1.0', 'arbitrary.invalid/repository')]:
            with self.subTest(args=args):
                result = self.publish(*args)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(self.calls())
                self.assert_cleaned()

    def test_missing_credentials_do_not_publish(self):
        result = self.publish(REGISTRY_PASSWORD='')
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c['command'] == 'login' for c in self.calls()))
        self.assert_cleaned()

    def test_failure_at_each_transfer_is_bounded_and_does_not_report_success(self):
        for copy in range(1, 6):
            with self.subTest(copy=copy):
                self.log.unlink(missing_ok=True)
                result = self.publish(IMAGE_STUB_FAIL_COPY=str(copy))
                self.assertEqual(result.returncode, 23, result.stdout + result.stderr)
                self.assertNotIn('Image publish completed', result.stdout)
                calls = [c for c in self.calls() if c['command'] == 'copy']
                self.assertEqual(len(calls), copy)
                self.assert_cleaned()

    def test_changed_pull_is_rejected_before_aliases(self):
        result = self.publish(IMAGE_STUB_BAD_PULL='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(sum(c['command'] == 'copy' for c in self.calls()), 2)
        self.assertNotIn('Image publish completed', result.stdout)
        self.assert_cleaned()

    def test_checksum_file_cannot_name_other_paths(self):
        self.archive.with_suffix('.tar.sha256').write_text('0' * 64 + '  ../different.tar\n')
        result = self.publish()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(self.calls())
        self.assert_cleaned()


if __name__ == '__main__':
    unittest.main(verbosity=2)
