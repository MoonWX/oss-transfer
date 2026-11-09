#!/usr/bin/env python3
"""Exercise installer uninstall, same-version reinstall and restart policy.

Uninstall functions run against fixture installations in a temporary
directory with a recording systemctl; no service on the test host is touched.
"""
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
NATIVE = ROOT / "deploy/native/install.sh"
PUBLIC = ROOT / "install.sh"


def function_source(source, name):
    """Return one top-level shell function definition by name."""
    match = re.search(r'(?m)^' + re.escape(name) + r'\(\) [{(]', source)
    if not match:
        raise AssertionError(f'missing function {name}')
    line_end = source.index('\n', match.start())
    line = source[match.start():line_end]
    if line.rstrip().endswith('}') or line.rstrip().endswith(')'):
        return line + '\n'
    end = source.index('\n}\n', match.start())
    return source[match.start():end + 3]


def uninstall_functions():
    source = NATIVE.read_text()
    names = ('say', 'safe_path', 'metadata_field', 'config_field', 'add_candidate', 'discover_installations')
    block = source[source.index('# BEGIN uninstall\n'):source.index('# END uninstall\n')]
    return ''.join(function_source(source, name) for name in names) + block


class Fixture:
    def __init__(self, test):
        temporary = tempfile.TemporaryDirectory(prefix='installer-uninstall-')
        test.addCleanup(temporary.cleanup)
        # Resolve /var -> /private/var on macOS; the installer rejects links.
        self.base = Path(temporary.name).resolve()
        self.root = self.base / 'root'
        self.systemd = self.base / 'systemd'
        self.home = self.base / 'home'
        self.bin = self.base / 'shims'
        self.global_bin = self.base / 'usr-local-bin'
        for directory in (self.root, self.systemd, self.home, self.bin, self.global_bin):
            directory.mkdir(parents=True)
        self.log = self.base / 'systemctl.log'
        shim = self.bin / 'systemctl'
        shim.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$SYSTEMCTL_LOG"\n'
                        '[ "$1" = stop ] && [ -n "$SYSTEMCTL_FAIL_STOP" ] && exit 1\nexit 0\n')
        shim.chmod(0o755)
        self.functions = self.base / 'functions.sh'
        self.functions.write_text(uninstall_functions())

    def install(self, role, service=None, cli=None, unit_command=None, data_files=('{role}.db',), foreign_backup=False):
        service = service or f'oss-transfer-{role}'
        base = self.root / role
        install, config, data, backups = base / 'bin', base / 'config', base / 'data', base / 'backups'
        for directory in (install, config, data / 'logs', backups / f'{service}-20260101T000000Z.abc123'):
            directory.mkdir(parents=True, exist_ok=True)
        if foreign_backup:
            (backups / 'other-installation').mkdir()
        binary = install / f'oss-transfer-{role}'
        binary.write_text('#!/bin/sh\n')
        (config / 'service.json').write_text('{"serviceName":"%s","dataDirectory":"%s"}\n' % (service, data))
        for name in data_files:
            (data / name.format(role=role)).write_text('state')
        cli = Path(cli or install / 'oss-transfer-cli')
        cli.write_text('#!/bin/sh\n')
        for suffix in ('cluster.json', 'local-auth.json', 'update.lock'):
            Path(f'{cli}.{suffix}').write_text('{}')
        unit = self.systemd / f'{service}.service'
        unit.write_text('[Service]\n' + (unit_command or f'ExecStart="{binary}" --config "{config}/service.json"') + '\n')
        (install / 'install.tsv').write_text(
            f'role\t{role}\nserviceName\t{service}\nconfig\t{config}/service.json\nunit\t{unit}\nversion\t4.2.0\n'
            f'cliPath\t{cli}\nbackupDir\t{backups}\ndataDir\t{data}\nrootDir\t{self.root}\n')
        return {'install': install, 'config': config, 'data': data, 'backups': backups, 'cli': cli, 'unit': unit, 'binary': binary}

    def uninstall(self, role='', keep_data=0, install_dir='', service='', lang='en', **environment):
        script = r'''
set -euo pipefail
source "$1"
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
lang=$FIXTURE_LANG; platform=linux; user_service=0; yes=1
root_dir=$FIXTURE_ROOT; root_explicit=1; role=$FIXTURE_ROLE; service_name=$FIXTURE_SERVICE
install_dir=$FIXTURE_INSTALL; keep_data=$FIXTURE_KEEP
systemd_unit_dir=$FIXTURE_SYSTEMD; rc_dir=$FIXTURE_SYSTEMD/rc.d; pid_dir=$FIXTURE_SYSTEMD/run
run_uninstall
'''
        env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ['PATH'], HOME=str(self.home),
                   SYSTEMCTL_LOG=str(self.log), FIXTURE_ROOT=str(self.root), FIXTURE_ROLE=role,
                   FIXTURE_SERVICE=service, FIXTURE_INSTALL=install_dir, FIXTURE_KEEP=str(keep_data),
                   FIXTURE_SYSTEMD=str(self.systemd), FIXTURE_LANG=lang, LC_ALL='C')
        env.pop('SUDO_USER', None)
        env.update(environment)
        return subprocess.run(['bash', '-c', script, 'test', str(self.functions)], env=env,
                              text=True, capture_output=True, timeout=30)

    def systemctl(self):
        return self.log.read_text().splitlines() if self.log.exists() else []


class UninstallTests(unittest.TestCase):
    def setUp(self):
        self.fixture = Fixture(self)

    def test_agent_uninstall_lists_and_removes_only_that_installation(self):
        f = self.fixture
        master = f.install('master', cli=f.global_bin / 'oss-transfer-cli', foreign_backup=True)
        agent = f.install('agent')
        sessions = f.home / '.config/oss-transfer-cli'
        sessions.mkdir(parents=True)
        result = f.uninstall(role='agent')
        self.assertEqual(result.returncode, 0, result.stderr)
        for expected in ('Installations to uninstall:', 'oss-transfer-agent (agent)', 'Will remove:',
                         f'Service: oss-transfer-agent ({agent["unit"]})', f'Program: {agent["binary"]}',
                         f'CLI: {agent["cli"]}', f'Data: {agent["data"]}', 'Uninstalled',
                         'oss-transfer-cli agent remove'):
            self.assertIn(expected, result.stdout)
        self.assertEqual(f.systemctl(), ['stop oss-transfer-agent', 'disable oss-transfer-agent', 'daemon-reload', 'reset-failed oss-transfer-agent'])
        self.assertFalse((f.root / 'agent').exists())
        self.assertFalse(agent['unit'].exists())
        # The Master, its global CLI, the user's CLI logins and the root stay.
        for path in (master['binary'], master['unit'], master['cli'], Path(f'{master["cli"]}.local-auth.json'),
                     master['data'] / 'master.db', master['backups'] / 'other-installation', sessions):
            self.assertTrue(path.exists(), path)

    def test_last_uninstall_removes_cli_state_and_keeps_foreign_files(self):
        f = self.fixture
        master = f.install('master', cli=f.global_bin / 'oss-transfer-cli', foreign_backup=True)
        agent = f.install('agent')
        (f.root / 'notes.txt').write_text('operator notes')
        sessions = f.home / '.config/oss-transfer-cli'
        sessions.mkdir(parents=True)
        (sessions / 'config.json').write_text('{}')
        result = f.uninstall()
        self.assertEqual(result.returncode, 0, result.stderr)
        for path in (master['binary'], agent['binary'], master['cli'], Path(f'{master["cli"]}.cluster.json'),
                     Path(f'{master["cli"]}.local-auth.json'), Path(f'{master["cli"]}.update.lock'),
                     master['data'], master['config'], sessions, master['backups'] / 'oss-transfer-master-20260101T000000Z.abc123'):
            self.assertFalse(path.exists(), path)
        # Another installation's backup and unknown files are never removed.
        self.assertTrue((master['backups'] / 'other-installation').exists())
        self.assertTrue((f.root / 'notes.txt').exists())
        self.assertIn(f'Kept (contains other files): {master["backups"]}', result.stdout)
        self.assertIn('CLI logins and preferences', result.stdout)
        self.assertIn('Agents that joined this Master', result.stdout)
        stops = [line for line in f.systemctl() if line.startswith('stop ')]
        self.assertEqual(sorted(stops), ['stop oss-transfer-agent', 'stop oss-transfer-master'])

    def test_uninstall_removes_only_its_own_backups_from_a_shared_directory(self):
        f = self.fixture
        agent = f.install('agent')
        # Another Agent whose service name extends this one shares the directory.
        foreign = (agent['backups'] / 'oss-transfer-agent-b-20260101T000000Z.xyz789',
                   agent['backups'] / 'oss-transfer-agent-20260101T000000Z.abc123.notes',
                   agent['backups'] / 'oss-transfer-agent-manual')
        for path in foreign:
            path.mkdir()
        result = f.uninstall(role='agent')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((agent['backups'] / 'oss-transfer-agent-20260101T000000Z.abc123').exists())
        for path in foreign:
            self.assertTrue(path.exists(), path)
        self.assertIn(f'Kept (contains other files): {agent["backups"]}', result.stdout)

    def test_keep_data_keeps_configuration_data_and_backups(self):
        f = self.fixture
        agent = f.install('agent')
        chinese = f.uninstall(role='agent', keep_data=1, lang='zh', SYSTEMCTL_FAIL_STOP='1')
        self.assertIn('将保留：', chinese.stdout)
        self.assertIn(f'数据: {agent["data"]}', chinese.stdout)
        self.assertIn('无法停止 oss-transfer-agent', chinese.stderr)
        result = f.uninstall(role='agent', keep_data=1)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Will keep:', result.stdout)
        self.assertIn(f'Data: {agent["data"]}', result.stdout.split('Will keep:')[1])
        for path in (agent['config'] / 'service.json', agent['data'] / 'agent.db', agent['backups'] / 'oss-transfer-agent-20260101T000000Z.abc123'):
            self.assertTrue(path.exists(), path)
        for path in (agent['binary'], agent['install'] / 'install.tsv', agent['unit'], agent['cli']):
            self.assertFalse(path.exists(), path)
        self.assertIn('Configuration and data were kept', result.stdout)

    def test_foreign_service_definition_stops_before_any_change(self):
        f = self.fixture
        agent = f.install('agent', unit_command='ExecStart="/usr/bin/other-program" --config "/etc/other.json"')
        result = f.uninstall(role='agent')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('does not run', result.stderr)
        self.assertEqual(f.systemctl(), [])
        for path in agent.values():
            self.assertTrue(Path(path).exists(), path)

    def test_failed_stop_keeps_all_files(self):
        f = self.fixture
        agent = f.install('agent')
        result = f.uninstall(role='agent', SYSTEMCTL_FAIL_STOP='1')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Cannot stop oss-transfer-agent', result.stderr)
        for path in (agent['binary'], agent['unit'], agent['data'] / 'agent.db', agent['cli']):
            self.assertTrue(path.exists(), path)

    def test_unrecognized_data_directory_is_kept(self):
        f = self.fixture
        agent = f.install('agent', data_files=('important.csv',))
        result = f.uninstall(role='agent')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((agent['data'] / 'important.csv').exists())
        self.assertIn('not removed automatically', result.stdout)
        self.assertFalse(agent['binary'].exists())

    def test_shared_cli_is_kept_for_the_remaining_installation(self):
        f = self.fixture
        shared = f.global_bin / 'oss-transfer-cli'
        f.install('master', cli=shared)
        f.install('agent', cli=shared)
        result = f.uninstall(role='agent')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(shared.exists())
        self.assertTrue(Path(f'{shared}.local-auth.json').exists())
        self.assertIn(f'CLI: {shared} (used by', result.stdout)

    def test_service_name_selects_one_installation(self):
        f = self.fixture
        agent = f.install('agent', service='edge-agent')
        result = f.uninstall(service='other-agent')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('nothing to uninstall', result.stdout)
        self.assertTrue(agent['binary'].exists())
        result = f.uninstall(service='edge-agent')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(agent['binary'].exists())

    def test_nothing_installed_is_not_an_error(self):
        result = self.fixture.uninstall(role='agent', lang='zh')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('未找到由安装脚本安装的服务，无需卸载', result.stdout)
        result = self.fixture.uninstall(role='agent')
        self.assertIn('nothing to uninstall', result.stdout)
        self.assertEqual(self.fixture.systemctl(), [])

    def test_uninstall_rejects_installation_options(self):
        for args in (['--uninstall', '--upgrade'], ['--uninstall', '--version', '4.2.0'], ['--uninstall', '--package-dir', '/tmp'],
                     ['--uninstall', '--force'], ['--keep-data']):
            with self.subTest(args=args):
                result = subprocess.run(['bash', str(NATIVE), '--lang', 'en', *args], text=True, capture_output=True, timeout=30)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('--uninstall', result.stderr)


class CombinedUninstallTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='combined-uninstall-')
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve()
        self.commands = self.base / 'commands'
        self.commands.mkdir()
        self.events = self.base / 'events'
        for name, body in {
            'id': 'printf "0\\n"',
            'uname': 'case "$1" in -s) printf "Linux\\n";; -m) printf "x86_64\\n";; esac',
            'curl': 'printf "curl %s\\n" "$*" >> "$EVENTS"; exit 99',
            'docker': 'printf "docker %s\\n" "$*" >> "$EVENTS"\n'
                      'case "$*" in\n'
                      '  *config_files=*) [ -z "$DOCKER_FILE_PROJECT" ] || printf "%s\\n" "$DOCKER_FILE_PROJECT";;\n'
                      '  *volume=*) [ -z "$DOCKER_MOUNTED" ] || printf "%s\\n" "$DOCKER_MOUNTED";;\n'
                      'esac\nexit 0',
            'systemctl': 'printf "systemctl %s\\n" "$*" >> "$EVENTS"; exit 0',
        }.items():
            path = self.commands / name
            path.write_text('#!/bin/sh\n' + body + '\n')
            path.chmod(0o755)
        self.env = dict(os.environ, PATH=str(self.commands) + os.pathsep + os.environ['PATH'], EVENTS=str(self.events), LC_ALL='C')

    def run_public(self, *args, **environment):
        return subprocess.run(['bash', str(PUBLIC), '--lang', 'en', *args], env=dict(self.env, **environment), text=True,
                              capture_output=True, timeout=60, stdin=subprocess.DEVNULL)

    def test_native_uninstall_uses_embedded_installer_without_downloading(self):
        result = self.run_public('--uninstall', '--role', 'agent', '--root-dir', str(self.base / 'empty-root'), '--yes')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('nothing to uninstall', result.stdout)
        events = self.events.read_text() if self.events.exists() else ''
        self.assertNotIn('curl', events)

    def test_compose_uninstall_removes_only_its_project(self):
        install, data = self.base / 'compose/agent', self.base / 'compose-data/agent'
        install.mkdir(parents=True)
        data.mkdir(parents=True)
        (data / 'agent.db').write_text('state')
        compose = install / 'docker-compose.yml'
        compose.write_text("services:\n  agent:\n    image: 'moonwx/osstransfer:4.2.0'\n"
                           "    command: ['oss-transfer-agent', '--data-dir', '/data']\n")
        result = self.run_public('--uninstall', '--method', 'compose', '--role', 'agent', '--install-dir', str(install),
                                 '--data-dir', str(data), '--yes')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f'docker compose --project-name oss-transfer-agent --file {compose} down --remove-orphans', self.events.read_text())
        self.assertFalse(install.exists())
        self.assertFalse(data.exists())
        self.assertIn('Docker image is kept', result.stdout)

    def compose_installation(self, marker=True, data=None):
        install, data = self.base / 'compose/agent', data or self.base / 'custom-data/agent'
        install.mkdir(parents=True)
        data.mkdir(parents=True)
        (data / 'agent.db').write_text('state')
        compose = install / 'docker-compose.yml'
        compose.write_text(('# oss-transfer-project: edge-agent\n' if marker else '') +
                           "services:\n  agent:\n    image: 'moonwx/osstransfer:4.2.0'\n"
                           "    command: ['oss-transfer-agent', '--data-dir', '/data']\n"
                           f"    volumes:\n      - '{data}:/data'\n      - '/srv/files:/srv/files'\n")
        return install, data, compose

    def test_compose_uninstall_uses_the_recorded_project_and_data_directory(self):
        for marker in (True, False):
            with self.subTest(marker=marker):
                install, data, compose = self.compose_installation(marker)
                # Earlier releases recorded neither; Compose labels the containers with the file.
                result = self.run_public('--uninstall', '--method', 'compose', '--role', 'agent', '--install-dir', str(install),
                                         '--yes', DOCKER_FILE_PROJECT='' if marker else 'edge-agent')
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f'docker compose --project-name edge-agent --file {compose} down --remove-orphans', self.events.read_text())
                self.assertIn(f'Remove data: {data}', result.stdout)
                self.assertFalse(install.exists())
                self.assertFalse(data.exists())
                self.events.unlink()

    def test_compose_uninstall_keeps_data_a_container_still_mounts(self):
        install, data, compose = self.compose_installation()
        result = self.run_public('--uninstall', '--method', 'compose', '--role', 'agent', '--install-dir', str(install),
                                 '--yes', DOCKER_MOUNTED='edge-agent-agent-1')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('still used by container(s): edge-agent-agent-1', result.stderr)
        self.assertTrue(compose.exists())
        self.assertTrue((data / 'agent.db').exists())

    def test_compose_uninstall_refuses_other_names_and_directories(self):
        install, data, compose = self.compose_installation()
        for args, message in ((['--service-name', 'oss-transfer-agent'], 'belongs to Compose project edge-agent'),
                              (['--data-dir', str(self.base / 'other')], f'uses data directory {data}')):
            with self.subTest(args=args):
                result = self.run_public('--uninstall', '--method', 'compose', '--role', 'agent', '--install-dir', str(install),
                                         '--yes', *args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertTrue(compose.exists())
                self.assertTrue((data / 'agent.db').exists())
        self.assertNotIn('down', self.events.read_text() if self.events.exists() else '')

    def test_compose_uninstall_refuses_a_foreign_compose_file(self):
        install = self.base / 'compose/agent'
        install.mkdir(parents=True)
        (install / 'docker-compose.yml').write_text('services:\n  web:\n    image: nginx\n')
        result = self.run_public('--uninstall', '--method', 'compose', '--role', 'agent', '--install-dir', str(install),
                                 '--data-dir', str(self.base / 'data'), '--yes')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('not written by this installer', result.stderr)
        self.assertTrue((install / 'docker-compose.yml').exists())
        self.assertFalse(self.events.exists() and 'down' in self.events.read_text())


class SameVersionTests(unittest.TestCase):
    """installation_current decides whether rerunning a release changes nothing."""

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix='installer-current-')
        self.addCleanup(temporary.cleanup)
        self.base = Path(temporary.name).resolve()
        source = NATIVE.read_text()
        self.functions = self.base / 'functions.sh'
        self.functions.write_text(function_source(source, 'metadata_field') + function_source(source, 'installation_current'))
        self.shims = self.base / 'shims'
        self.shims.mkdir()
        shim = self.shims / 'systemctl'
        shim.write_text('#!/bin/sh\n[ "$SERVICE_ACTIVE" = 1 ]\n')
        shim.chmod(0o755)
        # launchctl print succeeds for any loaded job; only its state says whether it runs.
        launchctl = self.shims / 'launchctl'
        launchctl.write_text('#!/bin/sh\n[ -n "$LAUNCHD_STATE" ] || exit 113\n'
                             'printf \'system/oss-transfer-master = {\\n\\tactive count = 0\\n\\tstate = %s\\n\\n'
                             '\\tendpoints = {\\n\\t\\tstate = running\\n\\t}\\n}\\n\' "$LAUNCHD_STATE"\n')
        launchctl.chmod(0o755)
        for name, output in {'binary': 'oss-transfer-master 4.2.0 (abc, built now)', 'cli': '{"version":"4.2.0"}'}.items():
            path = self.base / name
            path.write_text('#!/bin/sh\nprintf \'%s\\n\' \'' + output + '\'\n')
            path.chmod(0o755)
        (self.base / 'unit').write_text('[Service]\n')
        (self.base / 'install.tsv').write_text(f'role\tmaster\nversion\t4.2.0\ncliPath\t{self.base}/cli\n')
        (self.base / 'manifest.tsv').write_text('format\toss-transfer-go-v1\nversion\t4.2.0\n' + ''.join(
            f'asset\t{name}\t{"a" * 64}\t1\n' for name in ('install.sh', 'native-install.sh', 'Install.ps1')))
        (self.base / 'data/packages').mkdir(parents=True)
        (self.base / 'data/packages/manifest.tsv').write_text('format\toss-transfer-go-v1\nversion\t4.2.0\n')

    def current(self, **overrides):
        values = dict(existed='1', force='0', replace_cli='0', cli_migrated='0', token_stdin='0', adopt_config='',
                      version='4.2.0', SERVICE_ACTIVE='1', TRANSFER_JOIN_TOKEN='', PLATFORM='linux', LAUNCHD_STATE='')
        values.update(overrides)
        script = r'''
set -euo pipefail
source "$1"
role=master; platform=$PLATFORM; domain=system; service_name=oss-transfer-master; work=$BASE; data_dir=$BASE/data
metadata=$BASE/install.tsv; binary=$BASE/binary; cli_path=$BASE/cli; unit=$BASE/unit
existed=$existed; force=$force; replace_cli=$replace_cli; cli_migrated=$cli_migrated; token_stdin=$token_stdin
adopt_config=$adopt_config; version=$version
installation_current
'''
        env = dict(os.environ, PATH=str(self.shims) + os.pathsep + os.environ['PATH'], BASE=str(self.base), **values)
        return subprocess.run(['bash', '-c', script, 'test', str(self.functions)], env=env, text=True,
                              capture_output=True, timeout=30).returncode == 0

    def test_complete_running_installation_is_current(self):
        self.assertTrue(self.current())

    def test_macos_requires_the_launchd_job_to_be_running(self):
        self.assertTrue(self.current(PLATFORM='darwin', LAUNCHD_STATE='running'))
        self.assertFalse(self.current(PLATFORM='darwin', LAUNCHD_STATE='not running'), 'a loaded but stopped job was treated as running')
        self.assertFalse(self.current(PLATFORM='darwin'), 'an unloaded job was treated as running')

    def test_anything_requested_or_stale_runs_the_installation(self):
        for overrides in ({'force': '1'}, {'version': '4.2.1'}, {'SERVICE_ACTIVE': '0'}, {'existed': '0'},
                          {'TRANSFER_JOIN_TOKEN': 'invitation'}, {'token_stdin': '1'}, {'replace_cli': '1'},
                          {'adopt_config': '/tmp/service.json'}):
            with self.subTest(overrides=overrides):
                self.assertFalse(self.current(**overrides))
        (self.base / 'data/packages/manifest.tsv').write_text('format\toss-transfer-go-v1\nversion\t4.1.41\n')
        self.assertFalse(self.current(), 'stale Agent packages must be repaired')

    def test_skip_happens_before_download_and_force_is_documented(self):
        source = NATIVE.read_text()
        self.assertLess(source.index('if installation_current; then'), source.index('fetch "$archive_name"'))
        self.assertIn('Add --force to reinstall it.', source)
        self.assertIn('--force', subprocess.run(['bash', str(PUBLIC), '--help'], text=True, capture_output=True).stdout)


class RestartPolicyTests(unittest.TestCase):
    def test_systemd_unit_does_not_restart_a_removed_agent(self):
        source = NATIVE.read_text()
        unit = source[source.index('cat > "$unit" <<EOF\n[Unit]'):]
        unit = unit[:unit.index('\nEOF\n')]
        self.assertIn('Restart=on-failure', unit)
        self.assertIn('RestartPreventExitStatus=78', unit)

    def test_startup_failure_explains_a_removed_agent(self):
        source = NATIVE.read_text()
        startup = function_source(source, 'startup_failure')
        self.assertIn("*'enrollment was revoked'*", startup)


if __name__ == '__main__':
    unittest.main()
