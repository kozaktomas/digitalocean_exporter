# Metrics

All metrics are gauges unless stated otherwise.

## Account

Collected by `account` from `/v2/account`.

| Metric | Description |
|---|---|
| `digitalocean_account_active` | 1 if the account status is `active`, else 0 |
| `digitalocean_account_verified` | 1 if the account email address is verified |
| `digitalocean_account_droplet_limit` | Maximum number of droplets allowed |
| `digitalocean_account_floating_ip_limit` | Maximum number of floating IPs allowed |
| `digitalocean_account_reserved_ip_limit` | Maximum number of reserved IPs allowed |
| `digitalocean_account_volume_limit` | Maximum number of volumes allowed |

## Balance

Collected by `balance` from `/v2/customers/my/balance`.

| Metric | Description |
|---|---|
| `digitalocean_account_balance` | Current account balance |
| `digitalocean_month_to_date_balance` | Month-to-date balance |
| `digitalocean_month_to_date_usage` | Month-to-date usage |
| `digitalocean_balance_generated_at` | Unix timestamp the balance figures were generated at |

**This collector needs a token with the billing scope.** A token that can read every
resource still gets `403 Forbidden` from the balance endpoint unless billing is among its
scopes. That is why billing is a collector of its own: with such a token only
`digitalocean_exporter_collector_success{collector="balance"}` drops to 0, while the
account metrics keep flowing. Run with `--no-collector.balance` to switch it off entirely.

The three money metrics and `digitalocean_balance_generated_at` deliberately omit an
`account_` infix so that they match the names used by the older, unmaintained exporter.
Dashboards migrated from it keep working.

Note that the DigitalOcean API returns balances as strings. A value that does not parse as
a number fails the refresh rather than being reported as zero — zero is a legitimate
balance, and conflating the two would break billing dashboards silently.

## Spaces

Collected by `spaces` from the S3-compatible API, one `ListObjectsV2` pass per bucket.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_spaces_bucket_size_bytes` | `bucket`, `region` | Total size of every object in the bucket |
| `digitalocean_spaces_bucket_objects` | `bucket`, `region` | Number of objects in the bucket |
| `digitalocean_spaces_bucket_up` | `bucket`, `region` | 1 if the bucket's last listing succeeded, else 0 |

DigitalOcean publishes no bucket size in its API, so the only way to learn one is to add up
every object. That takes minutes on a large bucket, which is why this collector defaults to
a 6h interval and a 15m timeout of its own, and why it is disabled unless asked for.

A bucket that cannot be listed keeps its previous size and object count and reports
`digitalocean_spaces_bucket_up 0`; the buckets that listed fine are unaffected, and the
failure is logged with the reason. A bucket that has never listed successfully reports only
`digitalocean_spaces_bucket_up 0`, because a zero size is indistinguishable from an empty
bucket. The collector's own `collector_success` drops to 0 only when discovery fails or no
bucket at all could be listed.

The exporter only ever calls `ListObjectsV2`, plus `ListBuckets` and `GetBucketLocation` in
discovery mode. It never reads an object, so an access key with **Limited access** and
**Read** on the observed buckets is enough. Listing all buckets is a full-access capability,
so a limited key must be given an explicit bucket list.

## Exporter health

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_exporter_build_info` | `version`, `commit`, `goversion` | Always 1 |
| `digitalocean_exporter_collector_success` | `collector` | 1 if the collector's last refresh succeeded |
| `digitalocean_exporter_collector_duration_seconds` | `collector` | Duration of the last refresh |
| `digitalocean_exporter_collector_last_success_timestamp_seconds` | `collector` | Unix timestamp of the last successful refresh |
| `digitalocean_exporter_api_requests_total` (counter) | `resource`, `status` | API requests by resource and HTTP status |
| `digitalocean_exporter_api_rate_limit_remaining` | — | Requests left in the current API rate-limit window |

### Behaviour on failure

When a refresh fails, the collector's previous snapshot is kept and
`digitalocean_exporter_collector_success` drops to 0. Metrics are never dropped, so a
failing collector shows up as a flat line plus a failing health metric — not as a gap that
looks like DigitalOcean itself went away.

Before a collector's first successful refresh it emits nothing at all, rather than zeros.
A starting exporter must not be readable as an account with no droplets and no money.

### Rate limiting

DigitalOcean allows 5000 API requests per hour per token.
`digitalocean_exporter_api_rate_limit_remaining` is read from the response headers, so the
budget is visible before it runs out and the exporter starts serving stale data.

## Alerting

```yaml
groups:
  - name: digitalocean-exporter
    rules:
      - alert: DigitalOceanExporterCollectorFailing
        expr: digitalocean_exporter_collector_success == 0
        for: 15m
        annotations:
          summary: "Collector {{ $labels.collector }} has been failing for 15 minutes"

      - alert: DigitalOceanExporterRateLimitLow
        expr: digitalocean_exporter_api_rate_limit_remaining < 500
        for: 5m
        annotations:
          summary: "Fewer than 500 DigitalOcean API requests left this hour"
```
