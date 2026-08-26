# Values reference

Every value the chart accepts. This table is **generated from the comments in
`values.yaml`** by [helm-docs](https://github.com/norwoodj/helm-docs), and CI fails if it is
out of date — so it cannot drift from the chart it documents.

Show the defaults for the version you are about to install with:

```bash
helm show values digitalocean-exporter/digitalocean-exporter --version 0.3.0
```

{%
  include-markdown "../../charts/digitalocean-exporter/README.md"
  start="<!-- values-start -->"
  end="<!-- values-end -->"
%}

## Notes on a few of them

**`image.tag`** is empty by default, which means the chart's `appVersion` — the exporter
release this chart version shipped with. Set it only to pin something else, and remember
that doing so decouples the two version numbers that otherwise move together.

**`nameOverride` and `fullnameOverride`** shape the names of the objects the chart
creates. The default is `<release>-<chart>`, collapsed to just the release name when the
release name already contains the chart name — so `helm install digitalocean-exporter`
gives a Deployment called `digitalocean-exporter`, not
`digitalocean-exporter-digitalocean-exporter`. Setting `fullnameOverride` on a release that
already exists renames every object in it, which for Helm means creating the new ones and
deleting the old.

**`collectors.*.interval`** spends from a budget of 5000 API requests an hour. For the ten
collectors that are on by default the total is a few per cent at `5m`; for `spaces`,
`dropletmetrics` and `loadbalancermetrics` it is not, which is why they are off. See
[the monitoring API](../configuration/monitoring-api.md).

**`serviceMonitor.labels`** must match your Prometheus' `serviceMonitorSelector`, or the
ServiceMonitor is created and quietly ignored. For `kube-prometheus-stack` that is usually
`release: <release name>`.

**`resources`** are sized for what the exporter does: hold one snapshot per collector in
memory and sleep. The defaults are generous for accounts of any size; the one thing that
grows is the number of series, which is bounded by how many resources you own.

**`digitalocean.token` versus `digitalocean.existingSecret`** — the two are mutually
exclusive, and the second is the one you want outside a test cluster.
[Secrets](secrets.md) covers both.
