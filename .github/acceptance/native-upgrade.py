#!/usr/bin/env python3
"""Check native upgrade preservation and rollback with isolated real services."""
import argparse
import json
import os
from pathlib import Path
import platform
import re
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid


ROOT = Path(__file__).resolve().parents[2]


class NativeUpgradeAcceptance:
    def __init__(self, package, evidence, legacy_jar=None):
        self.package = Path(package).resolve()
        self.evidence = Path(evidence).resolve()
        self.evidence.mkdir(parents=True, exist_ok=True)
        self.temp = Path(tempfile.mkdtemp(prefix="oss-native-upgrade-"))
        self.temp.chmod(0o755)
        self.prefix = "oss-upgrade-" + uuid.uuid4().hex[:10]
        self.sudo = [] if os.geteuid() == 0 else ["sudo", "-n"]
        self.services, self.logs, self.checks, self.secrets = [], [], [], []
        self.external_backups = set()
        self.legacy_jar = Path(legacy_jar).resolve() if legacy_jar else None
        self.started = time.monotonic()
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())

    def command(self, args, *, check=True, timeout=300):
        result = subprocess.run(list(map(str, args)), text=True, capture_output=True, timeout=timeout)
        self.logs.append("$ " + " ".join(map(str, args)) + "\n" + result.stdout + result.stderr)
        if check and result.returncode:
            raise AssertionError("Command failed (exit {}): {}".format(result.returncode, " ".join(map(str, args[:3]))))
        return result

    def root_command(self, *args, **kwargs):
        return self.command([*self.sudo, *args], **kwargs)

    def read_root(self, path):
        # Read credentials in memory without adding file contents to the evidence log.
        return subprocess.run([*self.sudo, "cat", str(path)], capture_output=True, timeout=10, check=True).stdout

    def digest(self, path):
        return self.root_command("sha256sum", path).stdout.split()[0]

    def record(self, name):
        self.checks.append({"check": name, "elapsedSeconds": round(time.monotonic() - self.started, 2)})
        (self.evidence / "native-upgrade-checks.json").write_text(json.dumps(self.checks, indent=2))
        print("PASS " + name, flush=True)

    def fixture(self, name):
        directory = self.temp / name
        directory.mkdir()
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            port = listener.getsockname()[1]
        service = self.prefix + "-" + name
        self.services.append(service)
        return {"directory": directory, "service": service, "port": port,
                "cli": directory / "bin/oss-transfer-cli"}

    def state(self, fixture):
        result = self.root_command("systemctl", "show", fixture["service"], "--property=MainPID,WorkingDirectory,ActiveState")
        return dict(line.split("=", 1) for line in result.stdout.splitlines() if "=" in line)

    def api(self, fixture, path, body=None, token=None, raw=False):
        headers = {"Content-Type": "application/json"}
        if token:
            headers["Authorization"] = "Bearer " + token
        request = urllib.request.Request("http://127.0.0.1:" + str(fixture["port"]) + path,
                                         data=None if body is None else json.dumps(body).encode(), headers=headers)
        with self.http.open(request, timeout=20) as response:
            return response.read().decode() if raw else json.load(response)

    def login(self, fixture, password):
        token = self.api(fixture, "/api/auth/login", {"username": "admin", "password": password}).get("token")
        if not token:
            raise AssertionError("The existing admin password no longer authenticates")
        self.secrets.append(token)
        return token

    def prepare_persistence(self, fixture, initial_password):
        self.secrets.append(initial_password)
        token = self.login(fixture, initial_password)
        password = secrets.token_hex(24)
        self.secrets.append(password)
        changed = self.api(fixture, "/api/auth/change-password",
                           {"oldPassword": initial_password, "newPassword": password}, token)
        if changed.get("success") is not True:
            raise AssertionError("Could not prepare a password stored in the fixture database")
        fixture["password"] = password
        token = self.login(fixture, password)
        if not fixture.get("historical_jar"):
            fixture["label"] = "upgrade-preserved-" + uuid.uuid4().hex
            invitation = self.api(fixture, "/api/master/agents/invitations",
                                  {"address": "198.51.100.91", "label": fixture["label"]}, token)
            invitation_token = invitation["joinUrl"].rsplit("/", 1)[-1]
            self.secrets.extend([invitation_token, invitation["joinUrl"]])
            fixture["invitation_path"] = "/api/agents/bootstrap/" + invitation_token
        fixture["config_contents"] = self.read_root(fixture["config"])
        fixture["db_identity"] = self.read_root(fixture["db"] / "IDENTITY")
        fixture["working_directory"] = self.state(fixture)["WorkingDirectory"]
        self.assert_persistence(fixture)

    def assert_persistence(self, fixture):
        self.login(fixture, fixture["password"])
        if (fixture.get("invitation_path")
                and fixture["label"] not in self.api(fixture, fixture["invitation_path"], raw=True)):
            raise AssertionError("The saved invitation is missing after restart")
        actual_config = self.read_root(fixture["config"])
        if fixture.get("upgraded_legacy"):
            # Legacy upgrades add only the explicit Master role and bundle path;
            # every existing value, including credentials, must remain unchanged.
            additions = {"transfer.role: master",
                         "transfer.agent.package-path: " + str(fixture["config"].parent / "agent-bundle.tar.gz")}
            actual_lines = [line for line in actual_config.decode().splitlines() if line.strip()]
            previous_lines = [line for line in fixture["config_contents"].decode().splitlines() if line.strip()]
            if (any(actual_lines.count(line) != 1 for line in additions)
                    or [line for line in actual_lines if line not in additions] != previous_lines):
                raise AssertionError("Legacy upgrade changed existing settings or added unexpected configuration")
        elif actual_config != fixture["config_contents"]:
            raise AssertionError("Upgrade changed the existing application configuration")
        if self.read_root(fixture["db"] / "IDENTITY") != fixture["db_identity"]:
            raise AssertionError("Upgrade opened a different RocksDB database")
        if self.state(fixture)["WorkingDirectory"] != fixture["working_directory"]:
            raise AssertionError("Upgrade changed the service's working directory")

    def upgrade(self, fixture, package=None, check=True):
        return self.root_command("bash", self.package / "deploy/upgrade.sh", "--service-name", fixture["service"],
                                 "--cli-path", fixture["cli"], "--package-dir", package or self.package,
                                 "--backup-dir", self.temp / "backups", "--yes", check=check)

    def wizard(self, fixture, *, upgrade):
        args = ["bash", self.package / "install.sh", "--lang", "en", "--role", "master", "--method", "native",
                "--service-name", fixture["service"], "--cli-path", fixture["cli"], "--package-dir", self.package,
                "--no-deps", "--yes"]
        if upgrade:
            args.append("--upgrade")
        result = self.root_command(*args, check=False)
        for match in re.finditer(r"(?m)^Backup: (\S+)$", result.stdout + result.stderr):
            backup = Path(match.group(1))
            # Only track directories from this test's randomly named services.
            if (backup.parent != Path("/var/backups/oss-transfer")
                    or not backup.name.startswith(fixture["service"] + "-")):
                raise AssertionError("Installer returned an unexpected backup path")
            self.external_backups.add(backup)
        if upgrade and result.returncode:
            raise AssertionError("Explicit one-click upgrade failed")
        return result

    def check_restarted(self, fixture, previous_pid):
        state = self.state(fixture)
        if state["ActiveState"] != "active" or int(state["MainPID"]) <= 0 or state["MainPID"] == previous_pid:
            raise AssertionError("Upgrade did not restart the isolated service successfully")
        self.assert_persistence(fixture)
        result = self.root_command(fixture["cli"], "--json", "node", "list", timeout=30)
        nodes = json.loads(result.stdout)
        if not isinstance(nodes.get("data", {}).get("nodes"), list) or nodes.get("complete") is not True:
            raise AssertionError("The installed CLI cannot query its upgraded local service")

    def native(self):
        fixture = self.fixture("native")
        directory = fixture["directory"]
        self.root_command("bash", self.package / "deploy/install.sh", "--role", "master", "--package-dir", self.package,
                          "--install-dir", directory / "install", "--config-dir", directory / "config", "--data-dir", directory / "data",
                          "--service-name", fixture["service"], "--cli-path", fixture["cli"], "--port", fixture["port"],
                          "--web-url", "http://127.0.0.1:" + str(fixture["port"]))
        fixture["config"] = directory / "config/application.yml"
        fixture["db"] = directory / "data/rocksdb"
        bootstrap_path = directory / "config/admin-password"
        bootstrap_contents = self.read_root(bootstrap_path)
        self.prepare_persistence(fixture, bootstrap_contents.decode().strip())
        pid = self.state(fixture)["MainPID"]
        rejected = self.wizard(fixture, upgrade=False)
        if rejected.returncode == 0 or "existing deployment" not in rejected.stdout + rejected.stderr:
            raise AssertionError("Unattended fresh installation did not reject the existing service")
        if self.state(fixture)["MainPID"] != pid or self.read_root(bootstrap_path) != bootstrap_contents:
            raise AssertionError("Rejected fresh installation changed the existing process or credentials")
        self.assert_persistence(fixture)
        self.record("one-click --yes does not implicitly overwrite or restart an existing installation")
        guard_config = self.temp / "guard-application.yml"
        guard_config.touch(mode=0o600)
        for guard in ("reset-password", "mount"):
            if guard == "reset-password":
                changed = fixture["config_contents"].replace(b"auth.default.reset-password-on-startup: false", b"auth.default.reset-password-on-startup: TRUE")
                if changed == fixture["config_contents"]:
                    raise AssertionError("Fixture configuration lacks the expected password-reset setting")
                guard_config.write_bytes(changed)
                self.root_command("install", "-m", "0600", guard_config, fixture["config"])
            else:
                self.root_command("mount", "--bind", fixture["db"], fixture["db"])
            try:
                refused = self.upgrade(fixture, check=False)
                expected_error = ("disable auth.default.reset-password-on-startup" if guard == "reset-password"
                                  else "database mount points require a manual snapshot and upgrade")
                if (refused.returncode == 0 or expected_error not in refused.stdout + refused.stderr
                        or self.state(fixture)["MainPID"] != pid):
                    raise AssertionError("Upgrade did not reject " + guard + " before stopping the service")
            finally:
                if guard == "reset-password":
                    guard_config.write_bytes(fixture["config_contents"])
                    self.root_command("install", "-m", "0600", guard_config, fixture["config"])
                else:
                    self.root_command("umount", fixture["db"])
            self.assert_persistence(fixture)
        self.record("password-reset configuration and mounted database are rejected before service stop")
        self.wizard(fixture, upgrade=True)
        self.check_restarted(fixture, pid)
        if self.read_root(bootstrap_path) != bootstrap_contents:
            raise AssertionError("Upgrade replaced the bootstrap password file")
        if self.read_root(directory / "install/VERSION") != (self.package / "VERSION").read_bytes():
            raise AssertionError("Upgrade did not install the package's version metadata")
        self.record("current native upgrade preserves configuration, changed password, invitation, database and CLI access")

        # The replacement is deliberately unbootable, exercising restoration after
        # service stop and file replacement, not an early input-validation refusal.
        bad_package = self.temp / "bad-package"
        def copy_or_link(source, destination):
            try:
                return os.link(source, destination)
            except OSError:
                return shutil.copy2(source, destination)
        shutil.copytree(self.package, bad_package, copy_function=copy_or_link)
        (bad_package / "oss-transfer.jar").unlink()
        (bad_package / "oss-transfer.jar").write_bytes(b"invalid-upgrade-jar\n")
        unit = Path("/etc/systemd/system") / (fixture["service"] + ".service")
        original = {path: self.digest(path) for path in (directory / "install/oss-transfer.jar", fixture["cli"], unit)}
        pid = self.state(fixture)["MainPID"]
        result = self.upgrade(fixture, bad_package, check=False)
        if result.returncode == 0:
            raise AssertionError("Upgrade accepted an unbootable replacement")
        self.check_restarted(fixture, pid)
        if any(self.digest(path) != expected for path, expected in original.items()):
            raise AssertionError("Failed upgrade did not restore the original program, CLI and service unit")
        if self.read_root(bootstrap_path) != bootstrap_contents:
            raise AssertionError("Failed upgrade changed the bootstrap password file")
        self.record("failed startup rolls back program, unit, credentials and database, then restores authenticated service")
        self.root_command("systemctl", "stop", fixture["service"], timeout=100)

    def legacy(self):
        # By default this checks the legacy layout using the shipped JAR. An
        # optional historical JAR also exercises its actual database upgrade.
        fixture = self.fixture("legacy")
        fixture["historical_jar"] = self.legacy_jar is not None
        workdir = fixture["directory"] / "legacy-install"
        workdir.mkdir()
        fixture["config"] = workdir / "application.yml"
        fixture["db"] = workdir / "data/rocksdb"
        password = secrets.token_hex(24)
        self.secrets.append(password)
        fixture["config"].write_text("\n".join([
            "server.port: ${transfer.port:1685}", "server.address: 127.0.0.1", "transfer.port: " + str(fixture["port"]),
            "transfer.master-ip: 127.0.0.1", "transfer.local-ip: 127.0.0.1", "transfer.worker-ips: 127.0.0.1",
            "transfer.master-web-url: http://127.0.0.1:" + str(fixture["port"]),
            "transfer.internal-token: '" + secrets.token_hex(32) + "'", "rocksdb.path: ./data/rocksdb",
            "transfer.tmp-dir: ./data/tmp-objects", "logging.path: ./data/logs", "logging.file: ./data/logs/oss-transfer.log",
            "auth.default.username: admin", "auth.default.password: '" + password + "'",
            "auth.default.reset-password-on-startup: false", ""]))
        fixture["config"].chmod(0o600)
        jar = workdir / "transfer-tools-1.0.0.jar"
        shutil.copyfile(self.legacy_jar or self.package / "oss-transfer.jar", jar)
        shutil.copyfile(self.package / "VERSION", workdir / "VERSION")
        fixture["cli"].parent.mkdir()
        shutil.copyfile(self.package / ("oss-transfer-cli-" + self.arch), fixture["cli"])
        fixture["cli"].chmod(0o755)
        java = self.root_command("bash", self.package / "deploy/preflight-node.sh").stdout.strip().split("\t")[1]
        unit = fixture["directory"] / (fixture["service"] + ".service")
        unit.write_text("\n".join([
            "[Unit]", "Description=Isolated OSS Transfer legacy upgrade fixture", "[Service]", "Type=simple",
            "WorkingDirectory=" + str(workdir), "ExecStart=" + java + " -jar " + str(jar),
            "Restart=no", "TimeoutStopSec=90", "[Install]", "WantedBy=multi-user.target", ""]))
        self.root_command("install", "-m", "0644", unit, "/etc/systemd/system/" + unit.name)
        self.root_command("systemctl", "daemon-reload")
        self.root_command("systemctl", "start", fixture["service"])
        self.root_command("bash", self.package / "deploy/wait-node-ready.sh", "127.0.0.1", fixture["port"], "120", fixture["service"])
        self.prepare_persistence(fixture, password)
        pid = self.state(fixture)["MainPID"]
        # Add the exact historical hook only after the service has started. With
        # Restart=no it cannot run on an unexpected exit. The upgrade must remove
        # it before starting the replacement; cleanup only stops this service.
        historical_hook = r'''/bin/bash -c 'PID=$(netstat -tlnp 2>/dev/null | grep ":@PORT@ " | awk "{print \\$7}" | cut -d/ -f1); if [ -n "$PID" ]; then echo "Killing existing process $PID on port @PORT@"; kill -9 $PID; sleep 2; fi' '''.rstrip()
        historical_hook = historical_hook.replace("@PORT@", str(fixture["port"]))
        unit.write_text(unit.read_text().replace("[Service]\n", "[Service]\nExecStartPre=" + historical_hook + "\n", 1))
        installed_unit = Path("/etc/systemd/system") / unit.name
        self.root_command("install", "-m", "0644", unit, installed_unit)
        self.root_command("systemctl", "daemon-reload")
        if self.state(fixture)["MainPID"] != pid:
            raise AssertionError("Adding the inactive legacy hook unexpectedly restarted the fixture")
        self.wizard(fixture, upgrade=True)
        if "ExecStartPre=" in self.read_root(installed_unit).decode():
            raise AssertionError("Upgrade retained the historical port-killing startup hook")
        fixture["upgraded_legacy"] = True
        self.check_restarted(fixture, pid)
        if self.root_command("test", "-d", fixture["db"], check=False).returncode:
            raise AssertionError("Legacy database moved away from its relative working-directory path")
        self.record("{} upgrades in place, removing the legacy startup hook and preserving relative database, changed password and CLI access".format(
            "historical V4 JAR" if self.legacy_jar else "legacy service layout"))

    def clean_text(self, text):
        for secret in self.secrets:
            if secret:
                text = text.replace(secret, "[redacted]")
        return re.sub(r"(?im)^.*(?:password|token|authorization|secret).*$", "[credential-related output omitted]", text)

    def cleanup(self):
        for service in reversed(self.services):
            self.root_command("journalctl", "-u", service, "--no-pager", "-n", "100", check=False, timeout=15)
            self.root_command("systemctl", "stop", service, check=False, timeout=100)
            self.root_command("systemctl", "disable", service, check=False, timeout=20)
            self.root_command("rm", "-f", "/etc/systemd/system/" + service + ".service",
                              "/run/lock/oss-transfer-upgrade-" + service + ".lock")
            self.root_command("systemctl", "reset-failed", service, check=False, timeout=20)
        self.root_command("systemctl", "daemon-reload", check=False, timeout=20)
        for backup in self.external_backups:
            self.root_command("rm", "-rf", "--", backup)
        (self.evidence / "native-upgrade.log").write_text(self.clean_text("\n\n".join(self.logs)))
        self.root_command("rm", "-rf", "--", self.temp)

    def run(self):
        try:
            if not self.arch or not Path("/run/systemd/system").is_dir():
                raise AssertionError("Native upgrade acceptance requires Linux amd64/arm64 with systemd")
            for required in ("oss-transfer.jar", "install.sh", "deploy/install.sh", "deploy/upgrade.sh", "oss-transfer-cli-" + self.arch):
                if not (self.package / required).is_file():
                    raise AssertionError("Built package is missing " + required)
            self.native()
            self.legacy()
        finally:
            self.cleanup()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", default=str(ROOT / "output/oss-transfer"))
    parser.add_argument("--evidence", required=True)
    parser.add_argument("--legacy-jar", help="Optional historical V4 JAR, started with a fresh isolated database")
    args = parser.parse_args()
    acceptance = NativeUpgradeAcceptance(args.package, args.evidence, args.legacy_jar)
    try:
        acceptance.run()
    except Exception as error:
        print(acceptance.clean_text(type(error).__name__ + ": " + str(error)), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
