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

**`collectors.*.interval`** spends from a budget of 5000 API requests an hour. For the eleven
collectors that are on by default the total is a few per cent at `5m`; for `dropletmetrics`
and `loadbalancermetrics` it is not, which is why those two are off. See
[the monitoring API](../configuration/monitoring-api.md). `spaces` is off for a different
reason — it needs a Spaces key pair rather than the API token — and `firewalls` and
`certificates` because most accounts have no reason to watch them, not because of what they
cost.

**`serviceMonitor.labels`** must match your Prometheus' `serviceMonitorSelector`, or the
ServiceMonitor is created and quietly ignored. For `kube-prometheus-stack` that is usually
`release: <release name>`. The rest of the ServiceMonitor's knobs —
`namespaceSelector`, `jobLabel`, `honorLabels`, `relabelings`, `metricRelabelings` —
default to rendering nothing, which leaves the operator's own defaults in charge; the one
most worth knowing about is `metricRelabelings`, the place to drop series you do not want
to store.

**`strategy`** defaults to `Recreate`, not Kubernetes' `RollingUpdate`. With one replica
the rolling surge runs two exporters side by side through every upgrade and token
rotation, doubling the API spend and reporting the account twice — see
[one replica](index.md#one-replica).

**`networkPolicy.enabled`** creates a NetworkPolicy allowing only DNS and TCP 443 out and
the metrics port in; `networkPolicy.ingress.namespaceSelector` and
`networkPolicy.ingress.podSelector` narrow who may scrape. Off by default, because it only
does anything on a cluster whose CNI enforces it. See
[NetworkPolicy](index.md#networkpolicy).

**`probes.scheme`** exists for one case: a web configuration in `extraArgs` that turns TLS
on also turns it on for the probe endpoints, and the kubelet has to be told to probe over
HTTPS or the pod crash-loops. See [TLS and basic auth](index.md#tls-and-basic-auth).

**`resources`** are sized for what the exporter does: hold one snapshot per collector in
memory and sleep. The defaults are generous for accounts of any size; the one thing that
grows is the number of series, which is bounded by how many resources you own.

**`digitalocean.token` versus `digitalocean.existingSecret`** — the two are mutually
exclusive, and the second is the one you want outside a test cluster.
[Secrets](secrets.md) covers both.

**`extraArgs`** is appended after every flag the chart renders, for a flag the chart has no
value of its own for. It cannot override one it does render: the exporter's flag parser
rejects a flag given twice rather than taking the later one, so the container would
crash-loop. A collector is switched off with `collectors.<name>.enabled: false`, which
renders `--no-collector.<name>`, and never through this list.

**`imagePullSecrets`, `podAnnotations`, `podLabels` and `priorityClassName`** are the
escape hatches for what a cluster imposes rather than what the exporter needs: a private
mirror of the image, a pod annotation or label something else in the cluster reads, and a
priority low enough that the exporter is evicted before what it watches. All of them
render nothing when unset.

**`extraEnv`, `extraVolumes` and `extraVolumeMounts`** are the same kind of hatch for the
pod spec itself, in the corresponding Kubernetes forms — `extraEnv` entries take
`valueFrom` as well as `value`. Their main use together is mounting an exporter-toolkit
web configuration for TLS, worked through in
[TLS and basic auth](index.md#tls-and-basic-auth). Credentials do not belong in
`extraEnv`; the token travels as a mounted file so it cannot leak through
`kubectl describe pod`.
