#!/usr/bin/env python3
"""Exercise deploy/manual/oss-transfer-run.sh with stand-in programs.

The stand-ins print what the real programs print and record how they were
started; nothing outside a temporary directory is touched.
"""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/manual/oss-transfer-run.sh"

FAKE_MASTER = r'''#!/usr/bin/env bash
data='' listen=''
while (($#)); do case "$1" in --data-dir) data=$2; shift 2;; --listen) listen=$2; shift 2;; *) shift;; esac; done
printf '%s\n' "$listen" >> "$data/listen-calls"
printf '%s\n' "${OSS_TRANSFER_LAUNCHER:-}" > "$data/launcher"
if [[ -f "$data/fail" ]]; then echo "listen tcp $listen: bind: address already in use" >&2; exit 1; fi
# A slow first start: the password and the listening line come late.
if [[ -f "$data/delay" ]]; then sleep "$(cat "$data/delay")"; fi
if [[ ! -f "$data/master.db" ]]; then
  : > "$data/master.db"
  printf 'Initial administrator: admin\nInitial password: Initial%s\n' "$(cat "$data/listen-calls" | wc -l | tr -d ' ')"
fi
echo "Master listening on $listen"
sleep 3600 & trap 'kill $! 2>/dev/null; exit 0' TERM; wait
'''

FAKE_AGENT = r'''#!/usr/bin/env bash
data='' join='' enroll=0
while (($#)); do case "$1" in --data-dir) data=$2; shift 2;; --join-url) join=$2; shift 2;; --enroll-only) enroll=1; shift;; *) shift;; esac; done
if ((enroll)); then
  printf '%s\n' "$join" >> "$data/join-calls"
  : > "$data/agent.db"
  case "$(cat "$data/enroll-result" 2>/dev/null)" in
    403) echo "enroll Agent: HTTP 403: Invalid or expired Agent invitation" >&2; exit 1;;
    409) echo "enroll Agent: HTTP 409: Agent identity or address is already registered" >&2; exit 1;;
    network) echo 'enroll Agent: Post "http://master.invalid/api/agents/join": dial tcp: lookup master.invalid: no such host' >&2; exit 1;;
  esac
  echo "Agent enrolled: node-1"; exit 0
fi
printf '%s\n' "${OSS_TRANSFER_LAUNCHER:-}" > "$data/launcher"
sleep 3600 & trap 'kill $! 2>/dev/null; exit 0' TERM; wait
'''


class RunScriptTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="run-script-")
        self.root = Path(self.temporary.name)
        self.script = self.root / "oss-transfer-run.sh"
        shutil.copy(SCRIPT, self.script)
        for role, source in (("master", FAKE_MASTER), ("agent", FAKE_AGENT)):
            program = self.root / f"oss-transfer-{role}"
            program.write_text(source)
            program.chmod(0o755)
        self.unrelated = []
        self.addCleanup(self.cleanup)

    def cleanup(self):
        for role in ("master", "agent"):
            if (self.root / "data" / role / "run").exists():
                self.run_script(role, "stop")
        for process in self.unrelated:
            process.kill()
            process.wait()
        self.temporary.cleanup()

    def data(self, role):
        path = self.root / "data" / role
        path.mkdir(parents=True, exist_ok=True)
        return path

    def run_script(self, role, action, *options):
        environment = {key: value for key, value in os.environ.items() if key != "OSS_TRANSFER_LAUNCHER"}
        return subprocess.run(["bash", str(self.script), role, action, *options], cwd=self.root, env=environment,
                              encoding="utf-8", errors="backslashreplace", capture_output=True, timeout=120)

    def unrelated_process(self):
        process = subprocess.Popen(["sleep", "300"])
        self.unrelated.append(process)
        return process

    def test_join_link_is_used_when_the_data_directory_already_exists(self):
        data = self.data("agent")
        (data / "agent.db").write_text("identity of a removed node")
        result = self.run_script("agent", "start", "--join-url", "http://master:1685/api/agents/bootstrap/new?lang=zh")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual((data / "join-calls").read_text(), "http://master:1685/api/agents/bootstrap/new\n")
        self.assertEqual(self.run_script("agent", "status").returncode, 0)

    def test_running_agent_does_not_silently_drop_a_join_link(self):
        self.assertEqual(self.run_script("agent", "start", "--join-url", "http://master:1685/api/agents/bootstrap/a").returncode, 0)
        result = self.run_script("agent", "start", "--join-url", "http://master:1685/api/agents/bootstrap/b")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("restart --join-url", result.stderr)
        self.assertEqual((self.data("agent") / "join-calls").read_text().splitlines(), ["http://master:1685/api/agents/bootstrap/a"])

    def test_running_agent_accepts_the_link_it_joined_with(self):
        link = "http://master:1685/api/agents/bootstrap/a"
        self.assertEqual(self.run_script("agent", "start", "--join-url", link + "?lang=zh").returncode, 0)
        # An ensure-running job repeats the command it joined with.
        again = self.run_script("agent", "start", "--join-url", link)
        self.assertEqual(again.returncode, 0, again.stdout + again.stderr)
        self.assertIn("已在运行", again.stdout)
        self.assertEqual((self.data("agent") / "join-calls").read_text().splitlines(), [link])
        # The digest of the link is kept, not the link and its token.
        self.assertNotIn("bootstrap", (self.data("agent") / "run" / "joined").read_text())

    def test_joined_agent_starts_when_the_master_cannot_be_reached(self):
        # For example @reboot with the join command, and the Agent's host
        # boots before the Master's.
        data = self.data("agent")
        link = "http://master:1685/api/agents/bootstrap/a"
        self.assertEqual(self.run_script("agent", "start", "--join-url", link).returncode, 0)
        self.assertEqual(self.run_script("agent", "stop").returncode, 0)
        (data / "enroll-result").write_text("network")
        result = self.run_script("agent", "start", "--join-url", link)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("no such host", result.stderr)
        self.assertIn("本机已用此链接加入，Agent 照常启动", result.stderr)
        self.assertIn("connects once the Master can be reached", result.stderr)
        self.assertEqual(self.run_script("agent", "status").returncode, 0)
        self.assertEqual(self.run_script("agent", "stop").returncode, 0)
        # A link it has not joined with is not confirmed: nothing starts.
        other = self.run_script("agent", "start", "--join-url", "http://master:1685/api/agents/bootstrap/b")
        self.assertEqual(other.returncode, 1, other.stdout)
        self.assertIn("无法连接 Master", other.stderr)
        self.assertNotEqual(self.run_script("agent", "status").returncode, 0)
        # An answer that refuses the link it joined with (a removed node) stops the start.
        (data / "enroll-result").write_text("403")
        refused = self.run_script("agent", "start", "--join-url", link)
        self.assertEqual(refused.returncode, 1, refused.stdout)
        self.assertIn("生成新的加入链接", refused.stderr)
        self.assertNotEqual(self.run_script("agent", "status").returncode, 0)

    def test_refused_join_names_the_reason_and_what_to_do(self):
        cases = {
            "403": ("HTTP 403: Invalid or expired Agent invitation", "生成新的加入链接", "Create a new join link"),
            "409": ("HTTP 409: Agent identity or address is already registered", "先在控制台移除旧节点", "remove the old node"),
            "network": ("no such host", "无法连接 Master", "could not be reached"),
        }
        for outcome, expected in cases.items():
            with self.subTest(outcome=outcome):
                (self.data("agent") / "enroll-result").write_text(outcome)
                result = self.run_script("agent", "start", "--join-url", "http://master:1685/api/agents/bootstrap/x")
                self.assertEqual(result.returncode, 1, result.stdout)
                for text in expected:
                    self.assertIn(text, result.stderr)
                self.assertNotEqual(self.run_script("agent", "status").returncode, 0)

    def test_stale_pid_files_name_nothing_and_never_stop_other_processes(self):
        for role in ("master", "agent"):
            with self.subTest(role=role):
                data = self.data(role)
                if role == "agent":
                    (data / "agent.db").write_text("enrolled")
                other = self.unrelated_process()
                (data / "run").mkdir(exist_ok=True)
                (data / "run" / "supervisor.pid").write_text(f"{other.pid}\n")
                (data / "run" / f"{role}.pid").write_text(f"{other.pid}\n")
                status = self.run_script(role, "status")
                self.assertEqual(status.returncode, 3, status.stdout)
                self.assertIn("未运行", status.stdout)
                stop = self.run_script(role, "stop")
                self.assertIn("未运行", stop.stdout)
                self.assertIsNone(other.poll(), "stop signalled an unrelated process")
                (data / "run" / "supervisor.pid").write_text(f"{other.pid}\n")
                start = self.run_script(role, "start")
                self.assertEqual(start.returncode, 0, start.stdout + start.stderr)
                self.assertNotIn("已在运行", start.stdout)
                running = self.run_script(role, "status")
                self.assertEqual(running.returncode, 0, running.stdout)
                self.assertNotIn(f"pid {other.pid})", running.stdout)
                self.assertEqual(self.run_script(role, "stop").returncode, 0)
                self.assertIsNone(other.poll(), "stop signalled an unrelated process")
                self.assertEqual(self.run_script(role, "status").returncode, 3)

    def test_master_keeps_its_listen_address_and_prints_it(self):
        data = self.data("master")
        first = self.run_script("master", "start", "--listen", "127.0.0.1:18080")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        self.assertIn("127.0.0.1:18080", first.stdout)
        self.assertIn("http://127.0.0.1:18080", first.stdout)
        ignored = self.run_script("master", "start", "--listen", "0.0.0.0:18099")
        self.assertEqual(ignored.returncode, 1, ignored.stdout)
        self.assertIn("restart --listen 0.0.0.0:18099", ignored.stderr)
        self.assertEqual(self.run_script("master", "start", "--listen", "127.0.0.1:18080").returncode, 0)
        restart = self.run_script("master", "restart")
        self.assertEqual(restart.returncode, 0, restart.stdout + restart.stderr)
        self.assertIn("http://127.0.0.1:18080", restart.stdout)
        self.assertEqual(self.run_script("master", "stop").returncode, 0)
        self.assertEqual(self.run_script("master", "start").returncode, 0)
        self.assertEqual((data / "listen-calls").read_text().splitlines(), ["127.0.0.1:18080"] * 3)
        moved = self.run_script("master", "restart", "--listen", "0.0.0.0:18081")
        self.assertIn("127.0.0.1:18080", moved.stdout)
        self.assertIn("http://<本机IP / host IP>:18081", moved.stdout)
        self.assertEqual((data / "listen-calls").read_text().splitlines()[-1], "0.0.0.0:18081")

    def start_master_as_earlier_scripts_did(self, listen):
        """Starts the Master as the script did before it kept run/listen: the
        PID file holds only the supervisor's PID, and nothing records --listen."""
        data = self.data("master")
        for directory in ("run", "logs"):
            (data / directory).mkdir(exist_ok=True)
        log = data / "logs" / "service.log"
        loop = r'''binary=$1 log=$2 stopfile=$3 childfile=$4; shift 4
trap "kill -TERM \$child 2>/dev/null; wait \$child; exit 0" TERM INT
while [[ ! -f "$stopfile" ]]; do
  "$binary" "$@" >> "$log" 2>&1 & child=$!
  echo $child > "$childfile"
  wait $child && code=0 || code=$?
  rm -f "$childfile"
  [[ -f "$stopfile" || $code -eq 78 ]] && break
  sleep 5
done'''
        supervisor = subprocess.Popen(
            ["bash", "-c", loop, "oss-transfer-supervisor", str(self.root / "oss-transfer-master"), str(log),
             str(data / "run" / "stop"), str(data / "run" / "master.pid"), "--data-dir", str(data), "--listen", listen],
            stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        self.unrelated.append(supervisor)
        (data / "run" / "supervisor.pid").write_text(f"{supervisor.pid}\n")
        for _ in range(600):
            if (data / "run" / "master.pid").exists() and log.exists() and "Master listening on" in log.read_text():
                return
            time.sleep(0.1)
        self.fail("the Master of the earlier script did not start")

    def test_master_started_by_an_earlier_script_keeps_its_listen_address(self):
        data = self.data("master")
        self.start_master_as_earlier_scripts_did("127.0.0.1:18090")
        same = self.run_script("master", "start", "--listen", "127.0.0.1:18090")
        self.assertEqual(same.returncode, 0, same.stdout + same.stderr)
        self.assertIn("已在运行", same.stdout)
        self.assertEqual((data / "run" / "listen").read_text(), "127.0.0.1:18090\n")
        other = self.run_script("master", "start", "--listen", "0.0.0.0:18099")
        self.assertEqual(other.returncode, 1, other.stdout)
        self.assertIn("restart --listen 0.0.0.0:18099", other.stderr)
        restart = self.run_script("master", "restart")
        self.assertEqual(restart.returncode, 0, restart.stdout + restart.stderr)
        self.assertIn("http://127.0.0.1:18090", restart.stdout)
        self.assertEqual((data / "listen-calls").read_text().splitlines(), ["127.0.0.1:18090"] * 2)

    def test_restart_keeps_the_initial_password_valid_and_says_so(self):
        first = self.run_script("master", "start")
        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        self.assertIn("\nInitial1\n", first.stdout)
        restart = self.run_script("master", "restart")
        self.assertEqual(restart.returncode, 0, restart.stdout + restart.stderr)
        self.assertNotIn("管理员账号已存在", restart.stdout)
        self.assertIn("本次启动没有生成新的初始密码", restart.stdout)
        self.assertIn("仍然有效", restart.stdout)

    def test_slow_first_start_still_shows_the_initial_password(self):
        # Longer than the fixed four seconds the script used to wait.
        (self.data("master") / "delay").write_text("6")
        result = self.run_script("master", "start")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("\nInitial1\n", result.stdout)

    def test_master_that_cannot_start_is_reported_and_stopped(self):
        (self.data("master") / "fail").write_text("")
        result = self.run_script("master", "start")
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("address already in use", result.stderr)
        self.assertEqual(self.run_script("master", "status").returncode, 3)

    def test_programs_know_they_were_started_by_this_script(self):
        self.assertEqual(self.run_script("master", "start").returncode, 0)
        self.assertEqual((self.data("master") / "launcher").read_text().strip(), "run-script")
        self.assertEqual(self.run_script("agent", "start", "--join-url", "http://master:1685/api/agents/bootstrap/a").returncode, 0)
        self.assertEqual((self.data("agent") / "launcher").read_text().strip(), "run-script")


if __name__ == "__main__":
    unittest.main()
