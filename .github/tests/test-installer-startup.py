#!/usr/bin/env python3
"""Exercise native installer port ownership and startup failure handling."""
import os
import importlib.util
from pathlib import Path
import shutil
import socket
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
INSTALLER = ROOT / "deploy/native/install.sh"
PUBLIC_INSTALLER = ROOT / "install.sh"


def installer_say(source):
    """Keep isolated installer sections wired to their real message helper."""
    return next(line for line in source.splitlines() if line.startswith('say() {')) + '\n'


class InstallerGuidanceTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        spec=importlib.util.spec_from_file_location('installer_acceptance',ROOT/'.github/acceptance/install.py')
        cls.acceptance=importlib.util.module_from_spec(spec)
        spec.loader.exec_module(cls.acceptance)

    def test_all_role_upgrade_finds_master_after_agent(self):
        cases=(
            ('/tmp/custom Master/oss-transfer-cli',False,
             'Tasks: /tmp/agent/oss-transfer-cli task list\nTasks: /tmp/custom\\ Master/oss-transfer-cli task list\n'),
            ("C:\\custom Master's CLI\\oss-transfer-cli.exe",True,
             "Tasks: & 'C:\\agent\\oss-transfer-cli.exe' task list\nTasks: & 'C:\\custom Master''s CLI\\oss-transfer-cli.exe' task list\n"),
        )
        for path,windows,output in cases:
            with self.subTest(windows=windows):
                self.assertTrue(self.acceptance.has_task_guidance(output,path,windows))
                self.assertTrue(self.acceptance.has_task_guidance('\n'.join(reversed(output.splitlines())),path,windows))

    def test_missing_or_wrong_local_command_is_rejected(self):
        for output in ('', 'Tasks: /tmp/agent/oss-transfer-cli task list',
                       'Tasks: /tmp/master/oss-transfer-cli --host http://localhost:1685 task list',
                       'Tasks: /tmp/master/oss-transfer-cli task list-all'):
            with self.subTest(output=output):
                self.assertFalse(self.acceptance.has_task_guidance(output,'/tmp/master/oss-transfer-cli'))


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
            ["bash", "-c", 'set -euo pipefail; release_url=; source "$1"; ' + body, "test", str(self.functions), value],
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
        self.functions.write_text(installer_say(source) + source[source.index("listener_address() {"):source.index("rollback() {")])
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

    def test_iproute_310_header_and_owner_format(self):
        # CentOS 7.0-7.4: ss has no -H, and the oldest builds omit pid=.
        self.shim("ss", 'case "$1" in -H*) exit 255;; esac; printf "%s" "$QA_SS"')
        header = 'State      Recv-Q Send-Q Local Address:Port               Peer Address:Port\n'
        self.check_port(header, True)
        result = self.check_port(header + 'LISTEN 0 50 *:1685 *:* users:(("java",412,22)) ino:7001248 sk:2 <->\n', False)
        self.assertIn('PID 412', result.stderr)
        own = header + 'LISTEN 0 128 :::1685 :::* users:(("master",812,5)) ino:6994875 sk:1 <->\n'
        self.check_port(own, True, QA_OWN_PID='812', QA_EXISTED='1')
        self.check_port(own, False, QA_OWN_PID='812', QA_EXISTED='0')
        self.check_port(own.replace('("master",812,5)', '("master",812,5),("foreign",813,7)'), False,
                        QA_OWN_PID='812', QA_EXISTED='1')

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

    def test_chinese_credentials_only_show_new_password(self):
        old = 'Initial password: old_password\n'
        result = self.run_shell('lang=zh; startup_log_lines=1; print_initial_credentials',
                                QA_LOG=old+'Initial password: new_password\n')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, '\n初始管理员: admin\n初始密码: new_password\n')
        restarted = self.run_shell('lang=zh; startup_log_lines=1; print_initial_credentials', QA_LOG=old)
        self.assertEqual(restarted.returncode, 0, restarted.stderr)
        self.assertEqual(restarted.stdout, '')

    def test_console_url_uses_actual_listener_and_custom_port(self):
        self.shim('hostname', 'printf "10.0.0.2 172.17.0.1\\n"')
        for binding, expected in (
            ('0.0.0.0:1688', 'http://10.0.0.2:1688'),
            ('[::]:1688', 'http://10.0.0.2:1688'),
            ('127.0.0.1:1689', 'http://127.0.0.1:1689'),
            ('[fd00::43]:1688', 'http://[fd00::43]:1688'),
        ):
            with self.subTest(binding=binding):
                result = self.run_shell('console_url', QA_LISTEN=binding)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), expected)

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


class SystemdCompatibilityTests(unittest.TestCase):
    """CentOS 7 / RHEL 7 (systemd 219, bash 4.2) is the oldest supported Linux."""
    # RestartPreventExitStatus predates systemd 219 (it is documented in 219).
    UNIT_219 = {'Description', 'After', 'Wants', 'Type', 'ExecStart', 'WorkingDirectory', 'Restart',
                'RestartSec', 'RestartPreventExitStatus', 'TimeoutStopSec', 'KillMode', 'UMask', 'WantedBy'}

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='installer-systemd-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = INSTALLER.read_text()
        self.helper = self.source[self.source.index('systemd_property() {'):self.source.index('\ndefault_root() {')]

    def property(self, output, name, status=0):
        # Like systemd 219, the stub rejects --value and prints NAME=VALUE.
        stub = ('systemctl() { [[ " $* " != *" --value "* ]] || { echo "unrecognized option --value" >&2; return 1; }\n'
                '  [[ "$1 $2 $3" == "show oss-test -p" ]] || return 9; printf "%b" "$QA_OUTPUT"; return "$QA_STATUS"; }\n')
        return subprocess.run(['bash', '-c', 'set -euo pipefail\n' + stub + self.helper + '\nsystemd_property oss-test "$1"', 'test', name],
                              env=dict(os.environ, QA_OUTPUT=output, QA_STATUS=str(status)),
                              text=True, capture_output=True, timeout=5)

    def test_property_parses_name_value_output(self):
        for output, name, expected in (
            ('LoadState=not-found\n', 'LoadState', 'not-found\n'),
            ('MainPID=0\n', 'MainPID', '0\n'),
            ('ExecMainPID=5\nMainPID=812\n', 'MainPID', '812\n'),
            ('DropInPaths=\n', 'DropInPaths', '\n'),
            ('DropInPaths=/etc/systemd/system/a.service.d/x=y.conf\n', 'DropInPaths', '/etc/systemd/system/a.service.d/x=y.conf\n'),
            ('', 'DropInPaths', ''),
        ):
            with self.subTest(output=output):
                result = self.property(output, name)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, expected)
        self.assertEqual(self.property('LoadState=loaded\n', 'LoadState', status=1).returncode, 1)

    def test_unix_installers_avoid_options_newer_than_systemd_219(self):
        for path in (INSTALLER, PUBLIC_INSTALLER):
            source = path.read_text()
            with self.subTest(path=path.name):
                self.assertNotIn('--value', source.replace('`systemctl show --value`', ''))
                self.assertNotIn('enable --now', source)
                self.assertNotIn('ss -H', source)
                # Bash 4.2 treats an empty "${array[@]}" as unset under set -u.
                self.assertNotIn('"${original_args[@]}"', source.replace('${original_args[@]+"${original_args[@]}"}', ''))
                # A Master URL may be an IPv6 literal; curl 7.29 globs brackets.
                for line in source.splitlines():
                    if line.lstrip().startswith('curl ') and ('"$release_url/' in line or '"$origin/' in line):
                        self.assertIn('--globoff', line)

    def test_systemd_unit_uses_only_systemd_219_directives(self):
        start = self.source.index('if [[ "$platform" == linux ]]; then\n  cat > "$unit" <<EOF\n')
        unit = self.source[self.source.index('\n', self.source.index('<<EOF', start)) + 1:self.source.index('\nEOF\n', start)]
        keys = {line.split('=', 1)[0] for line in unit.splitlines() if '=' in line}
        self.assertEqual(keys - self.UNIT_219, set(), 'gate newer directives on the systemd version')
        self.assertIn('ExecStart="$binary" --config "$config"', unit)


class StartupCommitTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='installer-commit-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = INSTALLER.read_text()
        self.function = installer_say(self.source)
        self.function += self.source[self.source.index('commit_start() {'):self.source.index('\nprint_initial_credentials() {')]
        self.function += '\n' + self.source[self.source.index('finish_agent_maintenance() {'):self.source.index('\nrollback() {')]

    def invoke(self, body='', fail=''):
        start = self.source.rindex('\nprepare_start\n')
        end = self.source.index("\nif ! publish_master_packages", start)
        finish = self.source[start:end]
        script = '''set -eEuo pipefail
trap 'printf rolled-back > "$TEST_ROOT/rollback"; exit 7' ERR
ready_file=$TEST_ROOT/ready; launch_id=''' + 'a' * 64 + '''
platform=linux; existed=1; had_unit=1; was_enabled=1; service_name=test; cli_migrated=0
prepare_start() { printf '%s\\n' "$launch_id" > "$ready_file.request"; }
agent_maintenance_started=0
start_service() { return 0; }
wait_ready() { [[ "$TEST_FAIL" != ready ]]; }
systemctl() { [[ "$TEST_FAIL" != enable ]]; }
finish_backup() { printf confirmed > "$TEST_ROOT/backup-finished"; }
''' + self.function + '\n' + body + '\n' + finish + '\nprintf installed\n'
        result = subprocess.run(['bash', '-c', script], text=True, capture_output=True, timeout=5,
                                env=dict(os.environ, TEST_ROOT=str(self.root), TEST_FAIL=fail))
        self.assertNotIn('command not found', result.stderr)
        return result

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


class AgentMaintenanceTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='installer-maintenance-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = INSTALLER.read_text()
        candidate = self.root / 'candidate'
        candidate.write_text('''#!/bin/bash
set -euo pipefail
case "$3" in
  --upgrade-maintenance) printf '%s\\n' "$4" >> "$TEST_ROOT/events"; exit "$TEST_MAINTENANCE_STATUS" ;;
  --backup-state) printf 'backup\\n' >> "$TEST_ROOT/events" ;;
  *) exit 99 ;;
esac
''')
        candidate.chmod(0o755)

    def invoke(self, body, status=0):
        script = '''set -euo pipefail
work=$TEST_ROOT; filename=candidate; config=$TEST_ROOT/service.json; backup=$TEST_ROOT/backup
role=agent; service_name=test-agent; agent_maintenance_started=0; enrollment_requested=0
stop_service() { printf 'stop\\n' >> "$TEST_ROOT/events"; }
rollback() { printf 'rollback\\n' >> "$TEST_ROOT/events"; exit 9; }
''' + installer_say(self.source) + body
        result = subprocess.run(['bash','-c',script], text=True, capture_output=True, timeout=5,
                                env=dict(os.environ, TEST_ROOT=str(self.root), TEST_MAINTENANCE_STATUS=str(status)))
        self.assertNotIn('command not found', result.stderr)
        return result

    def test_failed_maintenance_never_stops_agent_or_backs_up_database(self):
        start = self.source.index('  if [[ "$role" == agent && "$enrollment_requested" == 0 ]]; then\n    printf \'Protecting Agent tasks')
        end = self.source.index('\nelif [[ -n "$adopt_config" ]]; then', start)
        section = self.source[start:end]
        failed = self.invoke(section, status=7)
        self.assertEqual(failed.returncode, 7, failed.stderr)
        self.assertEqual((self.root/'events').read_text().splitlines(), ['begin'])
        (self.root/'events').unlink()
        passed = self.invoke(section)
        self.assertEqual(passed.returncode, 0, passed.stderr)
        self.assertEqual((self.root/'events').read_text().splitlines(), ['begin','stop','backup'])

    def test_explicit_rejoin_backs_up_without_authenticating_with_revoked_identity(self):
        start = self.source.index('  if [[ "$role" == agent && "$enrollment_requested" == 0 ]]; then\n    printf \'Protecting Agent tasks')
        end = self.source.index('\nelif [[ -n "$adopt_config" ]]; then', start)
        result = self.invoke('enrollment_requested=1\n' + self.source[start:end], status=7)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root/'events').read_text().splitlines(), ['stop','backup'])

    def test_failed_end_warns_without_rolling_back_healthy_service(self):
        start = self.source.index('finish_agent_maintenance() {')
        end = self.source.index('\nrollback() {',start)
        section = self.source[start:end]
        body = section + '\nagent_maintenance_started=1\nfinish_agent_maintenance\nprintf healthy\\n\n'
        failed = self.invoke(body,status=7)
        self.assertEqual(failed.returncode,0,failed.stderr)
        self.assertIn('expires automatically',failed.stderr)
        self.assertEqual((self.root/'events').read_text().splitlines(),['end'])
        self.assertIn('healthy',failed.stdout)

    def test_end_only_follows_successful_readiness(self):
        start=self.source.rindex('\nprepare_start\n')
        end=self.source.index("\nif ! publish_master_packages",start)
        section=self.source[start:end]
        body='''
platform=linux; existed=1; had_unit=1; was_enabled=0; cli_migrated=0
prepare_start() { return 0; }
start_service() { return 0; }
wait_ready() { return 7; }
finish_agent_maintenance() { printf 'end\\n' >> "$TEST_ROOT/events"; }
commit_start() { return 0; }
finish_backup() { return 0; }
trap rollback ERR INT TERM
'''+section
        result=self.invoke(body)
        self.assertNotEqual(result.returncode,0)
        self.assertEqual((self.root/'events').read_text().splitlines(),['rollback'])


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
        completion = source.index('\nif commit_start; then\n')
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
        functions = installer_say(source)
        functions += source[source.index('maintain_backups() {'):source.index('\ncheck_backup_space() {')]
        functions += '\n' + source[source.index('commit_start() {'):source.index('\nprint_initial_credentials() {')]
        functions += '\n' + source[source.index('finish_agent_maintenance() {'):source.index('\nrollback() {')]
        functions += '\n' + source[source.index('cleanup() {'):source.index('\ntrap cleanup EXIT')]
        start = source.rindex('\nprepare_start\n')
        finish = source[start:source.index("\nif ! publish_master_packages", start)]
        body = '''set -eEuo pipefail
work=$TEST_ROOT/work; filename=candidate
backup_dir=$TEST_ROOT/backups; backup=$TEST_BACKUP; owned_backup=$backup
config=$TEST_ROOT/service.json; ready_file=$TEST_ROOT/ready
existed=$TEST_EXISTED; service_name=oss-transfer-agent; cli_migrated=0
platform=linux; had_unit=1; was_enabled=1
launch_id=''' + 'b' * 64 + '''
prepare_start() { printf '%s\\n' "$launch_id" > "$ready_file.request"; }
agent_maintenance_started=0
start_service() { return 0; }
wait_ready() { [[ "$TEST_FAIL" != ready ]]; }
systemctl() { [[ "$TEST_FAIL" != enable ]]; }
mv() { [[ "$TEST_FAIL" != commit ]] && command mv "$@"; }
rollback() { printf rollback > "$TEST_ROOT/rollback"; exit 7; }
''' + functions + '\ntrap cleanup EXIT\ntrap rollback ERR INT TERM\n' + finish + '\nprintf installed\n'
        result = subprocess.run(['bash','-c',body], text=True, capture_output=True, timeout=5,
                                env=dict(os.environ, TEST_ROOT=str(self.root), TEST_BACKUP=str(self.backup),
                                         TEST_EXISTED=str(existed), TEST_FAIL=fail))
        self.assertNotIn('command not found', result.stderr)
        return result

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
        (self.backup/'database.key').write_bytes(bytes(range(32)))
        for name in ('cli.cluster.json','cli.local-auth.json','data-cli.cluster.json','data-cli.local-auth.json'):
            (self.backup/name).write_text('saved connection')
        result = self.invoke(0)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.backup.exists())
        self.assertFalse((self.root/'maintenance').exists())
        self.assert_unknown_preserved()

    def test_failed_or_unconfirmed_start_retains_and_prints_backup(self):
        key = bytes(range(32))
        (self.backup/'database.key').write_bytes(key)
        for existed in (0, 1):
            for failure in ('ready', 'enable', 'commit'):
                with self.subTest(existed=existed, failure=failure):
                    self.work.mkdir(exist_ok=True)
                    result = self.invoke(existed, failure)
                    self.assertEqual(result.returncode, 0 if failure == 'commit' else 7, result.stderr)
                    self.assertEqual((self.backup/'database').read_text(), 'rollback state')
                    self.assertEqual((self.backup/'database.key').read_bytes(), key)
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
platform=linux; upgrade=1; user_service=0; root_dir=/opt/oss-transfer; root_explicit=0
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


class UnifiedInstallationRootTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='installer-unified-root-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.cluster = self.root/'custom cluster'
        self.install = self.cluster/'agent/bin'
        self.config = self.cluster/'agent/config'
        self.data = self.cluster/'agent/data'
        for directory in (self.install, self.config, self.data):
            directory.mkdir(parents=True)
        (self.config/'service.json').write_text('{}')
        binary = self.install/'oss-transfer-agent'
        binary.write_text('#!/bin/sh\nprintf "dataDirectory\\t%s\\n" "$TEST_DATA"\n')
        binary.chmod(0o755)
        self.source = INSTALLER.read_text()

    def upgrade_paths(self, marker=True, platform='linux', user=False, backup='', explicit_root=''):
        metadata = ('role\tagent\nserviceName\toss-transfer-agent\nconfig\t'+
                    str(self.config/'service.json')+'\ndataDir\t'+str(self.data)+
                    '\nbackupDir\t/mnt/cfs/old-test-backup\n')
        if marker:
            metadata += 'rootDir\t'+str(self.cluster if marker is True else marker)+'\n'
        (self.install/'install.tsv').write_text(metadata)
        saved = self.source[self.source.index('  installed_metadata=$install_dir/install.tsv'):self.source.index('\nrole=${role:-master}')]
        defaults = self.source[self.source.index('\nrole=${role:-master}'):self.source.index('\nfor path in "$install_dir"')]
        body = '''set -euo pipefail
install_dir=$TEST_INSTALL; backup_dir=$TEST_BACKUP
role=; service_name=; config_dir=; cli_path=; data_dir=
platform=$TEST_PLATFORM; upgrade=1; user_service=$TEST_USER
root_dir=$TEST_DEFAULT_ROOT; root_explicit=$TEST_ROOT_EXPLICIT
fail() { printf '%s\\n' "$*" >&2; exit 1; }
safe_path() { return 0; }
systemctl() { return 0; }
launchctl() { return 0; }
metadata_field() { awk -F '\\t' -v key="$2" '$1==key {print $2}' "$1"; }
if true; then
''' + saved + defaults + '\nprintf "%s\\n%s\\n%s\\n%s\\n" "$root_dir" "$backup_dir" "$config_dir" "$data_dir"\n'
        default = '/opt/oss-transfer' if platform == 'linux' else '/Library/Application Support/OSS Transfer'
        result = subprocess.run(['bash', '-c', body], text=True, capture_output=True, timeout=5,
                                env=dict(os.environ, TEST_INSTALL=str(self.install), TEST_DATA=str(self.data),
                                         TEST_BACKUP=str(backup), TEST_PLATFORM=platform, TEST_USER=str(int(user)),
                                         TEST_DEFAULT_ROOT=str(explicit_root or default),
                                         TEST_ROOT_EXPLICIT=str(int(bool(explicit_root)))))
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.splitlines()

    def test_discovered_managed_root_keeps_configuration_data_and_backup_together(self):
        for platform, user in (('linux', False), ('darwin', False), ('darwin', True)):
            with self.subTest(platform=platform, user=user):
                self.assertEqual(self.upgrade_paths(platform=platform, user=user), [
                    str(self.cluster), str(self.cluster/'agent/backups'), str(self.config), str(self.data)])

    def test_legacy_lookalike_without_root_marker_uses_local_backup_default(self):
        for platform, expected in (('linux', '/var/backups/oss-transfer/agent'),
                                   ('darwin', '/Library/Application Support/OSS Transfer/agent/backups')):
            with self.subTest(platform=platform):
                paths = self.upgrade_paths(marker=False, platform=platform)
                self.assertNotEqual(paths[0], str(self.cluster))
                self.assertEqual(paths[1:], [expected, str(self.config), str(self.data)])

    def test_wrong_root_marker_is_not_used(self):
        paths = self.upgrade_paths(marker=self.root/'other cluster')
        self.assertEqual(paths, ['/opt/oss-transfer', '/var/backups/oss-transfer/agent',
                                 str(self.config), str(self.data)])

    def test_explicit_overrides_take_priority_over_managed_root_defaults(self):
        backup = self.root/'selected backup'
        paths = self.upgrade_paths(backup=backup)
        self.assertEqual(paths[:2], [str(self.cluster), str(backup)])
        selected = self.root/'selected root'
        paths = self.upgrade_paths(explicit_root=selected)
        self.assertEqual(paths[0], str(selected))

    def test_metadata_marks_only_an_actual_unified_layout(self):
        start = self.source.index("printf 'role\\t%s\\nserviceName")
        end = self.source.index('\nif ((enrollment_requested)); then', start)
        writer = self.source[start:end]
        for split in ('', 'install', 'config', 'data'):
            with self.subTest(split=split):
                metadata = self.root/'generated.tsv'
                body = '''set -euo pipefail
role=agent; service_name=test-agent; version=4.1.22
root_dir=$TEST_ROOT; install_dir=$root_dir/agent/bin; config_dir=$root_dir/agent/config; data_dir=$root_dir/agent/data
backup_dir=/explicit/backup; unit=/unused/test.service; cli_path=/unused/oss-transfer-cli
metadata=$TEST_METADATA
case "$TEST_SPLIT" in install) install_dir=/legacy/bin;; config) config_dir=/legacy/config;; data) data_dir=/legacy/data;; esac
config=$config_dir/service.json
''' + writer
                result = subprocess.run(['bash', '-c', body], text=True, capture_output=True, timeout=5,
                                        env=dict(os.environ, TEST_ROOT=str(self.cluster), TEST_METADATA=str(metadata), TEST_SPLIT=split))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual('rootDir\t'+str(self.cluster)+'\n' in metadata.read_text(), not bool(split))


class ReinstallationTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='installer-recovery-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = INSTALLER.read_text()

    def discovery(self, role='master', service=''):
        start = self.source.index('config_field() {')
        end = self.source.index('\nif [[ -z "$release_url" && -z "$package_dir" ]]; then', start)
        body = '''set -euo pipefail
role=$TEST_ROLE; service_name=$TEST_SERVICE; root_dir=$TEST_ROOT/app
platform=linux; user_service=0; upgrade=0; uninstall=0; install_dir=; config_dir=; adopt_config=
fail() { printf '%s\\n' "$*" >&2; exit 1; }
safe_path() { return 0; }
metadata_field() { awk -F '\\t' -v key="$2" '$1==key {print $2}' "$1"; }
''' + self.source[start:end] + '\nprintf "%s\\n%s\\n%s\\n" "$install_dir" "$config_dir" "$upgrade"\n'
        return subprocess.run(['bash','-c',body], text=True, capture_output=True, timeout=5,
                              env=dict(os.environ,TEST_ROOT=str(self.root),TEST_ROLE=role,TEST_SERVICE=service))

    def test_existing_configuration_is_found_after_program_directory_removed(self):
        config = self.root/'app/master/config'
        config.mkdir(parents=True)
        (config/'service.json').write_text('{"serviceName":"existing-master","dataDirectory":"/existing/data"}')
        result = self.discovery()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines(), [str(self.root/'app/master/bin'), str(config), '1'])

    def test_existing_metadata_keeps_old_binary_directory(self):
        directory = self.root/'app/agent'
        directory.mkdir(parents=True)
        (directory/'install.tsv').write_text('role\tagent\nserviceName\texisting-agent\n')
        result = self.discovery('agent', 'existing-agent')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines(), [str(directory), '', '1'])
        wrong = self.discovery('agent', 'different-cluster-agent')
        self.assertEqual(wrong.returncode, 0, wrong.stderr)
        self.assertEqual(wrong.stdout.splitlines(), ['', '', '0'])

    def test_uninitialized_root_has_no_adoption(self):
        result = self.discovery()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines(), ['', '', '0'])

    def test_user_service_upgrade_stays_inside_explicit_root(self):
        home = self.root/'home'
        original = home/'Library/Application Support/OSS Transfer/master/bin'
        selected = self.root/'custom/master/bin'
        for directory in (original, selected):
            directory.mkdir(parents=True)
            (directory/'install.tsv').write_text('role\tmaster\nserviceName\tisolated-master\n')
        start = self.source.index('add_candidate() {')
        end = self.source.index('# BEGIN uninstall', start)
        body = '''set -euo pipefail
root_dir=$TEST_ROOT/custom; root_explicit=1; platform=darwin; user_service=1; role=; service_name=
metadata_field() { awk -F '\\t' -v key="$2" '$1==key {print $2}' "$1"; }
''' + self.source[start:end] + '\ndiscover_installations "$role" "$service_name"\nprintf "%s\\n" "${candidates[@]:-}"\n'
        env = dict(os.environ,TEST_ROOT=str(self.root),HOME=str(home))
        found = subprocess.run(['bash','-c',body],text=True,capture_output=True,env=env,timeout=5)
        self.assertEqual(found.returncode,0,found.stderr)
        self.assertEqual(found.stdout.strip(),str(selected))
        (selected/'install.tsv').unlink()
        absent = subprocess.run(['bash','-c',body],text=True,capture_output=True,env=env,timeout=5)
        self.assertEqual(absent.returncode,0,absent.stderr)
        self.assertEqual(absent.stdout.strip(),'')

    def test_master_download_exception_requires_exact_invited_origin_and_path(self):
        start = self.source.index('trusted_master_release() {')
        end = self.source.index('\nif [[ -z "$release_url"', start)
        body = 'set -euo pipefail; role=$1; master_url=$2; release_url=$3;\n' + self.source[start:end] + '\ntrusted_master_release'
        for role, master, release, accepted in (
            ('agent','http://10.1.0.43:1685','http://10.1.0.43:1685/api/agents/bootstrap/abc_123-DEF/files',True),
            ('agent','http://[fd00::43]:1685','http://[fd00::43]:1685/api/agents/bootstrap/abc/files',True),
            ('master','http://10.1.0.43:1685','http://10.1.0.43:1685/api/agents/bootstrap/abc/files',False),
            ('agent','http://10.1.0.43:1685','http://10.1.0.44:1685/api/agents/bootstrap/abc/files',False),
            ('agent','http://10.1.0.43:1685','http://10.1.0.43:1686/api/agents/bootstrap/abc/files',False),
            ('agent','http://10.1.0.43:1685','http://10.1.0.43:1685/releases/v4.1.21',False),
            ('agent','http://10.1.0.43:1685','http://10.1.0.43:1685/api/agents/bootstrap/../files',False),
            ('agent','http://10.1.0.43:1685','http://10.1.0.43:1685/api/agents/bootstrap/abc/../files',False),
            ('agent','http://10.1.0.43:1685','http://10.1.0.43:1685/api/agents/bootstrap/abc/files?next=evil',False),
        ):
            with self.subTest(role=role, release=release):
                result = subprocess.run(['bash','-c',body,'test',role,master,release],text=True,capture_output=True,timeout=5)
                self.assertEqual(result.returncode == 0, accepted, result.stderr)


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
database=$TEST_ROOT/missing.db; binary=$TEST_ROOT/missing-binary; cli_path=$TEST_ROOT/missing-cli; data_dir=$TEST_ROOT/data
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
binary=$TEST_ROOT/agent; cli_path=$TEST_ROOT/cli; data_dir=$TEST_ROOT/data
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



class CLIPlacementTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='installer-cli-placement-')
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.install = self.root/'agent'
        self.install.mkdir(mode=0o700)
        self.old = self.install/'oss-transfer-cli'
        self.global_cli = self.root/'global'/'oss-transfer-cli'
        self.global_cli.parent.mkdir()
        self.data = self.root/'data'
        self.data.mkdir(mode=0o700)
        self.source = INSTALLER.read_text()
        self.function = self.source[self.source.index('default_cli_path() {'):self.source.index('\ncli_path=${cli_path:-$(default_cli_path')]

    def selection(self, installed='', role='agent'):
        result = subprocess.run(['bash','-c','set -euo pipefail\n'+self.function+
                                 '\nrole=$1; install_dir=$2; global_cli=$3; default_cli_path "$4"',
                                 'test',role,str(self.install),str(self.global_cli),str(installed)],
                                text=True,capture_output=True,timeout=5)
        self.assertEqual(result.returncode,0,result.stderr)
        return result.stdout.strip()

    def test_agent_first_install_and_existing_role_cli_use_free_global_path(self):
        self.assertEqual(self.selection(),str(self.global_cli))
        self.old.write_text('previous agent CLI')
        self.assertEqual(self.selection(self.old),str(self.global_cli))
        self.assertEqual(self.old.read_text(),'previous agent CLI')
        self.assertEqual(self.install.stat().st_mode & 0o777,0o700)

    def test_existing_master_cli_and_stale_sidecar_are_not_overwritten(self):
        self.global_cli.write_text('Master CLI')
        self.assertEqual(self.selection(),str(self.old))
        self.assertEqual(self.selection(self.old),str(self.old))
        self.assertEqual(self.global_cli.read_text(),'Master CLI')
        self.global_cli.unlink()
        Path(str(self.global_cli)+'.cluster.json').write_text('existing cluster')
        self.assertEqual(self.selection(self.old),str(self.old))
        Path(str(self.global_cli)+'.cluster.json').unlink()
        Path(str(self.global_cli)+'.local-auth.json').write_text('existing private grant')
        self.assertEqual(self.selection(self.old),str(self.old))

    def test_custom_cli_and_master_existing_role_path_stay_unchanged(self):
        custom=self.root/'custom'/'oss-transfer-cli'
        self.assertEqual(self.selection(custom),str(custom))
        self.assertEqual(self.selection(self.old,'master'),str(self.old))

    def test_failed_migration_removes_only_new_global_files(self):
        self.old.write_text('previous agent CLI')
        Path(str(self.old)+'.local-auth.json').write_text('previous private grant')
        self.global_cli.write_text('new CLI')
        Path(str(self.global_cli)+'.cluster.json').write_text('new cluster')
        Path(str(self.global_cli)+'.local-auth.json').write_text('new private grant')
        rollback=self.source[self.source.index('backup_cli_sidecars() {'):self.source.index('\ncheck_listener\n')]
        body=r"""set -euo pipefail
existed=0; cli_changed=1; had_cli=0; cli_migrated=1; platform=linux; adopt_config=; enrollment_attempted=0
cli_path=$1; previous_cli=$2; unit=$3/unit; backup=$3/backup; log_file=$3/log; database=$3/missing.db; data_dir=$3/data
systemctl() { return 0; }
stop_service() { return 0; }
"""+rollback+'\nfalse || rollback\n'
        result=subprocess.run(['bash','-c',body,'test',str(self.global_cli),str(self.old),str(self.root)],
                              text=True,capture_output=True,timeout=5)
        self.assertEqual(result.returncode,1,result.stderr)
        self.assertEqual(self.old.read_text(),'previous agent CLI')
        self.assertFalse(self.global_cli.exists())
        self.assertFalse(Path(str(self.global_cli)+'.cluster.json').exists())
        self.assertFalse(Path(str(self.global_cli)+'.local-auth.json').exists())
        self.assertEqual(Path(str(self.old)+'.local-auth.json').read_text(),'previous private grant')

    def connection_functions(self):
        return self.source[self.source.index('backup_cli_sidecars() {'):self.source.index('\nrollback() {')]

    def run_connections(self, body):
        return subprocess.run(['bash','-c','set -euo pipefail\ncli_path=$1; backup=$2; data_dir=$3\n'+self.connection_functions()+'\n'+body,
                               'test',str(self.global_cli),str(self.root/'backup'),str(self.data)],
                              text=True,capture_output=True,timeout=5)

    def test_failed_upgrade_restores_connections_and_private_permissions(self):
        (self.root/'backup').mkdir(mode=0o700)
        public=Path(str(self.global_cli)+'.cluster.json')
        private=Path(str(self.global_cli)+'.local-auth.json')
        public.write_text('previous cluster')
        private.write_text('previous private grant')
        (self.data/'cli-cluster.json').write_text('previous data cluster')
        (self.data/'cli-local-auth.json').write_text('previous data grant')
        result=self.run_connections('backup_cli_sidecars\nprintf new-cluster > "$cli_path.cluster.json"\nprintf new-grant > "$cli_path.local-auth.json"\nprintf new-cluster > "$data_dir/cli-cluster.json"\nprintf new-grant > "$data_dir/cli-local-auth.json"\nrestore_cli_sidecars')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(public.read_text(),'previous cluster')
        self.assertEqual(private.read_text(),'previous private grant')
        self.assertEqual(private.stat().st_mode & 0o777,0o600)
        self.assertEqual((self.root/'backup'/'cli.local-auth.json').stat().st_mode & 0o777,0o600)
        self.assertEqual((self.root/'backup'/'data-cli.local-auth.json').stat().st_mode & 0o777,0o600)
        self.assertEqual((self.data/'cli-cluster.json').read_text(),'previous data cluster')
        self.assertEqual((self.data/'cli-local-auth.json').read_text(),'previous data grant')
        self.assertEqual((self.data/'cli-local-auth.json').stat().st_mode & 0o777,0o600)
        self.assertEqual(list(self.global_cli.parent.glob('*.restore.*')),[])

    def test_rollback_to_pre_auth_version_removes_only_new_private_grant(self):
        (self.root/'backup').mkdir(mode=0o700)
        public=Path(str(self.global_cli)+'.cluster.json')
        private=Path(str(self.global_cli)+'.local-auth.json')
        public.write_text('previous cluster')
        result=self.run_connections('backup_cli_sidecars\nprintf new-cluster > "$cli_path.cluster.json"\nprintf new-grant > "$cli_path.local-auth.json"\nprintf new-cluster > "$data_dir/cli-cluster.json"\nprintf new-grant > "$data_dir/cli-local-auth.json"\nrestore_cli_sidecars')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertEqual(public.read_text(),'previous cluster')
        self.assertFalse(private.exists())
        self.assertFalse((self.data/'cli-cluster.json').exists())
        self.assertFalse((self.data/'cli-local-auth.json').exists())

    def test_sidecar_backup_and_restore_reject_symlinks(self):
        (self.root/'backup').mkdir(mode=0o700)
        target=self.root/'unrelated'
        target.write_text('must stay untouched')
        private=Path(str(self.global_cli)+'.local-auth.json')
        private.symlink_to(target)
        for action in ('backup_cli_sidecars','restore_cli_sidecars'):
            result=self.run_connections(action)
            self.assertNotEqual(result.returncode,0)
            self.assertTrue(private.is_symlink())
            self.assertEqual(target.read_text(),'must stay untouched')
        private.unlink()
        (self.root/'backup'/'cli.local-auth.json').symlink_to(target)
        result=self.run_connections('restore_cli_sidecars')
        self.assertNotEqual(result.returncode,0)
        self.assertFalse(private.exists())
        self.assertEqual(target.read_text(),'must stay untouched')

    def test_old_cli_removed_only_after_startup_is_committed(self):
        start=self.source.index('\nif commit_start; then\n')
        finish=self.source[start:self.source.index("\nif ! publish_master_packages",start)]
        for committed in (False,True):
            self.old.write_text('previous agent CLI')
            Path(str(self.old)+'.cluster.json').write_text('previous cluster')
            Path(str(self.old)+'.local-auth.json').write_text('previous private grant')
            body='set -euo pipefail\ncli_migrated=1; previous_cli=$1\ncommit_start() { return '+('0' if committed else '1')+'; }\nfinish_backup() { return 0; }\n'+finish
            result=subprocess.run(['bash','-c',body,'test',str(self.old)],text=True,capture_output=True,timeout=5)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertEqual(self.old.exists(),not committed)
            self.assertEqual(Path(str(self.old)+'.cluster.json').exists(),not committed)
            self.assertEqual(Path(str(self.old)+'.local-auth.json').exists(),not committed)


if __name__ == '__main__':
    unittest.main()
