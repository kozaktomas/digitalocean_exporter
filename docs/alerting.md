# Alerting

The exporter ships its alerting rules in
[`charts/digitalocean-exporter/alerts/digitalocean.rules.yaml`](https://github.com/kozaktomas/digitalocean_exporter/blob/main/charts/digitalocean-exporter/alerts/digitalocean.rules.yaml);
every rule in that file has a row on this page.

It is a plain Prometheus rule file. Point `rule_files` at it directly, or let the chart wrap
it in a `PrometheusRule`. Every rule carries a `severity` label of `critical`, `warning` or
`info`, a `summary`, a `description` and a `runbook_url` pointing at its section of this
page.

Thresholds assume collectors refresh on their default interval — 5m for every collector but
`images`, which refreshes every 10m. A collector deliberately run slower needs the matching
`for` raised, or it will alert on data that is simply not due yet.

## Installing them

=== "Prometheus"

    ```yaml
    rule_files:
      - /etc/prometheus/rules/digitalocean.rules.yaml
    ```

=== "Helm"

    ```yaml
    prometheusRule:
      enabled: true
      labels:
        release: kube-prometheus-stack
    ```

    The `labels` have to match your Prometheus' `ruleSelector`; for
    `kube-prometheus-stack` that is usually `release: <your release name>`. The
    `PrometheusRule` CRD must exist in the cluster, exactly as for the
    [ServiceMonitor](install/kubernetes.md#getting-prometheus-to-scrape-it).

Off by default, so that installing the chart never adds alerts nobody asked for.

## The exporter itself

| Alert | Severity | For | Fires when |
|---|---|---|---|
| `DigitalOceanExporterAbsent` | critical | 15m | No `digitalocean_exporter_build_info` sample at all — nothing about the account is being observed |
| `DigitalOceanExporterCollectorFailing` | warning | 15m | A collector's refresh keeps failing. Its metrics are stale, not missing |
| `DigitalOceanExporterCollectorStale` | warning | 15m | A collector has not refreshed for an hour while still reporting success — a refresh that hangs rather than fails |
| `DigitalOceanExporterRateLimitLow` | warning | 5m | Under 10% of the account's hourly API budget left, measured against the ceiling the account itself reports |
| `DigitalOceanExporterAPIErrors` | warning | 15m | A collector's API requests keep coming back as 429, 5xx or transport errors |

`DigitalOceanExporterCollectorFailing` on `balance` is almost always a token without the
billing scope; see [token permissions](configuration/permissions.md). The two collector
alerts are complements: a failing refresh sets `collector_success` to 0, a hung one leaves it
at 1 while the timestamp ages.

`DigitalOceanExporterRateLimitLow` compares what is left against
`digitalocean_exporter_api_rate_limit`, the ceiling the account itself reports, rather than
against a number written into the rule — the hourly allowance is not the same everywhere. Its
description points at `digitalocean_exporter_api_rate_limit_reset_timestamp_seconds`, which
is how long the starvation still has to run, and at
`sum by (collector) (rate(digitalocean_exporter_api_requests_total[5m]))`, which is what is
spending it.

`DigitalOceanExporterAPIErrors` is the alert form of the advice under
[staying under the burst limit](configuration/index.md#staying-under-the-burst-limit): watch the
429 rate. It reads `digitalocean_exporter_api_requests_total` filtered to `429`, `5xx` and
`error` — the transport failures — and fires per collector and status once the rate stays
above one such answer every hundred seconds for fifteen minutes. Retries count too, because
each attempt spends from the budget. A sustained 429 rate means `--do.rate-limit` outruns
the account's hourly allowance, or something else is spending the same token; sustained 5xx
or `error` is DigitalOcean's side or the network.

## Account limits

| Alert | Severity | For | Fires when |
|---|---|---|---|
| `DigitalOceanAccountNotActive` | critical | 15m | The account is in a status other than `active` |
| `DigitalOceanAccountNearDropletLimit` | warning | 1h | Over 80% of the droplet limit is in use |
| `DigitalOceanAccountNearVolumeLimit` | warning | 1h | Over 80% of the volume limit is in use |
| `DigitalOceanAccountNearReservedIPLimit` | warning | 1h | Over 80% of the reserved IP limit is in use |

These are the alerts that turn a confusing failure into an expected one: a node pool that
will not scale up, or a PersistentVolumeClaim that leaves a pod pending, is often nothing but
an account limit.

`DigitalOceanAccountNotActive` is the exception among them, and the reason it is critical:
the limits above merely stop something being created, while a `warning` status is an unpaid
invoice that ends with resources being destroyed, and `locked` means that has already begun.
It reads `digitalocean_account_status{status!="active"}` rather than
`digitalocean_account_active`, so the status itself is in the summary — the two are handled
very differently and neither clears itself.

## Resources

| Alert | Severity | For | Fires when |
|---|---|---|---|
| `DigitalOceanDropletDown` | critical | 15m | A droplet is in a status other than `active`, `off` or `archive` |
| `DigitalOceanKubernetesClusterDown` | critical | 15m | A managed control plane is not `running` |
| `DigitalOceanNodePoolUnderProvisioned` | warning | 30m | A node pool runs fewer nodes than it is sized for |
| `DigitalOceanKubernetesNodeNotRunning` | warning | 30m | A node is in a state other than `running` |
| `DigitalOceanKubernetesUpgradeAvailable` | info | 24h | A newer version is offered for a cluster that does not upgrade itself |
| `DigitalOceanKubernetesNodePoolAtMaximum` | info | 1h | An autoscaling node pool sits at its maximum node count |
| `DigitalOceanLoadBalancerDown` | critical | 15m | A load balancer is not `active` |
| `DigitalOceanLoadBalancerWithoutBackends` | critical | 30m | An active load balancer proxies to no droplets |
| `DigitalOceanDatabaseUnhealthy` | critical | 15m | A managed database cluster is not `online` |
| `DigitalOceanDatabaseMaintenancePending` | info | 1h | A managed database cluster has maintenance queued for its window |
| `DigitalOceanDatabaseBackupStale` | warning | 30m | The newest backup of a managed database cluster is older than 36 hours |
| `DigitalOceanAppDeploymentError` | warning | 10m | The deployment an App Platform app is serving is in the `ERROR` phase |

`DigitalOceanLoadBalancerWithoutBackends` is the one worth having even in a small account: an
active load balancer with nothing behind it answers every request with an error while looking
healthy from the outside. A load balancer that selects backends by tag reports zero until
something carries the tag, which is why it waits 30 minutes.

`DigitalOceanDropletDown` deliberately ignores the two statuses somebody chooses: `off` and
`archive`. Paging on a droplet an operator powered off is how an alert gets a reputation for
crying wolf, and once it has one nobody reads it when a droplet stops on its own. What is
left is worth waking up for — a droplet DigitalOcean stopped, or one stuck in `new` a quarter
of an hour after it was created. The powered-off half is still billed, and is reported by
`DigitalOceanDropletOff` at info under [Cost](#cost).

`DigitalOceanAppDeploymentError` is a warning rather than a page because App Platform keeps
serving the last deployment that worked: nothing is down, but the change somebody shipped is
not live and the app looks fine from the outside, which is exactly the failure nobody
notices. Ten minutes is longer than a build takes to give up, so a deployment passing through
the phase on its way to being retried does not fire it. It reads
`digitalocean_app_deployment_phase{phase="ERROR"}`, which is reported for the *active*
deployment only — a failed deployment that was never promoted leaves the phase of the one
still being served alone.

`DigitalOceanKubernetesNodeNotRunning` is the per-node half of
`DigitalOceanNodePoolUnderProvisioned`. The pool alert says a pool is short; this one names
the node and the state it is stuck in, and `digitalocean_kubernetes_node_info` carries the
droplet id to look up next. Both wait half an hour, so an ordinary rolling replacement passes
in silence.

`DigitalOceanKubernetesUpgradeAvailable` is `info` and deliberately not a page: an available
version is a piece of maintenance, not an incident, and one that stays available for a day is
still something to read rather than to be woken by.
It stays quiet for a cluster with auto-upgrade on, which installs the version in its own
maintenance window, and it needs `--collector.kubernetes.upgrades`, which is on by default and
costs one request per cluster per refresh.

`DigitalOceanKubernetesNodePoolAtMaximum` fires when a pool that autoscales has sat at its
configured ceiling for an hour: the autoscaler has no headroom left, so pods that need
another node stay pending and nothing else says why. Either raise the pool's maximum or
treat it as a deliberate budget — in which case this alert is the reminder that scaling has
stopped there. The account's droplet limit can impose the same ceiling from outside, which
is `DigitalOceanAccountNearDropletLimit`'s job to report.

`DigitalOceanDatabaseMaintenancePending` is `info` because pending maintenance is a plan,
not a fault: DigitalOcean will apply it in the cluster's maintenance window, which usually
means a failover on a multi-node cluster and a short outage on a single-node one. The alert
is the nudge to check when that window falls, and to apply the maintenance by hand first if
a quieter moment suits better.

`DigitalOceanDatabaseBackupStale` watches the age of the newest backup rather than whether a
backup job ran: DigitalOcean backs a managed cluster up daily, so a newest backup older than
thirty-six hours means at least one daily backup did not happen — a day's cadence plus half a
day's slack, which an ordinary backup running a few hours late never crosses. What is at
stake is the restore: everything written since that backup would be lost. It reads
`digitalocean_database_last_backup_timestamp_seconds`, which needs
`--collector.databases.details` (on by default). Engines without backups — caching clusters,
say — never report the metric, so they cannot fire it.

## Monitoring API

| Alert | Severity | For | Fires when |
|---|---|---|---|
| `DigitalOceanDropletMetricsUnavailable` | warning | 30m | The monitoring API returns nothing for a droplet (needs `dropletmetrics`, off by default) |
| `DigitalOceanDropletDiskAlmostFull` | warning | 15m | A droplet filesystem has under 10% free (needs `dropletmetrics`, off by default) |
| `DigitalOceanDropletDiskCritical` | critical | 15m | A droplet filesystem has under 5% free (needs `dropletmetrics`, off by default) |
| `DigitalOceanDropletMemoryLow` | warning | 15m | A droplet has under 10% of its memory available (needs `dropletmetrics`, off by default) |
| `DigitalOceanLoadBalancerMetricsUnavailable` | warning | 30m | The monitoring API returns nothing for a load balancer (needs `loadbalancermetrics`, off by default) |
| `DigitalOceanLoadBalancerBackendUnhealthy` | critical | 10m | A backend droplet is failing the load balancer's health check (needs `loadbalancermetrics`, off by default) |
| `DigitalOceanLoadBalancerDropletDowntime` | warning | 10m | A backend droplet keeps registering downtime (needs `loadbalancermetrics`, off by default) |
| `DigitalOceanLoadBalancerConnectionsNearLimit` | warning | 10m | A frontend uses over 80% of its connection limit (needs `loadbalancermetrics`, off by default) |

All of these read the two [monitoring-API collectors](configuration/monitoring-api.md),
`dropletmetrics` and `loadbalancermetrics`. Both are **off by default** because their
request cost scales with the size of the account, so every rule here stays silent until the
collector behind it is switched on — do the budget arithmetic on that page first.

`DigitalOceanLoadBalancerBackendUnhealthy` is the reason to enable `loadbalancermetrics` at
all: it names the backend droplet the load balancer has marked down, which is getting no
traffic while the remaining backends carry its share. The health check value is 100 for a
healthy backend; ten minutes below that is a backend not coming back on its own. If every
backend fails at once, suspect the health check's definition before suspecting every droplet
together.

`DigitalOceanLoadBalancerDropletDowntime` is its flapping counterpart. Downtime is 0 for a
backend that is up, so a sustained value above zero without `BackendUnhealthy` firing
alongside it is a backend passing the check just often enough to keep receiving traffic it
then fails to serve.

`DigitalOceanDropletDiskAlmostFull` and `DigitalOceanDropletDiskCritical` are the
warning-then-page pair, like the certificate alerts: ten percent free is "find what is
growing", five percent is "free space now, or resize". Both compare
`digitalocean_droplet_filesystem_free_bytes` against `_size_bytes` per filesystem, so the
alert names the device and mountpoint. `DigitalOceanDropletMemoryLow` watches *available*
memory — what could be handed out without swapping — because free memory alone
undercounts what the page cache would give back; past it waits the OOM killer.

`DigitalOceanDropletMetricsUnavailable` usually means the droplet agent is not running,
which blanks the graphs in the DigitalOcean console too.
`DigitalOceanLoadBalancerMetricsUnavailable` is its twin, with no agent to have stopped: it
is the monitoring API itself declining to answer, and while it fires the backend-health and
frontend metrics beside it are stale rather than current.

`DigitalOceanLoadBalancerConnectionsNearLimit` pages before connections start being refused,
which from the client's side is the site being down while every backend stays healthy. The
limit scales with the load balancer's size units, and resizing is applied without downtime.

## Certificates and firewalls

| Alert | Severity | For | Fires when |
|---|---|---|---|
| `DigitalOceanCertificateExpiringSoon` | warning | 1h | A certificate has under 14 days left |
| `DigitalOceanCertificateExpiringCritical` | critical | 1h | A certificate has under 3 days left |
| `DigitalOceanCertificateError` | warning | 1h | A certificate's state is `error` — a failed issuance or renewal, weeks before expiry would say so |
| `DigitalOceanLoadBalancerCertificateExpiring` | warning | 1h | A certificate a load balancer's forwarding rule terminates TLS with has under 14 days left (needs `certificates`, off by default) |
| `DigitalOceanFirewallChangesPending` | warning | 15m | A firewall's ruleset has not reached every droplet |
| `DigitalOceanFirewallOpenRulesChanged` | info | — | The number of inbound rules open to the internet moved in the last day |

Both collectors are [off by default](configuration/collectors.md), so these stay silent until
you switch them on.

The two certificate alerts exist separately because they mean different things. Fourteen days
is "automatic renewal has not happened yet, keep an eye on it"; three days is "it has had its
window and failed, replace it by hand". DigitalOcean renews a `lets_encrypt` certificate
itself, but a failed renewal is quiet: the certificate keeps its old expiry and its state
turns to `error`, which is why both alert on time remaining rather than on state.

`DigitalOceanCertificateError` watches that state directly, and it is the earliest of the
three: a renewal fails weeks before the old certificate runs out, usually because DNS no
longer points where the domain validation expects. Fixed while this is the only alert
firing, it costs nothing; left alone, it matures into the two expiry alerts above.

`DigitalOceanLoadBalancerCertificateExpiring` narrows the fourteen-day warning to the
certificates actually serving traffic: it joins the expiry series to
`digitalocean_loadbalancer_forwarding_rule_info` through its `certificate_id` label, so it
only fires for a certificate some load balancer terminates TLS with. When it fires, expiry
means TLS errors on a live frontend rather than an unused certificate lapsing quietly. The
forwarding-rule side comes from the always-on `loadbalancers` collector, but the expiry side
needs the `certificates` collector, which is off by default — without it this alert never
fires.

`DigitalOceanFirewallOpenRulesChanged` has no `for` and is deliberately `info`. The count of
rules open to the internet is not a fault — a public web server needs one — so the alert is on
the change, as something to read rather than to act on.

## Cost

| Alert | Severity | For | Fires when |
|---|---|---|---|
| `DigitalOceanRegistryStorageNearQuota` | warning | 1h | Registry storage is over 90% of what the tier includes |
| `DigitalOceanVolumeUnattached` | info | 24h | A volume has been attached to nothing for a day |
| `DigitalOceanReservedIPUnassigned` | info | 24h | A reserved IP has been assigned to nothing for a day |
| `DigitalOceanSnapshotOld` | info | 1h | A snapshot has been stored for over ninety days |
| `DigitalOceanDropletOff` | info | 24h | A droplet has been powered off for a day |
| `DigitalOceanSpacesBucketUnreachable` | warning | 1h | A bucket could not be measured (needs `spaces`, off by default, with its own Spaces key) |

`DigitalOceanVolumeUnattached` waits a day because a volume is legitimately detached while
being moved between droplets. Past that it is usually a leftover from a deleted droplet or a
released PersistentVolumeClaim, billed by allocated size for as long as it exists.

`DigitalOceanReservedIPUnassigned` is the same rule for addresses. A reserved IP is free
while it serves a droplet and billed by the hour while it does not, which is the opposite way
round from what most people assume, and an address left behind by a destroyed droplet is
never mentioned again. A day is long enough to cover a migration between droplets.

`DigitalOceanSnapshotOld` is the storage equivalent of the same idea. DigitalOcean bills a
stored image by its size every month for as long as it exists, and a snapshot taken before a
change nobody rolled back is billed forever. Ninety days is a threshold to argue with: raise
it if you keep quarterly restore points, lower it if snapshots are only ever taken before a
deploy. It deliberately matches `type="snapshot"` alone — automatic droplet backups expire
on their own schedule, and a custom image is usually a base somebody uploaded on purpose.

`DigitalOceanDropletOff` is the other side of `DigitalOceanDropletDown`. A powered-off
droplet is billed by its size exactly as a running one is, because the disk and the address
stay reserved; stopping the charge means destroying it. A day of it is either something
forgotten or something waiting on a decision, which is a mail rather than a page.

`DigitalOceanSpacesBucketUnreachable` is per bucket: one bucket failing keeps its previous
size and object count and leaves the others alone. The usual cause is a Spaces key without
read access to that bucket. The `spaces` collector behind it is
[off by default](configuration/spaces.md) and needs that key on top of the API token, so
this alert stays silent until both are provided.

## What is deliberately not alerted on

**Money.** No rule fires on `digitalocean_account_balance` or on month-to-date usage. What
counts as too much is an account-level judgement, and a threshold shipped in a chart would be
wrong for everyone. Build it against the [Billing dashboard](dashboards.md) instead.

**CPU, load and traffic.** They come from DigitalOcean's monitoring API, which samples
every two minutes, and what counts as too busy is a judgement no shipped threshold gets
right. Alert on those from inside the droplet, where `node_exporter` sees them at scrape
resolution. Disk and memory are the exception, under
[Monitoring API](#monitoring-api) above: a filesystem at five percent free is a fault at
any sample rate, and for a droplet with no exporter of its own these are the only warning
there is.

## Keeping them in step

The rules are held against the exporter in the test suite, the same way the dashboards are.
`make check` verifies that every `digitalocean_` metric a rule names is one the collectors
actually register, and that every `{{ $labels.x }}` in an annotation is a label carried by a
metric that rule reads — a summary that renders as "Pool  is short of nodes" is what a
mistyped label looks like in Alertmanager. It also enforces that each alert has a severity, a
summary, a description, and a row on this page, and that each `runbook_url` points at a
section heading this page actually has — an anchor that does not resolve is a runbook link
that lands on the top of the page instead of the alert it was fired for.

`make alerts-lint` runs `promtool check rules` over the file, which is the only thing that
catches a malformed expression before Prometheus refuses to load the whole group.
