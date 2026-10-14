# Go migration verification

V4.1.0 uses separate Go Master, Agent and CLI executables. Release requires successful CI for the exact tagged commit and publishes those tested artifacts without rebuilding them. The [acceptance guide](../.github/acceptance/README.md) lists the current scenarios and local commands.

## Release gates

| Gate | Evidence |
| --- | --- |
| Package identity | Six native archives and both Linux image architectures have matching version, full commit, executable format and SHA-256 checksums. Mixed or corrupt artifacts are rejected. |
| Native operation | Packaged Master, Agent and CLI run on Linux, macOS and Windows, on amd64 and arm64. Transfer processes run with an empty executable PATH. |
| Setup and membership | Protected first setup, user login, two outbound Agents, restart persistence and credential revocation. Generated Agent commands use the Master's fixed release. |
| Transfer correctness | Local→OSS, OSS→local and OSS→OSS; exact destination keys, lengths and SHA-256 checked by an independent S3 client. Naming, SKIP/OVERWRITE, dispatch boundaries and child totals are checked. |
| Limits and recovery | Two simultaneous 8 Mbps tasks share their own budgets across two Agents. Physical destination growth is sampled. Stop intent, Agent termination, completed checkpoints and owned temporary-file/multipart cleanup are exercised. |
| Inventory and reports | Interrupted list import, lost batch/finalize responses, process restart, duplicate submission, missing source keys and report reconciliation. |
| Installation | Native service registration, PID/version checks, state-preserving upgrades, corrupt-binary refusal before stopping the service, rollback and adoption of existing Go data. |
| Containers | The archived image and Compose installer run on both Linux architectures, including nonroot storage mounts and state preservation. |

Each scenario writes evidence and sanitized logs. An assertion failure fails its job. All six native jobs and both container jobs must pass; a successful build alone cannot publish a release. Windows arm64 runs native Go executables without the unsupported race detector. Its SeaweedFS fixture uses the official Windows x64 binary under OS emulation.

The main transfer fixture has 260 objects, including empty, Unicode/space and multipart-sized objects. Separate tests cover larger listings, inventory batches, shared bandwidth and process recovery. Source-level tests exercise database transactions, stale generations, path confinement and controlled protocol errors; they are distinct from the black-box transfer checks.

## Recorded implementation checks

The Go implementation passed the six native jobs, both container jobs, packaging and image verification. These records describe the implementation checks; each release still needs the gates below on its own version and commit.

A separate public-download check installed Go from JD Cloud OSS into isolated macOS user services. The installed Master, Agent and CLI matched the published checksums. Initial setup, the actual generated Agent bootstrap, three transfer directions and reports passed; restarting retained the account, storage configuration, Agent identity and tasks. Both transfer services also ran with an empty PATH. The test used `--user` and private paths instead of the generated command's default `sudo` wrapper, and removed its services afterward.

Browser checks covered initial setup, login, Agent invitation, storage configuration, creation and completion of all three transfer directions, child details and report download. Destination data and the downloaded report were checked independently. This was not a complete mobile/theme visual sweep. Later Go-only frontend changes passed the packaged-build and API checks.

V4.1.0 must pass CI on its own commit, followed by publication and verification of the public `latest` installer. Earlier implementation checks do not substitute for those gates.

## Existing V4.0.x data

The [offline conversion tools](development/legacy-migration.md) use a real V4.0.6 RocksDB export. The migration fixture retained the old login, node identities and limits, adopted the converted native services, resumed two interrupted tasks and independently checked 520 destination objects. Completed historical records retain their original coverage; they are not presented as newly verified.

Conversion writes to a new directory and requires explicit node/path mappings. The original V4.0.x database remains available for rollback. Go cannot open RocksDB directly. An unfinished multipart object restarts; neither migration nor ordinary recovery claims to reuse already uploaded multipart parts.

## Performance observations

The Java baseline is V4.0.6, commit `835a1d899fea00a9af60fe3cb69032ccf65a2144`, archive SHA-256 `64a0e80a28d61322fad20be17c8d47c31f77565a76daf5b1f4718590f9c992d4`. Measurements used an isolated cloud host's root disk and loopback SeaweedFS, with two CPU cores, a 3 GiB cgroup memory limit and no swap. Destination contents were fully read and checked after timing.

An earlier Go build was slower on downloads. Investigation found repeated durable transactions for unchanged control-plane observations. The correction skips only those precisely identified no-change transactions; it preserves file sync, directory sync and all real database writes.

| Comparison | Java or original Go median | Corrected Go median | Scope |
| --- | ---: | ---: | --- |
| Java V4.0.x vs corrected Go | 10.519 s | 10.110 s | Five paired downloads, 16 × 48 MiB each; 160 destination hashes checked. |
| Original Go vs corrected Go | 10.979 s | 9.646 s | Three paired downloads of the same fixture; 96 destination hashes checked. |

Performance measurements used an isolated build of the transaction correction rather than a release archive. In the Go-to-Go comparison, CPU median increased from 1.93 to 2.26 seconds and peak application RSS from 174.0 to 182.4 MiB. No durability sync was removed. A fixture-seeding timeout invalidated one attempted pair before the measured Go task started; both sides of that pair were rerun and the failed attempt was retained.

A separate 100,000 × 4 KiB upload took 150.662 seconds in Go and 676.330 seconds in Java, with every hash checked. Those observations were collected on different days and are not an interleaved repeated benchmark. They do not establish a general speedup factor.

## Limits of this evidence

SeaweedFS transfer acceptance does not establish JD Cloud OSS signing, permissions, provider throttling or JPFS shared-filesystem behavior. Publishing packages to JD Cloud OSS tests the publishing path, not the application's S3 transfer compatibility.

The initial proposed 10,000 × 1 MiB, 8 × 2 GiB and mixed-size performance matrix across every direction was not completed. The recorded workloads support only their specific conclusions. There is no claim of equal performance across every storage system or workload.

Controlled error tests do not amount to a complete system-level matrix of network interruption, repeated 503 responses and exhausted destination filesystems. Power-loss durability and live-source incremental consistency are also outside this acceptance record. Service tests ran in isolated environments; publication does not migrate an existing production installation.
