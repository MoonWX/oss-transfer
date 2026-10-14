# Upgrade from V4.0.x to V4.1.0

V4.1.0 uses separate Go Master and Agent processes, plus a CLI. The Master handles the console, accounts, storage configuration, scheduling and reports. Agents scan storage, transfer objects, apply limits and save progress. A single-host deployment therefore needs both a Master and an Agent, with separate data directories.

## New installation

Use the [main deployment instructions](../README.md) for native services or Docker Compose. Linux, macOS and Windows have amd64 and arm64 packages. The Master embeds the WebUI; neither service requires Java.

First setup uses a token from the Master log to create the administrator and public URL. There is no default password. Agents enroll through single-use invitations and retain their identities across restarts. Use the same version on Master and Agents. The [native guide](../deploy/go/README.md) covers custom ports, service registration, upgrades and rollback.

## Convert V4.0.x data

Go uses its own databases and cannot open V4.0.x RocksDB directly. Follow the [offline conversion procedure](development/legacy-migration.md):

1. Stop the old Master and every execution node. Keep consistent copies of their databases and configuration.
2. Export each node with the offline legacy tool, including the old Master if it also performed transfers.
3. Inspect the exports and create an explicit migration plan with each node's effective settings and final data path.
4. Prepare and finalize into a new directory. Adopt the resulting `service.json` files with the native installers.

The Java exporter is an offline conversion tool that uses the old JAR's RocksDB library. It is not a Go runtime dependency. Conversion preparation currently runs on Linux or macOS.

The conversion retains accounts, storage connections, matching Agent identities, limits, task history and recycle-bin state. Login sessions are invalidated. The plan requires explicit acceptance of Go retry, concurrency and multipart defaults; those settings are not silently copied from Java.

Existing enrolled Agents retain their identity only when their private credential matches the Master snapshot. Old directly addressed workers need explicit identity mappings. Finalized Agent data must be placed at its recorded absolute path on the execution host. Do not overwrite or point Go at the original V4.0.x database.

## Resume after conversion

Unfinished tasks remain stopped. Their first explicit resume scans the original task scope, preserving ranges, inventories, filters and target mapping. Old successful objects are reused only after complete source and destination reads produce matching SHA-256 values and stable identities. Missing sources become failures; unequal existing targets follow the saved conflict policy. Historical exclusions remain excluded.

Completed historical tasks keep their recorded state and report coverage. They are not relabeled as freshly verified. Interrupted multipart objects restart because V4.0.x did not persist enough part state to continue them.

For ordinary Go recovery, stop intent is durable before cancellation. Object results and counters commit together. A crash after writing a destination but before committing its result can cause that object to be handled again under the conflict policy. Temporary files and multipart uploads are cleaned only when their recorded ownership can be verified.

## Validation boundaries

The migration fixture uses a real V4.0.6 RocksDB export and isolated SeaweedFS storage. After conversion, native services retained the old login, node identities and limits, resumed two interrupted tasks and independently verified all 520 resulting objects. This verifies that fixture; it does not establish provider compatibility or performance for every workload.

The [acceptance inventory](go-migration-acceptance.md) tracks the broader migration requirements. Native CI exercises packaged binaries on all six platforms; container acceptance runs on both Linux architectures.

On an isolated cloud host limited to two CPU cores, five paired downloads of 16 × 48 MiB objects from local SeaweedFS took a median 10.519 seconds with Java V4.0.x and 10.110 seconds with the optimized Go build. Every destination passed SHA-256 verification. These measurements cover that download workload; JD OSS, JPFS and other workloads require separate performance validation.
