# Spaces collector — design

Status: implemented on 2026-08-25. Date: 2026-08-24.

!!! warning "Superseded in part"

    The measurement described here — summing a full `ListObjectsV2` pass — was replaced on
    2026-08-26 by a single HEAD per bucket. See
    [Spaces bucket size from the gateway](2026-08-26-spaces-bucket-usage-headers.md). The
    architecture and the metric names below still hold; the costs and defaults do not.

## Why this collector exists at all

DigitalOcean does not expose the size of a Spaces bucket anywhere in its API. Verified
against the public OpenAPI specification: the only Spaces paths are `/v2/spaces/keys` and
`/v2/spaces/keys/{access_key}`, both about access keys; the monitoring metric families are
`apps`, `database`, `droplet`, `droplet_autoscale` and `load_balancer`; and no path in the
whole specification contains `object` or `storage`. Billing invoices report Spaces spend at
product level once a month, which answers a different question and needs a billing-scoped
token.

Sizing a bucket therefore means listing every object in it and summing `Size`. Measured
against three real buckets over the S3-compatible API:

| Bucket | Objects | Size | LIST pages | Wall clock |
|---|---:|---:|---:|---:|
| small | 264 | 0.02 GiB | 1 | 0.2 s |
| medium | 93,861 | 9.61 GiB | 95 | 13.9 s |
| large, many small objects | 61,418 | 48.21 GiB | 62 | 10.5 s |

Roughly 25 seconds sequentially for three buckets, and the largest grows continuously. A
Prometheus scrape timeout is typically 10 seconds. This is the measurement that justifies
the whole refresh-apart-from-scrape architecture, and it is why the collector needs its own
timeout rather than the global `--do.timeout`.

## Metrics

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_spaces_bucket_size_bytes` | `bucket`, `region` | Sum of the sizes of every object in the bucket |
| `digitalocean_spaces_bucket_objects` | `bucket`, `region` | Number of objects in the bucket |
| `digitalocean_spaces_bucket_up` | `bucket`, `region` | 1 if the bucket's last listing succeeded, else 0 |

All three are gauges. `digitalocean_spaces_bucket_objects` deliberately has no `_total`
suffix: it is a current count, not a monotonic counter.

The unmaintained metalmatze exporter exposes only bucket existence and creation date, never
size, so there is no naming compatibility to preserve here — unlike the billing metrics,
these names are chosen freely.

## Credentials

The Spaces S3 API authenticates with a Spaces access key and secret, which are unrelated to
the DigitalOcean API token the other collectors use. Both are configurable the same way the
token is:

| Flag | Environment variable | Description |
|---|---|---|
| `--spaces.access-key` | `DIGITALOCEAN_SPACES_KEY` | Spaces access key |
| `--spaces.access-key-file` | `DIGITALOCEAN_SPACES_KEY_FILE` | File holding the access key |
| `--spaces.secret-key` | `DIGITALOCEAN_SPACES_SECRET` | Spaces secret key |
| `--spaces.secret-key-file` | `DIGITALOCEAN_SPACES_SECRET_FILE` | File holding the secret key |

The exporter only ever calls `ListObjectsV2` (plus `ListBuckets` and `GetBucketLocation` in
discovery mode). It never reads an object, so a key with **Limited access** and **Read** on
the observed buckets is sufficient. DigitalOcean offers no list-only permission, so Read is
the narrowest grant that works.

## Bucket selection: two modes

**Explicit list.** `--collector.spaces.bucket=NAME[@REGION]`, repeatable, with
`--spaces.region` supplying the region for entries that omit one. This is the only mode a
limited-access key can use, and it keeps the cost of a refresh predictable: a new bucket
never silently enlarges the workload.

**Discovery.** When no bucket is configured, the collector calls `ListBuckets` and then
`GetBucketLocation` per bucket to route each one to its regional endpoint. `ListBuckets` is
a full-access capability, so a limited key gets `AccessDenied` here. That case must produce
an explicit error telling the operator to configure a bucket list, not a bare S3 error.

## Refresh mechanics

Each bucket is listed with `ListObjectsV2`, 1000 keys per page, accumulating a count and a
byte sum. Listing within a bucket is inherently sequential — each page needs the previous
continuation token — so parallelism is across buckets, bounded by
`--collector.spaces.concurrency` (default 4). The documented rate limit of 800 operations
per second is per bucket, and LIST may be throttled further under load, so concurrency
across buckets is the safe axis.

| Flag | Environment variable | Default | Description |
|---|---|---|---|
| `--collector.spaces` | `COLLECTOR_SPACES` | `false` | Enable the collector |
| `--collector.spaces.interval` | `COLLECTOR_SPACES_INTERVAL` | `6h` | Refresh interval |
| `--collector.spaces.timeout` | `COLLECTOR_SPACES_TIMEOUT` | `15m` | Timeout of one full refresh |
| `--collector.spaces.bucket` | `COLLECTOR_SPACES_BUCKET` | — | Bucket to observe, repeatable, `name[@region]` |
| `--collector.spaces.concurrency` | `COLLECTOR_SPACES_CONCURRENCY` | `4` | Buckets listed in parallel |
| `--spaces.region` | `SPACES_REGION` | — | Default region for buckets and for discovery |

`--collector.spaces.bucket` is repeatable; its environment variable takes a comma-separated
list. A bucket with no region — neither `@region` nor `--spaces.region` — is a configuration
error reported at startup, not a refresh that fails later.

The collector defaults to disabled because it needs credentials the other collectors do not,
and because its refresh is orders of magnitude more expensive than an API call.

## Failure isolation

The snapshot is a map from bucket to statistics, not a single struct. A bucket whose listing
fails keeps its previous values and reports `digitalocean_spaces_bucket_up 0`; every bucket
that succeeded is updated as usual. A bucket that has never been listed successfully has no
previous values to keep, so it emits `digitalocean_spaces_bucket_up 0` alone, with no size
and no object count — the same rule as the other collectors, which emit nothing rather than
zeros before their first success. `collector_success{collector="spaces"}` drops to 0 only
when discovery fails or when no bucket could be listed at all.

This is deliberate and comes from a real defect: the account collector used to fetch account
and balance in one refresh, so a single 403 on billing discarded the account snapshot too and
the exporter published nothing. Per-bucket isolation is the same lesson applied ahead of time.

## Scheduler change

`Scheduler` currently bounds every refresh by one shared timeout given to `NewScheduler`.
A Spaces refresh measured in minutes cannot share a timeout sized for a single API call, so
`Register` gains a per-collector timeout, with the scheduler's own value as the default for
collectors that do not need their own.

## Cost

Spaces Standard bills a $5/month subscription including 250 GiB of storage and 1,024 GiB of
outbound transfer; requests themselves are not billed. The per-operation charges documented
for Spaces (a 128 KiB minimum retrieval charge, early-deletion fees) belong to Cold Storage,
not Standard.

The only measurable consumption is the outbound transfer of the LIST responses: roughly 300
bytes of XML per object, so about 45 MB for a full pass over 155,000 objects. At the 6h
default that is around 5 GB per month, half a percent of the included allowance. An hourly
interval would be about 32 GB per month, which is why the default is 6h.

## Testing

- Unit tests drive `Refresh` against an `httptest` server speaking S3 XML: pagination across
  several pages, an empty bucket, a bucket that answers 403 alongside one that succeeds, and
  discovery mode. `Collect` is compared against golden exposition text.
- The failure-isolation rule gets its own test: after one bucket fails, the other bucket's
  metrics must be unchanged and the failed one must keep its previous values with `up 0`.
- `make smoke` gains a stub bucket so the end-to-end path stays covered without credentials.
- Verification against live buckets is read-only: `ListObjectsV2` and nothing else.

## Out of scope

Per-prefix breakdown, storage-class splits, object-age histograms and a multi-target
`/probe?bucket=` endpoint. The first three multiply cardinality for questions nobody has
asked yet; the last was considered and rejected in the original design because it moves the
listing cost back into the scrape path.

## Dependency

`aws-sdk-go-v2` (`service/s3` and `credentials`), as anticipated by the original design.
`minio-go` would be a lighter alternative and remains the fallback if the SDK's dependency
weight becomes a problem.
