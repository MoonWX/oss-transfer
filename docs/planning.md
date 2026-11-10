# Plan a migration

[中文](planning.zh-CN.md)

Read this page before you install. It covers where to run the Master and the Agents (execution nodes), which network connections to open, which storage permissions to grant, what limits the speed, how much disk to plan for, how to verify a large migration, and how to run incremental rounds and the final cut-over. For security settings, see [security](security.md).

## Checklist

1. Choose the hosts: one Master, and one or more Agents near the data ([topology](#topology)).
2. Open the ports in the [network table](#network-and-firewall).
3. Create storage keys with the [minimum permissions](#storage-permissions) for the source and the target.
4. Run a pilot task on a representative prefix and measure its speed ([speed](#speed-and-what-limits-it)).
5. Change the defaults that would hold a large migration back ([defaults to review](#defaults-to-review-for-large-migrations)).
6. Plan the verification and the cut-over ([verification](#verify-a-large-migration), [cut-over](#incremental-migration-and-cut-over)).

## Topology

| Component | How many | Where |
| --- | --- | --- |
| Master | Exactly one per cluster | Any host that the operators' browsers and every Agent can reach. It serves the console and the API and schedules tasks; no object data passes through it. |
| Agent | One or more | Hosts with good bandwidth to both the source and the target. Each Agent reads from the source and writes to the target itself. |

- **One Agent per host.** Several Agents on one host share its network card and disks. To use a bigger host better, raise the node's concurrency and limits instead ([defaults to review](#defaults-to-review-for-large-migrations)).
- **Near the source or near the target.** Every Agent of a task needs to reach both storage endpoints. Each object is read once from the source and written once to the target, so the data crosses the link between the two sites once wherever the Agent runs. Put the Agents on the side where the network is faster or traffic is cheaper. For example, with an Agent in the source cloud, it reads over the internal endpoint and sends the data out over the host's internet connection; with an Agent in the target cloud, it reads from the source's public endpoint and writes over the internal endpoint. Compare your providers' prices for those two kinds of outbound traffic.
- **Local directories need an Agent on that host.** A local path in a task is a path on the Agent's host. To upload or download a local directory, run an Agent on the host that has the directory, or on a host that mounts it. When several Agents work on one local directory, it must be shared storage that every one of them mounts.
- **The Master is a single service.** It is not clustered. Back up its data directory ([database maintenance](database-maintenance.md)). Agents reconnect by themselves after a Master restart.
- **The Master can also run an Agent.** The native installer offers to install a local Agent on the Master host. That is convenient for small migrations; for large ones, run the Agents on separate hosts so transfers do not compete with the console.

## Network and firewall

Agents open every connection themselves. The Master never connects to an Agent, so Agents need no inbound ports.

| From | To | Port | Needed for | Required |
| --- | --- | --- | --- | --- |
| Operators' browsers | Master | TCP 1685 (HTTP), or 443 behind an HTTPS proxy | The console | Yes |
| `oss-transfer-cli` on another computer | Master | Same as the browser | Remote CLI use | Only for remote CLI |
| Agents | Master | Same address as given in the join command | Commands, results, heartbeats (HTTP long polling) | Yes |
| Agents | Source and target storage endpoints | Usually 443 (HTTPS) or 80; self-hosted S3 often uses its own port, such as 9000 | Transfers, connection checks, object comparison | Yes |
| New native Agents | Master, or GitHub / JD Cloud OSS | 443 | Downloading the Agent package during the join. The **Add node** dialog shows the download source. When packages come from the public sources, the installer also asks `cloudflare.com` and `ipinfo.io` for the host's location, unless you choose the source with `--source github` or `--source jdoss` ([download sources](install.md#download-sources)). | During the join |
| New Docker Agents | Docker Hub (or your registry) | 443 | Pulling the `moonwx/osstransfer` image | During the join and updates |
| Master and native Agents | GitHub (`api.github.com`, `github.com`, `raw.githubusercontent.com`) or `moonwx.s3.cn-north-1.jdcloud-oss.com`, plus `www.cloudflare.com` | 443 | Update checks and online updates | Only for online updates |

- The Master listens on `0.0.0.0:1685` by default. To use another port, pass `--port` to `install.sh`; to bind one address, pass `--listen HOST:PORT` (first installation only; see [installation options](install.md#paths-and-ports)).
- The Master never contacts the storage endpoints. Connection checks, bucket lists and comparisons run on an Agent: the check when you save a connection runs on one online Agent chosen by the Master. So add at least one Agent before you add storage connections, and make sure every Agent can reach every endpoint.
- Hosts without internet access can do everything except online updates and public downloads. Install and upgrade them with [offline installation](offline-install.md).
- Agents may connect to the Master through an HTTPS reverse proxy. See [security](security.md#https-with-a-reverse-proxy) for the proxy settings that Agents need.

## Storage permissions

Give OSS Transfer its own keys, limited to the buckets of the migration. The tables use AWS IAM action names; other S3-compatible services name the same permissions in their own policy language.

**Source bucket**

| Action | Used for |
| --- | --- |
| `s3:ListBucket` | Listing the objects to transfer (ListObjectsV2), the connection check and object comparison |
| `s3:GetObject` | Reading objects (GetObject, including byte ranges of large objects). On AWS it also authorizes HeadObject. |

**Target bucket**

Which permissions the target needs depends on the conflict policy and on whether you compare objects:

| Permission | When it is needed | Without it |
| --- | --- | --- |
| `s3:PutObject` | Always: PutObject and the multipart calls CreateMultipartUpload, UploadPart and CompleteMultipartUpload | Nothing can be written |
| `s3:AbortMultipartUpload` | When objects are larger than 8 MiB (they use multipart uploads) | Failed or interrupted multipart uploads cannot be cleaned up, and their parts use (and are billed as) storage; the cleanup record is kept and retried every 5 minutes |
| `s3:ListMultipartUploadParts` | As above: confirms that the parts are gone after an abort (ListParts) | The upload is aborted, but completion cannot be confirmed; the cleanup record is kept and retried |
| `s3:GetObject` (grants HeadObject and GetObject) | With the conflict policy **Skip existing objects or files** (the default) or **Overwrite changed objects**: each object is checked with HEAD before it is written, to find whether the target exists or changed. After an Agent restart or a resumed task, to verify objects written before the interruption. For object comparison, and to read target objects in a content comparison | Writes fail under those two policies; the check after a resume fails; comparison is impossible |
| `s3:ListBucket` | The connection check (it lists one object in the bucket) and object comparison. On AWS and similar services, without it a HEAD on a missing key answers 403 instead of 404 | The connection check only warns; under the two policies above, new objects fail to be written; comparison is impossible |

Common minimal sets:

- **Default (Skip existing objects or files) or incremental runs (Overwrite changed objects):** `s3:PutObject`, `s3:GetObject` and `s3:ListBucket`, plus `s3:AbortMultipartUpload` and `s3:ListMultipartUploadParts` for large objects.
- **Only "Overwrite existing objects or files", without object comparison:** normal writes need only `s3:PutObject` (plus the two multipart cleanup permissions for large objects). The check after an Agent restart or a resumed task still uses HEAD, so `s3:GetObject` is still recommended.
- **Object comparison:** `s3:ListBucket` and `s3:GetObject` on both buckets.

When in doubt, run [the per-permission check](#check-each-permission-of-a-bucket) of the connection test on the target bucket with the write check selected.

**Optional:** `s3:ListAllMyBuckets` lets the connection check and the console list your buckets. Without it, see [bucket-scoped keys](#bucket-scoped-keys-and-the-connection-check).

Migrations never delete objects or buckets, so they need no `s3:DeleteObject` permission. Only the per-permission check, with the write check selected, tries to delete the test object it wrote; without delete permission the check still reports its results. If you want to delete objects that exist only on the target (see [deletions at the source](#deletions-at-the-source)), use your provider's tools with a separate key.

By direction:

| Direction | Source permissions | Target permissions |
| --- | --- | --- |
| Object storage → object storage | Source bucket table | Target bucket table |
| Object storage → local | Source bucket table | The Agent's service account writes the local directory |
| Local → object storage | The Agent's service account reads the local directory | Target bucket table |
| Object comparison | `s3:ListBucket` and `s3:GetObject` on both buckets | |

A minimal AWS-style policy for the source key (replace `source-bucket`):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::source-bucket"
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::source-bucket/*"
    }
  ]
}
```

And for the target key, covering the default conflict policy, incremental runs and object comparison (replace `target-bucket`):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucket"],
      "Resource": "arn:aws:s3:::target-bucket"
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:AbortMultipartUpload",
        "s3:ListMultipartUploadParts"
      ],
      "Resource": "arn:aws:s3:::target-bucket/*"
    }
  ]
}
```

Add a statement with `"Action": ["s3:ListAllMyBuckets"]` and `"Resource": "*"` if you want bucket lists. On AWS, buckets encrypted with SSE-KMS also need `kms:Decrypt` on the source key and `kms:GenerateDataKey` on the target key in the KMS key policy.

### Bucket-scoped keys and the connection check

A connection check (saving with **Validate access when saving**, **Test connection**, **Fetch buckets**, `storage test` and `storage buckets`) first lists the buckets (ListBuckets), then lists at most one object (ListObjectsV2) in one bucket with the connection's addressing: the bucket you name, or else the first one listed.

Keys limited to one bucket usually cannot list buckets. The connection is then still saved, with a warning instead of an error. Check the bucket itself:

- In the console, open **Test connection**, enter the bucket under **Check a bucket** and choose **Check**.
- From the CLI: `oss-transfer-cli storage test <connection> --bucket <bucket>`.

The check passes when it reports that it listed objects in that bucket; the result of each permission is described in the next section. When you create a task, type the bucket name. If listing the objects of the bucket is denied too, the save still succeeds with a warning, but transfers from that bucket will fail: fix its permissions first.

Automatic addressing and the bucket-level check run on the Agent, so they need Agents on v4.1.42. With older Agents, the check only lists buckets.

### Check each permission of a bucket

In **Test connection**, enter a bucket under **Check a bucket** and choose **Check** (CLI: `oss-transfer-cli storage test <connection> --bucket <bucket>`). The check makes, on that bucket, each request a migration uses, and shows for each one whether it passed, failed or was skipped, plus a one-sentence result (for example "reading works; it can be a migration source"):

| Check | Request | Used by migrations to |
| --- | --- | --- |
| List objects | ListObjectsV2 | Scan sources, compare |
| Read object metadata | HeadObject (first listed object) | Find existing targets, verify results |
| Read object content | GetObject of one byte | Read source objects |
| Write an object (with the write check, CLI `--write`) | PutObject with `If-None-Match: *` | Write targets; "Skip existing objects" uses this conditional write |
| Read the written object back | HeadObject | Verify after writing |
| Multipart upload | CreateMultipartUpload, UploadPart, AbortMultipartUpload | Upload and clean up large objects |
| Delete the test object | DeleteObject | Not needed for migrations; only removes the test object |

The write check writes a tiny test object under `.oss-transfer-permission-check/` and deletes it afterwards; the multipart upload is aborted and leaves no data. Without delete permission, the result names the leftover test object so you can delete it. In an empty bucket, reading objects cannot be checked and shows as skipped. In the CLI, the command exits 1 when a required check fails. The check needs a Master and Agents on v4.1.43 or later.

## Speed and what limits it

A migration runs as fast as its slowest limit:

| Limit | Default per Agent | Effect |
| --- | --- | --- |
| Node bandwidth | 2,000 Mbps | Data read from the source |
| LIST requests | 100 per second | Listing the source (and object comparisons) |
| PUT requests | 1,400 per second | Each written object or part costs at least one |
| HEAD requests | 2,800 per second | Metadata checks |
| Concurrency | 32 child tasks × 4 objects = 128 objects at once | How many objects are in flight |
| Storage service | Set by your provider | Request rate limits per bucket or prefix, throttling (`SlowDown`), latency |
| Network | Your hosts and links | Bandwidth and round-trip time to each endpoint |

These defaults are explained in [node concurrency](operations.md#node-concurrency) and [node limits](operations.md#node-limits). A task can have its own lower limits, and when the storage service throttles, Agents slow down by themselves ([storage throttling](operations.md#storage-throttling)).

Small objects are limited by requests and latency, large objects by bandwidth. The documentation gives no general speed figures, because they depend on the provider, the region and the object sizes. Measure instead:

1. Run a pilot task on a prefix whose object sizes are typical of the whole dataset.
2. In the task details, watch the speed and the LIST, GET, HEAD and PUT rates with their limits. If a rate sits at its limit, that limit is the bottleneck.
3. Estimate the duration: the larger of *total bytes ÷ measured bytes per second* and *object count ÷ measured objects per second*.
4. Estimate the Agents you need: the required rate divided by one Agent's measured rate, rounded up, as long as the storage service allows the total.

### Defaults to review for large migrations

| Setting | Default | Change it when | How |
| --- | --- | --- | --- |
| Node bandwidth | 2,000 Mbps | The host has 10 Gbps or more | **Execution nodes → Live status → Adjust**, or `oss-transfer-cli limit set bandwidth 8 --unit Gbps --all` |
| Node LIST / PUT / HEAD limits | 100 / 1,400 / 2,800 per second | Many small objects and the provider allows more | **Adjust**, or `limit set list 500 --all` |
| Child tasks and objects per child task | 32 and 4 | High latency or small objects leave bandwidth unused (at most 256, 64 and 1,024 objects in total) | **Execution nodes → Edit node**, or `node update <node> --child-concurrency 64 --object-concurrency 8` |
| Comparison result budget | 2 GiB per Agent, kept 30 days | You expect many differences | `comparison` in the Agent's `service.json` ([result storage](operations.md#result-storage-and-retention)) |

Raise one value at a time and watch the throttling status and error rate.

## Disk and database

| Where | What grows | Plan for |
| --- | --- | --- |
| Master data directory (`/opt/oss-transfer/master/data` on Linux) | `master.db`: tasks, child tasks and their results | Grows with the number of objects, not their size |
| Agent data directory (`/opt/oss-transfer/agent/data`) | `agent.db`: scan and execution records; `spool`: parts of large objects that do not fit in memory; `comparisons`: comparison results | Records grow with the number of objects. Comparison results take up to 2 GiB per Agent by default (about 8 million differences with 40-byte keys), and storing them stops when less than 1 GiB would stay free. |
| Local download destination | The files, plus temporary files in a hidden `.oss-transfer-staging-…` directory | Room for the data plus the objects in flight. The download precheck reports the free space. |

The documentation gives no fixed size per object. Measure it in the pilot: note the size of `master.db` and `agent.db` before and after, divide by the number of objects, and scale to the full dataset with a safety margin. Records stay until you permanently delete the task; stopping a task or moving it to the trash keeps them. Freed space is reused, and only an offline compaction shrinks the file; compaction needs room for a full copy of the database ([database maintenance](database-maintenance.md#reclaim-disk-space)).

## Verify a large migration

Use [object comparison](operations.md#compare-objects) (**Tools → Compare objects**, or `oss-transfer-cli object compare`):

- **Metadata mode** compares size, content headers and custom metadata with one HEAD request per object on each side. It reads no content. Use it for every object.
- **Content mode** reads both objects in full and compares SHA-256. Both sides are read and may be charged. Use it for samples, such as a few prefixes or your most important data.

For many objects:

1. **Split by prefix.** Each Agent runs one comparison at a time, and each comparison runs on one Agent. Start one comparison per prefix: the Master places each on an Agent that is not running one, so several prefixes run in parallel on several Agents.
2. **Avoid running them with transfers.** Comparisons share the node's request and bandwidth limits with the transfers on that Agent. Run them after the copy, or on Agents that are not transferring.
3. **Watch the result budget.** One comparison stores at most the Agent's budget (2 GiB by default) of differences; beyond that it keeps counting but stops listing them. Use narrower prefixes or raise the budget.
4. **Check that the export is complete.** `summary.json` in the exported ZIP has `complete: true`; `object export` exits with status 3 when the export is incomplete.
5. **Transfer the differences again.** The export's `retry-source-keys.incr` lists the keys that are missing or different on the target; import it with `import-inventory ... --overwrite` ([export results](operations.md#export-results)).

From the task details of an object storage → object storage task, **Compare source and target** opens the comparison form filled in with the task's buckets and prefixes.

## Incremental migration and cut-over

A large dataset usually changes while you copy it. Copy it in rounds and stop writes only for the last, short round.

### What each conflict policy does with existing objects

| Conflict policy | `--conflict` | An object that already exists on the target |
| --- | --- | --- |
| Skip existing objects or files (default) | `skip` | Is skipped, matched by name only. Objects changed at the source stay old on the target. |
| Overwrite changed objects | `changed` | Is overwritten when it looks changed (rule below), otherwise skipped |
| Overwrite existing objects or files | `overwrite` | Is always copied again |

**Overwrite changed objects** decides from the metadata of the listing and one HEAD request, without reading content. It overwrites the target object when:

1. the sizes differ; or
2. both ETags are plain MD5 ETags (32 hexadecimal digits) and they differ; or
3. the source's Last-Modified time is later than the target's.

Otherwise the object is skipped and counted as skipped. Its limits:

- **Same-size changes can be missed.** An object changed at the source keeps the same size, has a multipart ETag (`…-N`) or another non-MD5 ETag, and its Last-Modified time is not later than the target's: it is not detected. Multipart ETags, and the fingerprints of local files, are never compared.
- **Encryption can make everything look changed.** With SSE-KMS or SSE-C, and on some providers, ETags look like MD5 values but are not the MD5 of the content. Source and target ETags then differ for identical objects, and the policy copies them again, like **Overwrite existing objects or files**. Check a pilot first: if almost nothing is skipped in a second round, this is the cause.
- **Two clocks are compared.** The source's Last-Modified time comes from the source service and the target's from the target service. A target copy normally has a later time (when it was written), so rule 3 fires only for objects changed at the source after they were copied. A clock that is far off on either side can cause extra copies or missed changes.
- It needs Agents on v4.1.42. The Master refuses the policy when a selected Agent is older.

### Runbook

1. **Full copy.** Create the tasks, for example per bucket or top-level prefix:

   ```sh
   oss-transfer-cli task create oss-to-oss source/photos target/photos --conflict changed
   ```

   Wait until the task is **Completed**. Retry failed objects from **Exceptions**.
2. **Incremental rounds.** Create the same task again with **Overwrite changed objects** (`--conflict changed`). Each round lists the whole source again and checks each existing object, but copies only new and changed objects. Repeat until a round is short enough to fit in your maintenance window. To list less, you can add **Modified after** (`--date-after`), which filters by the source's Last-Modified time in the server's time zone; choose a time well before the previous round started, to allow for clock differences.
3. **Freeze writes.** Stop the applications from writing to the source, or switch them to read-only.
4. **Final round.** Run one more round with **Overwrite changed objects** and wait until it is **Completed** with no failed objects.
5. **Verify.** Run a metadata comparison of every prefix and content comparisons of samples ([verification](#verify-a-large-migration)). A result of **No differences found** means every object matches. If objects are missing or different, export the comparison, transfer `retry-source-keys.incr` again with `import-inventory ... --overwrite`, and compare again.
6. **Switch the applications** to the target endpoint, bucket and keys.
7. Keep the source read-only until you are sure you no longer need it.

### Deletions at the source

OSS Transfer copies; it never deletes. Objects deleted at the source between rounds stay on the target. An object deleted after the scan of a running task is counted as skipped, with the reason `SOURCE_DELETED` in the task report.

To find objects that exist only on the target, run an object comparison of the same scope after the final round. They appear as **Only on target** (`EXTRA_TARGET`). List them with `oss-transfer-cli object status <comparison-id> --status EXTRA_TARGET`, or export them with `object export <comparison-id> --output extra.zip --status EXTRA_TARGET` and read `differences.csv`. Delete them with your provider's tools if you need an exact copy.

## Mixed versions

Update all Agents to the Master's version before a large migration ([software updates](operations.md#software-updates)). Older Agents keep working, but:

- connections without an explicit addressing choice stay virtual-hosted on Agents before v4.1.42, and their connection checks only list buckets;
- the Master refuses **Overwrite changed objects** on Agents before v4.1.42.
