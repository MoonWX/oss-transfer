#!/usr/bin/env python3
"""Deployment regressions using isolated executables; no live SSH or systemd."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


DEPLOY = Path(__file__).resolve().parents[1]


class DeploymentRuntimeTest(unittest.TestCase):
    def setUp(self):
        self.sandbox = tempfile.TemporaryDirectory(prefix="oss-deploy-test-")
        self.addCleanup(self.sandbox.cleanup)
        self.root = Path(self.sandbox.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.env = dict(os.environ)
        for name in list(self.env):
            if name.startswith("OSS_TRANSFER_") or name in ("JAVA_HOME", "BASH_ENV"):
                self.env.pop(name)
        self.env.update(PATH=f"{self.bin}:{os.environ['PATH']}", TEST_ROOT=str(self.root),
                        OSS_TRANSFER_INTERNAL_TOKEN="synthetic-secret-must-not-appear")
        # Control only discovery of the fixed Java path. Other filesystem checks
        # use real fixture files, including executable permission checks.
        startup = self.root / "bash-env"
        startup.write_text('''
function [() {
    if [[ "$#" == 3 && ( "$1" == -f || "$1" == -x ) && "$2" == /usr/bin/java ]]; then
        [[ "${TEST_DEFAULT_JAVA:-missing}" != missing ]]
    elif [[ "$#" == 4 && "$1" == ! && ( "$2" == -f || "$2" == -x ) && "$3" == /usr/bin/java ]]; then
        [[ "${TEST_DEFAULT_JAVA:-missing}" == missing ]]
    else
        builtin [ "$@"
    fi
}
function command() {
    if [[ "$#" == 2 && "$1" == -v && "$2" == "${TEST_MISSING_DEPENDENCY:-}" ]]; then
        return 1
    elif [[ "$#" == 2 && "$1" == -v && "$2" == java ]]; then
        if [[ "${TEST_PATH_JAVA:-missing}" != missing ]]; then printf '%s\\n' "$TEST_PATH_JAVA"; else return 1; fi
    else
        builtin command "$@"
    fi
}
''')
        self.env["BASH_ENV"] = str(startup)
        self.executable("uname", 'printf "%s\\n" "${TEST_ARCH:-x86_64}"')
        self.executable("timeout", '''
shift
if [ "$1" = /usr/bin/java ]; then
    case "${TEST_DEFAULT_JAVA:-missing}" in
        good) echo 'openjdk version "1.8.0_432"' >&2; exit 0 ;;
        *) exit 126 ;;
    esac
fi
exec "$@"
''')
        self.executable("systemctl", '''
printf 'systemctl %s\\n' "$*" >> "$TEST_ROOT/operations"
if [ "$1" = show ]; then
    case "${TEST_STATE:-ready}" in
        exec-failure) printf '%s\\n' ActiveState=activating SubState=auto-restart Result=exit-code ExecMainCode=1 ExecMainStatus=203 NRestarts=1 ;;
        app-failure) printf '%s\\n' ActiveState=failed SubState=failed Result=exit-code ExecMainCode=1 ExecMainStatus=1 NRestarts=0 ;;
        inaccessible) exit 1 ;;
        *) printf '%s\\n' ActiveState=active SubState=running Result=success MainPID=1234 ExecMainCode=0 ExecMainStatus=0 NRestarts=0 ;;
    esac
fi
''')
        self.executable("ss", '''
case "${TEST_LISTENER:-owned}" in
    owned) echo 'LISTEN 0 100 *:1785 *:* users:(("java",pid=1234,fd=4))' ;;
    ipv4) echo 'LISTEN 0 100 0.0.0.0:1785 0.0.0.0:* users:(("java",pid=1234,fd=4))' ;;
    ipv6) echo 'LISTEN 0 100 [::]:1785 [::]:* users:(("java",pid=1234,fd=4))' ;;
    other) echo 'LISTEN 0 100 *:1785 *:* users:(("other",pid=5678,fd=4))' ;;
    inaccessible) exit 1 ;;
esac
''')
        self.executable("curl", '''
printf 'curl %s\\n' "$*" >> "$TEST_ROOT/operations"
count=0
if [ -f "$TEST_ROOT/curl-count" ]; then read -r count < "$TEST_ROOT/curl-count"; fi
count=$((count + 1))
echo "$count" > "$TEST_ROOT/curl-count"
case "${TEST_HTTP:-ready}" in
    delayed) [ "$count" -ge 3 ] && printf 200 ;;
    unavailable) exit 7 ;;
    redirect) printf 302 ;;
    *) printf 200 ;;
esac
''')

    def executable(self, name, content):
        path = self.bin / name
        path.write_text("#!/bin/bash\nset -euo pipefail\n" + content + "\n")
        path.chmod(0o755)
        return path

    def java(self, name="java", version="1.8.0_432"):
        return self.executable(name, f'''echo 'openjdk version "{version}"' >&2''')

    def run_script(self, script, *args, success=True):
        result = subprocess.run(["bash", str(script), *map(str, args)], env=self.env,
                                text=True, capture_output=True, timeout=15)
        if success:
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        else:
            self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn(self.env["OSS_TRANSFER_INTERNAL_TOKEN"], result.stdout + result.stderr)
        return result

    def preflight(self, path="", success=True):
        return self.run_script(DEPLOY / "preflight-node.sh", path, success=success)

    def test_default_java_precedes_java_home_and_path(self):
        self.env.update(TEST_DEFAULT_JAVA="good", JAVA_HOME=str(self.root),
                        TEST_PATH_JAVA=str(self.java(version="17.0.12")))
        self.assertEqual(self.preflight().stdout, "x86_64\t/usr/bin/java\t8\n")

    def test_java_home_fallback_uses_real_executable(self):
        self.env.update(TEST_DEFAULT_JAVA="broken", JAVA_HOME=str(self.root))
        path = self.java(version="11.0.25")
        self.assertEqual(self.preflight().stdout, f"x86_64\t{path}\t11\n")

    def test_path_fallback_and_versioned_override(self):
        java = self.java(name="java-17.0.12+7", version="17.0.12")
        self.env["TEST_PATH_JAVA"] = str(java)
        self.assertEqual(self.preflight().stdout, f"x86_64\t{java}\t17\n")
        self.env["TEST_DEFAULT_JAVA"] = "good"
        self.assertEqual(self.preflight(java).stdout, f"x86_64\t{java}\t17\n")

    def test_missing_java(self):
        self.assertIn("未找到可运行的 Java 8+", self.preflight(success=False).stderr)

    def test_missing_readiness_dependency(self):
        self.env["TEST_DEFAULT_JAVA"] = "good"
        for dependency in ("curl", "ss", "systemctl", "timeout"):
            self.env["TEST_MISSING_DEPENDENCY"] = dependency
            self.assertIn(f"缺少 {dependency}", self.preflight(success=False).stderr)

    def test_explicit_invalid_java_does_not_fall_back(self):
        self.env["TEST_DEFAULT_JAVA"] = "good"
        result = self.preflight(self.root / "absent-java", success=False)
        self.assertIn("未回退", result.stderr)
        java = self.java()
        java.chmod(0o644)
        self.assertIn("不可执行", self.preflight(java, success=False).stderr)

    def test_java_exec_failure_and_unsupported_version(self):
        java = self.executable("wrong-architecture-java", "exit 126")
        self.assertIn("Java 执行失败", self.preflight(java, success=False).stderr)
        self.assertIn("版本不支持", self.preflight(self.java(version="1.7.0_80"), success=False).stderr)
        self.assertIn("无法识别", self.preflight(self.java(version="unknown"), success=False).stderr)

    def test_architecture_and_path_validation(self):
        self.env["TEST_ARCH"] = "riscv64"
        self.assertIn("不支持的架构", self.preflight(success=False).stderr)
        self.env["TEST_ARCH"] = "aarch64"
        self.assertTrue(self.preflight(self.java()).stdout.startswith("aarch64\t"))
        for path in ("java", "/opt/jdk with spaces/bin/java", "/tmp/java;false", "/tmp/java%h"):
            self.assertIn("绝对路径", self.preflight(path, success=False).stderr)

    def ready(self, timeout=5, success=True):
        return self.run_script(DEPLOY / "wait-node-ready.sh", "10.1.0.10", 1785,
                               timeout, success=success)

    def test_delayed_http_readiness(self):
        self.env["TEST_HTTP"] = "delayed"
        self.assertIn("已就绪", self.ready().stdout)
        self.assertEqual((self.root / "curl-count").read_text().strip(), "3")
        self.assertIn("http://10.1.0.10:1785/api/auth/current", (self.root / "operations").read_text())

    def test_service_exec_failure_is_immediate_and_actionable(self):
        self.env["TEST_STATE"] = "exec-failure"
        result = self.ready(success=False)
        self.assertIn("203/EXEC", result.stderr)
        self.assertIn("10.1.0.10", result.stderr)
        self.assertFalse((self.root / "curl-count").exists())

    def test_app_failure_and_unavailable_systemd(self):
        for state in ("app-failure", "inaccessible"):
            self.env["TEST_STATE"] = state
            self.assertIn("启动失败", self.ready(success=False).stderr)

    def test_http_timeout_is_bounded(self):
        self.env["TEST_HTTP"] = "unavailable"
        self.assertIn("超过 1 秒", self.ready(timeout=1, success=False).stderr)

    def test_other_listener_cannot_satisfy_readiness(self):
        self.env["TEST_LISTENER"] = "other"
        self.assertIn("其他进程占用", self.ready(success=False).stderr)
        self.assertFalse((self.root / "curl-count").exists())

    def test_ipv4_and_ipv6_listener_formats(self):
        for listener in ("ipv4", "ipv6"):
            self.env["TEST_LISTENER"] = listener
            self.assertIn("已就绪", self.ready().stdout)

    def test_missing_listener_or_redirect_is_not_ready(self):
        self.env["TEST_LISTENER"] = "absent"
        self.assertIn("超过 1 秒", self.ready(timeout=1, success=False).stderr)
        self.env.update(TEST_LISTENER="owned", TEST_HTTP="redirect")
        self.assertIn("超过 1 秒", self.ready(timeout=1, success=False).stderr)

    def test_listener_check_failure(self):
        self.env["TEST_LISTENER"] = "inaccessible"
        self.assertIn("无法检查监听端口", self.ready(success=False).stderr)



if __name__ == "__main__":
    unittest.main(verbosity=2)
