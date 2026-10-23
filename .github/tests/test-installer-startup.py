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


class BackupSpaceTests(unittest.TestCase):
    def test_space_checked_before_stop_and_existing_backups_retained(self):
        source = INSTALLER.read_text()
        self.assertLess(source.index('\nif ((existed)); then check_backup_space; fi'),
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
            self.assertFalse((root/'backups'/'new').exists())
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
