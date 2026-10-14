# Release acceptance

CI builds the Go archives once, then runs their Master, Agent and CLI binaries against a real SeaweedFS 4.48 S3 service. Verification reads the destination data independently and compares every expected key, length and SHA-256. The release workflow uses those same checked archives and container image.

## Platforms

| Platform | Native runner |
| --- | --- |
| Linux amd64 | `ubuntu-latest` |
| Linux arm64 | `ubuntu-26.04-arm` |
| macOS amd64 | `macos-15-intel` |
| macOS arm64 | `macos-latest` |
| Windows amd64 | `windows-latest` |
| Windows arm64 | `windows-11-arm` |

Each platform runs the same application and installer scenarios. Both Linux platforms also run the archived Docker image and Compose installer. Jobs run in parallel; one failure does not cancel the other platforms. Go tests use the race detector where supported; Windows arm64 runs without it. The Windows arm64 Master, Agent and CLI are native arm64 binaries; its SeaweedFS fixture uses the official x64 build under Windows emulation because that fixture has no Windows arm64 release.

## Checks

| Check | What can make it fail |
| --- | --- |
| Archive and image identity | A wrong checksum, missing or duplicate member, wrong CPU architecture, mixed version/commit, or image containing different binaries. |
| State and request tests | Duplicate acceptance, stale-generation work after stop, inconsistent ledger/counters, cancellation races, invalid source identity, path escape or unsafe cleanup. Tests use local databases and controlled request order; protocol fakes cover error branches. |
| Initial setup and CLI | Administration before setup, repeated setup, invalid login/session handling, incorrect generated Agent release URLs, or lost users, node settings and limits after restart. |
| Three transfer directions | A missing, extra, corrupt or wrongly mapped target object; duplicate parent creation; wrong child totals; either selected Agent receiving no work. |
| Shared task bandwidth | Two concurrent 8 Mbps tasks exceed their individual shared budget across two Agents. Target growth is sampled while they run. |
| Stop and process recovery | Writes continue after stop acknowledgement, a restart clears stop intent, progress duplicates, an old destination is truncated, or final hashes/owned temporary-file cleanup fail after Agent SIGKILL. |
| Multipart crash cleanup | Restart leaves an owned incomplete upload or spool, or removes an unrelated upload or the previous completed object. This checks cleanup and object retry, not reuse of uploaded parts. |
| Reports | Rows, statuses, target mapping or totals differ from the verified file inventory. |
| Inventory tasks | A lost finalize response creates a second parent, restart loses an unsealed list, changed input is accepted under the same request ID, unlisted keys are copied, or a missing selected key is hidden. |
| Native installation | Wrong service PID/version, unexpected overwrite of an existing CLI, lost password or enrollment on upgrade, stopping a healthy service before rejecting bad checksums, failed rollback, or unsafe adoption of existing data. |
| Docker and Compose | Wrong runtime architecture, broken nonroot setup/enrollment, inaccessible bind mounts, lost state after recreation, or transferred bytes differ. |

The main transfer fixture contains 260 objects, including an empty object, a Unicode/space name, and two 17 MiB objects that exercise multipart upload. It crosses the 128-object dispatch boundary. Separate fixtures exercise shared bandwidth, stop/restart and inventory batches. SKIP and OVERWRITE are checked against existing destination data.

## Run locally

Build the current platform with `./build.sh --test`. Install the Python dependencies and prepare the checked package plus pinned S3 binary. Change `linux/amd64` to your platform:

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

Use fresh output directories. On Windows, use `.exe` filenames and your Python environment's executable. `--multipart-recovery` also runs the source-level Agent crash test against that S3 service, so Go must be available. The main transfer scenarios run the packaged executables with an empty executable PATH.

Service lifecycle tests use `go-install.py` with `--master`, `--agent`, `--cli`, `--repo` and `--output`. Linux needs root and systemd; Windows needs Administrator. macOS uses a user LaunchAgent by default, or a system LaunchDaemon with `--system`. They create isolated service names and paths and remove their own service registrations afterward.

`go-docker.py` tests the prepared image; `go-setup.py` tests the public installer against a local release directory. The exact image preparation and invocation are recorded in [ci.yml](../workflows/ci.yml).

## Evidence and scope

Each scenario records `evidence.json`, task or bandwidth samples, manifests where applicable and sanitized process logs. Package checks retain archive/version identity; image provenance links container executables to those archives. A failing assertion fails its CI job. Uploading diagnostic evidence does not make a failed run publishable.

SeaweedFS is a real S3 fixture with pinned download hashes. These results are not JD Cloud OSS signing, permission or throttling acceptance. Source-level HTTP fakes are used for controlled protocol errors; they are not described as real storage transfers.

CI does not establish power-loss durability, live-source incremental consistency or workload-wide performance parity. Performance comparison is separate: `go-compare.py` uses a fixed historical V4.0.x JAR package and Go package in the same fixture; `go-cloud-compare.py` adds cgroup limits and disk/OOM guards for an isolated Linux host. Keep the original V4.0.x artifacts and manifests when repeating those comparisons. The legacy exporter and real-V4.0.x fixture tools remain under `tools/legacy/`.
