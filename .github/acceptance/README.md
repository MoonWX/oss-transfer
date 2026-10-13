# Release acceptance

The tests install the release package and exercise its JAR, Linux CLI and embedded WebUI. An S3 client prepares conflict fixtures and checks transferred content against the source files.

CI runs the same suite in parallel on native amd64 (`ubuntu-latest`) and ARM64 (`ubuntu-26.04-arm`) runners. Each job downloads the package and image archive by artifact ID, checks their SHA-256 against the build outputs, and verifies the host and container architecture. Both jobs must pass before the release workflow can publish those artifacts.

Before acceptance, `StorageSynchronizationRaceTest` checks four configuration races: duplicate synchronization during create/delete, stale updates, and duplicate POST rejection. It uses controllers, RocksDB and loopback HTTP, with latches to control request order. S3 client construction is stubbed in these tests.

## Checks

| Scenario | Pass condition |
| --- | --- |
| Installer cancellation and checksum | Declining confirmation causes no download or installation. An archive with an incorrect checksum is rejected before installation. |
| Master installation | The installer starts systemd and Compose Masters, and their generated passwords authenticate. Reinstalling over the native deployment leaves its password, configuration and process unchanged. The Compose Master's local mount supports file writes. |
| Native upgrade | The existing password, database records, configuration and CLI remain usable. An unbootable replacement triggers rollback and the old service accepts its original login. The same checks run against the legacy directory layout. |
| Agent installation | Native and Compose Agents enroll through invitations and report to the Master. The native Agent downloads its bundle and checksum from the Master; its installed JAR matches the release JAR. |
| Startup and access | The Master accepts login, rejects unauthenticated task requests, and receives status from both Agents. The packaged CLI logs in with its own user session. |
| CLI upload | Both Agents transfer files. Destination keys, lengths and SHA-256 match the source manifest. The report is complete and contains exactly the source keys. |
| Browser copy | Chromium creates one task through the packaged WebUI. The selected Agents, mapping and limit reach the API; “View task” opens that task. Both Agents transfer files and destination hashes match. |
| Shared bandwidth | Each Agent receives about 18 MiB. At an 8 Mbps task limit, elapsed time from creation to completion is at least 80% of the theoretical transfer time. |
| Stop and recovery | Stop a 12 MiB overwrite after its temporary file reaches 6–12 MiB and verify the previous destination survives. Kill and restart the Master; the task stays stopped. Resume, kill and restart an Agent during another partial download, then finish the same task. Final hashes match, completed checkpoint files retain their inode and mtime, and temporary transfer files are removed. |
| Conflict policies | SKIP preserves existing destination bytes; OVERWRITE replaces them with source bytes. Reports match the files. |

The dataset contains 130 files totaling about 36 MiB, including empty files, names with spaces and Unicode, four 6 MiB objects, and one 12 MiB object. It crosses the 128-object dispatch boundary and gives both Agents approximately equal byte counts. An 8 MiB multipart threshold and 5 MiB part size exercise multipart transfers; checks cover the resulting ETag and content hash. Small LIST pages exercise executor pagination. The explorer's 1,000-key page boundary is outside this suite.

## Test environment

The S3 server uses the [official SeaweedFS image](https://github.com/seaweedfs/seaweedfs), version 4.48, pinned by digest in `run.py`. It stores the transferred objects and rejects an invalid request signature during setup. This covers S3 transfers against SeaweedFS. JD Cloud OSS signing, permissions and throttling still need tests against JD Cloud.

Each run creates its own credentials, Docker resources and temporary directories. Installer tests use separate systemd services, Compose projects and ports; a localhost HTTP server serves the archive for the checksum test. Agents share a transfer directory and keep separate databases. Cleanup removes the resources created by the run.

Chromium loads the WebUI from the JAR. The CLI runs directly from the release image with an isolated user session. When downloaded-file permissions prevent host access, the verifier reads through a read-only container mount. It computes hashes separately from Transfer and checks inode and mtime on the shared files.

## Run locally

Install Docker, Node.js and Python 3.9 or newer. Build the release with `./build.sh`, then build an image for the host architecture. On amd64:

```sh
docker build --platform linux/amd64 --build-arg TARGETARCH=amd64 -t oss-transfer:acceptance .
python3 -m venv /tmp/oss-acceptance-venv
/tmp/oss-acceptance-venv/bin/pip install -r .github/acceptance/requirements.txt
(cd frontend && npm ci && npx playwright install --with-deps chromium)
/tmp/oss-acceptance-venv/bin/python .github/acceptance/run.py \
  --image oss-transfer:acceptance --evidence output/acceptance/local
```

On ARM64, change the build options to `--platform linux/arm64 --build-arg TARGETARCH=arm64`.

The installer tests also require Linux with systemd, Java 8+, Docker Compose, and passwordless sudo or root:

```sh
python3 .github/acceptance/quick-install.py \
  --package output/oss-transfer --image oss-transfer:acceptance \
  --evidence output/acceptance/local
python3 .github/acceptance/native-upgrade.py \
  --package output/oss-transfer --evidence output/acceptance/local
```

The legacy upgrade fixture uses the current JAR in the old directory layout. Add `--legacy-jar /path/to/old.jar` to check a historical binary against a fresh test database.

CI allows 8 minutes for installer tests and 12 minutes for transfer tests. Initial Master and Agent readiness each allow 120 seconds. An architecture failure does not cancel the other job.

## Results and scope

The evidence directory contains:

- `installer-checks.json` and `installer.log`: installation assertions and sanitized service output.
- `native-upgrade-checks.json` and `native-upgrade.log`: upgrade preservation and rollback results.
- `artifact-identity.json`: runner architecture, commit and build artifact hashes.
- `checks.json`, file manifests and reports: assertion results, timing and content hashes.
- Container logs and task snapshots, plus traces for failed browser tests.

The bandwidth check detects large regressions, such as losing the shared quota. Scheduling delays can hide smaller overruns. The suite does not cover changing-source incremental consistency, power-loss durability or large-scale performance.
