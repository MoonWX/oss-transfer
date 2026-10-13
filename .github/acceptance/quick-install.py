#!/usr/bin/env python3
"""Exercise the shipped installer against real systemd and Compose services."""
import argparse
import http.server
import json
import os
from pathlib import Path
import pty
import re
import select
import shutil
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[2]


class InstallerAcceptance:
    def __init__(self, package, image, evidence):
        self.package = Path(package).resolve()
        self.installer = self.package / "install.sh"
        self.archive = self.package.parent / "oss-transfer.tar.gz"
        self.image = image
        self.evidence = Path(evidence).resolve()
        self.evidence.mkdir(parents=True, exist_ok=True)
        self.temp = Path(tempfile.mkdtemp(prefix="oss-installer-acceptance-"))
        self.temp.chmod(0o755)
        (self.temp / "local-files").mkdir(mode=0o777)
        (self.temp / "local-files").chmod(0o777)
        self.prefix = "oss-install-" + uuid.uuid4().hex[:10]
        self.sudo = [] if os.geteuid() == 0 else ["sudo", "-n"]
        self.native_service = self.prefix + "-native"
        self.native_agent_service = self.prefix + "-native-agent"
        self.compose_service = self.prefix + "-compose"
        self.agent_service = self.prefix + "-agent"
        self.logs, self.checks, self.secrets, self.requests = [], [], [], []
        self.http = None
        self.started = time.monotonic()

    def command(self, args, *, check=True, timeout=180):
        result = subprocess.run(list(map(str, args)), text=True, capture_output=True, timeout=timeout)
        self.logs.append("$ " + " ".join(map(str, args)) + "\n" + result.stdout + result.stderr)
        if check and result.returncode:
            raise AssertionError("Command failed (exit {}): {}".format(result.returncode, " ".join(map(str, args[:3]))))
        return result

    def root_command(self, *args, **kwargs):
        return self.command([*self.sudo, *args], **kwargs)

    def read_root(self, path):
        # Passwords are consumed only in memory and never included in command logs.
        result = subprocess.run([*self.sudo, "cat", str(path)], text=True, capture_output=True, timeout=10, check=True)
        return result.stdout

    def record(self, name, details=None):
        self.checks.append({"check": name, "elapsedSeconds": round(time.monotonic() - self.started, 2), "details": details})
        (self.evidence / "installer-checks.json").write_text(json.dumps(self.checks, indent=2))
        print("PASS " + name, flush=True)

    def port(self):
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            return listener.getsockname()[1]

    def args(self, name, method, port, download=False, join_url=None, address="198.51.100.80"):
        directory = self.temp / name
        args = [*self.sudo, "bash", str(self.installer), "--lang", "en", "--role", "agent" if join_url else "master", "--method", method,
                "--source", "oss", "--version", "latest", "--no-deps", "--install-dir", str(directory / "install"),
                "--data-dir", str(directory / "data"), "--config-dir", str(directory / "config"),
                "--cli-path", str(directory / "bin" / "oss-transfer-cli"), "--service-name", self.prefix + "-" + name,
                "--port", str(port)]
        if join_url:
            args += ["--join-url", join_url, "--address", address]
        else:
            args += ["--web-url", "http://" + (self.native_web_host if name == "native" else "127.0.0.1") + ":" + str(port)]
        if method == "compose":
            args += ["--image", self.image, "--files-dir", str(self.temp / "local-files")]
        if download:
            args += ["--package-url", self.download_origin + "/oss-transfer.tar.gz",
                     "--checksum-url", self.download_origin + "/bad.sha256"]
        elif method == "native" and not join_url:
            args += ["--package-dir", str(self.package)]
        return args

    def download_server(self):
        acceptance = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                acceptance.requests.append(self.path)
                if self.path == "/oss-transfer.tar.gz":
                    self.send_response(200)
                    self.send_header("Content-Length", str(acceptance.archive.stat().st_size))
                    self.end_headers()
                    with acceptance.archive.open("rb") as stream:
                        shutil.copyfileobj(stream, self.wfile)
                elif self.path == "/bad.sha256":
                    payload = ("0" * 64 + "  oss-transfer.tar.gz\n").encode()
                    self.send_response(200)
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                else:
                    self.send_error(404)

            def log_message(self, *args):
                pass

        self.http = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.http.serve_forever, daemon=True).start()
        self.download_origin = "http://127.0.0.1:" + str(self.http.server_port)

    def assert_no_install(self, name):
        if (self.temp / name).exists():
            raise AssertionError("Failed or cancelled installer created its destination")
        if Path("/etc/systemd/system/" + self.prefix + "-" + name + ".service").exists():
            raise AssertionError("Failed or cancelled installer created a service")

    def cancelled(self):
        args = self.args("cancelled", "native", self.port(), download=True)
        pid, descriptor = pty.fork()
        if pid == 0:
            os.execvp(args[0], args)
        output, ended = bytearray(), False
        try:
            os.write(descriptor, b"n\n")
            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                readable, _, _ = select.select([descriptor], [], [], 0.1)
                if readable:
                    try:
                        content = os.read(descriptor, 65536)
                    except OSError:
                        content = b""
                    output.extend(content)
                child, _ = os.waitpid(pid, os.WNOHANG)
                if child:
                    ended = True
                    break
            if not ended:
                raise AssertionError("Installer did not exit after declining confirmation")
        finally:
            if not ended:
                os.killpg(pid, signal.SIGKILL)
                os.waitpid(pid, 0)
            os.close(descriptor)
            self.logs.append(output.decode(errors="replace"))
        if "Cancelled. No changes made." not in output.decode(errors="replace"):
            raise AssertionError("Installer did not acknowledge cancellation")
        if self.requests:
            raise AssertionError("Installer downloaded files before confirmation")
        self.assert_no_install("cancelled")
        self.record("declining confirmation performs no download or installation")

    def bad_checksum(self):
        result = self.command([*self.args("checksum", "native", self.port(), download=True), "--yes"], check=False)
        if result.returncode == 0 or not re.search(r"checksum|SHA-?256", result.stdout + result.stderr, re.I):
            raise AssertionError("Installer did not reject the corrupted checksum")
        if sorted(self.requests) != ["/bad.sha256", "/oss-transfer.tar.gz"]:
            raise AssertionError("Checksum rejection did not exercise the real archive download: " + repr(self.requests))
        self.assert_no_install("checksum")
        self.record("downloaded archive with wrong checksum is rejected before installation")

    def login(self, port, password):
        origin = "http://127.0.0.1:" + str(port)
        body = json.dumps({"username": "admin", "password": password}).encode()
        request = urllib.request.Request(origin + "/api/auth/login", data=body, headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=20) as response:
            token = json.load(response).get("token")
        if not token:
            raise AssertionError("Generated admin password cannot obtain an authenticated session")
        self.secrets.append(token)
        request = urllib.request.Request(origin + "/api/master/nodes", headers={"Authorization": "Bearer " + token})
        with urllib.request.urlopen(request, timeout=20) as response:
            payload = json.load(response)
        if not isinstance(payload.get("data", {}).get("nodes"), list):
            raise AssertionError("Fresh Master does not serve authenticated cluster API")
        return token

    def native(self):
        # A bridge-network Agent must be able to reach the host Master URL in its invitation.
        bridge = json.loads(self.root_command("docker", "network", "inspect", "bridge").stdout)[0]
        self.native_web_host = bridge["IPAM"]["Config"][0]["Gateway"]
        port = self.port()
        args = [*self.args("native", "native", port), "--yes"]
        self.command(args)
        password = self.read_root(self.temp / "native/config/admin-password").strip()
        self.secrets.append(password)
        self.native_token = self.login(port, password)
        self.native_port = port
        mode = self.root_command("stat", "-c", "%a", self.temp / "native/config/admin-password").stdout.strip()
        if mode != "600":
            raise AssertionError("Native bootstrap password is not private")
        pid = self.root_command("systemctl", "show", "--property=MainPID", "--value", self.native_service).stdout.strip()
        if not pid.isdigit() or int(pid) <= 0:
            raise AssertionError("Native installer did not start a live systemd service")
        self.record("native Master starts from the shipped JAR and generated password authenticates", {"service": self.native_service})
        preserved = {str(path): self.read_root(path) for path in (
            self.temp / "native/config/application.yml", self.temp / "native/config/admin-password")}
        result = self.command(args, check=False)
        if result.returncode == 0:
            raise AssertionError("One-click installer unexpectedly overwrote an existing deployment")
        if any(self.read_root(path) != contents for path, contents in preserved.items()):
            raise AssertionError("Repeated installation changed existing credentials or configuration")
        current_pid = self.root_command("systemctl", "show", "--property=MainPID", "--value", self.native_service).stdout.strip()
        if current_pid != pid:
            raise AssertionError("Rejected repeated installation restarted the existing service")
        self.native_token = self.login(port, password)
        self.record("existing native deployment is rejected without changing credentials or its running process")

    def compose(self):
        port = self.port()
        args = [*self.args("compose", "compose", port), "--yes"]
        self.command(args)
        password = self.read_root(self.temp / "compose/data/secrets/admin_password").strip()
        self.secrets.append(password)
        self.login(port, password)
        result = self.root_command("docker", "compose", "--project-name", self.compose_service, "--file",
                                   self.temp / "compose/install/docker-compose.yml", "ps", "-q", "master")
        container = result.stdout.strip()
        if not container or "\n" in container:
            raise AssertionError("Compose installer did not create exactly one Master")
        detail = json.loads(self.root_command("docker", "inspect", container).stdout)[0]
        if detail["Config"]["Image"] != self.image:
            raise AssertionError("Compose installer did not use the exact accepted image")
        if not any(mount["Destination"] == str(self.temp / "local-files") and mount["Source"] == str(self.temp / "local-files") for mount in detail["Mounts"]):
            raise AssertionError("Compose installer did not mount the selected local file directory")
        self.root_command("docker", "exec", container, "sh", "-c", 'printf installer-check > "$1/installer-check"',
                          "_", self.temp / "local-files")
        if (self.temp / "local-files/installer-check").read_text() != "installer-check":
            raise AssertionError("Compose process cannot write through its selected local-files mount")
        self.record("Compose Master uses the shipped image, writes through its local mount and accepts its generated password")
        self.compose_down("compose", self.compose_service)

    def native_api(self, path, body=None):
        request = urllib.request.Request("http://127.0.0.1:" + str(self.native_port) + path,
                                         data=None if body is None else json.dumps(body).encode(),
                                         headers={"Authorization": "Bearer " + self.native_token, "Content-Type": "application/json"})
        with urllib.request.urlopen(request, timeout=20) as response:
            return json.load(response)

    def invitation(self, address):
        invitation = self.native_api("/api/master/agents/invitations", {"address": address, "label": "installer-acceptance"})
        join_url = invitation["joinUrl"]
        self.secrets.extend([join_url, join_url.rsplit("/", 1)[-1]])
        return join_url

    def wait_agent(self, address):
        deadline = time.monotonic() + 90
        while time.monotonic() < deadline:
            nodes = self.native_api("/api/master/nodes")["data"]["nodes"]
            workers = self.native_api("/api/master/worker-status").get("workers", {})
            if any(node.get("address") == address and node.get("online") is True for node in nodes) and workers.get(address, {}).get("success") is True:
                return
            time.sleep(1)
        raise AssertionError("Installed Agent did not join the Master and return live worker status: " + address)

    def agent(self):
        address = "198.51.100.80"
        self.command([*self.args("agent", "compose", self.port(), join_url=self.invitation(address), address=address), "--yes"])
        self.wait_agent(address)
        self.record("Compose Agent consumes a real invitation and reports live status to the installed native Master")

    def native_agent(self):
        address = "198.51.100.81"
        # No local package option: the wizard must download the bundle and checksum from this Master.
        self.command([*self.args("native-agent", "native", self.port(), join_url=self.invitation(address), address=address), "--yes"])
        self.wait_agent(address)
        actual = self.root_command("sha256sum", self.temp / "native-agent/install/oss-transfer.jar").stdout.split()[0]
        expected = self.root_command("sha256sum", self.package / "oss-transfer.jar").stdout.split()[0]
        if actual != expected:
            raise AssertionError("Downloaded native Agent JAR differs from the Master's shipped JAR")
        self.record("native Agent downloads the Master's verified bundle, starts via systemd and reports live status")

    def compose_down(self, name, service):
        compose = self.temp / name / "install/docker-compose.yml"
        if self.root_command("test", "-f", compose, check=False).returncode == 0:
            self.root_command("docker", "compose", "--project-name", service, "--file", compose,
                              "logs", "--no-color", "--tail", "100", check=False, timeout=15)
            self.root_command("docker", "compose", "--project-name", service, "--file", compose,
                              "down", "--volumes", "--remove-orphans", check=False, timeout=120)

    def clean_text(self, text):
        for secret in self.secrets:
            if secret:
                text = text.replace(secret, "[redacted]")
        # Startup and error output may mention credentials before the test can read them.
        return re.sub(r"(?im)^.*(?:password|token|authorization|secret).*$", "[credential-related output omitted]", text)

    def cleanup(self):
        if self.http:
            self.http.shutdown()
            self.http.server_close()
        # Collect only this test's service metadata/logs before deleting its resources.
        self.compose_down("compose", self.compose_service)
        self.compose_down("agent", self.agent_service)
        for service in (self.native_agent_service, self.native_service):
            self.root_command("journalctl", "-u", service, "--no-pager", "-n", "150", check=False, timeout=15)
            self.root_command("systemctl", "stop", service, check=False, timeout=100)
            self.root_command("systemctl", "disable", service, check=False, timeout=20)
            self.root_command("rm", "-f", "/etc/systemd/system/" + service + ".service")
        self.root_command("systemctl", "daemon-reload", check=False, timeout=20)
        (self.evidence / "installer.log").write_text(self.clean_text("\n\n".join(self.logs)))
        self.root_command("rm", "-rf", "--", self.temp)

    def run(self):
        if not self.installer.is_file() or not self.archive.is_file():
            raise AssertionError("Built release must contain install.sh and its verified tar archive")
        self.download_server()
        try:
            self.cancelled()
            self.bad_checksum()
            self.native()
            self.compose()
            self.agent()
            self.native_agent()
        finally:
            self.cleanup()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", default=str(ROOT / "output/oss-transfer"))
    parser.add_argument("--image", required=True)
    parser.add_argument("--evidence", required=True)
    args = parser.parse_args()
    InstallerAcceptance(args.package, args.image, args.evidence).run()


if __name__ == "__main__":
    main()
