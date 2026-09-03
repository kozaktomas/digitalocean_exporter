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
| `loadbalancermetrics` | 1 load balancer listing + **7 per load balancer** |
| `loadbalancermetrics` with [`--collector.loadbalancermetrics.extended`](#the-extended-metric-set) | 1 load balancer listing + **27 per load balancer** |

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
load balancers at `5m` is 12 × 71 = 852 requests an hour, which is affordable — 12 × 271 =
3,252 with the extended set, which is the arithmetic to do before switching that on.

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

    **A refresh that runs out of time is a failed refresh, not a partial one.** The droplets
    it did reach keep their fresh readings, but the collector reports
    `digitalocean_exporter_collector_success 0` and logs how many of how many it measured —
    an account whose fleet no longer fits in its timeout says so instead of quietly
    publishing two thirds of itself. The next refresh then **starts where that one stopped**,
    so a fleet slightly too large for its timeout is covered over a few refreshes rather than
    measuring the head of the list forever while the tail is never measured at all. Both
    collectors do this.

    A burst rejection that does get through comes back with a `retry-after` header, and the
    exporter honours it: the request is retried, up to three attempts, and only then does the
    refresh fail and keep its previous snapshot. Every attempt is counted by
    `digitalocean_exporter_api_requests_total`. See
    [staying under the burst limit](index.md#staying-under-the-burst-limit).

Watch the result rather than trusting the arithmetic:

```promql
digitalocean_exporter_api_rate_limit_remaining / digitalocean_exporter_api_rate_limit
sum by (collector) (rate(digitalocean_exporter_api_requests_total[5m])) * 3600
```

Both come from the exporter's own instrumentation: the first is DigitalOcean's count of what
is left over the ceiling it reports for this account, not an estimate, and the second is what
each collector is spending an hour — this collector's share of it included, which the
`resource` label cannot show, since every monitoring query reads `/v2/monitoring`. Alert on
the ratio rather than on a fixed number of requests; the bundled rules already do.

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

This collector honours [filtering](index.md#filtering), and here the filter cuts cost, not
just output: a droplet the tag and region filter rejects is never measured, so its ten
requests per refresh are never spent.

**Droplets without DigitalOcean's monitoring agent report no readings.** The agent is what
produces this data; a droplet created without it, or one where it stopped, simply has none.
That is not an exporter failure — the request succeeds and returns no series, so the droplet
reports `digitalocean_droplet_metrics_up 1` with no readings under it, and the collector
carries on.

The same droplet listing exports that feature as `digitalocean_droplet_monitoring_agent`, so
how many droplets this collector would report nothing for is answerable **before** enabling
it, from the `droplets` collector an untouched install already runs:

```promql
count(digitalocean_droplet_monitoring_agent == 0)
```

Those ten requests per refresh are spent all the same. `--collector.dropletmetrics.agent-only`
(default off) skips them: the droplet listing the collector already makes says whether a
droplet has the monitoring agent, and with the flag set only droplets that do are queried.
On an account where most droplets have no agent, that is most of the collector's cost gone.

!!! warning "The feature is what the droplet was *created* with"

    `agent-only` reads the `monitoring` feature from the droplet listing — the same feature
    `digitalocean_droplet_monitoring_agent` reports — and DigitalOcean sets it when the
    droplet is created with monitoring enabled. **An agent installed afterwards does not set
    it**, and neither do some droplets that answer the monitoring API anyway — managed Kubernetes nodes, for instance, list `droplet_agent` rather than
    `monitoring` and still return readings. Such a droplet disappears from the exposition
    entirely with this flag on: it is not measured, so it reports nothing at all, not even
    `digitalocean_droplet_metrics_up`.

    Run without the flag first and compare: if a droplet you care about is reporting readings
    now and vanishes with it on, leave it off.

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
`--collector.loadbalancermetrics.concurrency` (`4`). A load balancer whose fetch failed
reports `digitalocean_loadbalancer_metrics_up 0` and keeps its previous readings; one that
answered with no series at all — a network load balancer has no HTTP metrics ever — is up
with nothing under it.

This collector honours [filtering](index.md#filtering) the same way `dropletmetrics` does:
a load balancer the tag and region filter rejects is never measured, so its requests per
refresh — seven, or 27 with the extended set — are never spent.

### The extended metric set

The seven metrics above are a subset of what the monitoring API offers per load balancer.
`--collector.loadbalancermetrics.extended` (default off) reads the other twenty as well:
TLS connections (current rate, the rate limit, and connections closed for exceeding it),
the backend request queue, response time and session duration percentiles (average, p50,
p95, p99), per-backend connections and response codes, frontend network throughput per
protocol, and the bytes and packets a network load balancer's firewall dropped. The
[metrics reference](../metrics.md#load-balancer-metrics) lists them all.

```bash
digitalocean_exporter --collector.loadbalancermetrics --collector.loadbalancermetrics.extended
```

Each of those is **one more request per load balancer per refresh** — that is the entire
reason they are opt-in. With the flag set, a refresh costs 27 requests per load balancer
instead of 7; redo the budget arithmetic above before enabling it.

Many of the extended metrics apply to one kind of load balancer only: the `nlb_` throughputs
and the firewall drops exist for network load balancers, everything HTTP and TLS for
regional ones. The API answers an inapplicable metric with an empty result, which is not a
failure — the series is simply absent, exactly like an idle load balancer's HTTP metrics.

Failures are contained per metric rather than per load balancer: an extended metric the API
refuses is logged and left absent for that load balancer this refresh, while its other
readings stay fresh and `digitalocean_loadbalancer_metrics_up` stays 1. A failing *base*
metric still fails the whole load balancer, exactly as without the flag.
