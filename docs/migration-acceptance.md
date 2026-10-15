# Go migration test record

V4.1.0 replaced the Java service with Go Master, Agent and CLI binaries. This record covers the migration checks and performance measurements. Current release tests and commands are in the [acceptance guide](../.github/acceptance/README.md).

## Functional checks

| Check | Recorded result |
| --- | --- |
| Native packages | Master, Agent and CLI passed on Linux, macOS and Windows, on amd64 and arm64. The transfer processes ran with an empty executable PATH. |
| Containers | Both Linux image architectures passed setup, enrollment, bind-mounted transfers and state preservation after recreation. |
| Public installation | Packages downloaded from JD Cloud OSS passed checksum verification and were installed as isolated macOS user services. Generated Agent enrollment commands worked. Restart retained the account, storage connections, Agent identity and tasks. |
| Browser workflow | Setup, login, Agent invitation, storage configuration, all three transfer directions, child details and report download passed. Destination data and the downloaded report were checked separately. |
| V4.0.6 conversion | A real RocksDB export retained login details, node identities and limits. Two interrupted tasks resumed after service adoption; 520 destination objects passed hash checks. |

The macOS installation check used `--user` and separate directories instead of the generated command's `sudo` default. Its test services were removed afterward. Browser testing covered the transfer workflow; a full mobile and theme review was not part of this run.

## Existing V4.0.x data

Use the [offline conversion tools](development/legacy-migration.md) to import RocksDB data. Conversion requires node and path mappings and writes a new Go data directory. Keep the original database and V4.0.x package for rollback.

Historical records keep their existing object detail. Interrupted multipart uploads restart the affected object from the beginning, both after conversion and during normal Go recovery.

## Performance measurements

The Java baseline was V4.0.6, commit `835a1d899fea00a9af60fe3cb69032ccf65a2144`, archive SHA-256 `64a0e80a28d61322fad20be17c8d47c31f77565a76daf5b1f4718590f9c992d4`. Tests used an isolated cloud host's root disk and loopback SeaweedFS, limited to two CPU cores and 3 GiB memory with no swap. Every destination file was read and hashed after timing.

Download profiling found repeated database transactions for unchanged control-plane state. The fix skips those no-change transactions while retaining file sync, directory sync and database writes for actual changes.

| Comparison | Baseline median | Corrected Go median | Workload |
| --- | ---: | ---: | --- |
| Java V4.0.6 vs Go | 10.519 s | 10.110 s | Five paired downloads of 16 × 48 MiB; 160 destination hashes checked. |
| Original Go vs corrected Go | 10.979 s | 9.646 s | Three paired downloads of the same fixture; 96 destination hashes checked. |

These runs used a test build containing the transaction fix. In the Go-to-Go comparison, median CPU time rose from 1.93 to 2.26 seconds and peak application RSS from 174.0 to 182.4 MiB.

A separate upload of 100,000 × 4 KiB files took 150.662 seconds in Go and 676.330 seconds in Java, with all hashes checked. These two runs occurred on different days; use paired repeats before treating their ratio as a performance estimate.

## Remaining checks

Provider testing still needs JD Cloud OSS signing, permissions and throttling, plus JPFS shared-directory behavior. SeaweedFS was the storage service for the measurements above.

The proposed 10,000 × 1 MiB, 8 × 2 GiB and mixed-size benchmarks were not completed. Power-loss recovery, repeated network failures, sustained 503 responses and full destination disks also need system-level testing. Continuous migration while applications modify source data needs a separate consistency design.
