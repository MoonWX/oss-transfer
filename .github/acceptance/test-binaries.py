#!/usr/bin/env python3
"""Build target Go test executables once, then verify and run them without Go."""

import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import signal
import struct
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[2]
PLATFORMS = {row["os"] + "/" + row["arch"]: row for row in
             json.loads((ROOT / "deploy/native/platforms.json").read_text(encoding="utf-8"))}
SUPPORTED = ("linux/armv7", "linux/riscv64", "linux/loong64", "freebsd/amd64", "freebsd/arm64")
GO_OSES = set("aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd openbsd plan9 solaris wasip1 windows zos".split())
GO_ARCHES = set("386 amd64 amd64p32 arm armbe arm64 arm64be loong64 mips mipsle mips64 mips64le mips64p32 mips64p32le ppc ppc64 ppc64le riscv riscv64 s390 s390x sparc sparc64 wasm".split())
FIXTURES = {"failure-master": "QA_FAILURE_MASTER", "rollback-cli": "QA_ROLLBACK_CLI"}


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def confined(root, relative, directory=False):
    root = root.resolve()
    if not isinstance(relative, str) or not relative or any(c in relative for c in "\\\r\n\0"):
        raise ValueError("Invalid relative path: " + repr(relative))
    path = PurePosixPath(relative)
    if path.is_absolute() or ".." in path.parts or (path.as_posix() != relative and relative != "."):
        raise ValueError("Non-canonical relative path: " + relative)
    resolved = (root / relative).resolve()
    if resolved != root and root not in resolved.parents:
        raise ValueError("Path escapes its directory: " + relative)
    if directory and not resolved.is_dir():
        raise ValueError("Missing package directory: " + relative)
    if not directory and (not resolved.is_file() or resolved.is_symlink()):
        raise ValueError("Missing regular file: " + relative)
    return resolved


def commit_identity(repo):
    commit = os.environ.get("GITHUB_SHA")
    if not commit:
        commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=repo, text=True).strip()
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("A full Git commit is required")
    return commit


def build_expression(expression, tags):
    tokens = re.findall(r"&&|\|\||!|\(|\)|[A-Za-z0-9_.]+", expression)
    if "".join(tokens) != re.sub(r"\s+", "", expression):
        raise ValueError("Unsupported Go build constraint: " + expression)
    index = 0

    def atom():
        nonlocal index
        if index == len(tokens):
            raise ValueError("Incomplete Go build constraint")
        token = tokens[index]
        index += 1
        if token == "!":
            return not atom()
        if token == "(":
            value = either()
            if index == len(tokens) or tokens[index] != ")":
                raise ValueError("Unclosed Go build constraint")
            index += 1
            return value
        if token in ("&&", "||", ")"):
            raise ValueError("Invalid Go build constraint")
        return token in tags

    def both():
        nonlocal index
        value = atom()
        while index < len(tokens) and tokens[index] == "&&":
            index += 1
            right = atom()
            value = value and right
        return value

    def either():
        nonlocal index
        value = both()
        while index < len(tokens) and tokens[index] == "||":
            index += 1
            right = both()
            value = value or right
        return value

    result = either()
    if index != len(tokens):
        raise ValueError("Trailing Go build constraint")
    return result


def selected_test(path, target, go_version):
    parts = path.name[:-len("_test.go")].split("_")
    if len(parts) > 1 and parts[-1] in GO_ARCHES:
        if parts.pop() != target["goarch"]:
            return False
    if len(parts) > 1 and parts[-1] in GO_OSES and parts[-1] != target["os"]:
        return False
    match = re.match(r"go1\.(\d+)", go_version)
    if not match:
        raise ValueError("Unrecognized Go version: " + go_version)
    tags = {target["os"], target["goarch"], "unix", "gc"}
    tags.update("go1." + str(number) for number in range(1, int(match[1]) + 1))
    text = path.read_text(encoding="utf-8")
    constraint = re.search(r"^//go:build (.+)$", text, re.MULTILINE)
    if constraint:
        return build_expression(constraint[1], tags)
    # Older constraints combine lines with AND, words with OR, and comma
    # separated tags with AND. No checked-in test currently needs this branch.
    for line in re.findall(r"^// \+build (.+)$", text, re.MULTILINE):
        if not any(all((part[1:] not in tags) if part.startswith("!") else part in tags
                       for part in word.split(",")) for word in line.split()):
            return False
    return True


def source_inventory(repo, target, go_version):
    result = {}
    for module in (".", "cli"):
        base = repo / module
        for current, directories, files in os.walk(base):
            directories[:] = [name for name in directories if not name.startswith((".", "_"))
                              and name not in ("testdata", "vendor", "node_modules", "output")
                              and not (Path(current) / name / "go.mod").exists()]
            for name in files:
                if name.startswith((".", "_")) or not name.endswith("_test.go"):
                    continue
                path = Path(current) / name
                if selected_test(path, target, go_version):
                    relative = path.relative_to(repo).as_posix()
                    confined(repo, relative)
                    result[relative] = digest(path)
    return result


def json_stream(text):
    decoder = json.JSONDecoder()
    while text.strip():
        text = text.lstrip()
        value, end = decoder.raw_decode(text)
        yield value
        text = text[end:]


def build(platform, output):
    target = PLATFORMS[platform]
    output = output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    (output / "bin").mkdir()
    environment = dict(os.environ, CGO_ENABLED="0", GOOS=target["os"], GOARCH=target["goarch"],
                       GOARM=target.get("goarm", ""), GOAMD64="v1", GOARM64="v8.0", GORISCV64="rva20u64",
                       GOFLAGS="", GOEXPERIMENT="")
    go_version = subprocess.check_output(["go", "env", "GOVERSION"], env=environment, text=True).strip()
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    if commit != commit_identity(ROOT):
        raise ValueError("Build checkout differs from GITHUB_SHA")
    sources = source_inventory(ROOT, target, go_version)
    manifest = {"schema": 1, "platform": platform, "commit": commit, "goVersion": go_version,
                "testSources": sources, "tests": [], "fixtures": []}
    listed_sources = set()
    for module in (".", "cli"):
        listing = subprocess.check_output(["go", "list", "-json", "./..."], cwd=ROOT / module,
                                          env=environment, text=True)
        for package in json_stream(listing):
            files = package.get("TestGoFiles", []) + package.get("XTestGoFiles", [])
            if not files:
                continue
            cwd = Path(package["Dir"]).relative_to(ROOT).as_posix()
            test_files = sorted((PurePosixPath(cwd) / name).as_posix() for name in files)
            listed_sources.update(test_files)
            binary = "bin/" + cwd.replace("/", "-") + ".test"
            print("Compile " + platform + " " + package["ImportPath"], flush=True)
            subprocess.run(["go", "test", "-c", "-trimpath", "-o", str(output / binary), package["ImportPath"]],
                           cwd=ROOT / module, env=environment, check=True)
            manifest["tests"].append({"package": package["ImportPath"], "cwd": cwd,
                                      "binary": binary, "testFiles": test_files, "sha256": digest(output / binary)})
    if listed_sources != set(sources):
        raise ValueError("Go package discovery differs from test-source inventory: " +
                         repr(sorted(listed_sources.symmetric_difference(sources))))
    for name in FIXTURES:
        source = ".github/acceptance/fixtures/" + name + ".go"
        binary = "bin/" + name
        subprocess.run(["go", "build", "-trimpath", "-o", str(output / binary), str(confined(ROOT, source))],
                       cwd=ROOT, env=environment, check=True)
        manifest["fixtures"].append({"name": name, "source": source, "sourceSha256": digest(ROOT / source),
                                     "binary": binary, "sha256": digest(output / binary)})
    if source_inventory(ROOT, target, go_version) != sources:
        raise ValueError("Test sources changed while compiling the artifact")
    if subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip() != commit:
        raise ValueError("Git checkout changed while compiling the artifact")
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print(f"Built {len(manifest['tests'])} test packages for {platform}")


def verify_elf(path, platform):
    target = PLATFORMS[platform]
    expected = {"armv7": (1, 40), "riscv64": (2, 243), "loong64": (2, 258), "amd64": (2, 62), "arm64": (2, 183)}[target["arch"]]
    with path.open("rb") as stream:
        header = stream.read(64)
    abi = (9,) if target["os"] == "freebsd" else (0, 3)
    if (len(header) < 64 or header[:6] != b"\x7fELF" + bytes((expected[0], 1))
            or header[7] not in abi or struct.unpack_from("<H", header, 18)[0] != expected[1]):
        raise ValueError("Test executable has the wrong platform: " + str(path))


def verify(platform, directory, repo, env_file=None, publish_env=True):
    directory, repo = directory.resolve(), repo.resolve()
    manifest = json.loads(confined(directory, "manifest.json").read_text(encoding="utf-8"))
    if manifest.get("schema") != 1 or manifest.get("platform") != platform or manifest.get("commit") != commit_identity(repo):
        raise ValueError("Test manifest schema, platform or commit differs from this checkout")
    sources = source_inventory(repo, PLATFORMS[platform], manifest.get("goVersion", ""))
    if not sources or manifest.get("testSources") != sources:
        raise ValueError("Test-source inventory is incomplete or differs from this checkout")
    expected_cwds = {str(PurePosixPath(path).parent) for path in sources}
    seen_cwds, seen_packages, seen_files, seen_binaries = set(), set(), set(), set()
    values = {}
    executable_paths = []

    def executable(entry):
        binary = entry["binary"]
        if not binary.startswith("bin/") or binary in seen_binaries:
            raise ValueError("Duplicate or misplaced test executable: " + binary)
        seen_binaries.add(binary)
        path = confined(directory, binary)
        if not re.fullmatch(r"[0-9a-f]{64}", entry.get("sha256", "")) or digest(path) != entry["sha256"]:
            raise ValueError("Test executable checksum mismatch: " + binary)
        verify_elf(path, platform)
        executable_paths.append(path)
        return path

    tests = manifest.get("tests", [])
    if not isinstance(tests, list):
        raise ValueError("Test manifest has no package list")
    for entry in tests:
        cwd, package = entry["cwd"], entry["package"]
        confined(repo, cwd, directory=True)
        if cwd in seen_cwds or package in seen_packages:
            raise ValueError("Duplicate test package: " + package)
        expected_package = "oss-transfer-cli" if cwd == "cli" else "github.com/MoonWX/oss-transfer" + ("/" + cwd if cwd != "." else "")
        if package != expected_package:
            raise ValueError("Package import path differs from its source directory: " + package)
        seen_cwds.add(cwd)
        seen_packages.add(package)
        files = entry.get("testFiles", [])
        expected_files = {path for path in sources if str(PurePosixPath(path).parent) == cwd}
        if not files or len(files) != len(set(files)) or set(files) != expected_files:
            raise ValueError("Test package omits or repeats source files: " + package)
        seen_files.update(files)
        path = executable(entry)
        if cwd == "internal/agentservice":
            values["QA_MULTIPART_TEST_BINARY"] = str(path)
    if seen_cwds != expected_cwds or seen_files != set(sources):
        raise ValueError("Test manifest omits packages from this checkout")
    fixture_names = set()
    for entry in manifest.get("fixtures", []):
        name = entry["name"]
        if name not in FIXTURES or name in fixture_names:
            raise ValueError("Unknown or repeated test fixture: " + name)
        fixture_names.add(name)
        source = ".github/acceptance/fixtures/" + name + ".go"
        if entry.get("source") != source or digest(confined(repo, source)) != entry.get("sourceSha256"):
            raise ValueError("Fixture source differs from this checkout: " + name)
        values[FIXTURES[name]] = str(executable(entry))
    if fixture_names != set(FIXTURES) or "QA_MULTIPART_TEST_BINARY" not in values:
        raise ValueError("Required test fixtures are missing")
    # upload-artifact does not preserve executable modes. Restore them only
    # after validating the whole manifest, source inventory and every binary.
    for path in executable_paths:
        path.chmod(0o755)
    if publish_env and os.environ.get("GITHUB_ENV"):
        with open(os.environ["GITHUB_ENV"], "a", encoding="utf-8") as stream:
            for key, value in sorted(values.items()):
                if any(char in value for char in "\r\n"):
                    raise ValueError("Environment path contains a newline")
                stream.write(key + "=" + value + "\n")
    if env_file:
        env_file.write_text("".join("export " + key + "=" + shlex.quote(value) + "\n"
                                    for key, value in sorted(values.items())), encoding="utf-8")
    return manifest


def run(platform, directory, repo, output, parallel):
    output = output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    evidence = {"platform": platform, "result": "failed", "phase": "verify", "packages": []}
    # Match the native go test package deadline. Emulated packages contain
    # the same tests; their individual operation deadlines stay unchanged.
    package_timeout = 600
    started = time.monotonic()
    try:
        manifest = verify(platform, directory, repo, publish_env=False)
        evidence.update(commit=manifest["commit"], goVersion=manifest["goVersion"], phase="run")

        def execute(entry):
            log = output / (entry["cwd"].replace("/", "-") + ".log")
            began = time.monotonic()
            result = {"package": entry["package"], "binary": entry["binary"], "sha256": entry["sha256"],
                      "log": log.name, "timeoutSeconds": package_timeout}
            with log.open("w", encoding="utf-8") as stream:
                process = None
                try:
                    command = [str(confined(directory.resolve(), entry["binary"])), "-test.count=1", "-test.v", f"-test.timeout={package_timeout}s"]
                    process = subprocess.Popen(command, cwd=confined(repo.resolve(), entry["cwd"], directory=True),
                                               stdout=stream, stderr=subprocess.STDOUT, start_new_session=True)
                    result["exitCode"] = process.wait(timeout=package_timeout + 15)
                except Exception as error:
                    result.update(exitCode=None, error=str(error))
                    stream.write("\nTest runner error: " + str(error) + "\n")
                finally:
                    if process is not None:
                        # A test timeout can bypass the test's cleanup and leave
                        # its crash-helper subprocess alive. Reap the whole group.
                        try:
                            os.killpg(process.pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                        process.wait()
            result["seconds"] = round(time.monotonic() - began, 3)
            text = log.read_text(encoding="utf-8", errors="replace")
            result["passedTests"] = len(re.findall(r"^\s*--- PASS: ", text, re.MULTILINE))
            completed = re.search(r"^PASS\r?$", text, re.MULTILINE) is not None
            result["result"] = "passed" if result["exitCode"] == 0 and completed and result["passedTests"] > 0 else "failed"
            result["skipped"] = re.findall(r"^\s*--- SKIP: (.+)$", text, re.MULTILINE)
            print(result["result"] + " " + entry["package"] + " " + str(result["seconds"]) + "s", flush=True)
            if result["result"] != "passed":
                print(text, file=sys.stderr, flush=True)
            return result

        with ThreadPoolExecutor(max_workers=parallel) as pool:
            futures = [pool.submit(execute, entry) for entry in manifest["tests"]]
            for future in as_completed(futures):
                evidence["packages"].append(future.result())
        evidence["packages"].sort(key=lambda item: item["package"])
        evidence["result"] = "passed" if all(item["result"] == "passed" for item in evidence["packages"]) else "failed"
    except Exception as error:
        evidence["error"] = str(error)
        print("Test runner failed: " + str(error), file=sys.stderr, flush=True)
    finally:
        evidence["seconds"] = round(time.monotonic() - started, 3)
        (output / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n", encoding="utf-8")
    return 0 if evidence["result"] == "passed" else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    build_parser = commands.add_parser("build")
    build_parser.add_argument("--platform", choices=SUPPORTED, required=True)
    build_parser.add_argument("--output", type=Path, required=True)
    for name in ("verify", "run"):
        command = commands.add_parser(name)
        command.add_argument("--platform", choices=SUPPORTED, required=True)
        command.add_argument("--directory", type=Path, required=True)
        command.add_argument("--repo", type=Path, required=True)
        if name == "verify":
            command.add_argument("--env-file", type=Path)
        else:
            command.add_argument("--output", type=Path, required=True)
            command.add_argument("--parallel", type=int, choices=(1, 2), default=2)
    args = parser.parse_args()
    if args.command == "build":
        build(args.platform, args.output)
    elif args.command == "verify":
        manifest = verify(args.platform, args.directory, args.repo, args.env_file)
        print(f"Verified {len(manifest['tests'])} test packages for {args.platform}")
    else:
        return run(args.platform, args.directory, args.repo, args.output, args.parallel)
    return 0


if __name__ == "__main__":
    sys.exit(main())
