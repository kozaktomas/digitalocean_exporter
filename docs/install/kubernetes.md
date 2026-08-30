# Kubernetes and Helm

The chart is published as a classic Helm repository on GitHub Pages — the same URL as this
documentation.

```bash
helm repo add digitalocean-exporter https://kozaktomas.github.io/digitalocean_exporter
helm repo update
```

```bash
helm install digitalocean-exporter digitalocean-exporter/digitalocean-exporter \
  --version 0.3.0 \
  --namespace monitoring --create-namespace \
  --set digitalocean.existingSecret=digitalocean-token
```

Pin `--version`. Without it Helm takes whatever is newest at that moment, which is not the
same thing twice.

## Creating the Secret first

The chart can create a Secret for you from `digitalocean.token`, but that means the token
sits in your values file. Point it at a Secret you own instead:

```bash
kubectl create secret generic digitalocean-token \
  --namespace monitoring \
  --from-literal=token=dop_v1_...
```

Either way the token reaches the container **as a mounted file**, never as an environment
variable, so it does not appear in `kubectl describe pod`. The two modes and their key
names are covered in [secrets](../helm/secrets.md).

## A realistic values file

```yaml
# values.yaml
digitalocean:
  existingSecret: digitalocean-token

collectors:
  # This token has no billing scope.
  balance:
    enabled: false
  droplets:
    interval: 10m

serviceMonitor:
  enabled: true
  labels:
    release: kube-prometheus-stack

log:
  format: json
```

```bash
helm upgrade --install digitalocean-exporter \
  digitalocean-exporter/digitalocean-exporter \
  --version 0.3.0 --namespace monitoring -f values.yaml
```

Every key is listed in the [values reference](../helm/values.md).

## Getting Prometheus to scrape it

**With Prometheus Operator**, set `serviceMonitor.enabled: true`. The `serviceMonitor.labels`
must match your Prometheus' `serviceMonitorSelector` — for `kube-prometheus-stack` that is
usually `release: <your release name>`. The CRD has to exist in the cluster before you
install, or the chart will fail to render that object.

**Without the Operator**, annotate nothing and just point a scrape job at the service:

```yaml
scrape_configs:
  - job_name: digitalocean
    static_configs:
      - targets: ["digitalocean-exporter.monitoring.svc:9212"]
```

Because collectors refresh in the background, the scrape interval is yours to choose — it
does not affect how often DigitalOcean is called.

## Getting the dashboards in

Set `grafana.dashboards.enabled: true` and the chart renders one ConfigMap per bundled
dashboard, labelled `grafana_dashboard: "1"` for the Grafana sidecar to load. It is off by
default, since without that sidecar nothing reads them. [Dashboards](../dashboards.md)
covers the label, the folder annotation and importing the JSON by hand instead.

## One replica, always

Do not scale the Deployment up. Each replica refreshes independently, so two replicas
double the API requests and report the same account twice, which makes every alert
ambiguous. The exporter holds one snapshot per collector in memory and does nothing between
refreshes; one replica is enough for any account size.

## Upgrading

```bash
helm repo update
helm search repo digitalocean-exporter/digitalocean-exporter --versions
helm upgrade digitalocean-exporter digitalocean-exporter/digitalocean-exporter \
  --version 0.4.0 --namespace monitoring -f values.yaml
```

`helm search` after `helm repo update` is what tells you which versions exist; `0.4.0` here
is only standing in for a version newer than the one installed above.

`image.tag` defaults to the chart's `appVersion`, so moving the chart version moves the
exporter with it. Check the documentation for the version you are moving to — while the
project is `0.x`, a minor bump may change metric names or values.

## Uninstalling

```bash
helm uninstall digitalocean-exporter --namespace monitoring
```

A Secret you created yourself is not part of the release and stays behind; delete it
separately.
