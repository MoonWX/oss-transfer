# Acceptance tests

[中文](README.zh-CN.md)

CI builds the release archives once, then runs their Master, Agent and CLI against SeaweedFS 4.48. A separate S3 client reads destination objects and compares their keys, lengths and SHA-256 hashes with the source fixture. Release publishes the archives and image from that CI run.

## Platforms

| Platform | Runner |
| --- | --- |
| Linux amd64 | `ubuntu-latest` |
| Linux arm64 | `ubuntu-26.04-arm` |
| macOS amd64 | `macos-15-intel` |
| macOS arm64 | `macos-latest` |
| Windows amd64 | `windows-latest` |
| Windows arm64 | `windows-11-arm` |

Each tested platform has one acceptance job. These six platforms run the same application and native installer scenarios. The two Linux jobs also test their Docker image and Compose installer. Go tests enable the race detector except on Windows arm64, where it is unsupported. That runner uses native arm64 application binaries and an emulated x64 SeaweedFS fixture.

Linux ARMv7, riscv64 and loong64 execute the target binaries under QEMU, with an amd64 SeaweedFS fixture. FreeBSD amd64 and arm64 each run in one FreeBSD 14.4 VM; their SeaweedFS fixture runs natively on the Linux host. Master, Agent, CLI and crash-recovery test executables still run inside FreeBSD, using its filesystem and service manager. When enabled, these five platforms run the same Go/CLI tests and application scenarios as the six platforms above, including shared bandwidth, stop/restart, multipart cleanup, inventory recovery and installation rollback. Their test executables are cross-compiled with `CGO_ENABLED=0`; they do not run the race detector.

Pushes and pull requests run ten platforms. FreeBSD ARM64 is optional because its software-emulated suite takes about sixteen minutes. Its archive is still built and checked with the other ten release archives. To enable its complete runtime suite, select **Run workflow** in CI and enable `freebsd_arm64`, or run:

```sh
gh workflow run ci.yml --ref main -f freebsd_arm64=true
```

This builds the archives, then runs only the complete FreeBSD ARM64 suite. It does not repeat the other ten platforms or build the container image. Manual runs do not cancel push runs or supply release artifacts.

The old Java database converter supports Linux and macOS. Other platforms test that publication is rejected before changing migration state; FreeBSD also checks that staged data and the startup fence remain intact.

The Linux QEMU jobs install real systemd services on the Ubuntu runner. Only the installer's CPU detection is mapped to the target architecture; service management, executable files, PID checks and rollback remain real. This exercises target CPU code on the host Linux kernel. It does not establish compatibility with a particular physical CPU, distribution or kernel. FreeBSD uses rc.d inside its VM.

## Scheduling

The package job builds all eleven release archives, up to four platforms at a time, while the Python packaging checks and message tests run beside the build; any failure stops the job before it uploads anything. A separate test-program job runs in parallel with it and cross-compiles the enabled additional platforms' test executables: one `go test -c` per module and target, all targets at once, without DWARF or symbol tables. Default CI compiles four test bundles; selecting FreeBSD ARM64 compiles only its bundle.

Go build and module caches are kept per runner OS, architecture, Go version and purpose (`cross` for archives, `tests` for test programs, `race` for native tests). Main and manual runs save them under the commit; pull requests only restore them. Restored entries are trimmed relative to the previous save, so a quiet week does not make Go discard the cache.

Test artifacts record the commit, target architecture and SHA-256 hashes and are not published in releases. The multipart crash test uses the target `agentservice` test executable, including its self-executed crash helper.

Default CI runs ten platform jobs and the shared package, test-program and image builds. Only the two Linux native jobs wait for the image; macOS and Windows start as soon as the packages exist. The QEMU and FreeBSD jobs wait for the packages and the test programs. Selecting FreeBSD ARM64 runs the package and test-program jobs and one FreeBSD ARM64 job. Each tested platform starts four groups internally: unit tests, CLI and inventory checks, installation, and transfers. The unit group starts `internal/master`, the slowest package, as three concurrent shards next to the remaining packages, which run longest first and at most two at a time. Shards are balanced from [master-shards.json](master-shards.json); a test missing from that list goes to the lightest shard, and the run fails unless the shards cover exactly the tests that `go test -list` reports. The installer source checks and the CLI module tests run in the control group. Native installation and Compose installation run sequentially because both discover installed services.

Transfers can overlap other groups while checking setup, multipart recovery and the three transfer directions, and while uploading the bandwidth and recovery fixtures. Before measuring bandwidth, the transfer group waits for every other group to pass, exit and finish cleanup. Bandwidth and stop/restart tests then run alone. A failed group prevents this window from opening and fails the job; every run has its own gate identity.

FreeBSD caches its Python environment and dependencies; changes to the requirements file invalidate that cache. It receives only its own platform archive and returns only logs and evidence. The host S3 fixture uses the pinned SeaweedFS archive, temporary credentials and a loopback listener reached through QEMU's private network. Fixture credentials are excluded from uploaded artifacts. Installation fixtures use fast gzip/ZIP compression; published archives and archive validation are unchanged.

`output/platform/evidence.json` records each stage's time and result. Detailed output is in the adjacent logs. The cluster evidence also records which groups completed before the exclusive transfer window.

FreeBSD arm64 uses QEMU software emulation on `ubuntu-26.04-arm`, with two virtual CPUs and `GOMAXPROCS=2`. This ARM host did not expose `/dev/kvm` in the capability check; the guest still uses TCG, not hardware virtualization. Host and guest diagnostics record CPU counts, the QEMU version and a KVM VM-creation probe. Compare stage timings before drawing a speed conclusion. FreeBSD amd64 keeps the default VM CPU settings on `ubuntu-latest`.

Release waits for the default push CI at its commit. Optional manual FreeBSD ARM64 acceptance is separate and does not gate publication. A successful default CI run does not establish FreeBSD ARM64 runtime acceptance. VERSION-only commits can reuse the same default suite from a code-identical successful push run; manual runs are not reuse baselines. Builds and publication retain all eleven entries in the [platform list](../../deploy/native/platforms.json).

## Coverage

| Test | Assertions |
| --- | --- |
| Package and image verification | Checksums, archive members, executable architecture, version and commit match. Image binaries match the native packages. |
| State and request handling | Duplicate requests preserve one task; stale work cannot resume after stop; ledger entries and counters agree. Cancellation, source changes, path escapes and cleanup ownership are checked. |
| Setup and CLI | Setup protects administration, credentials are enforced, generated Agent URLs match the release, and accounts, nodes and limits survive restart. |
| Transfers | All three directions produce the expected keys and hashes. Both selected Agents receive work. Naming rules, SKIP, OVERWRITE and child totals match the fixture. |
| Task bandwidth | Two concurrent 2 Mbps tasks each share their own limit across two Agents. The test samples destination growth during transfer and verifies the completed files and reports. |
| Demand-based shares | A download task over two Agents has one 24 MiB object in its first dispatch batch and one 256 KiB object in the second. Once the Agent with the small object is idle, the node view must show it idle and the busy Agent holding at least the limit × (1 − 1/64) − 1 B/s; every node-view sample keeps the shares within the limit, and destination growth stays within the limit. Throughput is recorded, not asserted. It uses the recovery task limit. `--scenario dynamic_share` runs only this check. |
| Stop and restart | Writes stop after acknowledgement; stop intent survives restart. Killing an Agent preserves completed progress and existing targets. Recovery finishes the remaining files and removes its temporary files. |
| Multipart recovery | Restart cleans up the interrupted upload and spool while retaining unrelated uploads and the previous completed object. The interrupted object is retried from the beginning. |
| Execution node changes | An Agent added to a bandwidth-limited OSS-to-OSS task mid-run joins, the limit is split between both Agents, both transfer objects, and every object's SHA-256 and the task's counts match. Draining one of two Agents mid-run lets it leave before the task ends; the remaining Agent takes the whole limit and the task completes with exact accounting. Removing the scanning Agent immediately moves its work and, while needed, the scan to the other Agent; the node view then marks the new scanner or a completed scan, never the removed node. Throughout all three changes the task reports no error. `--scenario nodes` runs only this check. |
| Reports | Exported rows, statuses, destination keys and totals match the verified inventory. |
| Inventory import | Lost responses and repeated submissions retain one parent task. Restart preserves unfinished imports. Changed input under the same request ID is rejected, and missing keys are reported. |
| Native installation | Service PID and version are correct. Upgrade preserves passwords and enrollment. Bad checksums are rejected before stopping the service. Rollback and existing-data adoption are exercised. |
| Docker and Compose | Setup, enrollment and bind-mounted transfers work without root in the container. Recreating containers preserves state. |

The main fixture contains 260 objects, crossing the 128-object dispatch boundary. It includes an empty object, a name with Unicode and spaces, and two 17 MiB objects for multipart upload. The bandwidth fixture has 129 objects: two 4 MiB files, one in each dispatch batch, and 127 empty files. At 2 Mbps, its 8 MiB payload takes about 34 seconds per task. Recovery keeps its separate 256-file fixture with a 16 Mbps task limit, set per platform in `RECOVERY_BANDWIDTH_MBPS` in [run-platform.py](run-platform.py). If a recovery transfer finishes before its stop or crash is injected, the check fails and names that setting instead of passing without exercising recovery.

SeaweedFS supplies the S3 service; the tests do not connect to JD Cloud OSS. Provider-specific signing, permissions and throttling need a run against that provider. Unit tests use HTTP simulators to reproduce protocol errors.

## Run locally

Build a package for the current platform with `./build.sh --test`, then prepare the test environment. Replace `linux/amd64` below with your platform:

```sh
python3 -m venv /tmp/oss-transfer-qa-venv
/tmp/oss-transfer-qa-venv/bin/pip install -r .github/acceptance/requirements.txt
/tmp/oss-transfer-qa-venv/bin/python .github/acceptance/prepare.py \
  --platform linux/amd64 --directory /tmp/oss-transfer-qa-bin
/tmp/oss-transfer-qa-venv/bin/python .github/acceptance/cluster.py \
  --master /tmp/oss-transfer-qa-bin/oss-transfer-master \
  --agent /tmp/oss-transfer-qa-bin/oss-transfer-agent \
  --weed /tmp/oss-transfer-qa-bin/weed \
  --output output/cluster-local --scenario all --multipart-recovery
```

Use a fresh output directory. On Windows, use `.exe` filenames and the Python executable from your environment. By default, `--multipart-recovery` compiles an Agent test binary and requires Go. Pass `--multipart-test-binary` to use a verified executable from `test-binaries.py` instead. The main scenarios execute the packaged binaries with an empty executable PATH.

To prepare the additional platforms' test executables on a build host:

```sh
python3 .github/acceptance/test-binaries.py build --platform linux/armv7 --output /tmp/oss-transfer-tests
```

Copy that directory to the target environment and use `verify` or `run` with `--platform`, `--directory` and `--repo`. Verification checks the executable architecture, checksums and checkout before running anything. `run` also takes a fresh `--output` directory for logs and results. Linux emulation needs binfmt registered for both the initial process and the subprocesses created by recovery tests.

For service installation tests, run `install.py` with `--master`, `--agent`, `--cli`, `--repo` and `--output`. Linux requires root and systemd; Windows requires Administrator. macOS uses a user LaunchAgent, or a system LaunchDaemon with `--system`. Tests use separate service names and directories and remove their registrations afterward. Only disposable CI machines use `--all-installed`, which exercises discovery and upgrade of every installed role.

`docker.py` checks a prepared image. `setup.py` tests the public installer against a local release directory. [ci.yml](../workflows/ci.yml) contains their full commands.

The `local-cli.py` check starts separate installed Master and Agent processes. It verifies automatic local access, remote login isolation, credential file permissions, restart persistence and revocation after removing an Agent. It uses its own data and does not change an existing cluster.

## Results

Scenarios write `evidence.json` and sanitized logs under their output directory. Bandwidth tests also save samples; package and image checks record artifact identities. CI retains the uploaded results for seven days.

`compare.py` compares a V4.0.x JAR package with Go using the same fixture and supplies the real-data migration fixture helper. These optional comparisons are separate from release acceptance.
