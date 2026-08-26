# Spaces bucket size from the gateway, not from a listing — design

Status: implemented on 2026-08-26. Date: 2026-08-26.

Supersedes the measurement approach of
[the original Spaces collector](2026-08-24-spaces-collector.md). The architecture that
document describes is unchanged; what it costs is not.

## The problem with the original approach

The collector summed a full `ListObjectsV2` pass over each bucket, because DigitalOcean's
own API publishes no bucket size. That was correct and it did not scale. `ListObjectsV2`
returns at most 1000 keys, so the cost is one request per thousand objects and grows
without bound, and it fails outright on buckets large enough to be the ones worth watching.
The whole shape of the collector — a 6h interval, a 15m timeout of its own, a concurrency
knob — existed to work around that cost.

## What was found instead

Spaces is served by the Ceph RADOS Gateway, which reports its own accounting for a bucket
in two non-S3 headers on a **HEAD of the bucket**:

```
x-rgw-object-count: 93879
x-rgw-bytes-used:   10324594821
```

One request per bucket, roughly 100 ms, independent of what the bucket holds. This is
almost certainly what the DigitalOcean control panel displays, and it is what the account
is billed on.

## Verification

Measured against three live buckets in `fra1`, comparing the headers with a summed
`ListObjectsV2` pass over the same bucket:

| Bucket | HEAD | Summed listing | Listing wall clock |
|---|---|---|---:|
| small | 264 objects / 25,499,534 B | identical | 1.2 s |
| medium | 93,879 objects / 10,324,594,821 B | identical | 9.2 s |
| log storage | 50,000 objects / 39,934,962,394 B | identical | 6.5 s |

Byte for byte on all three, with no block rounding. The counter is live rather than a
periodic rollup: the log bucket moved from 50,000 to 50,002 objects between two HEADs while
its writer ran. A **limited-access** key reads them fine, and a bucket the key has no grant
for answers 403 without leaking anything.

Two figures were **not** verified, because none of the three buckets exercised them: all
were unversioned with no incomplete multipart uploads. By how the gateway accounts, its
counter includes noncurrent versions and pending upload parts, which a listing does not
return — making it closer to the billed figure than the listing was.

## Decisions

**No fallback to listing.** The headers are the only measurement. A bucket whose HEAD comes
back without them reports `digitalocean_spaces_bucket_up 0` with the reason logged, rather
than silently spending minutes to reach the same answer. Keeping a listing path would keep
the failure mode the change exists to remove.

**The defaults return to ordinary.** The interval goes from `6h` to `5m`, matching every
other collector; bucket size is now cheap enough that there is no reason to look at it less
often. The timeout goes from `15m` to `2m`, matching the other collectors that fan out.
`--collector.spaces.timeout` is kept rather than dropped, because discovery still costs a
`GetBucketLocation` per bucket and a large account can still want the headroom.

**Reading the headers lives in `internal/spacesclient`.** `aws-sdk-go-v2` does not model
headers outside the S3 specification, so `BucketUsage` captures the raw response with a
deserialize middleware. That is client plumbing, of a piece with the path-style addressing
and regional endpoints already in that package, and it keeps the collector about policy.

**A 403 is explained in the error.** A HEAD has no response body, so the S3 error code a
`GET` would have carried never arrives: a key that lost its grant and an invalid key pair
both produce a bare `403 Forbidden`. The wrapped error says what the two causes are, since
the API will not.

## What did not change

The collector's contract is the same: `Refresh` does the I/O and swaps a snapshot,
`Collect` never performs I/O, a failed bucket keeps its previous values and reports its own
`_up 0`, a bucket never measured emits nothing but that failure, and only discovery failing
or every bucket failing fails the refresh. The metric names are unchanged; two help strings
were reworded to stop saying "listing".
