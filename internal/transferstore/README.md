# Transfer stores

`Store` reads and writes local files and S3 objects. The caller handles scheduling, completion records, bandwidth limits and restart recovery.

## Discovery and reads

Use `Scan` for task discovery. It walks a local tree once, invokes callbacks synchronously and stops on callback error or cancellation. `List` provides paginated browsing; each local page walks from the root. `StartAfter` excludes keys up to the given key in byte order. Keep the same store, prefix and start key when reusing a cursor.

Pass the previous LIST or HEAD result as `OpenOptions.Expected` to detect source changes. S3 uses `If-Match` and the version ID when available. Local reads compare file identity, size and modification time before opening and again at EOF. Applications that need a consistent view of a changing filesystem should read from a snapshot. Treat ETags as opaque values, including multipart ETags.

## Local writes

Set `WriteOptions.Size` to the exact byte count. Short and oversized inputs fail before publication. Local writes sync a temporary file, then publish it atomically by rename or a no-replace hard link. `Committed` becomes true after publication and can remain true when a later sync or cleanup returns an error; callers must inspect both fields.

With the Agent recovery registry, temporary files use a private staging directory beside the destination. The directory identity is recorded before creating files, and each file identity is recorded before reading payload. Restart removes tracked incomplete files and empty unregistered files inside the verified staging directory. Files whose identities changed are left in place.

Unix syncs the parent directory after publication. Windows flushes the destination file, tracks file IDs, and rejects alternate data streams and reserved-name aliases. Recovery must check destination objects after an interruption, particularly power loss, before relying on a completed ledger entry.

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
