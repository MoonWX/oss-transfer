#!/usr/bin/env python3
"""Exercise packaged Go processes. This is enrollment/setup coverage, not transfer acceptance."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


def eventually(check, timeout=30):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (OSError, ValueError, URLError):
            pass
        time.sleep(0.1)
    raise AssertionError("condition was not satisfied before timeout")


def api(origin, path, method="GET", body=None, token=None, expected=200):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    request = Request(origin + path, data=None if body is None else json.dumps(body).encode(), headers=headers, method=method)
    try:
        response = urlopen(request, timeout=10)
    except HTTPError as failure:
        response = failure
    with response:
        status = response.status
        result = json.load(response)
    assert status == expected, (path, status, result.get("message"))
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--master", type=Path, required=True)
    parser.add_argument("--agent", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--expect-agent-release-url")
    parser.add_argument("--expect-agent-image")
    args = parser.parse_args()
    root = args.output.resolve()
    root.mkdir(parents=True, exist_ok=False)
    # No Java, shell, frontend directory or external tools are on the service PATH.
    environment = dict(os.environ, PATH=str(root / "no-external-programs"))
    children = []
    logs = []

    def spawn(name, executable, *arguments):
        log_path = root / (name + ".log")
        stream = log_path.open("wb")
        if os.name != "nt":
            os.chmod(log_path, 0o600)
        process = subprocess.Popen([str(executable.resolve()), *arguments], cwd=root, env=environment, stdout=stream, stderr=subprocess.STDOUT)
        children.append(process)
        logs.append(stream)
        return process, log_path

    checks = []
    try:
        master, log = spawn("master-first", args.master, "--listen", "127.0.0.1:0", "--data-dir", str(root / "master-data"))
        address = eventually(lambda: re.search(r"Master listening on (127\.0\.0\.1:\d+)", log.read_text(encoding="utf-8")))[1]
        origin = "http://" + address
        setup_token = eventually(lambda: re.search(r"Setup token: ([A-Za-z0-9_-]+)", log.read_text(encoding="utf-8")))[1]
        with urlopen(origin + "/", timeout=10) as response:
            html = response.read().decode()
            assert response.status == 200
        assets = re.findall(r'(?:src|href)="(/assets/[^" ]+)"', html)
        assert assets, "Packaged console did not reference embedded assets"
        for asset in assets:
            with urlopen(origin + asset, timeout=10) as response:
                assert response.status == 200 and response.read(), "Embedded asset missing"
                assert "text/html" not in response.headers.get("Content-Type", ""), "Missing asset served the page HTML"
        assert api(origin, "/api/setup/status")["configured"] is False
        api(origin, "/api/master/nodes", expected=503)
        credentials = {"username": "runtime-qa", "password": "temporary runtime QA password"}
        api(origin, "/api/setup/complete", "POST", dict(credentials, publicUrl=origin, setupToken=setup_token))
        login = api(origin, "/api/auth/login", "POST", credentials)
        token = login["token"]
        checks.append("embedded console and atomic authenticated setup without external runtime")
        agents = []
        for number in (1, 2):
            invitation = api(origin, "/api/master/agents/invitations", "POST", {"address": f"qa-node-{number}", "label": f"Agent {number}"}, token)["data"]
            if number == 1 and args.expect_agent_release_url:
                assert invitation.get("nativeAgent", {}).get("available") is True, "Published Master has no native Agent installation command"
                bootstrap = origin + "/api/agents/bootstrap/" + invitation["inviteToken"]
                assert bootstrap in invitation.get("traditionalCommand", ""), "Native command does not use this Master's invitation"
                assert bootstrap + "?platform=windows" in invitation.get("windowsCommand", ""), "Windows Agent command is missing"
                for suffix, installer in (("", "install.sh"), ("?platform=windows", "Install.ps1")):
                    with urlopen(bootstrap + suffix, timeout=10) as response:
                        script = response.read().decode()
                        assert args.expect_agent_release_url + "/" + installer in script, "Agent bootstrap downloads a different release"
                checks.append("packaged Master generates Linux/macOS and Windows Agent commands pinned to its release")
            if number == 1 and args.expect_agent_image:
                assert "'" + args.expect_agent_image + "'" in invitation.get("dockerCommand", ""), "Agent Docker command does not use the matching image"
                checks.append("packaged Master generates an Agent command with the matching container version")
            process, _ = spawn(f"agent-{number}", args.agent, "--master-url", origin, "--join-token", invitation["inviteToken"], "--data-dir", str(root / f"agent-{number}-data"))
            agents.append(process)

        def nodes_online():
            nodes = api(origin, "/api/master/nodes", token=token)["data"]["nodes"]
            return nodes if len(nodes) == 2 and all(node["online"] for node in nodes) else None

        nodes = eventually(nodes_online)
        identities = {node["address"]: node["id"] for node in nodes}
        checks.append("two separate native Agent processes enroll through outbound connections")
        agents[0].kill()
        agents[0].wait(timeout=10)
        agents[0], _ = spawn("agent-1-restart", args.agent, "--data-dir", str(root / "agent-1-data"))
        # Master is deliberately killed rather than gracefully shut down.
        master.kill()
        master.wait(timeout=10)
        master, restarted_log = spawn("master-restart", args.master, "--listen", address, "--data-dir", str(root / "master-data"))
        eventually(lambda: "Master listening on " in restarted_log.read_text(encoding="utf-8"))
        assert "Setup token:" not in restarted_log.read_text(encoding="utf-8")
        assert api(origin, "/api/setup/status")["configured"] is True
        assert api(origin, "/api/auth/current", token=token)["user"]["username"] == credentials["username"]
        nodes = eventually(nodes_online)
        assert {node["address"]: node["id"] for node in nodes} == identities
        checks.append("forced Master/Agent termination retains account, sessions and enrollment identities")
        api(origin, "/api/master/nodes/qa-node-1", "DELETE", token=token)
        eventually(lambda: agents[0].poll() is not None)
        remaining = api(origin, "/api/master/nodes", token=token)["data"]["nodes"]
        assert len(remaining) == 1 and remaining[0]["address"] == "qa-node-2"
        assert agents[1].poll() is None
        checks.append("revocation disconnects only the selected Agent")
        evidence = {"scope": "Go process setup, enrollment, crash recovery and revocation; no transfer claims", "checks": checks, "masterSha256": hashlib.sha256(args.master.read_bytes()).hexdigest(), "agentSha256": hashlib.sha256(args.agent.read_bytes()).hexdigest(), "platform": os.name}
        (root / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")
        print(json.dumps(evidence, indent=2))
    finally:
        for child in reversed(children):
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=10)
        for stream in logs:
            stream.close()
        # The original startup log contains the one-use setup token. Keep only
        # sanitized logs as acceptance evidence.
        for log in root.glob("*.log"):
            log.write_text(re.sub(r"Setup token: \S+", "Setup token: [redacted]", log.read_text(errors="replace", encoding="utf-8")), encoding="utf-8")


if __name__ == "__main__":
    main()
