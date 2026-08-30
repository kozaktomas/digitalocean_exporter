# Collectors

One page per question you might have about a collector: what it reports, what it costs, and
when to turn it off. The metric names themselves are in the [metrics reference](../metrics.md).

Eleven collectors are on by default. They are on because each costs one to three API
requests per refresh, which is negligible against the limit of 5000 an hour.

Five are off. Three of them — [`spaces`](spaces.md),
[`dropletmetrics`](monitoring-api.md#dropletmetrics) and
[`loadbalancermetrics`](monitoring-api.md#loadbalancermetrics) — have pages of their own,
because enabling them takes more than flipping a switch: arithmetic for the two
monitoring-API collectors, a second credential for `spaces`. The other two,
[`firewalls`](#firewalls) and [`certificates`](#certificates), cost no more than the
collectors that default on; they are off because what they report changes when somebody
deploys or renews something, not continuously, so most accounts have no use for them.

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

**Cost:** three requests per refresh, one for each of the three resources. Every one asks
for a single item and takes its figure from `meta.total`, so nothing more than a count
travels.

---

## registry

Container Registry storage usage, subscription tier and the repositories in it.

An account **without** a registry is not an error. The collector reports no metrics and
keeps `collector_success 1`, so it is safe to leave enabled everywhere, including on
accounts that will never have one.

Storage usage is the number to alert on: registry tiers have hard storage ceilings, and
pushing past one fails a deploy at the worst moment.

**Cost:** three requests per refresh — the registry, its subscription tier and its
repositories.

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

---

## domains

Every DNS zone the account hosts, and the zone's default TTL.

One list request covers the whole account, however many zones there are, which is what makes
this cheap enough to leave on. How many zones you have is
`count(digitalocean_domain_ttl_seconds)`, and a zone disappearing from that count is the
alert worth writing.

**The records inside a zone are not reported.** Counting them costs one request per zone, so
on an account with two hundred domains a five-minute interval would spend roughly half the
hourly rate limit on data that changes a few times a year. The list response does include
each zone's full BIND zone file, and record counts could be derived from it without any extra
request, but that file is a text format DigitalOcean documents no guarantees about — a
silently miscounted record is worse than an absent one.

**Cost:** one request per refresh.

---

## firewalls

Cloud firewalls: what each one is attached to, how many rules it carries, and how far
DigitalOcean has got applying it.

Two of these are alerts rather than dashboard panels.

`digitalocean_firewall_pending_changes` counts the droplets a rule change has not reached
yet. It is normally zero within seconds of an edit; one that stays non-zero means the
firewall is not protecting what its ruleset says it protects.

`digitalocean_firewall_inbound_rules_open` counts the inbound rules whose sources include
`0.0.0.0/0` or `::/0` — the rules reachable from the whole internet. The useful alert is not
`> 0`, since a public web server needs one, but a change in the number nobody intended.

**Configuration, not traffic.** DigitalOcean reports no packet or connection counts for a
firewall, so there is no way to see what a rule actually allows through.

**Off by default**, and not because of what it costs: one list request per refresh, the same
as the collectors that default on. A ruleset changes when somebody deploys, so most accounts
have no reason to scrape it. Turn it on when you want to alert on the two metrics above.

**Cost:** one request per refresh.

---

## certificates

Every TLS certificate the account holds for its load balancers and CDN endpoints, and when
each one expires.

`digitalocean_certificate_expiry_timestamp_seconds` is the reason this collector exists.
DigitalOcean renews a `lets_encrypt` certificate on its own, but renewal can fail quietly:
the certificate keeps its old `not_after` and its `state` turns to `error`. Alerting on the
expiry timestamp catches that; alerting on the state alone does not catch a certificate that
is simply old.

The `id` label matches the `certificate_id` on `digitalocean_cdn_endpoint_info`, so the
endpoint serving an expiring certificate can be found with a join rather than by hand.

A certificate whose `not_after` the API omits keeps its other metrics and has no expiry
sample, rather than reporting an expiry at the Unix epoch that would fire every alert written
against it.

**Off by default** for the same reason as `firewalls`: one list request per refresh, but a
certificate changes when it is renewed and not otherwise. Enable it if you terminate TLS at a
DigitalOcean load balancer or CDN endpoint.

**Cost:** one request per refresh.
