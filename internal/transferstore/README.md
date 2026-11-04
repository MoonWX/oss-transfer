# Transfer stores

`Store` reads and writes local files and S3 objects. The caller handles scheduling, completion records, bandwidth limits and restart recovery.

## Discovery and reads

Use `Scan` for task discovery. It walks a local tree once, invokes callbacks synchronously and stops on callback error or cancellation. `List` provides paginated browsing; each local page walks from the root. `StartAfter` excludes keys up to the given key in byte order. Keep the same store, prefix and start key when reusing a cursor.

Pass the previous LIST or HEAD result as `OpenOptions.Expected` to detect source changes. S3 uses `If-Match` and the version ID when available. Local reads compare file identity, size and modification time before opening and again at EOF. Applications that need a consistent view of a changing filesystem should read from a snapshot. Treat ETags as opaque values, including multipart ETags.

## Local writes

Set `WriteOptions.Size` to the exact byte count. Short and oversized inputs fail before publication. Local writes sync a temporary file, then publish it atomically by rename or a no-replace hard link. `Committed` becomes true after publication and can remain true when a later sync or cleanup returns an error; callers must inspect both fields.

With the Agent recovery registry, temporary files use a private staging directory beside the destination. The directory identity is recorded before creating files, and each file identity is recorded before reading payload: a `Local` creates empty temporary files ahead of use and registers them in one commit (1, 2, 4, ... up to 16 per directory), so a payload is only ever written into a registered file. Restart removes tracked incomplete files and empty unregistered files inside the verified staging directory. Files whose identities changed are left in place. Each `Local` holds a reference to every staging directory it uses. After the last `Local` using one closes, it stays idle for 15 seconds so the next job writing there reuses it; then the registry deletes the directory and its record, provided it holds nothing but its marker and no temporary-file record points into it. Closing the registry and restart recovery delete idle staging directories the same way, including those left by a crash, a failed removal or older versions. A staging directory whose removal failed halfway is repaired or replaced the next time it is prepared. If its parent directory was deleted or recreated meanwhile (for example `rm -rf` of the output before a rerun), the namespace cannot be reached any more, so its record is dropped, provided no temporary-file record points into it; recovery and the next prepare also drop such records left by earlier builds. Discovery skips staging directories that carry a valid marker, never lists a marker or temporary file inside a directory shaped like one (a namespace still being created), and skips entries deleted while a walk visits them, so scanning a directory that another node is writing into does not fail. `Write` refuses keys with a component shaped like a staging directory or temporary file, so it never creates an object discovery would hide.

Unix syncs the parent directory after publication. Windows flushes the destination file, tracks file IDs, and rejects alternate data streams and reserved-name aliases. Recovery must check destination objects after an interruption, particularly power loss, before relying on a completed ledger entry.

`WriteOptions.DeferDurability` batches that durability work on Unix; the execution engine uses it for every object. The payload is still ordered before publication, so a visible file is always complete: Linux waits for writeback on ext4 with an internal journal in `data=ordered` or `data=journal` mode without `journal_async_commit`, and on XFS for files outside the realtime device; every other filesystem gets a full fsync. The ext4 decision is cached per `Local` and mount ID, so each job rereads the mount options. macOS uses `F_BARRIERFSYNC` instead of a per-file `F_FULLFSYNC`. Directory syncs and temporary-file record deletion move to `Local.Sync`, which syncs each touched destination and staging directory once and then clears the records in one registry commit. A skipped write, or an existing destination passed to `DeferExisting` (the engine does this for SKIP), also adds its directory to the next `Sync`, so a skip never relies on a directory entry another writer has not made durable yet. The caller must call `Sync` before it records results durably; `Close` does the same for writes never synced. After a failed `Sync` every later `Sync` on that `Local` fails too. Until `Sync` clears a record, restart recovery still owns the temporary name. Windows ignores the option.

A `Local` caches up to 8 idle destination directories (directory handle and staging directory, at most four descriptors each); the least recently used idle one is evicted after its pending directory changes are fsynced, and replaced entries are closed once synced. Reserved temporary files hold no descriptor: each is reopened by name and identity when used. Before every use the cached directory's path is resolved again through the root and its identity compared, so a replaced or moved directory is opened afresh and the root's path-escape protection still applies to every write.

Cancellation closes the input when it implements `io.Closer`. Other readers must implement their own cancellation behavior.

## Shared directories

`CreateSharedProbe`, `VerifySharedProbe` and `CleanupSharedProbe` check that selected Agents see the same directory. The coordinator authorizes the paths, creates a random marker, asks every Agent to verify it and removes it before scanning. Cleanup checks both the generated filename and marker contents. The source directory must be writable for this check.

Local ETags include node-local file identities. After verifying the shared directory, a coordinator can omit that ETag for cross-node reads while retaining size and modification time. `Local.Open` still checks the current node's file identity. Mount availability can change after the probe, so subsequent I/O errors must still be handled.

## S3 writes

S3 uses static credentials with V2 or V4 signing. Defaults are 8 MiB parts, four concurrent uploads and a 32 MiB payload buffer budget per write. Reads remain sequential and reserve buffer capacity before reading. Parts larger than their buffer share use temporary disk files; part bodies can be replayed on SDK retries.

Objects of 128 MiB or more can be read as parallel ranges when the source supports it (`RangeOpener`: S3 or a pinned local file) and the caller passes `WriteOptions.Ranges`. A one-byte conditional GET first pins the S3 ETag and checks that the provider honours `Range`; if it does not, the caller streams instead. Each part slot then reads its own range, with `If-Match` on every S3 range, so a changed source fails with `ErrSourceChanged` and the upload is aborted. Local ranges use `ReadAt` on the opened handle and repeat the identity check at the end of each range. A truncated range body is read again, at most three times. Objects of 1 GiB or more use 16 MiB parts to halve request count.

Ranged parts are always held in the Agent upload memory budget and are never spooled. Four slots per object are guaranteed and queue for memory in FIFO order. While the node has idle object slots (`PartLender`), an object borrows up to 16 slots in total; a borrowed slot takes memory only if no part is queued and a quarter of the budget stays free, and returns as soon as its part ends. Every range still passes through the caller's bandwidth and progress decorators and the request transport, so S3 ranges count against the task LIST budget like other object GETs. A task with its own bandwidth, LIST or PUT limit streams instead (the engine checks per object, so a changed limit applies to the next object): its cap already bounds its speed, and its parts would otherwise hold shared memory while waiting on it.

Failed or cancelled multipart uploads are aborted with a separate, bounded cleanup context. The Agent registry records upload IDs and disk spools before sending payload. Restart cleans these up with the original connection settings; failed cleanup remains recorded for another attempt. Recovery retries the object from the beginning rather than reusing uploaded parts. If the final S3 response is lost, inspect the destination because the object may already have committed.

## Tests

Local tests use temporary files. S3 unit tests use an HTTP simulator for retries, conditional writes, multipart aborts, concurrency and byte integrity. V2 signature tests include fixed examples from AWS documentation. Windows filesystem tests run on Windows.

To test with SeaweedFS 4.48:

```sh
python3 .github/acceptance/store-qa.py --weed /path/to/weed --evidence /tmp/store-qa
```

This starts a loopback S3 server, transfers 1,010 files in all three directions and checks hashes and metadata. Cases include empty files, Unicode keys, multipart upload, SKIP, OVERWRITE, cancellation after an uploaded part, abort cleanup, V2 authentication and invalid credentials. The script removes its buckets and stops the server. Logs, fixture files, runtime data and binary hashes remain in the output directory. JD Cloud OSS compatibility requires a separate provider test.

For historical Java comparisons, add `--java /path/to/java8 --java-jar /path/to/oss-transfer.jar`. The script runs three upload repetitions per workload using the same source bytes and S3 server. It compares Go storage I/O with a complete Java task, whose timing includes scheduling and state persistence. Use the [cluster test script](../../.github/acceptance/README.md) to test the complete Go Master/Agent path.
