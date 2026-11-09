# Upgrade from v4.0.x

[中文](migration.zh-CN.md)

Data of v4.0.x needs an offline conversion before v4.1 or later, including v4.2.0, can use it. The converter keeps accounts, storage connections, node limits, task history and the recycle bin. Unfinished tasks enter the new installation stopped.

For a fresh installation, follow the [installation instructions](install.md). For an existing installation of v4.1 or later, use [software updates](operations.md#software-updates).

## Convert an existing installation

1. Stop the old Master and all execution nodes. Back up each node's database and configuration, including environment and command-line overrides.
2. Export every node's RocksDB data, including the old Master if it performed transfers.
3. Create a migration plan with the node identities, effective limits, Master URL and final data paths.
4. Prepare and finalize the conversion in a new directory.
5. Install the native services with the generated `service.json` files. Confirm the Agents (execution nodes) are online, then resume the tasks you want to continue.

The [conversion guide](development/legacy-migration.md) contains the commands and plan format. Preparation and finalization run on Linux or macOS, including early CentOS / RHEL 7 kernels without `renameat2`. Keep the original installation and backups until the new services and task results have been checked.

Since v4.1, scheduling is separate from execution. Install a Master and an Agent on a single-host system; they use separate data directories. In a cluster, place each finalized Agent directory on its execution host at the absolute path recorded in its configuration.

Existing Agents retain their identities when their private credentials match the Master snapshot. Directly addressed v4.0.x workers receive new identities through explicit entries in the migration plan. Sign in with the existing account password after conversion; old login sessions expire.

## Resume unfinished tasks

The first resume scans the task's original scope with its saved ranges, inventory, filters and target naming. Before reusing an old successful result, the Agent reads the current source and destination, compares SHA-256 hashes and checks their identities for changes during the reads.

A missing source becomes a failed item. An unequal existing target follows the saved [conflict policy](operations.md#conflict-policies): `SKIP` (*Skip existing objects or files*) leaves it untouched; `OVERWRITE` (*Overwrite existing objects or files*) permits another write. Historical time-filter exclusions stay excluded. Interrupted multipart objects restart at object level because the old database lacks the upload IDs and completed-part lists needed to continue them.

Completed tasks retain their historical results and report coverage. New transfer attempts record their own results. Review the retry, concurrency and multipart defaults in the conversion guide before setting `acceptGoRuntimeDefaults` in the plan.

## Validation

Test commands and v4.0.6 export fixtures are in the [conversion guide](development/legacy-migration.md#tests). Current release checks are described in the [acceptance guide](../.github/acceptance/README.md).
