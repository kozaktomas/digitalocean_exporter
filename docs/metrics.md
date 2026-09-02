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

## Droplets

Collected by `droplets` from `/v2/droplets`, one set of metrics per droplet.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_droplet_up` | `id`, `name`, `region` | 1 if the droplet's status is `active`, else 0 |
| `digitalocean_droplet_cpus` | `id`, `name`, `region` | Number of virtual CPUs |
| `digitalocean_droplet_memory_bytes` | `id`, `name`, `region` | Memory of the droplet |
| `digitalocean_droplet_disk_bytes` | `id`, `name`, `region` | Disk of the droplet |
| `digitalocean_droplet_price_hourly` | `id`, `name`, `region` | Price per hour in US dollars |
| `digitalocean_droplet_price_monthly` | `id`, `name`, `region` | Price per month in US dollars |
| `digitalocean_droplet_info` | `id`, `name`, `region`, `size`, `status`, `image` | Always 1 |

The first six names and their label sets are those of the older, unmaintained exporter, so
dashboards survive a migration. The size, the exact status and the image are the labels it
does not carry; widening the metrics would have broken that compatibility, so they live on
`digitalocean_droplet_info` instead. Join on `id` to break a bill down by size:

```promql
sum by (size) (
  digitalocean_droplet_price_monthly * on (id) group_left(size) digitalocean_droplet_info
)
```

`digitalocean_droplet_up` is 0 for every status that is not `active`, including the `off` of
a droplet somebody powered off on purpose. The same join separates the two, which is what
`DigitalOceanDropletDown` does so that it only pages for a droplet that stopped on its own:

```promql
digitalocean_droplet_up == 0
  unless on (id) digitalocean_droplet_info{status=~"off|archive"}
```

**One figure deliberately differs from the older exporter.** It reads DigitalOcean's disk
gigabytes as decimal and its memory megabytes as binary; this collector reads both as
binary, which makes `digitalocean_droplet_disk_bytes` about 7% larger. A droplet sold as
160 GB reports 160 GiB, matching what the operating system on it will show.

Droplets that belong to a managed Kubernetes cluster are ordinary droplets and are reported
like any other. They run a custom image, which carries no slug, so `image` falls back to the
image name — `do-kube-1.35.5-do.1`, which names the cluster version. Their names are
generated and change when a node is replaced, so the series churn; read them through
`sum()` rather than by name.

Droplet tags are not exported. A droplet carries any number of them, and a label per tag
would multiply the series of every droplet by the tags it happens to have.

## Volumes

Collected by `volumes` from `/v2/volumes`, one set of metrics per block storage volume.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_volume_size_bytes` | `id`, `name`, `region` | Size of the volume |
| `digitalocean_volume_droplets` | `id`, `name`, `region` | Number of droplets the volume is attached to |
| `digitalocean_volume_info` | `id`, `name`, `region`, `filesystem_type`, `filesystem_label` | Always 1 |

`digitalocean_volume_size_bytes` and its labels are those of the older exporter, including
its reading of the API's gigabytes as binary, so a volume sold as 100 GB reports 100 GiB.

Attachment is a count, not a boolean. A volume can be attached to more than one droplet, so
a single `droplet_id` label would have to pick one arbitrarily. The number answers the
question that matters, which is whether anything is using the volume at all:

```promql
digitalocean_volume_droplets == 0
```

An unattached volume is billed at full price while serving nothing, so this is worth an
alert rather than a panel. Volumes created by a Kubernetes `PersistentVolumeClaim` are the
usual culprits: deleting a pod does not delete its claim, and a released claim keeps its
volume until something reaps it.

## Load balancers

Collected by `loadbalancers` from `/v2/load_balancers`, one set of metrics per load balancer.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_loadbalancer_status` | `id`, `name`, `ip` | 1 if the load balancer's status is `active`, else 0 |
| `digitalocean_loadbalancer_droplets` | `id`, `name`, `ip` | Number of droplets it proxies to |
| `digitalocean_loadbalancer_size_units` | `id`, `name`, `ip` | Size units the load balancer is billed for |
| `digitalocean_loadbalancer_forwarding_rules` | `id`, `name`, `ip` | Number of forwarding rules configured |
| `digitalocean_loadbalancer_info` | `id`, `name`, `ip`, `region`, `size`, `type`, `algorithm`, `vpc_uuid` | Always 1 |

The prefix is `digitalocean_loadbalancer_`, without the underscore the rest of the exporter
would suggest, because that is what the older exporter used. Its two metrics — `status` and
`droplets` — keep their names and label sets here exactly.

A load balancer with no backends is the case worth alerting on:

```promql
digitalocean_loadbalancer_status == 1 and digitalocean_loadbalancer_droplets == 0
```

That reads "active but proxying to nothing", which returns 503 to every request. Note that a
load balancer selecting its backends by tag reports zero until something carries the tag, so
a newly created one trips this too.

`size_units` is what the load balancer costs: DigitalOcean bills a node-based load balancer
per unit, and a balancer scaled up for a traffic spike and never scaled back down is a
standing charge that nothing else in the account makes visible.

Traffic through the load balancer is not here. It comes from the monitoring API, which is a
different kind of request with a different cost, and lives in the `loadbalancermetrics`
collector.

## CDN endpoints

Collected by `cdn` from `/v2/cdn/endpoints`, one set of metrics per endpoint.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_cdn_endpoint_ttl_seconds` | `id`, `origin`, `endpoint` | Cache time-to-live |
| `digitalocean_cdn_endpoint_info` | `id`, `origin`, `endpoint`, `custom_domain`, `certificate_id` | Always 1 |

This is inventory, not traffic. DigitalOcean's API reports no request count, no bandwidth
and no cache hit ratio for a CDN endpoint, so none of that can be exported here. What an
endpoint fronts is a Spaces bucket, and the `spaces` collector measures that bucket's size.

`certificate_id` is on the info metric so an endpoint can be joined to the `id` of the
[certificate](#certificates) that serves it, and with it that certificate's expiry. An
endpoint with a `custom_domain` and an empty `certificate_id` is serving that domain without
TLS of its own.

## Domains

Collected by `domains` from `/v2/domains`, one metric per DNS zone.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_domain_ttl_seconds` | `domain` | Default TTL of the zone |

There is no metric counting the zones. `count(digitalocean_domain_ttl_seconds)` is the
number of them, the same way the number of droplets is `count(digitalocean_droplet_up)`.

The records inside a zone are deliberately absent: counting them costs one API request per
zone. The [collector page](configuration/collectors.md#domains) explains the trade-off.

## Firewalls

Collected by `firewalls` from `/v2/firewalls`, one set of metrics per firewall. Off by
default.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_firewall_info` | `id`, `name`, `status` | Always 1 |
| `digitalocean_firewall_droplets` | `id`, `name` | Droplets attached directly |
| `digitalocean_firewall_tags` | `id`, `name` | Tags attached; droplets carrying one are covered too |
| `digitalocean_firewall_inbound_rules` | `id`, `name` | Inbound rules |
| `digitalocean_firewall_outbound_rules` | `id`, `name` | Outbound rules |
| `digitalocean_firewall_inbound_rules_open` | `id`, `name` | Inbound rules whose sources include `0.0.0.0/0` or `::/0` |
| `digitalocean_firewall_pending_changes` | `id`, `name` | Droplets a change has not reached yet |

`pending_changes` is normally zero within seconds of an edit. One that stays non-zero means
the firewall is not yet protecting what its ruleset says it protects:

```promql
digitalocean_firewall_pending_changes > 0
```

`inbound_rules_open` counts the rules reachable from the whole internet. Alerting on `> 0`
is wrong — a public web server needs one — but an unexplained change in the number is worth
knowing about:

```promql
changes(digitalocean_firewall_inbound_rules_open[1d]) > 0
```

This is configuration, not traffic: DigitalOcean reports no packet or connection counts for
a firewall, so what a rule actually lets through cannot be exported.

## Certificates

Collected by `certificates` from `/v2/certificates`, one set of metrics per certificate. Off
by default.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_certificate_expiry_timestamp_seconds` | `id`, `name`, `type` | Expiry as a Unix timestamp |
| `digitalocean_certificate_info` | `id`, `name`, `type`, `state`, `sha1_fingerprint` | Always 1 |
| `digitalocean_certificate_dns_names` | `id`, `name` | DNS names the certificate covers |

`type` is `lets_encrypt` or `custom`. DigitalOcean renews a `lets_encrypt` certificate
itself, but renewal can fail quietly — the certificate keeps its old expiry and its `state`
turns to `error` — so the alert to write is on time remaining rather than on state:

```promql
(digitalocean_certificate_expiry_timestamp_seconds - time()) / 86400 < 14
```

The `id` label matches the `certificate_id` on `digitalocean_cdn_endpoint_info`, so the
endpoint serving a certificate that is about to expire can be found with a join:

```promql
label_replace(digitalocean_cdn_endpoint_info, "id", "$1", "certificate_id", "(.*)")
  * on(id) group_left(name)
    ((digitalocean_certificate_expiry_timestamp_seconds - time()) / 86400 < 14)
```

The endpoints are the many side of that join: one certificate can serve several of them. The
result is the days remaining, labelled with the endpoint serving it.

A certificate whose `not_after` the API omits has no expiry sample at all rather than one at
the Unix epoch, which would fire every alert written against it. Its other metrics are
reported as usual.

## Droplet metrics

Collected by `dropletmetrics` from `/v2/monitoring/metrics/droplet/*`, one set of metrics
per droplet. **Off by default** — see the request budget below.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_droplet_cpu_seconds_total` | `id`, `name`, `mode` | Cumulative CPU time. **This is a counter** |
| `digitalocean_droplet_memory_total_bytes` | `id`, `name` | Memory the operating system reports as installed |
| `digitalocean_droplet_memory_available_bytes` | `id`, `name` | Memory available without swapping |
| `digitalocean_droplet_memory_free_bytes` | `id`, `name` | Memory used for nothing at all |
| `digitalocean_droplet_memory_cached_bytes` | `id`, `name` | Memory used by the page cache |
| `digitalocean_droplet_filesystem_size_bytes` | `id`, `name`, `device`, `mountpoint`, `fstype` | Size of the filesystem |
| `digitalocean_droplet_filesystem_free_bytes` | `id`, `name`, `device`, `mountpoint`, `fstype` | Free space |
| `digitalocean_droplet_load1`, `_load5`, `_load15` | `id`, `name` | Load averages |
| `digitalocean_droplet_metrics_up` | `id`, `name` | 1 if the droplet's last fetch succeeded |
| `digitalocean_droplet_metrics_timestamp_seconds` | `id`, `name` | Unix time of the newest sample returned |

CPU is a counter, exactly like `node_cpu_seconds_total`, so it is read with `rate()`:

```promql
sum by (id) (rate(digitalocean_droplet_cpu_seconds_total{mode!="idle"}[5m]))
```

`digitalocean_droplet_memory_total_bytes` is not the `digitalocean_droplet_memory_bytes` of
the `droplets` collector. That one is the memory the droplet was *sold*, taken from its
size; this one is what the operating system reports as installed, which is a little less
because the hypervisor and the kernel keep some. On a droplet sold with 8 GiB the two read
8589934592 and about 8333348864.

### The request budget

This is the only collector whose cost grows with the size of the account. The monitoring API
answers **one metric of one droplet per request**, so a refresh costs one droplet listing
plus ten requests per droplet:

```
requests per hour = 3600/interval * (1 + droplets * 10)
```

Against the limit of 5000 requests an hour, at the default five-minute interval:

| Droplets | Requests per hour | |
|---|---|---|
| 5 | 612 | comfortable |
| 20 | 2412 | fine |
| 40 | 4812 | at the limit |
| 100 | 12012 | impossible — raise the interval |

That is why the collector is off unless asked for: no upgrade should quietly multiply
somebody's API usage. Work the number out before enabling it on a large account, and raise
`--collector.dropletmetrics.interval` rather than accepting rate limiting, which would take
every other collector down with it.

`--collector.dropletmetrics.agent-only` takes the droplets that have no monitoring agent out
of that arithmetic altogether, at the price of their series disappearing — a droplet it
skips emits nothing, not even `digitalocean_droplet_metrics_up`. Read the caveat on
[the monitoring API page](configuration/monitoring-api.md#dropletmetrics) before turning it
on: the feature it filters by is only set on droplets created with the agent.

**Do not refresh faster than two minutes.** The API samples every 120 seconds, so a shorter
interval spends requests re-reading a sample that has not changed. That cadence also bounds
freshness: the newest sample is between zero and 120 seconds old depending on where the
request lands in the cycle, which is what `digitalocean_droplet_metrics_timestamp_seconds`
makes visible.

### What it does not report

Bandwidth is deliberately absent. The API splits it by interface and direction and takes one
request per combination, which would add four requests per droplet — more than a third of
the budget again — for a figure the bill already summarises.

A droplet reports readings only if DigitalOcean's monitoring agent is installed and running
on it. A droplet without the agent is **not** a failure: its fetch succeeds and returns no
series, so it appears with `digitalocean_droplet_metrics_up` at 1, no readings and no
timestamp. If a droplet runs `node_exporter`, scrape that instead — it is free, more
detailed and not delayed by two minutes.

Each droplet is measured independently. One droplet failing keeps its previous readings,
sets its own `digitalocean_droplet_metrics_up` to 0 and logs why, without costing the
droplets that succeeded; only a failure to list the droplets, or every droplet failing,
fails the refresh and sets `collector_success` to 0.

## Load balancer metrics

Collected by `loadbalancermetrics` from `/v2/monitoring/metrics/load_balancer/*`, one set of
metrics per load balancer. **Off by default.**

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_loadbalancer_frontend_connections_current` | `id`, `name` | Active connections to the frontend |
| `digitalocean_loadbalancer_frontend_connections_limit` | `id`, `name` | Maximum connections the frontend allows |
| `digitalocean_loadbalancer_frontend_cpu_utilization_percent` | `id`, `name` | Frontend CPU utilization, in percent |
| `digitalocean_loadbalancer_frontend_http_responses_per_second` | `id`, `name`, `code` | Rate of HTTP responses, by code class |
| `digitalocean_loadbalancer_droplets_health_checks` | `id`, `name`, `server` | Health check status of one backend |
| `digitalocean_loadbalancer_droplets_downtime` | `id`, `name`, `server` | Downtime status of one backend |
| `digitalocean_loadbalancer_droplets_http_response_time_p95_seconds` | `id`, `name` | 95th percentile backend response time |
| `digitalocean_loadbalancer_metrics_up` | `id`, `name` | 1 if the last fetch succeeded |
| `digitalocean_loadbalancer_metrics_timestamp_seconds` | `id`, `name` | Unix time of the newest sample |

Unlike droplet metrics, none of this is available anywhere else: a load balancer cannot run
`node_exporter`. The metric that earns the collector its keep is
`digitalocean_loadbalancer_droplets_health_checks`, which names the **individual backend**
that is failing rather than only showing that the pool has shrunk:

```promql
digitalocean_loadbalancer_droplets_health_checks < 100
```

The `server` label is the backend droplet, as `node-<droplet id>`.

`frontend_http_responses` is a **rate**, not a running total — DigitalOcean's API
specification calls it the "rate of response code" — so it is a gauge of responses per
second and must not be wrapped in `rate()`. Error ratio:

```promql
sum by (id) (digitalocean_loadbalancer_frontend_http_responses_per_second{code="5xx"})
  / ignoring(code) sum by (id) (digitalocean_loadbalancer_frontend_http_responses_per_second)
```

Nothing this API returns for a load balancer is cumulative, so every metric here is a gauge.

### Units

The units are those DigitalOcean's own API specification states: percent for frontend CPU,
seconds for the 95th percentile response time. For the backend health check and downtime the
specification says only "status" and gives no unit, so none is claimed here either — the
observed values are 100 for a healthy backend and 0 for downtime on one that is up.

### Cost, and what an empty result means

Seven requests per load balancer per refresh, plus one listing. That is far cheaper than the
droplet equivalent simply because an account has far fewer load balancers; three of them at
the default interval cost 264 requests an hour against the limit of 5000. It is off by
default anyway, so that enabling monitoring is always deliberate.

An empty result is normal rather than exceptional here. A load balancer with no traffic has
no HTTP response series at all, and a network load balancer never has the HTTP metrics. Such
a load balancer reports `digitalocean_loadbalancer_metrics_up` at 1 with fewer series, not a
failure. As with droplets, one load balancer failing keeps its previous readings, sets its
own `_metrics_up` to 0 and logs why; only failing to list them, or every one failing, sets
`collector_success` to 0.

## Kubernetes

Collected by `kubernetes` from `/v2/kubernetes/clusters`, one set of metrics per cluster and
one per node pool in it.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_kubernetes_cluster_up` | `id`, `name`, `region`, `version` | 1 if the cluster state is `running` |
| `digitalocean_kubernetes_cluster_auto_upgrade` | `id`, `name`, `region` | 1 if the cluster upgrades itself in its maintenance window |
| `digitalocean_kubernetes_cluster_surge_upgrade` | `id`, `name`, `region` | 1 if it adds a node before replacing one |
| `digitalocean_kubernetes_cluster_ha` | `id`, `name`, `region` | 1 if the control plane is highly available |
| `digitalocean_kubernetes_node_pool_nodes` | `cluster_id`, `cluster`, `pool`, `size` | Nodes the pool is configured to run |
| `digitalocean_kubernetes_node_pool_nodes_running` | `cluster_id`, `cluster`, `pool`, `size` | Nodes in the pool reporting `running` |
| `digitalocean_kubernetes_node_pool_auto_scale` | `cluster_id`, `cluster`, `pool`, `size` | 1 if the pool scales itself |
| `digitalocean_kubernetes_node_pool_min_nodes` | `cluster_id`, `cluster`, `pool`, `size` | Smallest size the pool may scale to |
| `digitalocean_kubernetes_node_pool_max_nodes` | `cluster_id`, `cluster`, `pool`, `size` | Largest size the pool may scale to |

The configured count and the running count are kept apart on purpose: a pool that is waiting
for a node to come up reports the two apart, and that gap is the moment worth alerting on.

```promql
digitalocean_kubernetes_node_pool_nodes_running < digitalocean_kubernetes_node_pool_nodes
```

That comparison ships as `DigitalOceanNodePoolUnderProvisioned` on the
[alerting page](alerting.md#resources).

A pool carries its cluster twice, as `cluster_id` and as `cluster`. The name is what a
dashboard variable and an alert summary read; the id is what joins a pool to the cluster
metrics, which are labelled by `id`, and it is the half that survives a rename:

```promql
digitalocean_kubernetes_node_pool_nodes_running
  * on (cluster_id) group_left (region, version)
    label_replace(digitalocean_kubernetes_cluster_up, "cluster_id", "$1", "id", "(.*)")
```

`digitalocean_kubernetes_cluster_up` keeps the name and labels of the older, unmaintained
exporter. The node pool metrics do not: that exporter labels a pool by its own id and name
and leaves the cluster out, so there is no telling whose pool it is.

**This is the view from outside the cluster.** Pods, deployments and the rest are
kube-state-metrics' job. The worker nodes themselves are ordinary droplets and are also
reported by the `droplets` collector, from `/v2/droplets`.

## Limits in use

Collected by `limits` from `/v2/droplets`, `/v2/reserved_ips` and `/v2/volumes`. Each is
asked for one item per page, because the figure comes from `meta.total`: the inventory
itself never travels.

| Metric | Description |
|---|---|
| `digitalocean_account_droplets` | Number of droplets on the account |
| `digitalocean_account_reserved_ips` | Number of reserved IP addresses on the account |
| `digitalocean_account_volumes` | Number of block storage volumes on the account |

A limit only raises the question of how much of it is left. Paired with the account
collector's limits, these answer it:

```promql
digitalocean_account_droplets / digitalocean_account_droplet_limit
digitalocean_account_reserved_ips / digitalocean_account_reserved_ip_limit
digitalocean_account_volumes / digitalocean_account_volume_limit
```

A response without `meta.total` fails the refresh instead of falling back to the length of
the page — a page holds one item by design, so that fallback would report one droplet for an
account running a hundred. All three counts are read before the snapshot is replaced, so one
failing endpoint keeps the previous figures rather than mixing old and new.

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

## Managed databases

Collected by `databases` from `/v2/databases`, one set of metrics per cluster.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_database_status` | `id`, `name`, `region`, `size`, `engine`, `version` | 1 if the cluster is `online`, else 0 |
| `digitalocean_database_nodes` | `id`, `name`, `region`, `size`, `engine`, `version` | Number of nodes in the cluster |
| `digitalocean_database_storage_bytes` | `id`, `name`, `region` | Storage allocated to the cluster |
| `digitalocean_database_maintenance_pending` | `id`, `name`, `region` | 1 if maintenance is waiting for the cluster |

`digitalocean_database_status` and `digitalocean_database_nodes` keep the names and the
descriptive labels of the older, unmaintained exporter. Its three maintenance-window labels
are deliberately left off: `maintenance_window_pending` flips from `false` to `true` and
back, and a label that flips ends one series and starts another, which is exactly what a
gauge is for. Hence `digitalocean_database_maintenance_pending`.

**This is the state of the clusters, not their load.** Connections, queries, cache hits and
disk actually in use come from a Prometheus endpoint DigitalOcean runs per cluster, reached
with credentials of its own; that is a separate exporter's job, not this one's.

`storage_bytes` is the storage the plan allocates, again not the storage in use.

godo does not expose the pagination links of this endpoint, so the collector treats a full
page as the signal that another may follow. An account whose cluster count divides exactly
by the page size costs one extra empty request per refresh. The walk also stops at the first
cluster it has already seen: the endpoint documents no paging at all, and one that ignores
the page parameter would otherwise be asked for page 2, 3 and so on until the refresh died
on its deadline.

## Container registry

Collected by `registry` from `/v2/registries`, `/v2/registries/subscription` and
`/v2/registries/{name}/repositoriesV2`: two GETs per refresh plus one per registry, and one
more for every further page of a registry's repositories.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_registry_storage_usage_bytes` | `registry`, `region` | Storage the registry uses, as last measured by DigitalOcean |
| `digitalocean_registry_storage_usage_updated_timestamp_seconds` | `registry`, `region` | Unix timestamp of that measurement |
| `digitalocean_registry_storage_included_bytes` | `registry`, `region` | Storage included in the subscription tier |
| `digitalocean_registry_bandwidth_included_bytes` | `registry`, `region` | Outbound transfer included in the subscription tier each month |
| `digitalocean_registry_subscription_monthly_price_usd` | `registry`, `tier` | Monthly price of the subscription tier in US dollars |
| `digitalocean_registry_info` | `registry`, `region`, `tier`, `tier_name` | Always 1; the labels carry the tier slug and its display name |
| `digitalocean_registry_up` | `registry`, `region` | Whether the last refresh could list that registry's repositories |
| `digitalocean_registry_repositories` | `registry` | Number of repositories in the registry |
| `digitalocean_registry_repository_tags` | `registry`, `repository` | Number of tags in the repository |
| `digitalocean_registry_repository_manifests` | `registry`, `repository` | Number of manifests in the repository |
| `digitalocean_registry_repository_latest_manifest_size_bytes` | `registry`, `repository` | Compressed size of the repository's newest manifest |
| `digitalocean_registry_repository_last_push_timestamp_seconds` | `registry`, `repository` | Unix timestamp of the last push to the repository |

**The `registry` label is the registry's name, and an account can hold several.** A
Professional subscription may create more than one, and once it has, part of the
single-registry `/v2/registry` surface stops answering. The collector therefore enumerates
registries through `/v2/registries` and measures each one the same way. Where that endpoint
is unavailable — an account whose API does not offer it yet — it reads `/v2/registry`
instead and reports the one registry, which is what every account did before. The
subscription is account-wide however many registries it covers, so it stays a single
request and its allowance is reported against each of them.

**One registry that cannot be read does not cost the others.** Its
`digitalocean_registry_up` goes to 0, it keeps the repository figures it last reported, and
the refresh still succeeds with `collector_success` 1 — the failure is logged at warning
level, because nothing else would report it. Only every registry failing fails the refresh.
A registry seen for the first time whose repositories could not be listed reports no
repository metrics at all, rather than a count of zero that would read as a registry
holding nothing.

**An account without a registry is not a failure.** Both endpoints answer `404` there, which
the collector treats as a legitimate state: the refresh succeeds, `collector_success` stays
1 and no registry metric is emitted at all. It logs that once, at info level. The same
applies while the exporter runs — a deleted registry stops being reported rather than
freezing on its last known size. A `403`, on the other hand, means the token lacks the
registry scope: that is a real failure and drops `collector_success` to 0.

`digitalocean_registry_storage_usage_bytes` is DigitalOcean's own measurement, recomputed on
its schedule of several hours, not the exporter's. Refreshing more often does not make the
figure fresher, which is why the default interval of 5m is about the collector staying in
step with the rest of the exporter rather than about resolution.
`digitalocean_registry_storage_usage_updated_timestamp_seconds` is when that measurement was
taken, so the age of the size is visible rather than assumed:

```promql
time() - digitalocean_registry_storage_usage_updated_timestamp_seconds
```

The API leaves that field unset until it has measured the registry at least once, and a
registry in that state reports no timestamp rather than the epoch.

A repository that has never been pushed to reports its tag and manifest counts and nothing
else: it has no manifest, and a zero size would read as an image of no size.

Repository names are used as label values verbatim, slashes included
(`api.example.com/nginx`). The count is bounded by the repositories in the registry, so the
cardinality is that of a registry, not of a tag list — tags and manifests are counted, never
enumerated.

## Spaces

Collected by `spaces` from the S3-compatible API, one `HeadBucket` request per bucket.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_spaces_bucket_size_bytes` | `bucket`, `region` | Bytes stored in the bucket, as Spaces accounts for them |
| `digitalocean_spaces_bucket_objects` | `bucket`, `region` | Number of objects in the bucket |
| `digitalocean_spaces_bucket_up` | `bucket`, `region` | 1 if the bucket's last measurement succeeded, else 0 |

DigitalOcean publishes no bucket size in its own API, but the S3-compatible endpoint does:
Spaces runs on the Ceph RADOS Gateway, which reports `x-rgw-object-count` and
`x-rgw-bytes-used` on a HEAD of the bucket. Both figures match a full listing byte for
byte, at one request per bucket instead of one per thousand objects — see
[Spaces](configuration/spaces.md). The collector is disabled by default only because it
takes a Spaces key pair.

The size is the gateway's own accounting, so on a versioned bucket it includes noncurrent
versions, and incomplete multipart uploads count while they are pending. That is what
DigitalOcean bills for.

A bucket that cannot be measured keeps its previous size and object count and reports
`digitalocean_spaces_bucket_up 0`; the buckets that measured fine are unaffected, and the
failure is logged with the reason. A bucket never measured successfully reports only
`digitalocean_spaces_bucket_up 0`, because a zero size is indistinguishable from an empty
bucket. The collector's own `collector_success` drops to 0 only when discovery fails or no
bucket at all could be measured.

The exporter only ever calls `HeadBucket`, plus `ListBuckets` and `GetBucketLocation` in
discovery mode. It never lists or reads an object, so an access key with **Limited access**
and **Read** on the observed buckets is enough. Listing all buckets is a full-access
capability, so a limited key must be given an explicit bucket list.

## Exporter health

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_exporter_build_info` | `version`, `commit`, `goversion` | Always 1 |
| `digitalocean_exporter_collector_success` | `collector` | 1 if the collector's last refresh succeeded |
| `digitalocean_exporter_collector_duration_seconds` | `collector` | Duration of the last refresh |
| `digitalocean_exporter_collector_last_success_timestamp_seconds` | `collector` | Unix timestamp of the last successful refresh |
| `digitalocean_exporter_api_requests_total` (counter) | `collector`, `resource`, `status` | API requests by collector, resource and HTTP status |
| `digitalocean_exporter_api_request_duration_seconds` (histogram) | `collector`, `resource` | Duration of one API request |
| `digitalocean_exporter_api_rate_limit_remaining` | — | Requests left in the current API rate-limit window |
| `digitalocean_exporter_api_rate_limit` | — | Requests that window allows in total |
| `digitalocean_exporter_api_rate_limit_reset_timestamp_seconds` | — | Unix timestamp at which that window refills |

### Behaviour on failure

When a refresh fails, the collector's previous snapshot is kept and
`digitalocean_exporter_collector_success` drops to 0. Metrics are never dropped, so a
failing collector shows up as a flat line plus a failing health metric — not as a gap that
looks like DigitalOcean itself went away.

Before a collector's first successful refresh it emits nothing at all, rather than zeros.
A starting exporter must not be readable as an account with no droplets and no money.

### Attributing the API cost

`collector` on `digitalocean_exporter_api_requests_total` is the collector whose refresh
made the request, which is what the `resource` label cannot say: `limits` and `droplets`
both read `/v2/droplets`, and both monitoring collectors read `/v2/monitoring`. So the cost
each collector is quoted at in the [collector reference](configuration/collectors.md) can be
checked against the exporter itself:

```promql
sum by (collector) (rate(digitalocean_exporter_api_requests_total[5m])) * 3600
```

A request made outside any refresh carries `collector="none"`.

`digitalocean_exporter_api_request_duration_seconds` times the same requests, per collector
and resource. It measures the request alone: the wait behind the exporter's own rate limiter
is not in it, which is what separates "the API is slow" from "this collector is queued
behind its own fan-out". Neither metric carries a per-resource identifier, so their
cardinality is bounded by the number of collectors.

### Rate limiting

DigitalOcean allows 5000 API requests per hour per token, though the ceiling varies by
account. All three rate-limit gauges are read from the response headers, so they are
DigitalOcean's own count rather than an estimate: what is left, what the window allows, and
when it refills.

```promql
digitalocean_exporter_api_rate_limit_remaining / digitalocean_exporter_api_rate_limit
digitalocean_exporter_api_rate_limit_reset_timestamp_seconds - time()
```

The first is the share of the budget still available — the shape the bundled alert fires on,
because a fixed threshold means nothing against a ceiling that varies. The second is how
long a starved exporter stays starved. A response that carries no rate-limit headers leaves
the gauges as they were rather than zeroing them: a zero reads as a budget that has just run
out.

## Alerting

Twenty-one rules ship with the exporter as a plain Prometheus rule file, covering the
exporter's own health, account limits, resources that are down, certificates about to expire
and volumes billed for nothing. The chart can install them as a `PrometheusRule`.

See [alerting](alerting.md) for the full list, what each one fires on and what is deliberately
left out.
