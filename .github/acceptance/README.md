# Acceptance tests

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

These six platforms run the same application and native installer scenarios in parallel. Linux also tests the Docker image and Compose installer on both architectures. Go tests enable the race detector except on Windows arm64, where it is unsupported. That runner uses native arm64 application binaries and an emulated x64 SeaweedFS fixture.

Linux ARMv7, riscv64 and loong64 run the packaged applications under QEMU, with a native amd64 SeaweedFS fixture. FreeBSD amd64 and arm64 run in separate FreeBSD 14.4 VMs. These five jobs check setup, durable identity, node settings, three transfer directions and report contents; FreeBSD also checks rc.d installation, upgrade and rollback. They reuse the core transfer fixture rather than repeating the long bandwidth and multipart crash suites. QEMU coverage does not establish compatibility with a specific physical CPU or distribution.

All platform jobs run in parallel after the archives are built. Release waits for every job; added platforms increase runner usage, while elapsed time is determined by the slowest job. Builds and publication read the same [platform list](../../deploy/go/platforms.json).

## Coverage

| Test | Assertions |
| --- | --- |
| Package and image verification | Checksums, archive members, executable architecture, version and commit match. Image binaries match the native packages. |
| State and request handling | Duplicate requests preserve one task; stale work cannot resume after stop; ledger entries and counters agree. Cancellation, source changes, path escapes and cleanup ownership are checked. |
| Setup and CLI | Setup protects administration, credentials are enforced, generated Agent URLs match the release, and accounts, nodes and limits survive restart. |
| Transfers | All three directions produce the expected keys and hashes. Both selected Agents receive work. Naming rules, SKIP, OVERWRITE and child totals match the fixture. |
| Task bandwidth | Two concurrent 8 Mbps tasks each share their own limit across two Agents. The test samples destination growth during transfer. |
| Stop and restart | Writes stop after acknowledgement; stop intent survives restart. Killing an Agent preserves completed progress and existing targets. Recovery finishes the remaining files and removes its temporary files. |
| Multipart recovery | Restart cleans up the interrupted upload and spool while retaining unrelated uploads and the previous completed object. The interrupted object is retried from the beginning. |
| Reports | Exported rows, statuses, destination keys and totals match the verified inventory. |
| Inventory import | Lost responses and repeated submissions retain one parent task. Restart preserves unfinished imports. Changed input under the same request ID is rejected, and missing keys are reported. |
| Native installation | Service PID and version are correct. Upgrade preserves passwords and enrollment. Bad checksums are rejected before stopping the service. Rollback and existing-data adoption are exercised. |
| Docker and Compose | Setup, enrollment and bind-mounted transfers work without root in the container. Recreating containers preserves state. |

The main fixture contains 260 objects, crossing the 128-object dispatch boundary. It includes an empty object, a name with Unicode and spaces, and two 17 MiB objects for multipart upload. Bandwidth, recovery and inventory tests use separate fixtures.

SeaweedFS supplies the S3 service; the tests do not connect to JD Cloud OSS. Provider-specific signing, permissions and throttling need a run against that provider. Unit tests use HTTP simulators to reproduce protocol errors.

## Run locally

Build a package for the current platform with `./build.sh --test`, then prepare the test environment. Replace `linux/amd64` below with your platform:

```sh
python3 -m venv /tmp/oss-transfer-qa-venv
/tmp/oss-transfer-qa-venv/bin/pip install -r .github/acceptance/requirements.txt
/tmp/oss-transfer-qa-venv/bin/python .github/acceptance/prepare-go.py \
  --platform linux/amd64 --directory /tmp/oss-transfer-qa-bin
/tmp/oss-transfer-qa-venv/bin/python .github/acceptance/go-cluster.py \
  --master /tmp/oss-transfer-qa-bin/oss-transfer-master \
  --agent /tmp/oss-transfer-qa-bin/oss-transfer-agent \
  --weed /tmp/oss-transfer-qa-bin/weed \
  --output output/go-cluster-local --scenario all --multipart-recovery
```

Use a fresh output directory. On Windows, use `.exe` filenames and the Python executable from your environment. The multipart crash test also compiles an Agent test binary, so `--multipart-recovery` requires Go. The main scenarios execute the packaged binaries with an empty executable PATH.

For service installation tests, run `go-install.py` with `--master`, `--agent`, `--cli`, `--repo` and `--output`. Linux requires root and systemd; Windows requires Administrator. macOS uses a user LaunchAgent, or a system LaunchDaemon with `--system`. Tests use separate service names and directories and remove their registrations afterward.

`go-docker.py` checks a prepared image. `go-setup.py` tests the public installer against a local release directory. [ci.yml](../workflows/ci.yml) contains their full commands.

## Results

Scenarios write `evidence.json` and sanitized logs under their output directory. Bandwidth tests also save samples; package and image checks record artifact identities. CI retains the uploaded results for seven days.

Performance comparisons are separate from release acceptance. `go-compare.py` compares a historical V4.0.x JAR package with Go using the same fixture; `go-cloud-compare.py` adds resource limits for an isolated Linux host. See [the migration record](../../docs/go-migration-acceptance.md) for measurements and remaining checks.
