#!/usr/bin/env python3
"""Verify installed-cluster CLI access with real Master and Agent processes."""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import time
import urllib.error
import urllib.request


def require(value, message):
    if not value:
        raise AssertionError(message)


def wait(check, description, timeout=40):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(0.1)
    raise AssertionError("Timed out: " + description)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("master", "agent", "cli", "output"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    root = args.output.resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    environment = {key: value for key, value in os.environ.items() if not key.startswith("OSS_TRANSFER_")}
    environment.update(OSS_TRANSFER_CLI_CONFIG_DIR=str(root / "profile"), OSS_TRANSFER_LANG="en")
    processes, streams, private_values, checks, commands = [], [], [], [], []
    evidence = {"status": "failed", "checks": checks, "commands": commands,
                "scope": "Isolated native processes and managed installation metadata; no live cluster changes"}

    def install(name, role):
        directory = root / name
        directory.mkdir(mode=0o700)
        data = directory / "data"
        data.mkdir(mode=0o700)
        extension = ".exe" if os.name == "nt" else ""
        service = directory / ("oss-transfer-" + role + extension)
        cli = directory / ("oss-transfer-cli" + extension)
        shutil.copy2(getattr(args, role), service)
        shutil.copy2(args.cli, cli)
        fields = {"role": role, "serviceName": "oss-transfer-" + role, "cliPath": str(cli), "dataDir": str(data)}
        if os.name == "nt":
            (directory / "install.json").write_text(json.dumps(fields))
        else:
            (directory / "install.tsv").write_text("".join(key + "\t" + value + "\n" for key, value in fields.items()))
        return {"directory": directory, "data": data, "service": service, "cli": cli}

    def start(installation, role, *arguments, extra_env=None):
        log = installation["directory"] / (role + "-" + str(len(processes)) + ".log")
        stream = log.open("wb")
        log.chmod(0o600)
        process = subprocess.Popen([str(installation["service"]), "--data-dir", str(installation["data"]), *arguments],
                                   cwd=root, env=dict(environment, **(extra_env or {})), stdout=stream, stderr=subprocess.STDOUT)
        processes.append(process)
        streams.append(stream)
        return process, log

    def cli(installation, *arguments, expected=0, input_text=None, env=None):
        result = subprocess.run([str(installation["cli"]), "--json", *map(str, arguments)],
                                cwd=root, env=env or environment, input=input_text, text=True,
                                capture_output=True, timeout=30)
        commands.append({"installation": installation["directory"].name, "command": list(arguments),
                         "exitCode": result.returncode})
        require(result.returncode == expected, "CLI " + str(arguments[:2]) + " unexpected exit " + str(result.returncode))
        if arguments[:2] != ("agent", "invite"):
            require(not any(value in result.stdout + result.stderr for value in private_values), "CLI leaked a private credential")
        return json.loads(result.stdout)

    def grants(installation, role, origin):
        public = Path(str(installation["cli"]) + ".cluster.json")
        private = Path(str(installation["cli"]) + ".local-auth.json")
        wait(private.is_file, role + " local credential")
        value = json.loads(private.read_text())
        private_values.append(value["token"])
        require(value["role"] == role and value["masterUrl"] == origin, "Local credential binding differs")
        require(json.loads(public.read_text()) == {"role": role, "masterUrl": origin}, "Public connection contains unexpected fields")
        if os.name != "nt":
            require(stat.S_IMODE(private.stat().st_mode) == 0o600, "Private credential is not mode 0600")
        require(json.loads((installation["data"] / "cli-local-auth.json").read_text()) == value, "Data-volume credential differs")
        return value["token"]

    try:
        master = install("master", "master")
        master_process, log = start(master, "master", "--listen", "127.0.0.1:0")
        address = wait(lambda: re.search(r"Master listening on (127\.0\.0\.1:\d+)", log.read_text()), "Master listen")[1]
        origin = "http://" + address
        initial = wait(lambda: re.search(r"Initial password: ([A-Za-z0-9_-]+)", log.read_text()), "Initial password")[1]
        private_values.append(initial)
        token = grants(master, "master", origin)
        require(cli(master, "whoami")["user"]["role"] == "admin", "Master local access did not authenticate")
        cli(master, "task", "list")
        invitation = cli(master, "agent", "invite", "--label", "Local CLI acceptance")["data"]
        private_values.append(invitation["inviteToken"])
        require(not list((root / "profile").rglob("*.json")), "Local access persisted a login session")
        checks.append("Fresh installed Master reads tasks and creates an invitation without login or a host argument")
        agent = install("agent", "agent")
        agent_process, _ = start(agent, "agent", "--master-url", origin,
                                 extra_env={"OSS_TRANSFER_JOIN_TOKEN": invitation["inviteToken"]})
        agent_token = grants(agent, "agent", origin)
        cli(agent, "task", "list")
        node_id = wait(lambda: cli(master, "agent", "invite-status", invitation["id"])["data"].get("nodeId"), "Agent enrollment")
        nodes = cli(agent, "agent", "list")["data"]["nodes"]
        address = next(node["address"] for node in nodes if node["id"] == node_id)
        cli(agent, "agent", "update", address, "--label", "Changed by local CLI")
        require(cli(master, "agent", "show", address)["node"]["label"] == "Changed by local CLI", "Agent CLI mutation was not applied")
        checks.append("Installed Agent discovers its cluster and updates a node without login; no browser credentials copied")
        # A plain HTTP caller receives no privilege merely by sharing this host.
        try:
            urllib.request.urlopen(origin + "/api/master/nodes", timeout=10)
            raise AssertionError("Unauthenticated loopback request was accepted")
        except urllib.error.HTTPError as error:
            require(error.code == 401, "Unauthenticated loopback did not require login")
        anonymous = root / "anonymous-profile"
        login_env = dict(environment, OSS_TRANSFER_CLI_CONFIG_DIR=str(anonymous))
        login = cli(master, "login", "--password-stdin", input_text=initial + "\n", env=login_env)
        require(login["user"]["mustChangePassword"], "Local credential changed browser password policy")
        checks.append("Unauthenticated loopback is rejected and initial browser-account password policy remains intact")
        remote = install("other-master", "master")
        _, remote_log = start(remote, "master", "--listen", "127.0.0.1:0")
        remote_address = wait(lambda: re.search(r"Master listening on (127\.0\.0\.1:\d+)", remote_log.read_text()), "Other Master listen")[1]
        remote_origin = "http://" + remote_address
        cli(master, "--host", remote_origin, "whoami", expected=1)
        cli(agent, "--host", remote_origin, "whoami", expected=1)
        cli(master, "task", "list")
        checks.append("Both CLIs reject an unlogged-in different Master and retain their own default cluster")
        master_process.terminate()
        master_process.wait(timeout=25)
        master_process, _ = start(master, "master", "--listen", origin.removeprefix("http://"))
        wait(lambda: Path(str(master["cli"]) + ".local-auth.json").is_file() and master_process.poll() is None,
             "Restarted Master")
        wait(lambda: healthy(origin), "Restarted Master health")
        require(grants(master, "master", origin) == token, "Master restart changed the local grant unexpectedly")
        cli(master, "task", "list")
        cli(agent, "task", "list")
        checks.append("Master restart preserves local access on both installed roles and keeps the enrolled node")
        agent_process.terminate()
        agent_process.wait(timeout=25)
        cli(master, "agent", "remove", address, "--yes")
        cli(agent, "whoami", expected=1)
        cli(master, "task", "list")
        checks.append("Removing an enrolled Agent immediately revokes its existing local credential")
        evidence["status"] = "passed"
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=25)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
        for stream in streams:
            stream.close()
        for log in root.rglob("*.log"):
            content = log.read_text(errors="replace")
            for secret in private_values:
                content = content.replace(secret, "[redacted]")
            content = re.sub(r"(?m)^Initial password: .+$", "Initial password: [redacted]", content)
            log.write_text(content)
        # This fixture has finished; do not retain reusable administrative tokens.
        for path in list(root.rglob("*.local-auth.json")) + list(root.rglob("cli-local-auth.json")):
            path.unlink()
        shutil.rmtree(root / "profile", ignore_errors=True)
        shutil.rmtree(root / "anonymous-profile", ignore_errors=True)
        (root / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
    print(json.dumps({"status": evidence["status"], "checks": len(checks), "commands": len(commands)}))


def healthy(origin):
    try:
        with urllib.request.urlopen(origin + "/healthz", timeout=2) as response:
            return response.status == 200
    except (urllib.error.URLError, OSError):
        return False


if __name__ == "__main__":
    main()
