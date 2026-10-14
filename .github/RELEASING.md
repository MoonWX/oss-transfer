# Releases

A version tag publishes the already tested artifacts from the successful `main` CI run for that exact commit. Release does not rebuild them. It uploads native packages to JD Cloud OSS and the optional container image to Docker Hub, then creates the GitHub Release.

`VERSION` is the version source. `ci.yml` builds six native packages and one multi-architecture image, then tests them. All native and container acceptance jobs must pass before Release can resolve the immutable artifact IDs. A successful package job alone does not authorize publication.

## Publish a version

Commit intermediate changes to `main` normally; these commits run CI without publishing. When the changes are ready:

1. Update `VERSION`, such as `4.1.0-rc.1` for a candidate or `4.1.0` for a stable release.
2. Test and commit that change, then push `main`.
3. Wait for the complete CI run on that commit to pass.
4. Create and push an annotated tag pointing to that commit.

```sh
git push origin main
# After the matching CI run passes:
version=$(cat VERSION)
git tag -a "v$version" -m "OSS Transfer v$version"
git push origin "refs/tags/v$version"
```

Check that `HEAD` is the tested commit before tagging. The version in the tag must match `VERSION`. Use a new tag for each release; do not move an existing published tag.

Release waits for a successful `push` to `main` CI run at the tagged commit. A manual workflow run or a passing run at another commit cannot substitute. If tests fail or artifacts expire, rerun the matching main CI run before retrying Release.

## Outputs

GitHub and OSS provide all six platform archives, individual Master/Agent/CLI binaries, Unix and Windows installers, Compose, version metadata and SHA-256 checksums. The image contains the same tested Linux executables; Docker packaging does not compile another copy.

| Release | OSS directory | GitHub | Docker Hub tags |
| --- | --- | --- | --- |
| Stable `vX.Y.Z` | `osstransfer/vX.Y.Z/` and `osstransfer/latest/` | Release | `X.Y.Z`, `X.Y`, `latest` |
| Candidate `vX.Y.Z-rc.N` | `osstransfer/vX.Y.Z-rc.N/` | Prerelease | `X.Y.Z-rc.N` |

Prereleases do not update `latest`. Native installers resolve `latest` metadata to a fixed version before fetching binaries and verify that the manifest has not changed during resolution. The optional Compose file defaults to `latest`; a prerelease download contains Compose pinned to its candidate image.

After uploading a stable Go version and its `latest` metadata, publication retires only the obsolete V4.0.x `latest/oss-transfer.tar.gz`, its `.sha256`, and `latest/install.sh.sha256`. Existing `v4.0.*` directories remain available.

## Secrets

| Destination | Address | Repository secrets |
| --- | --- | --- |
| JD Cloud OSS | `moonwx.s3.cn-north-1.jdcloud-oss.com/osstransfer/` | `JDOSS_AK`, `JDOSS_SK` |
| Docker Hub | `moonwx/osstransfer` | `DOCKER_USERNAME`, `DOCKER_PAT` |

Publishing runs on GitHub-hosted `ubuntu-latest` runners. Publishing credentials are used only by the release workflow.
