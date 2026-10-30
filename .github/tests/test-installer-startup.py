#!/usr/bin/env python3
"""Exercise native installer port ownership and startup failure handling."""
import os
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
INSTALLER = ROOT / "deploy/native/install.sh"
PUBLIC_INSTALLER = ROOT / "install.sh"


class EnrollmentInputTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="installer-enrollment-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        source = PUBLIC_INSTALLER.read_text()
        self.functions = self.root / "enrollment.sh"
        self.functions.write_text(source[source.index("valid_master_origin() {"):source.index("\nstage=$(mktemp")])

    def invoke(self, body, value):
        return subprocess.run(
            ["bash", "-c", 'set -euo pipefail; source "$1"; ' + body, "test", str(self.functions), value],
            text=True, capture_output=True, timeout=10)

    def test_master_origins_preserve_dns_ipv4_and_accept_ipv6(self):
        for origin in (
            'http://10.1.0.43:1685', 'https://transfer.example', 'http://localhost:80/',
            'http://[fd00::43]:1685', 'https://[2001:db8:1:2:3:4:5:6]',
            'http://[::1]:65535/', 'http://[::ffff:192.0.2.43]:1685', 'http://[::]:1685',
        ):
            with self.subTest(origin=origin):
                result = self.invoke('valid_master_origin "$2"', origin)
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_master_origins_reject_paths_credentials_invalid_ports_and_literals(self):
        for origin in (
            'http://user:secret@10.1.0.43:1685', 'http://10.1.0.43:1685/path',
            'http://[::1]:1685?secret=1', 'http://[::1]:1685#fragment', 'ftp://[::1]:1685',
            'http://[::1]:0', 'http://[::1]:65536', 'http://[::1]:99999999999999999999',
            'http://[::1]:abc', 'http://[::1]:', 'http://fd00::43:1685',
            'http://[fd00:::43]:1685', 'http://[fd00::1::43]:1685', 'http://[fd00:43]:1685',
            'http://[1:2:3:4:5:6:7:8:9]', 'http://[::1:2:3:4:5:6:7:8]',
            'http://[::ffff:192.0.2.999]', 'http://[::ffff:192.0.2]', 'http://[::xyz]',
            'http://[fe80::1%eth0]', 'http://[::1]\n', 'http:// transfer.example',
        ):
            with self.subTest(origin=origin):
                result = self.invoke('valid_master_origin "$2"', origin)
                self.assertNotEqual(result.returncode, 0, result.stdout)

    def test_invitation_url_extracts_ipv6_master_and_exact_token(self):
        body = r'''
fail() { exit 12; }
role=agent; upgrade=0; adopt_config=; master_url=; join_url=$2; yes=1
enrollment_input
printf '%s\n%s\n' "$master_url" "$TRANSFER_JOIN_TOKEN"
'''
        origin = 'http://[fd00::43]:1685'
        token = 'invitation_token-123'
        result = self.invoke(body, origin + '/api/agents/bootstrap/' + token)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, origin + '\n' + token + '\n')
        for url in (
            origin + '//api/agents/bootstrap/' + token,
            origin + '/api/agents/bootstrap/' + token + '?foo=1',
            'http://user:secret@[fd00::43]:1685/api/agents/bootstrap/' + token,
            'http://[fd00::43]:65536/api/agents/bootstrap/' + token,
            origin + '/api/agents/bootstrap/',
        ):
            with self.subTest(url=url):
                self.assertNotEqual(self.invoke(body, url).returncode, 0)

    def test_real_installer_sudo_reentry_keeps_token_stdin_and_arguments(self):
        # Stop at the elevation boundary: no sudo privilege, network access,
        # or service installation is performed by this test.
        for name, text in {
            'id': '#!/bin/sh\nprintf 1000\n',
            'sudo': '#!/bin/sh\nprintf "%s\\n" "$@" > "$ELEVATION_ARGS"\ncat > "$ELEVATION_TOKEN"\n',
            'curl': '#!/bin/sh\nexit 99\n',
        }.items():
            path = self.root / name
            path.write_text(text)
            path.chmod(0o755)
        args = ['--role', 'agent', '--method', 'native', '--master-url', 'http://[fd00::43]:1685',
                '--token-stdin', '--yes']
        token = 'invitation-token_kept-exactly'
        result = subprocess.run(
            ['bash', str(PUBLIC_INSTALLER), *args], input=token, text=True, capture_output=True,
            env=dict(os.environ, PATH=str(self.root) + os.pathsep + os.environ['PATH'],
                     ELEVATION_ARGS=str(self.root / 'args'), ELEVATION_TOKEN=str(self.root / 'token')),
            timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / 'args').read_text().splitlines(), ['bash', str(PUBLIC_INSTALLER), *args])
        self.assertEqual((self.root / 'token').read_text(), token)
        self.assertNotIn(token, result.stdout + result.stderr)


class StartupTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="installer-startup-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        source = INSTALLER.read_text()
        self.functions = self.root / "functions.sh"
        self.functions.write_text(source[source.index("listener_address() {"):source.index("rollback() {")])
        self.environment = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                                QA_ROOT=str(self.root), QA_OWN_PID="0", QA_LISTEN="0.0.0.0:1685",
                                QA_PLATFORM="linux", QA_EXISTED="0", QA_SS="", QA_SOCKET_STATUS="0",
                                QA_MANAGER_STATUS="", QA_LOG="", QA_READY_AFTER="0")
        self.shim("ss", 'printf "%s" "$QA_SS"; exit "$QA_SOCKET_STATUS"')
        self.shim("systemctl", 'printf "%s\\n" "$QA_MANAGER_STATUS"')
        self.shim("master", '''count=$(cat "$QA_ROOT/probes" 2>/dev/null || printf 0)
count=$((count+1)); printf '%s' "$count" > "$QA_ROOT/probes"
[ "$QA_READY_AFTER" -gt 0 ] && [ "$count" -ge "$QA_READY_AFTER" ]''')

    def shim(self, name, body):
        path = self.bin / name
        path.write_text("#!/bin/sh\n" + body + "\n")
        path.chmod(0o755)

    def run_shell(self, expression, **environment):
        preamble = r'''
set -euo pipefail
source "$1"
fail() { printf 'Error: %s\n' "$*" >&2; exit 1; }
service_pid() { printf '%s\n' "$QA_OWN_PID"; }
role=master; platform=$QA_PLATFORM; existed=$QA_EXISTED; work=$QA_ROOT
printf 'listen\t%s\n' "$QA_LISTEN" > "$work/config.tsv"
service_name=oss-qa-startup; log_file=$QA_ROOT/service.log
printf '%s' "$QA_LOG" > "$log_file"
binary=$QA_ROOT/bin/master; config=$QA_ROOT/service.json; launch_id=qa
startup_log_lines=0; startup_previous_attempt=100; startup_timeout=3
'''
        return subprocess.run(["bash", "-c", preamble + expression, "test", str(self.functions)],
                              env=dict(self.environment, **environment), text=True, capture_output=True, timeout=10)

    def check_port(self, rows, success, **environment):
        result = self.run_shell('check_listener\nprintf passed\n', QA_SS=rows, **environment)
        self.assertEqual(result.returncode == 0, success, result.stdout + result.stderr)
        return result

    def test_foreign_java_wildcard_rejected_without_install_mutation(self):
        result = self.check_port('LISTEN 0 50 *:1685 *:* users:(("java",pid=412,fd=22))\n', False)
        self.assertIn('1685', result.stderr)
        self.assertIn('PID 412', result.stderr)
        self.assertIn('No installation files or services were changed', result.stderr)
        source = INSTALLER.read_text()
        check = source.index('\ncheck_listener\n')
        for operation in ('\nmkdir -p "$install_dir"', '\n  backup=$(mktemp -d "$backup_dir/', '\n  stop_service\n', '\ncp "$work/$filename" "$binary.new"'):
            self.assertLess(check, source.index(operation, check if operation.startswith('\n  backup') else 0))

    def test_managed_process_allowed_but_unrelated_shared_socket_refused(self):
        own = 'LISTEN 0 128 0.0.0.0:1685 0.0.0.0:* users:(("master",pid=812,fd=5))\n'
        self.check_port(own, True, QA_OWN_PID='812', QA_EXISTED='1')
        self.check_port(own, False, QA_OWN_PID='812', QA_EXISTED='0')
        self.check_port(own.replace('pid=812,fd=5', 'pid=812,fd=5),("foreign",pid=813,fd=7'), False,
                        QA_OWN_PID='812', QA_EXISTED='1')

    def test_address_matching_and_unknown_owner(self):
        for listen, bound, success in [
            ('127.0.0.1:1685', '127.0.0.2:1685', True),
            ('127.0.0.1:1685', '0.0.0.0:1685', False),
            ('0.0.0.0:1685', '[::]:1685', False),
            ('[::1]:1685', '[0:0:0:0:0:0:0:1]:1685', False),
            ('[::1]:1685', '[::2]:1685', True),
            ('[::1]:1685', '0.0.0.0:1685', True),
            ('127.0.0.1:1685', '[::ffff:127.0.0.1]:1685', False),
        ]:
            with self.subTest(listen=listen, bound=bound):
                self.check_port(f'LISTEN 0 128 {bound} *:*\n', success, QA_LISTEN=listen)

    def test_ipv6_only_wildcard_does_not_reserve_ipv4(self):
        for v6only, success in [('1', True), ('0', False)]:
            self.check_port(f'LISTEN 0 128 [::]:1685 [::]:* v6only:{v6only}\n', success,
                            QA_LISTEN='127.0.0.1:1685')

    def test_inspection_failure_is_not_treated_as_free(self):
        result = self.check_port('', False, QA_SOCKET_STATUS='1')
        self.assertIn('Cannot inspect', result.stderr)

    def test_agent_skips_listener_check(self):
        result = self.run_shell('role=agent; check_listener', QA_SOCKET_STATUS='1')
        self.assertEqual(result.returncode, 0, result.stderr)

    @unittest.skipUnless(shutil.which('lsof'), 'lsof is unavailable')
    def test_real_local_socket_conflicts_and_its_owner_can_upgrade(self):
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            listener.listen()
            port = listener.getsockname()[1]
            result = self.run_shell('check_listener', QA_PLATFORM='darwin', QA_LISTEN=f'127.0.0.1:{port}')
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertIn(f'PID {os.getpid()}', result.stderr)
            result = self.run_shell('check_listener', QA_PLATFORM='darwin', QA_LISTEN=f'127.0.0.1:{port}',
                                    QA_EXISTED='1', QA_OWN_PID=str(os.getpid()))
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_old_failure_record_does_not_reject_current_start(self):
        old = 'Service failed: listen tcp :1685: bind: address already in use\n'
        result = self.run_shell('startup_log_lines=1; wait_ready', QA_LOG=old, QA_OWN_PID='812',
                                QA_READY_AFTER='2', QA_MANAGER_STATUS='ExecMainStartTimestampMonotonic=100\nExecMainCode=1\nExecMainStatus=1')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Waiting for', result.stdout)

    def test_new_service_failure_exits_before_timeout_and_redacts_message(self):
        result = self.run_shell('wait_ready', QA_LOG='Service failed: private-endpoint-password\n')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('service reported a startup failure', result.stderr)
        self.assertNotIn('private-endpoint-password', result.stderr)
        self.assertNotIn('timed out', result.stderr)

    def test_systemd_execution_failure_is_reported_without_waiting_for_restarts(self):
        result = self.run_shell('wait_ready', QA_MANAGER_STATUS='ExecMainStartTimestampMonotonic=101\nExecMainCode=1\nExecMainStatus=203')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('exited with status 203', result.stderr)
        self.assertNotIn('timed out', result.stderr)

    def test_running_unready_service_keeps_waiting_then_reports_timeout(self):
        result = self.run_shell('wait_ready', QA_OWN_PID='812',
                                QA_MANAGER_STATUS='ExecMainStartTimestampMonotonic=101\nExecMainCode=0\nExecMainStatus=0')
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Startup timed out after 3s', result.stderr)
        self.assertGreaterEqual(int((self.root/'probes').read_text()), 1)

    def test_initial_password_printed_only_from_this_master_startup(self):
        old = 'Initial administrator: admin\nInitial password: old_password\n'
        current = 'Initial administrator: admin\nInitial password: current_password\n'
        for platform in ('linux', 'darwin', 'freebsd'):
            with self.subTest(platform=platform):
                result = self.run_shell('startup_log_lines=2; print_initial_credentials',
                                        QA_PLATFORM=platform, QA_LOG=old + current)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, '\nInitial administrator: admin\nInitial password: current_password\n')
                initialized = self.run_shell('startup_log_lines=2; print_initial_credentials',
                                            QA_PLATFORM=platform, QA_LOG=old + 'Master listening on :1685\n')
                self.assertEqual(initialized.returncode, 0, initialized.stderr)
                self.assertEqual(initialized.stdout, '')
        agent = self.run_shell('role=agent; print_initial_credentials', QA_LOG=current)
        self.assertEqual(agent.returncode, 0, agent.stderr)
        self.assertEqual(agent.stdout, '')

    def test_initial_password_ignores_unrelated_and_malformed_logs(self):
        result = self.run_shell('print_initial_credentials', QA_LOG=(
            'Initial password: first_password\n'
            's3 error with private-key-value\n'
            'Initial password: current-password_2\n'
            'Initial password: invalid password\n'))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, '\nInitial administrator: admin\nInitial password: current-password_2\n')

    def test_fixed_older_release_prints_only_its_current_setup_token(self):
        old = 'Open the console to complete setup. Setup token: old_token\n'
        current = 'Open the console to complete setup. Setup token: current_token\n'
        result = self.run_shell('startup_log_lines=1; print_initial_credentials', QA_LOG=old + current)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, '\nSetup token: current_token\n')
        restarted = self.run_shell('startup_log_lines=1; print_initial_credentials',
                                   QA_LOG=old + 'Master listening on :1685\n')
        self.assertEqual(restarted.returncode, 0, restarted.stderr)
        self.assertEqual(restarted.stdout, '')
        modern = self.run_shell('print_initial_credentials',
                               QA_LOG=current + 'Initial password: modern_password\n')
        self.assertEqual(modern.returncode, 0, modern.stderr)
        self.assertEqual(modern.stdout, '\nInitial administrator: admin\nInitial password: modern_password\n')


class StartupCommitTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='installer-commit-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = INSTALLER.read_text()
        self.function = self.source[self.source.index('commit_start() {'):self.source.index('\nprint_initial_credentials() {')]

    def invoke(self, body='', fail=''):
        start = self.source.rindex('\nprepare_start\n')
        end = self.source.index("\nprintf 'Installed ", start)
        finish = self.source[start:end]
        script = '''set -eEuo pipefail
trap 'printf rolled-back > "$TEST_ROOT/rollback"; exit 7' ERR
ready_file=$TEST_ROOT/ready; launch_id=''' + 'a' * 64 + '''
platform=linux; existed=1; had_unit=1; was_enabled=1; service_name=test
prepare_start() { printf '%s\\n' "$launch_id" > "$ready_file.request"; }
start_service() { return 0; }
wait_ready() { [[ "$TEST_FAIL" != ready ]]; }
systemctl() { [[ "$TEST_FAIL" != enable ]]; }
finish_backup() { printf confirmed > "$TEST_ROOT/backup-finished"; }
''' + self.function + '\n' + body + '\n' + finish + '\nprintf installed\n'
        return subprocess.run(['bash', '-c', script], text=True, capture_output=True, timeout=5,
                              env=dict(os.environ, TEST_ROOT=str(self.root), TEST_FAIL=fail))

    def test_commit_is_written_only_after_readiness_and_rollback_region(self):
        for phase in ('ready', 'enable'):
            with self.subTest(phase=phase):
                result = self.invoke(fail=phase)
                self.assertEqual(result.returncode, 7, result.stderr)
                self.assertTrue((self.root/'rollback').exists())
                self.assertFalse((self.root/'ready.committed').exists())
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        committed = self.root/'ready.committed'
        self.assertEqual(committed.read_text(), (self.root/'ready.request').read_text())
        self.assertEqual(committed.stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.root/'backup-finished').read_text(), 'confirmed')

    def test_commit_failure_warns_without_rolling_back(self):
        result = self.invoke('mv() { return 7; }')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('installed', result.stdout)
        self.assertIn('service remains available', result.stderr)
        self.assertFalse((self.root/'rollback').exists())
        self.assertFalse((self.root/'ready.committed').exists())
        self.assertEqual(list(self.root.glob('ready.committed.*')), [])
        self.assertFalse((self.root/'backup-finished').exists())

    def test_commit_refuses_symlink_or_nonregular_target(self):
        target = self.root/'unchanged'
        target.write_text('unchanged')
        committed = self.root/'ready.committed'
        for kind in ('symlink', 'directory'):
            with self.subTest(kind=kind):
                if kind == 'symlink': committed.symlink_to(target)
                else: committed.mkdir()
                result = self.invoke()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('service remains available', result.stderr)
                self.assertEqual(target.read_text(), 'unchanged')
                self.assertFalse((self.root/'rollback').exists())
                self.assertFalse((self.root/'backup-finished').exists())
                if kind == 'symlink': committed.unlink()
                else: committed.rmdir()


class BackupRetentionTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='installer-retention-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        source = INSTALLER.read_text()
        self.functions = source[source.index('maintain_backups() {'):source.index('\ncheck_backup_space() {')]
        candidate = self.root/'candidate'
        candidate.write_text('''#!/bin/sh
printf '%s\\n' "$@" >> "$TEST_ROOT/arguments"
if [ "$TEST_FAIL" = 1 ]; then exit 7; fi
case "$2" in
  complete) printf '{"completed":"upgrade-1"}\\n';;
  prune) printf '{"removed":["upgrade-old"],"count":1,"freedBytes":4096}\\n';;
esac
''')
        candidate.chmod(0o755)

    def invoke(self, action, fail=False):
        body = '''set -eEuo pipefail
trap 'printf rollback >> "$TEST_ROOT/rollback"' ERR
''' + self.functions + '''
work=$TEST_ROOT; filename=candidate
config="$TEST_ROOT/config directory/service.json"
backup_dir="$TEST_ROOT/backup directory"; backup="$backup_dir/upgrade-1"
maintain_backups "$1" || true
printf 'installation continues\\n'
'''
        return subprocess.run(['bash','-c',body,'test',action],
                              env=dict(os.environ, TEST_ROOT=str(self.root), TEST_FAIL='1' if fail else '0'),
                              text=True,capture_output=True,timeout=5)

    def test_candidate_completes_only_the_named_backup(self):
        result = self.invoke('complete')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root/'arguments').read_text().splitlines(), [
            '--backup-maintenance','complete','--backup-dir',str(self.root/'backup directory'),
            '--config',str(self.root/'config directory/service.json'),'--backup-entry','upgrade-1'])
        self.assertEqual(result.stdout, 'installation continues\n')

    def test_prune_reports_removals_without_printing_internal_json(self):
        result = self.invoke('prune')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Removed 1 completed upgrade backup(s).', result.stdout)
        self.assertNotIn('freedBytes', result.stdout)
        self.assertNotIn('--backup-entry', (self.root/'arguments').read_text())

    def test_maintenance_failure_does_not_abort_or_roll_back_service(self):
        for action in ('prune','complete'):
            with self.subTest(action=action):
                result = self.invoke(action, fail=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('installation continues', result.stdout)
                self.assertIn('could not finish upgrade backup maintenance', result.stderr)
                self.assertFalse((self.root/'rollback').exists())

    def test_retention_runs_before_space_check_and_after_startup_success(self):
        source = INSTALLER.read_text()
        preflight = source.index('\n  maintain_backups prune || true\n')
        self.assertLess(preflight, source.index('\n  check_backup_space\n'))
        self.assertLess(preflight, source.index('\n  stop_service\n'))
        completion = source.index('\nif commit_start; then finish_backup; fi\n')
        self.assertGreater(completion, source.rindex('\nwait_ready\n'))
        self.assertGreater(completion, source.rindex('\ntrap - ERR INT TERM\n'))


class TemporaryBackupTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='installer-temporary-backup-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = INSTALLER.read_text()
        self.backup_dir = self.root/'backups'
        self.backup = self.backup_dir/'oss-transfer-agent-20261030.abcdef'
        self.backup.mkdir(parents=True)
        (self.backup/'database').write_text('rollback state')
        self.unknown = self.backup_dir/'unknown'
        self.unknown.mkdir()
        (self.unknown/'database').write_text('keep unknown backup')
        self.work = self.root/'work'
        self.work.mkdir()
        candidate = self.work/'candidate'
        candidate.write_text('''#!/bin/sh
printf '%s\\n' "$2" >> "$TEST_ROOT/maintenance"
[ "$TEST_FAIL" != "$2" ] || exit 7
case "$2" in
  complete) touch "$TEST_BACKUP/completed"; printf '{"completed":"entry"}\\n';;
  prune) [ ! -f "$TEST_BACKUP/completed" ] || rm -rf -- "$TEST_BACKUP"; printf '{"count":1}\\n';;
esac
''')
        candidate.chmod(0o755)

    def invoke(self, existed, fail=''):
        source = self.source
        functions = source[source.index('maintain_backups() {'):source.index('\ncheck_backup_space() {')]
        functions += '\n' + source[source.index('commit_start() {'):source.index('\nprint_initial_credentials() {')]
        functions += '\n' + source[source.index('cleanup() {'):source.index('\ntrap cleanup EXIT')]
        start = source.rindex('\nprepare_start\n')
        finish = source[start:source.index("\nprintf 'Installed ", start)]
        body = '''set -eEuo pipefail
work=$TEST_ROOT/work; filename=candidate
backup_dir=$TEST_ROOT/backups; backup=$TEST_BACKUP; owned_backup=$backup
config=$TEST_ROOT/service.json; ready_file=$TEST_ROOT/ready
existed=$TEST_EXISTED; service_name=oss-transfer-agent
platform=linux; had_unit=1; was_enabled=1
launch_id=''' + 'b' * 64 + '''
prepare_start() { printf '%s\\n' "$launch_id" > "$ready_file.request"; }
start_service() { return 0; }
wait_ready() { [[ "$TEST_FAIL" != ready ]]; }
systemctl() { [[ "$TEST_FAIL" != enable ]]; }
mv() { [[ "$TEST_FAIL" != commit ]] && command mv "$@"; }
rollback() { printf rollback > "$TEST_ROOT/rollback"; exit 7; }
''' + functions + '\ntrap cleanup EXIT\ntrap rollback ERR INT TERM\n' + finish + '\nprintf installed\n'
        return subprocess.run(['bash','-c',body], text=True, capture_output=True, timeout=5,
                              env=dict(os.environ, TEST_ROOT=str(self.root), TEST_BACKUP=str(self.backup),
                                       TEST_EXISTED=str(existed), TEST_FAIL=fail))

    def assert_unknown_preserved(self):
        self.assertEqual((self.unknown/'database').read_text(), 'keep unknown backup')

    def test_upgrade_completes_then_prunes_only_after_committed_start(self):
        result = self.invoke(1)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root/'maintenance').read_text().splitlines(), ['complete','prune'])
        self.assertFalse(self.backup.exists())
        self.assertNotIn('Backup retained', result.stderr)
        self.assert_unknown_preserved()

    def test_successful_cli_replacement_or_adoption_removes_current_backup(self):
        result = self.invoke(0)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.backup.exists())
        self.assertFalse((self.root/'maintenance').exists())
        self.assert_unknown_preserved()

    def test_failed_or_unconfirmed_start_retains_and_prints_backup(self):
        for existed in (0, 1):
            for failure in ('ready', 'enable', 'commit'):
                with self.subTest(existed=existed, failure=failure):
                    self.work.mkdir(exist_ok=True)
                    result = self.invoke(existed, failure)
                    self.assertEqual(result.returncode, 0 if failure == 'commit' else 7, result.stderr)
                    self.assertEqual((self.backup/'database').read_text(), 'rollback state')
                    self.assertIn('Backup retained: '+str(self.backup), result.stderr)
                    self.assertFalse((self.root/'maintenance').exists())
                    self.assert_unknown_preserved()

    def test_completion_failure_never_calls_prune(self):
        result = self.invoke(1, 'complete')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root/'maintenance').read_text().splitlines(), ['complete'])
        self.assertTrue(self.backup.is_dir())
        self.assertIn('Backup retained: '+str(self.backup), result.stderr)
        self.assertFalse((self.root/'rollback').exists())

    def test_cleanup_failure_keeps_backup_without_rolling_back_healthy_service(self):
        result = self.invoke(1, 'prune')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root/'maintenance').read_text().splitlines(), ['complete','prune'])
        self.assertTrue(self.backup.is_dir())
        self.assertIn('Backup retained: '+str(self.backup), result.stderr)
        self.assertFalse((self.root/'rollback').exists())

    def test_partial_backup_cleanup_refuses_a_replaced_symlink(self):
        shutil.rmtree(self.backup)
        self.backup.symlink_to(self.unknown, target_is_directory=True)
        result = self.invoke(0)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('temporary backup path changed', result.stderr)
        self.assertTrue(self.backup.is_symlink())
        self.assert_unknown_preserved()

    def test_partial_backup_cleanup_preserves_unexpected_or_nonregular_contents(self):
        for kind in ('unexpected', 'hidden', 'directory', 'symlink'):
            with self.subTest(kind=kind):
                extra = self.backup/('.unexpected' if kind == 'hidden' else 'cli' if kind in ('directory','symlink') else 'unexpected')
                if kind == 'directory': extra.mkdir()
                elif kind == 'symlink': extra.symlink_to(self.unknown/'database')
                else: extra.write_text('unexpected file')
                self.work.mkdir(exist_ok=True)
                result = self.invoke(0)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('unexpected temporary backup contents', result.stderr)
                self.assertEqual((self.backup/'database').read_text(), 'rollback state')
                self.assertTrue(extra.exists())
                self.assert_unknown_preserved()
                if kind == 'directory': extra.rmdir()
                else: extra.unlink()

    def test_upgrade_ignores_old_backup_location_but_preserves_explicit_override(self):
        install = self.root/'installed'
        install.mkdir()
        config_dir = self.root/'config'
        config_dir.mkdir()
        (config_dir/'service.json').write_text('{}')
        (install/'install.tsv').write_text('role\tagent\nserviceName\toss-transfer-agent\nconfig\t'+
                                          str(config_dir/'service.json')+'\nbackupDir\t/mnt/cfs/old-test-backup\n')
        binary = install/'oss-transfer-agent'
        binary.write_text('#!/bin/sh\nprintf "dataDirectory\\t%s/data\\n" "$TEST_ROOT"\n')
        binary.chmod(0o755)
        metadata = self.source[self.source.index('  installed_metadata=$install_dir/install.tsv'):self.source.index('\nrole=${role:-master}')]
        defaults = self.source[self.source.index('\nrole=${role:-master}'):self.source.index('\nfor path in "$install_dir"')]
        for override in ('', str(self.root/'chosen-backups')):
            with self.subTest(override=override):
                body = '''set -euo pipefail
install_dir=$TEST_ROOT/installed; backup_dir=$TEST_BACKUP_OVERRIDE
role=; service_name=; config_dir=; cli_path=; data_dir=
platform=linux; upgrade=1; user_service=0
fail() { printf '%s\\n' "$*" >&2; exit 1; }
safe_path() { return 0; }
systemctl() { return 0; }
metadata_field() { awk -F '\\t' -v key="$2" '$1==key {print $2}' "$1"; }
if true; then
''' + metadata + defaults + '\nprintf "%s\\n" "$backup_dir"\n'
                result = subprocess.run(['bash','-c',body], text=True, capture_output=True, timeout=5,
                                        env=dict(os.environ,TEST_ROOT=str(self.root),TEST_BACKUP_OVERRIDE=override))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), override or '/var/backups/oss-transfer/agent')


class BackupSpaceTests(unittest.TestCase):
    def test_directory_and_write_probe_failures_happen_before_service_stop(self):
        source = INSTALLER.read_text()
        functions = source[source.index('check_backup_space() {'):source.index('\nactive() {')]
        with tempfile.TemporaryDirectory(prefix='installer-backup-write-') as directory:
            root = Path(directory)
            blocked = root/'not-a-directory'
            blocked.write_text('leave this file unchanged')
            for backup_dir, shim, expected in (
                (blocked/'backups', '', 'Cannot create backup directory'),
                (root/'backups', 'mktemp() { return 7; }', 'Cannot write to backup directory'),
            ):
                with self.subTest(backup_dir=backup_dir):
                    body = '''set -euo pipefail
fail() { printf '%s\\n' "$*" >&2; exit 1; }
backup_dir=$TEST_BACKUP_DIR
database=$TEST_ROOT/missing.db; binary=$TEST_ROOT/missing-binary; cli_path=$TEST_ROOT/missing-cli
''' + functions + '\n' + shim + '\ncheck_backup_space\nprintf stopped > "$TEST_ROOT/stopped"\n'
                    result = subprocess.run(['bash','-c',body], text=True, capture_output=True, timeout=5,
                                            env=dict(os.environ,TEST_ROOT=str(root),TEST_BACKUP_DIR=str(backup_dir)))
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn(expected, result.stderr)
                    self.assertIn('The service has not been stopped', result.stderr)
                    self.assertFalse((root/'stopped').exists())
                    self.assertEqual(blocked.read_text(), 'leave this file unchanged')

    def test_space_checked_before_stop_and_existing_backups_retained(self):
        source = INSTALLER.read_text()
        self.assertLess(source.index('\n  check_backup_space\n'),
                        source.index('\n  stop_service\n'))
        functions = source[source.index('check_backup_space() {'):source.index('\nactive() {')]
        with tempfile.TemporaryDirectory(prefix='installer-backup-space-') as directory:
            root = Path(directory)
            (root/'bin').mkdir()
            database = root/'agent.db'
            # Sparse file catches accidental 32-bit size arithmetic without
            # allocating a large fixture or reading its contents.
            with database.open('wb') as stream:
                stream.truncate(5 * 1024**3)
            (root/'agent').write_text('binary')
            (root/'cli').write_text('cli')
            (root/'backups').mkdir()
            previous = root/'backups'/'previous'
            previous.write_text('only rollback copy')
            shim = root/'bin'/'df'
            shim.write_text('#!/bin/sh\nprintf "Filesystem 1024-blocks Used Available Capacity Mounted on\\n/dev/test 50000000 1 %s 90%% /\\n" "$TEST_AVAILABLE"\n')
            shim.chmod(0o755)
            body = '''set -euo pipefail
fail() { printf '%s\\n' "$*" >&2; exit 1; }
''' + functions + '''
backup_dir=$TEST_ROOT/backups/new; database=$TEST_ROOT/agent.db
binary=$TEST_ROOT/agent; cli_path=$TEST_ROOT/cli
check_backup_space
printf 'can stop service\\n'
'''
            env = dict(os.environ, PATH=str(root/'bin')+os.pathsep+os.environ['PATH'], TEST_ROOT=str(root))
            insufficient = subprocess.run(['bash','-c',body], env=dict(env,TEST_AVAILABLE=str(2*1024**2)),
                                          text=True,capture_output=True,timeout=5)
            self.assertNotEqual(insufficient.returncode, 0)
            self.assertIn('The service has not been stopped', insufficient.stderr)
            self.assertIn('5185 MiB', insufficient.stderr)
            self.assertNotIn('can stop service', insufficient.stdout)
            self.assertEqual(previous.read_text(), 'only rollback copy')
            self.assertTrue((root/'backups'/'new').is_dir())
            self.assertEqual(list((root/'backups'/'new').iterdir()), [])
            enough = subprocess.run(['bash','-c',body], env=dict(env,TEST_AVAILABLE=str(6*1024**2)),
                                    text=True,capture_output=True,timeout=5)
            self.assertEqual(enough.returncode, 0, enough.stderr)
            self.assertIn('can stop service', enough.stdout)
            invalid = subprocess.run(['bash','-c',body], env=dict(env,TEST_AVAILABLE='unknown'),
                                      text=True,capture_output=True,timeout=5)
            self.assertNotEqual(invalid.returncode, 0)
            self.assertIn('Cannot determine available backup space', invalid.stderr)


if __name__ == '__main__':
    unittest.main()
