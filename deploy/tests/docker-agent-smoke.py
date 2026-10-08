#!/usr/bin/env python3
"""Isolated Docker enrollment smoke test. Removes only containers created here."""
import argparse
import json
import secrets
import subprocess
import time
import urllib.error
import urllib.request
import uuid


def run(*args):
    result = subprocess.run(args, text=True, capture_output=True, check=False)
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed: {result.stderr[-1000:]}")
    return result.stdout.strip()


def request(origin, path, token=None, body=None, expected=200):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(origin + path, data=data, headers=headers,
                                 method="GET" if body is None else "POST")
    try:
        response = urllib.request.urlopen(req, timeout=30)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        if response.status != expected:
            raise RuntimeError(f"{path.split('/')[2]} endpoint returned HTTP {response.status}")
        content = response.read()
    return json.loads(content) if content else {}


def until(check, timeout=150):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except (OSError, RuntimeError, ValueError):
            pass
        time.sleep(1)
    raise RuntimeError("Docker enrollment check timed out")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="oss-transfer:ci")
    args = parser.parse_args()
    suffix = secrets.token_hex(6)
    network = "oss-smoke-" + suffix
    master = network + "-master"
    agent = network + "-agent"
    volumes = [master + "-data", agent + "-data"]
    containers = []
    try:
        run("docker", "network", "create", network)
        run("docker", "run", "-d", "--name", master, "--network", network,
            "--network-alias", "master", "-p", "127.0.0.1::1685",
            "-e", "TRANSFER_MASTER_WEB_URL=http://master:1685",
            "-v", volumes[0] + ":/app/data", args.image)
        containers.append(master)
        detail = json.loads(run("docker", "inspect", master))[0]
        port = detail["NetworkSettings"]["Ports"]["1685/tcp"][0]["HostPort"]
        origin = "http://127.0.0.1:" + port
        until(lambda: request(origin, "/api/auth/current") is not None)
        password = run("docker", "exec", master, "cat", "/app/data/secrets/admin_password")
        login = request(origin, "/api/auth/login", body={"username": "admin", "password": password})
        token = login["token"]
        invite = request(origin, "/api/master/agents/invitations", token,
                         {"address": "198.51.100.20", "label": "ci-agent"})
        join_token = invite["joinUrl"].rsplit("/", 1)[1]
        run("docker", "run", "-d", "--name", agent, "--network", network,
            "-e", "TRANSFER_ROLE=agent", "-e", "TRANSFER_AGENT_MASTER_URL=http://master:1685",
            "-e", "TRANSFER_AGENT_JOIN_TOKEN=" + join_token,
            "-e", "TRANSFER_AGENT_ADDRESS=198.51.100.20",
            "-v", volumes[1] + ":/app/data", args.image)
        containers.append(agent)

        def connected():
            snapshots = request(origin, "/api/master/worker-status", token).get("workers", {})
            nodes = request(origin, "/api/master/nodes", token)["data"]["nodes"]
            online = any(node["address"] == "198.51.100.20" and node.get("online") is True for node in nodes)
            return online and snapshots.get("198.51.100.20", {}).get("success") is True

        until(connected)
        detail = json.loads(run("docker", "inspect", agent))[0]
        if detail["HostConfig"]["PortBindings"]:
            raise RuntimeError("Agent incorrectly exposes an inbound port")
        before = request(origin, "/api/master/nodes", token)["data"]["nodes"]
        if not any(node["address"] == "198.51.100.20" for node in before):
            raise RuntimeError("Agent membership was not persisted")
        request(origin, "/api/agents/join", body={"token": join_token,
                "instanceId": str(uuid.uuid4()), "address": "198.51.100.21",
                "credential": secrets.token_urlsafe(32)}, expected=400)
        run("docker", "restart", agent)
        until(connected)
        run("docker", "restart", master)
        # Docker can assign a new host port when an ephemeral binding restarts.
        detail = json.loads(run("docker", "inspect", master))[0]
        port = detail["NetworkSettings"]["Ports"]["1685/tcp"][0]["HostPort"]
        origin = "http://127.0.0.1:" + port
        until(lambda: request(origin, "/api/auth/current") is not None)
        token = request(origin, "/api/auth/login", body={"username": "admin", "password": password})["token"]
        until(connected)
        after = request(origin, "/api/master/nodes", token)["data"]["nodes"]
        def membership(nodes):
            fields = ("address", "label", "master", "agent")
            return sorted(tuple(node.get(field) for field in fields) for node in nodes)
        if membership(before) != membership(after):
            raise RuntimeError("Membership changed across a restart")
        print("Docker smoke passed: outbound enrollment, authenticated RPC, token reuse rejected, master/agent restart persistence, no inbound agent ports.")
    finally:
        for container in reversed(containers):
            subprocess.run(["docker", "rm", "-f", container], capture_output=True)
        for volume in volumes:
            subprocess.run(["docker", "volume", "rm", volume], capture_output=True)
        subprocess.run(["docker", "network", "rm", network], capture_output=True)


if __name__ == "__main__":
    main()
