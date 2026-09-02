# Alerting

Twenty-five alerting rules ship with the exporter, in
[`charts/digitalocean-exporter/alerts/digitalocean.rules.yaml`](https://github.com/kozaktomas/digitalocean_exporter/blob/main/charts/digitalocean-exporter/alerts/digitalocean.rules.yaml).

It is a plain Prometheus rule file. Point `rule_files` at it directly, or let the chart wrap
it in a `PrometheusRule`. Every rule carries a `severity` label of `critical`, `warning` or
`info`, a `summary` and a `description`.

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
| `DigitalOceanLoadBalancerDown` | critical | 15m | A load balancer is not `active` |
| `DigitalOceanLoadBalancerWithoutBackends` | critical | 30m | An active load balancer proxies to no droplets |
| `DigitalOceanDatabaseUnhealthy` | critical | 15m | A managed database cluster is not `online` |
| `DigitalOceanDropletMetricsUnavailable` | warning | 30m | The monitoring API returns nothing for a droplet |

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

`DigitalOceanDropletMetricsUnavailable` usually means the droplet agent is not running, which
blanks the graphs in the DigitalOcean console too.

## Certificates and firewalls

| Alert | Severity | For | Fires when |
|---|---|---|---|
| `DigitalOceanCertificateExpiringSoon` | warning | 1h | A certificate has under 14 days left |
| `DigitalOceanCertificateExpiringCritical` | critical | 1h | A certificate has under 3 days left |
| `DigitalOceanFirewallChangesPending` | warning | 15m | A firewall's ruleset has not reached every droplet |
| `DigitalOceanFirewallOpenRulesChanged` | info | — | The number of inbound rules open to the internet moved in the last day |

Both collectors are [off by default](configuration/collectors.md), so these stay silent until
you switch them on.

The two certificate alerts exist separately because they mean different things. Fourteen days
is "automatic renewal has not happened yet, keep an eye on it"; three days is "it has had its
window and failed, replace it by hand". DigitalOcean renews a `lets_encrypt` certificate
itself, but a failed renewal is quiet: the certificate keeps its old expiry and its state
turns to `error`, which is why both alert on time remaining rather than on state.

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
| `DigitalOceanSpacesBucketUnreachable` | warning | 1h | A bucket could not be measured |

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
read access to that bucket.

## What is deliberately not alerted on

**Money.** No rule fires on `digitalocean_account_balance` or on month-to-date usage. What
counts as too much is an account-level judgement, and a threshold shipped in a chart would be
wrong for everyone. Build it against the [Billing dashboard](dashboards.md) instead.

**Anything derived from load.** CPU, memory and traffic come from DigitalOcean's monitoring
API, which samples every two minutes and is off by default because of what it costs in
requests. Alert on those from inside the droplet, where `node_exporter` sees them at scrape
resolution.

## Keeping them in step

The rules are held against the exporter in the test suite, the same way the dashboards are.
`make check` verifies that every `digitalocean_` metric a rule names is one the collectors
actually register, and that every `{{ $labels.x }}` in an annotation is a label carried by a
metric that rule reads — a summary that renders as "Pool  is short of nodes" is what a
mistyped label looks like in Alertmanager. It also enforces that each alert has a severity, a
summary, a description, and a row on this page.

`make alerts-lint` runs `promtool check rules` over the file, which is the only thing that
catches a malformed expression before Prometheus refuses to load the whole group.
