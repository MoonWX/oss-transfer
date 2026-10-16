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


if __name__ == '__main__':
    unittest.main()
