# Metrics

All metrics are gauges unless stated otherwise.

## Account

Collected by `account` from `/v2/account` and `/v2/customers/my/balance`.

| Metric | Description |
|---|---|
| `digitalocean_account_active` | 1 if the account status is `active`, else 0 |
| `digitalocean_account_verified` | 1 if the account email address is verified |
| `digitalocean_account_droplet_limit` | Maximum number of droplets allowed |
| `digitalocean_account_floating_ip_limit` | Maximum number of floating IPs allowed |
| `digitalocean_account_reserved_ip_limit` | Maximum number of reserved IPs allowed |
| `digitalocean_account_volume_limit` | Maximum number of volumes allowed |
| `digitalocean_account_balance` | Current account balance |
| `digitalocean_month_to_date_balance` | Month-to-date balance |
| `digitalocean_month_to_date_usage` | Month-to-date usage |
| `digitalocean_balance_generated_at` | Unix timestamp the balance figures were generated at |

The three money metrics and `digitalocean_balance_generated_at` deliberately omit an
`account_` infix so that they match the names used by the older, unmaintained exporter.
Dashboards migrated from it keep working.

Note that the DigitalOcean API returns balances as strings. A value that does not parse as
a number fails the refresh rather than being reported as zero — zero is a legitimate
balance, and conflating the two would break billing dashboards silently.

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
