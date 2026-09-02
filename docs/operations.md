# Operations

Running it, scraping it, and working out what is wrong when a number stops moving.

## Scraping

```yaml
scrape_configs:
  - job_name: digitalocean
    static_configs:
      - targets: ["localhost:9212"]
```

Scrape as often as you like. Serving `/metrics` reads an in-memory snapshot and performs no
I/O, so the scrape interval is unrelated to how often DigitalOcean is called — that is set
by the collector intervals alone.

The corollary: **a short scrape interval does not give you fresher data.** If a collector
refreshes every 5 minutes, scraping every 15 seconds records the same value twenty times.
To learn how stale a value is, use the collector's own timestamp:

```promql
time() - digitalocean_exporter_collector_last_success_timestamp_seconds
```

## Health metrics

The exporter reports on itself. These are the series to build a "is the exporter fine"
panel from:

| Metric | What it tells you |
|---|---|
| `digitalocean_exporter_collector_success` | Last refresh of each collector, 1 or 0 |
| `digitalocean_exporter_collector_last_success_timestamp_seconds` | When each collector last succeeded |
| `digitalocean_exporter_collector_duration_seconds` | How long a refresh takes |
| `digitalocean_exporter_api_requests_total` | API requests by resource and HTTP status |
| `digitalocean_exporter_api_rate_limit_remaining` | Requests left in the current window, from DigitalOcean's own headers |
| `digitalocean_exporter_build_info` | Version and commit of the running binary |

The full list, with labels, is in the [metrics reference](metrics.md); the rules written
against them are in [alerting](alerting.md).

## Reading a failure correctly

Two states look similar on a dashboard and mean completely different things.

**The exporter is up, one collector is failing.** `up` is 1, that collector's
`digitalocean_exporter_collector_success` is 0, and its metrics **keep their last values**
rather than disappearing. This is deliberate: dropping them would put a gap in the graph,
and a gap reads as *the resource went away* — a different incident from *the exporter cannot
reach the API*. Trust `collector_success`, not the shape of the line.

**The exporter is down.** `up` is 0 and everything is gone. Now the gap is honest.

There is a third state worth recognising: **a collector that has never succeeded reports
nothing at all.** Before its first successful refresh there are no series, rather than
zeros. A metric that is missing right after a deploy is not necessarily broken — give it one
refresh interval.

## Troubleshooting

### One collector reports `collector_success 0` forever

Look at which one, then at the logs:

```bash
curl -s localhost:9212/metrics | grep 'collector_success{.*} 0'
journalctl -u digitalocean-exporter -n 50        # or: kubectl logs deploy/digitalocean-exporter
```

The refresh failure is logged with its reason each time it happens, because that failure
never reaches Prometheus as anything but a `0`. A collector that hit a bug rather than an
API error logs `collector refresh panicked` with the panic value and a stack trace; the
exporter keeps running and keeps refreshing that collector, so the `0` is the symptom and
the stack trace is the report worth filing.

Shutdown is not a failure: a refresh still in flight when the exporter is stopped is
abandoned quietly, at debug level, without touching `collector_success`. The last lines
before a restart are therefore not the ones to read as an outage.

The most common answer by far is `balance`: `403 Forbidden` from
`/v2/customers/my/balance` because the token has no billing scope. Turn that collector off —
see [balance](configuration/collectors.md#balance).

### The exporter exits at startup

A configuration error is fatal on purpose, rather than running half-configured. The usual
causes:

- **`--collector.<name>=false`.** Not valid. Use `--no-collector.<name>`, or the
  environment variable, which does take a value. See
  [turning a collector off](configuration/index.md#turning-a-collector-off).
- **Both or neither of `--do.token` and `--do.token-file`.** Exactly one must be set.
- **A token file the process cannot read.** Under systemd the unit runs as an unprivileged
  user with `ProtectSystem=strict`; the file must be readable by that user and live
  somewhere the unit can reach.

### `rate_limit_remaining` is falling towards zero

Something is spending the 5000-per-hour budget faster than it can be replenished. Almost
always `dropletmetrics` on an account with more droplets than the arithmetic allows — see
[the monitoring API](configuration/monitoring-api.md#the-budget). Lengthen that collector's
interval, or turn it off.

Note that a second replica of the exporter doubles the spend. Run one.

The per-minute limit is a separate matter, and the exporter defends it itself: it paces its
requests at `--do.rate-limit` a second and retries the rejections it does get. A `429` rate
that is not zero means the limit is set higher than the collectors can afford —
`sum by (status) (rate(digitalocean_exporter_api_requests_total[5m]))` shows it. See
[staying under the burst limit](configuration/index.md#staying-under-the-burst-limit).

### A refresh takes much longer than the API does

The rate limiter is between the collector and the API, and a collector that fans out over
every droplet or bucket spends most of its refresh waiting in it: at the default 4 requests
a second, 200 requests take 50 seconds however fast the API answers.
`digitalocean_exporter_collector_duration_seconds` is measuring the queue as much as the
API. That is the intended trade — a slow refresh beats a rejected one — but if a collector
is timing out on it, raise that collector's timeout or lengthen its interval rather than
raising the rate limit into DigitalOcean's own.

### A Spaces bucket stopped updating

Buckets are isolated: one failing bucket keeps its own previous values and reports its own
`_up 0` without affecting the others or the collector as a whole. Check the logs for that
bucket's name. Wrong region and a key that lost its grant are the usual causes; a refresh
that hits `--collector.spaces.timeout` is the other. See [Spaces](configuration/spaces.md#tuning).

### Metrics are stale but everything reports success

Check the interval. Every collector defaults to `5m`, but one you deliberately slowed down
— `dropletmetrics` at an hour on a large account, say — is not stale at minute fifty.
`time() - digitalocean_exporter_collector_last_success_timestamp_seconds` against that
collector's **configured** interval is the honest comparison; against a fixed number it is
not.

## Alerting

Twenty-one rules ship with the exporter as a plain Prometheus rule file, which the chart can
install as a `PrometheusRule`. [Alerting](alerting.md) lists every one of them, what it fires
on and what is deliberately left out. The three worth having on day one:

- `DigitalOceanExporterCollectorFailing` — `digitalocean_exporter_collector_success == 0`.
- `DigitalOceanExporterRateLimitLow` — `digitalocean_exporter_api_rate_limit_remaining < 500`.
- `DigitalOceanVolumeUnattached` — `digitalocean_volume_droplets == 0`, a volume billed while
  attached to nothing.
