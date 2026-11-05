# Releases

[中文](RELEASING.zh-CN.md)

Push a `v*` tag to publish a version. The release workflow waits for CI on the tagged commit, takes the packages and image that CI built, uploads them to JD Cloud OSS and Docker Hub, and publishes the GitHub Release.

## Publish a version

1. Update `VERSION` to the release number, for example `4.1.3` or `4.1.3-rc.1`.
2. Commit the change.
3. Tag that commit and push the commit and the tag together.

```sh
version=$(cat VERSION)
git tag -a "v$version" -m "OSS Transfer v$version"
git push --atomic origin main "v$version"
```

`--atomic` updates both refs or neither. You do not need to wait for CI first: the release run waits up to 65 minutes for the `main` CI run on that commit and starts publishing as soon as it passes. If that CI run fails, the release run fails without publishing anything.

The tag must match `VERSION` and point to the commit you intend to publish. Use a new version number when changing a published release.

The GitHub Release is created as a draft while OSS and Docker Hub upload, and made public only after both succeed. A failed release run can leave a draft; the next run for that tag reuses it.

### Fix a release before it is published

If CI fails and a fix is needed, commit the fix and move the tag to it. Only do this while the GitHub Release is missing or still a draft; the new run overwrites any OSS files and Docker Hub tags an earlier run uploaded for that version. First cancel any release run for the tag that is still in progress, so it cannot publish the old commit:

```sh
gh run list --workflow release.yml --limit 5
gh run cancel <run-id>
```

Then move the tag and push it with the fix:

```sh
version=$(cat VERSION)
git tag -fa "v$version" -m "OSS Transfer v$version"
git push --atomic origin main "+refs/tags/v$version"
```

The `+` forces only the tag. The tag push starts a new release run, which waits for CI on the fix commit.

If CI failed for a reason unrelated to the code, rerun the failed CI jobs instead, then retry publication as described below.

For a change to `VERSION` alone, CI reuses passing tests from a code-identical ancestor on `main`. It still rebuilds all archives and the image, checks the packaged program versions, and verifies checksums and image identity. Changes to code, dependencies, installers or workflows run the default suite. If no passing baseline can be verified, CI runs the default suite.

Default CI tests ten platforms. FreeBSD arm64 archives are built and checked, but their runtime tests are optional. Run the complete FreeBSD arm64 suite separately with:

```sh
gh workflow run ci.yml --ref main -f freebsd_arm64=true
```

This runs one FreeBSD arm64 acceptance job without repeating the other platforms. It does not gate publication; releases use the default `main` CI run.

The release workflow selects artifacts by commit and CI run ID. Pull-request and manual CI runs are excluded. A cancelled version-only run can supply artifacts only when both build jobs passed, no test failed, and a code-identical ancestor has passing CI. Artifacts expire after seven days. If they have expired, rerun the matching CI run.

To retry publication for an existing tag using the current release workflow, run:

```sh
gh workflow run release.yml --ref main -f tag=v4.1.7
```

This keeps the tag and its artifacts unchanged.

## Publish a beta

A prerelease is any version with a SemVer prerelease suffix, such as `4.1.29-beta.1` or `4.1.29-rc.1`. Publish it exactly like a stable release: set `VERSION`, commit, tag `v4.1.29-beta.1` and push. Use dot-separated identifiers so ordering is predictable (`beta.2` before `beta.10`, `beta` before `rc`, every prerelease before `4.1.29`). Do not use build metadata (`+...`), leading zeros in numeric identifiers (`beta.01`), or a `-dev` suffix, which is reserved for unreleased builds; such versions cannot be installed online.

What a beta touches:

- GitHub creates the release as a **prerelease** and does not mark it latest.
- OSS uploads only the versioned directory `oss-transfer/v4.1.29-beta.1/`. `oss-transfer/latest/` keeps pointing at the last stable release.
- Docker Hub receives only the exact tag `4.1.29-beta.1`; `X.Y`, `latest` and other aliases stay on the stable release.

What it does not touch: default installer runs, **Check for updates** in the console, `update check` in the CLI and Compose files using `latest` all keep resolving the latest stable release. No installation is offered a prerelease.

To try a beta:

- New installation: run the installer with `--version 4.1.29-beta.1` (PowerShell `-Version 4.1.29-beta.1`), or set `OSS_TRANSFER_IMAGE=moonwx/osstransfer:4.1.29-beta.1` for Compose. See [installing a specific version](../docs/install.md#install-a-specific-or-prerelease-version).
- Existing managed installation: open `/tools?tab=updates&channel=prerelease` in the console, or run `oss-transfer-cli update master --version 4.1.29-beta.1 --allow-prerelease --yes`. Agents can then follow the Master's version. See [prerelease updates](../docs/operations.md#try-a-prerelease).

When the stable `4.1.29` is published, installations running any `4.1.29-*` prerelease are offered it by the normal update check, because a release outranks its prereleases.

## Published files

GitHub Release contains the eleven platform archives listed in [platforms.json](../deploy/native/platforms.json). Each archive includes Master, Agent, CLI, documentation and checksums. Installation scripts live in the repository.

OSS stores the same archives, three installation scripts, `manifest.tsv`, `release.json` and `SHA256SUMS`. The installer downloads one platform archive and extracts the service and CLI. For example, Linux amd64 uses `oss-transfer-linux-amd64.tar.gz`. The Docker image uses the tested Linux binaries.

Installers choose OSS for a mainland Chinese public IP and GitHub elsewhere. GitHub downloads use the release API's archive digest and the installer from the commit recorded in that archive; OSS uses its versioned manifest. Explicit source and local package options skip location detection.

| Version | OSS location | GitHub | Docker Hub tags |
| --- | --- | --- | --- |
| `vX.Y.Z` | `oss-transfer/vX.Y.Z/` | Release | `X.Y.Z`, `X.Y`, `latest` |
| `vX.Y.Z-rc.N`, `vX.Y.Z-beta.N` | `oss-transfer/vX.Y.Z-rc.N/` | Prerelease | `X.Y.Z-rc.N` |

Stable releases update four files under `oss-transfer/latest/`: `install.sh`, `Install.ps1`, `manifest.tsv` and `release.json`. Installers resolve this metadata to a fixed version before downloading an archive. Prereleases leave `latest` unchanged. Compose uses `latest` for stable releases and the exact image tag for a prerelease.

The old `osstransfer/` tree remains available for clients released before v4.1.8. New releases use `oss-transfer/`.

## Credentials

Set these repository secrets before releasing:

| Destination | Address | Secrets |
| --- | --- | --- |
| JD Cloud OSS | `moonwx.s3.cn-north-1.jdcloud-oss.com/oss-transfer/` | `JDOSS_AK`, `JDOSS_SK` |
| Docker Hub | `moonwx/osstransfer` | `DOCKER_USERNAME`, `DOCKER_PAT` |

Publishing uses GitHub-hosted `ubuntu-latest` runners. See [release.yml](workflows/release.yml) for the upload order and timeouts. OSS uploads the version directory in parallel and updates `latest/` only after every versioned file has been uploaded, with `release.json` last.
