# Releases

Push a `v*` tag to publish a version. The release workflow takes the packages and image built for that commit, uploads them to JD Cloud OSS and Docker Hub, then creates the GitHub Release.

## Publish a version

1. Update `VERSION` to the release number, for example `4.1.3` or `4.1.3-rc.1`.
2. Commit the changes and push `main`.
3. Tag that commit and push the tag. Release waits for its CI artifacts.

```sh
git push origin main
version=$(cat VERSION)
git tag -a "v$version" -m "OSS Transfer v$version"
git push origin "refs/tags/v$version"
```

The tag must match `VERSION` and point to the commit you intend to publish. Use a new version number when changing a published release.

For a change to `VERSION` alone, CI reuses passing tests from a code-identical ancestor on `main`. It still rebuilds all archives and the image, checks the packaged program versions, and verifies checksums and image identity. Changes to code, dependencies, installers or workflows run the full suite. If no passing baseline can be verified, CI runs the full suite.

The release workflow selects artifacts by commit and CI run ID. Pull-request and manual CI runs are excluded. A cancelled version-only run can supply artifacts only when both build jobs passed, no test failed, and a code-identical ancestor has passing CI. Artifacts expire after seven days. If they have expired, rerun the matching CI run.

To retry publication for an existing tag using the current release workflow, run:

```sh
gh workflow run release.yml --ref main -f tag=v4.1.7
```

This keeps the tag and its artifacts unchanged.

## Published files

GitHub Release contains the eleven platform archives listed in [platforms.json](../deploy/native/platforms.json). Each archive includes Master, Agent, CLI, documentation and checksums. Installation scripts live in the repository.

OSS stores the same archives, three installation scripts, `manifest.tsv`, `release.json` and `SHA256SUMS`. The installer downloads one platform archive and extracts the service and CLI. For example, Linux amd64 uses `oss-transfer-linux-amd64.tar.gz`. The Docker image uses the tested Linux binaries.

Installers choose OSS for a mainland Chinese public IP and GitHub elsewhere. GitHub downloads use the release API's archive digest and the installer from the commit recorded in that archive; OSS uses its versioned manifest. Explicit source and local package options skip location detection.

| Version | OSS location | GitHub | Docker Hub tags |
| --- | --- | --- | --- |
| `vX.Y.Z` | `osstransfer/vX.Y.Z/` | Release | `X.Y.Z`, `X.Y`, `latest` |
| `vX.Y.Z-rc.N` | `osstransfer/vX.Y.Z-rc.N/` | Prerelease | `X.Y.Z-rc.N` |

Stable releases update four files under `osstransfer/latest/`: `install.sh`, `Install.ps1`, `manifest.tsv` and `release.json`. Installers resolve this metadata to a fixed version before downloading an archive. Prereleases leave `latest` unchanged. Compose uses `latest` for stable releases and the exact image tag for a prerelease.

## Credentials

Set these repository secrets before releasing:

| Destination | Address | Secrets |
| --- | --- | --- |
| JD Cloud OSS | `moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/` | `JDOSS_AK`, `JDOSS_SK` |
| Docker Hub | `moonwx/osstransfer` | `DOCKER_USERNAME`, `DOCKER_PAT` |

Publishing uses GitHub-hosted `ubuntu-latest` runners. See [release.yml](workflows/release.yml) for the upload order and timeouts.
