# Releases

## Workflow

- CI builds the installation archive once, then builds Linux `amd64`/`arm64` images from that archive.
- Pull requests, pushes to `main` and manual runs exercise those artifacts against a real, isolated S3 service. Only successful pushes to `main` upload release artifacts; CI does not publish a release.
- Pushing a version tag starts Release. It waits for successful `push`/`main` CI for that exact commit, then downloads its artifacts by their fixed IDs. It does not compile or build again.
- Docker Hub publication runs on a GitHub-hosted runner. JCR and OSS publication runs on the existing runner in China, which downloads, verifies and uploads the same artifacts.
- GitHub Release is published after Docker Hub, JCR and OSS succeed.

If CI fails or its artifacts are missing or expired, rerun the `main` CI run for the tagged commit, then rerun Release. Do not use artifacts from a different commit or the latest `main` build.

## Tests

CI runs one acceptance chain using a real Master, two Agents, the packaged CLI, the packaged WebUI in Chrome, and a pinned official SeaweedFS S3 service. It uploads local files, copies them between buckets, downloads them, and independently compares every key, size and SHA-256. The same chain checks two-agent execution, task bandwidth, stop/resume, process termination and recovery, reports, and SKIP/OVERWRITE. An ARM64 check performs a small real upload as well. See [the acceptance contract](acceptance/README.md) for exact assertions and limits.

The previous mock-based server, browser, CLI and publisher suites are not default CI gates. They remain available for focused development with `./build.sh --test`, `cd frontend && npm test`, and the individual deployment test scripts. There is no test-count target or path-based bypass. Acceptance failures preserve checks, independent manifests, task reports, logs and browser traces as `acceptance-evidence`; they prevent release artifact upload.

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
| JD Cloud JCR | `moonwx-cn-north-1.jcr.service.jdcloud.com/osstransfer` | `JCR_USERNAME`, `JCR_PASSWD` |
| JD Cloud OSS | `moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/` | `JDOSS_AK`, `JDOSS_SK` |

JD Cloud jobs use runner labels `[self-hosted, linux, x64, jdcloud-publish]`. Set the Actions variable `JD_PUBLISH_RUNNER_LABEL` to override the last label. Pull-request jobs never use this runner or publishing secrets.
