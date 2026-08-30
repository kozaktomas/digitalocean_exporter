# Spaces

The `spaces` collector reports the size and object count of Spaces buckets. It is **off by
default**, not because it is expensive — it costs one request per bucket — but because it
authenticates with a Spaces key pair, a credential the rest of the exporter does not use.

## Where the number comes from

DigitalOcean publishes no bucket size in its own API. Not in the API, not in the monitoring
API — the monitoring metric families are `apps`, `database`, `droplet`, `droplet_autoscale`
and `load_balancer`, and the only Spaces paths in the whole OpenAPI specification are about
access keys. Billing invoices report Spaces spend at product level once a month, which
answers a different question.

The S3-compatible endpoint does publish it. Spaces runs on the Ceph RADOS Gateway, which
answers a **HEAD of a bucket** with its own accounting in two headers:

```console
$ curl -I --aws-sigv4 ... https://fra1.digitaloceanspaces.com/images
HTTP/1.1 200 OK
x-rgw-object-count: 93879
x-rgw-bytes-used: 10324594821
```

That is the whole measurement: one request per bucket, about a tenth of a second, whatever
the bucket holds.

Neither header is part of S3, so no SDK models them and DigitalOcean does not document
them. Measured against three live buckets, they agree with a full `ListObjectsV2` pass
**byte for byte**:

| Bucket | HEAD | Summed listing | Listing wall clock |
|---|---|---|---:|
| small | 264 objects / 25,499,534 B | identical | 1.2 s |
| medium | 93,879 objects / 10,324,594,821 B | identical | 9.2 s |
| log storage, many small objects | 50,000 objects / 39,934,962,394 B | identical | 6.5 s |

The figures are live rather than a periodic rollup: the log bucket ticked from 50,000 to
50,002 objects between two HEADs while its writer was running.

## Why not add up a listing

Because it does not scale, and its cost is unbounded. `ListObjectsV2` returns at most 1000
keys, so summing a bucket costs a request per thousand objects, takes minutes once a bucket
is large, and stops being workable on the buckets you most want measured. The HEAD costs
one request no matter the size.

There is no fallback to listing. If the headers are missing the bucket reports
`digitalocean_spaces_bucket_up 0` with the reason logged, rather than quietly spending
minutes to reach the same answer.

## What the two numbers count

They are the gateway's own accounting for the bucket, which is what DigitalOcean bills and
what the control panel shows. On a bucket with versioning, or with incomplete multipart
uploads, that includes noncurrent versions and uploaded parts — objects a listing does not
return. On an ordinary bucket the two are the same number, which is what the table above
measured.

## It does not touch the API budget, and it is not billed

Spaces is an S3-compatible endpoint, not the DigitalOcean API, so these requests count
against **neither** the 5,000-per-hour nor the 250-per-minute API limit. Nothing else in
the exporter is affected by turning it on.

Nor do they cost money. [Spaces is billed](https://docs.digitalocean.com/products/spaces/details/pricing/)
on storage and outbound transfer only — $5/month including 250 GiB and 1 TiB of transfer,
then $0.02/GiB and $0.01/GiB — with no per-request charge, unlike S3. A HEAD of a bucket
transfers headers and no object data.

## Credentials

Spaces authenticates with an **access key pair**, created under *API → Spaces Keys*. It has
nothing to do with the DigitalOcean API token — a valid token grants no access to Spaces,
and a Spaces key grants no access to the API.

The collector only ever reads bucket metadata; it never reads an object. A **Limited
access** key with **Read** on the buckets you want measured is enough.

```bash
digitalocean_exporter \
  --do.token-file=/etc/digitalocean-exporter/token \
  --collector.spaces \
  --spaces.access-key-file=/etc/digitalocean-exporter/spaces-key \
  --spaces.secret-key-file=/etc/digitalocean-exporter/spaces-secret \
  --spaces.region=fra1 \
  --collector.spaces.bucket=assets \
  --collector.spaces.bucket=backups@ams3
```

In the chart, set `spaces.accessKey` and `spaces.secretKey`, or point `spaces.existingSecret`
at a Secret holding both — see [secrets](../helm/secrets.md).

## Naming buckets, or discovering them

Two modes, and the difference is which kind of key you need.

**Named buckets** — `--collector.spaces.bucket`, repeatable, each as `name` or
`name@region`. A bucket without a region falls back to `--spaces.region`. This works with a
limited-access key, and it is what you want in production: you measure the buckets you care
about and nothing else.

A bucket is identified by its name *and* its region, here and in the metrics, because a
Spaces name is only unique within a region. Naming `backups@fra1` and `backups@ams3`
measures two buckets and reports two series, one per region label.

**Discovery** — pass no buckets and the collector lists them itself, then locates each one.
Listing all buckets is a full-access capability, so this needs a **full-access** key; a
limited key is told to name its buckets instead. Convenient for a first look, more
privilege than the job needs, and one extra request per bucket on every refresh.

## Failure is isolated per bucket

A collector that measures many things at once isolates them. If one bucket fails — wrong
region, key lost its grant, network — that bucket keeps its previous values and reports its
own `_up 0`, and the reason is logged. The buckets that succeeded are unaffected, and the
collector as a whole still reports success. Only a failure of discovery, or of every
bucket, fails the refresh.

One wrinkle worth knowing when you read those logs: **a HEAD carries no response body**, so
the S3 error code that a `GET` would have spelled out never arrives. A key that has lost
its grant on a bucket produces a bare `403 Forbidden` rather than `AccessDenied`, and an
invalid key pair produces the same thing. The exporter says so in the log line, but the API
cannot tell you which of the two it was.

## Tuning

`--collector.spaces.interval` (default `5m`) is the ordinary collector interval; there is
no longer a reason for this collector to refresh less often than the others.

`--collector.spaces.concurrency` (default `4`) sets how many buckets are measured at once.
It matters only if you measure a lot of buckets, since each one is a single request.

`--collector.spaces.timeout` (default `2m`) bounds one **full** refresh, all buckets
together. It is separate from the global `--do.timeout` because the collector fans out over
buckets and discovery adds a request per bucket. Check a real refresh duration against it
if you measure many:

```promql
digitalocean_exporter_collector_duration_seconds{collector="spaces"}
```
