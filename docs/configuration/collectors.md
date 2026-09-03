# Collectors

One page per question you might have about a collector: what it reports, what it costs, and
when to turn it off. The metric names themselves are in the [metrics reference](../metrics.md).

Sixteen collectors are on by default. They are on because each costs a few API requests per
refresh, which is negligible against the limit of 5000 an hour.

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

Every managed database cluster: engine, version, state, node count, storage size, users,
logical databases, whether storage autoscaling is on, and which project and VPC it belongs
to — all from the one list response.

This is **inventory, not load**. Connections, queries and replication lag come from a
Prometheus endpoint DigitalOcean runs per cluster, with credentials of its own, which this
exporter does not touch. If you want those, scrape that endpoint directly — it is a
separate target, not a missing feature here.

The read-only replicas and the age of the newest backup do not arrive in the list: they are
two endpoints of their own, two requests per cluster per refresh, behind
`--collector.databases.details`. It is on by default because an account with clusters has a
handful of them, not hundreds; turn it off with `--no-collector.databases.details` on an
account where two requests per cluster every five minutes is worth saving, and the replica
and backup metrics simply stop being reported. One cluster's lookup failing is not a failed
refresh: that cluster keeps its previous detail values and the exporter logs why, so the
others are unaffected. An engine that offers no backups or replicas — a caching cluster —
answers those endpoints with a client error, which reads as "none" rather than a failure.
Because the refresh fans out over the account, the collector carries a timeout of its own,
`--collector.databases.timeout`, two minutes by default.

**Cost:** one request per refresh, plus one per additional page for accounts with many
clusters, plus two per cluster for the detail lookups unless they are switched off.

---

## droplets

Every droplet: state, region, size slug, vCPUs, memory, disk and hourly price.

This includes the droplets that make up a managed Kubernetes cluster, because to the API
they are droplets. Expect a node pool to appear here as well as under
[`kubernetes`](#kubernetes); the labels tell them apart.

Price is what the size costs, which makes a simple `sum by (region)` a decent
cost-per-region panel without touching the billing API.

Whether backups are on, whether the droplet carries DigitalOcean's monitoring agent, when it
was created, which VPC it is in and what it is tagged with all arrive in the same list
response as the rest. Reporting them costs no extra request, which is why they are on rather
than behind a flag. The agent one is worth knowing before enabling
[`dropletmetrics`](monitoring-api.md#dropletmetrics): a droplet without the agent is one that
collector will spend ten requests a refresh on and get no readings back for.

**Cost:** one request per refresh, plus one per additional page for accounts with many
droplets.

---

## kubernetes

Managed Kubernetes clusters, their node pools and the individual nodes: version, state, node
count, auto-scaling bounds, the maintenance window and which droplet is under each node.

This describes clusters **from the outside** — what DigitalOcean thinks it is running for
you. What happens inside a cluster is kube-state-metrics' job. The two answer different
questions, and the interesting alerts come from comparing them: a pool DigitalOcean reports
as having three nodes while the cluster sees two is a real incident.

The pools, their nodes and everything the cluster list already says — surge upgrade, high
availability, the registry integration, the maintenance window — arrive in that one response
and cost nothing extra. The versions a cluster could be upgraded to do not: they are an
endpoint of their own, one request per cluster per refresh, behind
`--collector.kubernetes.upgrades`. It is on by default because an account with clusters has a
handful of them, not hundreds; turn it off with `--no-collector.kubernetes.upgrades` on an
account where a request per cluster every five minutes is worth saving, and the two upgrade
metrics simply stop being reported. One cluster's lookup failing is not a failed refresh: that
cluster keeps its previous upgrade values and the exporter logs why, so the others are
unaffected.

`digitalocean_kubernetes_node_state` is four series per node, one per documented state. That
is the only part of this collector whose series count grows with the size of the account
rather than the number of clusters.

**Cost:** one request per refresh, plus one per additional page for accounts with many
clusters, plus one per cluster for the upgrades lookup unless it is switched off.

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

An account may hold **more than one** registry, and every one of them is measured and
labelled by name. One registry that cannot be read keeps its last figures and reports
`digitalocean_registry_up 0` without costing the others.

An account **without** a registry is not an error. The collector reports no metrics and
keeps `collector_success 1`, so it is safe to leave enabled everywhere, including on
accounts that will never have one.

Storage usage is the number to alert on: registry tiers have hard storage ceilings, and
pushing past one fails a deploy at the worst moment.

**Cost:** two requests per refresh — listing the registries and the account-wide
subscription tier — plus one per registry for its repositories, and one more for every
further page of them. An account with a single registry therefore still costs three.

---

## reservedips

Every reserved IP address the account holds, IPv4 and IPv6, and the droplet each one is
assigned to.

This is on by default for the same reason [`volumes`](#volumes) is: the address assigned to
**nothing**. A reserved IP is free while it serves a droplet and billed by the hour while it
does not, which is the opposite way round from what most people assume, and an address left
behind by a destroyed droplet is never mentioned again:

```promql
digitalocean_reserved_ip_assigned == 0
```

The IPv6 listing carries no project, so `project_id` on `digitalocean_reserved_ip_info` is
empty for `version="6"` addresses. Everything else is reported for both.

It does not replace the count [`limits`](#limits) reports. That one is a single account-wide
total to hold against the account's reserved IP limit; this one is the addresses themselves.

**Cost:** two requests per refresh, one per address family, plus one more for every further
200 addresses of either. An account with fewer than 200 reserved IPs — which is nearly all of
them — therefore costs exactly two.

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

## images

Every private image the account stores: droplet and volume snapshots, the automatic droplet
backups DigitalOcean takes when the option is enabled, and custom images uploaded from
outside. Size, minimum disk, creation time, distribution, status and regions for each.

Stored images are the classic forgotten DigitalOcean cost. Destroying a droplet stops its
charge; the snapshot taken just before destroying it keeps being billed by size every month
until somebody deletes it, and nothing in the control panel brings it up. What the account
is paying for images that nobody has looked at in three months is:

```promql
sum(digitalocean_image_size_bytes{}) by (type)
```

and `DigitalOceanSnapshotOld` is the alert that names them one by one.

Only the account's own images are read — the images endpoint with `private=true`. The public
distribution and application images are DigitalOcean's, cost nothing and number in the
hundreds.

It refreshes every ten minutes rather than the five every other collector defaults to. An
image appears when somebody takes a snapshot or a nightly backup runs, which is hours apart;
reading the list twice as often would only spend requests.

**Cost:** one request per refresh, plus one more for every further 200 images. An account
with fewer than 200 images — which is nearly all of them — therefore costs exactly one.

---

## loadbalancers

Every load balancer: state, region, the number of droplets behind it, and its billed size —
plus its configuration: each forwarding rule with its certificate, the health check
settings, and the rule counts of the load balancer's own firewall. The configuration series
come out of the same list response as the rest, so they add nothing to the collector's cost.

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

## apps

Every App Platform app: its tier and region, the phase of the deployment it is serving,
whether another one is rolling out, when it was created, when it last went live, and the
instances each component of its spec asks for.

One list response carries all of it — the tier, both deployments and the whole spec arrive
together — so nothing here costs a request per app.

The widely used older exporter shipped an app metric, so this is also one of the collectors
a migration looks for.

**Runtime load is not here.** CPU, memory and restart count per component live behind
DigitalOcean's monitoring API, under endpoints the API client this exporter uses has no
methods for. That is a gap in what can be read, not a decision, and it is why an App
Platform app has no equivalent of the [droplet metrics](monitoring-api.md#dropletmetrics).
What is here is the state and the shape of the app, which is what a deployment that failed
shows up in.

A failed deployment is the reason to have it. App Platform keeps serving the last deployment
that worked, so a build that failed takes nothing down and is invisible from the outside;
`digitalocean_app_deployment_phase{phase="ERROR"}` is what
[`DigitalOceanAppDeploymentError`](../alerting.md#resources) fires on.

**Cost:** one request per refresh, plus one per additional page. The exporter asks for 200
items a page as it does everywhere else; the apps endpoint serves a smaller page by default,
so an account with a great many apps pays a request per page of whatever size it returns.

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

## tags

Every tag of the account and how many resources of each type carry it — droplets, images,
volumes, volume snapshots and databases.

The counts arrive on the tag list itself, so one paged request covers however many resources
the tags are spread across; nothing fans out per tag. A type the API reports no count for is
skipped rather than exported as zero, so `digitalocean_tag_resources` only carries the series
the response actually claimed.

The natural questions are joins: which droplets carry no `env` tag, whether a `backup` tag
still covers the volumes it should. The counts alone answer "how much do we have under this
tag", which is enough to alert on a tag emptying out unexpectedly.

**Cost:** one request per 200 tags per refresh.

---

## projects

Every project of the account — name, purpose, environment, whether it is the default — and
how many resources of each type it owns, counted from the URN type of each entry in the
project's resources list (`droplet`, `volume`, `loadbalancer`, `domain` and so on).

This is the collector that fans out: one paged resources request per project on top of the
project list, which is why it carries a timeout of its own
(`--collector.projects.timeout`). One project's resources lookup failing keeps that
project's previous counts and is logged; the other projects still refresh, and only every
lookup failing at once fails the refresh.

An account has a handful of projects, not hundreds, so the fan-out is cheap in practice.
If yours is the exception, slow the interval down before turning the collector off — the
assignments change when somebody moves a resource, not continuously.

**Cost:** one request for the project list plus one per project per refresh.

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
