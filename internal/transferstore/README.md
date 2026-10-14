# Transfer stores

`Store` provides local and S3 object I/O. Task scheduling, object completion records, bandwidth limits, and restart recovery belong to the caller.

Use `Scan` for task discovery. It invokes the callback synchronously and stops on callback errors or cancellation. Local scanning visits the tree once. `List` is for paginated browsing; each local page walks from the root. `StartAfter` excludes keys up to the specified key in byte order. Cursors are tied to the store, prefix, and start key; preserve these options on every page.

Pass the prior object to `OpenOptions.Expected`. S3 reads use `If-Match` and the version ID when available. Local reads compare file identity, size, and modification time before opening and again at EOF. This detects ordinary replacement and concurrent writes; it does not create a filesystem snapshot. An ETag is an opaque fingerprint, including for multipart objects.

`WriteOptions.Size` is exact. Short or oversized sources fail before publication. Cancellation or a failed multipart upload closes the input if it implements `io.Closer`; other readers must honor their own cancellation. Local writes sync a temporary file, then atomically publish it by rename or no-replace hard link. With the Agent registry, temporary files live in a private staging directory beside the destination. Its identity is committed before file creation; file identity is committed before reading payload. Restart removes tracked incomplete files and empty, unregistered files inside the verified private directory. Files whose identity changed are retained. Windows uses file IDs and rejects alternate data streams and names that alias reserved files. A completed write reports `Committed`; this can remain true if a later sync or cleanup fails.

On Unix, publication syncs the parent directory. Windows flushes the destination file because it has no POSIX directory fsync. Recovery must verify destination objects after interruption, particularly after power loss. Do not use a completed ledger entry alone as proof that a file still exists.

`CreateSharedProbe`, `VerifySharedProbe`, and `CleanupSharedProbe` let Agents verify a shared directory before multi-node discovery. The coordinator must authorize every path, have all selected Agents verify the same random marker, and clean it up before scanning. Cleanup accepts only the generated filename format and matching marker contents. This requires one writable source path and does not prove that a mount stays present after verification. Local ETags contain node-local file identities; a coordinator may strip that ETag for cross-node reads only after verifying the shared root, while retaining size and modification time. `Local.Open` still pins and rechecks the current node's file identity.

S3 uses static credentials and supports V2 or V4 signing. The default part size is 8 MiB with four concurrent upload requests and a 32 MiB payload buffer budget per write. Source reads remain sequential, and a buffer slot is reserved before each read. Larger parts use temporary disk files; each part body can be replayed on SDK retries. Failed or canceled multipart uploads are aborted with a separate bounded cleanup context. With the Agent recovery registry, upload IDs and disk spools are recorded before uploading payload and cleaned on restart using the same connection configuration. Cleanup failures remain recorded; there is no part-level crash checkpoint in this package. An interrupted final S3 response can also leave a committed object, which restart logic must inspect.

Local tests use actual temporary files. S3 tests exercise a limited HTTP protocol simulator, including retries, conditional writes, multipart aborts, bounded concurrency, and byte integrity. V2 signing also has fixed signatures from AWS documentation. They do not establish compatibility or performance against JD OSS. Windows-specific tests require a native Windows runner; cross-compilation only checks that they build.

For real S3 acceptance, supply the native SeaweedFS 4.48 executable:

```sh
python3 .github/acceptance/go-store-qa.py --weed /path/to/weed --evidence /tmp/store-qa
```

The harness starts a loopback S3 server, transfers 1,010 files in all three directions, and verifies SHA-256 and metadata. Cases include Unicode keys, empty files, multipart upload, SKIP, OVERWRITE, cancellation after a part was uploaded, abort cleanup, V2 authentication, and rejection of incorrect credentials. It removes its own test buckets and stops its server. Logs, fixture files, runtime data, and binary hashes remain in the evidence directory.

Adding `--java /path/to/java8 --java-jar /path/to/oss-transfer.jar` starts a separate copy of the Java release with an isolated database. Three repetitions compare upload timings for a small file, many small files, a large file, and several large files, using the same S3 server and fixture bytes. These measurements compare Go storage I/O with a complete Java task, including Java scheduling and state persistence. They do not establish performance of the complete Go Master/Agent path.
