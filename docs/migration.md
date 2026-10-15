# Upgrade from V4.0.x to V4.1.x

V4.0.x data needs an offline conversion before it can be used by V4.1.x. The converter keeps accounts, storage connections, node limits, task history and the recycle bin. Unfinished tasks enter the new installation stopped.

For a fresh installation, follow the [deployment instructions](../README.md). For an existing V4.1.x installation, use the [native upgrade command](../deploy/native/README.md#upgrade).

## Convert an existing installation

1. Stop the old Master and all execution nodes. Back up each node's database and configuration, including environment and command-line overrides.
2. Export every node's RocksDB data, including the old Master if it performed transfers.
3. Create a migration plan with the node identities, effective limits, Master URL and final data paths.
4. Prepare and finalize the conversion in a new directory.
5. Install the native services with the generated `service.json` files. Confirm the Agents are online, then resume the tasks you want to continue.

The [conversion guide](development/legacy-migration.md) contains the commands and plan format. Preparation runs on Linux or macOS. Keep the original installation and backups until the new services and task results have been checked.

V4.1.x separates scheduling from execution. Install a Master and an Agent on a single-host system; they use separate data directories. In a cluster, place each finalized Agent directory on its execution host at the absolute path recorded in its configuration.

Existing Agents retain their identities when their private credentials match the Master snapshot. Directly addressed V4.0.x workers receive new identities through explicit entries in the migration plan. Sign in with the existing account password after conversion; old login sessions expire.

## Resume unfinished tasks

The first resume scans the task's original scope with its saved ranges, inventory, filters and target naming. Before reusing an old successful result, the Agent reads the current source and destination, compares SHA-256 hashes and checks their identities for changes during the reads.

A missing source becomes a failed item. An unequal existing target follows the saved conflict policy: `SKIP` leaves it untouched; `OVERWRITE` permits another write. Historical time-filter exclusions stay excluded. Interrupted multipart objects restart at object level because the old database lacks the upload IDs and completed-part lists needed to continue them.

Completed tasks retain their historical results and report coverage. New transfer attempts record their own results. Review the retry, concurrency and multipart defaults in the conversion guide before setting `acceptGoRuntimeDefaults` in the plan.

## Validation

The migration tests use a V4.0.6 RocksDB export and SeaweedFS. The recorded native-process run retained the account, Agent identity and node limit, resumed two interrupted tasks, and verified all 520 resulting source/target pairs with SHA-256. Test commands and fixture details are in the [conversion guide](development/legacy-migration.md#tests). The [acceptance inventory](migration-acceptance.md) lists the remaining verification scope.
