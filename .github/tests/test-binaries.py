#!/usr/bin/env python3
"""Check that target-test artifacts cannot silently omit tests or change identity."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import stat
import struct
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("test_binaries", ROOT / ".github/acceptance/test-binaries.py")
TESTS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TESTS)


class TestArtifacts(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="go test artifacts ")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.repo, self.directory = self.root / "repo", self.root / "artifacts"
        self.repo.mkdir()
        (self.directory / "bin").mkdir(parents=True)
        self.commit = "a" * 40
        self.environment = mock.patch.dict(os.environ, {"GITHUB_SHA": self.commit, "GITHUB_ENV": ""})
        self.environment.start()
        self.addCleanup(self.environment.stop)
        (self.repo / "go.mod").write_text("module github.com/MoonWX/oss-transfer\n")
        for relative in ("internal/agentservice/service_test.go", "internal/task/integer_test.go", "cli/cli_test.go"):
            path = self.repo / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("package qa\n")
        (self.repo / "cli/go.mod").write_text("module oss-transfer-cli\n")
        (self.repo / "internal/task/windows_test.go").write_text("//go:build windows\n\npackage qa\n")
        self.manifest = {"schema": 1, "platform": "linux/armv7", "commit": self.commit, "goVersion": "go1.26.5",
                         "testSources": TESTS.source_inventory(self.repo, TESTS.PLATFORMS["linux/armv7"], "go1.26.5"),
                         "tests": [], "fixtures": []}
        for cwd in ("internal/agentservice", "internal/task", "cli"):
            binary = "bin/" + cwd.replace("/", "-") + ".test"
            self.elf(binary)
            self.manifest["tests"].append({"cwd": cwd, "package": "oss-transfer-cli" if cwd == "cli" else "github.com/MoonWX/oss-transfer/" + cwd,
                                           "binary": binary, "sha256": TESTS.digest(self.directory / binary),
                                           "testFiles": sorted(p for p in self.manifest["testSources"] if str(Path(p).parent) == cwd)})
        for name in TESTS.FIXTURES:
            source = ".github/acceptance/fixtures/" + name + ".go"
            path = self.repo / source
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("package main\n")
            binary = "bin/" + name
            self.elf(binary)
            self.manifest["fixtures"].append({"name": name, "source": source, "sourceSha256": TESTS.digest(path),
                                              "binary": binary, "sha256": TESTS.digest(self.directory / binary)})
        self.save()

    def elf(self, binary):
        header = bytearray(64)
        header[:6] = b"\x7fELF\x01\x01"
        struct.pack_into("<H", header, 18, 40)
        (self.directory / binary).write_bytes(header)

    def save(self):
        (self.directory / "manifest.json").write_text(json.dumps(self.manifest))

    def verify(self, **kwargs):
        return TESTS.verify("linux/armv7", self.directory, self.repo, **kwargs)

    def test_verified_artifacts_restore_mode_and_publish_quoted_paths_without_go(self):
        env_file = self.root / "test.env"
        github_env = self.root / "github.env"
        with mock.patch.dict(os.environ, {"GITHUB_ENV": str(github_env)}), mock.patch.object(TESTS.subprocess, "check_output", side_effect=AssertionError("external tools are forbidden")):
            self.verify(env_file=env_file)
        for item in self.manifest["tests"] + self.manifest["fixtures"]:
            self.assertTrue((self.directory / item["binary"]).stat().st_mode & stat.S_IXUSR)
        text = env_file.read_text()
        self.assertIn("export QA_MULTIPART_TEST_BINARY='", text)
        self.assertEqual(len(github_env.read_text().splitlines()), 3)

    def test_missing_or_duplicate_packages_and_test_sources_fail_closed(self):
        original = copy.deepcopy(self.manifest)
        for mutation in ("omit-package", "duplicate-package", "omit-file", "omit-inventory", "wrong-cwd"):
            with self.subTest(mutation=mutation):
                self.manifest = copy.deepcopy(original)
                if mutation == "omit-package":
                    self.manifest["tests"].pop()
                elif mutation == "duplicate-package":
                    self.manifest["tests"].append(copy.deepcopy(self.manifest["tests"][0]))
                elif mutation == "omit-file":
                    self.manifest["tests"][0]["testFiles"] = []
                elif mutation == "omit-inventory":
                    self.manifest["testSources"].pop("internal/task/integer_test.go")
                else:
                    self.manifest["tests"][0]["cwd"] = "../outside"
                self.save()
                with self.assertRaises(ValueError):
                    self.verify()

    def test_changed_source_commit_or_binary_is_rejected(self):
        with mock.patch.dict(os.environ, {"GITHUB_SHA": "b" * 40}):
            with self.assertRaises(ValueError):
                self.verify()
        path = self.repo / "internal/task/integer_test.go"
        original = path.read_text()
        path.write_text(original + "// changed\n")
        with self.assertRaises(ValueError):
            self.verify()
        path.write_text(original)
        binary = self.directory / self.manifest["tests"][0]["binary"]
        binary.write_bytes(binary.read_bytes() + b"corrupted")
        with self.assertRaises(ValueError):
            self.verify()

    def test_wrong_elf_architecture_is_rejected_even_with_updated_checksum(self):
        item = self.manifest["tests"][0]
        path = self.directory / item["binary"]
        data = bytearray(path.read_bytes())
        struct.pack_into("<H", data, 18, 62)
        path.write_bytes(data)
        item["sha256"] = TESTS.digest(path)
        self.save()
        with self.assertRaises(ValueError):
            self.verify()

    def test_binary_symlink_cannot_escape_artifact_directory(self):
        item = self.manifest["tests"][0]
        path = self.directory / item["binary"]
        outside = self.root / "outside.test"
        path.rename(outside)
        path.symlink_to(outside)
        with self.assertRaises(ValueError):
            self.verify()

    def test_target_constraints_keep_arm_tests_and_exclude_other_platforms(self):
        directory = self.repo / "internal/task"
        cases = {"boundary_linux_arm_test.go": ("package qa\n", True),
                 "boundary_linux_amd64_test.go": ("package qa\n", False),
                 "boundary_freebsd_test.go": ("package qa\n", False),
                 "portable_test.go": ("//go:build (linux || freebsd) && !cgo && go1.26\n\npackage qa\n", True),
                 "native_test.go": ("//go:build cgo || (freebsd && amd64)\n\npackage qa\n", False)}
        for name, (source, expected) in cases.items():
            path = directory / name
            path.write_text(source)
            self.assertEqual(TESTS.selected_test(path, TESTS.PLATFORMS["linux/armv7"], "go1.26.5"), expected, name)

    def test_verification_failure_still_writes_evidence(self):
        self.manifest["fixtures"] = []
        self.save()
        output = self.root / "evidence"
        self.assertEqual(TESTS.run("linux/armv7", self.directory, self.repo, output, 2), 1)
        evidence = json.loads((output / "evidence.json").read_text())
        self.assertEqual(evidence["phase"], "verify")
        self.assertEqual(evidence["result"], "failed")

    def test_runner_keeps_all_package_logs_when_one_process_fails(self):
        manifest = copy.deepcopy(self.manifest)
        manifest["tests"] = manifest["tests"][:2]
        for index, item in enumerate(manifest["tests"]):
            path = self.directory / item["binary"]
            path.write_text("#!/bin/sh\nprintf '%s\\n' 'package output' '--- PASS: TestFixture (0.00s)' 'PASS'\nexit " + str(index) + "\n")
            path.chmod(0o755)
        output = self.root / "run"
        with mock.patch.object(TESTS, "verify", return_value=manifest):
            self.assertEqual(TESTS.run("linux/armv7", self.directory, self.repo, output, 2), 1)
        evidence = json.loads((output / "evidence.json").read_text())
        self.assertEqual(len(evidence["packages"]), 2)
        self.assertEqual({item["exitCode"] for item in evidence["packages"]}, {0, 1})
        for item in evidence["packages"]:
            self.assertIn("package output", (output / item["log"]).read_text())

    def test_success_exit_without_tests_cannot_pass(self):
        manifest = copy.deepcopy(self.manifest)
        manifest["tests"] = manifest["tests"][:1]
        path = self.directory / manifest["tests"][0]["binary"]
        path.write_text("#!/bin/sh\nprintf 'helper returned successfully\\n'\n")
        path.chmod(0o755)
        output = self.root / "no-tests"
        with mock.patch.object(TESTS, "verify", return_value=manifest):
            self.assertEqual(TESTS.run("linux/armv7", self.directory, self.repo, output, 2), 1)
        result = json.loads((output / "evidence.json").read_text())["packages"][0]
        self.assertEqual(result["exitCode"], 0)
        self.assertEqual(result["passedTests"], 0)
        self.assertEqual(result["result"], "failed")

    def test_all_platforms_use_native_package_timeout_with_cleanup_grace(self):
        manifest = copy.deepcopy(self.manifest)
        manifest["tests"] = manifest["tests"][:1]
        for platform in TESTS.SUPPORTED:
            with self.subTest(platform=platform):
                timeout = 900
                process = mock.Mock(pid=123456, wait=mock.Mock(return_value=0))

                def start(command, **kwargs):
                    self.assertIn(f"-test.timeout={timeout}s", command)
                    kwargs["stdout"].write("--- PASS: TestFixture (0.00s)\nPASS\n")
                    return process

                output = self.root / platform.replace("/", "-")
                with mock.patch.object(TESTS, "verify", return_value=manifest), \
                        mock.patch.object(TESTS.subprocess, "Popen", side_effect=start), \
                        mock.patch.object(TESTS.os, "killpg"):
                    self.assertEqual(TESTS.run(platform, self.directory, self.repo, output, 1), 0)
                process.wait.assert_any_call(timeout=timeout + 15)
                evidence = json.loads((output / "evidence.json").read_text())
                self.assertEqual(evidence["packages"][0]["timeoutSeconds"], timeout)


if __name__ == "__main__":
    unittest.main()
