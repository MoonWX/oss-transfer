# Releases

## Workflow

- CI builds the installation archive once, then builds Linux `amd64`/`arm64` images from that archive.
- Pull requests, pushes to `main` and manual runs test those same artifacts in parallel on native `ubuntu-latest` (amd64) and `ubuntu-26.04-arm` (ARM64) runners. Both run the full acceptance suite against an isolated S3 service. CI does not publish a release.
- The build job uploads candidate artifacts for the two test jobs to download by immutable ID and verify against the build SHA-256. Release can use them only after the entire `push`/`main` CI run succeeds; an uploaded artifact alone is not a passed release.
- Pushing a version tag starts Release. It waits for successful `push`/`main` CI for that exact commit, then downloads its artifacts by their fixed IDs. It does not compile or build again.
- Docker Hub and JD Cloud OSS publish in parallel on GitHub-hosted `ubuntu-latest` runners. Each downloads only the artifacts it needs; neither rebuilds them.
- GitHub Release is published after Docker Hub and OSS succeed.

If CI fails or its artifacts are missing or expired, rerun the `main` CI run for the tagged commit, then rerun Release. Do not use artifacts from a different commit or the latest `main` build.

## Tests

Each architecture runs the same acceptance chain using a real Master, two Agents, the packaged CLI, the packaged WebUI in Chromium, and a pinned official SeaweedFS S3 service. It uploads local files, copies them between buckets, downloads them, and independently compares every key, size and SHA-256. The same chain checks two-agent execution, task bandwidth, stop/resume, process termination and recovery, reports, and SKIP/OVERWRITE. See [the acceptance contract](acceptance/README.md) for exact assertions and limits.

The previous mock-based server, browser, CLI and publisher suites are not default CI gates. They remain available for focused development with `./build.sh --test`, `cd frontend && npm test`, and the individual deployment test scripts. There is no test-count target or path-based bypass. Acceptance failures preserve checks, independent manifests, task reports, logs and browser traces as `acceptance-evidence-amd64` and `acceptance-evidence-arm64`; either failure blocks release. One failed architecture does not cancel the other, so both results remain available.

The focused `StorageSynchronizationRaceTest` also runs in CI. It deterministically reproduces competing user and background configuration changes using real controllers, RocksDB and loopback HTTP, without waiting for a scheduling race to occur during a transfer.

## Publish a version

Change the project `<version>` in `pom.xml`, test and commit it on `main`. This is the version source for the JAR, CLI and package metadata; installers and Compose default to `latest`.

From that committed checkout:

```sh
version=$(sed -n 's/^[[:space:]]*<version>\([^<]*\)<\/version>.*/\1/p' pom.xml | head -n1)
git push origin main
git tag -a "v$version" -m "OSS Transfer v$version"
git push origin "refs/tags/v$version"
```

The tag must match the POM version. Use a new version and tag for each release; do not move an existing release tag.

Stable releases publish image tags `X.Y.Z`, `X.Y` and `latest`. Pre-releases publish only their version tag and are marked as pre-releases on GitHub.

OSS stores `oss-transfer.tar.gz`, its SHA-256 file and `VERSION` under `osstransfer/vX.Y.Z/`. Stable releases also update `osstransfer/latest/`; pre-releases only update their version directory.

## Repository configuration

| Destination | Address | Secrets |
| --- | --- | --- |
| Docker Hub | `moonwx/osstransfer` | `DOCKER_USERNAME`, `DOCKER_PAT` |
| JD Cloud OSS | `moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/` | `JDOSS_AK`, `JDOSS_SK` |

Publishing secrets are used only by the release workflow triggered by version tags. Pull-request CI does not use them.
