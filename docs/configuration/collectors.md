# Collectors

One page per question you might have about a collector: what it reports, what it costs, and
when to turn it off. The metric names themselves are in the [metrics reference](../metrics.md).

Ten collectors are on by default. They are on because each costs one or two API requests per
refresh, which is negligible against the limit of 5000 an hour. The three that are off —
[`spaces`](spaces.md), [`dropletmetrics`](monitoring-api.md#dropletmetrics) and
[`loadbalancermetrics`](monitoring-api.md#loadbalancermetrics) — have pages of their own,
because deciding to enable them means doing arithmetic first.

Every collector reports `digitalocean_exporter_collector_success` and
`digitalocean_exporter_collector_duration_seconds`, so the health of each one is visible
independently. A failed refresh keeps the previous values rather than dropping them.

---

## account

Account status, verification state and the account's resource limits — the ceilings on
droplets, reserved IPs and volumes.

Pairs with [`limits`](#limits), which reports what is actually in use. On their own, "you
may have 25 droplets" and "you are running 21" are trivia; together they are an alert.

**Cost:** one request per refresh.

---

## balance

Account balance and month-to-date usage.

!!! warning "Needs a billing-scoped token"

    This is the one collector a plain read-only token cannot serve. It reads
    `/v2/customers/my/balance`, and a token scoped to resources alone gets
    `403 Forbidden` — the collector then reports `collector_success 0` on every refresh,
    forever, while everything else keeps working.

    If your token cannot have the billing scope, disable this collector:
    `--no-collector.balance`, `COLLECTOR_BALANCE=false`, or `collectors.balance.enabled: false`.

Some metric names here deliberately lack an `account_` infix —
`digitalocean_month_to_date_usage` rather than `digitalocean_account_month_to_date_usage`.
They match an older, widely deployed exporter so that dashboards survive a migration. That
is not a bug and will not be "fixed".

**Cost:** one request per refresh.

---

## databases

Every managed database cluster: engine, version, state, node count and storage size.

This is **inventory, not load**. Connections, queries and replication lag come from a
Prometheus endpoint DigitalOcean runs per cluster, with credentials of its own, which this
exporter does not touch. If you want those, scrape that endpoint directly — it is a
separate target, not a missing feature here.

**Cost:** one request per refresh.

---

## droplets

Every droplet: state, region, size slug, vCPUs, memory, disk and hourly price.

This includes the droplets that make up a managed Kubernetes cluster, because to the API
they are droplets. Expect a node pool to appear here as well as under
[`kubernetes`](#kubernetes); the labels tell them apart.

Price is what the size costs, which makes a simple `sum by (region)` a decent
cost-per-region panel without touching the billing API.

**Cost:** one request per refresh, plus one per additional page for accounts with many
droplets.

---

## kubernetes

Managed Kubernetes clusters and their node pools: version, state, node count, auto-scaling
bounds.

This describes clusters **from the outside** — what DigitalOcean thinks it is running for
you. What happens inside a cluster is kube-state-metrics' job. The two answer different
questions, and the interesting alerts come from comparing them: a pool DigitalOcean reports
as having three nodes while the cluster sees two is a real incident.

**Cost:** one request per refresh, plus one per cluster for its node pools.

---

## limits

Droplets, reserved IPs and volumes currently in use, counted against the account limits that
[`account`](#account) reports.

The alert this exists for is "you are at 90% of your droplet limit", which you want to know
before an autoscaler discovers it.

**Cost:** shares the droplet listing where it can; one to three requests per refresh.

---

## registry

Container Registry storage usage, subscription tier and the repositories in it.

An account **without** a registry is not an error. The collector reports no metrics and
keeps `collector_success 1`, so it is safe to leave enabled everywhere, including on
accounts that will never have one.

Storage usage is the number to alert on: registry tiers have hard storage ceilings, and
pushing past one fails a deploy at the worst moment.

**Cost:** one request for the registry, plus one for its repositories.

---

## volumes

Every block storage volume: size, region, and how many droplets it is attached to.

The reason this is on by default is the volume attached to **nothing**. It is billed at the
full rate while serving no purpose, and nothing in the control panel nags you about it:

```promql
digitalocean_volume_droplets == 0
```

**Cost:** one request per refresh.

---

## loadbalancers

Every load balancer: state, region, the number of droplets behind it, and its billed size.

The backend count is worth an alert of its own — a load balancer with zero backends is
usually a deploy that went wrong rather than a deliberate configuration.

Traffic *through* a load balancer, and which individual backend is failing its health
check, come from [`loadbalancermetrics`](monitoring-api.md#loadbalancermetrics) instead.

**Cost:** one request per refresh.

---

## cdn

CDN endpoints, their origin, cache TTL and the certificate each one serves.

**Inventory only.** DigitalOcean exposes no traffic, bandwidth or hit-rate figures for CDN
endpoints anywhere in its API, so there are none here.

What it is genuinely useful for is certificates: the endpoint that quietly serves an
expiring certificate is easier to catch here than in the control panel.

**Cost:** one request per refresh. Note that CDN endpoints carry a
[stricter limit of their own](https://docs.digitalocean.com/reference/api/reference/public-apis/) —
5 requests per 10 seconds, independent of the account-wide limits. One request every 5
minutes is nowhere near it, but do not set this interval to seconds.
