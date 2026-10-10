# Release acceptance

The test subject is the built release: its JAR, Linux CLI and embedded WebUI. Tests do not import application classes, replace HTTP responses, or seed successful task records. An independent S3 client prepares conflict fixtures and verifies results; Transfer itself must perform the main upload, copy and download.

## Required outcomes

| Scenario | Pass condition |
| --- | --- |
| Startup and access | A real Master accepts login, rejects an unauthenticated task request, and receives live status from both enrolled Agents. The packaged CLI logs in using its own user session. |
| CLI upload | Both Agents transfer real payload. Independently enumerated S3 keys, lengths and SHA-256 match the source manifest. The report is complete and contains exactly those source keys. |
| Browser copy | Chrome loads the JAR's actual UI and creates one task through the form. The selected Agents, mapping and limit reach the real API; “View task” opens that task. Both Agents transfer payload and independent destination hashes match. |
| Shared bandwidth | The two Agents each receive about 18 MiB. At 8 Mbps total, elapsed time from the real creation request to completion must be at least 80% of the payload's theoretical transfer time. |
| Stop and recovery | During a real 12 MiB overwrite, wait for a 6–12 MiB temporary file before stopping. The previous destination survives. SIGKILL/restart the Master and verify the task remains stopped. Resume, wait for another partial large download, SIGKILL/restart the Agent, and finish the same task. Final hashes match; completed checkpoint files keep their inode and mtime; no owned partial files remain. |
| Conflict policies | Two small real tasks encounter different existing destination bytes. SKIP preserves them; OVERWRITE replaces them with source bytes. Reports agree with the observed results. |
| ARM64 | Start an actual Master and Agent from the ARM64 image and upload a 64 KiB file using the packaged ARM64 CLI. Independently read and hash the result. |

The main dataset is 130 files, about 36 MiB: empty, small, Unicode/space names, four 6 MiB objects and one 12 MiB object. Padding keys deliberately cross the application's 128-object dispatch boundary with approximately equal bytes on each Agent. CI lowers the multipart threshold to 8 MiB and part size to 5 MiB, then checks the S3 service's multipart ETag as well as the resulting content hash. Small LIST pages exercise executor pagination; the explorer's separate 1,000-key page boundary is not covered.

## Dependencies and isolation

S3 is the [official SeaweedFS image](https://github.com/seaweedfs/seaweedfs), release 4.48, fixed to the multi-architecture digest in `run.py`. It handles actual signed HTTP requests and stores actual object bytes; setup also verifies that an invalid signature is rejected. Credentials, Docker network, container names, volumes and data directories are created for each run. Agents share the transfer directory but have separate databases. Cleanup names only resources created by that run.

The browser uses no Vite development server and no intercepted responses. The CLI is the binary inside the release image, called directly rather than through the wrapper that supplies an internal token. It has an isolated user session.

On Linux, downloaded files retain the Agent's private permissions. When the host cannot read them, the independent verifier reads their bytes through a read-only container mount. It does not change output permissions; hashes are still computed outside Transfer, and inode/mtime checks use the actual shared files.

## Run locally

Requires Docker, Node.js and Python 3.9 or newer. Build the release with `./build.sh`, then build an image for the test host architecture; for an amd64 host:

```sh
docker build --platform linux/amd64 --build-arg TARGETARCH=amd64 -t oss-transfer:acceptance .
python3 -m venv /tmp/oss-acceptance-venv
/tmp/oss-acceptance-venv/bin/pip install -r .github/acceptance/requirements.txt
(cd frontend && npx playwright install chrome)
/tmp/oss-acceptance-venv/bin/python .github/acceptance/run.py \
  --image oss-transfer:acceptance --evidence output/acceptance/local
```

Use `--quick` only for the documented second-architecture small-file check. It is not a replacement for the main chain. CI has explicit phase timeouts, no browser retries and no fallback to an older successful artifact. A failed recovery cannot be hidden by creating another task.

Initial Master and Agent readiness each allow 120 seconds. CI uses `--startup-timeout 240` for ARM64 under QEMU because JVM cold startup can approach two minutes. Transfer and recovery deadlines are unchanged, and the entire ARM64 step remains limited to eight minutes.

## Evidence and limits

`checks.json` records completed assertions and measured values; expected and actual manifests record independent content hashes. Reports, sanitized container logs, task snapshots and failed-browser traces are retained. A successful API response or a matching count alone is insufficient.

The bandwidth assertion catches large regressions such as losing a shared quota, but scheduling delays can mask smaller overspeed; it is not a precision network benchmark. SeaweedFS is not JD Cloud OSS: provider-specific signing, permissions, throttling and WAN failures require separate provider acceptance. This chain does not certify changing-source incremental consistency, power-loss durability, PB-scale performance, all key shapes, or traditional SSH/systemd installer behavior. The full recovery scenario runs on amd64; ARM64 gets the stated small transfer.
