# Metrics

All metrics are gauges unless stated otherwise.

## Account

Collected by `account` from `/v2/account`.

| Metric | Description |
|---|---|
| `digitalocean_account_active` | 1 if the account status is `active`, else 0 |
| `digitalocean_account_status` | 1 for the account's current status and 0 for every other known one. Labelled `status`, one series each for `active`, `warning` and `locked` |
| `digitalocean_account_verified` | 1 if the account email address is verified |
| `digitalocean_account_droplet_limit` | Maximum number of droplets allowed |
| `digitalocean_account_floating_ip_limit` | Maximum number of floating IPs allowed |
| `digitalocean_account_reserved_ip_limit` | Maximum number of reserved IPs allowed |
| `digitalocean_account_volume_limit` | Maximum number of volumes allowed |

`digitalocean_account_active` collapses everything that is not `active` into one 0, which
loses the distinction worth having: `warning` is the billing-trouble state — an unpaid
invoice, a card that would not charge — while `locked` means DigitalOcean has already begun
acting on it and the API refuses to create anything. `digitalocean_account_status` keeps them
apart, and reports all three statuses on every scrape, so the query for the one you care
about returns a 0 rather than no data at all before the account ever enters it:

```promql
digitalocean_account_status{status="warning"} == 1
```

A status DigitalOcean invents later gets a series of its own alongside the three, so an
unrecognised status still reads as *some* status rather than as the metric disappearing.
That is what `DigitalOceanAccountNotActive` alerts on.

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
| `digitalocean_droplet_backups_enabled` | `id`, `name`, `region` | 1 if DigitalOcean's automatic backups are enabled |
| `digitalocean_droplet_monitoring_agent` | `id`, `name`, `region` | 1 if the droplet carries DigitalOcean's monitoring agent |
| `digitalocean_droplet_created_timestamp_seconds` | `id`, `name`, `region` | When the droplet was created, as a Unix timestamp |
| `digitalocean_droplet_info` | `id`, `name`, `region`, `size`, `status`, `image`, `vpc_uuid`, `tags` | Always 1 |

The first six names and their label sets are those of the older, unmaintained exporter, so
dashboards survive a migration. The size, the exact status, the image, the VPC and the tags
are the labels it does not carry; widening the metrics would have broken that compatibility,
so they live on `digitalocean_droplet_info` instead. Join on `id` to break a bill down by
size:

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

The last three metrics and the last two labels come out of the same droplet listing as
everything else here, so none of them costs an extra request.

`digitalocean_droplet_backups_enabled` is 0 for a droplet with no restore point of any kind.
Backups are switched on per droplet and billed as a percentage of what the droplet costs, so
the share of an account that has them is a decision somebody made rather than a default:

```promql
avg(digitalocean_droplet_backups_enabled)
```

`digitalocean_droplet_monitoring_agent` is 1 when the droplet was created with DigitalOcean's
monitoring agent. It is the cheap half of the [droplet metrics](#droplet-metrics) question:
a droplet at 0 here is one that will report no readings once that collector is enabled — an
empty graph, and the usual explanation behind `DigitalOceanDropletMetricsUnavailable` — and
it is what `--collector.dropletmetrics.agent-only` skips. Knowing it before paying ten
requests per droplet per refresh is the point.

The age of a droplet is `time() - digitalocean_droplet_created_timestamp_seconds`. A droplet
whose creation time the API did not return emits no sample at all rather than a zero, since
an epoch timestamp would read as a droplet created in 1970 and put it past every threshold.

`vpc_uuid` is what joins a droplet to the load balancer in front of it, which carries the
same label:

```promql
count by (vpc_uuid) (digitalocean_droplet_info)
  and on (vpc_uuid) digitalocean_loadbalancer_info
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

Tags are one label rather than a label per tag: `tags` holds all of them, sorted and joined
with commas, which keeps a droplet to a single info series however many it carries. Matching
one tag therefore means matching around the commas — `digitalocean_droplet_info{tags=~"(.*,)?web(,.*)?"}`,
since a Prometheus label matcher is anchored at both ends — and the label is on the info
metric alone. Neither `tags` nor `vpc_uuid` is on the up or the status series: retagging a
droplet, or moving it between VPCs, must not break the continuity of the series an alert
watches.

## Droplet autoscale pools

Collected by `dropletautoscale` from `/v2/droplets/autoscale`, one set of metrics per
autoscale pool.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_droplet_autoscale_pool_info` | `id`, `name`, `region`, `size`, `status` | Always 1 |
| `digitalocean_droplet_autoscale_pool_active_instances` | `id`, `name` | Active droplets in the pool |
| `digitalocean_droplet_autoscale_pool_min_instances` | `id`, `name` | Minimum droplets the pool keeps |
| `digitalocean_droplet_autoscale_pool_max_instances` | `id`, `name` | Maximum droplets the pool may grow to |
| `digitalocean_droplet_autoscale_pool_target_instances` | `id`, `name` | Fixed droplet count the pool is configured to run |
| `digitalocean_droplet_autoscale_pool_target_cpu_utilization_ratio` | `id`, `name` | CPU utilisation the pool scales towards, 0 to 1 |
| `digitalocean_droplet_autoscale_pool_target_memory_utilization_ratio` | `id`, `name` | Memory utilisation the pool scales towards, 0 to 1 |
| `digitalocean_droplet_autoscale_pool_current_cpu_utilization_ratio` | `id`, `name` | Average CPU utilisation across the pool's droplets, 0 to 1 |
| `digitalocean_droplet_autoscale_pool_current_memory_utilization_ratio` | `id`, `name` | Average memory utilisation across the pool's droplets, 0 to 1 |

A pool is configured one of two ways, and the metrics follow the split. A pool that scales
on utilisation carries `min_instances`, `max_instances` and at least one of the two target
ratios; a pool with a fixed target carries `target_instances` and none of those. The metrics
of the shape not in use are absent rather than zero — a fixed-target pool reporting
`max_instances 0` would read as a pool forbidden to run anything.

The utilisation ratios are the API's own decimals between 0 and 1, passed through
unchanged, so `current > target` is directly the condition the autoscaler acts on. A pool
whose listing reports no current utilisation emits neither `current_` series rather than
zeros.

The droplets a pool runs are ordinary droplets and appear in the [droplets](#droplets)
metrics like any other; nothing here duplicates them. What this collector adds is the
pool's own view — the bounds, the targets and the aggregate utilisation the scaling
decisions are made from. `DigitalOceanDropletAutoscalePoolAtMaximum` in the bundled
[alerting rules](alerting.md) reads exactly these series.

`status` on the info metric is `active`, `deleting` or `error`, as the API reports it.

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

## Reserved IPs

Collected by `reservedips` from `/v2/reserved_ips` and `/v2/reserved_ipv6`, one set of
metrics per reserved IP address. The `version` label is `4` or `6` and is what tells the two
listings apart.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_reserved_ip_assigned` | `ip`, `region`, `version` | 1 when the address is assigned to a droplet, 0 when it is idle |
| `digitalocean_reserved_ip_info` | `ip`, `region`, `version`, `droplet_id`, `droplet_name`, `project_id` | Always 1 |
| `digitalocean_reserved_ips` | `region`, `version` | Number of reserved IPs in that region of that version |

A reserved IP costs nothing while it is assigned to a droplet and is billed by the hour as
soon as it is not — the opposite way round from what most people expect, and the reason the
assignment is a metric rather than a label:

```promql
digitalocean_reserved_ip_assigned == 0
```

`digitalocean_reserved_ip_info` names the droplet behind an address, with `droplet_id` and
`droplet_name` empty for an idle one. `project_id` comes from the IPv4 listing only: the
IPv6 endpoint reports no project, so it is empty for every `version="6"` address.

`digitalocean_reserved_ips` counts the same addresses by region, which is the panel version
of the question. It is unrelated to `digitalocean_account_reserved_ips`, which the
[`limits`](#limits-in-use) collector reads as a single account-wide total to compare against
the account's limit.

## Images

Collected by `images` from `/v2/images?private=true`, one set of metrics per stored image.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_image_size_bytes` | `id`, `name`, `type` | Size of the stored image |
| `digitalocean_image_min_disk_size_bytes` | `id`, `name`, `type` | Smallest disk a droplet must have to boot the image |
| `digitalocean_image_created_timestamp_seconds` | `id`, `name`, `type` | When the image was created |
| `digitalocean_image_info` | `id`, `name`, `type`, `distribution`, `status`, `regions` | Always 1 |
| `digitalocean_images` | `type` | Number of private images of that type |

`type` is `snapshot` for a droplet or volume snapshot somebody took, `backup` for one of the
automatic droplet backups DigitalOcean takes when the option is enabled, and `custom` for an
image uploaded from outside. Only the account's own images are reported: the public
distribution and application images are DigitalOcean's, cost nothing and number in the
hundreds.

Both sizes read DigitalOcean's gigabytes as binary, the way every other size in this exporter
does, so an image the control panel calls 2.5 GB reports 2.5 GiB.

Stored images are the DigitalOcean cost that nothing nags about. A droplet that is destroyed
disappears from the bill; the snapshot taken before destroying it does not, and it is billed
by size every month until somebody deletes it. What that costs is:

```promql
sum(digitalocean_image_size_bytes) / 1024^3 * 0.06
```

DigitalOcean charges $0.06 per GiB per month for snapshot storage at the time of writing;
check the current figure rather than trusting the constant. Broken down by what it is:

```promql
sum by (type) (digitalocean_image_size_bytes)
```

`digitalocean_images` is reported for all three types even when the account holds none of
one, so a backup policy that silently stopped running shows as a count falling to zero rather
than as a series that disappears — which a graph draws identically to "the exporter went
away".

The age of an image is `time() - digitalocean_image_created_timestamp_seconds`, which is what
`DigitalOceanSnapshotOld` fires on. An image whose creation time the API did not report has
no sample at all rather than a zero, since an epoch timestamp would read as an image created
in 1970 and age past every threshold there is.

`regions` is every region slug the image has been distributed to, sorted and joined with
commas. Sorting matters: the API does not document an order, and a label that reorders itself
between two refreshes would churn the series for nothing. A custom image is billed once
however many regions it is available in.

## Load balancers

Collected by `loadbalancers` from `/v2/load_balancers`, one set of metrics per load balancer.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_loadbalancer_status` | `id`, `name`, `ip` | 1 if the load balancer's status is `active`, else 0 |
| `digitalocean_loadbalancer_droplets` | `id`, `name`, `ip` | Number of droplets it proxies to |
| `digitalocean_loadbalancer_size_units` | `id`, `name`, `ip` | Size units the load balancer is billed for |
| `digitalocean_loadbalancer_forwarding_rules` | `id`, `name`, `ip` | Number of forwarding rules configured |
| `digitalocean_loadbalancer_info` | `id`, `name`, `ip`, `region`, `size`, `type`, `algorithm`, `vpc_uuid`, `tag` | Always 1. `tag` is the droplet tag the load balancer selects its backends by, empty when they are listed by ID |
| `digitalocean_loadbalancer_forwarding_rule_info` | `id`, `name`, `ip`, `entry_protocol`, `entry_port`, `target_protocol`, `target_port`, `certificate_id`, `tls_passthrough` | Always 1, one series per forwarding rule |
| `digitalocean_loadbalancer_health_check_info` | `id`, `name`, `ip`, `protocol`, `port`, `path` | Always 1. How the load balancer probes its backends |
| `digitalocean_loadbalancer_health_check_interval_seconds` | `id`, `name`, `ip` | Seconds between two health checks of the same backend |
| `digitalocean_loadbalancer_health_check_timeout_seconds` | `id`, `name`, `ip` | Seconds the health check waits for a response before counting a failure |
| `digitalocean_loadbalancer_health_check_healthy_threshold` | `id`, `name`, `ip` | Consecutive successes before a backend rejoins the rotation |
| `digitalocean_loadbalancer_health_check_unhealthy_threshold` | `id`, `name`, `ip` | Consecutive failures before a backend leaves the rotation |
| `digitalocean_loadbalancer_firewall_rules` | `id`, `name`, `ip`, `kind` | Rules of that `kind` (`allow` or `deny`) on the load balancer's own firewall |

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

The configuration series — forwarding rules, health check settings and firewall rule counts —
all come out of the same list response as the rest, so they cost no extra requests. The
useful join is the forwarding rule's `certificate_id`: it matches the `id` label of the
[certificates collector](#certificates)'s expiry metric, so a query can find the certificates
that are actually terminating TLS on a load balancer:

```promql
digitalocean_certificate_expiry_timestamp_seconds
and on (id) label_replace(
  digitalocean_loadbalancer_forwarding_rule_info{certificate_id!=""},
  "id", "$1", "certificate_id", "(.+)")
```

A load balancer with no health check configured — a `REGIONAL_NETWORK` one passes packets
through — emits no health check series at all, rather than zeros. The firewall counts are the
load balancer's *own* allow and deny lists, not the account's cloud firewalls; both are 0
when none is configured.

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

## App Platform

Collected by `apps` from `/v2/apps`, one set of metrics per app and one more per component
of its spec.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_app_info` | `id`, `name`, `tier`, `region`, `default_ingress` | Always 1 |
| `digitalocean_app_deployment_phase` | `id`, `name`, `phase` | 1 for the phase the active deployment is in and 0 for every other known one |
| `digitalocean_app_deployment_in_progress` | `id`, `name` | 1 while a deployment is building or rolling out |
| `digitalocean_app_last_deployment_active_timestamp_seconds` | `id`, `name` | When the most recent deployment went active |
| `digitalocean_app_created_timestamp_seconds` | `id`, `name` | When the app was created |
| `digitalocean_app_component_instances` | `id`, `name`, `component`, `kind`, `instance_size` | Instances the spec asks for of this component |

`phase` is spelled the way the API spells it: `PENDING_BUILD`, `BUILDING`, `PENDING_DEPLOY`,
`DEPLOYING`, `ACTIVE`, `SUPERSEDED`, `ERROR`, `CANCELED`. All eight are reported for every
app on every scrape, so a query for the one you care about has a series before a deployment
ever enters it. A phase DigitalOcean adds later is reported beside them rather than dropped.

The phase is the **active** deployment's — the one App Platform is actually serving. An app
that has never had a successful deployment has no active deployment, and reports no phase
series at all rather than eight zeros, which would read as a deployment in none of the
phases. `digitalocean_app_deployment_in_progress` is the other half: it is 1 while a new
deployment is building or rolling out over whatever is live.

That split is what makes the failure worth alerting on. A deployment in `ERROR` does not take
the app down — the previous one keeps serving — so the app looks healthy from the outside
while the change somebody shipped is not on it:

```promql
digitalocean_app_deployment_phase{phase="ERROR"} == 1
```

That is `DigitalOceanAppDeploymentError` on the [alerting page](alerting.md#resources).

`kind` on the component metric is `service`, `worker`, `job` or `static_site`. A static site
runs no instances — App Platform serves it from its CDN — so it reports 0 with an empty
`instance_size`; the series exists so that a table of an app's components does not silently
miss half of a site-plus-API app. Functions components are not reported: they are billed by
invocation and have neither an instance count nor an instance size.

The instance count is what the spec **asks for**, not what is running. App Platform bills a
service by instance count and instance size, which makes `sum by (name)` of it the line that
explains a bill, but a component that is failing to start still reports the count it was
configured with.

### Runtime load is not here

Runtime load per component — CPU, memory, restart count — is not here. It lives behind
DigitalOcean's monitoring API under endpoints the API client this exporter uses has no
methods for, so there is nothing to read yet. The
[`dropletmetrics`](#droplet-metrics) and [`loadbalancermetrics`](#load-balancer-metrics)
collectors cover the parts of that API that are reachable.

## Domains

Collected by `domains` from `/v2/domains`, one metric per DNS zone.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_domain_ttl_seconds` | `domain` | Default TTL of the zone |

There is no metric counting the zones. `count(digitalocean_domain_ttl_seconds)` is the
number of them, the same way the number of droplets is `count(digitalocean_droplet_up)`.

The records inside a zone are deliberately absent: counting them costs one API request per
zone. The [collector page](configuration/collectors.md#domains) explains the trade-off.

## Tags

Collected by `tags` from `/v2/tags`, one metric per tag and resource type.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_tag_resources` | `tag`, `type` | Resources of that type carrying the tag |

`type` is one of `droplet`, `image`, `volume`, `volume_snapshot` or `database` — the types
the tag list reports counts for. A type the API reports no count for emits no series, rather
than a zero for every tag, so `sum by (tag)` adds up exactly what the API claimed.

The number of tags is `count(count by (tag) (digitalocean_tag_resources))`; a tag attached
to nothing at all emits no series and is not in that count.

## Projects

Collected by `projects` from `/v2/projects` and each project's `/resources` list, one set of
metrics per project.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_project_info` | `id`, `name`, `purpose`, `environment`, `is_default` | Always 1 |
| `digitalocean_project_resources` | `id`, `name`, `type` | Resources of that type the project owns |

`type` is taken from each resource's URN, `do:<type>:<id>` — `droplet`, `volume`,
`loadbalancer`, `domain` and whatever else the account holds. A URN of a shape the exporter
does not recognise is counted under `unknown` rather than dropped, so
`sum by (id) (digitalocean_project_resources)` is the number of resources the project owns.

A project whose resources lookup has never succeeded reports its `_info` and no counts: a
fabricated zero would be indistinguishable from an empty project. A lookup that fails after
succeeding once keeps the counts of the last success, the same way a whole failed refresh
keeps the previous snapshot.

The [collector page](configuration/collectors.md#projects) explains what the fan-out costs
and the timeout that bounds it.

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

Which droplets have the agent at all is answered for free by
`digitalocean_droplet_monitoring_agent` in the [droplets](#droplets) collector, without
enabling this one: a droplet at 0 there is one this collector will spend ten requests a
refresh on and get nothing back for, and the explanation for its empty panels — and for a
`digitalocean_droplet_metrics_up` of 0 when the fetch fails outright — before anybody starts
looking for a fault in the exporter.

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

With [`--collector.loadbalancermetrics.extended`](configuration/monitoring-api.md#the-extended-metric-set)
the rest of what the monitoring API offers per load balancer is read as well:

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_loadbalancer_frontend_http_requests_per_second` | `id`, `name` | Rate of HTTP requests received |
| `digitalocean_loadbalancer_frontend_network_throughput_http_bytes_per_second` | `id`, `name` | HTTP throughput through the frontend |
| `digitalocean_loadbalancer_frontend_network_throughput_udp_bytes_per_second` | `id`, `name` | UDP throughput through the frontend |
| `digitalocean_loadbalancer_frontend_network_throughput_tcp_bytes_per_second` | `id`, `name` | TCP throughput through the frontend |
| `digitalocean_loadbalancer_frontend_nlb_tcp_network_throughput_bytes_per_second` | `id`, `name` | TCP throughput, network load balancers |
| `digitalocean_loadbalancer_frontend_nlb_udp_network_throughput_bytes_per_second` | `id`, `name` | UDP throughput, network load balancers |
| `digitalocean_loadbalancer_frontend_firewall_dropped_bytes_per_second` | `id`, `name` | Bytes the firewall dropped, network load balancers |
| `digitalocean_loadbalancer_frontend_firewall_dropped_packets_per_second` | `id`, `name` | Packets the firewall dropped, network load balancers |
| `digitalocean_loadbalancer_frontend_tls_connections_current` | `id`, `name` | Rate of new TLS connections |
| `digitalocean_loadbalancer_frontend_tls_connections_limit` | `id`, `name` | Maximum TLS connection rate allowed |
| `digitalocean_loadbalancer_frontend_tls_connections_exceeding_rate_limit` | `id`, `name` | TLS connections closed for exceeding that rate |
| `digitalocean_loadbalancer_droplets_connections` | `id`, `name`, `server` | Active connections to one backend |
| `digitalocean_loadbalancer_droplets_queue_size` | `id`, `name` | HTTP requests queued waiting for a backend |
| `digitalocean_loadbalancer_droplets_http_responses_per_second` | `id`, `name`, `code` | Rate of backend HTTP responses, by code class |
| `digitalocean_loadbalancer_droplets_http_session_duration_avg_seconds` | `id`, `name` | Average backend HTTP session duration |
| `digitalocean_loadbalancer_droplets_http_session_duration_p50_seconds` | `id`, `name` | Median backend HTTP session duration |
| `digitalocean_loadbalancer_droplets_http_session_duration_p95_seconds` | `id`, `name` | 95th percentile backend HTTP session duration |
| `digitalocean_loadbalancer_droplets_http_response_time_avg_seconds` | `id`, `name` | Average backend response time |
| `digitalocean_loadbalancer_droplets_http_response_time_p50_seconds` | `id`, `name` | Median backend response time |
| `digitalocean_loadbalancer_droplets_http_response_time_p99_seconds` | `id`, `name` | 99th percentile backend response time |

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
seconds for the response times and session durations, bytes per second for the network
throughputs. For the backend health check and downtime the specification says only "status"
and gives no unit, so none is claimed here either — the observed values are 100 for a
healthy backend and 0 for downtime on one that is up. The TLS connection metrics are rates —
the specification calls the current one a "TLS connections rate", and the limit is the rate
cap the load balancer's size allows — but it names no unit for them, so their names carry
none, like `frontend_connections_current`.

### Cost, and what an empty result means

Seven requests per load balancer per refresh, plus one listing — 27 with the
[extended set](configuration/monitoring-api.md#the-extended-metric-set) on. That is far
cheaper than the droplet equivalent simply because an account has far fewer load balancers;
three of them at the default interval cost 264 requests an hour against the limit of 5000.
It is off by default anyway, so that enabling monitoring is always deliberate.

An empty result is normal rather than exceptional here. A load balancer with no traffic has
no HTTP response series at all, and a network load balancer never has the HTTP metrics. Such
a load balancer reports `digitalocean_loadbalancer_metrics_up` at 1 with fewer series, not a
failure. As with droplets, one load balancer failing keeps its previous readings, sets its
own `_metrics_up` to 0 and logs why; only failing to list them, or every one failing, sets
`collector_success` to 0.

## Kubernetes

Collected by `kubernetes` from `/v2/kubernetes/clusters`, one set of metrics per cluster, one
per node pool in it and one per node in those pools, plus
`/v2/kubernetes/clusters/<id>/upgrades` per cluster when
[`--collector.kubernetes.upgrades`](configuration/collectors.md#kubernetes) is on, which it is
by default.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_kubernetes_cluster_up` | `id`, `name`, `region`, `version` | 1 if the cluster state is `running` |
| `digitalocean_kubernetes_cluster_auto_upgrade` | `id`, `name`, `region` | 1 if the cluster upgrades itself in its maintenance window |
| `digitalocean_kubernetes_cluster_surge_upgrade` | `id`, `name`, `region` | 1 if it adds a node before replacing one |
| `digitalocean_kubernetes_cluster_ha` | `id`, `name`, `region` | 1 if the control plane is highly available |
| `digitalocean_kubernetes_cluster_registry_enabled` | `id`, `name`, `region` | 1 if the account's container registry is integrated with the cluster |
| `digitalocean_kubernetes_cluster_info` | `id`, `name`, `region`, `version`, `maintenance_day`, `maintenance_start_time` | Always 1; the maintenance window in the account's own words |
| `digitalocean_kubernetes_cluster_upgrade_available` | `cluster_id`, `cluster` | 1 if at least one newer version is offered |
| `digitalocean_kubernetes_cluster_available_version_info` | `cluster_id`, `cluster`, `version` | Always 1, once per version on offer |
| `digitalocean_kubernetes_node_pool_nodes` | `cluster_id`, `cluster`, `pool_id`, `pool`, `size` | Nodes the pool is configured to run |
| `digitalocean_kubernetes_node_pool_nodes_running` | `cluster_id`, `cluster`, `pool_id`, `pool`, `size` | Nodes in the pool reporting `running` |
| `digitalocean_kubernetes_node_pool_auto_scale` | `cluster_id`, `cluster`, `pool_id`, `pool`, `size` | 1 if the pool scales itself |
| `digitalocean_kubernetes_node_pool_min_nodes` | `cluster_id`, `cluster`, `pool_id`, `pool`, `size` | Smallest size the pool may scale to |
| `digitalocean_kubernetes_node_pool_max_nodes` | `cluster_id`, `cluster`, `pool_id`, `pool`, `size` | Largest size the pool may scale to |
| `digitalocean_kubernetes_node_state` | `cluster_id`, `cluster`, `pool_id`, `pool`, `node_id`, `node`, `state` | 1 for the node's current state and 0 for every other known one |
| `digitalocean_kubernetes_node_info` | `cluster_id`, `cluster`, `pool_id`, `pool`, `node_id`, `node`, `droplet_id` | Always 1; ties the node to the droplet underneath it |

The configured count and the running count are kept apart on purpose: a pool that is waiting
for a node to come up reports the two apart, and that gap is the moment worth alerting on.

```promql
digitalocean_kubernetes_node_pool_nodes_running < digitalocean_kubernetes_node_pool_nodes
```

That comparison ships as `DigitalOceanNodePoolUnderProvisioned` on the
[alerting page](alerting.md#resources).

`digitalocean_kubernetes_node_state` reports every state DigitalOcean documents —
`provisioning`, `running`, `draining` and `deleting` — for every node on every scrape, plus
whichever state a node is in if DigitalOcean has invented one since. A state that only appears
once something is wrong is a query that returns no data exactly when it matters:

```promql
digitalocean_kubernetes_node_state{state="running"} == 0
```

That is `DigitalOceanKubernetesNodeNotRunning`, the per-node half of the pool comparison
above. The status message DigitalOcean writes beside the state is not exported: it is free
text that changes wording without the node changing, and every wording of it would start a new
series. Four series per node is the cost of the state metric, so an account running hundreds
of nodes is where to think about it — switching the collector off is the only lever, since the
nodes arrive in the cluster list either way.

`digitalocean_kubernetes_cluster_upgrade_available` needs the upgrades lookup, which is on by
default and costs one request per cluster per refresh. Both upgrade metrics are absent, rather
than zero, until a lookup has succeeded: a zero would read as "you are on the newest version".
The versions on offer are `digitalocean_kubernetes_cluster_available_version_info`, one series
each, and they join to the cluster booleans by name or through `cluster_id`.

A pool, a node and the upgrade metrics carry the cluster twice, as `cluster_id` and as
`cluster`. The name is what a dashboard variable and an alert summary read; the id is what
joins them to the cluster metrics, which are labelled by `id`, and it is the half that
survives a rename:

```promql
digitalocean_kubernetes_node_pool_nodes_running
  * on (cluster_id) group_left (region, version)
    label_replace(digitalocean_kubernetes_cluster_up, "cluster_id", "$1", "id", "(.*)")
```

`digitalocean_kubernetes_cluster_up` keeps the name and labels of the older, unmaintained
exporter, and the cluster booleans beside it follow its `id`/`name` convention. The pool and
node metrics do not: that exporter labels a pool by its own id and name and leaves the cluster
out, so there is no telling whose pool it is.

A pool and a node carry their own id as well as their own name, for the same reason the
cluster does. A pool name is only unique within its cluster — two clusters commonly both have
a `workers` — so a query that groups by `pool` alone silently folds them together, and a
rename ends every series the pool had. Group by `pool_id` and `node_id`, or by the cluster and
the pool together, and neither happens.

**This is the view from outside the cluster.** Pods, deployments and the rest are
kube-state-metrics' job. The worker nodes themselves are ordinary droplets and are also
reported by the `droplets` collector, from `/v2/droplets`, which is what the `droplet_id` on
`digitalocean_kubernetes_node_info` joins to:

```promql
digitalocean_kubernetes_node_info
  * on (droplet_id) group_left (size, status)
    label_replace(digitalocean_droplet_info, "droplet_id", "$1", "id", "(.*)")
```

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

Collected by `databases` from `/v2/databases`, one set of metrics per cluster, plus —
unless `--no-collector.databases.details` — each cluster's replicas from
`/v2/databases/{id}/replicas` and its backups from `/v2/databases/{id}/backups`.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_database_status` | `id`, `name`, `region`, `size`, `engine`, `version` | 1 if the cluster is `online`, else 0 |
| `digitalocean_database_nodes` | `id`, `name`, `region`, `size`, `engine`, `version` | Number of nodes in the cluster |
| `digitalocean_database_storage_bytes` | `id`, `name`, `region` | Storage allocated to the cluster |
| `digitalocean_database_maintenance_pending` | `id`, `name`, `region` | 1 if maintenance is waiting for the cluster |
| `digitalocean_database_users` | `id`, `name`, `region` | Number of database users on the cluster |
| `digitalocean_database_databases` | `id`, `name`, `region` | Number of logical databases on the cluster |
| `digitalocean_database_storage_autoscale_enabled` | `id`, `name`, `region` | 1 if the cluster grows its own storage before it fills |
| `digitalocean_database_cluster_info` | `id`, `name`, `region`, `project_id`, `private_network_uuid` | Always 1; ties the cluster to its project and its VPC |
| `digitalocean_database_replicas` | `id`, `name`, `region` | Number of read-only replicas of the cluster |
| `digitalocean_database_replica_status` | `id`, `name`, `replica`, `region`, `status` | 1 for the replica's current status, 0 for every other known one |
| `digitalocean_database_last_backup_timestamp_seconds` | `id`, `name`, `region` | Unix time the newest backup of the cluster was taken |

`digitalocean_database_status` and `digitalocean_database_nodes` keep the names and the
descriptive labels of the older, unmaintained exporter. Its three maintenance-window labels
are deliberately left off: `maintenance_window_pending` flips from `false` to `true` and
back, and a label that flips ends one series and starts another, which is exactly what a
gauge is for. Hence `digitalocean_database_maintenance_pending`.

**This is the state of the clusters, not their load.** Connections, queries, cache hits and
disk actually in use come from a Prometheus endpoint DigitalOcean runs per cluster, reached
with credentials of its own; that is a separate exporter's job, not this one's.

`storage_bytes` is the storage the plan allocates, again not the storage in use.

The last three rows are the detail metrics, behind `--collector.databases.details` (on by
default, two extra requests per cluster per refresh). On the replica series `id` and `name`
are the cluster's and `replica` and `region` the replica's own — a replica may live in a
different region from its cluster, which is much of the point of having one. Every documented
status — `creating`, `online`, `resizing`, `migrating`, `forking` — is reported for every
replica on every scrape, so an alert has a series before a replica ever enters the status it
watches for. One cluster's detail lookup failing does not fail the refresh or cost the other
clusters: that cluster keeps the details its last successful lookup found and the exporter
logs why. An engine that does not offer backups or replicas — a caching cluster, say —
answers those endpoints with a client error, which is read as "this cluster has none":
`digitalocean_database_replicas` reports 0 and no backup series appears.

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

Alerting rules ship with the exporter as a plain Prometheus rule file, covering the
exporter's own health, account limits, resources that are down, certificates about to expire,
and volumes and snapshots billed for nothing. The chart can install them as a `PrometheusRule`.

See [alerting](alerting.md) for the full list, what each one fires on and what is deliberately
left out.
