#!/usr/bin/env python3
"""Run CLI administration against isolated native Master and Agent processes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import time


def eventually(check, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(0.1)
    raise AssertionError("Condition not met before timeout")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("master", "agent", "cli", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    root = args.output.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    environment = dict(os.environ)
    for name in list(environment):
        if name.startswith("OSS_TRANSFER_"):
            del environment[name]
    environment["OSS_TRANSFER_CLI_CONFIG_DIR"] = str(root / "cli-profile")
    # No local V4 deployment file may supply an internal credential.
    environment["OSS_TRANSFER_CONFIG_FILE"] = str(root / "absent-v4-config")
    environment["QA_PASSWORD"] = secrets.token_urlsafe(24)
    password = environment["QA_PASSWORD"]
    private_values = [password]
    processes, logs, checks = [], [], []
    origin = ""

    def spawn(name, executable, *arguments, extra_env=None):
        log = root / (name + ".log")
        stream = log.open("wb")
        if os.name != "nt":
            log.chmod(0o600)
        proc = subprocess.Popen([str(executable.resolve()), *arguments], cwd=root,
                                env=dict(environment, **(extra_env or {})), stdout=stream, stderr=subprocess.STDOUT)
        processes.append(proc)
        logs.append(stream)
        return proc, log

    def cli(*arguments, expected=0, input_text=None):
        result = subprocess.run([str(args.cli.resolve()), "--host", origin, "--json", *arguments],
                                cwd=root, env=environment, input=input_text, text=True,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=20, encoding="utf-8")
        if result.returncode != expected:
            # Do not include successful invitation output or secret inputs in diagnostics.
            raise AssertionError(f"CLI {arguments[0:2]} exited {result.returncode}, expected {expected}: {result.stderr}")
        value = json.loads(result.stdout)
        if arguments[:2] != ("agent", "invite"):
            if any(secret in result.stdout + result.stderr for secret in private_values):
                raise AssertionError("CLI exposed an input credential")
        return value

    try:
        master, log = spawn("master-first", args.master, "--listen", "127.0.0.1:0", "--data-dir", str(root / "master-data"))
        address = eventually(lambda: re.search(r"Master listening on (127\.0\.0\.1:\d+)", log.read_text(encoding="utf-8")))[1]
        origin = "http://" + address
        token = eventually(lambda: re.search(r"Setup token: ([A-Za-z0-9_-]+)", log.read_text(encoding="utf-8")))[1]
        private_values.append(token)
        environment["QA_SETUP_TOKEN"] = token
        assert cli("setup", "status")["configured"] is False
        cli("agent", "list", expected=1)
        cli("setup", "complete", "--username", "cli-qa", "--public-url", origin,
            "--setup-token-env", "QA_SETUP_TOKEN", "--password-env", "QA_PASSWORD")
        assert cli("setup", "status")["configured"] is True
        cli("setup", "complete", "--username", "replacement", "--public-url", origin,
            "--setup-token-env", "QA_SETUP_TOKEN", "--password-env", "QA_PASSWORD", expected=1)
        cli("login", "--username", "cli-qa", "--password-stdin", input_text=password + "\n")
        assert cli("whoami")["user"]["username"] == "cli-qa"
        checks.append("bootstrap-only setup, repeated setup rejection, stdin login and private persisted CLI session")

        invitation = cli("agent", "invite", "--address", "cli-node", "--label", "CLI Agent")["data"]
        private_values.append(invitation["inviteToken"])
        agent, _ = spawn("agent-first", args.agent, "--master-url", origin, "--data-dir", str(root / "agent-data"),
                         extra_env={"OSS_TRANSFER_JOIN_TOKEN": invitation["inviteToken"]})

        def ready_node():
            result = subprocess.run([str(args.cli.resolve()), "--host", origin, "--json", "agent", "show", "cli-node"],
                                    cwd=root, env=environment, capture_output=True, text=True, timeout=15, encoding="utf-8")
            if result.returncode != 0:
                return None
            data = json.loads(result.stdout)
            return data if data.get("status", {}).get("success") else None

        original = eventually(ready_node)
        node_id = original["node"]["id"]
        local_root = root / "data with spaces"
        local_root.mkdir()
        cli("agent", "update", "cli-node", "--label", "Renamed Agent", "--local-root", str(local_root))
        show = cli("agent", "show", "cli-node")
        assert show["node"]["label"] == "Renamed Agent"
        assert show["status"]["localStorage"]["advertisedLocalRoots"] == [str(local_root)]
        cli("limit", "set", "bandwidth", "12", "--unit", "Mbps", "--worker", "cli-node")
        show = cli("agent", "show", "cli-node")
        assert show["status"]["rateLimit"]["bandwidth"]["limitBps"] == 12_000_000
        rates = cli("limit", "list")["rateLimits"]
        assert rates[0]["workerIp"] == "cli-node" and rates[0]["bandwidth"]["limitBps"] == 12_000_000
        history = cli("limit", "history", "--limit", "10")
        assert history["adjustments"]
        cli("agent", "remove", "cli-node", expected=1)
        checks.append("Agent enrollment, named-node administration, local roots and exact Mbps rate persistence")

        # Exceptions with no failures and the unsupported V4 fallback must not
        # become a fabricated partial cluster result.
        assert cli("exceptions", "list", "failed", "--worker", "cli-node")["complete"] is True
        assert cli("exceptions", "list", "fallback")["supported"] is False
        assert cli("exceptions", "clear", "fallback", "--all", "--yes")["complete"] is True
        checks.append("empty failure results and explicit unsupported fallback produce honest completeness")

        agent.kill()
        agent.wait(timeout=10)
        master.kill()
        master.wait(timeout=10)
        master, restarted = spawn("master-restart", args.master, "--listen", address, "--data-dir", str(root / "master-data"))
        eventually(lambda: "Master listening on " in restarted.read_text(encoding="utf-8"))
        assert "Setup token:" not in restarted.read_text(encoding="utf-8")
        agent, _ = spawn("agent-restart", args.agent, "--data-dir", str(root / "agent-data"))
        show = eventually(ready_node)
        assert show["node"]["id"] == node_id
        assert show["node"]["label"] == "Renamed Agent"
        assert show["status"]["localStorage"]["advertisedLocalRoots"] == [str(local_root)]
        assert show["status"]["rateLimit"]["bandwidth"]["limitBps"] == 12_000_000
        assert cli("whoami")["user"]["username"] == "cli-qa"
        cli("agent", "update", "cli-node", "--clear-local-roots")
        assert cli("agent", "show", "cli-node")["status"]["localStorage"]["advertisedLocalRoots"] == []
        cli("agent", "remove", "cli-node", "--yes")
        eventually(lambda: agent.poll() is not None)
        assert cli("agent", "list")["data"]["nodes"] == []
        cli("logout")
        cli("whoami", expected=1)
        cli("login", "--username", "cli-qa", "--password-env", "QA_PASSWORD")
        assert cli("whoami")["user"]["username"] == "cli-qa"
        checks.append("forced restart preserves account/session/node/settings; revocation disconnects Agent; logout invalidates session")
        evidence = {"status": "PASSED", "checks": checks, "platform": os.name,
                    "scope": "CLI administration against real native Master and Agent; transfer content is covered by cluster.py",
                    "sha256": {name: hashlib.sha256(getattr(args, name).read_bytes()).hexdigest() for name in ("master", "agent", "cli")}}
        (root / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(evidence, indent=2))
    finally:
        for child in reversed(processes):
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=10)
        for stream in logs:
            stream.close()
        for log in root.glob("*.log"):
            content = log.read_text(errors="replace", encoding="utf-8")
            for value in private_values:
                content = content.replace(value, "[redacted]")
            log.write_text(re.sub(r"Setup token: \S+", "Setup token: [redacted]", content), encoding="utf-8")


if __name__ == "__main__":
    main()
