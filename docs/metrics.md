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

## Kubernetes

Collected by `kubernetes` from `/v2/kubernetes/clusters`, one set of metrics per cluster and
one per node pool in it.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_kubernetes_cluster_up` | `id`, `name`, `region`, `version` | 1 if the cluster state is `running` |
| `digitalocean_kubernetes_cluster_auto_upgrade` | `id`, `name`, `region` | 1 if the cluster upgrades itself in its maintenance window |
| `digitalocean_kubernetes_cluster_surge_upgrade` | `id`, `name`, `region` | 1 if it adds a node before replacing one |
| `digitalocean_kubernetes_cluster_ha` | `id`, `name`, `region` | 1 if the control plane is highly available |
| `digitalocean_kubernetes_node_pool_nodes` | `cluster`, `pool`, `size` | Nodes the pool is configured to run |
| `digitalocean_kubernetes_node_pool_nodes_running` | `cluster`, `pool`, `size` | Nodes in the pool reporting `running` |
| `digitalocean_kubernetes_node_pool_auto_scale` | `cluster`, `pool`, `size` | 1 if the pool scales itself |
| `digitalocean_kubernetes_node_pool_min_nodes` | `cluster`, `pool`, `size` | Smallest size the pool may scale to |
| `digitalocean_kubernetes_node_pool_max_nodes` | `cluster`, `pool`, `size` | Largest size the pool may scale to |

The configured count and the running count are kept apart on purpose: a pool that is waiting
for a node to come up reports the two apart, and that gap is the moment worth alerting on.

```yaml
      - alert: DigitalOceanKubernetesNodePoolShort
        expr: >
          digitalocean_kubernetes_node_pool_nodes_running
            < digitalocean_kubernetes_node_pool_nodes
        for: 15m
        annotations:
          summary: "Pool {{ $labels.pool }} of {{ $labels.cluster }} is short of nodes"
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
by the page size costs one extra empty request per refresh.

## Container registry

Collected by `registry` from `/v2/registry`, `/v2/registry/subscription` and
`/v2/registry/{name}/repositoriesV2`: three GETs per refresh, plus one for every further
page of repositories.

| Metric | Labels | Description |
|---|---|---|
| `digitalocean_registry_storage_usage_bytes` | `registry`, `region` | Storage the registry uses, as last measured by DigitalOcean |
| `digitalocean_registry_storage_included_bytes` | `registry`, `region` | Storage included in the subscription tier |
| `digitalocean_registry_bandwidth_included_bytes` | `registry`, `region` | Outbound transfer included in the subscription tier each month |
| `digitalocean_registry_subscription_monthly_price_usd` | `registry`, `tier` | Monthly price of the subscription tier in US dollars |
| `digitalocean_registry_info` | `registry`, `region`, `tier`, `tier_name` | Always 1; the labels carry the tier slug and its display name |
| `digitalocean_registry_repositories` | `registry` | Number of repositories in the registry |
| `digitalocean_registry_repository_tags` | `registry`, `repository` | Number of tags in the repository |
| `digitalocean_registry_repository_manifests` | `registry`, `repository` | Number of manifests in the repository |
| `digitalocean_registry_repository_latest_manifest_size_bytes` | `registry`, `repository` | Compressed size of the repository's newest manifest |
| `digitalocean_registry_repository_last_push_timestamp_seconds` | `registry`, `repository` | Unix timestamp of the last push to the repository |

**An account without a registry is not a failure.** `/v2/registry` answers `404` there, which
the collector treats as a legitimate state: the refresh succeeds, `collector_success` stays
1 and no registry metric is emitted at all. It logs that once, at info level. The same
applies while the exporter runs — a deleted registry stops being reported rather than
freezing on its last known size. A `403`, on the other hand, means the token lacks the
registry scope: that is a real failure and drops `collector_success` to 0.

`digitalocean_registry_storage_usage_bytes` is DigitalOcean's own measurement, recomputed on
its schedule of several hours, not the exporter's. Refreshing more often does not make the
figure fresher, which is why the default interval of 5m is about the collector staying in
step with the rest of the exporter rather than about resolution. The API does report when it
last measured, but under a field name the DigitalOcean Go client does not parse, so there is
no staleness metric for it.

A repository that has never been pushed to reports its tag and manifest counts and nothing
else: it has no manifest, and a zero size would read as an image of no size.

Repository names are used as label values verbatim, slashes included
(`api.example.com/nginx`). The count is bounded by the repositories in the registry, so the
cardinality is that of a registry, not of a tag list — tags and manifests are counted, never
enumerated.

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

      - alert: DigitalOceanAccountNearDropletLimit
        expr: digitalocean_account_droplets / digitalocean_account_droplet_limit > 0.8
        for: 1h
        annotations:
          summary: "The account uses over 80% of its droplet limit"

      - alert: DigitalOceanRegistryStorageNearQuota
        expr: >
          digitalocean_registry_storage_usage_bytes
            / digitalocean_registry_storage_included_bytes > 0.9
        for: 1h
        annotations:
          summary: "Registry {{ $labels.registry }} uses over 90% of its included storage"

      - alert: DigitalOceanExporterRateLimitLow
        expr: digitalocean_exporter_api_rate_limit_remaining < 500
        for: 5m
        annotations:
          summary: "Fewer than 500 DigitalOcean API requests left this hour"
```
