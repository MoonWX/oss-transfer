# Releases

## Workflow

- Pull requests run CI tests on GitHub-hosted runners.
- Pushes to `main` run the tests and build the installation archive and Linux `amd64`/`arm64` image once. CI stores them as Actions artifacts; it does not publish a release.
- Pushing a version tag starts Release. It waits for successful `push`/`main` CI for that exact commit, then downloads its artifacts by their fixed IDs. It does not compile or build again.
- Docker Hub publication runs on a GitHub-hosted runner. JCR and OSS publication runs on the existing runner in China, which downloads, verifies and uploads the same artifacts.
- GitHub Release is published after Docker Hub, JCR and OSS succeed.

If CI fails or its artifacts are missing or expired, rerun the `main` CI run for the tagged commit, then rerun Release. Do not use artifacts from a different commit or the latest `main` build.

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
