# Spaces

The `spaces` collector reports the size and object count of Spaces buckets. It is **off by
default**, and it is the one collector whose cost is measured in minutes rather than
requests.

## Why it is different

DigitalOcean does not expose the size of a Spaces bucket anywhere. Not in the API, not in
the monitoring API — the monitoring metric families are `apps`, `database`, `droplet`,
`droplet_autoscale` and `load_balancer`, and the only Spaces paths in the whole OpenAPI
specification are about access keys. Billing invoices report Spaces spend at product level
once a month, which answers a different question.

So sizing a bucket means **listing every object in it** and adding up the sizes. Measured
against three real buckets over the S3-compatible endpoint:

| Bucket | Objects | Size | LIST pages | Wall clock |
|---|---:|---:|---:|---:|
| small | 264 | 0.02 GiB | 1 | 0.2 s |
| medium | 93,861 | 9.61 GiB | 95 | 13.9 s |
| large, many small objects | 61,418 | 48.21 GiB | 62 | 10.5 s |

A Prometheus scrape timeout is typically ten seconds. This measurement is the reason the
whole exporter refreshes in the background instead of collecting on scrape, and it is why
this collector carries a timeout of its own (`15m`) rather than the global `--do.timeout`.

The defaults follow from that: refresh every **6h**, not every 5m. Bucket size is not a
number that moves meaningfully in five minutes, and pretending otherwise costs real time.

## It does not touch the API budget, and it is not billed

Spaces is an S3-compatible endpoint, not the DigitalOcean API, so the LIST requests this
collector makes count against **neither** the 5,000-per-hour nor the 250-per-minute API
limit. Nothing else in the exporter is affected by turning it on.

Nor do those requests cost money. [Spaces is billed](https://docs.digitalocean.com/products/spaces/details/pricing/)
on storage and outbound transfer only — $5/month including 250 GiB and 1 TiB of transfer,
then $0.02/GiB and $0.01/GiB — with no per-request or per-operation charge, unlike S3. The
collector only ever lists metadata and never downloads an object, so it generates no
outbound transfer worth counting either.

The one exception to watch is **cold storage**, where each read operation carries a 128 KiB
minimum retrieval charge. Listing is not a read of object data, but if you keep buckets in
cold storage, confirm your own bill rather than taking this page's word for it.

## Credentials

Spaces authenticates with an **access key pair**, created under *API → Spaces Keys*. It has
nothing to do with the DigitalOcean API token — a valid token grants no access to Spaces,
and a Spaces key grants no access to the API.

The collector only ever lists; it never downloads an object. A **Limited access** key with
**Read** on the buckets you want measured is enough.

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

**Discovery** — pass no buckets and the collector lists them itself. Listing all buckets is
a full-access capability, so this needs a **full-access** key; a limited key is told to name
its buckets instead. Convenient for a first look, more privilege than the job needs.

## Failure is isolated per bucket

A collector that measures many things at once isolates them. If one bucket fails — wrong
region, key lost its grant, network — that bucket keeps its previous values and reports its
own `_up 0`, and the reason is logged. The buckets that succeeded are unaffected, and the
collector as a whole still reports success.

This matters because the failure never reaches the scheduler: a single unreachable bucket
must not blank out the measurements of the other nine.

## Tuning

`--collector.spaces.concurrency` (default `4`) sets how many buckets are listed at once.
Raising it shortens a refresh over many buckets; each concurrent listing is an independent
stream of LIST requests against the Spaces endpoint, not against the 5000/hour API budget.

`--collector.spaces.timeout` (default `15m`) bounds one **full** refresh, all buckets
together. If you measure a bucket with millions of objects, check a real refresh duration
against it:

```promql
digitalocean_exporter_collector_duration_seconds{collector="spaces"}
```

A refresh that hits the timeout leaves the previous snapshot in place and reports failure,
which is visible rather than silent — but it means the numbers stop moving, so raise the
timeout or narrow the bucket list.
