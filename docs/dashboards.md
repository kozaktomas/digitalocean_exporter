# Dashboards

Ten Grafana dashboards ship with the exporter, in
[`charts/digitalocean-exporter/dashboards/`](https://github.com/kozaktomas/digitalocean_exporter/tree/main/charts/digitalocean-exporter/dashboards).
Between them they cover every collector, so the metrics in the
[metrics reference](metrics.md) can be looked at rather than read about.

They live inside the chart directory because Helm can only package files that sit below it.
Nothing about them is Helm-specific: the JSON imports into any Grafana.

## What ships

| Dashboard | What it answers | Collectors it needs |
|---|---|---|
| `overview.json` | Is anything down, and how much of the account is there? One row of totals across droplets, clusters, load balancers, databases, CDN, DNS, registry and Spaces, plus the reserved IPs that are assigned to nothing, and per database cluster the age of the newest backup and the read-only replicas | `account`, `droplets`, `kubernetes`, `loadbalancers`, `databases`, `cdn`, `domains`, `registry`, `reservedips`, `spaces` |
| `droplets.json` | Per-droplet CPU, memory, disk and load, filtered by a droplet variable, plus what share of droplets have backups and the monitoring agent | `droplets`, `dropletmetrics` |
| `kubernetes.json` | Cluster state, node pools and every node in them: sizes, autoscaling bounds, nodes actually running, and the state each node reports with the droplet under it | `kubernetes` |
| `loadbalancers.json` | Traffic, response times and which backend droplet is failing its health check, plus the configuration: forwarding rules with their certificate and health check settings | `loadbalancers`, `loadbalancermetrics` |
| `apps.json` | App Platform: tier and region per app, the phase of the deployment being served, how long ago it went live, and the instances each component asks for | `apps` |
| `storage.json` | Volumes, including those attached to nothing, stored images by type and the oldest of them, and container registry repositories | `volumes`, `images`, `registry` |
| `spaces.json` | Bucket size and object count, and how fast a bucket is growing | `spaces` |
| `exporter.json` | Is the exporter itself healthy? Refresh durations, last success, API requests and latency per collector, and how much of the rate-limit budget is left before it resets | none; self-metrics only |
| `billing.json` | What the account costs: balance, month-to-date usage, droplet run rate, registry overage | `balance`, `droplets`, `loadbalancers`, `volumes`, `registry` |
| `security.json` | Certificates about to expire, firewall rules open to the internet, changes that have not landed | `certificates`, `firewalls` |

A dashboard whose collector is switched off shows empty panels rather than breaking, which is
why all ten ship together. `security`, `spaces` and parts of `droplets` and `loadbalancers`
depend on collectors that are [off by default](configuration/collectors.md); every other
dashboard fills itself from the collectors an untouched install already runs.

## Importing them by hand

Download the JSON, then in Grafana pick **Dashboards → New → Import**, upload the file and
choose your Prometheus datasource. Repeat per file, or point Grafana's
[file provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/#dashboards)
at a directory holding all ten.

```bash
curl -sSLO https://raw.githubusercontent.com/kozaktomas/digitalocean_exporter/main/charts/digitalocean-exporter/dashboards/overview.json
```

## Installing them with the chart

The chart renders one ConfigMap per dashboard, labelled for the Grafana sidecar that
kube-prometheus-stack and the Grafana chart run. It is off by default, because without that
sidecar they are ConfigMaps nothing reads.

```yaml
grafana:
  dashboards:
    enabled: true
```

To file them into a folder, set `grafana.dashboards.folder`. That adds a `grafana_folder`
annotation, which **only works if the Grafana side is configured to read it**:

```yaml
# In this chart.
grafana:
  dashboards:
    enabled: true
    folder: DigitalOcean
```

```yaml
# In the Grafana chart, or under `grafana:` in kube-prometheus-stack.
sidecar:
  dashboards:
    enabled: true
    folderAnnotation: grafana_folder
    provider:
      foldersFromFilesStructure: true
```

Without both halves the annotation is inert and the dashboards land in the sidecar's default
folder. If your sidecar watches a different label, `grafana.dashboards.label` and
`grafana.dashboards.labelValue` change what the ConfigMaps carry. The
[values reference](helm/values.md) lists all four.

## The variables

Every dashboard declares two:

- **Data source** — a `prometheus` datasource picker. No dashboard names a datasource UID, so
  the same file works against any Prometheus.
- **Job** — populated from `label_values(digitalocean_exporter_build_info, job)`, multi-select
  with `All`. Every query filters on it, so two exporters scraped by one Prometheus — a
  personal account and a company one, say — stay apart.

`droplets`, `kubernetes`, `loadbalancers`, `spaces` and `apps` add a third for the resource
they break down by.

The dashboards are tagged `digitalocean` and each carries a links dropdown filtered to that
tag, so they navigate between each other. A dashboard added to the folder with the same tag
joins that dropdown by itself.

## Keeping them in step

A renamed or dropped metric does not fail anything on its own: the panel that used it simply
goes empty, months before anyone notices. So the dashboards are held against the exporter in
the test suite. `make check` extracts every PromQL expression from all ten and checks each
`digitalocean_` metric it names against the descriptors the collectors actually register,
using the same wiring the exporter itself uses. It also enforces the portability rules above,
and that each file is committed in its normalised form.

That last one matters when editing. Grafana does not guarantee key order on export, and it
bakes in whichever values the exporting instance had selected, so a dashboard edited in the
browser and pasted back arrives as an unreadable diff carrying one account's droplet names.
After replacing a file, normalise it:

```bash
go test ./cmd/digitalocean_exporter -run TestDashboardsAreNormalised -update.dashboards
```

This strips the instance's `id` and `version`, clears the variables' remembered selections and
rewrites the file with sorted keys. Commit the result.
