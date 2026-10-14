# Releases

Push a `v*` tag to publish a version. The release workflow takes the packages and image from the successful `main` CI run for that commit, uploads them to JD Cloud OSS and Docker Hub, then creates the GitHub Release.

## Publish a version

1. Update `VERSION` to the release number, for example `4.1.3` or `4.1.3-rc.1`.
2. Commit the changes and push `main`.
3. Wait for CI to pass on that commit, including all native and container jobs.
4. Tag the tested commit and push the tag.

```sh
git push origin main
# Run the remaining commands after CI passes for HEAD.
version=$(cat VERSION)
git tag -a "v$version" -m "OSS Transfer v$version"
git push origin "refs/tags/v$version"
```

The tag must match `VERSION`. If more commits have landed since testing, check out the tested commit before tagging. Use a new version number when changing a published release.

The release workflow selects artifacts by commit and CI run ID. It accepts a successful CI run triggered by a push to `main`; pull-request and manual runs are excluded. Artifacts expire after seven days. If they have expired, rerun the matching CI run, then rerun Release.

## Published files

GitHub Release contains six archives for Linux, macOS and Windows on amd64 and arm64. Each archive includes Master, Agent, CLI, documentation and checksums.

OSS stores these archives plus the binaries and metadata used by the installer and Agent enrollment. Installation scripts are maintained in the repository. The Docker image uses the tested Linux binaries.

| Version | OSS location | GitHub | Docker Hub tags |
| --- | --- | --- | --- |
| `vX.Y.Z` | `osstransfer/vX.Y.Z/` | Release | `X.Y.Z`, `X.Y`, `latest` |
| `vX.Y.Z-rc.N` | `osstransfer/vX.Y.Z-rc.N/` | Prerelease | `X.Y.Z-rc.N` |

Stable releases also update `osstransfer/latest/`. Installers resolve this metadata to a fixed version before downloading binaries. Prereleases leave `latest` unchanged. Compose uses `latest` for stable releases and the exact image tag for a prerelease.

## Credentials

Set these repository secrets before releasing:

| Destination | Address | Secrets |
| --- | --- | --- |
| JD Cloud OSS | `moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/` | `JDOSS_AK`, `JDOSS_SK` |
| Docker Hub | `moonwx/osstransfer` | `DOCKER_USERNAME`, `DOCKER_PAT` |

Publishing uses GitHub-hosted `ubuntu-latest` runners. See [release.yml](workflows/release.yml) for the upload order and timeouts.
