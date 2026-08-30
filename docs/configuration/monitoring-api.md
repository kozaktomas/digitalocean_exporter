# Monitoring API

Two collectors read DigitalOcean's monitoring API rather than its resource API:
[`dropletmetrics`](#dropletmetrics) and [`loadbalancermetrics`](#loadbalancermetrics). Both
are **off by default**, for the same reason: that API answers **one metric of one resource
per request**, so its cost scales with the size of your account rather than being a
constant.

## The budget

DigitalOcean applies **5,000 requests per hour and 250 requests per minute**. A refresh of
these collectors costs:

| Collector | Requests per refresh |
|---|---|
| `dropletmetrics` | 1 droplet listing + **10 per droplet** |
| `loadbalancermetrics` | **7 per load balancer** |

Turned into an hourly figure:

```
requests/hour = 3600 / interval_seconds × requests_per_refresh
```

At the default `5m` interval — twelve refreshes an hour:

| Account | `dropletmetrics` per hour | Share of budget |
|---|---:|---:|
| 3 droplets | 12 × 31 = 372 | 7% |
| 10 droplets | 12 × 101 = 1,212 | 24% |
| 25 droplets | 12 × 251 = 3,012 | 60% |
| 50 droplets | 12 × 501 = 6,012 | **over the limit** |

So: fine for a handful of droplets, impossible for a hundred. **Do this arithmetic for your
account before enabling it**, and if the number is uncomfortable, lengthen the interval —
`15m` cuts it to a third.

Load balancers are different in practice only because accounts have far fewer of them. Ten
load balancers at `5m` is 12 × 70 = 840 requests an hour, which is affordable.

!!! danger "The per-minute limit bites first"

    A refresh would otherwise fire its requests back to back, and a large account trips the
    burst limit long before the hourly budget looks worrying. Fifty droplets is 501 requests
    in **one** refresh — twice the 250-per-minute allowance, spent in seconds, regardless of
    how long the interval is. Lengthening the interval does not help with this; it is the
    size of a single refresh that matters.

    **The client-side rate limit is what holds it back**, and it is on by default:
    `--do.rate-limit` (`4` a second, 240 a minute) paces every request the exporter makes, so
    a refresh of any size goes out as a trickle rather than a burst. The cost is that a large
    refresh now takes as long as its size implies: 501 requests at 4 a second is just over
    two minutes, past `--collector.dropletmetrics.timeout`. It then fails on its own deadline
    rather than on a rejection — the same signal as ever,
    `digitalocean_exporter_collector_success 0` with the previous snapshot kept — and the
    answer is a longer timeout, fewer droplets or a longer interval, not a higher limit.

    `--collector.dropletmetrics.concurrency` is the secondary lever. With the rate limit in
    place it no longer decides how fast the requests go out — the limiter does — so lowering
    it changes little; raising it only piles more requests up behind the limiter. Leave it
    at `4` unless you have turned the rate limit off.

    A burst rejection that does get through comes back with a `retry-after` header, and the
    exporter honours it: the request is retried, up to three attempts, and only then does the
    refresh fail and keep its previous snapshot. Every attempt is counted by
    `digitalocean_exporter_api_requests_total`. See
    [staying under the burst limit](index.md#staying-under-the-burst-limit).

Watch the result rather than trusting the arithmetic:

```promql
digitalocean_exporter_api_rate_limit_remaining
```

It is read from the API response headers, so it is DigitalOcean's own count, not an
estimate. Alert on it going below a few hundred.

---

## dropletmetrics

CPU, memory, disk, filesystem and load per droplet — `digitalocean_droplet_cpu_seconds_total`,
`digitalocean_droplet_memory_available_bytes`, `digitalocean_droplet_filesystem_free_bytes`
and the rest.

```bash
digitalocean_exporter --collector.dropletmetrics --collector.dropletmetrics.interval=15m
```

**The API samples every 2 minutes.** An interval shorter than that buys nothing: you pay the
requests and get the same readings back. `5m` is the default; going below `2m` is waste.

**Droplets without DigitalOcean's monitoring agent report no readings.** The agent is what
produces this data; a droplet created without it, or one where it stopped, simply has none.
That is not an exporter failure — the droplet reports `digitalocean_droplet_metrics_up 0`
and the collector carries on.

`--collector.dropletmetrics.concurrency` (default `4`) sets how many droplets are queried
at once, and `--collector.dropletmetrics.timeout` (default `2m`) bounds one full refresh.
With many droplets the refresh has to fit in that timeout, and what decides how long it
takes is the [rate limit](index.md#staying-under-the-burst-limit) rather than the
concurrency: at 4 requests a second, fifty droplets is 501 requests and a little over two
minutes, whatever the concurrency is set to.

!!! question "Should you use this at all?"

    If you can run `node_exporter` on the droplet, run it. It gives you more, at higher
    resolution, for no API budget. This collector exists for droplets where you cannot —
    someone else's, an appliance image, a managed Kubernetes node.

---

## loadbalancermetrics

Traffic through each load balancer and the health of each individual backend droplet:
`digitalocean_loadbalancer_frontend_connections_current`,
`digitalocean_loadbalancer_frontend_http_responses_per_second`,
`digitalocean_loadbalancer_droplets_health_checks`, and others.

```bash
digitalocean_exporter --collector.loadbalancermetrics
```

The case for enabling this one is stronger than for `dropletmetrics`, and it is not about
cost. **A load balancer cannot run `node_exporter`.** There is no other way to learn how
much traffic it is passing, or which specific backend droplet is failing its health check —
the [`loadbalancers`](collectors.md#loadbalancers) collector tells you *how many* backends
there are, not which one is sick.

Same knobs, same meanings: `--collector.loadbalancermetrics.interval` (`5m`),
`--collector.loadbalancermetrics.timeout` (`2m`),
`--collector.loadbalancermetrics.concurrency` (`4`). A load balancer with no readings
reports `digitalocean_loadbalancer_metrics_up 0`.
