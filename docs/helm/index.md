# Helm chart

The chart lives in the same repository as the exporter and is released with it: chart
version, `appVersion` and exporter version are always the same number.

```bash
helm repo add digitalocean-exporter https://kozaktomas.github.io/digitalocean_exporter
helm repo update
helm search repo digitalocean-exporter/digitalocean-exporter --versions
```

Installation, upgrades and Prometheus wiring are on the
[Kubernetes page](../install/kubernetes.md); Terraform has [its own](../install/terraform.md).
This section documents the chart itself.

## What it creates

| Object | When |
|---|---|
| `Deployment` | always, one replica |
| `Service` | always, `ClusterIP` on `service.port` (default 9212) |
| `ServiceAccount` | always |
| `Secret` | unless `digitalocean.existingSecret` is set |
| `Secret` (Spaces) | when the spaces collector is on and `spaces.existingSecret` is not set |
| `ServiceMonitor` | when `serviceMonitor.enabled` is true |
| `PrometheusRule` | when `prometheusRule.enabled` is true |
| `ConfigMap` (dashboards) | when `grafana.dashboards.enabled` is true, one per bundled dashboard |

No PVC, no RBAC beyond the ServiceAccount, and no ConfigMap unless you ask for the
dashboards. The exporter is stateless and talks only to the DigitalOcean API — it does not
read the Kubernetes API, so it needs no cluster permissions at all.

The two Prometheus Operator objects need their CRDs to exist in the cluster; both are off by
default, so a cluster without the Operator renders the chart unchanged.

## How values become flags

The chart does not pass a config file; it renders command-line flags. `collectors.<name>.enabled`
decides which form each switch takes:

```yaml
collectors:
  balance:
    enabled: false
```

renders `--no-collector.balance`, because `--collector.balance=false` is a parse error that
would crash the container at startup. This is exactly the trap described in
[configuration](../configuration/index.md#turning-a-collector-off), and the chart exists
partly to keep you out of it.

An enabled collector renders both its switch and its interval:

```yaml
collectors:
  droplets:
    enabled: true
    interval: 10m
```

```
--collector.droplets --collector.droplets.interval=10m
```

## Probes

The two probes point at different endpoints because they answer different questions.

```yaml
livenessProbe:
  httpGet: {path: /healthz, port: metrics}
readinessProbe:
  httpGet: {path: /readyz, port: metrics}
  initialDelaySeconds: 10
  periodSeconds: 10
  failureThreshold: 45
```

`/healthz` answers without consulting a collector, which is what liveness wants: a refresh
that cannot reach the DigitalOcean API is not fixed by killing the pod, and killing it throws
away the snapshots every other collector holds.

`/readyz` answers 503 until every enabled collector has refreshed successfully at least once.
That is the honest readiness condition, because a collector emits nothing before its first
success — a pod that went Ready earlier would join the Service and serve scrapes missing
whole metrics. Afterwards it stays 200 even while a collector is failing, so a later API
outage does not take the exporter out of the Service and stop the scrape that reports it.

The failure threshold is generous on purpose: `10s × 45` plus the initial delay is 7m40s,
which covers the default `5m` collector interval plus a slow first refresh. A pod still
unready after that has a collector that cannot succeed rather than one that is slow — most
often `balance` on a token without the billing scope. [Operations](../operations.md#the-pod-never-becomes-ready)
covers what to do about it.

## Security posture

The pod runs as UID 65532, non-root, with `seccompProfile: RuntimeDefault`. The token is
mounted at `/etc/digitalocean-exporter/token` and passed with `--do.token-file`, never as
an environment variable — so it does not appear in `kubectl describe pod`, in the container's
environment, or in anything that dumps `/proc/*/environ`. The Spaces key pair, when the
spaces collector is on, mounts the same way at `/etc/digitalocean-exporter-spaces/`.

The pod also sets `automountServiceAccountToken: false`. The exporter never calls the
Kubernetes API, so a token for it would be one more credential sitting in the container
for nothing.

See [secrets](secrets.md) for the two ways to supply it.

## One replica

Do not raise `replicaCount` — there is deliberately no such value. Each replica refreshes
independently, so a second one doubles the API requests and reports the same account twice.

## Dashboards

The chart can render the bundled Grafana dashboards as ConfigMaps for the Grafana sidecar to
pick up, with `grafana.dashboards.enabled=true`. It is off by default. See
[dashboards](../dashboards.md) for what ships and for the folder annotation.

## Alerting rules

The chart can also wrap the bundled Prometheus rule file in a `PrometheusRule`, with
`prometheusRule.enabled=true`. It is off by default too — a chart that installs alerts
nobody asked for is a chart that pages somebody at 3am — and `prometheusRule.labels` has to
match whatever your Prometheus selects rules on. See [alerting](../alerting.md) for the
twenty-three rules and for using the file without the Operator.

## Values

Every key, its type and its default: [values reference](values.md).
