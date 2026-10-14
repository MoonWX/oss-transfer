#!/usr/bin/env python3
"""Install real Master/Agent Compose services through the public Go installer.

Run as root on a Linux Docker host with the verified candidate image loaded.
Only this run's random Compose projects and temporary data are removed.
"""
import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
from urllib.error import URLError
from urllib.request import Request, urlopen


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def eventually(check, description, timeout=90):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            result = check()
            if result:
                return result
        except (URLError, ConnectionError, TimeoutError):
            pass
        time.sleep(0.2)
    raise AssertionError("Timed out: " + description)


class Acceptance:
    def __init__(self, args):
        self.args = args
        self.output = args.output.resolve()
        self.output.mkdir(parents=True, mode=0o700, exist_ok=False)
        # Only scrubbed evidence lives here; installer configuration and state
        # stay in the separate private temporary directory until cleanup.
        self.output.chmod(0o755)
        self.work = Path(tempfile.mkdtemp(prefix="oss-go-setup-"))
        self.prefix = "oss-go-setup-" + secrets.token_hex(6)
        self.projects = {role: self.prefix + "-" + role for role in ("master", "agent")}
        self.secrets, self.manifest_requests = [], []
        self.server = self.thread = None
        self.origin, self.token = "", ""
        self.evidence = {"status": "RUNNING", "checks": [], "image": args.image,
                         "limitations": ["Linux Compose installation only; native service installation is tested separately",
                                         "Both services share one Docker host; no cloud compatibility or throughput claim"]}

    def scrub(self, text):
        for value in self.secrets:
            text = text.replace(value, "[redacted]")
        return re.sub(r"Setup token: \S+", "Setup token: [redacted]", text)

    def log(self, name, text):
        path = self.output / name
        path.write_text(self.scrub(text), encoding="utf-8")
        path.chmod(0o644)

    def save(self):
        self.log("evidence.json", json.dumps(self.evidence, indent=2) + "\n")

    def record(self, name, **details):
        self.evidence["checks"].append({"name": name, **details})
        self.save()
        print("PASS " + name, flush=True)

    def docker(self, *arguments, timeout=60, check=True):
        result = subprocess.run(["docker", *map(str, arguments)], capture_output=True,
                                text=True, encoding="utf-8", timeout=timeout)
        if check and result.returncode:
            raise RuntimeError("Docker " + arguments[0] + " failed: " + self.scrub(result.stderr))
        return result.stdout.strip()

    def compose(self, role, *arguments, **kwargs):
        return self.docker("compose", "--project-name", self.projects[role], "--file",
                           self.work / (role + "-install") / "docker-compose.yml", *arguments, **kwargs)

    def api(self, path, body=None):
        headers = {"Content-Type": "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        request = Request(self.origin + path, headers=headers,
                          data=None if body is None else json.dumps(body).encode())
        with urlopen(request, timeout=20) as response:
            payload = json.load(response)
        require(payload.get("success") is True, "API failure: " + path)
        data = payload.get("data", payload)
        require(not data.get("partial") and not data.get("failedCount"), "Incomplete API result: " + path)
        return data

    def online(self):
        nodes = self.api("/api/master/nodes")["nodes"]
        return nodes[0] if len(nodes) == 1 and nodes[0].get("online") else None

    def serve_manifest(self, manifest, version):
        owner = self
        expected = "/v" + version + "/manifest.tsv"

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass

            def do_GET(self):
                owner.manifest_requests.append(self.path)
                if self.path != expected:
                    self.send_error(404)
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/plain")
                self.send_header("Content-Length", str(len(manifest)))
                self.end_headers()
                self.wfile.write(manifest)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return "http://127.0.0.1:" + str(self.server.server_port) + "/v" + version

    def install(self, role, extra=(), token=None, expect_success=True, log_name=None):
        command = ["bash", str(self.args.installer.resolve()), "--yes", "--no-deps", "--role", role,
                   "--method", "compose", "--image", self.args.image, "--release-url", self.release_url,
                   "--service-name", self.projects[role], "--install-dir", str(self.work / (role + "-install")),
                   "--data-dir", str(self.work / (role + "-data")), *map(str, extra)]
        environment = dict(os.environ)
        # Only the explicit stdin invitation is allowed into this QA install.
        environment.pop("TRANSFER_JOIN_TOKEN", None)
        environment.pop("OSS_TRANSFER_JOIN_TOKEN", None)
        try:
            result = subprocess.run(command, input=None if token is None else token + "\n",
                                    env=environment, capture_output=True, text=True, encoding="utf-8", timeout=180)
        except subprocess.TimeoutExpired as error:
            def text(value):
                return value.decode("utf-8", errors="replace") if isinstance(value, bytes) else value or ""
            self.log(log_name or (role + "-install.log"), text(error.stdout) + text(error.stderr))
            raise AssertionError(role + " public installer timed out") from None
        self.log(log_name or (role + "-install.log"), result.stdout + "\n" + result.stderr)
        require((result.returncode == 0) == expect_success,
                role + " installer returned unexpected status " + str(result.returncode) + "; see sanitized log")

    def run(self):
        require(sys.platform.startswith("linux") and os.geteuid() == 0, "Run this acceptance with sudo on Linux")
        raw = self.args.manifest.read_bytes()
        require(len(raw) < 65536, "Manifest is too large")
        identities = {}
        for line in raw.decode("utf-8").splitlines():
            fields = line.split("\t")
            if fields[0] in ("format", "version", "commit"):
                require(len(fields) == 2 and fields[0] not in identities, "Invalid or repeated manifest identity")
                identities[fields[0]] = fields[1]
        require(identities.get("format") == "oss-transfer-go-v1" and
                re.fullmatch(r"[a-f0-9]{40}", identities.get("commit", "")) and
                re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?", identities.get("version", "")),
                "Expected the real, versioned candidate manifest")
        image = json.loads(self.docker("image", "inspect", self.args.image))[0]
        labels = image["Config"].get("Labels", {})
        require(image["Os"] == "linux" and image["Config"]["User"] == "65532:65532", "Candidate is not the Linux nonroot image")
        require(labels.get("org.opencontainers.image.revision") == identities["commit"] and
                labels.get("org.opencontainers.image.version") == identities["version"], "Image identity differs from manifest")
        if os.environ.get("GITHUB_SHA"):
            require(identities["commit"] == os.environ["GITHUB_SHA"], "Manifest is not from this CI commit")
        self.evidence.update(imageId=image["Id"], architecture=image["Architecture"], release=identities,
                             manifestSha256=hashlib.sha256(raw).hexdigest(),
                             installerSha256=hashlib.sha256(self.args.installer.read_bytes()).hexdigest())
        self.release_url = self.serve_manifest(raw, identities["version"])
        with socket.socket() as bound:
            bound.bind(("0.0.0.0", 0))
            port = bound.getsockname()[1]
        self.origin = "http://127.0.0.1:" + str(port)
        gateway = json.loads(self.docker("network", "inspect", "bridge"))[0]["IPAM"]["Config"][0]["Gateway"]
        require(re.fullmatch(r"[0-9.]+", gateway), "Expected an IPv4 Docker bridge gateway")
        master_url = "http://" + gateway + ":" + str(port)
        self.install("master", ["--port", port])
        master = self.compose("master", "ps", "-q", "master")
        require(master and "\n" not in master, "Installer did not create exactly one Master container")
        status = eventually(lambda: self.api("/api/setup/status"), "installed Master setup")
        require(not status["configured"] and status["tokenRequired"], "Fresh Compose Master bypassed setup")
        setup_token = eventually(lambda: re.search(r"Setup token: ([A-Za-z0-9_-]+)", self.docker("logs", master)), "setup token")[1]
        credentials = {"username": "compose-setup-qa", "password": secrets.token_urlsafe(24)}
        self.secrets += [setup_token, credentials["password"]]
        self.api("/api/setup/complete", dict(credentials, publicUrl=master_url, setupToken=setup_token))
        self.token = self.api("/api/auth/login", credentials)["token"]
        self.secrets.append(self.token)
        require(self.api("/api/master/nodes")["nodes"] == [], "Master unexpectedly registered itself as an Agent")
        self.record("public Compose installer starts a nonroot Master and completes initial setup")

        invitation = self.api("/api/master/agents/invitations", {"address": "compose-setup-agent", "label": "Compose setup QA"})["inviteToken"]
        self.secrets.append(invitation)
        files = self.work / "agent-files"
        files.mkdir(mode=0o755)
        self.install("agent", ["--master-url", master_url, "--token-stdin", "--files-dir", files], token=invitation)
        agent = self.compose("agent", "ps", "-q", "agent")
        require(agent and "\n" not in agent, "Installer did not create exactly one Agent container")
        member = eventually(self.online, "installed Agent online")
        require(member.get("address") == "compose-setup-agent", "Invitation address was not retained")
        inspections = {role: json.loads(self.docker("inspect", container))[0] for role, container in (("master", master), ("agent", agent))}
        for role, inspection in inspections.items():
            compose_text = (self.work / (role + "-install") / "docker-compose.yml").read_text(encoding="utf-8")
            require(invitation not in compose_text and "JOIN_TOKEN" not in compose_text, "Invitation persisted in Compose")
            require(not any("JOIN_TOKEN" in value or invitation in value for value in inspection["Config"]["Env"]),
                    "Long-running container retained an enrollment token")
            require(not any(invitation in value for value in inspection["Config"]["Cmd"]), "Invitation persisted in container command")
            mounts = {mount["Destination"]: mount for mount in inspection["Mounts"]}
            require(mounts["/data"]["Source"] == str(self.work / (role + "-data")), "Role uses another service's data directory")
            require((self.work / (role + "-data")).stat().st_uid == 65532, "Data ownership prevents nonroot service writes")
            if role == "agent":
                require(mounts[str(files)]["Source"] == str(files), "Agent bind path was remapped unexpectedly")
            else:
                require(str(files) not in mounts, "Master mounted local transfer files")
        self.record("Agent installer enrolls once with separate data; long-running Compose and environment contain no join token", nodeId=member["id"])

        started = inspections["master"]["State"]["StartedAt"]
        self.install("master", ["--port", port], expect_success=False, log_name="master-reinstall-refused.log")
        current = json.loads(self.docker("inspect", master))[0]
        require(current["State"]["StartedAt"] == started and current["State"]["Running"], "Refused reinstall disrupted the existing Master")
        self.record("rerunning first-install over existing data is refused without restarting the service")

        self.docker("restart", "--time", "30", agent, master, timeout=100)
        eventually(lambda: self.api("/api/setup/status")["configured"], "configured Master after restart")
        self.token = self.api("/api/auth/login", credentials)["token"]
        self.secrets.append(self.token)
        restored = eventually(self.online, "Agent reconnecting after both containers restart")
        require(restored["id"] == member["id"] and restored.get("address") == member.get("address"), "Restart changed the Agent identity")
        require(json.loads(self.docker("inspect", master))[0]["State"]["StartedAt"] != started, "Master did not actually restart")
        require(len(self.manifest_requests) >= 2 and set(self.manifest_requests) == {"/v" + identities["version"] + "/manifest.tsv"},
                "Installer did not fetch the fixed-version manifest")
        self.record("Master credentials and one Agent identity survive real container restarts")

    def close(self):
        errors = []
        for role, project in self.projects.items():
            try:
                containers = self.docker("ps", "-aq", "--filter", "label=com.docker.compose.project=" + project).split()
                for index, container in enumerate(containers):
                    result = subprocess.run(["docker", "logs", container], capture_output=True, text=True, timeout=15)
                    self.log(role + "-container-" + str(index) + ".log", result.stdout + result.stderr)
                if containers:
                    self.docker("rm", "-f", *containers)
                networks = self.docker("network", "ls", "-q", "--filter", "label=com.docker.compose.project=" + project).split()
                if networks:
                    self.docker("network", "rm", *networks)
                require(not self.docker("ps", "-aq", "--filter", "label=com.docker.compose.project=" + project), "Test containers remain")
            except Exception as error:
                errors.append(self.scrub(str(error)))
        if self.server:
            self.server.shutdown()
            self.server.server_close()
            self.thread.join(timeout=5)
        if not errors:
            shutil.rmtree(self.work)
        self.evidence["testResourcesRemoved"] = not errors
        if errors:
            self.evidence.update(status="FAILED", cleanupErrors=errors)
        self.save()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--installer", type=Path, default=Path("install.sh"))
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--output", type=Path, required=True)
    acceptance = Acceptance(parser.parse_args())
    try:
        acceptance.run()
        acceptance.evidence["status"] = "PASSED"
    except Exception as error:
        acceptance.evidence.update(status="FAILED", error=acceptance.scrub(str(error)))
        print("FAIL " + acceptance.scrub(str(error)), file=sys.stderr)
    finally:
        acceptance.close()
    return 0 if acceptance.evidence["status"] == "PASSED" else 1


if __name__ == "__main__":
    sys.exit(main())
